package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// webhookStore returns a repo-admin fakeStore primed for the webhooks tab.
func webhookStore() *fakeStore {
	s := newFakeStore()
	s.perm = auth.PermAdmin
	s.getRepoFlags = func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
		return auth.RepoFlags{}, nil
	}
	return s
}

// stdEndpoint is a fixture endpoint for use across tests.
var stdEndpoint = webhooks.Endpoint{
	ID:            42,
	Tenant:        "acme",
	Repo:          "demo",
	URL:           "https://example.com/hook",
	SecretPreview: "abc123...",
	EventMask:     webhooks.EventPush | webhooks.EventLFSUpload,
	Active:        true,
	CreatedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
}

// extendedFakeWebhooks extends fakeWebhooks with recording stubs for all methods.
type extendedFakeWebhooks struct {
	createFn         func(ctx context.Context, in webhooks.EndpointInput) (webhooks.Endpoint, error)
	listFn           func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error)
	removeFn         func(ctx context.Context, id int64) error
	enableFn         func(ctx context.Context, id int64) error
	disableFn        func(ctx context.Context, id int64) error
	rotateSecretFn   func(ctx context.Context, id int64) (string, error)
	listDeliveriesFn func(ctx context.Context, f webhooks.ListDeliveriesFilter) ([]webhooks.Delivery, error)
	replayDeliveryFn func(ctx context.Context, id string) error
	enqueueFn        func(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error
}

func (w *extendedFakeWebhooks) Create(ctx context.Context, in webhooks.EndpointInput) (webhooks.Endpoint, error) {
	if w.createFn != nil {
		return w.createFn(ctx, in)
	}
	return webhooks.Endpoint{ID: 42, Secret: "test-secret-once", URL: in.URL, EventMask: in.EventMask, Active: true, CreatedAt: time.Now()}, nil
}
func (w *extendedFakeWebhooks) List(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
	if w.listFn != nil {
		return w.listFn(ctx, tenant, repo)
	}
	return nil, nil
}
func (w *extendedFakeWebhooks) Remove(ctx context.Context, id int64) error {
	if w.removeFn != nil {
		return w.removeFn(ctx, id)
	}
	return nil
}
func (w *extendedFakeWebhooks) Enable(ctx context.Context, id int64) error {
	if w.enableFn != nil {
		return w.enableFn(ctx, id)
	}
	return nil
}
func (w *extendedFakeWebhooks) Disable(ctx context.Context, id int64) error {
	if w.disableFn != nil {
		return w.disableFn(ctx, id)
	}
	return nil
}
func (w *extendedFakeWebhooks) RotateSecret(ctx context.Context, id int64) (string, error) {
	if w.rotateSecretFn != nil {
		return w.rotateSecretFn(ctx, id)
	}
	return "new-secret", nil
}
func (w *extendedFakeWebhooks) ListDeliveries(ctx context.Context, f webhooks.ListDeliveriesFilter) ([]webhooks.Delivery, error) {
	if w.listDeliveriesFn != nil {
		return w.listDeliveriesFn(ctx, f)
	}
	return nil, nil
}
func (w *extendedFakeWebhooks) ReplayDelivery(ctx context.Context, id string) error {
	if w.replayDeliveryFn != nil {
		return w.replayDeliveryFn(ctx, id)
	}
	return nil
}
func (w *extendedFakeWebhooks) Enqueue(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error {
	if w.enqueueFn != nil {
		return w.enqueueFn(ctx, event, tenant, repo, actor, payload)
	}
	return nil
}

// TestRepoSettingsWebhooksGet covers the GET /{t}/{r}/settings/webhooks page.
func TestRepoSettingsWebhooksGet(t *testing.T) {
	t.Run("nil webhooks → renders notice, no forms", func(t *testing.T) {
		store := webhookStore()
		h := newTestHandlerWith(store, nil) // Webhooks stays nil
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/webhooks", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "not enabled") {
			t.Fatalf("expected 'not enabled' notice; body=%s", body)
		}
		// Must not render the add form when disabled
		if strings.Contains(body, "add endpoint") {
			t.Fatalf("add form must not appear when webhooks disabled; body=%s", body)
		}
	})

	t.Run("enabled with endpoints → table + forms", func(t *testing.T) {
		store := webhookStore()
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/webhooks", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{
			"https://example.com/hook", // endpoint URL
			"abc123...",                // secret preview
			"disable",                  // active → disable button shown
			"rotate secret",
			"remove",
			"deliveries",
			"add endpoint", // add form
			"csrf_token",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("webhooks page missing %q; body=%s", want, body)
			}
		}
	})

	t.Run("reader → 404", func(t *testing.T) {
		store := newFakeStore()
		store.perm = auth.PermRead
		wh := &extendedFakeWebhooks{}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/webhooks", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestRepoSettingsWebhooksAdd covers POST .../webhooks/add.
func TestRepoSettingsWebhooksAdd(t *testing.T) {
	t.Run("form security: reader → 404", func(t *testing.T) {
		store := newFakeStore()
		store.perm = auth.PermRead
		wh := &extendedFakeWebhooks{}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		assertFormSecurity(t, h, secOpts{
			store:     store,
			path:      "/acme/demo/settings/webhooks/add",
			form:      url.Values{"url": {"https://x.com/hook"}, "events": {"all"}},
			asSession: userSession(),
		})
	})

	t.Run("nil webhooks → 404 on POST", func(t *testing.T) {
		store := webhookStore()
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/webhooks/add", url.Values{"url": {"https://x.com/hook"}, "events": {"all"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("nil webhooks add: status %d, want 404", rec.Code)
		}
	})

	t.Run("bad url (no scheme) → flash, Create not called", func(t *testing.T) {
		store := webhookStore()
		var called bool
		wh := &extendedFakeWebhooks{
			createFn: func(ctx context.Context, in webhooks.EndpointInput) (webhooks.Endpoint, error) {
				called = true
				return webhooks.Endpoint{}, nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := csrfPost(t, "/acme/demo/settings/webhooks/add", url.Values{"url": {"example.com/hook"}, "events": {"all"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("Create must not be called for bad URL")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for bad URL")
		}
	})

	t.Run("bad events → flash, Create not called", func(t *testing.T) {
		store := webhookStore()
		var called bool
		wh := &extendedFakeWebhooks{
			createFn: func(ctx context.Context, in webhooks.EndpointInput) (webhooks.Endpoint, error) {
				called = true
				return webhooks.Endpoint{}, nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := csrfPost(t, "/acme/demo/settings/webhooks/add", url.Values{"url": {"https://x.com/hook"}, "events": {"bogus.event"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("Create must not be called for bad events")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for bad events")
		}
	})

	t.Run("happy path: Create called + secret-once rendered + audit", func(t *testing.T) {
		store := webhookStore()
		var gotInput webhooks.EndpointInput
		wh := &extendedFakeWebhooks{
			createFn: func(ctx context.Context, in webhooks.EndpointInput) (webhooks.Endpoint, error) {
				gotInput = in
				return webhooks.Endpoint{
					ID: 99, URL: in.URL, EventMask: in.EventMask,
					Secret: "mysecrettoken", Active: true, CreatedAt: time.Now(),
				}, nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh; d.Logger = logger })
		mask, _ := webhooks.ParseEvents("push,lfs.upload")
		req := csrfPost(t, "/acme/demo/settings/webhooks/add", url.Values{
			"url":    {"https://x.com/hook"},
			"events": {"push,lfs.upload"},
		})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		// secret-once page: 200, no redirect
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		// Secret must appear exactly once in the page
		count := strings.Count(body, "mysecrettoken")
		if count != 1 {
			t.Fatalf("secret appears %d times, want exactly 1; body=%s", count, body)
		}
		// Cache-Control must be no-store
		if !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("Cache-Control must contain no-store; got %q", rec.Header().Get("Cache-Control"))
		}
		// Create called with correct params
		if gotInput.Tenant != "acme" || gotInput.Repo != "demo" {
			t.Fatalf("Create tenant=%q repo=%q, want acme/demo", gotInput.Tenant, gotInput.Repo)
		}
		if gotInput.URL != "https://x.com/hook" {
			t.Fatalf("Create URL=%q, want https://x.com/hook", gotInput.URL)
		}
		if gotInput.EventMask != mask {
			t.Fatalf("Create EventMask=%v, want %v", gotInput.EventMask, mask)
		}
		// Audit event
		if !sink.Has("webhooks.endpoint_created", map[string]string{"tenant": "acme", "repo": "demo"}) {
			t.Fatal("missing webhooks.endpoint_created audit event")
		}
	})
}

// TestRepoSettingsWebhooksEnable covers POST .../webhooks/enable.
func TestRepoSettingsWebhooksEnable(t *testing.T) {
	t.Run("foreign endpoint id → 404, Enable not called", func(t *testing.T) {
		store := webhookStore()
		var called bool
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				// only owns id=42
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
			enableFn: func(ctx context.Context, id int64) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := csrfPost(t, "/acme/demo/settings/webhooks/enable", url.Values{"id": {"999"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("Enable must not be called for foreign endpoint")
		}
	})

	t.Run("happy path: Enable + audit + 303", func(t *testing.T) {
		store := webhookStore()
		var enabledID int64
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
			enableFn: func(ctx context.Context, id int64) error {
				enabledID = id
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/webhooks/enable", url.Values{"id": {"42"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings/webhooks" {
			t.Fatalf("Location %q, want /acme/demo/settings/webhooks", loc)
		}
		if enabledID != 42 {
			t.Fatalf("Enable(%d), want 42", enabledID)
		}
		if !sink.Has("webhooks.endpoint_enabled", map[string]string{"tenant": "acme", "repo": "demo"}) {
			t.Fatal("missing webhooks.endpoint_enabled audit event")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on enable success")
		}
	})
}

// TestRepoSettingsWebhooksDisable covers POST .../webhooks/disable.
func TestRepoSettingsWebhooksDisable(t *testing.T) {
	t.Run("foreign id → 404, Disable not called", func(t *testing.T) {
		store := webhookStore()
		var called bool
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
			disableFn: func(ctx context.Context, id int64) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := csrfPost(t, "/acme/demo/settings/webhooks/disable", url.Values{"id": {"999"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rec.Code)
		}
		if called {
			t.Fatal("Disable must not be called for foreign endpoint")
		}
	})

	t.Run("happy path: Disable + audit + 303", func(t *testing.T) {
		store := webhookStore()
		var disabledID int64
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
			disableFn: func(ctx context.Context, id int64) error {
				disabledID = id
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/webhooks/disable", url.Values{"id": {"42"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if disabledID != 42 {
			t.Fatalf("Disable(%d), want 42", disabledID)
		}
		if !sink.Has("webhooks.endpoint_disabled", map[string]string{"tenant": "acme", "repo": "demo"}) {
			t.Fatal("missing webhooks.endpoint_disabled audit event")
		}
	})
}

// TestRepoSettingsWebhooksRemove covers POST .../webhooks/remove.
func TestRepoSettingsWebhooksRemove(t *testing.T) {
	t.Run("foreign id → 404, Remove not called", func(t *testing.T) {
		store := webhookStore()
		var called bool
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
			removeFn: func(ctx context.Context, id int64) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := csrfPost(t, "/acme/demo/settings/webhooks/remove", url.Values{"id": {"999"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rec.Code)
		}
		if called {
			t.Fatal("Remove must not be called for foreign endpoint")
		}
	})

	t.Run("happy path: Remove + audit + 303", func(t *testing.T) {
		store := webhookStore()
		var removedID int64
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
			removeFn: func(ctx context.Context, id int64) error {
				removedID = id
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/webhooks/remove", url.Values{"id": {"42"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if removedID != 42 {
			t.Fatalf("Remove(%d), want 42", removedID)
		}
		if !sink.Has("webhooks.endpoint_removed", map[string]string{"tenant": "acme", "repo": "demo"}) {
			t.Fatal("missing webhooks.endpoint_removed audit event")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on remove success")
		}
	})
}

// TestRepoSettingsWebhooksRotate covers POST .../webhooks/rotate.
func TestRepoSettingsWebhooksRotate(t *testing.T) {
	t.Run("foreign id → 404, RotateSecret not called", func(t *testing.T) {
		store := webhookStore()
		var called bool
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
			rotateSecretFn: func(ctx context.Context, id int64) (string, error) {
				called = true
				return "", nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := csrfPost(t, "/acme/demo/settings/webhooks/rotate", url.Values{"id": {"999"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rec.Code)
		}
		if called {
			t.Fatal("RotateSecret must not be called for foreign endpoint")
		}
	})

	t.Run("happy path: RotateSecret → secret-once + audit", func(t *testing.T) {
		store := webhookStore()
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
			rotateSecretFn: func(ctx context.Context, id int64) (string, error) {
				return "rotated-secret-value", nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/webhooks/rotate", url.Values{"id": {"42"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (secret-once page); body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "rotated-secret-value") {
			t.Fatalf("secret-once must contain the new secret; body=%s", body)
		}
		if !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("Cache-Control must contain no-store; got %q", rec.Header().Get("Cache-Control"))
		}
		if !sink.Has("webhooks.endpoint_secret_rotated", map[string]string{"tenant": "acme", "repo": "demo"}) {
			t.Fatal("missing webhooks.endpoint_secret_rotated audit event")
		}
	})
}

// TestRepoSettingsWebhooksDeliveries covers GET .../webhooks/deliveries?endpoint=<id>.
func TestRepoSettingsWebhooksDeliveries(t *testing.T) {
	t.Run("nil webhooks → 404", func(t *testing.T) {
		store := webhookStore()
		h := newTestHandlerWith(store, nil)
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/webhooks/deliveries?endpoint=42", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rec.Code)
		}
	})

	t.Run("foreign endpoint id → 404, ListDeliveries not called", func(t *testing.T) {
		store := webhookStore()
		var called bool
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil // only owns 42
			},
			listDeliveriesFn: func(ctx context.Context, f webhooks.ListDeliveriesFilter) ([]webhooks.Delivery, error) {
				called = true
				return nil, nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/webhooks/deliveries?endpoint=999", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rec.Code)
		}
		if called {
			t.Fatal("ListDeliveries must not be called for foreign endpoint")
		}
	})

	t.Run("happy path: deliveries listed with replay forms", func(t *testing.T) {
		store := webhookStore()
		now := time.Now()
		del := webhooks.Delivery{
			ID:         "del-uuid-1",
			EndpointID: 42,
			EventType:  "push",
			Status:     "dead_letter",
			Attempts:   5,
			CreatedAt:  now.Add(-1 * time.Hour),
			LastError:  "connection refused",
		}
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
			listDeliveriesFn: func(ctx context.Context, f webhooks.ListDeliveriesFilter) ([]webhooks.Delivery, error) {
				if f.EndpointID != 42 || f.Limit != 50 {
					return nil, fmt.Errorf("unexpected filter: %+v", f)
				}
				return []webhooks.Delivery{del}, nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/webhooks/deliveries?endpoint=42", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{
			"del-uuid-1",         // delivery id
			"push",               // event type
			"dead_letter",        // status
			"connection refused", // last error
			"replay",             // replay button
			"csrf_token",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("deliveries page missing %q; body=%s", want, body)
			}
		}
	})
}

// TestRepoSettingsWebhooksReplay covers POST .../webhooks/replay.
func TestRepoSettingsWebhooksReplay(t *testing.T) {
	t.Run("nil webhooks → 404", func(t *testing.T) {
		store := webhookStore()
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/webhooks/replay",
			url.Values{"delivery_id": {"del-1"}, "endpoint_id": {"42"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("nil webhooks replay: status %d, want 404", rec.Code)
		}
	})

	t.Run("foreign endpoint → 404, ReplayDelivery not called", func(t *testing.T) {
		store := webhookStore()
		var called bool
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil // owns 42 only
			},
			replayDeliveryFn: func(ctx context.Context, id string) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := csrfPost(t, "/acme/demo/settings/webhooks/replay",
			url.Values{"delivery_id": {"del-1"}, "endpoint_id": {"999"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("foreign endpoint: status %d, want 404", rec.Code)
		}
		if called {
			t.Fatal("ReplayDelivery must not be called for foreign endpoint")
		}
	})

	t.Run("service error → flash, preserve endpoint param", func(t *testing.T) {
		store := webhookStore()
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
			replayDeliveryFn: func(ctx context.Context, id string) error {
				return fmt.Errorf("webhooks: cannot replay %s: row is in_flight", id)
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := csrfPost(t, "/acme/demo/settings/webhooks/replay",
			url.Values{"delivery_id": {"del-1"}, "endpoint_id": {"42"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("service error: status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		loc := rec.Header().Get("Location")
		if !strings.Contains(loc, "endpoint=42") {
			t.Fatalf("Location %q must contain endpoint=42 to preserve deliveries view", loc)
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for service error")
		}
	})

	t.Run("happy path: ReplayDelivery + audit + 303 preserving endpoint param", func(t *testing.T) {
		store := webhookStore()
		var replayedID string
		wh := &extendedFakeWebhooks{
			listFn: func(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
				return []webhooks.Endpoint{stdEndpoint}, nil
			},
			replayDeliveryFn: func(ctx context.Context, id string) error {
				replayedID = id
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/webhooks/replay",
			url.Values{"delivery_id": {"del-uuid-99"}, "endpoint_id": {"42"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		loc := rec.Header().Get("Location")
		if !strings.Contains(loc, "endpoint=42") {
			t.Fatalf("Location %q must preserve endpoint=42", loc)
		}
		if replayedID != "del-uuid-99" {
			t.Fatalf("ReplayDelivery(%q), want del-uuid-99", replayedID)
		}
		if !sink.Has("webhooks.delivery_replayed", map[string]string{"tenant": "acme", "repo": "demo"}) {
			t.Fatal("missing webhooks.delivery_replayed audit event")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on replay success")
		}
	})
}

// TestTruncateRunes verifies the server-side truncation helper.
func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello…"},
		{"日本語テスト", 4, "日本語テ…"},
		{"", 80, ""},
	}
	for _, tc := range tests {
		got := truncateRunes(tc.in, tc.n)
		if got != tc.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
