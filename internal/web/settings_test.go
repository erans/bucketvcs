package web

import (
	"context"
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
		if !sink.Has("auth.user.password_changed", map[string]string{
			"actor":  "user",
			"source": "web",
			"user":   "user",
		}) {
			t.Fatal("happy: audit event auth.user.password_changed not logged with expected attrs")
		}
	})
}
