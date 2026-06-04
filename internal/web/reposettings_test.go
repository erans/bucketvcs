package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
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
		path string
		want int
	}{
		{"anon redirects to login", nil, auth.PermNone, "/acme/demo/settings", http.StatusSeeOther},
		{"non-admin reader 404", userSession(), auth.PermRead, "/acme/demo/settings", http.StatusNotFound},
		{"repo admin 200", userSession(), auth.PermAdmin, "/acme/demo/settings", http.StatusOK},
		{"global admin 200", adminSession(), auth.PermNone, "/acme/demo/settings", http.StatusOK},
		{"repo admin unknown tab 404", userSession(), auth.PermAdmin, "/acme/demo/settings/bogus", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.perm = tc.perm
			h := newTestHandlerWith(store, nil)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
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
