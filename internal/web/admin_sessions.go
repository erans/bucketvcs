package web

import (
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// adminSessionsDisplayCap is the maximum number of sessions rendered in the
// admin page. ListAllSessions returns the full table; we cap here so a large
// deployment never produces a multi-megabyte page.
const adminSessionsDisplayCap = 500

type adminSessionsData struct {
	base
	Sessions  []auth.AdminSessionInfo
	Total     int
	Truncated bool
}

// handleAdminSessions renders GET /admin/sessions.
func (s *server) handleAdminSessions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess := SessionFromContext(r.Context())
	list, err := s.store.ListAllSessions(r.Context())
	if err != nil {
		s.logger.Error("admin sessions: list", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	total := len(list)
	truncated := false
	if len(list) > adminSessionsDisplayCap {
		list = list[:adminSessionsDisplayCap]
		truncated = true
	}
	d := adminSessionsData{
		base:      base{Session: sess, CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Sessions:  list,
		Total:     total,
		Truncated: truncated,
	}
	if err := s.renderBuffered(w, "admin_sessions.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "admin_sessions", http.StatusOK)
}

// handleAdminSessionRevoke processes POST /admin/sessions/revoke.
func (s *server) handleAdminSessionRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	const dest = "/admin/sessions"
	idHash := r.PostFormValue("id_hash")
	if idHash == "" {
		EmitAdminActionMetric(r.Context(), s.logger, "session", "admin_revoke", "invalid")
		s.redirectFlash(w, r, dest, "id_hash required")
		return
	}
	n, err := s.store.DeleteSessionByHash(r.Context(), idHash)
	if err != nil {
		s.logger.Error("admin sessions: revoke", "id_hash", idHash, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "session", "admin_revoke", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		EmitAdminActionMetric(r.Context(), s.logger, "session", "admin_revoke", "ok")
		s.redirectFlash(w, r, dest, "session already gone")
		return
	}
	EmitAdminSessionRevoked(r.Context(), s.logger, SessionFromContext(r.Context()).Name, idHash, n)
	EmitAdminActionMetric(r.Context(), s.logger, "session", "admin_revoke", "ok")
	s.redirectFlash(w, r, dest, "session revoked")
}
