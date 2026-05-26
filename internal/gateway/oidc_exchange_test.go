package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// fakeVerifier returns preset claims for a matching token, else an error.
type fakeVerifier struct {
	claims map[string]any
	jwks   int // counts Verify calls (proxy for "fetch happened")
}

func (f *fakeVerifier) Verify(ctx context.Context, raw, issuer string) (map[string]any, error) {
	f.jwks++
	if raw == "bad" {
		return nil, auth.ErrInvalidCredential
	}
	return f.claims, nil
}

// fakeOIDCStore implements OIDCExchangeStore.
type fakeOIDCStore struct {
	issuers  map[string]auth.OIDCIssuer // keyed by url
	rules    []auth.OIDCTrustRule
	minted   int
	lastMint MintRecord
}

type MintRecord struct {
	Tenant, Repo string
	Perm         auth.Perm
	Scopes       auth.TokenScope
}

func (s *fakeOIDCStore) FindOIDCIssuerByURL(ctx context.Context, u string) (auth.OIDCIssuer, error) {
	i, ok := s.issuers[u]
	if !ok {
		return auth.OIDCIssuer{}, auth.ErrNoSuchRepo // any not-found sentinel
	}
	return i, nil
}
func (s *fakeOIDCStore) ListOIDCRulesForIssuer(ctx context.Context, alias string) ([]auth.OIDCTrustRule, error) {
	return s.rules, nil
}
func (s *fakeOIDCStore) MintOIDCToken(ctx context.Context, tenant, repo string, perm auth.Perm,
	scopes auth.TokenScope, ttl int64, label string) (string, error) {
	s.minted++
	s.lastMint = MintRecord{tenant, repo, perm, scopes}
	return "bvts_minted", nil
}

// fakeJWT builds a compact token whose payload carries iss (signature is not
// checked by fakeVerifier).
func fakeJWT(iss string) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	pl := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + iss + `"}`))
	return hdr + "." + pl + ".sig"
}

func newOIDCTestServer(t *testing.T, store *fakeOIDCStore, ver *fakeVerifier) *Server {
	t.Helper()
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.opts = Options{OIDCEnabled: true, OIDCStore: store, OIDCVerifier: ver}
	return s
}

func TestOIDCExchange_HappyPath(t *testing.T) {
	iss := "https://i.example"
	store := &fakeOIDCStore{
		issuers: map[string]auth.OIDCIssuer{iss: {Alias: "gh", IssuerURL: iss}},
		rules: []auth.OIDCTrustRule{{
			ID: "r1", IssuerAlias: "gh", Audience: "aud", Tenant: "org", Repo: "app",
			Scopes: auth.ScopeRepoWrite, TTLSeconds: 900, Claims: map[string]string{"repository": "org/app"},
		}},
	}
	ver := &fakeVerifier{claims: map[string]any{"aud": "aud", "sub": "s", "repository": "org/app"}}
	s := newOIDCTestServer(t, store, ver)

	form := url.Values{
		"grant_type":    {grantTokenExchange},
		"subject_token": {fakeJWT(iss)},
	}
	r := httptest.NewRequest(http.MethodPost, "/_oidc/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleOIDCExchange(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["access_token"] != "bvts_minted" {
		t.Fatalf("token = %v", resp["access_token"])
	}
	if store.lastMint.Perm != auth.PermWrite || store.lastMint.Repo != "app" {
		t.Fatalf("mint = %+v", store.lastMint)
	}
}

func TestOIDCExchange_UnknownIssuer_NoVerify(t *testing.T) {
	store := &fakeOIDCStore{issuers: map[string]auth.OIDCIssuer{}}
	ver := &fakeVerifier{}
	s := newOIDCTestServer(t, store, ver)
	form := url.Values{"grant_type": {grantTokenExchange}, "subject_token": {fakeJWT("https://nope")}}
	r := httptest.NewRequest(http.MethodPost, "/_oidc/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleOIDCExchange(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
	if ver.jwks != 0 {
		t.Fatalf("verifier called %d times for unregistered issuer, want 0", ver.jwks)
	}
}

func TestOIDCExchange_NoRule_403(t *testing.T) {
	iss := "https://i.example"
	store := &fakeOIDCStore{
		issuers: map[string]auth.OIDCIssuer{iss: {Alias: "gh", IssuerURL: iss}},
		rules:   []auth.OIDCTrustRule{{ID: "r1", IssuerAlias: "gh", Audience: "other", Tenant: "org", Repo: "app"}},
	}
	ver := &fakeVerifier{claims: map[string]any{"aud": "aud", "sub": "s"}}
	s := newOIDCTestServer(t, store, ver)
	form := url.Values{"grant_type": {grantTokenExchange}, "subject_token": {fakeJWT(iss)}}
	r := httptest.NewRequest(http.MethodPost, "/_oidc/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleOIDCExchange(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d", w.Code)
	}
}

func TestOIDCExchange_DownScopeReadFromWriteRule(t *testing.T) {
	iss := "https://i.example"
	store := &fakeOIDCStore{
		issuers: map[string]auth.OIDCIssuer{iss: {Alias: "gh", IssuerURL: iss}},
		rules: []auth.OIDCTrustRule{{
			ID: "r1", IssuerAlias: "gh", Audience: "aud", Tenant: "org", Repo: "app",
			Scopes: auth.ScopeRepoWrite, TTLSeconds: 900, Claims: map[string]string{},
		}},
	}
	ver := &fakeVerifier{claims: map[string]any{"aud": "aud", "sub": "s"}}
	s := newOIDCTestServer(t, store, ver)
	form := url.Values{
		"grant_type":    {grantTokenExchange},
		"subject_token": {fakeJWT(iss)},
		"scope":         {"repo:read"},
	}
	r := httptest.NewRequest(http.MethodPost, "/_oidc/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleOIDCExchange(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if store.lastMint.Perm != auth.PermRead {
		t.Fatalf("perm = %v, want PermRead", store.lastMint.Perm)
	}
	if store.lastMint.Scopes != auth.ScopeRepoRead {
		t.Fatalf("scopes = %v, want repo:read only", store.lastMint.Scopes)
	}
}
