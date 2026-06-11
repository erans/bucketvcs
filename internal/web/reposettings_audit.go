package web

import (
	"errors"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auditlog"
)

// repoAuditData is the view-model for the per-repo audit tab. The forced
// Tenant/Repo are the security boundary: a repo-admin can only ever see their
// own repo's events, regardless of any ?tenant=/?repo= query override.
type repoAuditData struct {
	base
	Tenant, Repo string
	IsAdmin      bool
	Enabled      bool
	Rows         []auditRow
	NextCursor   string
	// Echoed filter values (repopulate the form + carry into the pager href).
	FEvent, FActor, FSince, FUntil string
}

// repoSettingsAudit renders the AUDIT tab: a repo-scoped view of the audit log.
// The chassis (handleRepoSettings) has already verified canAdminRepo and the
// repo's existence. This handler HARD-FORCES Filter.Tenant/Filter.Repo to the
// repo in scope so a repo-admin can never read another tenant's events even with
// a crafted ?tenant=/?repo= query. A nil audit reader renders a "not available"
// notice (never 500).
func (s *server) repoSettingsAudit(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d := repoAuditData{
		base:    base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:  sr.tenant,
		Repo:    sr.repo,
		IsAdmin: isGlobalAdmin(r),
		Enabled: s.audit != nil,
	}
	if s.audit != nil {
		f, since, until := parseAuditFilter(r)
		// HARD cross-tenant boundary: force this repo, ignore any client tenant/repo.
		f.Tenant = sr.tenant
		f.Repo = sr.repo
		d.FEvent, d.FActor, d.FSince, d.FUntil = r.URL.Query().Get("event"), r.URL.Query().Get("actor"), since, until
		evs, next, err := s.audit.Page(r.Context(), f, r.URL.Query().Get("cursor"))
		if errors.Is(err, auditlog.ErrBadCursor) {
			s.renderError(w, r, http.StatusBadRequest, "bad cursor")
			return
		}
		if err != nil {
			s.logger.Error("repo audit: page", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		d.Rows = toAuditRows(evs)
		d.NextCursor = next
	}
	if err := s.renderBuffered(w, "reposettings_audit.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_audit", http.StatusOK)
}
