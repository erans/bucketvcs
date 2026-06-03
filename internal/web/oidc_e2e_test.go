package web

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// TestOIDC_EndToEnd drives the full browser-login flow against the REAL
// internal/oidc.Verifier (Verifier:nil in the provider → NewHandler builds
// oidc.NewVerifier()). A stub IdP, built on httptest, performs OIDC discovery,
// serves a JWKS, and signs a real RS256 id_token. A successful 303 + session
// cookie proves the real verifier validated the RS256 signature against the
// stub JWKS, that iss/exp/aud/nonce matched, and that verified-email TOFU
// linked the identity and issued a session.
//
// httptest.NewServer binds 127.0.0.1, which the discovery layer treats as a
// loopback exception to its https-only rule — so http is allowed here.
func TestOIDC_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: real RSA keygen + HTTP discovery; skipped under -short")
	}

	// 1. One RSA key for the whole test (signing + JWKS public half).
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	const kid = "test-key-1"

	// The /token handler signs an id_token whose nonce must match the value
	// minted by the authorize step. The test sets this between steps.
	var capturedNonce string

	// stubURL is needed inside the handlers (iss claim, discovery doc), but the
	// server's URL is only known after httptest.NewServer returns. Capture it
	// into a closure variable that the handlers read at request time.
	var stubURL string

	signIDToken := func(t *testing.T, nonce string) string {
		t.Helper()
		now := time.Now()
		claims := map[string]any{
			"iss":            stubURL,
			"sub":            "sub-e2e",
			"aud":            "cid",
			"nonce":          nonce,
			"email":          "alice@corp.com",
			"email_verified": true,
			"exp":            now.Add(time.Hour).Unix(),
			"iat":            now.Unix(),
		}
		signingKey := jose.SigningKey{
			Algorithm: jose.RS256,
			Key:       jose.JSONWebKey{Key: priv, KeyID: kid, Algorithm: "RS256", Use: "sig"},
		}
		signer, err := jose.NewSigner(signingKey, (&jose.SignerOptions{}).WithType("JWT"))
		if err != nil {
			t.Fatalf("new signer: %v", err)
		}
		payload, err := json.Marshal(claims)
		if err != nil {
			t.Fatalf("marshal claims: %v", err)
		}
		jws, err := signer.Sign(payload)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		compact, err := jws.CompactSerialize()
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		return compact
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 stubURL,
			"authorization_endpoint": stubURL + "/authorize",
			"token_endpoint":         stubURL + "/token",
			"jwks_uri":               stubURL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &priv.PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig",
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at",
			"token_type":   "Bearer",
			"id_token":     signIDToken(t, capturedNonce),
		})
	})

	stub := httptest.NewServer(mux)
	defer stub.Close()
	stubURL = stub.URL

	provider := &OIDCProvider{
		Issuer:      stubURL,
		ClientID:    "cid",
		AuthURL:     stubURL + "/authorize",
		TokenURL:    stubURL + "/token",
		RedirectURL: stubURL + "/login/oidc/callback",
		Scopes:      []string{"openid", "email"},
		HMACKey:     []byte("0123456789abcdef0123456789abcdef"),
		Verifier:    nil, // <- REAL verifier
	}

	store := newFakeStore()
	linked := false
	store.findIdentity = func(iss, sub string) (*auth.Actor, error) {
		return nil, auth.ErrNoSuchUser
	}
	store.findByEmail = func(email string) (*auth.Actor, error) {
		if email == "alice@corp.com" {
			return &auth.Actor{UserID: "u1", Name: "alice"}, nil
		}
		return nil, auth.ErrNoSuchUser
	}
	store.linkIdentity = func(uid, iss, sub, email string) error { linked = true; return nil }

	h := NewHandler(Deps{Store: store, OIDC: provider})

	// runLogin performs the authorize → callback round-trip and returns the
	// callback recorder.
	runLogin := func(t *testing.T) *httptest.ResponseRecorder {
		t.Helper()

		// 6. authorize → 302, capture state + nonce + temp cookie.
		authRec := httptest.NewRecorder()
		h.ServeHTTP(authRec, httptest.NewRequest("GET", "/login/oidc?next=/", nil))
		if authRec.Code != http.StatusFound {
			t.Fatalf("authorize: status %d, want 302; body=%s", authRec.Code, authRec.Body.String())
		}
		loc, err := url.Parse(authRec.Header().Get("Location"))
		if err != nil {
			t.Fatalf("authorize: bad Location: %v", err)
		}
		state := loc.Query().Get("state")
		nonce := loc.Query().Get("nonce")
		if state == "" || nonce == "" {
			t.Fatalf("authorize: missing state/nonce in %s", loc)
		}
		tempCookie := findCookie(authRec.Result().Cookies(), oidcCookieName)
		if tempCookie == nil {
			t.Fatal("authorize: no bvcs_oidc cookie")
		}

		// 7. The stub's /token must sign with this nonce.
		capturedNonce = nonce

		// 8. callback with captured state + temp cookie.
		q := url.Values{"code": {"anycode"}, "state": {state}}
		cbReq := httptest.NewRequest("GET", "/login/oidc/callback?"+q.Encode(), nil)
		cbReq.AddCookie(tempCookie)
		cbRec := httptest.NewRecorder()
		h.ServeHTTP(cbRec, cbReq)
		return cbRec
	}

	// First login: TOFU path (FindIdentity misses → match by verified email → link).
	rec := runLogin(t)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback (TOFU): status %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if findCookie(rec.Result().Cookies(), sessionCookieName) == nil {
		t.Fatal("callback (TOFU): no bvcs_session cookie issued")
	}
	if !linked {
		t.Fatal("callback (TOFU): expected LinkIdentity call")
	}

	// 9. Second login: identity already pinned → already-linked path, still 303.
	store.findIdentity = func(iss, sub string) (*auth.Actor, error) {
		return &auth.Actor{UserID: "u1", Name: "alice"}, nil
	}
	relinked := false
	store.linkIdentity = func(uid, iss, sub, email string) error { relinked = true; return nil }

	rec2 := runLogin(t)
	if rec2.Code != http.StatusSeeOther {
		t.Fatalf("callback (pre-linked): status %d, want 303; body=%s", rec2.Code, rec2.Body.String())
	}
	if findCookie(rec2.Result().Cookies(), sessionCookieName) == nil {
		t.Fatal("callback (pre-linked): no bvcs_session cookie issued")
	}
	if relinked {
		t.Fatal("callback (pre-linked): should not re-link an already-pinned identity")
	}
}
