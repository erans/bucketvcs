package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
	"github.com/bucketvcs/bucketvcs/internal/oidc"
)

// OIDCVerifier is the subset of *oidc.Verifier the gateway depends on.
type OIDCVerifier interface {
	Verify(ctx context.Context, rawToken, issuer string) (map[string]any, error)
}

// OIDCExchangeStore is the store surface the exchange handler needs.
type OIDCExchangeStore interface {
	FindOIDCIssuerByURL(ctx context.Context, issuerURL string) (auth.OIDCIssuer, error)
	ListOIDCRulesForIssuer(ctx context.Context, alias string) ([]auth.OIDCTrustRule, error)
	MintOIDCToken(ctx context.Context, tenant, repo string, perm auth.Perm,
		scopes auth.TokenScope, ttlSeconds int64, label string) (string, error)
}

const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	subjectTokenJWT    = "urn:ietf:params:oauth:token-type:jwt"
	issuedTokenAccess  = "urn:ietf:params:oauth:token-type:access-token"
)

// handleOIDCExchange implements POST /_oidc/token (RFC 8693).
func (s *Server) handleOIDCExchange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := ClientIP(r, s.opts.TrustProxyHeaders)

	if r.Method != http.MethodPost {
		emitOIDCMetric(ctx, s.logger, "bad_request")
		writeOIDCError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	// Rate-limit gate (failures only count below; this just blocks floods).
	if allowed, _, _ := s.opts.Limiter.CheckDetailed(ip, ""); !allowed {
		emitOIDCMetric(ctx, s.logger, "rate_limited")
		writeOIDCError(w, http.StatusTooManyRequests, "slow_down", "rate limited")
		return
	}
	if err := r.ParseForm(); err != nil {
		emitOIDCMetric(ctx, s.logger, "bad_request")
		writeOIDCError(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	if r.PostForm.Get("grant_type") != grantTokenExchange {
		emitOIDCMetric(ctx, s.logger, "bad_request")
		writeOIDCError(w, http.StatusBadRequest, "unsupported_grant_type", "")
		return
	}
	// We accept an empty subject_token_type for client compatibility (some CI
	// token-exchange callers omit it); a present-but-non-JWT value is rejected.
	// The token is fully JWT-verified regardless, so leniency here is safe.
	if st := r.PostForm.Get("subject_token_type"); st != "" && st != subjectTokenJWT {
		emitOIDCMetric(ctx, s.logger, "bad_request")
		writeOIDCError(w, http.StatusBadRequest, "invalid_request", "unsupported subject_token_type")
		return
	}
	raw := r.PostForm.Get("subject_token")
	if raw == "" {
		emitOIDCMetric(ctx, s.logger, "bad_request")
		writeOIDCError(w, http.StatusBadRequest, "invalid_request", "missing subject_token")
		return
	}

	// Peek iss from the unverified payload to find the registered issuer.
	iss, ok := unverifiedIssuer(raw)
	if !ok {
		emitOIDCMetric(ctx, s.logger, "bad_request")
		writeOIDCError(w, http.StatusBadRequest, "invalid_request", "malformed token")
		return
	}
	issuer, err := s.opts.OIDCStore.FindOIDCIssuerByURL(ctx, iss)
	if err != nil {
		// Unknown issuer: uniform 400, NO JWKS fetch.
		auth.EmitOIDCRejected(ctx, s.logger, "unknown", ip, "unknown_issuer")
		emitOIDCMetric(ctx, s.logger, "bad_request")
		writeOIDCError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}

	claims, err := s.opts.OIDCVerifier.Verify(ctx, raw, issuer.IssuerURL)
	if err != nil {
		if errors.Is(err, oidc.ErrIssuerUnavailable) {
			// Issuer unreachable: retryable, NOT a credential failure (don't
			// punish the client for the IdP being down).
			auth.EmitOIDCRejected(ctx, s.logger, issuer.Alias, ip, "issuer_unavailable")
			emitOIDCMetric(ctx, s.logger, "issuer_unavailable")
			writeOIDCError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "issuer discovery/JWKS unreachable")
			return
		}
		s.opts.Limiter.MarkFailure(ip, "")
		ratelimit.EmitRateLimitMetric(ctx, s.logger, "failure_counted")
		auth.EmitOIDCRejected(ctx, s.logger, issuer.Alias, ip, "invalid_token")
		emitOIDCMetric(ctx, s.logger, "invalid_token")
		writeOIDCError(w, http.StatusUnauthorized, "invalid_token", "")
		return
	}

	rules, err := s.opts.OIDCStore.ListOIDCRulesForIssuer(ctx, issuer.Alias)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rule := auth.MatchRule(rules, claims)
	if rule == nil {
		auth.EmitOIDCRejected(ctx, s.logger, issuer.Alias, ip, "no_rule")
		emitOIDCMetric(ctx, s.logger, "no_rule")
		writeOIDCError(w, http.StatusForbidden, "access_denied", "")
		return
	}

	// Effective scopes: rule.Scopes, optionally narrowed by requested scope.
	effective := rule.Scopes
	if req := strings.TrimSpace(r.PostForm.Get("scope")); req != "" {
		want, perr := auth.ParseScopes(strings.ReplaceAll(req, " ", ","))
		if perr != nil {
			emitOIDCMetric(ctx, s.logger, "invalid_scope")
			writeOIDCError(w, http.StatusBadRequest, "invalid_scope", "")
			return
		}
		// Intersect against the EXPANDED grant so a write-granting rule can
		// still mint a read-only token (write implies read; CheckScope applies
		// the same hierarchy at use time).
		effective = auth.EffectiveScopes(rule.Scopes) & want
		if effective == 0 {
			emitOIDCMetric(ctx, s.logger, "invalid_scope")
			writeOIDCError(w, http.StatusBadRequest, "invalid_scope", "requested scope exceeds grant")
			return
		}
	}

	perm := auth.PermRead
	if auth.EffectiveScopes(effective).Has(auth.ScopeRepoWrite) {
		perm = auth.PermWrite
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		sub = "unknown"
	}
	label := "oidc:" + issuer.Alias + ":" + sub
	token, err := s.opts.OIDCStore.MintOIDCToken(ctx, rule.Tenant, rule.Repo, perm,
		effective, rule.TTLSeconds, label)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	auth.EmitOIDCExchanged(ctx, s.logger, issuer.Alias, sub, rule.Tenant, rule.Repo, effective, rule.TTLSeconds)
	emitOIDCMetric(ctx, s.logger, "minted")
	s.opts.Limiter.MarkSuccess(ip, "")

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":      token,
		"issued_token_type": issuedTokenAccess,
		"token_type":        "Bearer",
		"expires_in":        rule.TTLSeconds,
		"scope":             strings.ReplaceAll(auth.FormatScopes(effective), ",", " "),
	})
}

// unverifiedIssuer extracts the "iss" claim from an unverified compact JWS.
// This is used ONLY to select the registered issuer before signature
// verification; nothing here is trusted.
func unverifiedIssuer(raw string) (string, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &c); err != nil || c.Iss == "" {
		return "", false
	}
	return c.Iss, true
}

func writeOIDCError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	_ = json.NewEncoder(w).Encode(body)
}

// emitOIDCMetric logs an oidc_exchange_total{result} counter increment in the
// gateway's structured-metric shape (mirrors internal/lfs/metrics.go).
func emitOIDCMetric(ctx context.Context, logger *slog.Logger, result string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "oidc_exchange_total"),
		slog.Int64("value", 1),
		slog.String("result", result),
	)
}
