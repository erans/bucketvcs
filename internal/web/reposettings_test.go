package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// fakeQuotas implements QuotaAdmin. Only Get is used by Task 7.
type fakeQuotas struct {
	get func(ctx context.Context, tenant string) (quota.State, error)
}

func (q *fakeQuotas) Set(ctx context.Context, tenant string, limitBytes int64) error { return nil }
func (q *fakeQuotas) Get(ctx context.Context, tenant string) (quota.State, error) {
	if q.get != nil {
		return q.get(ctx, tenant)
	}
	return quota.State{}, nil
}
func (q *fakeQuotas) Clear(ctx context.Context, tenant string) error  { return nil }
func (q *fakeQuotas) List(ctx context.Context) ([]quota.State, error) { return nil, nil }

// newTestHandlerWith builds the full handler with custom Deps fields layered on
// top of the standard test wiring (so existing call sites stay untouched).
func newTestHandlerWith(store DataStore, mut func(d *Deps)) http.Handler {
	d := Deps{Store: store, Logger: slog.Default()}
	if mut != nil {
		mut(&d)
	}
	return NewHandler(d)
}

func TestParseSettingsPath(t *testing.T) {
	tests := []struct {
		in     string
		ok     bool
		tenant string
		repo   string
		tab    string
		action string
	}{
		{"/acme/demo/settings", true, "acme", "demo", "", ""},
		{"/acme/demo/settings/", true, "acme", "demo", "", ""},
		{"/acme/demo/settings/access", true, "acme", "demo", "access", ""},
		{"/acme/demo/settings/access/grant", true, "acme", "demo", "access", "grant"},
		{"/acme/demo/settings/webhooks/deliveries", true, "acme", "demo", "webhooks", "deliveries"},
		{"/acme/demo/settings/a/b/c", true, "acme", "demo", "a", "b/c"},
		{"/acme/demo/tree/main", false, "", "", "", ""},
		{"/acme/demo", false, "", "", "", ""},
		{"/ac me/demo/settings", false, "", "", "", ""},
		{"/acme/de mo/settings", false, "", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			sr, ok := parseSettingsPath(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if sr.tenant != tc.tenant || sr.repo != tc.repo || sr.tab != tc.tab || sr.action != tc.action {
				t.Fatalf("got %+v, want {%s %s %s %s}", sr, tc.tenant, tc.repo, tc.tab, tc.action)
			}
		})
	}
}

func TestRepoSettingsAuthzMatrix(t *testing.T) {
	tests := []struct {
		name string
		sess *auth.Session
		perm auth.Perm
		// post sends a CSRF-protected POST with `form`; otherwise a plain GET.
		post bool
		form url.Values
		path string
		// mut customizes the fake store (e.g. to inject ErrNoSuchRepo).
		mut  func(*fakeStore)
		want int
	}{
		{name: "anon redirects to login", sess: nil, perm: auth.PermNone, path: "/acme/demo/settings", want: http.StatusSeeOther},
		{name: "non-admin reader 404", sess: userSession(), perm: auth.PermRead, path: "/acme/demo/settings", want: http.StatusNotFound},
		{name: "repo admin 200", sess: userSession(), perm: auth.PermAdmin, path: "/acme/demo/settings", want: http.StatusOK},
		{name: "global admin 200", sess: adminSession(), perm: auth.PermNone, path: "/acme/demo/settings", want: http.StatusOK},
		{name: "repo admin unknown tab 404", sess: userSession(), perm: auth.PermAdmin, path: "/acme/demo/settings/bogus", want: http.StatusNotFound},
		{
			// Locks ErrNoSuchRepo→404 in repoSettingsGeneral: a future refactor
			// must not silently turn a missing repo into a 500 or a 200.
			name: "global admin missing repo 404", sess: adminSession(), perm: auth.PermNone,
			path: "/acme/demo/settings",
			mut: func(s *fakeStore) {
				s.getRepoFlags = func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
					return auth.RepoFlags{}, auth.ErrNoSuchRepo
				}
			},
			want: http.StatusNotFound,
		},
		{
			// Locks the CHASSIS-level existence probe (roborev round 16): tabs
			// without their own repo probe (webhooks/policy/hooks) must also
			// 404 for global admins on a missing repo, not render empty pages.
			name: "global admin missing repo 404 on webhooks tab", sess: adminSession(), perm: auth.PermNone,
			path: "/acme/demo/settings/webhooks",
			mut: func(s *fakeStore) {
				s.getRepoFlags = func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
					return auth.RepoFlags{}, auth.ErrNoSuchRepo
				}
			},
			want: http.StatusNotFound,
		},
		{
			// Same lock on the POST /settings/public path: SetRepoPublic
			// reporting a missing repo must surface as 404, not 500/303.
			name: "global admin public toggle missing repo 404", sess: adminSession(), perm: auth.PermNone,
			post: true, form: url.Values{"public": {"on"}},
			path: "/acme/demo/settings/public",
			mut: func(s *fakeStore) {
				s.setRepoPublic = func(ctx context.Context, tenant, repo string, public bool) error {
					return auth.ErrNoSuchRepo
				}
			},
			want: http.StatusNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.perm = tc.perm
			if tc.mut != nil {
				tc.mut(store)
			}
			h := newTestHandlerWith(store, nil)
			var req *http.Request
			if tc.post {
				req = csrfPost(t, tc.path, cloneValues(tc.form))
			} else {
				req = httptest.NewRequest(http.MethodGet, tc.path, nil)
			}
			if tc.sess != nil {
				addSessionCookie(t, req, store, tc.sess)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusSeeOther {
				if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
					t.Fatalf("Location %q, want /login...", loc)
				}
			}
		})
	}
}

func TestRepoSettingsGeneralRenders(t *testing.T) {
	mkStore := func(public bool) *fakeStore {
		s := newFakeStore()
		s.getRepoFlags = func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
			return auth.RepoFlags{PublicRead: public}, nil
		}
		return s
	}

	t.Run("repo admin: checked + csrf + nav, no hooks link, no quota when nil", func(t *testing.T) {
		store := mkStore(true)
		store.perm = auth.PermAdmin
		h := newTestHandlerWith(store, nil)
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "checkbox") || !strings.Contains(body, "checked") {
			t.Fatalf("expected checked checkbox; body=%s", body)
		}
		if !strings.Contains(body, "csrf_token") {
			t.Fatalf("expected csrf_token field; body=%s", body)
		}
		if !strings.Contains(body, "/acme/demo/settings/access") {
			t.Fatalf("expected access nav link; body=%s", body)
		}
		if strings.Contains(body, "/acme/demo/settings/hooks") {
			t.Fatalf("repo-admin must NOT see hooks link; body=%s", body)
		}
		if strings.Contains(body, "lfs storage") {
			t.Fatalf("quota section must be absent when s.quotas is nil; body=%s", body)
		}
	})

	t.Run("global admin sees hooks link", func(t *testing.T) {
		store := mkStore(false)
		h := newTestHandlerWith(store, nil)
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings", nil)
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if !strings.Contains(body, "/acme/demo/settings/hooks") {
			t.Fatalf("global admin must see hooks link; body=%s", body)
		}
		if strings.Contains(body, "checked") {
			t.Fatalf("expected unchecked checkbox for non-public repo; body=%s", body)
		}
	})

	t.Run("quota section present when quotas configured", func(t *testing.T) {
		store := mkStore(false)
		store.perm = auth.PermAdmin
		q := &fakeQuotas{get: func(ctx context.Context, tenant string) (quota.State, error) {
			return quota.State{Tenant: tenant, LimitBytes: 1 << 30, UsedBytes: 5 << 20, Exists: true, UpdatedAt: time.Now()}, nil
		}}
		h := newTestHandlerWith(store, func(d *Deps) { d.Quotas = q })
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if !strings.Contains(body, "lfs storage") {
			t.Fatalf("expected quota section; body=%s", body)
		}
		if !strings.Contains(body, humanSize(5<<20)) {
			t.Fatalf("expected used bytes %q; body=%s", humanSize(5<<20), body)
		}
		if !strings.Contains(body, humanSize(1<<30)) {
			t.Fatalf("expected limit bytes %q; body=%s", humanSize(1<<30), body)
		}
	})
}

func TestRepoSettingsSetPublic(t *testing.T) {
	// Security matrix: reader (PermRead) WITH valid CSRF → 404.
	store := newFakeStore()
	store.perm = auth.PermRead
	h := newTestHandlerWith(store, nil)
	assertFormSecurity(t, h, secOpts{
		store:     store,
		path:      "/acme/demo/settings/public",
		form:      url.Values{"public": {"on"}},
		asSession: userSession(),
	})

	cases := []struct {
		name     string
		form     url.Values
		wantBool bool
		wantMsg  string
	}{
		{"on", url.Values{"public": {"on"}}, true, "public"},
		{"off", url.Values{}, false, "private"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotTenant, gotRepo string
			var gotBool bool
			var called bool
			store := newFakeStore()
			store.perm = auth.PermAdmin
			store.setRepoPublic = func(ctx context.Context, tenant, repo string, public bool) error {
				called = true
				gotTenant, gotRepo, gotBool = tenant, repo, public
				return nil
			}
			logger, sink := newTestLogger()
			h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
			req := csrfPost(t, "/acme/demo/settings/public", cloneValues(tc.form))
			addSessionCookie(t, req, store, userSession())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings" {
				t.Fatalf("Location %q, want /acme/demo/settings", loc)
			}
			if !called {
				t.Fatal("SetRepoPublic not called")
			}
			if gotTenant != "acme" || gotRepo != "demo" || gotBool != tc.wantBool {
				t.Fatalf("SetRepoPublic(%q,%q,%v), want (acme,demo,%v)", gotTenant, gotRepo, gotBool, tc.wantBool)
			}
			if !sink.Has("repo.public_set", map[string]string{"tenant": "acme", "repo": "demo"}) {
				t.Fatal("missing repo.public_set audit event")
			}
			// Flash cookie should be set (carries the success message).
			if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
				t.Fatalf("expected flash cookie for %q message", tc.wantMsg)
			}
		})
	}
}

func TestBrowseSettingsLink(t *testing.T) {
	content := &fakeContent{refs: mainRefs()}

	render := func(perm auth.Perm, sess *auth.Session) string {
		store := &browseDataStore{visible: map[string]bool{"acme/demo": true}, perm: perm}
		h := NewHandler(Deps{Store: store, Content: content})
		req := httptest.NewRequest(http.MethodGet, "/acme/demo", nil)
		if sess != nil {
			// browseDataStore has no session table; inject via context. The
			// session middleware only overrides context when a cookie session
			// is found, so this survives.
			req = withTestSession(req, sess)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}

	// repo admin sees the link
	body := render(auth.PermAdmin, userSession())
	if !strings.Contains(body, `/acme/demo/settings">[settings]`) {
		t.Fatalf("repo admin should see [settings] link; body=%s", body)
	}

	// plain reader does not
	body = render(auth.PermRead, userSession())
	if strings.Contains(body, "[settings]") {
		t.Fatalf("plain reader must NOT see [settings] link; body=%s", body)
	}
}

// fakeWebhooks implements WebhookAdmin. The danger-zone handlers only call
// Enqueue; the rest are stubs. enqueue records each call so tests can assert
// the event/tenant/repo and the relative ordering against DeleteRepoCascade.
type fakeWebhooks struct {
	enqueue func(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error
}

func (w *fakeWebhooks) Create(ctx context.Context, in webhooks.EndpointInput) (webhooks.Endpoint, error) {
	return webhooks.Endpoint{}, nil
}
func (w *fakeWebhooks) List(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error) {
	return nil, nil
}
func (w *fakeWebhooks) Remove(ctx context.Context, id int64) error  { return nil }
func (w *fakeWebhooks) Enable(ctx context.Context, id int64) error  { return nil }
func (w *fakeWebhooks) Disable(ctx context.Context, id int64) error { return nil }
func (w *fakeWebhooks) RotateSecret(ctx context.Context, id int64) (string, error) {
	return "", nil
}
func (w *fakeWebhooks) ListDeliveries(ctx context.Context, f webhooks.ListDeliveriesFilter) ([]webhooks.Delivery, error) {
	return nil, nil
}
func (w *fakeWebhooks) ShowDelivery(ctx context.Context, id string) (webhooks.Delivery, error) {
	return webhooks.Delivery{}, nil
}
func (w *fakeWebhooks) ReplayDelivery(ctx context.Context, id string) error { return nil }
func (w *fakeWebhooks) Enqueue(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error {
	if w.enqueue != nil {
		return w.enqueue(ctx, event, tenant, repo, actor, payload)
	}
	return nil
}

// destAbsent is a getRepoFlags fake for rename tests: the CURRENT repo
// ("demo") resolves, any other name (the rename destination) is absent so
// the auth-row pre-check passes.
func destAbsent(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
	if repo == "demo" {
		return auth.RepoFlags{}, nil
	}
	return auth.RepoFlags{}, auth.ErrNoSuchRepo
}

func TestRepoSettingsRename(t *testing.T) {
	// Security matrix: reader (PermRead) WITH valid CSRF → 404. Rename is
	// global-admin-only, so a non-admin session is uniformly rejected.
	t.Run("form security", func(t *testing.T) {
		store := newFakeStore()
		store.perm = auth.PermRead
		h := newTestHandlerWith(store, nil)
		assertFormSecurity(t, h, secOpts{
			store:     store,
			path:      "/acme/demo/settings/rename",
			form:      url.Values{"newname": {"demo2"}},
			asSession: userSession(),
		})
	})

	// THE key regression: a repo-admin (not global admin) must get a uniform
	// 404 BEFORE any store mutation. M21 rename is an operator procedure
	// (auth rename + out-of-band storage migration); repo-admins can't migrate
	// storage, so the web surface is gated to global admin like delete.
	t.Run("repo-admin (non-global) → 404, RenameRepo not called", func(t *testing.T) {
		store := newFakeStore()
		store.perm = auth.PermAdmin // repo-admin but session is not global admin
		var called bool
		store.renameRepo = func(ctx context.Context, tenant, oldName, newName string) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, func(d *Deps) {
			d.RenameCheck = func(ctx context.Context, tenant, newName string) error { return nil }
		})
		req := csrfPost(t, "/acme/demo/settings/rename", url.Values{"newname": {"demo2"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("RenameRepo must not be called for a non-global admin")
		}
	})

	t.Run("nil renameCheck → flash unavailable, RenameRepo not called", func(t *testing.T) {
		store := newFakeStore()
		var called bool
		store.renameRepo = func(ctx context.Context, tenant, oldName, newName string) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, nil) // d.RenameCheck stays nil
		req := csrfPost(t, "/acme/demo/settings/rename", url.Values{"newname": {"demo2"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("RenameRepo must not be called when renameCheck is nil")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for unavailable-rename")
		}
	})

	t.Run("storage collision → flash, RenameRepo + enqueue not called", func(t *testing.T) {
		store := newFakeStore()
		var called bool
		store.renameRepo = func(ctx context.Context, tenant, oldName, newName string) error {
			called = true
			return nil
		}
		var enqueued bool
		wh := &fakeWebhooks{enqueue: func(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error {
			enqueued = true
			return nil
		}}
		h := newTestHandlerWith(store, func(d *Deps) {
			d.Webhooks = wh
			d.RenameCheck = func(ctx context.Context, tenant, newName string) error {
				return errors.New("destination storage prefix not empty (first key: tenants/acme/repos/demo2/manifest/root.json)")
			}
		})
		req := csrfPost(t, "/acme/demo/settings/rename", url.Values{"newname": {"demo2"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("RenameRepo must not be called on a storage collision")
		}
		if enqueued {
			t.Fatal("webhook must not be enqueued on a storage collision (refused before enqueue)")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for storage collision")
		}
	})

	t.Run("invalid name → flash, RenameRepo not called", func(t *testing.T) {
		store := newFakeStore()
		var called bool
		store.renameRepo = func(ctx context.Context, tenant, oldName, newName string) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, func(d *Deps) {
			d.RenameCheck = func(ctx context.Context, tenant, newName string) error { return nil }
		})
		req := csrfPost(t, "/acme/demo/settings/rename", url.Values{"newname": {"bad name!"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("RenameRepo must not be called for an invalid name")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for invalid-name error")
		}
	})

	t.Run("conflict → flash name already taken", func(t *testing.T) {
		store := newFakeStore()
		// Default fake getRepoFlags resolves ANY name, so the auth-row
		// pre-check catches the conflict BEFORE the webhook enqueue — the
		// round-12 fix: no spurious repo.renamed event on a name conflict.
		renameCalled := false
		store.renameRepo = func(ctx context.Context, tenant, oldName, newName string) error {
			renameCalled = true
			return sqlitestore.ErrRepoExists
		}
		enqueueCalled := false
		wh := &fakeWebhooks{enqueue: func(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error {
			enqueueCalled = true
			return nil
		}}
		h := newTestHandlerWith(store, func(d *Deps) {
			d.Webhooks = wh
			d.RenameCheck = func(ctx context.Context, tenant, newName string) error { return nil }
		})
		req := csrfPost(t, "/acme/demo/settings/rename", url.Values{"newname": {"taken"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for conflict error")
		}
		if enqueueCalled {
			t.Fatal("repo.renamed must NOT be enqueued when the destination name is taken")
		}
		if renameCalled {
			t.Fatal("RenameRepo must NOT be called when the pre-check catches the conflict")
		}
	})

	t.Run("no such repo → 404", func(t *testing.T) {
		store := newFakeStore()
		store.getRepoFlags = destAbsent // dest row absent: pre-check passes
		store.renameRepo = func(ctx context.Context, tenant, oldName, newName string) error {
			return auth.ErrNoSuchRepo
		}
		h := newTestHandlerWith(store, func(d *Deps) {
			d.RenameCheck = func(ctx context.Context, tenant, newName string) error { return nil }
		})
		req := csrfPost(t, "/acme/demo/settings/rename", url.Values{"newname": {"demo2"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("happy path: rename + webhook + audit + metric + redirect", func(t *testing.T) {
		store := newFakeStore()
		store.getRepoFlags = destAbsent // dest row absent: pre-check passes
		var order []string
		var mu sync.Mutex
		var gotTenant, gotOld, gotNew string
		store.renameRepo = func(ctx context.Context, tenant, oldName, newName string) error {
			mu.Lock()
			order = append(order, "rename")
			mu.Unlock()
			gotTenant, gotOld, gotNew = tenant, oldName, newName
			return nil
		}
		var whEvent webhooks.Event
		var whTenant, whRepo string
		var whPayload any
		wh := &fakeWebhooks{enqueue: func(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error {
			mu.Lock()
			order = append(order, "enqueue")
			mu.Unlock()
			whEvent, whTenant, whRepo, whPayload = event, tenant, repo, payload
			return nil
		}}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) {
			d.Logger = logger
			d.Webhooks = wh
			d.RenameCheck = func(ctx context.Context, tenant, newName string) error { return nil }
		})
		req := csrfPost(t, "/acme/demo/settings/rename", url.Values{"newname": {"demo2"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo2/settings" {
			t.Fatalf("Location %q, want /acme/demo2/settings", loc)
		}
		// Enqueue MUST run before RenameRepo: the webhook_endpoints rows are
		// keyed on the OLD name when Enqueue's SELECT runs; RenameRepo moves
		// them to the new name, so enqueuing after would match zero endpoints.
		if len(order) != 2 || order[0] != "enqueue" || order[1] != "rename" {
			t.Fatalf("call order %v, want [enqueue rename]", order)
		}
		if gotTenant != "acme" || gotOld != "demo" || gotNew != "demo2" {
			t.Fatalf("RenameRepo(%q,%q,%q), want (acme,demo,demo2)", gotTenant, gotOld, gotNew)
		}
		if whEvent != webhooks.EventRepoRenamed || whTenant != "acme" || whRepo != "demo" {
			t.Fatalf("Enqueue(event=%v,%q,%q), want (EventRepoRenamed,acme,demo)", whEvent, whTenant, whRepo)
		}
		pl, ok := whPayload.(webhooks.RepoRenamedPayload)
		if !ok || pl.OldName != "demo" || pl.NewName != "demo2" {
			t.Fatalf("payload %#v, want RepoRenamedPayload{OldName:demo,NewName:demo2}", whPayload)
		}
		if !sink.Has("repo.renamed", map[string]string{"tenant": "acme", "from": "demo", "to": "demo2"}) {
			t.Fatal("missing repo.renamed audit event with from/to attrs")
		}
	})

	t.Run("rename to same name → flash name unchanged, RenameRepo not called", func(t *testing.T) {
		store := newFakeStore()
		var called bool
		store.renameRepo = func(ctx context.Context, tenant, oldName, newName string) error {
			called = true
			return nil
		}
		var enqueued bool
		wh := &fakeWebhooks{enqueue: func(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error {
			enqueued = true
			return nil
		}}
		h := newTestHandlerWith(store, func(d *Deps) {
			d.Webhooks = wh
			d.RenameCheck = func(ctx context.Context, tenant, newName string) error { return nil }
		})
		req := csrfPost(t, "/acme/demo/settings/rename", url.Values{"newname": {"demo"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings" {
			t.Fatalf("Location %q, want /acme/demo/settings", loc)
		}
		if called {
			t.Fatal("RenameRepo must not be called when newName == current name")
		}
		if enqueued {
			t.Fatal("webhook must not be enqueued for a same-name no-op")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for same-name no-op")
		}
	})

	t.Run("nil webhooks → still succeeds", func(t *testing.T) {
		store := newFakeStore()
		store.getRepoFlags = destAbsent // dest row absent: pre-check passes
		var called bool
		store.renameRepo = func(ctx context.Context, tenant, oldName, newName string) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, func(d *Deps) { // d.Webhooks stays nil
			d.RenameCheck = func(ctx context.Context, tenant, newName string) error { return nil }
		})
		req := csrfPost(t, "/acme/demo/settings/rename", url.Values{"newname": {"demo2"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if !called {
			t.Fatal("RenameRepo must be called even when webhooks is nil")
		}
	})
}

func TestRepoSettingsDelete(t *testing.T) {
	t.Run("repo-admin (non-global) → 404, DeleteRepoCascade not called", func(t *testing.T) {
		store := newFakeStore()
		store.perm = auth.PermAdmin // repo-admin but session is not global admin
		var called bool
		store.deleteRepo = func(ctx context.Context, tenant, repo string) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/delete", url.Values{"confirm": {"acme/demo"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("DeleteRepoCascade must not be called for a non-global admin")
		}
	})

	t.Run("global admin wrong confirm → flash, not called", func(t *testing.T) {
		store := newFakeStore()
		var called bool
		store.deleteRepo = func(ctx context.Context, tenant, repo string) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/delete", url.Values{"confirm": {"wrong/name"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings" {
			t.Fatalf("Location %q, want /acme/demo/settings", loc)
		}
		if called {
			t.Fatal("DeleteRepoCascade must not be called on a wrong confirm")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for wrong-confirm error")
		}
	})

	t.Run("global admin correct confirm: enqueue-before-delete, audit, metric, redirect", func(t *testing.T) {
		store := newFakeStore()
		var order []string
		var mu sync.Mutex
		store.deleteRepo = func(ctx context.Context, tenant, repo string) error {
			mu.Lock()
			order = append(order, "delete")
			mu.Unlock()
			return nil
		}
		var whEvent webhooks.Event
		var whTenant, whRepo string
		wh := &fakeWebhooks{enqueue: func(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error {
			mu.Lock()
			order = append(order, "enqueue")
			mu.Unlock()
			whEvent, whTenant, whRepo = event, tenant, repo
			return nil
		}}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) {
			d.Logger = logger
			d.Webhooks = wh
		})
		req := csrfPost(t, "/acme/demo/settings/delete", url.Values{"confirm": {"acme/demo"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Fatalf("Location %q, want /", loc)
		}
		if len(order) != 2 || order[0] != "enqueue" || order[1] != "delete" {
			t.Fatalf("call order %v, want [enqueue delete]", order)
		}
		if whEvent != webhooks.EventRepoDeleted || whTenant != "acme" || whRepo != "demo" {
			t.Fatalf("Enqueue(event=%v,%q,%q), want (EventRepoDeleted,acme,demo)", whEvent, whTenant, whRepo)
		}
		if !sink.Has("repo.deleted", map[string]string{"tenant": "acme", "repo": "demo"}) {
			t.Fatal("missing repo.deleted audit event")
		}
		flash := findCookie(rec.Result().Cookies(), flashCookieName)
		if flash == nil {
			t.Fatal("expected flash cookie on delete")
		}
	})

	t.Run("enqueue error → delete proceeds (fail-open)", func(t *testing.T) {
		store := newFakeStore()
		var deleted bool
		store.deleteRepo = func(ctx context.Context, tenant, repo string) error {
			deleted = true
			return nil
		}
		wh := &fakeWebhooks{enqueue: func(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error {
			return errors.New("boom")
		}}
		h := newTestHandlerWith(store, func(d *Deps) { d.Webhooks = wh })
		req := csrfPost(t, "/acme/demo/settings/delete", url.Values{"confirm": {"acme/demo"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if !deleted {
			t.Fatal("DeleteRepoCascade must run even when Enqueue fails (fail-open)")
		}
	})

	t.Run("unsupported backend (postgres) → flash, not 500", func(t *testing.T) {
		store := newFakeStore()
		store.deleteRepo = func(ctx context.Context, tenant, repo string) error {
			return sqlitestore.ErrCascadeUnsupportedBackend
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/delete", url.Values{"confirm": {"acme/demo"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// Operator-environment limitation, not a server fault: redirect+flash,
		// never a 500.
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303 (flash, not 500); body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings" {
			t.Fatalf("Location %q, want /acme/demo/settings", loc)
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for unsupported-backend error")
		}
	})

	t.Run("double-submit delete is idempotent (303+flash both times)", func(t *testing.T) {
		// DeleteRepoCascade returns nil even when the repo row is already gone
		// (its DELETEs are no-ops on a missing repo). The handler therefore
		// treats a second POST exactly like the first: 303 + success flash. This
		// locks the current idempotent semantics — a double-click or browser
		// re-submit must NOT surface an error.
		store := newFakeStore()
		store.deleteRepo = func(ctx context.Context, tenant, repo string) error {
			return nil // silent success regardless of whether the repo exists
		}
		h := newTestHandlerWith(store, nil)
		for i := 0; i < 2; i++ {
			req := csrfPost(t, "/acme/demo/settings/delete", url.Values{"confirm": {"acme/demo"}})
			addSessionCookie(t, req, store, adminSession())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("submit %d: status %d, want 303; body=%s", i+1, rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != "/" {
				t.Fatalf("submit %d: Location %q, want /", i+1, loc)
			}
			if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
				t.Fatalf("submit %d: expected success flash cookie", i+1)
			}
		}
	})
}

func TestRepoSettingsDangerZoneRender(t *testing.T) {
	mkStore := func() *fakeStore {
		s := newFakeStore()
		s.getRepoFlags = func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
			return auth.RepoFlags{}, nil
		}
		return s
	}

	t.Run("global admin sees rename + delete forms", func(t *testing.T) {
		store := mkStore()
		h := newTestHandlerWith(store, nil)
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings", nil)
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if !strings.Contains(body, "/acme/demo/settings/rename") {
			t.Fatalf("expected rename form; body=%s", body)
		}
		if !strings.Contains(body, "/acme/demo/settings/delete") {
			t.Fatalf("global admin must see delete form; body=%s", body)
		}
		if !strings.Contains(body, `name="confirm"`) {
			t.Fatalf("delete form must have a confirm input; body=%s", body)
		}
	})

	t.Run("repo-admin sees neither rename nor delete (both admin-only)", func(t *testing.T) {
		store := mkStore()
		store.perm = auth.PermAdmin
		h := newTestHandlerWith(store, nil)
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		// Rename is now an operator procedure (auth rename + out-of-band storage
		// migration) gated to global admin like delete, so a repo-admin sees the
		// danger-zone forms for neither.
		if strings.Contains(body, "/acme/demo/settings/rename") {
			t.Fatalf("repo-admin must NOT see rename form; body=%s", body)
		}
		if strings.Contains(body, "/acme/demo/settings/delete") {
			t.Fatalf("repo-admin must NOT see delete form; body=%s", body)
		}
	})
}
