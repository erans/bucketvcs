package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auditlog"
	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// repoAuditStore returns a repo-admin fakeStore primed for the audit tab, with a
// GetRepoFlags that succeeds so the chassis renders.
func repoAuditStore() *fakeStore {
	s := newFakeStore()
	s.perm = auth.PermAdmin
	s.getRepoFlags = func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
		return auth.RepoFlags{}, nil
	}
	return s
}

// TestRepoAudit_ForcesTenantRepoAndIgnoresOverride is THE security test: a
// repo-admin for acme/demo who supplies ?tenant=other&repo=x must NOT be able to
// read another tenant's events. The handler HARD-FORCES Filter.Tenant/Filter.Repo
// to the repo in scope; the client-supplied tenant/repo are ignored.
func TestRepoAudit_ForcesTenantRepoAndIgnoresOverride(t *testing.T) {
	store := repoAuditStore()
	fr := &fakeAuditReader{
		events: []auditlog.Event{
			{
				Ts:     time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC),
				Level:  "INFO",
				Event:  "policy.ref.rejected",
				Tenant: "acme",
				Repo:   "demo",
				Actor:  "alice",
			},
		},
		nextCursor: "older-cursor-key",
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.Audit = fr })
	req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/audit?tenant=other&repo=x&event=policy.", nil)
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if fr.calls != 1 {
		t.Fatalf("Page called %d times, want 1", fr.calls)
	}
	// The security boundary: the forced tenant/repo win over the query override.
	if fr.gotFilter.Tenant != "acme" {
		t.Errorf("Filter.Tenant = %q, want %q (client ?tenant=other MUST be ignored)", fr.gotFilter.Tenant, "acme")
	}
	if fr.gotFilter.Repo != "demo" {
		t.Errorf("Filter.Repo = %q, want %q (client ?repo=x MUST be ignored)", fr.gotFilter.Repo, "demo")
	}
	// The non-tenant/repo filters still pass through.
	if fr.gotFilter.EventPrefix != "policy." {
		t.Errorf("Filter.EventPrefix = %q, want %q", fr.gotFilter.EventPrefix, "policy.")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "policy.ref.rejected") {
		t.Errorf("body missing event name; body: %s", body)
	}
	// Guard against embedded-struct dump (see Task 7 lesson): a correct render
	// uses {{.Event.Event}}, never {{.Event}}, so "map[" must not appear.
	if strings.Contains(body, "map[") {
		t.Errorf("body contains struct-dump artefact \"map[\"; event cell renders the embedded struct instead of the event-name string; body: %s", body)
	}
}

// TestRepoAudit_NonAdmin404 confirms the chassis canAdminRepo gate: a
// non-repo-admin reader gets a uniform 404 and the audit reader is never touched.
func TestRepoAudit_NonAdmin404(t *testing.T) {
	store := newFakeStore()
	store.perm = auth.PermRead
	fr := &fakeAuditReader{}
	h := newTestHandlerWith(store, func(d *Deps) { d.Audit = fr })
	req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/audit", nil)
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	if fr.calls != 0 {
		t.Fatalf("Page called %d times, want 0 (chassis must reject before dispatch)", fr.calls)
	}
}

// TestRepoAudit_NilReaderNotice confirms a nil audit reader renders a
// "not available" notice with 200 (never a 500).
func TestRepoAudit_NilReaderNotice(t *testing.T) {
	store := repoAuditStore()
	h := newTestHandlerWith(store, nil) // Audit stays nil
	req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/audit", nil)
	addSessionCookie(t, req, store, userSession())
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

// TestRepoAudit_BadCursor400: a garbage ?cursor= is client input; the handler
// must map auditlog.ErrBadCursor to 400, not 500.
func TestRepoAudit_BadCursor400(t *testing.T) {
	store := repoAuditStore()
	fr := &fakeAuditReader{err: auditlog.ErrBadCursor}
	h := newTestHandlerWith(store, func(d *Deps) { d.Audit = fr })
	req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/audit?cursor=garbage", nil)
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

// TestRepoAudit_PagerVisibleOnEmptyPage mirrors the admin-page guard: an empty
// filtered page with a next cursor must still offer the [older] link.
func TestRepoAudit_PagerVisibleOnEmptyPage(t *testing.T) {
	store := repoAuditStore()
	fr := &fakeAuditReader{events: nil, nextCursor: "older-cursor-key"}
	h := newTestHandlerWith(store, func(d *Deps) { d.Audit = fr })
	req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/audit?actor=ghost", nil)
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "older-cursor-key") {
		t.Errorf("pager [older] link must render on an empty page with a next cursor; body: %s", body)
	}
}
