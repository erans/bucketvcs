package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// sshKeyRow is the per-row view model for the SSH keys table. State is computed
// server-side so the template stays dumb.
type sshKeyRow struct {
	auth.SSHKey
	State string // "active" | "revoked"
}

type keysData struct {
	base
	Keys []sshKeyRow
}

// sshKeyState computes the display state for an SSH key.
func sshKeyState(k auth.SSHKey) string {
	if k.RevokedAt != 0 {
		return "revoked"
	}
	return "active"
}

// handleKeysPage renders GET /settings/keys.
func (s *server) handleKeysPage(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess := SessionFromContext(r.Context())
	keys, err := s.store.ListSSHKeysForUser(r.Context(), sess.UserID)
	if err != nil {
		s.logger.Error("keys: list", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	rows := make([]sshKeyRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, sshKeyRow{SSHKey: k, State: sshKeyState(k)})
	}
	d := keysData{
		base: base{Session: sess, CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Keys: rows,
	}
	if err := s.renderBuffered(w, "settings_keys.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "settings_keys", 200)
}

// handleKeyAdd processes POST /settings/keys/add.
func (s *server) handleKeyAdd(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	sess := SessionFromContext(r.Context())

	k, err := auth.BuildUserSSHKey([]byte(r.PostFormValue("pubkey")), sess.UserID, r.PostFormValue("label"))
	if err != nil {
		EmitAdminActionMetric(r.Context(), s.logger, "sshkey", "add", "invalid")
		s.redirectFlash(w, r, "/settings/keys", "could not parse public key")
		return
	}
	if err := s.store.AddSSHKey(r.Context(), k); err != nil {
		if errors.Is(err, auth.ErrDuplicateFingerprint) {
			EmitAdminActionMetric(r.Context(), s.logger, "sshkey", "add", "invalid")
			s.redirectFlash(w, r, "/settings/keys", "key already registered")
			return
		}
		s.logger.Error("key add: store", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "sshkey", "add", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.sshkey.added",
		slog.String("kind", "user"),
		slog.String("key_id", k.ID),
		slog.String("fingerprint", k.Fingerprint),
	)
	EmitAdminActionMetric(r.Context(), s.logger, "sshkey", "add", "ok")
	s.redirectFlash(w, r, "/settings/keys", "key added")
}

// ownKeyOr404 resolves the posted key id and verifies the session owns it by
// listing the session user's keys and doing a linear match.
// Uniform 404 hides both "no such key" and "not yours".
func (s *server) ownKeyOr404(w http.ResponseWriter, r *http.Request) (auth.SSHKey, bool) {
	id := r.PostFormValue("id")
	sess := SessionFromContext(r.Context())
	keys, err := s.store.ListSSHKeysForUser(r.Context(), sess.UserID)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return auth.SSHKey{}, false
	}
	for _, k := range keys {
		if k.ID == id {
			return k, true
		}
	}
	s.renderError(w, r, http.StatusNotFound, "not found")
	return auth.SSHKey{}, false
}

// handleKeyRevoke processes POST /settings/keys/revoke.
func (s *server) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	k, ok := s.ownKeyOr404(w, r)
	if !ok {
		return
	}
	if err := s.store.RevokeSSHKey(r.Context(), k.ID); err != nil {
		s.logger.Error("key revoke: store", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "sshkey", "revoke", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.sshkey.revoked",
		slog.String("kind", "user"),
		slog.String("key_id", k.ID),
		slog.String("fingerprint", k.Fingerprint),
	)
	EmitAdminActionMetric(r.Context(), s.logger, "sshkey", "revoke", "ok")
	s.redirectFlash(w, r, "/settings/keys", "key revoked")
}
