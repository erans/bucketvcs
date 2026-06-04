package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// fakeRepoInit is a recording RepoInitializer for the register-flow tests.
type fakeRepoInit struct {
	mu     sync.Mutex
	calls  []string // "tenant/name" per invocation
	err    error
	actors []string
}

func (f *fakeRepoInit) fn() RepoInitializer {
	return func(ctx context.Context, tenant, repoName, actor string) error {
		f.mu.Lock()
		f.calls = append(f.calls, tenant+"/"+repoName)
		f.actors = append(f.actors, actor)
		f.mu.Unlock()
		return f.err
	}
}

// --- GET /admin/repos ---

func TestAdminRepos_AuthGuard(t *testing.T) {
	t.Run("anon → 303 login", func(t *testing.T) {
		store := adminStore()
		h := newTestHandlerWith(store, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/repos", nil))
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
		req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/repos", nil), store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rec.Code)
		}
	})
	t.Run("admin → 200 with repo rows + register form", func(t *testing.T) {
		store := adminStore()
		store.repos = func(actor *auth.Actor) []Repo {
			return []Repo{
				{Tenant: "acme", Name: "demo", PublicRead: true, CreatedAt: 1700000000},
				{Tenant: "acme", Name: "private", PublicRead: false, CreatedAt: 1700000001},
			}
		}
		ri := &fakeRepoInit{}
		h := newTestHandlerWith(store, func(d *Deps) { d.RepoInit = ri.fn() })
		req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/repos", nil), store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "demo") {
			t.Errorf("missing repo demo in list; body: %s", body)
		}
		// Linkified to the repo browse page.
		if !strings.Contains(body, `href="/acme/demo"`) {
			t.Errorf("missing browse link /acme/demo; body: %s", body)
		}
		// Settings link is the single delete path.
		if !strings.Contains(body, "/acme/demo/settings") {
			t.Errorf("missing settings link /acme/demo/settings; body: %s", body)
		}
		// Register form present when repoInit is wired.
		if !strings.Contains(body, "/admin/repos/register") {
			t.Errorf("missing register form action; body: %s", body)
		}
	})
	t.Run("nil repoInit → notice, no register form", func(t *testing.T) {
		store := adminStore()
		h := newTestHandlerWith(store, nil) // d.RepoInit stays nil
		req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/repos", nil), store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "/admin/repos/register") {
			t.Errorf("register form must be absent when repoInit is nil; body: %s", body)
		}
		if !strings.Contains(body, "unavailable") {
			t.Errorf("expected 'unavailable' notice when repoInit is nil; body: %s", body)
		}
	})
}

// --- POST /admin/repos/register ---

func TestAdminRepoRegister_FormSecurity(t *testing.T) {
	store := adminStore()
	ri := &fakeRepoInit{}
	h := newTestHandlerWith(store, func(d *Deps) { d.RepoInit = ri.fn() })
	assertFormSecurity(t, h, secOpts{
		store: store,
		path:  "/admin/repos/register",
		form:  url.Values{"tenant": {"acme"}, "name": {"demo"}},
		// non-admin with valid CSRF → 404 (admin area not advertised)
		asSession: userSession(),
	})
}

func TestAdminRepoRegister_Validation(t *testing.T) {
	cases := []struct {
		name   string
		tenant string
		repo   string
	}{
		{"bad tenant", "ac me", "demo"},
		{"bad name", "acme", "de mo"},
		{"empty tenant", "", "demo"},
		{"empty name", "acme", ""},
		// Reserved web UI segments would be shadowed by the literal mux routes.
		{"reserved tenant admin", "admin", "users"},
		{"reserved tenant settings", "settings", "tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := adminStore()
			ri := &fakeRepoInit{}
			var registered bool
			store.registerRepoIfNew = func(ctx context.Context, tenant, name string) (bool, error) {
				registered = true
				return true, nil
			}
			h := newTestHandlerWith(store, func(d *Deps) { d.RepoInit = ri.fn() })
			req := csrfPost(t, "/admin/repos/register", url.Values{"tenant": {tc.tenant}, "name": {tc.repo}})
			addSessionCookie(t, req, store, adminSession())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
			}
			if len(ri.calls) != 0 {
				t.Fatalf("repoInit must NOT be called for invalid input; calls=%v", ri.calls)
			}
			if registered {
				t.Fatal("RegisterRepoIfNew must NOT be called for invalid input")
			}
			if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
				t.Fatal("expected flash cookie for invalid input")
			}
		})
	}
}

func TestAdminRepoRegister_NilRepoInit_404(t *testing.T) {
	store := adminStore()
	h := newTestHandlerWith(store, nil) // d.RepoInit nil
	req := csrfPost(t, "/admin/repos/register", url.Values{"tenant": {"acme"}, "name": {"demo"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestAdminRepoRegister_RepoInitError(t *testing.T) {
	store := adminStore()
	ri := &fakeRepoInit{err: errors.New("storage boom")}
	var registered bool
	store.registerRepoIfNew = func(ctx context.Context, tenant, name string) (bool, error) {
		registered = true
		return true, nil
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.RepoInit = ri.fn() })
	req := csrfPost(t, "/admin/repos/register", url.Values{"tenant": {"acme"}, "name": {"demo"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303 (flash); body: %s", rec.Code, rec.Body.String())
	}
	if len(ri.calls) != 1 {
		t.Fatalf("repoInit calls=%v, want 1", ri.calls)
	}
	if registered {
		t.Fatal("RegisterRepoIfNew must NOT be called when repoInit fails with a non-exists error")
	}
	if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
		t.Fatal("expected flash cookie for storage-init failure")
	}
}

// Healing path: storage already exists (ErrRepoExists) but the registry row may
// be missing — continue to RegisterRepoIfNew to heal the half-registered state.
func TestAdminRepoRegister_AlreadyExistsHeals(t *testing.T) {
	store := adminStore()
	ri := &fakeRepoInit{err: repoerrs.ErrRepoExists}
	var registered bool
	store.registerRepoIfNew = func(ctx context.Context, tenant, name string) (bool, error) {
		registered = true
		return true, nil // the row was missing; registration heals it
	}
	logger, sink := newTestLogger()
	h := newTestHandlerWith(store, func(d *Deps) {
		d.RepoInit = ri.fn()
		d.Logger = logger
	})
	req := csrfPost(t, "/admin/repos/register", url.Values{"tenant": {"acme"}, "name": {"demo"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if !registered {
		t.Fatal("RegisterRepoIfNew must be called on ErrRepoExists (healing path)")
	}
	if !sink.Has("repo.created", map[string]string{"tenant": "acme", "repo": "demo"}) {
		t.Fatal("missing repo.created audit event on healing path")
	}
}

func TestAdminRepoRegister_AlreadyRegistered(t *testing.T) {
	store := adminStore()
	ri := &fakeRepoInit{}
	store.registerRepoIfNew = func(ctx context.Context, tenant, name string) (bool, error) {
		return false, nil // already registered
	}
	var enqueued bool
	wh := &fakeWebhooks{enqueue: func(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error {
		enqueued = true
		return nil
	}}
	h := newTestHandlerWith(store, func(d *Deps) {
		d.RepoInit = ri.fn()
		d.Webhooks = wh
	})
	req := csrfPost(t, "/admin/repos/register", url.Values{"tenant": {"acme"}, "name": {"demo"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if enqueued {
		t.Fatal("webhook must NOT be enqueued when repo was already registered")
	}
	if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
		t.Fatal("expected flash cookie for already-registered")
	}
}

func TestAdminRepoRegister_RegisterError_500(t *testing.T) {
	store := adminStore()
	ri := &fakeRepoInit{}
	store.registerRepoIfNew = func(ctx context.Context, tenant, name string) (bool, error) {
		return false, errors.New("db boom")
	}
	h := newTestHandlerWith(store, func(d *Deps) { d.RepoInit = ri.fn() })
	req := csrfPost(t, "/admin/repos/register", url.Values{"tenant": {"acme"}, "name": {"demo"}})
	addSessionCookie(t, req, store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
}

func TestAdminRepoRegister_Happy(t *testing.T) {
	t.Run("order: repoInit BEFORE register; webhook + audit; 303", func(t *testing.T) {
		store := adminStore()
		var order []string
		var mu sync.Mutex
		ri := &fakeRepoInit{}
		// Wrap the recorder's fn to capture ordering.
		base := ri.fn()
		repoInit := func(ctx context.Context, tenant, repoName, actor string) error {
			mu.Lock()
			order = append(order, "init")
			mu.Unlock()
			return base(ctx, tenant, repoName, actor)
		}
		store.registerRepoIfNew = func(ctx context.Context, tenant, name string) (bool, error) {
			mu.Lock()
			order = append(order, "register")
			mu.Unlock()
			return true, nil
		}
		var whEvent webhooks.Event
		var whTenant, whRepo, whActor string
		wh := &fakeWebhooks{enqueue: func(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error {
			whEvent, whTenant, whRepo, whActor = event, tenant, repo, actor
			return nil
		}}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) {
			d.RepoInit = repoInit
			d.Webhooks = wh
			d.Logger = logger
		})
		req := csrfPost(t, "/admin/repos/register", url.Values{"tenant": {"acme"}, "name": {"demo"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/admin/repos" {
			t.Fatalf("Location %q, want /admin/repos", loc)
		}
		if len(order) != 2 || order[0] != "init" || order[1] != "register" {
			t.Fatalf("call order %v, want [init register]", order)
		}
		// actor passed to repoInit is the session name.
		if len(ri.actors) != 1 || ri.actors[0] != adminSession().Name {
			t.Fatalf("repoInit actor=%v, want %q", ri.actors, adminSession().Name)
		}
		if whEvent != webhooks.EventRepoCreated || whTenant != "acme" || whRepo != "demo" {
			t.Fatalf("Enqueue(event=%v,%q,%q), want (EventRepoCreated,acme,demo)", whEvent, whTenant, whRepo)
		}
		if whActor != adminSession().Name {
			t.Fatalf("webhook actor=%q, want %q", whActor, adminSession().Name)
		}
		if !sink.Has("repo.created", map[string]string{"tenant": "acme", "repo": "demo"}) {
			t.Fatal("missing repo.created audit event")
		}
	})

	t.Run("webhook enqueue error → register still succeeds (fail-open)", func(t *testing.T) {
		store := adminStore()
		ri := &fakeRepoInit{}
		store.registerRepoIfNew = func(ctx context.Context, tenant, name string) (bool, error) {
			return true, nil
		}
		wh := &fakeWebhooks{enqueue: func(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error {
			return errors.New("boom")
		}}
		h := newTestHandlerWith(store, func(d *Deps) {
			d.RepoInit = ri.fn()
			d.Webhooks = wh
		})
		req := csrfPost(t, "/admin/repos/register", url.Values{"tenant": {"acme"}, "name": {"demo"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303 (fail-open); body: %s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/admin/repos" {
			t.Fatalf("Location %q, want /admin/repos", loc)
		}
	})

	t.Run("nil webhooks → succeeds without enqueue", func(t *testing.T) {
		store := adminStore()
		ri := &fakeRepoInit{}
		store.registerRepoIfNew = func(ctx context.Context, tenant, name string) (bool, error) {
			return true, nil
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.RepoInit = ri.fn() }) // d.Webhooks nil
		req := csrfPost(t, "/admin/repos/register", url.Values{"tenant": {"acme"}, "name": {"demo"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
		}
	})
}
