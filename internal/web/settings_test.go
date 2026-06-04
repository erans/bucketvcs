package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// TestSettingsPageRequiresLogin: anonymous GET /settings → 303 to /login?next=%2Fsettings
func TestSettingsPageRequiresLogin(t *testing.T) {
	store := newFakeStore()
	h := newTestHandler(store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("anon GET /settings: status %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login?next=%2Fsettings" {
		t.Fatalf("anon GET /settings: Location %q, want /login?next=%%2Fsettings", loc)
	}
}

// TestSettingsPageRenders: logged-in → 200; body contains user name; csrf token;
// with HasPassword=true contains password form; with false contains "password login not configured".
func TestSettingsPageRenders(t *testing.T) {
	// with password
	store := newFakeStore()
	store.getUserByName = func(ctx context.Context, name string) (*auth.User, error) {
		return &auth.User{ID: "u1", Name: "alice"}, nil
	}
	store.hasPassword = func(ctx context.Context, name string) (bool, error) {
		return true, nil
	}
	h := newTestHandler(store)
	sess := &auth.Session{UserID: "u1", Name: "alice"}
	sess.ExpiresAt = userSession().ExpiresAt

	req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/settings", nil), store, sess)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings: status %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "alice") {
		t.Fatalf("settings page: user name missing:\n%s", body)
	}
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Fatalf("settings page: csrf_token missing:\n%s", body)
	}
	if !strings.Contains(body, `action="/settings/password"`) {
		t.Fatalf("settings page: password form missing (HasPassword=true):\n%s", body)
	}

	// without password
	store2 := newFakeStore()
	store2.getUserByName = func(ctx context.Context, name string) (*auth.User, error) {
		return &auth.User{ID: "u1", Name: "alice"}, nil
	}
	store2.hasPassword = func(ctx context.Context, name string) (bool, error) {
		return false, nil
	}
	h2 := newTestHandler(store2)
	sess2 := &auth.Session{UserID: "u1", Name: "alice"}
	sess2.ExpiresAt = userSession().ExpiresAt

	req2 := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/settings", nil), store2, sess2)
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /settings (no pw): status %d, want 200", rec2.Code)
	}
	body2 := rec2.Body.String()
	if !strings.Contains(body2, "password login not configured") {
		t.Fatalf("settings page: 'password login not configured' text missing:\n%s", body2)
	}
}

// TestSettingsPageShowsAdminBadge: admin session → body contains "[admin]".
func TestSettingsPageShowsAdminBadge(t *testing.T) {
	store := newFakeStore()
	store.getUserByName = func(ctx context.Context, name string) (*auth.User, error) {
		return &auth.User{ID: "admin1", Name: "admin", IsAdmin: true}, nil
	}
	store.hasPassword = func(ctx context.Context, name string) (bool, error) {
		return true, nil
	}
	h := newTestHandler(store)

	req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/settings", nil), store, adminSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /settings: status %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "[admin]") {
		t.Fatalf("admin settings page: [admin] badge missing:\n%s", rec.Body.String())
	}
}

// TestPasswordChange covers the form-security matrix and all business-logic paths.
func TestPasswordChange(t *testing.T) {
	baseForm := url.Values{
		"current": {"oldpass"},
		"new1":    {"newpass123"},
		"new2":    {"newpass123"},
	}

	t.Run("formSecurity", func(t *testing.T) {
		store := newFakeStore()
		h := newTestHandler(store)
		// no asSession: any logged-in user may POST; the form-security kit's step 3
		// (authorized → 404) does not apply here.
		assertFormSecurity(t, h, secOpts{
			store: store,
			path:  "/settings/password",
			form:  cloneValues(baseForm),
		})
	})

	t.Run("mismatch", func(t *testing.T) {
		store := newFakeStore()
		var setPWCalled bool
		store.setPassword = func(ctx context.Context, u, p string) error {
			setPWCalled = true
			return nil
		}
		h := newTestHandler(store)

		form := cloneValues(baseForm)
		form.Set("new2", "different")
		req := csrfPost(t, "/settings/password", form)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("mismatch: status %d, want 303", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/settings" {
			t.Fatalf("mismatch: Location %q, want /settings", loc)
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("mismatch: no flash cookie set")
		}
		if setPWCalled {
			t.Fatal("mismatch: SetPassword should not have been called")
		}
	})

	t.Run("tooShort", func(t *testing.T) {
		store := newFakeStore()
		var setPWCalled bool
		store.setPassword = func(ctx context.Context, u, p string) error {
			setPWCalled = true
			return nil
		}
		h := newTestHandler(store)

		form := cloneValues(baseForm)
		form.Set("new1", "short")
		form.Set("new2", "short")
		req := csrfPost(t, "/settings/password", form)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("tooShort: status %d, want 303", rec.Code)
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("tooShort: no flash cookie set")
		}
		if setPWCalled {
			t.Fatal("tooShort: SetPassword should not have been called")
		}
	})

	t.Run("wrongCurrent", func(t *testing.T) {
		store := newFakeStore()
		store.verify = func(ctx context.Context, u, p string) (*auth.Actor, error) {
			return nil, auth.ErrInvalidCredential
		}
		var setPWCalled bool
		store.setPassword = func(ctx context.Context, u, p string) error {
			setPWCalled = true
			return nil
		}
		h := newTestHandler(store)

		req := csrfPost(t, "/settings/password", cloneValues(baseForm))
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("wrongCurrent: status %d, want 303", rec.Code)
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("wrongCurrent: no flash cookie set")
		}
		if setPWCalled {
			t.Fatal("wrongCurrent: SetPassword should not have been called")
		}
	})

	t.Run("happy", func(t *testing.T) {
		logger, sink := newTestLogger()
		store := newFakeStore()
		store.verify = func(ctx context.Context, u, p string) (*auth.Actor, error) {
			return &auth.Actor{UserID: "user1", Name: u}, nil
		}
		var setPWUser, setPWPlain string
		store.setPassword = func(ctx context.Context, u, p string) error {
			setPWUser = u
			setPWPlain = p
			return nil
		}
		var revokedUser, revokedExcept string
		var revokeCalled bool
		store.deleteSessionsFor = func(ctx context.Context, userID, exceptRawID string) (int64, error) {
			revokeCalled = true
			revokedUser = userID
			revokedExcept = exceptRawID
			return 2, nil
		}
		h := NewHandler(Deps{Store: store, Logger: logger})

		req := csrfPost(t, "/settings/password", cloneValues(baseForm))
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("happy: status %d, want 303; body:\n%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/settings" {
			t.Fatalf("happy: Location %q, want /settings", loc)
		}
		if setPWUser != "user" {
			t.Fatalf("happy: SetPassword called with user %q, want %q", setPWUser, "user")
		}
		if setPWPlain != "newpass123" {
			t.Fatalf("happy: SetPassword called with wrong plaintext %q", setPWPlain)
		}
		if !revokeCalled {
			t.Fatal("happy: DeleteSessionsForUser was not called")
		}
		if revokedUser != "user1" {
			t.Fatalf("happy: DeleteSessionsForUser userID %q, want %q", revokedUser, "user1")
		}
		// The current session's raw cookie value must be the exclusion so the
		// user is not logged out of the session that just changed the password.
		if revokedExcept != "test-sess-user1" {
			t.Fatalf("happy: DeleteSessionsForUser exceptRawID %q, want current cookie", revokedExcept)
		}
		if fc := findCookie(rec.Result().Cookies(), flashCookieName); fc == nil {
			t.Fatal("happy: no flash cookie set")
		} else if got := decodeFlash(fc); got != "password changed; 2 other session(s) signed out" {
			// Count-driven message (roborev round 13): revocation is only
			// mentioned when other sessions were actually deleted.
			t.Fatalf("happy: flash %q, want count-based revocation message", got)
		}
		if !sink.Has("auth.user.password_changed", map[string]string{
			"actor":  "user",
			"source": "web",
			"user":   "user",
		}) {
			t.Fatal("happy: audit event auth.user.password_changed not logged with expected attrs")
		}
	})

	t.Run("zeroRevokedPlainFlash", func(t *testing.T) {
		// Common single-session case: nothing else to revoke, so the flash
		// must not claim "other sessions signed out" (roborev round 13).
		store := newFakeStore()
		store.verify = func(ctx context.Context, u, p string) (*auth.Actor, error) {
			return &auth.Actor{UserID: "user1", Name: u}, nil
		}
		store.setPassword = func(ctx context.Context, u, p string) error { return nil }
		store.deleteSessionsFor = func(ctx context.Context, userID, exceptRawID string) (int64, error) {
			return 0, nil
		}
		h := NewHandler(Deps{Store: store, Logger: slog.Default()})
		req := csrfPost(t, "/settings/password", cloneValues(baseForm))
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
		fc := findCookie(rec.Result().Cookies(), flashCookieName)
		if fc == nil {
			t.Fatal("no flash cookie set")
		}
		if got := decodeFlash(fc); got != "password changed" {
			t.Fatalf("flash %q, want plain %q", got, "password changed")
		}
	})

	t.Run("revokeCleanupErrorStill303", func(t *testing.T) {
		store := newFakeStore()
		store.verify = func(ctx context.Context, u, p string) (*auth.Actor, error) {
			return &auth.Actor{UserID: "user1", Name: u}, nil
		}
		var setPWCalled bool
		store.setPassword = func(ctx context.Context, u, p string) error {
			setPWCalled = true
			return nil
		}
		store.deleteSessionsFor = func(ctx context.Context, userID, exceptRawID string) (int64, error) {
			return 0, errors.New("boom")
		}
		h := newTestHandler(store)

		req := csrfPost(t, "/settings/password", cloneValues(baseForm))
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// Cleanup failure is best-effort: the password change still succeeds.
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("revokeCleanupError: status %d, want 303; body:\n%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/settings" {
			t.Fatalf("revokeCleanupError: Location %q, want /settings", loc)
		}
		if !setPWCalled {
			t.Fatal("revokeCleanupError: SetPassword should have been called")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("revokeCleanupError: no flash cookie set")
		}
	})

	t.Run("noCookieFlashNoRevocation", func(t *testing.T) {
		// Drive the handler with a context-injected session but NO session cookie
		// (the unreachable-in-prod shape: session was injected by middleware but
		// the cookie value was not readable via r.Cookie). In this path
		// DeleteSessionsForUser must NOT be called and the flash must be just
		// "password changed" (not "...other sessions signed out").
		store := newFakeStore()
		store.verify = func(ctx context.Context, u, p string) (*auth.Actor, error) {
			return &auth.Actor{UserID: "user1", Name: u}, nil
		}
		store.setPassword = func(ctx context.Context, u, p string) error { return nil }
		var revokeCalled bool
		store.deleteSessionsFor = func(ctx context.Context, userID, exceptRawID string) (int64, error) {
			revokeCalled = true
			return 0, nil
		}
		s := newTestServerStruct(store)

		// Build POST request with CSRF but WITHOUT the session cookie; inject
		// the session directly into the request context via withTestSession.
		form := cloneValues(baseForm)
		form.Set(csrfFormField, "test-csrf-token")
		req := httptest.NewRequest(http.MethodPost, "/settings/password",
			strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "test-csrf-token"})
		req = withTestSession(req, userSession())

		rec := httptest.NewRecorder()
		s.handlePasswordChange(rec, req)

		if rec.Code != http.StatusSeeOther {
			t.Fatalf("noCookie: status %d, want 303; body:\n%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/settings" {
			t.Fatalf("noCookie: Location %q, want /settings", loc)
		}
		if revokeCalled {
			t.Fatal("noCookie: DeleteSessionsForUser must not be called without a cookie")
		}
		flashCookie := findCookie(rec.Result().Cookies(), flashCookieName)
		if flashCookie == nil {
			t.Fatal("noCookie: no flash cookie set")
		}
		// The flash value is URL-encoded; just check for the "signed out" absence.
		if strings.Contains(flashCookie.Value, "signed+out") ||
			strings.Contains(flashCookie.Value, "signed%20out") ||
			strings.Contains(flashCookie.Value, "signed out") {
			t.Fatalf("noCookie: flash must not mention session revocation, got %q", flashCookie.Value)
		}
	})
}
