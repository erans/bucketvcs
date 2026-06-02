package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func newTestHandler(store DataStore) http.Handler {
	return NewHandler(Deps{
		Store:   store,
		Logger:  slog.Default(),
		Limiter: nil, // nil limiter is a no-op
	})
}

func TestLoginFlow(t *testing.T) {
	store := newFakeStore()
	store.verify = func(ctx context.Context, u, p string) (*auth.Actor, error) {
		if u == "alice" && p == "pw" {
			return &auth.Actor{UserID: "u1", Name: "alice"}, nil
		}
		return nil, auth.ErrInvalidCredential
	}
	store.repos = func(a *auth.Actor) []Repo {
		if a == nil {
			return []Repo{{Tenant: "acme", Name: "pub", PublicRead: true}}
		}
		return []Repo{{Tenant: "acme", Name: "pub", PublicRead: true}, {Tenant: "acme", Name: "priv"}}
	}
	h := newTestHandler(store)

	// GET /login => 200 + csrf cookie + token in form
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /login status %d", rec.Code)
	}
	csrfCookie := findCookie(rec.Result().Cookies(), csrfCookieName)
	if csrfCookie == nil {
		t.Fatal("no csrf cookie")
	}
	tok := extractHidden(rec.Body.String(), "csrf_token")
	if tok == "" {
		t.Fatal("no csrf token in form")
	}

	// POST /login (good creds) => 303 + session cookie
	form := url.Values{"username": {"alice"}, "password": {"pw"}, "csrf_token": {tok}, "next": {"/"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login status %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	sessCookie := findCookie(rec.Result().Cookies(), sessionCookieName)
	if sessCookie == nil || sessCookie.Value == "" {
		t.Fatal("no session cookie issued")
	}

	// GET / with session => shows private repo
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessCookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "priv") {
		t.Fatalf("landing missing private repo:\n%s", rec.Body.String())
	}

	// GET / anon => no private repo
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(rec.Body.String(), "priv") {
		t.Fatalf("anon landing leaked private repo:\n%s", rec.Body.String())
	}
}

func TestLoginBadPassword(t *testing.T) {
	store := newFakeStore()
	store.verify = func(ctx context.Context, u, p string) (*auth.Actor, error) {
		return nil, auth.ErrInvalidCredential
	}
	h := newTestHandler(store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	csrfCookie := findCookie(rec.Result().Cookies(), csrfCookieName)
	tok := extractHidden(rec.Body.String(), "csrf_token")

	form := url.Values{"username": {"alice"}, "password": {"bad"}, "csrf_token": {tok}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login status %d, want 401", rec.Code)
	}
	if findCookie(rec.Result().Cookies(), sessionCookieName) != nil {
		t.Fatal("session cookie issued on bad login")
	}
}

func TestLoginCSRFRejected(t *testing.T) {
	store := newFakeStore()
	store.verify = func(ctx context.Context, u, p string) (*auth.Actor, error) {
		return &auth.Actor{UserID: "u1", Name: "alice"}, nil
	}
	h := newTestHandler(store)
	// POST without csrf cookie/token => 403
	form := url.Values{"username": {"alice"}, "password": {"pw"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing csrf status %d, want 403", rec.Code)
	}
}

// helpers
func findCookie(cs []*http.Cookie, name string) *http.Cookie {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func extractHidden(html, field string) string {
	marker := `name="` + field + `" value="`
	i := strings.Index(html, marker)
	if i < 0 {
		return ""
	}
	rest := html[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}
