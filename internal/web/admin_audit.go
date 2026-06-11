package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auditlog"
)

// auditRow is one rendered audit event row. It embeds the decoded Event and adds
// a preformatted UTC timestamp string. Shared with the per-repo audit tab.
type auditRow struct {
	auditlog.Event
	TimeStr string
}

type adminAuditData struct {
	base
	Enabled    bool
	Rows       []auditRow
	NextCursor string
	// Echoed filter values (repopulate the form + carry into the pager href).
	FEvent  string
	FTenant string
	FRepo   string
	FActor  string
	FSince  string
	FUntil  string
}

// handleAdminAudit renders GET /admin/audit: the global audit-log viewer with a
// filter form and an object-cursor pager. Requires global admin. A nil audit
// reader renders a "not available" notice (never 500).
func (s *server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess := SessionFromContext(r.Context())
	d := adminAuditData{
		base:    base{Session: sess, CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Enabled: s.audit != nil,
	}
	if s.audit != nil {
		f, since, until := parseAuditFilter(r)
		d.FEvent = f.EventPrefix
		d.FTenant = f.Tenant
		d.FRepo = f.Repo
		d.FActor = f.Actor
		d.FSince = since
		d.FUntil = until
		events, next, err := s.audit.Page(r.Context(), f, r.URL.Query().Get("cursor"))
		if errors.Is(err, auditlog.ErrBadCursor) {
			s.renderError(w, r, http.StatusBadRequest, "bad cursor")
			return
		}
		if err != nil {
			s.logger.Error("admin audit: page", "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		d.Rows = toAuditRows(events)
		d.NextCursor = next
	}
	if err := s.renderBuffered(w, "admin_audit.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "admin_audit", http.StatusOK)
}

// parseAuditFilter builds an auditlog.Filter from the request query. event maps
// to EventPrefix; tenant/repo/actor match exactly. since/until are parsed as
// "2006-01-02" dates; until is extended to the last instant of the named day so
// the whole day is inclusive.
// Bad dates are ignored (the bound is left zero). It returns the raw since/until
// query strings so the caller can echo them back into the form. Shared with the
// per-repo audit tab.
func parseAuditFilter(r *http.Request) (auditlog.Filter, string, string) {
	q := r.URL.Query()
	f := auditlog.Filter{
		EventPrefix: q.Get("event"),
		Tenant:      q.Get("tenant"),
		Repo:        q.Get("repo"),
		Actor:       q.Get("actor"),
	}
	since := q.Get("since")
	until := q.Get("until")
	if since != "" {
		if t, err := time.Parse("2006-01-02", since); err == nil {
			f.Since = t
		}
	}
	if until != "" {
		if t, err := time.Parse("2006-01-02", until); err == nil {
			// Inclusive of the named day: Filter.Until is itself inclusive, so
			// the bound is the LAST instant of the day — a flat +24h would also
			// admit an event stamped exactly at the next day's midnight.
			f.Until = t.Add(24*time.Hour - time.Nanosecond)
		}
	}
	return f, since, until
}

// toAuditRows formats decoded events for display. Shared with the per-repo
// audit tab. A zero timestamp (missing/corrupt ts) is flagged visibly rather
// than rendering as year 1 — such events also sort last and interact oddly
// with date filters, so the operator should know the source line is damaged.
func toAuditRows(events []auditlog.Event) []auditRow {
	rows := make([]auditRow, 0, len(events))
	for _, e := range events {
		ts := "(no timestamp)"
		if !e.Ts.IsZero() {
			ts = e.Ts.UTC().Format("2006-01-02 15:04:05Z")
		}
		rows = append(rows, auditRow{
			Event:   e,
			TimeStr: ts,
		})
	}
	return rows
}
