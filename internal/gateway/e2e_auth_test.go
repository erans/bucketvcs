package gateway

// End-to-end auth scenarios (M4 Task 24): drive a real `git` binary against
// the gateway with a credential helper and assert the protocol-level outcome
// for each combination of (auth state, repo flags, requested action).
//
// Each scenario:
//   1. Builds a localfs object store and seeds it with a tiny bare repo via
//      the importer (so the bucket has at least one commit + ref to clone).
//   2. Builds a sqlitestore.Store, registers the repo, optionally sets it
//      public, and seeds users + tokens as the scenario requires.
//   3. Starts an httptest.Server wrapping the gateway with the real
//      sqlitestore as AuthStore.
//   4. Drives `git clone` / `git push` via exec.CommandContext with
//      GIT_TERMINAL_PROMPT=0 and a credential.helper that emits exactly the
//      configured username + password. Asserts on exit status + stderr
//      substring (lower-cased, because git's exact wording varies by
//      version).

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// skipIfNoGit skips the test if `git` is not on PATH.
func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

// e2eEnv bundles everything an e2e test needs: a running httptest server, a
// sqlite-backed auth store, and the (tenant, repo) it was seeded with.
type e2eEnv struct {
	srv     *httptest.Server
	store   *sqlitestore.Store
	tenant  string
	repo    string
	baseURL string
}

// newE2EEnv spins up a fresh gateway, seeds a one-commit fixture repo, and
// registers (tenant, repo) in the auth store. Cleanup is registered via
// t.Cleanup. The returned auth store is mutable so the caller can grant
// permissions, register users and tokens, etc., before driving git.
func newE2EEnv(t *testing.T, tenant, repo string) *e2eEnv {
	t.Helper()
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, tenant, repo)
	objStore, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = objStore.Close() })

	authStore, err := sqlitestore.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = authStore.Close() })
	if err := authStore.RegisterRepo(context.Background(), tenant, repo); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}

	srv, err := NewServer(objStore, Options{
		MirrorDir: t.TempDir(),
		Version:   "test",
		AuthStore: authStore,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	return &e2eEnv{
		srv:     ts,
		store:   authStore,
		tenant:  tenant,
		repo:    repo,
		baseURL: ts.URL,
	}
}

// remoteURL returns the .git URL the test should clone/push against.
func (e *e2eEnv) remoteURL() string {
	return e.baseURL + "/" + e.tenant + "/" + e.repo + ".git"
}

// seedUserToken creates a user (admin or not), generates a token, persists it,
// and returns the cleartext token string.
func seedUserToken(t *testing.T, s *sqlitestore.Store, name string, isAdmin bool, expiresAt *int64) (token string) {
	t.Helper()
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, name, isAdmin)
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", name, err)
	}
	tokStr, id, secret, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	if err := s.CreateToken(ctx, id, uid, hash, "e2e", expiresAt); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return tokStr
}

// writeCredentialHelper writes a small `sh` script that emits username= and
// password= lines for `git credential fill`. Returns the script's absolute
// path. The script is mode 0o700 inside a t.TempDir() so it's auto-cleaned.
func writeCredentialHelper(t *testing.T, user, pass string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "helper.sh")
	// `git credential fill` ignores any extra output; we emit exactly the
	// two required lines and exit. Single-quoting in shell to defend
	// against accidental shell metachars in the test inputs (we control
	// them, but defense in depth keeps the helpers robust).
	body := fmt.Sprintf("#!/bin/sh\nprintf 'username=%%s\\n' %s\nprintf 'password=%%s\\n' %s\n",
		shellSingleQuote(user), shellSingleQuote(pass))
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	return script
}

// shellSingleQuote wraps s in POSIX single-quotes, escaping any embedded
// single quotes by terminating, escaping, and re-opening: foo'bar -> 'foo'\”bar'.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gitWithHelper runs git with credential.helper pinned to `helper` (or no
// helper at all when helper == ""), GIT_TERMINAL_PROMPT=0, and a 30 s
// deadline. Returns combined stdout+stderr and the exit error.
func gitWithHelper(t *testing.T, helper string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	full := []string{
		// Wipe any inherited helpers from the user's gitconfig so the
		// test is hermetic — the empty value resets the chain, then we
		// optionally append our shell helper.
		"-c", "credential.helper=",
	}
	if helper != "" {
		full = append(full, "-c", "credential.helper=!"+helper)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	// Hermetic env: never prompt the terminal, never consult the user's
	// askpass, and silence the askpass-of-last-resort.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GCM_INTERACTIVE=Never",
	)
	return cmd.CombinedOutput()
}

// gitConfigEnv returns os.Environ() augmented with a tmp HOME so any global
// gitconfig on the developer's box doesn't leak into the test.
//
// (Currently unused — gitWithHelper resets credential.helper inline. Kept as
// a documented hook for future scenarios that need stricter isolation.)
func gitConfigEnv(t *testing.T) []string {
	t.Helper()
	return append(os.Environ(), "HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir())
}

// makeWorkRepo builds a fresh local git repo with one commit and returns its
// path. Used by the push scenarios.
func makeWorkRepo(t *testing.T) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "wt")
	mustExecGW(t, "", "git", "init", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExecGW(t, work, "git", "add", ".")
	mustExecGW(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "-m", "init")
	return work
}

// containsAny reports whether haystack (lower-cased) contains any of the
// given lower-cased substrings. Used to match git's varying error wording.
func containsAny(haystack string, needles ...string) bool {
	low := strings.ToLower(haystack)
	for _, n := range needles {
		if strings.Contains(low, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

// expectGitFails asserts the git command failed and the output mentions one
// of the expected error tokens.
func expectGitFails(t *testing.T, out []byte, err error, errTokens ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected git to fail, but it succeeded. output:\n%s", out)
	}
	if len(errTokens) > 0 && !containsAny(string(out), errTokens...) {
		t.Fatalf("git failed but output didn't match any of %v.\nerr=%v\noutput:\n%s",
			errTokens, err, out)
	}
}

// withBasicAuth rewrites a remote URL to embed user:pass credentials.
// Used as a fallback when credential.helper is awkward (e.g. for `git push`,
// where helpers run only on 401 challenge — for clone/fetch we always use
// the helper). NOTE: this is currently unused in the suite; the helper
// approach exercises the full WWW-Authenticate path. Kept for future use.
func withBasicAuth(remote, user, pass string) string {
	u, err := url.Parse(remote)
	if err != nil {
		return remote
	}
	u.User = url.UserPassword(user, pass)
	return u.String()
}

// -----------------------------------------------------------------------------
// Scenarios
// -----------------------------------------------------------------------------

// 1. clone no creds, private repo → fails (401).
func TestE2E_CloneNoCreds_PrivateRepo_Fails(t *testing.T) {
	skipIfNoGit(t)
	env := newE2EEnv(t, "acme", "demo")
	dst := filepath.Join(t.TempDir(), "clone")

	out, err := gitWithHelper(t, "", "clone", env.remoteURL(), dst)
	expectGitFails(t, out, err,
		"401", "unauthorized", "authentication", "could not read username", "terminal prompts disabled")
}

// 2. clone no creds, public repo → succeeds.
func TestE2E_CloneNoCreds_PublicRepo_Succeeds(t *testing.T) {
	skipIfNoGit(t)
	env := newE2EEnv(t, "acme", "demo")
	if err := env.store.SetRepoPublic(context.Background(), env.tenant, env.repo, true); err != nil {
		t.Fatalf("SetRepoPublic: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "clone")

	out, err := gitWithHelper(t, "", "clone", env.remoteURL(), dst)
	if err != nil {
		t.Fatalf("clone failed: %v\noutput:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "a.txt")); err != nil {
		t.Fatalf("expected a.txt in clone: %v", err)
	}
}

// 3. clone with valid creds → succeeds.
func TestE2E_CloneWithValidCreds_Succeeds(t *testing.T) {
	skipIfNoGit(t)
	env := newE2EEnv(t, "acme", "demo")
	tok := seedUserToken(t, env.store, "alice", false, nil)
	if err := env.store.Grant(context.Background(), "alice", env.tenant, env.repo, "read"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	helper := writeCredentialHelper(t, "alice", tok)
	dst := filepath.Join(t.TempDir(), "clone")

	out, err := gitWithHelper(t, helper, "clone", env.remoteURL(), dst)
	if err != nil {
		t.Fatalf("clone failed: %v\noutput:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "a.txt")); err != nil {
		t.Fatalf("expected a.txt in clone: %v", err)
	}
}

// 4. clone with revoked token → fails 401.
func TestE2E_CloneWithRevokedToken_Fails(t *testing.T) {
	skipIfNoGit(t)
	env := newE2EEnv(t, "acme", "demo")
	tok := seedUserToken(t, env.store, "alice", false, nil)
	if err := env.store.Grant(context.Background(), "alice", env.tenant, env.repo, "read"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	// Revoke before use. The token id is the second underscore-separated
	// segment of the printable token string.
	id, _, perr := auth.ParseToken(tok)
	if perr != nil {
		t.Fatalf("ParseToken: %v", perr)
	}
	if err := env.store.RevokeToken(context.Background(), id); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	helper := writeCredentialHelper(t, "alice", tok)
	dst := filepath.Join(t.TempDir(), "clone")

	out, err := gitWithHelper(t, helper, "clone", env.remoteURL(), dst)
	expectGitFails(t, out, err, "401", "unauthorized", "authentication")
}

// 5. clone with expired token → fails 401.
func TestE2E_CloneWithExpiredToken_Fails(t *testing.T) {
	skipIfNoGit(t)
	env := newE2EEnv(t, "acme", "demo")
	expired := int64(1) // 1970-01-01, guaranteed in the past
	tok := seedUserToken(t, env.store, "alice", false, &expired)
	if err := env.store.Grant(context.Background(), "alice", env.tenant, env.repo, "read"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	helper := writeCredentialHelper(t, "alice", tok)
	dst := filepath.Join(t.TempDir(), "clone")

	out, err := gitWithHelper(t, helper, "clone", env.remoteURL(), dst)
	expectGitFails(t, out, err, "401", "unauthorized", "authentication")
}

// 6. push with read-only token → fails 403.
func TestE2E_PushReadOnlyToken_Fails(t *testing.T) {
	skipIfNoGit(t)
	env := newE2EEnv(t, "acme", "demo")
	tok := seedUserToken(t, env.store, "alice", false, nil)
	if err := env.store.Grant(context.Background(), "alice", env.tenant, env.repo, "read"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	helper := writeCredentialHelper(t, "alice", tok)
	work := makeWorkRepo(t)

	// Add a new commit to push.
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExecGW(t, work, "git", "add", ".")
	mustExecGW(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "-m", "more")

	out, err := gitWithHelper(t, helper, "-C", work, "push", env.remoteURL(), "HEAD:refs/heads/topic")
	expectGitFails(t, out, err, "403", "forbidden", "insufficient")
}

// 7. push with write token → succeeds.
func TestE2E_PushWriteToken_Succeeds(t *testing.T) {
	skipIfNoGit(t)
	env := newE2EEnv(t, "acme", "demo")
	tok := seedUserToken(t, env.store, "alice", false, nil)
	if err := env.store.Grant(context.Background(), "alice", env.tenant, env.repo, "write"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	helper := writeCredentialHelper(t, "alice", tok)
	work := makeWorkRepo(t)

	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExecGW(t, work, "git", "add", ".")
	mustExecGW(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "-m", "more")

	out, err := gitWithHelper(t, helper, "-C", work, "push", env.remoteURL(), "HEAD:refs/heads/topic")
	if err != nil {
		t.Fatalf("push failed: %v\noutput:\n%s", err, out)
	}
}

// 8. push public repo no creds → fails 401 challenge.
//
// Even on a public-read repo, writes always require a real credential. The
// gateway responds with 401 (not 403) so the client can re-attempt with
// auth.
func TestE2E_PushPublicRepoNoCreds_Fails(t *testing.T) {
	skipIfNoGit(t)
	env := newE2EEnv(t, "acme", "demo")
	if err := env.store.SetRepoPublic(context.Background(), env.tenant, env.repo, true); err != nil {
		t.Fatalf("SetRepoPublic: %v", err)
	}
	work := makeWorkRepo(t)
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExecGW(t, work, "git", "add", ".")
	mustExecGW(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "-m", "more")

	out, err := gitWithHelper(t, "", "-C", work, "push", env.remoteURL(), "HEAD:refs/heads/topic")
	expectGitFails(t, out, err,
		"401", "unauthorized", "authentication", "could not read username", "terminal prompts disabled")
}

// 9. clone with disabled-user token → fails 401.
func TestE2E_CloneDisabledUserToken_Fails(t *testing.T) {
	skipIfNoGit(t)
	env := newE2EEnv(t, "acme", "demo")
	tok := seedUserToken(t, env.store, "alice", false, nil)
	if err := env.store.Grant(context.Background(), "alice", env.tenant, env.repo, "read"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := env.store.SetUserDisabled(context.Background(), "alice", true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	helper := writeCredentialHelper(t, "alice", tok)
	dst := filepath.Join(t.TempDir(), "clone")

	out, err := gitWithHelper(t, helper, "clone", env.remoteURL(), dst)
	expectGitFails(t, out, err, "401", "unauthorized", "authentication")
}

// 10. admin user accesses any repo without explicit grant → succeeds.
func TestE2E_AdminUserNoExplicitGrant_Succeeds(t *testing.T) {
	skipIfNoGit(t)
	env := newE2EEnv(t, "acme", "demo")
	tok := seedUserToken(t, env.store, "root", true, nil)
	// Deliberately NO Grant call: admin short-circuits in Decide.
	helper := writeCredentialHelper(t, "root", tok)
	dst := filepath.Join(t.TempDir(), "clone")

	out, err := gitWithHelper(t, helper, "clone", env.remoteURL(), dst)
	if err != nil {
		t.Fatalf("admin clone failed: %v\noutput:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "a.txt")); err != nil {
		t.Fatalf("expected a.txt in admin clone: %v", err)
	}
}

// Compile-time references to satisfy the unused-import / unused-helper
// linters in case future scenarios drop the helpers.
var (
	_ = importer.Import
	_ = gitConfigEnv
	_ = withBasicAuth
)
