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

// TestAdminSessions_ListsAllAndRevoke verifies:
//   - GET /admin/sessions (admin) → 200 containing the session's UserName and Provider
//   - POST /admin/sessions/revoke with id_hash → 303 and DeleteSessionByHash was called
func TestAdminSessions_ListsAllAndRevoke(t *testing.T) {
	store := adminStore()
	store.allSessions = []auth.AdminSessionInfo{
		{
			SessionInfo: auth.SessionInfo{
				IDHash:    "h1",
				Provider:  "password",
				CreatedAt: time.Now().Add(-time.Hour).Unix(),
				ExpiresAt: time.Now().Add(time.Hour).Unix(),
				LastSeen:  time.Now().Add(-time.Minute).Unix(),
				IsCurrent: false,
			},
			UserID:   "u1",
			UserName: "alice",
		},
	}

	logger, sink := newTestLogger()
	h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })

	t.Run("GET /admin/sessions lists sessions", func(t *testing.T) {
		req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/sessions", nil), store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "alice") {
			t.Errorf("body missing 'alice'; body:\n%s", body)
		}
		if !strings.Contains(body, "password") {
			t.Errorf("body missing provider 'password'; body:\n%s", body)
		}
	})

	t.Run("POST /admin/sessions/revoke revokes by id_hash", func(t *testing.T) {
		req := csrfPost(t, "/admin/sessions/revoke", url.Values{"id_hash": {"h1"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
		}
		if store.lastRevokeHash != "h1" {
			t.Fatalf("DeleteSessionByHash called with %q, want %q", store.lastRevokeHash, "h1")
		}
		if !sink.Has("auth.session.admin_revoked", map[string]string{"id_hash": "h1"}) {
			t.Fatalf("missing auth.session.admin_revoked audit event; events: %v", sink)
		}
	})
}

// TestAdminSessions_NonAdmin404 verifies that a non-admin user sees 404 on
// GET /admin/sessions (requireAdmin hides the endpoint).
func TestAdminSessions_NonAdmin404(t *testing.T) {
	store := adminStore()
	h := newTestHandlerWith(store, nil)
	req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/sessions", nil), store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}
