package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/buildtrigger"
)

// triggersStore returns a repo-admin fakeStore primed for the triggers tab.
func triggersStore() *fakeStore {
	s := newFakeStore()
	s.perm = auth.PermAdmin
	s.getRepoFlags = func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
		return auth.RepoFlags{}, nil
	}
	return s
}

// fakeTriggers implements TriggerAdmin. Most methods return zero values; List
// and ListDeliveriesPage return canned data wired via the closures. Extended in
// later tasks (Tasks 10-12) for the create/edit/delivery handlers.
type fakeTriggers struct {
	createFn             func(ctx context.Context, in buildtrigger.TriggerInput) (buildtrigger.Trigger, error)
	listFn               func(ctx context.Context, tenant, repo string) ([]buildtrigger.Trigger, error)
	getFn                func(ctx context.Context, id string) (buildtrigger.Trigger, error)
	editFn               func(ctx context.Context, id string, in buildtrigger.EditInput) (buildtrigger.Trigger, error)
	rotateSecretFn       func(ctx context.Context, id string) (string, error)
	enableFn             func(ctx context.Context, id string) error
	disableFn            func(ctx context.Context, id string) error
	removeFn             func(ctx context.Context, id string) error
	listDeliveriesPageFn func(ctx context.Context, triggerID, status string, before time.Time, limit int) ([]buildtrigger.Delivery, error)
	recentDeliveryIDsFn  func(ctx context.Context, triggerID string, n int) ([]string, error)
	getDeliveryFn        func(ctx context.Context, id string) (buildtrigger.Delivery, error)
	replayDeliveryFn     func(ctx context.Context, id string) error
}

func (t *fakeTriggers) Create(ctx context.Context, in buildtrigger.TriggerInput) (buildtrigger.Trigger, error) {
	if t.createFn != nil {
		return t.createFn(ctx, in)
	}
	return buildtrigger.Trigger{}, nil
}
func (t *fakeTriggers) List(ctx context.Context, tenant, repo string) ([]buildtrigger.Trigger, error) {
	if t.listFn != nil {
		return t.listFn(ctx, tenant, repo)
	}
	return nil, nil
}
func (t *fakeTriggers) Get(ctx context.Context, id string) (buildtrigger.Trigger, error) {
	if t.getFn != nil {
		return t.getFn(ctx, id)
	}
	return buildtrigger.Trigger{}, nil
}
func (t *fakeTriggers) Edit(ctx context.Context, id string, in buildtrigger.EditInput) (buildtrigger.Trigger, error) {
	if t.editFn != nil {
		return t.editFn(ctx, id, in)
	}
	return buildtrigger.Trigger{}, nil
}
func (t *fakeTriggers) RotateSecret(ctx context.Context, id string) (string, error) {
	if t.rotateSecretFn != nil {
		return t.rotateSecretFn(ctx, id)
	}
	return "", nil
}
func (t *fakeTriggers) Enable(ctx context.Context, id string) error {
	if t.enableFn != nil {
		return t.enableFn(ctx, id)
	}
	return nil
}
func (t *fakeTriggers) Disable(ctx context.Context, id string) error {
	if t.disableFn != nil {
		return t.disableFn(ctx, id)
	}
	return nil
}
func (t *fakeTriggers) Remove(ctx context.Context, id string) error {
	if t.removeFn != nil {
		return t.removeFn(ctx, id)
	}
	return nil
}
func (t *fakeTriggers) ListDeliveriesPage(ctx context.Context, triggerID, status string, before time.Time, limit int) ([]buildtrigger.Delivery, error) {
	if t.listDeliveriesPageFn != nil {
		return t.listDeliveriesPageFn(ctx, triggerID, status, before, limit)
	}
	return nil, nil
}
func (t *fakeTriggers) RecentDeliveryIDs(ctx context.Context, triggerID string, n int) ([]string, error) {
	if t.recentDeliveryIDsFn != nil {
		return t.recentDeliveryIDsFn(ctx, triggerID, n)
	}
	return nil, nil
}
func (t *fakeTriggers) GetDelivery(ctx context.Context, id string) (buildtrigger.Delivery, error) {
	if t.getDeliveryFn != nil {
		return t.getDeliveryFn(ctx, id)
	}
	return buildtrigger.Delivery{}, nil
}
func (t *fakeTriggers) ReplayDelivery(ctx context.Context, id string) error {
	if t.replayDeliveryFn != nil {
		return t.replayDeliveryFn(ctx, id)
	}
	return nil
}

// TestTriggersNewForm_DefaultAndKindSwap covers GET .../triggers/new: the
// default (generic) form, and the htmx-less ?kind=codebuild variant that must
// list the configured AWS connector names in the connector <select>.
func TestTriggersNewForm_DefaultAndKindSwap(t *testing.T) {
	store := triggersStore()
	h := newTestHandlerWith(store, func(d *Deps) {
		d.Triggers = &fakeTriggers{}
		d.Connectors = ConnectorNames{AWS: []string{"prod"}}
	})

	t.Run("default generic form", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/triggers/new", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "add trigger") {
			t.Fatalf("form missing 'add trigger'; body=%s", body)
		}
	})

	t.Run("codebuild kind shows connector option", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/triggers/new?kind=codebuild", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "prod") {
			t.Fatalf("codebuild form missing connector option 'prod'; body=%s", rec.Body.String())
		}
	})
}

// TestTriggersNewForm_HXModalReturnsFormNotEmpty guards the htmx modal-open
// path: an HX-Request GET with no kind query (and no id) must render the form
// CONTENT fragment — non-empty, the form markup, but WITHOUT the full-page
// chrome that base.html emits. Regression for the empty-modal bug where the
// template emitted nothing when HXFragment was true.
func TestTriggersNewForm_HXModalReturnsFormNotEmpty(t *testing.T) {
	store := triggersStore()
	h := newTestHandlerWith(store, func(d *Deps) {
		d.Triggers = &fakeTriggers{}
		d.Connectors = ConnectorNames{AWS: []string{"prod"}}
	})

	t.Run("modal open returns form content without chrome", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/triggers/new", nil)
		req.Header.Set("HX-Request", "true")
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.TrimSpace(body) == "" {
			t.Fatal("htmx modal form rendered an EMPTY body")
		}
		// Form content must be present.
		if !strings.Contains(body, `name="name"`) {
			t.Fatalf("htmx modal missing form field name=\"name\"; body=%s", body)
		}
		if !strings.Contains(body, "add trigger") {
			t.Fatalf("htmx modal missing 'add trigger'; body=%s", body)
		}
		// Full-page chrome from base.html must NOT be present — this is a
		// fragment for swap into #trigger-modal.
		for _, chrome := range []string{"<!doctype", "<html", "┌─ bucketvcs ─┐"} {
			if strings.Contains(body, chrome) {
				t.Fatalf("htmx modal leaked page chrome %q; body=%s", chrome, body)
			}
		}
	})

	t.Run("kind swap still returns connector option", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/triggers/new?kind=codebuild", nil)
		req.Header.Set("HX-Request", "true")
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "prod") {
			t.Fatalf("htmx kind swap missing connector option 'prod'; body=%s", rec.Body.String())
		}
	})
}

// TestTriggersAdd_GenericShowsSecretOnce covers the happy path: a generic
// trigger whose Create auto-generates a secret renders the secret-once page.
func TestTriggersAdd_GenericShowsSecretOnce(t *testing.T) {
	store := triggersStore()
	var gotInput buildtrigger.TriggerInput
	tr := &fakeTriggers{
		createFn: func(ctx context.Context, in buildtrigger.TriggerInput) (buildtrigger.Trigger, error) {
			gotInput = in
			return buildtrigger.Trigger{
				ID:     "bvbt_9",
				Tenant: "acme",
				Repo:   "demo",
				Name:   in.Name,
				Kind:   buildtrigger.KindGeneric,
				Secret: "supersecretvalue",
			}, nil
		},
	}
	logger, sink := newTestLogger()
	h := newTestHandlerWith(store, func(d *Deps) { d.Triggers = tr; d.Logger = logger })
	req := csrfPost(t, "/acme/demo/settings/triggers/add", url.Values{
		"name": {"ci"},
		"kind": {"generic"},
		"url":  {"https://ci.example.com/hook"},
	})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (secret-once page); body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if count := strings.Count(body, "supersecretvalue"); count != 1 {
		t.Fatalf("secret appears %d times, want exactly 1; body=%s", count, body)
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("Cache-Control must contain no-store; got %q", rec.Header().Get("Cache-Control"))
	}
	if gotInput.Tenant != "acme" || gotInput.Repo != "demo" || gotInput.Name != "ci" {
		t.Fatalf("Create input tenant=%q repo=%q name=%q, want acme/demo/ci", gotInput.Tenant, gotInput.Repo, gotInput.Name)
	}
	if gotInput.Kind != buildtrigger.KindGeneric || gotInput.Config.URL != "https://ci.example.com/hook" {
		t.Fatalf("Create input kind=%q url=%q", gotInput.Kind, gotInput.Config.URL)
	}
	if !sink.Has("buildtrigger.created", map[string]string{"tenant": "acme", "repo": "demo"}) {
		t.Fatal("missing buildtrigger.created audit event")
	}
}

// TestTriggersAdd_InvalidFlash covers the ErrInvalidInput → redirectFlash path.
func TestTriggersAdd_InvalidFlash(t *testing.T) {
	store := triggersStore()
	tr := &fakeTriggers{
		createFn: func(ctx context.Context, in buildtrigger.TriggerInput) (buildtrigger.Trigger, error) {
			return buildtrigger.Trigger{}, buildtrigger.ErrInvalidInput
		},
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.Triggers = tr })
	req := csrfPost(t, "/acme/demo/settings/triggers/add", url.Values{
		"name": {"ci"},
		"kind": {"generic"},
		"url":  {"https://ci.example.com/hook"},
	})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
		t.Fatal("expected flash cookie for ErrInvalidInput")
	}
}

// TestTriggersPage_ListsTriggers covers GET /{t}/{r}/settings/triggers with one
// trigger present.
func TestTriggersPage_ListsTriggers(t *testing.T) {
	store := triggersStore()
	tr := &fakeTriggers{
		listFn: func(ctx context.Context, tenant, repo string) ([]buildtrigger.Trigger, error) {
			return []buildtrigger.Trigger{{
				ID:        "bvbt_1",
				Tenant:    "acme",
				Repo:      "demo",
				Name:      "ci",
				Kind:      buildtrigger.KindCloudBuild,
				Active:    true,
				CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			}}, nil
		},
		listDeliveriesPageFn: func(ctx context.Context, triggerID, status string, before time.Time, limit int) ([]buildtrigger.Delivery, error) {
			return nil, nil
		},
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.Triggers = tr })
	req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/triggers", nil)
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"ci", "cloudbuild", "csrf_token"} {
		if !strings.Contains(body, want) {
			t.Fatalf("triggers page missing %q; body=%s", want, body)
		}
	}
}

// TestTriggersEdit_KindImmutable covers POST .../triggers/edit: the form posts
// a new name and immutable kind, the handler calls Edit with the parsed input
// and redirects with a flash.
func TestTriggersEdit_KindImmutable(t *testing.T) {
	store := triggersStore()
	var gotInput buildtrigger.EditInput
	tr := &fakeTriggers{
		getFn: func(ctx context.Context, id string) (buildtrigger.Trigger, error) {
			return buildtrigger.Trigger{ID: id, Tenant: "acme", Repo: "demo", Name: "ci", Kind: buildtrigger.KindGeneric}, nil
		},
		editFn: func(ctx context.Context, id string, in buildtrigger.EditInput) (buildtrigger.Trigger, error) {
			gotInput = in
			return buildtrigger.Trigger{ID: id, Tenant: "acme", Repo: "demo", Name: in.Name, Kind: buildtrigger.KindGeneric}, nil
		},
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.Triggers = tr })
	req := csrfPost(t, "/acme/demo/settings/triggers/edit", url.Values{
		"id":         {"bvbt_1"},
		"name":       {"ci2"},
		"token_mode": {"none"},
		"active":     {"on"},
	})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if gotInput.Name != "ci2" {
		t.Fatalf("Edit input name=%q, want ci2", gotInput.Name)
	}
	if !gotInput.Active {
		t.Fatalf("Edit input Active=false, want true (active=on)")
	}
}

// TestTriggersEnable_Toggles covers POST .../triggers/enable: ownership check
// passes, Enable is called, and the handler redirects.
func TestTriggersEnable_Toggles(t *testing.T) {
	store := triggersStore()
	enabled := false
	tr := &fakeTriggers{
		getFn: func(ctx context.Context, id string) (buildtrigger.Trigger, error) {
			return buildtrigger.Trigger{ID: id, Tenant: "acme", Repo: "demo", Name: "ci", Kind: buildtrigger.KindGeneric}, nil
		},
		enableFn: func(ctx context.Context, id string) error {
			enabled = true
			return nil
		},
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.Triggers = tr })
	req := csrfPost(t, "/acme/demo/settings/triggers/enable", url.Values{"id": {"bvbt_1"}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if !enabled {
		t.Fatal("Enable was not called")
	}
}

// TestTriggersRemove_OwnershipEnforced verifies a repo admin cannot remove a
// trigger that belongs to a DIFFERENT repo: ownTriggerOr404 returns a uniform
// 404 and Remove is never called.
func TestTriggersRemove_OwnershipEnforced(t *testing.T) {
	store := triggersStore()
	removed := false
	tr := &fakeTriggers{
		getFn: func(ctx context.Context, id string) (buildtrigger.Trigger, error) {
			return buildtrigger.Trigger{ID: id, Tenant: "other", Repo: "repo", Name: "ci", Kind: buildtrigger.KindGeneric}, nil
		},
		removeFn: func(ctx context.Context, id string) error {
			removed = true
			return nil
		},
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.Triggers = tr })
	req := csrfPost(t, "/acme/demo/settings/triggers/remove", url.Values{"id": {"bvbt_1"}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (foreign trigger); body=%s", rec.Code, rec.Body.String())
	}
	if removed {
		t.Fatal("Remove was called on a foreign trigger (ownership boundary breached)")
	}
}

// TestTriggersDisable_Toggles covers POST .../triggers/disable: ownership check
// passes, Disable is called, and the handler redirects.
func TestTriggersDisable_Toggles(t *testing.T) {
	store := triggersStore()
	disabled := false
	tr := &fakeTriggers{
		getFn: func(ctx context.Context, id string) (buildtrigger.Trigger, error) {
			return buildtrigger.Trigger{ID: id, Tenant: "acme", Repo: "demo", Name: "ci", Kind: buildtrigger.KindGeneric}, nil
		},
		disableFn: func(ctx context.Context, id string) error {
			disabled = true
			return nil
		},
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.Triggers = tr })
	req := csrfPost(t, "/acme/demo/settings/triggers/disable", url.Values{"id": {"bvbt_1"}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if !disabled {
		t.Fatal("Disable was not called")
	}
}

// TestTriggersRotateSecret_ShownOnce covers POST .../triggers/rotate-secret:
// the rotated secret is rendered exactly once on the secret-once page.
func TestTriggersRotateSecret_ShownOnce(t *testing.T) {
	store := triggersStore()
	tr := &fakeTriggers{
		getFn: func(ctx context.Context, id string) (buildtrigger.Trigger, error) {
			return buildtrigger.Trigger{ID: id, Tenant: "acme", Repo: "demo", Name: "ci", Kind: buildtrigger.KindCloudBuild}, nil
		},
		rotateSecretFn: func(ctx context.Context, id string) (string, error) {
			return "rotatedvalue", nil
		},
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.Triggers = tr })
	req := csrfPost(t, "/acme/demo/settings/triggers/rotate-secret", url.Values{"id": {"bvbt_1"}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (secret-once page); body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if count := strings.Count(body, "rotatedvalue"); count != 1 {
		t.Fatalf("rotated secret appears %d times, want exactly 1; body=%s", count, body)
	}
}

// TestTriggersPage_NotEnabled covers the nil-Triggers case (dep not wired).
func TestTriggersPage_NotEnabled(t *testing.T) {
	store := triggersStore()
	h := newTestHandlerWith(store, nil) // Triggers stays nil
	req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/triggers", nil)
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not enabled") {
		t.Fatalf("expected 'not enabled' notice; body=%s", rec.Body.String())
	}
}
