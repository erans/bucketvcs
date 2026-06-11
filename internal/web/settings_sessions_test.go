package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
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
	if store.lastRevokeAllUserID != "user1" {
		t.Fatalf("revoke-all: recorded userID %q, want %q (must be user-scoped)", store.lastRevokeAllUserID, "user1")
	}
	if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
		t.Fatal("revoke-all: no flash cookie set")
	}
}

// TestSessionsRevoke_CSRFRejected: an authenticated POST to
// /settings/sessions/revoke WITHOUT a valid CSRF token is rejected by postGuard
// (403) and never reaches the store — the revoke method must not be called.
func TestSessionsRevoke_CSRFRejected(t *testing.T) {
	store := newFakeStore()
	h := newTestHandler(store)

	// Plain authed POST: session cookie but no CSRF cookie/token.
	form := url.Values{"id_hash": {"hashOTHER"}}
	req := httptest.NewRequest(http.MethodPost, "/settings/sessions/revoke",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: status %d, want 403; body:\n%s", rec.Code, rec.Body.String())
	}
	if store.lastRevokeHash != "" {
		t.Fatalf("CSRF-rejected POST still reached the store (lastRevokeHash=%q); revoke must not be called", store.lastRevokeHash)
	}
}

// TestSessionsRevoke_CannotRevokeCurrent: posting the id hash of the user's OWN
// current session is refused by the self-revoke guard — the handler 303s with a
// "cannot revoke your current session" flash and DeleteSessionByHashForUser is
// never called. The stored id is SHA-256(rawCookieValue); addSessionCookie uses
// "test-sess-" + UserID as the raw value.
func TestSessionsRevoke_CannotRevokeCurrent(t *testing.T) {
	store := newFakeStore()
	h := newTestHandler(store)

	rawCookie := "test-sess-" + userSession().UserID
	sum := sha256.Sum256([]byte(rawCookie))
	currentHash := hex.EncodeToString(sum[:])

	req := csrfPost(t, "/settings/sessions/revoke", url.Values{"id_hash": {currentHash}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("self-revoke: status %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	// The self-revoke guard must fire BEFORE the store is touched.
	if store.lastRevokeHash != "" {
		t.Fatalf("self-revoke guard did not fire: DeleteSessionByHashForUser was called with %q", store.lastRevokeHash)
	}
	// The flash carries the guard's message (base64url-encoded in the cookie).
	fc := findCookie(rec.Result().Cookies(), flashCookieName)
	if fc == nil {
		t.Fatal("self-revoke: no flash cookie set")
	}
	dec, err := base64.RawURLEncoding.DecodeString(fc.Value)
	if err != nil || !strings.Contains(string(dec), "cannot revoke your current session") {
		t.Fatalf("self-revoke: flash %q does not contain the current-session guard message", fc.Value)
	}
}

// TestSessionsRevoke_NoAuditEventWhenAlreadyGone: a revoke that deletes zero
// rows (session expired or already revoked) must NOT record an
// auth.session.revoked audit event — nothing was revoked. Mirrors the admin
// handler's n==0 behavior.
func TestSessionsRevoke_NoAuditEventWhenAlreadyGone(t *testing.T) {
	store := newFakeStore()
	store.revokeCount = -1 // DeleteSessionByHashForUser reports 0 rows
	var buf bytes.Buffer
	h := newTestHandlerWith(store, func(d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	})

	req := csrfPost(t, "/settings/sessions/revoke", url.Values{"id_hash": {"hashGONE"}})
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(buf.String(), "auth.session.revoked") {
		t.Fatalf("auth.session.revoked emitted for a no-op revoke (0 rows deleted); log:\n%s", buf.String())
	}
}

// TestSessionsRevokeAll_NoAuditEventWhenNoOthers: revoke-all with no other
// sessions deletes nothing and must not record auth.session.revoked_all —
// matching the no-op rule of the sibling revoke handlers.
func TestSessionsRevokeAll_NoAuditEventWhenNoOthers(t *testing.T) {
	store := newFakeStore()
	var buf bytes.Buffer
	h := newTestHandlerWith(store, func(d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	})

	req := csrfPost(t, "/settings/sessions/revoke-all", nil)
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(buf.String(), "auth.session.revoked_all") {
		t.Fatalf("auth.session.revoked_all emitted for a no-op revoke-all; log:\n%s", buf.String())
	}
}
