package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func oidcTestDeps(store DataStore) Deps {
	return Deps{
		Store: store,
		OIDC: &OIDCProvider{
			Issuer:      "https://idp.example.com",
			ClientID:    "cid",
			AuthURL:     "https://idp.example.com/authorize",
			TokenURL:    "https://idp.example.com/token",
			RedirectURL: "https://app.example.com/login/oidc/callback",
			Scopes:      []string{"openid", "email", "profile"},
			Label:       "Single sign-on",
			HMACKey:     []byte("0123456789abcdef0123456789abcdef"),
		},
	}
}

func TestOIDCAuthorize_RedirectsWithParams(t *testing.T) {
	h := NewHandler(oidcTestDeps(newFakeStore()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login/oidc?next=/repos", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("bad Location %q: %v", loc, err)
	}
	if !strings.HasPrefix(loc, "https://idp.example.com/authorize") {
		t.Fatalf("Location not the auth endpoint: %s", loc)
	}
	q := u.Query()
	for _, k := range []string{"client_id", "state", "nonce", "code_challenge", "redirect_uri"} {
		if q.Get(k) == "" {
			t.Fatalf("missing %s in %s", k, loc)
		}
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q", q.Get("code_challenge_method"))
	}
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type = %q", q.Get("response_type"))
	}
	if findCookie(rec.Result().Cookies(), oidcCookieName) == nil {
		t.Fatal("no bvcs_oidc cookie")
	}
}

func TestOIDCAuthorize_DisabledIs404(t *testing.T) {
	h := NewHandler(Deps{Store: newFakeStore()}) // no OIDC
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login/oidc", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 when OIDC disabled", rec.Code)
	}
}
