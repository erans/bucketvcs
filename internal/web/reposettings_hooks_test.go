package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/hooks"
)

// hooksStore returns a repo-admin fakeStore primed for the hooks tab.
func hooksStore() *fakeStore {
	s := newFakeStore()
	s.perm = auth.PermAdmin
	s.getRepoFlags = func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
		return auth.RepoFlags{}, nil
	}
	return s
}

// fakeHooks implements HookAdmin for testing. All methods are no-ops unless a
// corresponding Fn field is set. Mirrors fakePolicy style.
type fakeHooks struct {
	addFn        func(ctx context.Context, r hooks.Row) error
	listFn       func(ctx context.Context, tenant, repo, triggerFilter string) ([]hooks.Row, error)
	removeFn     func(ctx context.Context, tenant, repo, trigger, scriptName string) error
	setEnabledFn func(ctx context.Context, tenant, repo, trigger, scriptName string, enabled bool, now time.Time) error
}

func (f *fakeHooks) Add(ctx context.Context, r hooks.Row) error {
	if f.addFn != nil {
		return f.addFn(ctx, r)
	}
	return nil
}
func (f *fakeHooks) List(ctx context.Context, tenant, repo, triggerFilter string) ([]hooks.Row, error) {
	if f.listFn != nil {
		return f.listFn(ctx, tenant, repo, triggerFilter)
	}
	return nil, nil
}
func (f *fakeHooks) Remove(ctx context.Context, tenant, repo, trigger, scriptName string) error {
	if f.removeFn != nil {
		return f.removeFn(ctx, tenant, repo, trigger, scriptName)
	}
	return nil
}
func (f *fakeHooks) SetEnabled(ctx context.Context, tenant, repo, trigger, scriptName string, enabled bool, now time.Time) error {
	if f.setEnabledFn != nil {
		return f.setEnabledFn(ctx, tenant, repo, trigger, scriptName, enabled, now)
	}
	return nil
}

// stdHookRow is a fixture hooks.Row for use across tests.
var stdHookRow = hooks.Row{
	Tenant:     "acme",
	Repo:       "demo",
	Trigger:    hooks.TriggerPreReceive,
	ScriptName: "check-policy.sh",
	SortOrder:  0,
	Enabled:    true,
	CreatedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	UpdatedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
}

// TestRepoSettingsHooksGet covers GET /{t}/{r}/settings/hooks.
func TestRepoSettingsHooksGet(t *testing.T) {
	t.Run("repo-admin (not global) → 404", func(t *testing.T) {
		store := hooksStore()
		hk := &fakeHooks{}
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk })
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/hooks", nil)
		addSessionCookie(t, req, store, userSession()) // userSession has IsAdmin=false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("repo-admin: status %d, want 404", rec.Code)
		}
	})

	t.Run("global admin GET → 200 with table+forms+notice", func(t *testing.T) {
		store := hooksStore()
		hk := &fakeHooks{
			listFn: func(ctx context.Context, tenant, repo, triggerFilter string) ([]hooks.Row, error) {
				return []hooks.Row{stdHookRow}, nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk })
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/hooks", nil)
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{
			"check-policy.sh", // hook row
			"pre-receive",     // trigger
			"add hook",        // add form
			"csrf_token",      // CSRF present
			"--hooks-dir",     // operator notice
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("hooks page missing %q; body=%s", want, body)
			}
		}
	})

	t.Run("nil s.hooks → notice rendered, no forms", func(t *testing.T) {
		store := hooksStore()
		h := newTestHandlerWith(store, nil) // Hooks stays nil
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/hooks", nil)
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("nil hooks: status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "not enabled") {
			t.Fatalf("expected 'not enabled' notice; body=%s", body)
		}
		if strings.Contains(body, "add hook") {
			t.Fatalf("add hook form must not appear when hooks disabled; body=%s", body)
		}
	})
}

// TestRepoSettingsHooksAdd covers POST .../hooks/add.
func TestRepoSettingsHooksAdd(t *testing.T) {
	t.Run("form security: repo-admin (not global) + valid CSRF → 404; Add NOT called", func(t *testing.T) {
		store := hooksStore()
		var called bool
		hk := &fakeHooks{
			addFn: func(ctx context.Context, r hooks.Row) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk })
		assertFormSecurity(t, h, secOpts{
			store: store,
			path:  "/acme/demo/settings/hooks/add",
			form: url.Values{
				"trigger":     {hooks.TriggerPreReceive},
				"script_name": {"check-policy.sh"},
				"sort_order":  {"0"},
			},
			asSession: userSession(), // repo-admin but not global admin → 404
		})
		if called {
			t.Fatal("Add must not be called for non-global-admin")
		}
	})

	t.Run("nil s.hooks → 404 on POST", func(t *testing.T) {
		store := hooksStore()
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/hooks/add",
			url.Values{"trigger": {hooks.TriggerPreReceive}, "script_name": {"check-policy.sh"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("nil hooks add: status %d, want 404", rec.Code)
		}
	})

	t.Run("invalid trigger → flash, Add not called", func(t *testing.T) {
		store := hooksStore()
		var called bool
		hk := &fakeHooks{
			addFn: func(ctx context.Context, r hooks.Row) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk })
		req := csrfPost(t, "/acme/demo/settings/hooks/add",
			url.Values{"trigger": {"invalid-trigger"}, "script_name": {"check-policy.sh"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("Add must not be called for invalid trigger")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for invalid trigger")
		}
	})

	t.Run("invalid script name → flash, Add not called", func(t *testing.T) {
		store := hooksStore()
		var called bool
		hk := &fakeHooks{
			addFn: func(ctx context.Context, r hooks.Row) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk })
		req := csrfPost(t, "/acme/demo/settings/hooks/add",
			url.Values{"trigger": {hooks.TriggerPreReceive}, "script_name": {"bad script/name"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("Add must not be called for invalid script name")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for invalid script name")
		}
	})

	t.Run("bad sort_order (non-integer) → flash, Add not called", func(t *testing.T) {
		store := hooksStore()
		var called bool
		hk := &fakeHooks{
			addFn: func(ctx context.Context, r hooks.Row) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk })
		req := csrfPost(t, "/acme/demo/settings/hooks/add",
			url.Values{"trigger": {hooks.TriggerPreReceive}, "script_name": {"check.sh"}, "sort_order": {"notanint"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("Add must not be called for non-integer sort_order")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for bad sort_order")
		}
	})

	t.Run("unknown DB error → 500, no flash", func(t *testing.T) {
		store := hooksStore()
		hk := &fakeHooks{
			addFn: func(ctx context.Context, r hooks.Row) error {
				// Mirrors hooks.Store.Add's DB wrap: fmt.Errorf("hooks.Add: %w", err).
				return errors.New("hooks.Add: disk I/O error")
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk })
		req := csrfPost(t, "/acme/demo/settings/hooks/add", url.Values{
			"trigger":     {hooks.TriggerPreReceive},
			"script_name": {"check-policy.sh"},
		})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) != nil {
			t.Fatal("must NOT set flash cookie for masked DB error")
		}
		if strings.Contains(rec.Body.String(), "disk I/O error") {
			t.Fatal("DB error text must not leak into response body")
		}
	})

	t.Run("happy path: Add called + correct Row fields + audit + 303", func(t *testing.T) {
		store := hooksStore()
		var gotRow hooks.Row
		hk := &fakeHooks{
			addFn: func(ctx context.Context, r hooks.Row) error {
				gotRow = r
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/hooks/add", url.Values{
			"trigger":     {hooks.TriggerPreReceive},
			"script_name": {"check-policy.sh"},
			"sort_order":  {"5"},
		})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings/hooks" {
			t.Fatalf("Location %q, want /acme/demo/settings/hooks", loc)
		}
		// Row fields
		if gotRow.Tenant != "acme" || gotRow.Repo != "demo" {
			t.Fatalf("Add Tenant=%q Repo=%q, want acme/demo", gotRow.Tenant, gotRow.Repo)
		}
		if gotRow.Trigger != hooks.TriggerPreReceive {
			t.Fatalf("Add Trigger=%q, want pre-receive", gotRow.Trigger)
		}
		if gotRow.ScriptName != "check-policy.sh" {
			t.Fatalf("Add ScriptName=%q, want check-policy.sh", gotRow.ScriptName)
		}
		if gotRow.SortOrder != 5 {
			t.Fatalf("Add SortOrder=%d, want 5", gotRow.SortOrder)
		}
		if !gotRow.Enabled {
			t.Fatal("Add Enabled must be true")
		}
		if gotRow.Now.IsZero() {
			t.Fatal("Add Now must not be zero")
		}
		// Audit event
		if !sink.Has("policy.hook.added", map[string]string{
			"tenant": "acme", "repo": "demo",
			"trigger":    hooks.TriggerPreReceive,
			"script":     "check-policy.sh",
			"sort_order": "5",
		}) {
			t.Fatal("missing policy.hook.added audit event with full attrs")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on success")
		}
	})
}

// TestRepoSettingsHooksRemove covers POST .../hooks/remove.
func TestRepoSettingsHooksRemove(t *testing.T) {
	t.Run("ErrNotFound → flash 'no such hook'", func(t *testing.T) {
		store := hooksStore()
		hk := &fakeHooks{
			removeFn: func(ctx context.Context, tenant, repo, trigger, scriptName string) error {
				return hooks.ErrNotFound
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk })
		req := csrfPost(t, "/acme/demo/settings/hooks/remove",
			url.Values{"trigger": {hooks.TriggerPreReceive}, "script_name": {"check-policy.sh"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for not-found")
		}
	})

	t.Run("happy path: Remove + audit + 303", func(t *testing.T) {
		store := hooksStore()
		var removedTrigger, removedScript string
		hk := &fakeHooks{
			removeFn: func(ctx context.Context, tenant, repo, trigger, scriptName string) error {
				removedTrigger = trigger
				removedScript = scriptName
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/hooks/remove",
			url.Values{"trigger": {hooks.TriggerPreReceive}, "script_name": {"check-policy.sh"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings/hooks" {
			t.Fatalf("Location %q, want /acme/demo/settings/hooks", loc)
		}
		if removedTrigger != hooks.TriggerPreReceive {
			t.Fatalf("Remove trigger=%q, want pre-receive", removedTrigger)
		}
		if removedScript != "check-policy.sh" {
			t.Fatalf("Remove scriptName=%q, want check-policy.sh", removedScript)
		}
		if !sink.Has("policy.hook.removed", map[string]string{
			"tenant": "acme", "repo": "demo",
			"trigger": hooks.TriggerPreReceive,
			"script":  "check-policy.sh",
		}) {
			t.Fatal("missing policy.hook.removed audit event with full attrs")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on success")
		}
	})
}

// TestRepoSettingsHooksEnable covers POST .../hooks/enable.
func TestRepoSettingsHooksEnable(t *testing.T) {
	t.Run("ErrNotFound → flash 'no such hook'", func(t *testing.T) {
		store := hooksStore()
		hk := &fakeHooks{
			setEnabledFn: func(ctx context.Context, tenant, repo, trigger, scriptName string, enabled bool, now time.Time) error {
				return hooks.ErrNotFound
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk })
		req := csrfPost(t, "/acme/demo/settings/hooks/enable",
			url.Values{"trigger": {hooks.TriggerPreReceive}, "script_name": {"check-policy.sh"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for not-found")
		}
	})

	t.Run("happy path: SetEnabled(true) + audit policy.hook.enabled + 303", func(t *testing.T) {
		store := hooksStore()
		var gotEnabled bool
		hk := &fakeHooks{
			setEnabledFn: func(ctx context.Context, tenant, repo, trigger, scriptName string, enabled bool, now time.Time) error {
				gotEnabled = enabled
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/hooks/enable",
			url.Values{"trigger": {hooks.TriggerPostReceive}, "script_name": {"notify.sh"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if !gotEnabled {
			t.Fatal("SetEnabled must be called with enabled=true")
		}
		if !sink.Has("policy.hook.enabled", map[string]string{
			"tenant": "acme", "repo": "demo",
			"trigger": hooks.TriggerPostReceive,
			"script":  "notify.sh",
		}) {
			t.Fatal("missing policy.hook.enabled audit event with full attrs")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on success")
		}
	})
}

// TestRepoSettingsHooksDisable covers POST .../hooks/disable.
func TestRepoSettingsHooksDisable(t *testing.T) {
	t.Run("ErrNotFound → flash 'no such hook'", func(t *testing.T) {
		store := hooksStore()
		hk := &fakeHooks{
			setEnabledFn: func(ctx context.Context, tenant, repo, trigger, scriptName string, enabled bool, now time.Time) error {
				return hooks.ErrNotFound
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk })
		req := csrfPost(t, "/acme/demo/settings/hooks/disable",
			url.Values{"trigger": {hooks.TriggerPreReceive}, "script_name": {"check-policy.sh"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for not-found")
		}
	})

	t.Run("happy path: SetEnabled(false) + audit policy.hook.disabled + 303", func(t *testing.T) {
		store := hooksStore()
		var gotEnabled *bool
		hk := &fakeHooks{
			setEnabledFn: func(ctx context.Context, tenant, repo, trigger, scriptName string, enabled bool, now time.Time) error {
				v := enabled
				gotEnabled = &v
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/hooks/disable",
			url.Values{"trigger": {hooks.TriggerPreReceive}, "script_name": {"check-policy.sh"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if gotEnabled == nil || *gotEnabled {
			t.Fatal("SetEnabled must be called with enabled=false")
		}
		if !sink.Has("policy.hook.disabled", map[string]string{
			"tenant": "acme", "repo": "demo",
			"trigger": hooks.TriggerPreReceive,
			"script":  "check-policy.sh",
		}) {
			t.Fatal("missing policy.hook.disabled audit event with full attrs")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on success")
		}
	})
}

// TestRepoSettingsHooksRepoAdminPostBlocked verifies that a repo-admin (PermAdmin
// but not IsAdmin) with valid CSRF is blocked by the isGlobalAdmin gate on POSTs,
// and that Add is never called. This is THE key privilege-escalation test.
func TestRepoSettingsHooksRepoAdminPostBlocked(t *testing.T) {
	store := hooksStore() // perm = PermAdmin, IsAdmin = false
	var called bool
	hk := &fakeHooks{
		addFn: func(ctx context.Context, r hooks.Row) error {
			called = true
			return nil
		},
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.Hooks = hk })

	req := csrfPost(t, "/acme/demo/settings/hooks/add", url.Values{
		"trigger":     {hooks.TriggerPreReceive},
		"script_name": {"check-policy.sh"},
		"sort_order":  {"0"},
	})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("repo-admin POST hooks/add: status %d, want 404", rec.Code)
	}
	if called {
		t.Fatal("Add must NOT be called for repo-admin (privilege escalation blocked)")
	}
}

// Compile guard: fakeHooks must satisfy HookAdmin.
var _ HookAdmin = (*fakeHooks)(nil)

// errHooksNotFound is used to return hooks.ErrNotFound in tests that use errors.Is.
var _ = errors.Is(hooks.ErrNotFound, hooks.ErrNotFound)
