package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auditlog"
)

// fakeAuditReader implements AuditReader. It records the Filter + cursor it was
// last called with and returns canned events + a next cursor.
type fakeAuditReader struct {
	events     []auditlog.Event
	nextCursor string
	err        error

	gotFilter auditlog.Filter
	gotCursor string
	calls     int
}

func (f *fakeAuditReader) Page(ctx context.Context, filter auditlog.Filter, cursor string) ([]auditlog.Event, string, error) {
	f.calls++
	f.gotFilter = filter
	f.gotCursor = cursor
	if f.err != nil {
		return nil, "", f.err
	}
	return f.events, f.nextCursor, nil
}

func TestAdminAudit_RendersAndFilters(t *testing.T) {
	store := adminStore()
	fr := &fakeAuditReader{
		events: []auditlog.Event{
			{
				Ts:     time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC),
				Level:  "INFO",
				Event:  "policy.ref.rejected",
				Tenant: "acme",
				Repo:   "app",
				Actor:  "alice",
			},
		},
		nextCursor: "older-cursor-key",
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.Audit = fr })
	req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/audit?event=policy.&tenant=acme", nil), store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"policy.ref.rejected", "acme", "alice"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body: %s", want, body)
		}
	}
	// Guard against embedded-struct dump: a correct render never emits "map[".
	// If {{.Event}} resolves to the embedded auditlog.Event struct instead of the
	// Event.Event string field, the Attrs map is printed as "map[...]".
	if strings.Contains(body, "map[") {
		t.Errorf("body contains struct-dump artefact \"map[\"; event cell is rendering the embedded struct instead of the event-name string; body: %s", body)
	}
	// Guard pager cursor: the [older] link must carry the next-page cursor.
	if !strings.Contains(body, "older-cursor-key") {
		t.Errorf("body missing pager cursor %q; body: %s", "older-cursor-key", body)
	}
	if fr.calls != 1 {
		t.Fatalf("Page called %d times, want 1", fr.calls)
	}
	if fr.gotFilter.EventPrefix != "policy." {
		t.Errorf("EventPrefix = %q, want %q", fr.gotFilter.EventPrefix, "policy.")
	}
	if fr.gotFilter.Tenant != "acme" {
		t.Errorf("Tenant = %q, want %q", fr.gotFilter.Tenant, "acme")
	}
}

func TestAdminAudit_NonAdmin404(t *testing.T) {
	store := adminStore()
	fr := &fakeAuditReader{}
	h := newTestHandlerWith(store, func(d *Deps) { d.Audit = fr })
	req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/audit", nil), store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestAdminAudit_NilReaderNotice(t *testing.T) {
	store := adminStore()
	h := newTestHandlerWith(store, nil) // Audit stays nil
	req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/audit", nil), store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "not available") {
		t.Errorf("expected 'not available' notice when audit is nil; body: %s", body)
	}
}

// TestAdminAudit_PagerVisibleOnEmptyPage: pagination is object-based, so a page
// can yield zero matching rows while older objects still hold matches. The
// [older] link must render even when Rows is empty.
func TestAdminAudit_PagerVisibleOnEmptyPage(t *testing.T) {
	store := adminStore()
	fr := &fakeAuditReader{events: nil, nextCursor: "older-cursor-key"}
	h := newTestHandlerWith(store, func(d *Deps) { d.Audit = fr })
	req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/audit?actor=ghost", nil), store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no audit events match") {
		t.Errorf("expected empty-state text; body: %s", body)
	}
	if !strings.Contains(body, "older-cursor-key") {
		t.Errorf("pager [older] link must render on an empty page with a next cursor; body: %s", body)
	}
}

// TestParseAuditFilter_UntilInclusiveOfNamedDayOnly: ?until=2026-06-01 must
// include the whole named day but NOT the first instant of June 2 —
// Filter.Until is an inclusive bound, so the parsed value must be the last
// instant of the named day, not next-day midnight.
func TestParseAuditFilter_UntilInclusiveOfNamedDayOnly(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/audit?until=2026-06-01", nil)
	f, _, until := parseAuditFilter(req)
	if until != "2026-06-01" {
		t.Fatalf("raw until = %q, want 2026-06-01", until)
	}
	endOfDay := auditlog.Event{Ts: time.Date(2026, 6, 1, 23, 59, 59, 999999999, time.UTC)}
	if !f.Match(endOfDay) {
		t.Errorf("event at the last instant of the named day must match")
	}
	nextMidnight := auditlog.Event{Ts: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)}
	if f.Match(nextMidnight) {
		t.Errorf("event at next-day midnight must NOT match an until=named-day filter")
	}
}
