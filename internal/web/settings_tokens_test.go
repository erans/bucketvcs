package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// tokensFixture builds a fakeStore with two tokens for "user1":
// one active, one revoked.
func tokensFixture() *fakeStore {
	store := newFakeStore()
	now := time.Now().Unix()
	revoked := now - 60
	store.listTokensForUser = func(ctx context.Context, name string) ([]TokenInfo, error) {
		return []TokenInfo{
			{
				ID:        "tok1AAAAAAAAAAAAAAAAAAA",
				Label:     "ci-token",
				Scopes:    auth.ScopeRepoRead | auth.ScopeLFSRead,
				CreatedAt: now - 3600,
			},
			{
				ID:        "tok2AAAAAAAAAAAAAAAAAAA",
				Label:     "old-token",
				Scopes:    auth.ScopeLegacy,
				CreatedAt: now - 7200,
				RevokedAt: &revoked,
			},
		}, nil
	}
	return store
}

// --- GET /settings/tokens ---

func TestTokensPageRequiresLogin(t *testing.T) {
	store := newFakeStore()
	h := newTestHandler(store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/tokens", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("anon GET /settings/tokens: status %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login") {
		t.Fatalf("anon GET /settings/tokens: Location %q, want /login...", loc)
	}
}

func TestTokensPageRenders(t *testing.T) {
	store := tokensFixture()
	h := newTestHandler(store)

	req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/settings/tokens", nil), store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/tokens: status %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// labels visible
	if !strings.Contains(body, "ci-token") {
		t.Fatalf("token page: label 'ci-token' missing:\n%s", body)
	}
	if !strings.Contains(body, "old-token") {
		t.Fatalf("token page: label 'old-token' missing:\n%s", body)
	}

	// scopes rendered via scopestr
	if !strings.Contains(body, "repo:read") {
		t.Fatalf("token page: 'repo:read' scope missing:\n%s", body)
	}
	if !strings.Contains(body, "lfs:read") {
		t.Fatalf("token page: 'lfs:read' scope missing:\n%s", body)
	}

	// legacy scope warning text on the create form
	if !strings.Contains(body, "full access") {
		t.Fatalf("token page: legacy scope warning 'full access' missing:\n%s", body)
	}

	// revoked state
	if !strings.Contains(body, "revoked") {
		t.Fatalf("token page: 'revoked' state missing:\n%s", body)
	}

	// active state
	if !strings.Contains(body, "active") {
		t.Fatalf("token page: 'active' state missing:\n%s", body)
	}

	// create form with csrf_token
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Fatalf("token page: csrf_token missing in form:\n%s", body)
	}
}

func TestTokensPageExpiredState(t *testing.T) {
	store := newFakeStore()
	past := time.Now().Add(-time.Hour).Unix()
	store.listTokensForUser = func(ctx context.Context, name string) ([]TokenInfo, error) {
		return []TokenInfo{
			{
				ID:        "tokExpAAAAAAAAAAAAAAAAA",
				Label:     "expired-token",
				Scopes:    auth.ScopeRepoRead,
				CreatedAt: time.Now().Add(-24 * time.Hour).Unix(),
				ExpiresAt: &past,
			},
		}, nil
	}
	h := newTestHandler(store)
	req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/settings/tokens", nil), store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("expired state not shown:\n%s", rec.Body.String())
	}
}

// --- POST /settings/tokens/create ---

func TestTokenCreateFormSecurity(t *testing.T) {
	store := newFakeStore()
	h := newTestHandler(store)
	assertFormSecurity(t, h, secOpts{
		store: store,
		path:  "/settings/tokens/create",
		form:  url.Values{"label": {"ci"}, "scopes": {"repo:read"}, "expires": {"720h"}},
	})
}

func TestTokenCreateBadScopes(t *testing.T) {
	store := newFakeStore()
	var createCalled bool
	store.createToken = func(ctx context.Context, id, userID, secretHash, label string, expiresAt *int64, scopes auth.TokenScope) error {
		createCalled = true
		return nil
	}
	h := newTestHandler(store)

	req := csrfPost(t, "/settings/tokens/create", url.Values{
		"label":  {"ci"},
		"scopes": {"bogus"},
	})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("bad scopes: status %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/settings/tokens" {
		t.Fatalf("bad scopes: Location %q, want /settings/tokens", loc)
	}
	if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
		t.Fatal("bad scopes: no flash cookie")
	}
	if createCalled {
		t.Fatal("bad scopes: CreateToken should not have been called")
	}
}

func TestTokenCreateBadExpiry(t *testing.T) {
	store := newFakeStore()
	var createCalled bool
	store.createToken = func(ctx context.Context, id, userID, secretHash, label string, expiresAt *int64, scopes auth.TokenScope) error {
		createCalled = true
		return nil
	}
	h := newTestHandler(store)

	req := csrfPost(t, "/settings/tokens/create", url.Values{
		"label":   {"ci"},
		"scopes":  {"repo:read"},
		"expires": {"not-a-duration"},
	})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("bad expiry: status %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/settings/tokens" {
		t.Fatalf("bad expiry: Location %q, want /settings/tokens", loc)
	}
	if createCalled {
		t.Fatal("bad expiry: CreateToken should not have been called")
	}
}

func TestTokenCreateHappy(t *testing.T) {
	logger, sink := newTestLogger()
	store := newFakeStore()

	var capturedID, capturedUserID, capturedLabel string
	var capturedScopes auth.TokenScope
	var capturedExpiry *int64
	store.createToken = func(ctx context.Context, id, userID, secretHash, label string, expiresAt *int64, scopes auth.TokenScope) error {
		capturedID = id
		capturedUserID = userID
		capturedLabel = label
		capturedScopes = scopes
		capturedExpiry = expiresAt
		return nil
	}
	h := NewHandler(Deps{Store: store, Logger: logger})

	before := time.Now().Add(-time.Second) // 1s slack for test scheduling
	req := csrfPost(t, "/settings/tokens/create", url.Values{
		"label":   {"ci"},
		"scopes":  {"repo:read,lfs:read"},
		"expires": {"720h"},
	})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	after := time.Now()

	if rec.Code != http.StatusOK {
		t.Fatalf("happy create: status %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}

	// body must contain bvts_ plaintext EXACTLY once
	body := rec.Body.String()
	count := strings.Count(body, "bvts_")
	if count != 1 {
		t.Fatalf("happy create: body should contain 'bvts_' exactly once, got %d:\n%s", count, body)
	}

	// body must contain "will not be shown again"
	if !strings.Contains(body, "will not be shown again") {
		t.Fatalf("happy create: 'will not be shown again' missing:\n%s", body)
	}

	// CreateToken called with session's UserID
	if capturedUserID != "user1" {
		t.Fatalf("happy create: CreateToken userID %q, want %q", capturedUserID, "user1")
	}
	if capturedLabel != "ci" {
		t.Fatalf("happy create: CreateToken label %q, want %q", capturedLabel, "ci")
	}
	want := auth.ScopeRepoRead | auth.ScopeLFSRead
	if capturedScopes != want {
		t.Fatalf("happy create: scopes %v, want %v", capturedScopes, want)
	}
	// expiry ≈ now + 720h
	if capturedExpiry == nil {
		t.Fatal("happy create: expiry should be set for 720h")
	}
	expTime := time.Unix(*capturedExpiry, 0)
	wantExpLo := before.Add(720 * time.Hour)
	wantExpHi := after.Add(720 * time.Hour)
	if expTime.Before(wantExpLo) || expTime.After(wantExpHi) {
		t.Fatalf("happy create: expiry %v not in expected range [%v, %v]", expTime, wantExpLo, wantExpHi)
	}

	// audit event
	if !sink.Has("auth.token.created", map[string]string{
		"token_id": capturedID,
		"label":    "ci",
		"actor":    "user",
		"source":   "web",
	}) {
		t.Fatal("happy create: audit event auth.token.created not logged with expected attrs")
	}

	// plaintext NOT in Location header (there isn't one on 200, but verify
	// it's not in any Set-Cookie value either)
	for _, c := range rec.Result().Cookies() {
		if strings.Contains(c.Value, "bvts_") {
			t.Fatalf("happy create: plaintext token leaked into Set-Cookie %s=%s", c.Name, c.Value)
		}
	}

	// secret page must not be cached
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store, private" {
		t.Fatalf("happy create: Cache-Control %q, want \"no-store, private\"", cc)
	}
}

func TestTokenCreateLegacyScopes(t *testing.T) {
	logger, sink := newTestLogger()
	store := newFakeStore()
	var capturedID string
	var capturedScopes auth.TokenScope
	store.createToken = func(ctx context.Context, id, userID, secretHash, label string, expiresAt *int64, scopes auth.TokenScope) error {
		capturedID = id
		capturedScopes = scopes
		return nil
	}
	h := NewHandler(Deps{Store: store, Logger: logger})

	// empty scopes field => legacy (ScopeLegacy == 0)
	req := csrfPost(t, "/settings/tokens/create", url.Values{
		"label":  {"no-scope"},
		"scopes": {""},
	})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("legacy scopes: status %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if capturedScopes != auth.ScopeLegacy {
		t.Fatalf("legacy scopes: scopes %v, want ScopeLegacy", capturedScopes)
	}

	// audit parity: the elevated legacy grant must be flagged.
	if !sink.Has("auth.token.created", map[string]string{
		"token_id": capturedID,
		"legacy":   "true",
	}) {
		t.Fatal("legacy scopes: audit event auth.token.created missing legacy=true attr")
	}
}

// --- POST /settings/tokens/revoke ---

func TestTokenRevokeFormSecurity(t *testing.T) {
	store := newFakeStore()
	// getTokenOwner returns the other user for the "asSession" probe → 404
	otherUserID := "other1"
	store.getTokenOwner = func(ctx context.Context, id string) (string, error) {
		return otherUserID, nil
	}
	h := newTestHandler(store)
	assertFormSecurity(t, h, secOpts{
		store:     store,
		path:      "/settings/tokens/revoke",
		form:      url.Values{"id": {"tok1AAAAAAAAAAAAAAAAAAA"}},
		asSession: userSession(), // userID="user1" != "other1" => 404
	})
}

func TestTokenRevokeForeignToken(t *testing.T) {
	store := newFakeStore()
	store.getTokenOwner = func(ctx context.Context, id string) (string, error) {
		return "other-user", nil // not the session user
	}
	var revokeCalled bool
	store.revokeToken = func(ctx context.Context, id string) error {
		revokeCalled = true
		return nil
	}
	h := newTestHandler(store)

	req := csrfPost(t, "/settings/tokens/revoke", url.Values{"id": {"tok1AAAAAAAAAAAAAAAAAAA"}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign revoke: status %d, want 404", rec.Code)
	}
	if revokeCalled {
		t.Fatal("foreign revoke: RevokeToken should not have been called")
	}
}

func TestTokenRevokeHappy(t *testing.T) {
	logger, sink := newTestLogger()
	store := newFakeStore()
	store.getTokenOwner = func(ctx context.Context, id string) (string, error) {
		return "user1", nil // session.UserID matches
	}
	var revokedID string
	store.revokeToken = func(ctx context.Context, id string) error {
		revokedID = id
		return nil
	}
	h := NewHandler(Deps{Store: store, Logger: logger})

	req := csrfPost(t, "/settings/tokens/revoke", url.Values{"id": {"tok1AAAAAAAAAAAAAAAAAAA"}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("happy revoke: status %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/settings/tokens" {
		t.Fatalf("happy revoke: Location %q, want /settings/tokens", loc)
	}
	if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
		t.Fatal("happy revoke: no flash cookie")
	}
	if revokedID != "tok1AAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("happy revoke: RevokeToken called with %q, want %q", revokedID, "tok1AAAAAAAAAAAAAAAAAAA")
	}
	if !sink.Has("auth.token.revoked", map[string]string{
		"token_id": "tok1AAAAAAAAAAAAAAAAAAA",
		"actor":    "user",
		"source":   "web",
	}) {
		t.Fatal("happy revoke: audit event auth.token.revoked not logged")
	}
}

// --- POST /settings/tokens/rotate ---

func TestTokenRotateForeignToken(t *testing.T) {
	store := newFakeStore()
	store.getTokenOwner = func(ctx context.Context, id string) (string, error) {
		return "other-user", nil
	}
	var rotateCalled bool
	store.rotateToken = func(ctx context.Context, id, newHash string) error {
		rotateCalled = true
		return nil
	}
	h := newTestHandler(store)

	req := csrfPost(t, "/settings/tokens/rotate", url.Values{"id": {"tok1AAAAAAAAAAAAAAAAAAA"}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign rotate: status %d, want 404", rec.Code)
	}
	if rotateCalled {
		t.Fatal("foreign rotate: RotateToken should not have been called")
	}
}

func TestTokenRotateHappy(t *testing.T) {
	logger, sink := newTestLogger()
	store := newFakeStore()
	const existingID = "tok1AAAAAAAAAAAAAAAAAAA"
	store.getTokenOwner = func(ctx context.Context, id string) (string, error) {
		return "user1", nil
	}
	var rotatedID, rotatedHash string
	store.rotateToken = func(ctx context.Context, id, newHash string) error {
		rotatedID = id
		rotatedHash = newHash
		return nil
	}
	h := NewHandler(Deps{Store: store, Logger: logger})

	req := csrfPost(t, "/settings/tokens/rotate", url.Values{"id": {existingID}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("happy rotate: status %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// body must contain the assembled token starting with bvts_<existingID>_
	prefix := "bvts_" + existingID + "_"
	if !strings.Contains(body, prefix) {
		t.Fatalf("happy rotate: assembled token prefix %q not found in body:\n%s", prefix, body)
	}

	// RotateToken was called with the right ID
	if rotatedID != existingID {
		t.Fatalf("happy rotate: RotateToken called with %q, want %q", rotatedID, existingID)
	}
	if rotatedHash == "" {
		t.Fatal("happy rotate: RotateToken called with empty hash")
	}

	// audit
	if !sink.Has("auth.token.rotated", map[string]string{
		"token_id": existingID,
		"actor":    "user",
		"source":   "web",
	}) {
		t.Fatal("happy rotate: audit event auth.token.rotated not logged")
	}

	// plaintext not in cookies
	for _, c := range rec.Result().Cookies() {
		if strings.Contains(c.Value, "bvts_") {
			t.Fatalf("happy rotate: plaintext token leaked into Set-Cookie %s=%s", c.Name, c.Value)
		}
	}

	// secret page must not be cached
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store, private" {
		t.Fatalf("happy rotate: Cache-Control %q, want \"no-store, private\"", cc)
	}
}

func TestTokenRotateRevoked(t *testing.T) {
	// Simulates rotating a token whose revoked_at IS NOT NULL in the DB:
	// GetTokenOwner succeeds (row exists), but RotateToken returns
	// ErrNoSuchToken (sqlitestore's guard on revoked_at IS NULL).
	store := newFakeStore()
	store.getTokenOwner = func(ctx context.Context, id string) (string, error) {
		return "user1", nil // owned by the session user
	}
	store.rotateToken = func(ctx context.Context, id, newHash string) error {
		return auth.ErrNoSuchToken // token is revoked; cannot rotate
	}
	h := newTestHandler(store)

	req := csrfPost(t, "/settings/tokens/rotate", url.Values{"id": {"tok1AAAAAAAAAAAAAAAAAAA"}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("rotate revoked: status %d, want 404; body:\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "bvts_") {
		t.Fatalf("rotate revoked: plaintext token must not appear in 404 body:\n%s", rec.Body.String())
	}
}
