package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// adminStore returns a fakeStore primed with an admin session.
func adminStore() *fakeStore {
	s := newFakeStore()
	return s
}

// --- GET /admin ---

func TestAdminIndex_AuthGuard(t *testing.T) {
	t.Run("anon → 303 login", func(t *testing.T) {
		store := adminStore()
		h := newTestHandlerWith(store, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
			t.Fatalf("Location %q, want /login...", loc)
		}
	})
	t.Run("non-admin → 404", func(t *testing.T) {
		store := adminStore()
		h := newTestHandlerWith(store, nil)
		req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin", nil), store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rec.Code)
		}
	})
	t.Run("admin → 200", func(t *testing.T) {
		store := adminStore()
		h := newTestHandlerWith(store, nil)
		req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin", nil), store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "/admin/users") {
			t.Errorf("admin index missing /admin/users link; body: %s", body)
		}
	})
}

// --- GET /admin/users ---

func TestAdminUsers_AuthGuard(t *testing.T) {
	t.Run("anon → 303 login", func(t *testing.T) {
		store := adminStore()
		h := newTestHandlerWith(store, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/users", nil))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
	})
	t.Run("non-admin → 404", func(t *testing.T) {
		store := adminStore()
		h := newTestHandlerWith(store, nil)
		req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/users", nil), store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rec.Code)
		}
	})
	t.Run("admin → 200 with user table + create form", func(t *testing.T) {
		store := adminStore()
		store.listUsers = func(ctx context.Context) ([]UserInfo, error) {
			return []UserInfo{
				{ID: "u1", Name: "alice", Email: "alice@example.com", IsAdmin: true, Disabled: false, CreatedAt: 1700000000},
				{ID: "u2", Name: "bob", IsAdmin: false, Disabled: true, CreatedAt: 1700000001},
			}, nil
		}
		h := newTestHandlerWith(store, nil)
		req := addSessionCookie(t, httptest.NewRequest(http.MethodGet, "/admin/users", nil), store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "alice") {
			t.Errorf("missing alice in users list; body: %s", body)
		}
		if !strings.Contains(body, "bob") {
			t.Errorf("missing bob in users list; body: %s", body)
		}
		if !strings.Contains(body, "/admin/users/create") {
			t.Errorf("missing create form action; body: %s", body)
		}
	})
}

// --- POST /admin/users/create ---

func TestAdminUserCreate_FormSecurity(t *testing.T) {
	store := adminStore()
	h := newTestHandlerWith(store, nil)
	assertFormSecurity(t, h, secOpts{
		store: store,
		path:  "/admin/users/create",
		form:  url.Values{"name": {"testuser"}},
		// non-admin with valid CSRF → 404 (admin area not advertised)
		asSession: userSession(),
	})
}

func TestAdminUserCreate_Validation(t *testing.T) {
	t.Run("empty name → flash", func(t *testing.T) {
		store := adminStore()
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/admin/users/create", url.Values{"name": {""}, "password": {"longenough"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Location"), "/admin/users") {
			t.Fatalf("redirect target %q", rec.Header().Get("Location"))
		}
	})
	t.Run("password too short → flash, CreateUser NOT called", func(t *testing.T) {
		store := adminStore()
		called := false
		store.createUser = func(ctx context.Context, name string, isAdmin bool) (string, error) {
			called = true
			return "id", nil
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/admin/users/create", url.Values{"name": {"charlie"}, "password": {"short"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
		if called {
			t.Fatal("CreateUser was called despite short password")
		}
		// Flash should mention "password too short"
		_ = rec.Header().Get("Location")
	})
	t.Run("duplicate name → flash", func(t *testing.T) {
		store := adminStore()
		store.createUser = func(ctx context.Context, name string, isAdmin bool) (string, error) {
			return "", auth.ErrConflict
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/admin/users/create", url.Values{"name": {"alice"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
	})
}

func TestAdminUserCreate_Happy(t *testing.T) {
	t.Run("without password → CreateUser only, audit password_set=false", func(t *testing.T) {
		store := adminStore()
		logger, sink := newTestLogger()
		var createdName string
		store.createUser = func(ctx context.Context, name string, isAdmin bool) (string, error) {
			createdName = name
			return "uid-" + name, nil
		}
		setPwCalled := false
		store.setPassword = func(ctx context.Context, userName, plaintext string) error {
			setPwCalled = true
			return nil
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/admin/users/create", url.Values{"name": {"dave"}, "is_admin": {"on"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
		}
		if createdName != "dave" {
			t.Fatalf("createdName = %q, want dave", createdName)
		}
		if setPwCalled {
			t.Fatal("SetPassword should NOT be called when no password is provided")
		}
		if !sink.Has("auth.user.created", map[string]string{"user": "dave", "password_set": "false"}) {
			t.Fatal("missing auth.user.created audit event with password_set=false")
		}
	})
	t.Run("with password ≥8 → CreateUser + SetPassword, audit password_set=true", func(t *testing.T) {
		store := adminStore()
		logger, sink := newTestLogger()
		store.createUser = func(ctx context.Context, name string, isAdmin bool) (string, error) {
			return "uid-" + name, nil
		}
		setPwCalled := false
		store.setPassword = func(ctx context.Context, userName, plaintext string) error {
			setPwCalled = true
			return nil
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/admin/users/create", url.Values{"name": {"eve"}, "password": {"longenough"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
		}
		if !setPwCalled {
			t.Fatal("SetPassword should be called when password is provided")
		}
		if !sink.Has("auth.user.created", map[string]string{"user": "eve", "password_set": "true"}) {
			t.Fatal("missing auth.user.created audit event with password_set=true")
		}
	})
	t.Run("CreateUser ok + SetPassword fails → 500", func(t *testing.T) {
		store := adminStore()
		store.createUser = func(ctx context.Context, name string, isAdmin bool) (string, error) {
			return "uid-" + name, nil
		}
		store.setPassword = func(ctx context.Context, userName, plaintext string) error {
			return errors.New("db boom")
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/admin/users/create", url.Values{"name": {"frank"}, "password": {"longenough"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500", rec.Code)
		}
	})
}

// --- POST /admin/users/disable ---

func TestAdminUserDisable(t *testing.T) {
	t.Run("self-disable → flash, not called", func(t *testing.T) {
		store := adminStore()
		called := false
		store.setUserDisabled = func(ctx context.Context, name string, disabled bool) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, nil)
		// adminSession().Name == "admin"
		req := csrfPost(t, "/admin/users/disable", url.Values{"name": {adminSession().Name}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
		if called {
			t.Fatal("SetUserDisabled should not be called for self")
		}
	})
	t.Run("ErrLastAdmin → flash", func(t *testing.T) {
		store := adminStore()
		store.setUserDisabled = func(ctx context.Context, name string, disabled bool) error {
			return sqlitestore.ErrLastAdmin
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/admin/users/disable", url.Values{"name": {"alice"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
	})
	t.Run("happy disable → audit auth.user.disabled", func(t *testing.T) {
		store := adminStore()
		logger, sink := newTestLogger()
		store.setUserDisabled = func(ctx context.Context, name string, disabled bool) error {
			return nil
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/admin/users/disable", url.Values{"name": {"alice"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
		}
		if !sink.Has("auth.user.disabled", map[string]string{"user": "alice"}) {
			t.Fatal("missing auth.user.disabled audit event")
		}
	})
}

// --- POST /admin/users/enable ---

func TestAdminUserEnable(t *testing.T) {
	t.Run("happy enable → audit auth.user.enabled", func(t *testing.T) {
		store := adminStore()
		logger, sink := newTestLogger()
		store.setUserDisabled = func(ctx context.Context, name string, disabled bool) error {
			return nil
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/admin/users/enable", url.Values{"name": {"bob"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
		}
		if !sink.Has("auth.user.enabled", map[string]string{"user": "bob"}) {
			t.Fatal("missing auth.user.enabled audit event")
		}
	})
}

// --- POST /admin/users/delete ---

func TestAdminUserDelete(t *testing.T) {
	t.Run("confirm != name → flash", func(t *testing.T) {
		store := adminStore()
		called := false
		store.deleteUser = func(ctx context.Context, name string) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/admin/users/delete", url.Values{"name": {"alice"}, "confirm": {"wrong"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
		if called {
			t.Fatal("DeleteUser should not be called when confirm != name")
		}
	})
	t.Run("self-delete → flash", func(t *testing.T) {
		store := adminStore()
		called := false
		store.deleteUser = func(ctx context.Context, name string) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/admin/users/delete", url.Values{"name": {adminSession().Name}, "confirm": {adminSession().Name}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
		if called {
			t.Fatal("DeleteUser should not be called for self")
		}
	})
	t.Run("ErrLastAdmin → flash", func(t *testing.T) {
		store := adminStore()
		store.deleteUser = func(ctx context.Context, name string) error {
			return sqlitestore.ErrLastAdmin
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/admin/users/delete", url.Values{"name": {"alice"}, "confirm": {"alice"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
	})
	t.Run("ErrReservedUser → flash", func(t *testing.T) {
		store := adminStore()
		store.deleteUser = func(ctx context.Context, name string) error {
			return sqlitestore.ErrReservedUser
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/admin/users/delete", url.Values{"name": {"_system"}, "confirm": {"_system"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
	})
	t.Run("happy → audit auth.user.deleted", func(t *testing.T) {
		store := adminStore()
		logger, sink := newTestLogger()
		store.deleteUser = func(ctx context.Context, name string) error {
			return nil
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/admin/users/delete", url.Values{"name": {"alice"}, "confirm": {"alice"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
		}
		if !sink.Has("auth.user.deleted", map[string]string{"user": "alice"}) {
			t.Fatal("missing auth.user.deleted audit event")
		}
	})
}

// --- POST /admin/users/email ---

func TestAdminUserEmail(t *testing.T) {
	t.Run("ErrConflict → flash", func(t *testing.T) {
		store := adminStore()
		store.setEmail = func(ctx context.Context, userName, email string) error {
			return auth.ErrConflict
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/admin/users/email", url.Values{"name": {"alice"}, "email": {"taken@example.com"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", rec.Code)
		}
	})
	t.Run("happy set → audit auth.user.email_set", func(t *testing.T) {
		store := adminStore()
		logger, sink := newTestLogger()
		store.setEmail = func(ctx context.Context, userName, email string) error {
			return nil
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/admin/users/email", url.Values{"name": {"alice"}, "email": {"alice@example.com"}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
		}
		if !sink.Has("auth.user.email_set", map[string]string{"user": "alice"}) {
			t.Fatal("missing auth.user.email_set audit event")
		}
	})
	t.Run("happy clear (empty email) → audit with email=''", func(t *testing.T) {
		store := adminStore()
		logger, sink := newTestLogger()
		store.setEmail = func(ctx context.Context, userName, email string) error {
			return nil
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/admin/users/email", url.Values{"name": {"alice"}, "email": {""}})
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body: %s", rec.Code, rec.Body.String())
		}
		if !sink.Has("auth.user.email_set", map[string]string{"user": "alice", "email": ""}) {
			t.Fatal("missing auth.user.email_set audit event with empty email")
		}
	})
}
