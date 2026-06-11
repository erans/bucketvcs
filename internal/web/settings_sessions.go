package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

type sessionsData struct {
	base
	Sessions []auth.SessionInfo
}

// handleSessionsPage renders GET /settings/sessions: the signed-in user's active
// web sessions, with the current one badged and not revocable.
func (s *server) handleSessionsPage(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess := SessionFromContext(r.Context())
	// The current session is identified by the raw cookie id; the store hashes
	// it internally to set IsCurrent on the matching row.
	var curRaw string
	if c, err := r.Cookie(sessionCookieName); err == nil {
		curRaw = c.Value
	}
	sessions, err := s.store.ListSessionsForUser(r.Context(), sess.UserID, curRaw)
	if err != nil {
		s.logger.Error("sessions: list", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	d := sessionsData{
		base:     base{Session: sess, CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Sessions: sessions,
	}
	if err := s.renderBuffered(w, "settings_sessions.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "settings_sessions", 200)
}

// handleSessionRevoke processes POST /settings/sessions/revoke: signs out one of
// the user's own sessions by its id hash. User-scoped at the store, so a hash
// belonging to another user cannot be revoked even if guessed.
func (s *server) handleSessionRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	sess := SessionFromContext(r.Context())
	idHash := r.PostFormValue("id_hash")
	if idHash == "" {
		EmitAdminActionMetric(r.Context(), s.logger, "session", "revoke", "invalid")
		s.redirectFlash(w, r, "/settings/sessions", "missing session id")
		return
	}
	// The current session is not individually revocable (log out instead). The
	// template omits its revoke form; this guards a hand-crafted POST. The stored
	// id is SHA-256(rawCookieID), matching sqlitestore.hashSessionID.
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		sum := sha256.Sum256([]byte(c.Value))
		if hex.EncodeToString(sum[:]) == idHash {
			s.redirectFlash(w, r, "/settings/sessions", "cannot revoke your current session; use log out")
			return
		}
	}
	n, err := s.store.DeleteSessionByHashForUser(r.Context(), sess.UserID, idHash)
	if err != nil {
		s.logger.Error("sessions: revoke", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "session", "revoke", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.session.revoked",
		slog.String("id_hash", idHash), slog.Int64("count", n))
	EmitAdminActionMetric(r.Context(), s.logger, "session", "revoke", "ok")
	msg := "session revoked"
	if n == 0 {
		msg = "session already gone"
	}
	s.redirectFlash(w, r, "/settings/sessions", msg)
}

// handleSessionRevokeAll processes POST /settings/sessions/revoke-all: signs out
// every OTHER session the user holds, keeping the current one.
func (s *server) handleSessionRevokeAll(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	sess := SessionFromContext(r.Context())
	var curRaw string
	if c, err := r.Cookie(sessionCookieName); err == nil {
		curRaw = c.Value
	}
	if curRaw == "" {
		s.redirectFlash(w, r, "/settings/sessions", "could not identify current session")
		return
	}
	n, err := s.store.DeleteSessionsForUser(r.Context(), sess.UserID, curRaw)
	if err != nil {
		s.logger.Error("sessions: revoke all", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "session", "revoke_all", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.session.revoked_all", slog.Int64("count", n))
	EmitAdminActionMetric(r.Context(), s.logger, "session", "revoke_all", "ok")
	s.redirectFlash(w, r, "/settings/sessions",
		fmt.Sprintf("%d other session(s) signed out", n))
}
