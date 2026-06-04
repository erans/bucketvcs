package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/policy"
)

// policyStore returns a repo-admin fakeStore primed for the policy tab.
func policyStore() *fakeStore {
	s := newFakeStore()
	s.perm = auth.PermAdmin
	s.getRepoFlags = func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
		return auth.RepoFlags{}, nil
	}
	return s
}

// fakePolicy implements PolicyAdmin for testing. All methods are no-ops unless
// a corresponding Fn field is set. Recorded calls can be inspected directly.
type fakePolicy struct {
	addFn            func(ctx context.Context, r policy.ProtectedRef) error
	listFn           func(ctx context.Context, tenant, repo string) ([]policy.ProtectedRef, error)
	removeFn         func(ctx context.Context, tenant, repo, pattern string) error
	addPathRuleFn    func(ctx context.Context, in policy.ProtectedPath) error
	listPathRulesFn  func(ctx context.Context, tenant, repo string) ([]policy.ProtectedPath, error)
	removePathRuleFn func(ctx context.Context, tenant, repo, refnamePattern, pathPattern string) error
}

func (f *fakePolicy) Add(ctx context.Context, r policy.ProtectedRef) error {
	if f.addFn != nil {
		return f.addFn(ctx, r)
	}
	return nil
}
func (f *fakePolicy) List(ctx context.Context, tenant, repo string) ([]policy.ProtectedRef, error) {
	if f.listFn != nil {
		return f.listFn(ctx, tenant, repo)
	}
	return nil, nil
}
func (f *fakePolicy) Remove(ctx context.Context, tenant, repo, pattern string) error {
	if f.removeFn != nil {
		return f.removeFn(ctx, tenant, repo, pattern)
	}
	return nil
}
func (f *fakePolicy) AddPathRule(ctx context.Context, in policy.ProtectedPath) error {
	if f.addPathRuleFn != nil {
		return f.addPathRuleFn(ctx, in)
	}
	return nil
}
func (f *fakePolicy) ListPathRules(ctx context.Context, tenant, repo string) ([]policy.ProtectedPath, error) {
	if f.listPathRulesFn != nil {
		return f.listPathRulesFn(ctx, tenant, repo)
	}
	return nil, nil
}
func (f *fakePolicy) RemovePathRule(ctx context.Context, tenant, repo, refnamePattern, pathPattern string) error {
	if f.removePathRuleFn != nil {
		return f.removePathRuleFn(ctx, tenant, repo, refnamePattern, pathPattern)
	}
	return nil
}

// stdRef is a fixture protected ref for use across tests.
var stdRef = policy.ProtectedRef{
	Tenant:         "acme",
	Repo:           "demo",
	RefnamePattern: "refs/heads/main",
	BlockDeletion:  true,
	BlockForcePush: true,
	CreatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
}

// stdPath is a fixture protected path for use across tests.
var stdPath = policy.ProtectedPath{
	Tenant:         "acme",
	Repo:           "demo",
	RefnamePattern: "refs/heads/main",
	PathPattern:    "secrets/**",
	CreatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
}

// TestRepoSettingsPolicyGet covers GET /{t}/{r}/settings/policy.
func TestRepoSettingsPolicyGet(t *testing.T) {
	t.Run("nil policy → renders notice, no forms", func(t *testing.T) {
		store := policyStore()
		h := newTestHandlerWith(store, nil) // Policy stays nil
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/policy", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "not enabled") {
			t.Fatalf("expected 'not enabled' notice; body=%s", body)
		}
		// Must not render add forms when disabled.
		if strings.Contains(body, "add ref rule") {
			t.Fatalf("add ref rule form must not appear when policy disabled; body=%s", body)
		}
		if strings.Contains(body, "add path rule") {
			t.Fatalf("add path rule form must not appear when policy disabled; body=%s", body)
		}
	})

	t.Run("enabled with rules → tables + forms", func(t *testing.T) {
		store := policyStore()
		pol := &fakePolicy{
			listFn: func(ctx context.Context, tenant, repo string) ([]policy.ProtectedRef, error) {
				return []policy.ProtectedRef{stdRef}, nil
			},
			listPathRulesFn: func(ctx context.Context, tenant, repo string) ([]policy.ProtectedPath, error) {
				return []policy.ProtectedPath{stdPath}, nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/policy", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{
			"refs/heads/main", // ref pattern
			"yes",             // block deletion yes
			"add ref rule",    // add refs form
			"secrets/**",      // path pattern
			"add path rule",   // add paths form
			"csrf_token",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("policy page missing %q; body=%s", want, body)
			}
		}
	})

	t.Run("reader → 404 via chassis", func(t *testing.T) {
		store := newFakeStore()
		store.perm = auth.PermRead
		pol := &fakePolicy{}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := httptest.NewRequest(http.MethodGet, "/acme/demo/settings/policy", nil)
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("reader: status %d, want 404", rec.Code)
		}
	})
}

// TestRepoSettingsPolicyRefsAdd covers POST .../policy/refs/add.
func TestRepoSettingsPolicyRefsAdd(t *testing.T) {
	t.Run("form security: reader → 404", func(t *testing.T) {
		store := newFakeStore()
		store.perm = auth.PermRead
		pol := &fakePolicy{}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		assertFormSecurity(t, h, secOpts{
			store:     store,
			path:      "/acme/demo/settings/policy/refs/add",
			form:      url.Values{"pattern": {"refs/heads/main"}, "block_deletion": {"on"}},
			asSession: userSession(),
		})
	})

	t.Run("nil policy → 404 on POST", func(t *testing.T) {
		store := policyStore()
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/policy/refs/add",
			url.Values{"pattern": {"refs/heads/main"}, "block_deletion": {"on"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("nil policy refs/add: status %d, want 404", rec.Code)
		}
	})

	t.Run("empty pattern → flash, Add not called", func(t *testing.T) {
		store := policyStore()
		var called bool
		pol := &fakePolicy{
			addFn: func(ctx context.Context, r policy.ProtectedRef) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := csrfPost(t, "/acme/demo/settings/policy/refs/add",
			url.Values{"pattern": {""}, "block_deletion": {"on"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("Add must not be called for empty pattern")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for empty pattern")
		}
	})

	t.Run("no checkbox selected → flash, Add not called", func(t *testing.T) {
		store := policyStore()
		var called bool
		pol := &fakePolicy{
			addFn: func(ctx context.Context, r policy.ProtectedRef) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := csrfPost(t, "/acme/demo/settings/policy/refs/add",
			url.Values{"pattern": {"refs/heads/main"}}) // no block_deletion or block_force_push
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("Add must not be called with no protection selected")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for no-checkbox")
		}
	})

	t.Run("service ErrInvalidInput → flash surfaced verbatim", func(t *testing.T) {
		store := policyStore()
		svcErr := errors.New("policy: invalid refname_pattern \"[\": syntax error in pattern")
		pol := &fakePolicy{
			addFn: func(ctx context.Context, r policy.ProtectedRef) error {
				return svcErr
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := csrfPost(t, "/acme/demo/settings/policy/refs/add",
			url.Values{"pattern": {"["}, "block_deletion": {"on"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		// Follow the redirect to read the flash
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for service error")
		}
	})

	t.Run("unknown DB error → 500, no flash", func(t *testing.T) {
		store := policyStore()
		pol := &fakePolicy{
			addFn: func(ctx context.Context, r policy.ProtectedRef) error {
				// Mirrors policy.Add's DB wrap: fmt.Errorf("policy add ...: %w", err).
				return errors.New(`policy add "acme"/"demo" "refs/heads/main": disk I/O error`)
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := csrfPost(t, "/acme/demo/settings/policy/refs/add",
			url.Values{"pattern": {"refs/heads/main"}, "block_deletion": {"on"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) != nil {
			t.Fatal("must NOT set flash cookie for masked DB error")
		}
		if strings.Contains(rec.Body.String(), "disk I/O error") {
			t.Fatal("DB error text must not leak into response body")
		}
	})

	t.Run("happy path: Add called + correct ProtectedRef fields + audit", func(t *testing.T) {
		store := policyStore()
		var gotRef policy.ProtectedRef
		pol := &fakePolicy{
			addFn: func(ctx context.Context, r policy.ProtectedRef) error {
				gotRef = r
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/policy/refs/add", url.Values{
			"pattern":          {"refs/heads/main"},
			"block_deletion":   {"on"},
			"block_force_push": {"on"},
		})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings/policy" {
			t.Fatalf("Location %q, want /acme/demo/settings/policy", loc)
		}
		// Check ProtectedRef fields
		if gotRef.Tenant != "acme" || gotRef.Repo != "demo" {
			t.Fatalf("Add Tenant=%q Repo=%q, want acme/demo", gotRef.Tenant, gotRef.Repo)
		}
		if gotRef.RefnamePattern != "refs/heads/main" {
			t.Fatalf("Add RefnamePattern=%q, want refs/heads/main", gotRef.RefnamePattern)
		}
		if !gotRef.BlockDeletion {
			t.Fatal("Add BlockDeletion must be true")
		}
		if !gotRef.BlockForcePush {
			t.Fatal("Add BlockForcePush must be true")
		}
		// Audit event
		if !sink.Has("policy.ref.rule_added", map[string]string{
			"tenant": "acme", "repo": "demo",
			"pattern":          "refs/heads/main",
			"block_deletion":   "true",
			"block_force_push": "true",
		}) {
			t.Fatal("missing policy.ref.rule_added audit event with full attrs")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on success")
		}
	})

	t.Run("happy path: only block_deletion checked", func(t *testing.T) {
		store := policyStore()
		var gotRef policy.ProtectedRef
		pol := &fakePolicy{
			addFn: func(ctx context.Context, r policy.ProtectedRef) error {
				gotRef = r
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := csrfPost(t, "/acme/demo/settings/policy/refs/add", url.Values{
			"pattern":        {"refs/heads/release/*"},
			"block_deletion": {"on"},
			// block_force_push not submitted
		})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if !gotRef.BlockDeletion {
			t.Fatal("Add BlockDeletion must be true")
		}
		if gotRef.BlockForcePush {
			t.Fatal("Add BlockForcePush must be false when not submitted")
		}
	})
}

// TestRepoSettingsPolicyRefsRemove covers POST .../policy/refs/remove.
func TestRepoSettingsPolicyRefsRemove(t *testing.T) {
	t.Run("empty pattern → flash, Remove not called", func(t *testing.T) {
		store := policyStore()
		var called bool
		pol := &fakePolicy{
			removeFn: func(ctx context.Context, tenant, repo, pattern string) error {
				called = true
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/policy/refs/remove",
			url.Values{"pattern": {""}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("Remove must not be called for empty pattern")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for empty pattern")
		}
		if sink.Has("policy.ref.rule_removed", nil) {
			t.Fatal("must not emit audit event for empty pattern")
		}
	})

	t.Run("service ErrNotFound → flash 'no such rule'", func(t *testing.T) {
		store := policyStore()
		pol := &fakePolicy{
			removeFn: func(ctx context.Context, tenant, repo, pattern string) error {
				return policy.ErrNotFound
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := csrfPost(t, "/acme/demo/settings/policy/refs/remove",
			url.Values{"pattern": {"refs/heads/main"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for not-found")
		}
	})

	t.Run("happy path: Remove + audit + 303", func(t *testing.T) {
		store := policyStore()
		var removedPattern string
		pol := &fakePolicy{
			removeFn: func(ctx context.Context, tenant, repo, pattern string) error {
				removedPattern = pattern
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/policy/refs/remove",
			url.Values{"pattern": {"refs/heads/main"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings/policy" {
			t.Fatalf("Location %q, want /acme/demo/settings/policy", loc)
		}
		if removedPattern != "refs/heads/main" {
			t.Fatalf("Remove(%q), want refs/heads/main", removedPattern)
		}
		if !sink.Has("policy.ref.rule_removed", map[string]string{
			"tenant": "acme", "repo": "demo",
			"pattern": "refs/heads/main",
		}) {
			t.Fatal("missing policy.ref.rule_removed audit event with full attrs")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on success")
		}
	})
}

// TestRepoSettingsPolicyPathsAdd covers POST .../policy/paths/add.
func TestRepoSettingsPolicyPathsAdd(t *testing.T) {
	t.Run("nil policy → 404 on POST", func(t *testing.T) {
		store := policyStore()
		h := newTestHandlerWith(store, nil)
		req := csrfPost(t, "/acme/demo/settings/policy/paths/add",
			url.Values{"refname_pattern": {"refs/heads/main"}, "path_pattern": {"secrets/**"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("nil policy paths/add: status %d, want 404", rec.Code)
		}
	})

	t.Run("empty refname_pattern → flash, AddPathRule not called", func(t *testing.T) {
		store := policyStore()
		var called bool
		pol := &fakePolicy{
			addPathRuleFn: func(ctx context.Context, in policy.ProtectedPath) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := csrfPost(t, "/acme/demo/settings/policy/paths/add",
			url.Values{"refname_pattern": {""}, "path_pattern": {"secrets/**"}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("AddPathRule must not be called for empty refname_pattern")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for empty fields")
		}
	})

	t.Run("empty path_pattern → flash, AddPathRule not called", func(t *testing.T) {
		store := policyStore()
		var called bool
		pol := &fakePolicy{
			addPathRuleFn: func(ctx context.Context, in policy.ProtectedPath) error {
				called = true
				return nil
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := csrfPost(t, "/acme/demo/settings/policy/paths/add",
			url.Values{"refname_pattern": {"refs/heads/main"}, "path_pattern": {""}})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("AddPathRule must not be called for empty path_pattern")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for empty fields")
		}
	})

	t.Run("service ErrInvalidInput (bad pattern) → flash error text surfaced", func(t *testing.T) {
		store := policyStore()
		// Return an error that wraps ErrInvalidInput (as AddPathRule does for bad patterns).
		invalidErr := errors.Join(policy.ErrInvalidInput,
			errors.New("invalid path_pattern: pattern contains invalid characters"))
		pol := &fakePolicy{
			addPathRuleFn: func(ctx context.Context, in policy.ProtectedPath) error {
				return invalidErr
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := csrfPost(t, "/acme/demo/settings/policy/paths/add", url.Values{
			"refname_pattern": {"refs/heads/main"},
			"path_pattern":    {"secrets/[bad"},
		})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for invalid pattern")
		}
	})

	t.Run("unknown DB error → 500, no flash", func(t *testing.T) {
		store := policyStore()
		pol := &fakePolicy{
			addPathRuleFn: func(ctx context.Context, in policy.ProtectedPath) error {
				// Mirrors AddPathRule's DB wrap: fmt.Errorf("policy: add path rule: %w", err).
				// Note the "policy: " prefix MUST still mask (it is a DB wrap, not validation).
				return errors.New("policy: add path rule: disk I/O error")
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := csrfPost(t, "/acme/demo/settings/policy/paths/add", url.Values{
			"refname_pattern": {"refs/heads/main"},
			"path_pattern":    {"secrets/**"},
		})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) != nil {
			t.Fatal("must NOT set flash cookie for masked DB error")
		}
		if strings.Contains(rec.Body.String(), "disk I/O error") {
			t.Fatal("DB error text must not leak into response body")
		}
	})

	t.Run("happy path: AddPathRule called + correct fields + audit", func(t *testing.T) {
		store := policyStore()
		var gotPath policy.ProtectedPath
		pol := &fakePolicy{
			addPathRuleFn: func(ctx context.Context, in policy.ProtectedPath) error {
				gotPath = in
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/policy/paths/add", url.Values{
			"refname_pattern": {"refs/heads/main"},
			"path_pattern":    {"secrets/**"},
		})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings/policy" {
			t.Fatalf("Location %q, want /acme/demo/settings/policy", loc)
		}
		if gotPath.Tenant != "acme" || gotPath.Repo != "demo" {
			t.Fatalf("AddPathRule Tenant=%q Repo=%q, want acme/demo", gotPath.Tenant, gotPath.Repo)
		}
		if gotPath.RefnamePattern != "refs/heads/main" {
			t.Fatalf("AddPathRule RefnamePattern=%q, want refs/heads/main", gotPath.RefnamePattern)
		}
		if gotPath.PathPattern != "secrets/**" {
			t.Fatalf("AddPathRule PathPattern=%q, want secrets/**", gotPath.PathPattern)
		}
		if !sink.Has("policy.path.rule_added", map[string]string{
			"tenant": "acme", "repo": "demo",
			"refname_pattern": "refs/heads/main",
			"path_pattern":    "secrets/**",
		}) {
			t.Fatal("missing policy.path.rule_added audit event with full attrs")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on success")
		}
	})
}

// TestRepoSettingsPolicyPathsRemove covers POST .../policy/paths/remove.
func TestRepoSettingsPolicyPathsRemove(t *testing.T) {
	t.Run("empty refname_pattern → flash, RemovePathRule not called", func(t *testing.T) {
		store := policyStore()
		var called bool
		pol := &fakePolicy{
			removePathRuleFn: func(ctx context.Context, tenant, repo, refnamePattern, pathPattern string) error {
				called = true
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/policy/paths/remove", url.Values{
			"refname_pattern": {""},
			"path_pattern":    {"secrets/**"},
		})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("RemovePathRule must not be called for empty refname_pattern")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for empty patterns")
		}
		if sink.Has("policy.path.rule_removed", nil) {
			t.Fatal("must not emit audit event for empty patterns")
		}
	})

	t.Run("empty path_pattern → flash, RemovePathRule not called", func(t *testing.T) {
		store := policyStore()
		var called bool
		pol := &fakePolicy{
			removePathRuleFn: func(ctx context.Context, tenant, repo, refnamePattern, pathPattern string) error {
				called = true
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/policy/paths/remove", url.Values{
			"refname_pattern": {"refs/heads/main"},
			"path_pattern":    {""},
		})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if called {
			t.Fatal("RemovePathRule must not be called for empty path_pattern")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for empty patterns")
		}
		if sink.Has("policy.path.rule_removed", nil) {
			t.Fatal("must not emit audit event for empty patterns")
		}
	})

	t.Run("service ErrNotFound → flash 'no such rule'", func(t *testing.T) {
		store := policyStore()
		pol := &fakePolicy{
			removePathRuleFn: func(ctx context.Context, tenant, repo, refnamePattern, pathPattern string) error {
				return policy.ErrNotFound
			},
		}
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol })
		req := csrfPost(t, "/acme/demo/settings/policy/paths/remove", url.Values{
			"refname_pattern": {"refs/heads/main"},
			"path_pattern":    {"secrets/**"},
		})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie for not-found")
		}
	})

	t.Run("happy path: RemovePathRule + audit + 303", func(t *testing.T) {
		store := policyStore()
		var gotRefname, gotPath string
		pol := &fakePolicy{
			removePathRuleFn: func(ctx context.Context, tenant, repo, refnamePattern, pathPattern string) error {
				gotRefname = refnamePattern
				gotPath = pathPattern
				return nil
			},
		}
		logger, sink := newTestLogger()
		h := newTestHandlerWith(store, func(d *Deps) { d.Policy = pol; d.Logger = logger })
		req := csrfPost(t, "/acme/demo/settings/policy/paths/remove", url.Values{
			"refname_pattern": {"refs/heads/main"},
			"path_pattern":    {"secrets/**"},
		})
		addSessionCookie(t, req, store, userSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/demo/settings/policy" {
			t.Fatalf("Location %q, want /acme/demo/settings/policy", loc)
		}
		if gotRefname != "refs/heads/main" {
			t.Fatalf("RemovePathRule refname=%q, want refs/heads/main", gotRefname)
		}
		if gotPath != "secrets/**" {
			t.Fatalf("RemovePathRule path=%q, want secrets/**", gotPath)
		}
		if !sink.Has("policy.path.rule_removed", map[string]string{
			"tenant": "acme", "repo": "demo",
			"refname_pattern": "refs/heads/main",
			"path_pattern":    "secrets/**",
		}) {
			t.Fatal("missing policy.path.rule_removed audit event with full attrs")
		}
		if findCookie(rec.Result().Cookies(), flashCookieName) == nil {
			t.Fatal("expected flash cookie on success")
		}
	})
}
