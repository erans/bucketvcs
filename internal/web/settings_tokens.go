package web

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// tokenRow is the per-row view model for the tokens table. State is computed
// server-side so the template stays dumb.
type tokenRow struct {
	TokenInfo
	State string // "active" | "revoked" | "expired"
}

type tokensData struct {
	base
	Tokens []tokenRow
}

// tokenState computes the display state for a token.
func tokenState(t TokenInfo) string {
	if t.RevokedAt != nil {
		return "revoked"
	}
	if t.ExpiresAt != nil && *t.ExpiresAt < time.Now().Unix() {
		return "expired"
	}
	return "active"
}

// handleTokensPage renders GET /settings/tokens.
func (s *server) handleTokensPage(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess := SessionFromContext(r.Context())
	tokens, err := s.store.ListTokensForUser(r.Context(), sess.Name)
	if err != nil {
		s.logger.Error("tokens: list", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	rows := make([]tokenRow, 0, len(tokens))
	for _, t := range tokens {
		rows = append(rows, tokenRow{TokenInfo: t, State: tokenState(t)})
	}
	d := tokensData{
		base:   base{Session: sess, CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tokens: rows,
	}
	if err := s.renderBuffered(w, "settings_tokens.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "settings_tokens", 200)
}

// handleTokenCreate processes POST /settings/tokens/create.
func (s *server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	sess := SessionFromContext(r.Context())
	label := r.PostFormValue("label")
	scopesStr := r.PostFormValue("scopes")
	var scopes auth.TokenScope
	if scopesStr != "" {
		var err error
		scopes, err = auth.ParseScopes(scopesStr)
		if err != nil {
			EmitAdminActionMetric(r.Context(), s.logger, "token", "create", "invalid")
			s.redirectFlash(w, r, "/settings/tokens", "invalid scopes: "+scopesStr)
			return
		}
	}
	var expiresAt *int64
	if d := r.PostFormValue("expires"); d != "" {
		dur, err := time.ParseDuration(d)
		if err != nil || dur <= 0 {
			EmitAdminActionMetric(r.Context(), s.logger, "token", "create", "invalid")
			s.redirectFlash(w, r, "/settings/tokens", "invalid expiry duration")
			return
		}
		t := time.Now().Add(dur).Unix()
		expiresAt = &t
	}
	plaintext, id, secret, err := auth.GenerateToken()
	if err != nil {
		s.logger.Error("token create: generate", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "token", "create", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		s.logger.Error("token create: hash secret", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "token", "create", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.store.CreateToken(r.Context(), id, sess.UserID, hash, label, expiresAt, scopes); err != nil {
		s.logger.Error("token create: store", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "token", "create", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.token.created",
		slog.String("token_id", id),
		slog.String("label", label),
		slog.String("scopes", scopesStr),
	)
	EmitAdminActionMetric(r.Context(), s.logger, "token", "create", "ok")
	s.renderSecretOnce(w, r, "token created", plaintext, "/settings/tokens")
}

// ownTokenOr404 resolves the posted token id and verifies the session owns it.
// Uniform 404 hides both "no such token" and "not yours".
func (s *server) ownTokenOr404(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PostFormValue("id")
	sess := SessionFromContext(r.Context())
	owner, err := s.store.GetTokenOwner(r.Context(), id)
	if err != nil || owner != sess.UserID {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return "", false
	}
	return id, true
}

// handleTokenRevoke processes POST /settings/tokens/revoke.
func (s *server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	id, ok := s.ownTokenOr404(w, r)
	if !ok {
		return
	}
	if err := s.store.RevokeToken(r.Context(), id); err != nil {
		s.logger.Error("token revoke: store", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "token", "revoke", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.token.revoked", slog.String("token_id", id))
	EmitAdminActionMetric(r.Context(), s.logger, "token", "revoke", "ok")
	s.redirectFlash(w, r, "/settings/tokens", "token revoked")
}

// handleTokenRotate processes POST /settings/tokens/rotate.
func (s *server) handleTokenRotate(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	id, ok := s.ownTokenOr404(w, r)
	if !ok {
		return
	}
	// Generate a fresh secret; discard the new token string and id (keep existing id).
	_, _, secret, err := auth.GenerateToken()
	if err != nil {
		s.logger.Error("token rotate: generate", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "token", "rotate", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		s.logger.Error("token rotate: hash secret", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "token", "rotate", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.store.RotateToken(r.Context(), id, hash); err != nil {
		if err == auth.ErrNoSuchToken {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("token rotate: store", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "token", "rotate", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.token.rotated", slog.String("token_id", id))
	EmitAdminActionMetric(r.Context(), s.logger, "token", "rotate", "ok")
	s.renderSecretOnce(w, r, "token rotated", auth.AssembleToken(id, secret), "/settings/tokens")
}
