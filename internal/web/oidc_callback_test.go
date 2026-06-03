package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/oidc"
)

type fakeVerifier struct {
	claims oidc.Claims
	err    error
}

func (f fakeVerifier) Verify(ctx context.Context, raw, issuer string) (oidc.Claims, error) {
	return f.claims, f.err
}

func callbackEnv(t *testing.T, store DataStore, claims oidc.Claims, verr error) (http.Handler, func()) {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "id_token": "dummy",
		})
	}))
	dep := Deps{
		Store: store,
		OIDC: &OIDCProvider{
			Issuer: "https://idp.example.com", ClientID: "cid",
			AuthURL: "https://idp.example.com/authorize", TokenURL: tokenSrv.URL,
			RedirectURL: "https://app/login/oidc/callback",
			Scopes:      []string{"openid", "email"},
			HMACKey:     []byte("0123456789abcdef0123456789abcdef"),
			Verifier:    fakeVerifier{claims: claims, err: verr},
		},
	}
	return NewHandler(dep), tokenSrv.Close
}

func mkStateCookie(hmacKey []byte, state, nonce string) *http.Cookie {
	return &http.Cookie{Name: oidcCookieName, Value: encodeOIDCState(hmacKey, oidcState{
		State: state, Nonce: nonce, Verifier: "v", Next: "/", Exp: time.Now().Add(10 * time.Minute).Unix(),
	})}
}

func doCallback(t *testing.T, h http.Handler, hmacKey []byte, cookieState, cookieNonce, code, queryState string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{"code": {code}, "state": {queryState}}
	req := httptest.NewRequest("GET", "/login/oidc/callback?"+q.Encode(), nil)
	req.AddCookie(mkStateCookie(hmacKey, cookieState, cookieNonce))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func goodClaims() oidc.Claims {
	return oidc.Claims{
		"iss": "https://idp.example.com", "sub": "sub-1", "aud": "cid",
		"nonce": "nonce-1", "email": "alice@corp.com", "email_verified": true,
	}
}

var hmacKey = []byte("0123456789abcdef0123456789abcdef")

func TestOIDCCallback_HappyTOFU(t *testing.T) {
	store := newFakeStore()
	linked := false
	store.findIdentity = func(iss, sub string) (*auth.Actor, error) { return nil, auth.ErrNoSuchUser }
	store.findByEmail = func(email string) (*auth.Actor, error) {
		if email == "alice@corp.com" {
			return &auth.Actor{UserID: "u1", Name: "alice"}, nil
		}
		return nil, auth.ErrNoSuchUser
	}
	store.linkIdentity = func(uid, iss, sub, email string) error { linked = true; return nil }

	h, done := callbackEnv(t, store, goodClaims(), nil)
	defer done()
	rec := doCallback(t, h, hmacKey, "state-1", "nonce-1", "code-1", "state-1")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if !linked {
		t.Fatal("expected TOFU LinkIdentity call")
	}
	if findCookie(rec.Result().Cookies(), sessionCookieName) == nil {
		t.Fatal("no session cookie issued")
	}
}

func TestOIDCCallback_PreLinkedNoRelink(t *testing.T) {
	store := newFakeStore()
	relinked := false
	store.findIdentity = func(iss, sub string) (*auth.Actor, error) {
		return &auth.Actor{UserID: "u1", Name: "alice"}, nil
	}
	store.linkIdentity = func(uid, iss, sub, email string) error { relinked = true; return nil }
	h, done := callbackEnv(t, store, goodClaims(), nil)
	defer done()
	rec := doCallback(t, h, hmacKey, "state-1", "nonce-1", "code-1", "state-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", rec.Code)
	}
	if relinked {
		t.Fatal("should not re-link an already-linked identity")
	}
}

func TestOIDCCallback_Rejections(t *testing.T) {
	base := func() *fakeStore {
		s := newFakeStore()
		s.findIdentity = func(iss, sub string) (*auth.Actor, error) { return nil, auth.ErrNoSuchUser }
		s.findByEmail = func(email string) (*auth.Actor, error) { return &auth.Actor{UserID: "u1", Name: "alice"}, nil }
		return s
	}
	cases := []struct {
		name       string
		claims     oidc.Claims
		verr       error
		cookieN    string
		queryState string
		findByMail func(string) (*auth.Actor, error)
		wantCode   int
	}{
		{"state_mismatch", goodClaims(), nil, "nonce-1", "WRONG", nil, http.StatusBadRequest},
		{"nonce_mismatch", func() oidc.Claims { c := goodClaims(); c["nonce"] = "other"; return c }(), nil, "nonce-1", "state-1", nil, http.StatusUnauthorized},
		{"aud_mismatch", func() oidc.Claims { c := goodClaims(); c["aud"] = "someone-else"; return c }(), nil, "nonce-1", "state-1", nil, http.StatusUnauthorized},
		{"email_unverified", func() oidc.Claims { c := goodClaims(); c["email_verified"] = false; return c }(), nil, "nonce-1", "state-1", nil, http.StatusUnauthorized},
		{"token_invalid", goodClaims(), oidc.ErrInvalidToken, "nonce-1", "state-1", nil, http.StatusUnauthorized},
		{"no_user", goodClaims(), nil, "nonce-1", "state-1", func(string) (*auth.Actor, error) { return nil, auth.ErrNoSuchUser }, http.StatusUnauthorized},
		{"disabled", goodClaims(), nil, "nonce-1", "state-1", func(string) (*auth.Actor, error) { return nil, auth.ErrUserDisabled }, http.StatusUnauthorized},
		{"empty_sub", func() oidc.Claims { c := goodClaims(); delete(c, "sub"); return c }(), nil, "nonce-1", "state-1", nil, http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := base()
			if c.findByMail != nil {
				store.findByEmail = c.findByMail
			}
			h, done := callbackEnv(t, store, c.claims, c.verr)
			defer done()
			rec := doCallback(t, h, hmacKey, "state-1", c.cookieN, "code-1", c.queryState)
			if rec.Code != c.wantCode {
				t.Fatalf("%s: status %d, want %d; body=%s", c.name, rec.Code, c.wantCode, rec.Body.String())
			}
			if findCookie(rec.Result().Cookies(), sessionCookieName) != nil {
				t.Fatalf("%s: session cookie must NOT be set on rejection", c.name)
			}
		})
	}
}

func TestOIDCCallback_MissingCookie(t *testing.T) {
	store := newFakeStore()
	h, done := callbackEnv(t, store, goodClaims(), nil)
	defer done()
	req := httptest.NewRequest("GET", "/login/oidc/callback?code=c&state=s", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 on missing temp cookie", rec.Code)
	}
}

func TestOIDCCallback_IdPError(t *testing.T) {
	store := newFakeStore()
	h, done := callbackEnv(t, store, goodClaims(), nil)
	defer done()
	req := httptest.NewRequest("GET", "/login/oidc/callback?error=access_denied", nil)
	req.AddCookie(mkStateCookie(hmacKey, "s", "n"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code < 400 {
		t.Fatalf("status %d, want >=400 on IdP error", rec.Code)
	}
}

// Bad-HMAC / undecodable temp cookie → 400, distinct from missing-cookie.
func TestOIDCCallback_TamperedCookie(t *testing.T) {
	store := newFakeStore()
	h, done := callbackEnv(t, store, goodClaims(), nil)
	defer done()
	// A cookie value that fails HMAC verification.
	req := httptest.NewRequest("GET", "/login/oidc/callback?code=c&state=state-1", nil)
	req.AddCookie(&http.Cookie{Name: oidcCookieName, Value: "garbage.not-a-valid-hmac"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tampered cookie: status %d, want 400", rec.Code)
	}
	if findCookie(rec.Result().Cookies(), sessionCookieName) != nil {
		t.Fatal("no session on tampered cookie")
	}
}

// Token endpoint returns no id_token → 401.
func TestOIDCCallback_MissingIDToken(t *testing.T) {
	store := newFakeStore()
	store.findIdentity = func(iss, sub string) (*auth.Actor, error) { return nil, auth.ErrNoSuchUser }
	store.findByEmail = func(email string) (*auth.Actor, error) { return &auth.Actor{UserID: "u1", Name: "alice"}, nil }
	// Build a handler whose token endpoint omits id_token.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer"}) // no id_token
	}))
	defer tokenSrv.Close()
	h := NewHandler(Deps{
		Store: store,
		OIDC: &OIDCProvider{
			Issuer: "https://idp.example.com", ClientID: "cid",
			AuthURL: "https://idp.example.com/authorize", TokenURL: tokenSrv.URL,
			RedirectURL: "https://app/login/oidc/callback", Scopes: []string{"openid", "email"},
			HMACKey:  hmacKey,
			Verifier: fakeVerifier{claims: goodClaims()},
		},
	})
	rec := doCallback(t, h, hmacKey, "state-1", "nonce-1", "code-1", "state-1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing id_token: status %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if findCookie(rec.Result().Cookies(), sessionCookieName) != nil {
		t.Fatal("no session when id_token missing")
	}
}

// LinkIdentity races with a concurrent link (ErrConflict) → handler re-resolves
// via FindIdentity and still issues a session (303), no duplicate link.
func TestOIDCCallback_LinkConflictReResolves(t *testing.T) {
	store := newFakeStore()
	// First FindIdentity (pre-link) misses; after the conflicting link, the
	// re-resolve must succeed. Model with a call counter.
	calls := 0
	store.findIdentity = func(iss, sub string) (*auth.Actor, error) {
		calls++
		if calls == 1 {
			return nil, auth.ErrNoSuchUser // initial miss → TOFU path
		}
		return &auth.Actor{UserID: "u1", Name: "alice"}, nil // re-resolve after conflict
	}
	store.findByEmail = func(email string) (*auth.Actor, error) {
		return &auth.Actor{UserID: "u1", Name: "alice"}, nil
	}
	store.linkIdentity = func(uid, iss, sub, email string) error { return auth.ErrConflict }

	h, done := callbackEnv(t, store, goodClaims(), nil)
	defer done()
	rec := doCallback(t, h, hmacKey, "state-1", "nonce-1", "code-1", "state-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("conflict-race: status %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if findCookie(rec.Result().Cookies(), sessionCookieName) == nil {
		t.Fatal("conflict-race should still issue a session after re-resolve")
	}
	if calls != 2 {
		t.Fatalf("conflict-race: FindIdentity called %d times, want 2 (initial miss + re-resolve)", calls)
	}
}

// On success, the temp cookie is cleared (MaxAge<0) and the session cookie has the hardened attrs.
func TestOIDCCallback_CookieHygiene(t *testing.T) {
	store := newFakeStore()
	store.findIdentity = func(iss, sub string) (*auth.Actor, error) { return &auth.Actor{UserID: "u1", Name: "alice"}, nil }
	h, done := callbackEnv(t, store, goodClaims(), nil)
	defer done()
	rec := doCallback(t, h, hmacKey, "state-1", "nonce-1", "code-1", "state-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", rec.Code)
	}
	cleared := findCookie(rec.Result().Cookies(), oidcCookieName)
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("temp cookie not cleared: %+v", cleared)
	}
	sess := findCookie(rec.Result().Cookies(), sessionCookieName)
	if sess == nil || !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" {
		t.Fatalf("session cookie attrs wrong: %+v", sess)
	}
}
