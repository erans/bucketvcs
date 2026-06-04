package web

import (
	"log/slog"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

type settingsData struct {
	base
	User        *auth.User
	HasPassword bool
}

// handleSettings renders the profile page (GET /settings).
func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess := SessionFromContext(r.Context())
	u, err := s.store.GetUserByName(r.Context(), sess.Name)
	if err != nil {
		s.logger.Error("settings: get user", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	hasPW, err := s.store.HasPassword(r.Context(), sess.Name)
	if err != nil {
		s.logger.Error("settings: has password", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	d := settingsData{
		base:        base{Session: sess, CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		User:        u,
		HasPassword: hasPW,
	}
	if err := s.renderBuffered(w, "settings.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
	}
	EmitRequestMetric(r.Context(), s.logger, "settings", 200)
}

// handlePasswordChange processes POST /settings/password.
func (s *server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	sess := SessionFromContext(r.Context())
	cur, n1, n2 := r.PostFormValue("current"), r.PostFormValue("new1"), r.PostFormValue("new2")
	fail := func(msg string) {
		EmitAdminActionMetric(r.Context(), s.logger, "user", "password_change", "invalid")
		s.redirectFlash(w, r, "/settings", msg)
	}
	if n1 != n2 {
		fail("passwords do not match")
		return
	}
	if len(n1) < 8 {
		fail("password too short (min 8)")
		return
	}
	if _, err := s.store.VerifyPassword(r.Context(), sess.Name, cur); err != nil {
		if auth.IsCredentialError(err) {
			fail("current password incorrect")
			return
		}
		s.logger.Error("password change: verify", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.store.SetPassword(r.Context(), sess.Name, n1); err != nil {
		s.logger.Error("password change: set", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "user", "password_change", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	// Revoke the user's OTHER web sessions: a password change should kill any
	// attacker-held cookies. The current session survives (passed as the
	// exclusion) so the user is not logged out. Cleanup is best-effort — the
	// rotation already succeeded, so a failure here is logged but not fatal.
	var revoked int64
	revokeRan := false
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		revokeRan = true
		n, derr := s.store.DeleteSessionsForUser(r.Context(), sess.UserID, c.Value)
		if derr != nil {
			s.logger.Warn("password change: revoke other sessions", "user", sess.Name, "err", derr)
		} else {
			revoked = n
		}
	}
	s.emitAdmin(r.Context(), "auth.user.password_changed",
		slog.String("user", sess.Name), slog.Int64("sessions_revoked", revoked))
	EmitAdminActionMetric(r.Context(), s.logger, "user", "password_change", "ok")
	flashMsg := "password changed"
	if revokeRan {
		flashMsg = "password changed; other sessions signed out"
	}
	s.redirectFlash(w, r, "/settings", flashMsg)
}
