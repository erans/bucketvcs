package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// testDeployPubLine is a valid authorized_keys line distinct from the user-key
// fixtures, used for deploy-key add tests.
const testDeployPubLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFpotneIfuGp8t6tsn1sFS3ehwRteumxH4JRK5ZzNSb8 deploy-test"

// accessStore returns a repo-admin fakeStore primed for the access tab, with a
// GetRepoFlags that succeeds so the chassis renders.
func accessStore() *fakeStore {
	s := newFakeStore()
	s.perm = auth.PermAdmin
	s.getRepoFlags = func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
		return auth.RepoFlags{}, nil
	}
	return s
}

func TestRepoSettingsAccessRenders(t *testing.T) {
	store := accessStore()
	store.listRepoGrants = func(ctx context.Context, tenant, repo string) ([]RepoGrant, error) {
		return []RepoGrant{
			{UserName: "alice", Perm: "admin"},
			{UserName: "bob", Perm: "write"},
		}, nil
	}
	store.listSSHKeysForRepo = func(ctx context.Context, tenant, repo string) ([]auth.SSHKey, error) {
		return []auth.SSHKey{
			{ID: "bvsk_dep1", Fingerprint: "SHA256:DEPLOYFP", KeyType: "ssh-ed25519", Label: "ci", ScopePerm: auth.PermRead},
		}, nil
	}
	h := newTestHandlerWith(store, nil)
	req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/access", nil)
	addSessionCookie(t, req, store, userSession())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"alice", "admin", "bob", "write", // grants rows
		"SHA256:DEPLOYFP", "ssh-ed25519", "ci", "read", // deploy key row (perm text)
		"/acme/demo/settings/access/grant",         // grant add form
		"/acme/demo/settings/access/deploykey/add", // deploy add form
		"csrf_token",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("access page missing %q; body=%s", want, body)
		}
	}
}

func TestRepoSettingsAccessGrant(t *testing.T) {
	t.Run("form security: reader → 404", func(t *testing.T) {
		store := newFakeStore()
		store.perm = auth.PermRead
		h := newTestHandlerWith(store, nil)
		assertFormSecurity(t, h, secOpts{
			store:     store,
			path:      "/acme/demo/settings/access/grant",
			form:      url.Values{"username": {"alice"}, "perm": {"write"}},
			asSession: userSession(),
		})
	})

	t.Run("invalid perm → flash, Grant not called", func(t *testing.T) {
		store := accessStore()
		var called bool
		store.grant = func(ctx context.Context, userName, tenant, repo, perm string) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/access/grant", url.Values{"username": {"alice"}, "perm": {"owner"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("Grant must not be called for an invalid perm")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for invalid perm")
		}
	})

	t.Run("empty username → flash, Grant not called", func(t *testing.T) {
		store := accessStore()
		var called bool
		store.grant = func(ctx context.Context, userName, tenant, repo, perm string) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/access/grant", url.Values{"username": {""}, "perm": {"write"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("Grant must not be called for an empty username")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for empty username")
		}
	})

	t.Run("no such user → flash", func(t *testing.T) {
		store := accessStore()
		store.grant = func(ctx context.Context, userName, tenant, repo, perm string) error {
			return auth.ErrNoSuchUser
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/access/grant", url.Values{"username": {"ghost"}, "perm": {"write"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303 (flash, not 500); body=%s", rec.Code, rec.Body.String())
		}
		flash := findCookie(rec.Result().Cookies(), flashCookieName)
		if flash == nil {
			t.Fatal("expected flash cookie for no-such-user")
		}
	})

	t.Run("reserved user → flash, no audit", func(t *testing.T) {
		store := accessStore()
		store.grant = func(ctx context.Context, userName, tenant, repo, perm string) error {
			return sqlitestore.ErrReservedUser
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/access/grant", url.Values{"username": {"_oidc"}, "perm": {"write"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303 (flash, not 500); body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for reserved-user")
		}
		if sink.Has("repo.grant.added", map[string]string{"tenant": "acme", "repo": "demo"}) {
			t.Fatal("repo.grant.added must NOT be emitted on a rejected grant")
		}
	})

	t.Run("happy path: Grant + audit + metric + redirect", func(t *testing.T) {
		store := accessStore()
		var gotUser, gotTenant, gotRepo, gotPerm string
		store.grant = func(ctx context.Context, userName, tenant, repo, perm string) error {
			gotUser, gotTenant, gotRepo, gotPerm = userName, tenant, repo, perm
			return nil
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/access/grant", url.Values{"username": {"alice"}, "perm": {"write"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings/access" {
			t.Fatalf("Location %q, want /acme/demo/settings/access", loc)
		}
		if gotUser != "alice" || gotTenant != "acme" || gotRepo != "demo" || gotPerm != "write" {
			t.Fatalf("Grant(%q,%q,%q,%q), want (alice,acme,demo,write)", gotUser, gotTenant, gotRepo, gotPerm)
		}
		if !sink.Has("repo.grant.added", map[string]string{"tenant": "acme", "repo": "demo", "user": "alice", "perm": "write"}) {
			t.Fatal("missing repo.grant.added audit with tenant/repo/user/perm")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on grant success")
		}
	})
}

func TestRepoSettingsAccessRevoke(t *testing.T) {
	t.Run("happy path: RevokeRepoPermission + audit + redirect", func(t *testing.T) {
		store := accessStore()
		var gotUser, gotTenant, gotRepo string
		store.revokeRepoPermission = func(ctx context.Context, userName, tenant, repo string) error {
			gotUser, gotTenant, gotRepo = userName, tenant, repo
			return nil
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/access/revoke", url.Values{"username": {"bob"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings/access" {
			t.Fatalf("Location %q, want /acme/demo/settings/access", loc)
		}
		if gotUser != "bob" || gotTenant != "acme" || gotRepo != "demo" {
			t.Fatalf("RevokeRepoPermission(%q,%q,%q), want (bob,acme,demo)", gotUser, gotTenant, gotRepo)
		}
		if !sink.Has("repo.grant.removed", map[string]string{"tenant": "acme", "repo": "demo", "user": "bob"}) {
			t.Fatal("missing repo.grant.removed audit")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on revoke success")
		}
	})

	t.Run("no such user → flash, no 500", func(t *testing.T) {
		store := accessStore()
		store.revokeRepoPermission = func(ctx context.Context, userName, tenant, repo string) error {
			return auth.ErrNoSuchUser
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/access/revoke", url.Values{"username": {"ghost"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303 (flash, not 500); body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for no-such-user revoke")
		}
	})
}

func TestRepoSettingsAccessDeployKeyAdd(t *testing.T) {
	t.Run("invalid pubkey → flash, AddSSHKey not called", func(t *testing.T) {
		store := accessStore()
		var called bool
		store.addSSHKey = func(ctx context.Context, k auth.SSHKey) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/access/deploykey/add",
			url.Values{"pubkey": {"not a key"}, "perm": {"read"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("AddSSHKey must not be called for an unparseable pubkey")
		}
		flash := findCookie(rec.Result().Cookies(), flashCookieName)
		if flash == nil {
			t.Fatal("expected flash cookie for parse failure")
		}
	})

	t.Run("perm=admin REJECTED → flash, AddSSHKey not called", func(t *testing.T) {
		store := accessStore()
		var called bool
		store.addSSHKey = func(ctx context.Context, k auth.SSHKey) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/access/deploykey/add",
			url.Values{"pubkey": {testDeployPubLine}, "perm": {"admin"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("deploy keys must never be admin: AddSSHKey must not be called")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie rejecting admin deploy key")
		}
	})

	t.Run("happy path (read): AddSSHKey with scope + audit + redirect", func(t *testing.T) {
		store := accessStore()
		var got auth.SSHKey
		store.addSSHKey = func(ctx context.Context, k auth.SSHKey) error {
			got = k
			return nil
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/access/deploykey/add",
			url.Values{"pubkey": {testDeployPubLine}, "label": {"ci"}, "perm": {"read"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings/access" {
			t.Fatalf("Location %q, want /acme/demo/settings/access", loc)
		}
		if got.ScopeTenant != "acme" || got.ScopeRepo != "demo" {
			t.Fatalf("scope (%q,%q), want (acme,demo)", got.ScopeTenant, got.ScopeRepo)
		}
		if got.ScopePerm != auth.PermRead {
			t.Fatalf("ScopePerm %v, want PermRead", got.ScopePerm)
		}
		if got.UserID != "" {
			t.Fatalf("UserID %q, want empty (deploy key)", got.UserID)
		}
		if !sink.Has("auth.sshkey.added", map[string]string{"kind": "deploy", "fingerprint": got.Fingerprint}) {
			t.Fatal("missing auth.sshkey.added audit with kind=deploy + fingerprint")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on deploy key add")
		}
	})

	t.Run("duplicate fingerprint → flash", func(t *testing.T) {
		store := accessStore()
		store.addSSHKey = func(ctx context.Context, k auth.SSHKey) error {
			return auth.ErrDuplicateFingerprint
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/access/deploykey/add",
			url.Values{"pubkey": {testDeployPubLine}, "perm": {"write"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for duplicate fingerprint")
		}
	})
}

func TestRepoSettingsAccessDeployKeyRevoke(t *testing.T) {
	t.Run("key not in this repo → 404, RevokeSSHKey not called", func(t *testing.T) {
		store := accessStore()
		store.listSSHKeysForRepo = func(ctx context.Context, tenant, repo string) ([]auth.SSHKey, error) {
			return []auth.SSHKey{{ID: "bvsk_owned"}}, nil
		}
		var called bool
		store.revokeSSHKey = func(ctx context.Context, id string) error {
			called = true
			return nil
		}
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/access/deploykey/revoke", url.Values{"id": {"bvsk_other"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("RevokeSSHKey must not be called for a key outside this repo")
		}
	})

	t.Run("owned key → revoke + audit + redirect", func(t *testing.T) {
		store := accessStore()
		store.listSSHKeysForRepo = func(ctx context.Context, tenant, repo string) ([]auth.SSHKey, error) {
			return []auth.SSHKey{{ID: "bvsk_owned", Fingerprint: "SHA256:OWNED"}}, nil
		}
		var gotID string
		store.revokeSSHKey = func(ctx context.Context, id string) error {
			gotID = id
			return nil
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/access/deploykey/revoke", url.Values{"id": {"bvsk_owned"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings/access" {
			t.Fatalf("Location %q, want /acme/demo/settings/access", loc)
		}
		if gotID != "bvsk_owned" {
			t.Fatalf("RevokeSSHKey(%q), want bvsk_owned", gotID)
		}
		if !sink.Has("auth.sshkey.revoked", map[string]string{"kind": "deploy", "fingerprint": "SHA256:OWNED"}) {
			t.Fatal("missing auth.sshkey.revoked audit with kind=deploy")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on deploy key revoke")
		}
	})
}
