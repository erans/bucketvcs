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
