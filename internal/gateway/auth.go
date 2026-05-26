package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
)

const authRealm = `Basic realm="bucketvcs"`

// actorContextKey is the context key under which the authenticated actor is
// stored after successful auth. Handlers retrieve it via ActorFromContext.
type actorContextKey struct{}

// ActorFromContext returns the authenticated actor or nil if anonymous.
func ActorFromContext(ctx context.Context) *auth.Actor {
	v, _ := ctx.Value(actorContextKey{}).(*auth.Actor)
	return v
}

// RunAuth executes the spec §6.2 sequence:
//
//  1. Rate-limit gate (M18) — 429 + Retry-After if the per-IP bucket is
//     over Burst. Runs BEFORE credential verification so a rate-limited
//     client never touches the auth store.
//  2. GetRepoFlags  -> 404 on ErrNoSuchRepo, 500 on other err
//  3. Extract Basic -> 401 on credential errors (counted via MarkFailure)
//  4. LookupRepoPerm
//  5. Decide        -> allow | 401 (anon) | 403 (authed)
//
// On allow, the returned actor (nil for anonymous) is also attached to the
// request context (the request pointer is mutated in place to add the value).
// A successful credential check resets the rate-limit bucket via MarkSuccess.
//
// limiter may be nil — Check / MarkFailure / MarkSuccess are all no-ops on
// nil receivers, so a nil Limiter disables rate limiting entirely.
// trustProxy is forwarded to ClientIP; see Options.TrustProxyHeaders.
// logger may be nil; emitters fall through to slog.Default() in that case.
//
// On deny, the response has already been fully written.
func RunAuth(w http.ResponseWriter, r *http.Request, store auth.Store, rr *RoutedRequest,
	limiter *ratelimit.Limiter, trustProxy bool, logger *slog.Logger) (*auth.Actor, bool) {
	ctx := r.Context()

	// M18 — rate-limit gate before any store call. The user parameter is
	// accepted for audit attribution but does NOT gate (see ratelimit
	// package doc on the dropped cross-IP per-user bucket).
	ip := ClientIP(r, trustProxy)
	var basicUser string
	if user, _, ok := r.BasicAuth(); ok {
		basicUser = user
	}
	if allowed, retryAfter, _ := limiter.CheckDetailed(ip, basicUser); !allowed {
		retrySec := int(retryAfter.Seconds())
		if retrySec < 1 {
			retrySec = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retrySec))
		http.Error(w,
			fmt.Sprintf("rate limited; retry after %ds", retrySec),
			http.StatusTooManyRequests)
		auth.EmitRateLimitHit(ctx, logger, ip, basicUser, "ip", retrySec, "https")
		ratelimit.EmitRateLimitMetric(ctx, logger, "limited_ip")
		return nil, false
	}

	flags, err := store.GetRepoFlags(ctx, rr.Tenant, rr.Repo)
	if errors.Is(err, auth.ErrNoSuchRepo) {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}

	var actor *auth.Actor
	var tokenID string
	var scope *auth.Scope
	if user, pass, hasBasic := r.BasicAuth(); hasBasic {
		actor, tokenID, scope, err = store.VerifyCredential(ctx, auth.BasicPassword{Username: user, Password: pass})
		if err != nil {
			// Only credential-state errors map to 401. Backend / internal
			// errors (DB unreachable, etc.) must surface as 500 so they
			// aren't masked as bad credentials.
			if auth.IsCredentialError(err) {
				limiter.MarkFailure(ip, basicUser)
				ratelimit.EmitRateLimitMetric(ctx, logger, "failure_counted")
				challenge(w, "invalid credentials")
			} else {
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			return nil, false
		}
		// Best-effort last-used update off the hot path.
		go func(id string) {
			tctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_ = store.TouchTokenUsage(tctx, id)
		}(tokenID)
		if scope != nil && (scope.Tenant != rr.Tenant || scope.Repo != rr.Repo) {
			http.Error(w, "scope mismatch", http.StatusForbidden)
			return nil, false
		}
		// Successful credential verification resets the rate-limit bucket
		// (good behavior earns full quota back). Scope mismatch above is a
		// policy denial, not a credential failure, so it deliberately does
		// NOT reset — but it also does NOT count as failure.
		limiter.MarkSuccess(ip, basicUser)
		ratelimit.EmitRateLimitMetric(ctx, logger, "success_reset")
	}

	var perm auth.Perm
	if scope != nil {
		// Scoped credential: deploy-key SSH (M6) or an OIDC-minted token (M22)
		// on the BasicPassword path. The credential is valid for exactly one
		// (tenant, repo); use the pre-authorized permission from the scope.
		perm = scope.Perm
	} else {
		perm, err = store.LookupRepoPerm(ctx, actor, rr.Tenant, rr.Repo)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return nil, false
		}
	}

	ok, _ := auth.Decide(actor, perm, rr.RequiredAction, flags)
	if !ok {
		if actor == nil {
			challenge(w, "authentication required")
		} else {
			http.Error(w, "insufficient permissions", http.StatusForbidden)
		}
		return nil, false
	}

	*r = *r.WithContext(context.WithValue(ctx, actorContextKey{}, actor))
	return actor, true
}

func challenge(w http.ResponseWriter, body string) {
	w.Header().Set("WWW-Authenticate", authRealm)
	http.Error(w, body, http.StatusUnauthorized)
}
