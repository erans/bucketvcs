package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
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
//  1. GetRepoFlags  -> 404 on ErrNoSuchRepo, 500 on other err
//  2. Extract Basic -> 401 on credential errors
//  3. LookupRepoPerm
//  4. Decide        -> allow | 401 (anon) | 403 (authed)
//
// On allow, the returned actor (nil for anonymous) is also attached to the
// request context (the request pointer is mutated in place to add the value).
//
// On deny, the response has already been fully written.
func RunAuth(w http.ResponseWriter, r *http.Request, store auth.Store, rr *RoutedRequest) (*auth.Actor, bool) {
	ctx := r.Context()

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
	if user, pass, hasBasic := r.BasicAuth(); hasBasic {
		actor, tokenID, err = store.VerifyCredential(ctx, auth.BasicPassword{Username: user, Password: pass})
		if err != nil {
			challenge(w, "invalid credentials")
			return nil, false
		}
		// Best-effort last-used update off the hot path.
		go func(id string) {
			tctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_ = store.TouchTokenUsage(tctx, id)
		}(tokenID)
	}

	perm, err := store.LookupRepoPerm(ctx, actor, rr.Tenant, rr.Repo)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
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
