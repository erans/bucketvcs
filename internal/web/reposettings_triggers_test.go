package web

import (
	"context"
	"net/http"
	"net/http/httptest"
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
