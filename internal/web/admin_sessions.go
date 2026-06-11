package web

import (
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// adminSessionsDisplayCap is the maximum number of sessions rendered in the
// admin page, pushed into the store query as a LIMIT so a large deployment
// never loads the full table (or produces a multi-megabyte page).
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
	list, total, err := s.store.ListAllSessions(r.Context(), adminSessionsDisplayCap)
	if err != nil {
		s.logger.Error("admin sessions: list", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	// Badge the admin's own session (ListAllSessions has no currentRawID
	// parameter, so IsCurrent arrives false): the template hides its revoke
	// form, sparing the admin a self-sign-out mis-click.
	if c, cerr := r.Cookie(sessionCookieName); cerr == nil && c.Value != "" {
		curHash := auth.HashSessionID(c.Value)
		for i := range list {
			if list[i].IDHash == curHash {
				list[i].IsCurrent = true
			}
		}
	}
	truncated := total > len(list)
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
	// The admin's own session is not revocable here (log out instead). The
	// template omits its revoke form; this guards a hand-crafted POST.
	if c, cerr := r.Cookie(sessionCookieName); cerr == nil && c.Value != "" {
		if auth.HashSessionID(c.Value) == idHash {
			s.redirectFlash(w, r, dest, "cannot revoke your current session; use log out")
			return
		}
	}
	// Resolve the target BEFORE deleting — afterwards the hash no longer maps
	// to a user and the audit event couldn't say whose session was revoked.
	// Best-effort: a failed lookup degrades to empty target attrs, never blocks
	// the revoke. Server-side lookup only; a form-supplied user is not trusted.
	targetUserID, targetUser, oerr := s.store.SessionOwnerByHash(r.Context(), idHash)
	if oerr != nil {
		targetUserID, targetUser = "", ""
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
	EmitAdminSessionRevoked(r.Context(), s.logger, SessionFromContext(r.Context()).Name, idHash, targetUserID, targetUser, n)
	EmitAdminActionMetric(r.Context(), s.logger, "session", "admin_revoke", "ok")
	s.redirectFlash(w, r, dest, "session revoked")
}
