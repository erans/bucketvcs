package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// TestSessionsPage_ListsAndMarksCurrent: GET /settings/sessions lists every
// session, marks the current one, and renders the non-current id hash (so it can
// be revoked).
func TestSessionsPage_ListsAndMarksCurrent(t *testing.T) {
	store := newFakeStore()
	now := time.Now().Unix()
	store.sessionsForUser = []auth.SessionInfo{
		{IDHash: "hashCURRENT", Provider: "password", CreatedAt: now - 3600, LastSeen: now - 60, ExpiresAt: now + 3600, IsCurrent: true},
		{IDHash: "hashOTHER", Provider: "oidc", CreatedAt: now - 7200, LastSeen: now - 120, ExpiresAt: now + 3600, IsCurrent: false},
	}
	h := newTestHandler(store)

	req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/settings/sessions", nil), store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/sessions: status %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "password") {
		t.Fatalf("sessions page: provider 'password' missing:\n%s", body)
	}
	if !strings.Contains(body, "oidc") {
		t.Fatalf("sessions page: provider 'oidc' missing:\n%s", body)
	}
	if !strings.Contains(body, "current") {
		t.Fatalf("sessions page: 'current' marker missing:\n%s", body)
	}
	// the non-current id hash must be present (it is the revoke form's target)
	if !strings.Contains(body, "hashOTHER") {
		t.Fatalf("sessions page: non-current id hash 'hashOTHER' missing:\n%s", body)
	}
}

// TestSessionsRevoke_UserScoped: POST /settings/sessions/revoke with id_hash
// records a user-scoped revoke (logged-in user's id + the supplied hash).
func TestSessionsRevoke_UserScoped(t *testing.T) {
	store := newFakeStore()
	h := newTestHandler(store)

	req := csrfPost(t, "/settings/sessions/revoke", url.Values{"id_hash": {"hashOTHER"}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke: status %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/settings/sessions" {
		t.Fatalf("revoke: Location %q, want /settings/sessions", loc)
	}
	if store.lastRevokeUserID != "user1" {
		t.Fatalf("revoke: recorded userID %q, want %q (must be user-scoped)", store.lastRevokeUserID, "user1")
	}
	if store.lastRevokeHash != "hashOTHER" {
		t.Fatalf("revoke: recorded hash %q, want %q", store.lastRevokeHash, "hashOTHER")
	}
	if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
		t.Fatal("revoke: no flash cookie set")
	}
}

// TestSessionsRevokeAll_Others: POST /settings/sessions/revoke-all signs out the
// user's OTHER sessions (keeps current) and flashes the count.
func TestSessionsRevokeAll_Others(t *testing.T) {
	store := newFakeStore()
	h := newTestHandler(store)

	req := csrfPost(t, "/settings/sessions/revoke-all", nil)
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke-all: status %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/settings/sessions" {
		t.Fatalf("revoke-all: Location %q, want /settings/sessions", loc)
	}
	if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
		t.Fatal("revoke-all: no flash cookie set")
	}
}
