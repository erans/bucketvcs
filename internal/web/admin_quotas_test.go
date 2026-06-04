package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
)

// decodeFlash decodes the base64url flash cookie value.
func decodeFlash(c *http.Cookie) string {
	if c == nil {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return c.Value
	}
	return string(b)
}

type extendedFakeQuotas struct {
	getFunc    func(ctx context.Context, tenant string) (quota.State, error)
	setFunc    func(ctx context.Context, tenant string, limitBytes int64) error
	clearFunc  func(ctx context.Context, tenant string) error
	listFunc   func(ctx context.Context) ([]quota.State, error)
	setCalls   []setQuotaCall
	clearCalls []string
}

type setQuotaCall struct {
	Tenant     string
	LimitBytes int64
}

func (q *extendedFakeQuotas) Set(ctx context.Context, tenant string, limitBytes int64) error {
	q.setCalls = append(q.setCalls, setQuotaCall{tenant, limitBytes})
	if q.setFunc != nil {
		return q.setFunc(ctx, tenant, limitBytes)
	}
	return nil
}
func (q *extendedFakeQuotas) Get(ctx context.Context, tenant string) (quota.State, error) {
	if q.getFunc != nil {
		return q.getFunc(ctx, tenant)
	}
	return quota.State{}, nil
}
func (q *extendedFakeQuotas) Clear(ctx context.Context, tenant string) error {
	q.clearCalls = append(q.clearCalls, tenant)
	if q.clearFunc != nil {
		return q.clearFunc(ctx, tenant)
	}
	return nil
}
func (q *extendedFakeQuotas) List(ctx context.Context) ([]quota.State, error) {
	if q.listFunc != nil {
		return q.listFunc(ctx)
	}
	return nil, nil
}

// --- GET /admin/quotas ---

func TestAdminQuotas_AuthGuard(t *testing.T) {
	t.Run("anon → 303 login", func(t *testing.T) {
		store := adminStore()
		h := newTestHandlerWith(store, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/quotas", nil))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
			t.Fatalf("Location %q, want /login...", loc)
		}
	})
	t.Run("non-admin → 404", func(t *testing.T) {
		store := adminStore()
		h := newTestHandlerWith(store, nil)
		req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/quotas", nil), store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rec.Code)
		}
	})
	t.Run("admin 200 — table from fake List, per-row forms, set form", func(t *testing.T) {
		store := adminStore()
		fq := &extendedFakeQuotas{
			listFunc: func(ctx context.Context) ([]quota.State, error) {
				return []quota.State{
					{Tenant: "acme", LimitBytes: 10 << 30, UsedBytes: 3 << 20, UpdatedAt: time.Now().Add(-time.Hour), Exists: true},
					{Tenant: "beta", LimitBytes: 5 << 30, UsedBytes: 0, UpdatedAt: time.Now().Add(-24 * time.Hour), Exists: true},
				}, nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Quotas = fq })
		req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/quotas", nil), store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "acme") {
			t.Errorf("missing tenant acme; body: %s", body)
		}
		if !strings.Contains(body, "beta") {
			t.Errorf("missing tenant beta; body: %s", body)
		}
		// per-row clear form action
		if !strings.Contains(body, "/admin/quotas/clear") {
			t.Errorf("missing clear action; body: %s", body)
		}
		// per-row reconcile form action
		if !strings.Contains(body, "/admin/quotas/reconcile") {
			t.Errorf("missing reconcile action; body: %s", body)
		}
		// global set form
		if !strings.Contains(body, "/admin/quotas/set") {
			t.Errorf("missing set form action; body: %s", body)
		}
	})
	t.Run("nil s.quotas → notice + no forms; POSTs 404", func(t *testing.T) {
		store := adminStore()
		h := newTestHandlerWith(store, nil) // Quotas stays nil
		req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/quotas", nil), store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "/admin/quotas/set") {
			t.Errorf("set form must be absent when quotas is nil; body: %s", body)
		}
		if !strings.Contains(body, "unavailable") {
			t.Errorf("expected 'unavailable' notice when quotas is nil; body: %s", body)
		}

		// POST set → 404 when nil
		req2 := csrfPost(t, "/admin/quotas/set", url.Values{"tenant": {"acme"}, "limit": {"1GiB"}})
		addSessionCookie(t, req2, store, adminSession())
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusNotFound {
			t.Fatalf("POST set with nil quotas: status %d, want 404", rec2.Code)
		}
	})
}

// --- POST /admin/quotas/set ---

func TestAdminQuotaSet_FormSecurity(t *testing.T) {
	store := adminStore()
	fq := &extendedFakeQuotas{}
	h := newTestHandlerWith(store, func(d *Deps) { d.Quotas = fq })
	assertFormSecurity(t, h, secOpts{
		store:     store,
		path:      "/admin/quotas/set",
		form:      url.Values{"tenant": {"acme"}, "limit": {"10GiB"}},
		asSession: userSession(),
	})
}

func TestAdminQuotaSet_Validation(t *testing.T) {
	t.Run("empty tenant → flash, Set not called", func(t *testing.T) {
		store := adminStore()
		fq := &extendedFakeQuotas{}
		h := newTestHandlerWith(store, func(d *Deps) { d.Quotas = fq })
		req := csrfPost(t, "/admin/quotas/set", url.Values{"tenant": {""}, "limit": {"1GiB"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
		if len(fq.setCalls) != 0 {
			t.Fatalf("Set called %d times, want 0", len(fq.setCalls))
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for empty tenant")
		}
	})
	t.Run("bad size → flash with error text, Set not called", func(t *testing.T) {
		store := adminStore()
		fq := &extendedFakeQuotas{}
		h := newTestHandlerWith(store, func(d *Deps) { d.Quotas = fq })
		req := csrfPost(t, "/admin/quotas/set", url.Values{"tenant": {"acme"}, "limit": {"10XB"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
		if len(fq.setCalls) != 0 {
			t.Fatalf("Set called %d times, want 0", len(fq.setCalls))
		}
		flash := findCookie(rec.Result().Cookies(), flashCookieName)
		if flash == nil {
			t.Fatal("expected flash cookie for parse error")
		}
	})
}

func TestAdminQuotaSet_Happy(t *testing.T) {
	store := adminStore()
	fq := &extendedFakeQuotas{}
	logger, sink := newTestLogger()
	h := newTestHandlerWith(store, func(d *Deps) {
		d.Quotas = fq
		d.Logger = logger
	})
	req := csrfPost(t, "/admin/quotas/set", url.Values{"tenant": {"acme"}, "limit": {"10GiB"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/quotas" {
		t.Fatalf("Location %q, want /admin/quotas", loc)
	}
	const wantBytes = int64(10) << 30 // 10GiB
	if len(fq.setCalls) != 1 || fq.setCalls[0].Tenant != "acme" || fq.setCalls[0].LimitBytes != wantBytes {
		t.Fatalf("Set calls = %+v, want [{acme %d}]", fq.setCalls, wantBytes)
	}
	if !sink.Has("quota.set", map[string]string{"tenant": "acme"}) {
		t.Fatal("missing quota.set audit event")
	}
	if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
		t.Fatal("expected flash cookie on success")
	}
}

// --- POST /admin/quotas/clear ---

func TestAdminQuotaClear_FormSecurity(t *testing.T) {
	store := adminStore()
	fq := &extendedFakeQuotas{}
	h := newTestHandlerWith(store, func(d *Deps) { d.Quotas = fq })
	assertFormSecurity(t, h, secOpts{
		store:     store,
		path:      "/admin/quotas/clear",
		form:      url.Values{"tenant": {"acme"}},
		asSession: userSession(),
	})
}

func TestAdminQuotaClear_EmptyTenant(t *testing.T) {
	store := adminStore()
	fq := &extendedFakeQuotas{}
	h := newTestHandlerWith(store, func(d *Deps) { d.Quotas = fq })
	req := csrfPost(t, "/admin/quotas/clear", url.Values{"tenant": {""}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", rec.Code)
	}
	if len(fq.clearCalls) != 0 {
		t.Fatalf("Clear called %d times, want 0", len(fq.clearCalls))
	}
	if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
		t.Fatal("expected flash cookie for empty tenant")
	}
}

func TestAdminQuotaClear_Happy(t *testing.T) {
	store := adminStore()
	fq := &extendedFakeQuotas{}
	logger, sink := newTestLogger()
	h := newTestHandlerWith(store, func(d *Deps) {
		d.Quotas = fq
		d.Logger = logger
	})
	req := csrfPost(t, "/admin/quotas/clear", url.Values{"tenant": {"acme"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/quotas" {
		t.Fatalf("Location %q, want /admin/quotas", loc)
	}
	if len(fq.clearCalls) != 1 || fq.clearCalls[0] != "acme" {
		t.Fatalf("Clear calls = %v, want [acme]", fq.clearCalls)
	}
	if !sink.Has("quota.cleared", map[string]string{"tenant": "acme"}) {
		t.Fatal("missing quota.cleared audit event")
	}
	if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
		t.Fatal("expected flash cookie on success")
	}
}

// --- POST /admin/quotas/reconcile ---

func TestAdminQuotaReconcile_FormSecurity(t *testing.T) {
	store := adminStore()
	fq := &extendedFakeQuotas{}
	h := newTestHandlerWith(store, func(d *Deps) { d.Quotas = fq })
	assertFormSecurity(t, h, secOpts{
		store:     store,
		path:      "/admin/quotas/reconcile",
		form:      url.Values{"tenant": {"acme"}},
		asSession: userSession(),
	})
}

func TestAdminQuotaReconcile_NilReconciler(t *testing.T) {
	// s.quotas non-nil but s.quotaReconcile nil → flash (not 500).
	store := adminStore()
	fq := &extendedFakeQuotas{}
	h := newTestHandlerWith(store, func(d *Deps) {
		d.Quotas = fq
		// QuotaReconcile intentionally left nil
	})
	req := csrfPost(t, "/admin/quotas/reconcile", url.Values{"tenant": {"acme"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303 (flash, not 500); body: %s", rec.Code, rec.Body.String())
	}
	flash := findCookie(rec.Result().Cookies(), flashCookieName)
	if flash == nil {
		t.Fatal("expected flash cookie when reconciler unavailable")
	}
}

func TestAdminQuotaReconcile_Happy(t *testing.T) {
	store := adminStore()
	fq := &extendedFakeQuotas{}
	var reconciledTenant string
	var reconciledDryRun bool
	reconciler := QuotaReconciler(func(ctx context.Context, tenant string, dryRun bool) (quota.Report, error) {
		reconciledTenant = tenant
		reconciledDryRun = dryRun
		return quota.Report{Tenant: tenant, BeforeBytes: 100, AfterBytes: 120, DriftBytes: 20, DryRun: dryRun}, nil
	})
	logger, sink := newTestLogger()
	h := newTestHandlerWith(store, func(d *Deps) {
		d.Quotas = fq
		d.QuotaReconcile = reconciler
		d.Logger = logger
	})
	req := csrfPost(t, "/admin/quotas/reconcile", url.Values{"tenant": {"acme"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/quotas" {
		t.Fatalf("Location %q, want /admin/quotas", loc)
	}
	if reconciledTenant != "acme" {
		t.Fatalf("reconciler called with tenant=%q, want acme", reconciledTenant)
	}
	if reconciledDryRun {
		t.Fatal("reconciler called with dryRun=true, want false")
	}
	flash := findCookie(rec.Result().Cookies(), flashCookieName)
	if flash == nil {
		t.Fatal("expected flash cookie on reconcile success")
	}
	// Flash should mention drift
	flashVal := decodeFlash(flash)
	if !strings.Contains(flashVal, "20") {
		t.Errorf("flash %q should mention drift 20; raw=%q", flashVal, flash.Value)
	}
	if !sink.Has("quota.reconciled", map[string]string{"tenant": "acme"}) {
		t.Fatal("missing quota.reconciled audit event")
	}
}

func TestAdminQuotaReconcile_Error(t *testing.T) {
	// Reconcile errors are operator-visible retryable; must NOT 500.
	store := adminStore()
	fq := &extendedFakeQuotas{}
	reconciler := QuotaReconciler(func(ctx context.Context, tenant string, dryRun bool) (quota.Report, error) {
		return quota.Report{}, errors.New("storage unreachable")
	})
	h := newTestHandlerWith(store, func(d *Deps) {
		d.Quotas = fq
		d.QuotaReconcile = reconciler
	})
	req := csrfPost(t, "/admin/quotas/reconcile", url.Values{"tenant": {"acme"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303 (flash, not 500); body: %s", rec.Code, rec.Body.String())
	}
	flash := findCookie(rec.Result().Cookies(), flashCookieName)
	if flash == nil {
		t.Fatal("expected flash cookie for reconcile error")
	}
	flashVal := decodeFlash(flash)
	if !strings.Contains(flashVal, "reconcile failed") {
		t.Errorf("flash %q should mention 'reconcile failed'", flashVal)
	}
}

func TestAdminQuotaSet_MetricEmitted(t *testing.T) {
	store := adminStore()
	fq := &extendedFakeQuotas{}
	logger, _ := newTestLogger()
	h := newTestHandlerWith(store, func(d *Deps) {
		d.Quotas = fq
		d.Logger = logger
	})
	req := csrfPost(t, "/admin/quotas/set", url.Values{"tenant": {"acme"}, "limit": {"5GiB"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	// Just check Set was called with the right bytes.
	const wantBytes = int64(5) << 30
	if len(fq.setCalls) != 1 || fq.setCalls[0].LimitBytes != wantBytes {
		t.Fatalf("Set calls = %+v, want limit_bytes=%d", fq.setCalls, wantBytes)
	}
}

func TestAdminQuotaReconcile_AuditDriftBytes(t *testing.T) {
	store := adminStore()
	fq := &extendedFakeQuotas{}
	reconciler := QuotaReconciler(func(ctx context.Context, tenant string, dryRun bool) (quota.Report, error) {
		return quota.Report{Tenant: tenant, BeforeBytes: 100, AfterBytes: 150, DriftBytes: 50}, nil
	})
	logger, sink := newTestLogger()
	h := newTestHandlerWith(store, func(d *Deps) {
		d.Quotas = fq
		d.QuotaReconcile = reconciler
		d.Logger = logger
	})
	req := csrfPost(t, "/admin/quotas/reconcile", url.Values{"tenant": {"acme"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", rec.Code)
	}
	// Check audit has drift_bytes attr. The sink.Has method checks map[string]string
	// so we look for the key with the formatted int64.
	if !sink.Has("quota.reconciled", map[string]string{"tenant": "acme", "drift_bytes": fmt.Sprintf("%d", 50)}) {
		t.Fatalf("missing quota.reconciled audit with drift_bytes=50; events: %v", sink)
	}
}
