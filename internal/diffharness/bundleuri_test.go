package diffharness

// bundleuri_test.go drives upstream `git clone` against an in-process
// gateway with `transfer.bundleURI=true` to exercise the M11 bundle-uri
// end-to-end path. Two cases:
//
//   - TestBundleURI_ClientUsesBundle: happy path. Maintenance produces a
//     bundle; a fresh clone with bundle-uri enabled succeeds, HEAD
//     matches upstream, and the GIT_TRACE2+GIT_TRACE_CURL trace shows
//     the client actually downloaded a bundle from the gateway's
//     `/_bundle/<hash>` proxied route (the only marker that a real
//     server-side bundle-uri exchange occurred — the client config
//     string alone is not sufficient evidence).
//
//   - TestBundleURI_ForcePushDropsBundle: regression. After bundle
//     generation, push a divergent tip via force-push. Re-clone with
//     bundle-uri enabled: clone still succeeds but the trace shows the
//     standard pack path fired (`--keep=fetch-pack` keepfile arg, which
//     fetch-pack.c injects only on the pack-protocol fetch path — not
//     during bundle verification), i.e. either the gateway suppressed a
//     stale bundle or the client ignored it on fingerprint mismatch —
//     either way the fallback works.

import (
	"context"
	"crypto/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	maintpkg "github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// bundleURITestTimeout bounds every `git` child this test spawns. Without
// a per-command deadline, a gateway deadlock during clone or a stuck
// fetch-pack waiting on protocol bytes would block until Go's test
// timeout fires. 60s is generous for these tiny fixtures.
const bundleURITestTimeout = 60 * time.Second

// startBundleURIGateway constructs an in-process gateway with bundle-uri
// enabled (auto mode, proxied-URL fallback wired) and a preallocated
// listener so ProxiedBaseURL can be set before NewServer is called.
// Returns the started httptest.Server; both server-close and listener-
// close are registered via t.Cleanup. The test should reference ts.URL
// only — baseURL is asserted equal internally.
//
// As of M19 the gateway computes storage keys directly via
// internal/repo/keys (no ProxiedKeyResolver indirection); the (tenant,
// repo) the URLBuilder embeds in each minted URL identifies the repo.
func startBundleURIGateway(t *testing.T, store storage.ObjectStore, authStore auth.Store) *httptest.Server {
	t.Helper()

	// Preallocate the listener so we know ProxiedBaseURL before
	// gateway.NewServer validates the proxied-URL configuration.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	baseURL := "http://" + l.Addr().String()

	signingKey := mustRandKey(t)
	srv, err := gateway.NewServer(store, gateway.Options{
		MirrorDir:            t.TempDir(),
		Version:              "test",
		AuthStore:            authStore,
		BundleURIEnabled:     true,
		BundleURIMode:        gateway.URIModeAuto,
		ProxiedURLSigningKey: signingKey,
		ProxiedBaseURL:       baseURL,
		BundleURITTL:         4 * time.Hour,
		BundleWarmCommits:    100,
		BundleWarmAge:        24 * time.Hour,
	})
	if err != nil {
		_ = l.Close()
		t.Fatalf("gateway.NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// Wrap the preallocated listener in an httptest.Server. Start()
	// derives ts.URL from the listener's address, so it will match
	// baseURL exactly.
	ts := &httptest.Server{Listener: l, Config: &http.Server{Handler: srv}}
	ts.Start()
	t.Cleanup(ts.Close)
	if ts.URL != baseURL {
		t.Fatalf("ts.URL=%q != baseURL=%q (preallocated-listener trick failed)",
			ts.URL, baseURL)
	}
	return ts
}

// mustRandKey returns 32 random bytes for the proxied URL signing key.
// 32 bytes comfortably exceeds the 16-byte minimum enforced by
// gateway.NewServer and matches the size proxiedurl.Mint uses internally.
func mustRandKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return k
}

// redactURLCreds rewrites any "scheme://user:pass@host..." URL embedded
// in s to "scheme://***:***@host..." so credential-bearing URLs (e.g.
// pushURL with admin-token basic auth) don't leak into test logs when
// args slices get logged on failure.
//
// We operate on a slice of args by walking each element and re-parsing
// values that look like URLs. The bar is "credentials never appear in a
// t.Fatalf output", not exhaustive URL detection; non-URL args pass
// through unchanged.
func redactURLCreds(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if !strings.Contains(a, "://") {
			out[i] = a
			continue
		}
		u, err := url.Parse(a)
		if err != nil || u.User == nil {
			out[i] = a
			continue
		}
		u.User = url.UserPassword("***", "***")
		out[i] = u.String()
	}
	return out
}

// credSafeGitEnv returns an environment slice suitable for git child
// processes that handle credentialed URLs. It scrubs the GIT_TRACE_*
// and GIT_TRACE2* variables that can cause git to echo full URLs (with
// embedded basic auth) into stderr/stdout — even if a developer's shell
// pre-sets any of them, the explicit empty-string overrides here disable
// them for the child. Without this, `out` from CombinedOutput() (logged
// via %s on failure) could leak admin tokens into CI logs since
// redactURLCreds only covers the args slice.
//
// The GIT_TRACE2 family is included because trace2's child_start events
// emit the full argv (including pushURL with admin-token basic auth) on
// every git subprocess invocation; a pre-set GIT_TRACE2 would surface
// credentials in CombinedOutput on any push/clone failure path.
//
// Tests that intentionally want a trace (e.g. for `--keep=fetch-pack`
// assertions) should start from credSafeGitEnv() and then append the
// specific trace vars they want — appending wins over the empty-string
// scrub since the child env is read left-to-right.
func credSafeGitEnv() []string {
	return append(os.Environ(),
		"GIT_TRACE=",
		"GIT_TRACE_CURL=",
		"GIT_TRACE_PACKET=",
		"GIT_CURL_VERBOSE=",
		"GIT_TRACE2=",
		"GIT_TRACE2_EVENT=",
		"GIT_TRACE2_PERF=",
	)
}

// gitVersion runs `git --version` and returns its first line, or "" on
// error. Tests log this at startup so a future failure that turns out
// to be git-version-related (e.g. the undocumented `--keep=fetch-pack`
// argv format changing) is easier to diagnose from logs alone.
func gitVersion(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// TestBundleURI_ClientUsesBundle drives the happy path: bundle generated
// by maintenance, advertised over /_bundle/<hash>, consumed by upstream
// `git clone -c transfer.bundleURI=true`.
func TestBundleURI_ClientUsesBundle(t *testing.T) {
	skipIfNoGit(t)

	ctx, cancel := context.WithTimeout(context.Background(), bundleURITestTimeout)
	t.Cleanup(cancel)

	t.Logf("git version: %s", gitVersion(ctx))

	tenant := "fx"
	repoID := "single_commit"

	// Build source fixture (single commit on refs/heads/main).
	srcDir := filepath.Join(t.TempDir(), "src")
	build := fixtures.Registry[repoID]
	if build == nil {
		t.Fatalf("fixture %q not in registry", repoID)
	}
	build(t, srcDir)
	gitFsck(t, srcDir)

	store := newTestStore(t)

	// Import the bare source into the store.
	if _, err := importer.Import(ctx, store, importer.Options{
		SourceDir: srcDir, Tenant: tenant, Repo: repoID, Actor: "harness",
	}); err != nil {
		t.Fatalf("importer.Import: %v", err)
	}

	// Open repo + keys for maintenance.Run.
	r, err := repo.Open(ctx, store, tenant, repoID)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	k, err := keys.NewRepo(tenant, repoID)
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	// Force a bundle to be generated. BundleOnly skips repack/compact so
	// the test stays focused on the bundle pipeline.
	report, err := maintpkg.Run(ctx, store, r, k, maintpkg.RunOptions{
		BundleOnly: true,
		Force:      true,
	})
	if err != nil {
		t.Fatalf("maintenance.Run: %v", err)
	}
	if report.BundleResult == nil {
		t.Fatalf("BundleResult is nil; want non-nil for forced bundle run")
	}
	if !report.BundleResult.Generated {
		t.Fatalf("BundleResult.Generated=false (reason=%q, err=%q); want true",
			report.BundleResult.TriggerReason, report.BundleResult.ErrorMessage)
	}

	ts := startBundleURIGateway(t, store, newDiffharnessAuthStore(t, tenant, repoID))

	// Clone with bundle-uri enabled, capturing both GIT_TRACE2 (which
	// records subprocess child_start events including their argv) and
	// GIT_TRACE_CURL (which records full HTTP request URLs). Together
	// they emit the proxied bundle URL (`/_bundle/<hash>`) on two
	// independent surfaces, so the assertion below is robust against
	// trace-format variations across git versions.
	cloneDir := filepath.Join(t.TempDir(), "clone")
	cmd := exec.CommandContext(ctx, "git", "clone",
		"-c", "protocol.version=2",
		"-c", "transfer.bundleURI=true",
		ts.URL+"/"+tenant+"/"+repoID+".git", cloneDir)
	cmd.Env = append(credSafeGitEnv(), "GIT_TRACE2=2", "GIT_TRACE_CURL=1")
	var trace strings.Builder
	cmd.Stdout = &trace
	cmd.Stderr = &trace
	if err := cmd.Run(); err != nil {
		t.Fatalf("git clone failed: %v\n%s", err, trace.String())
	}

	// Compare cloned HEAD to source HEAD (main branch).
	srcHead, err := gitcli.RunForTest(srcDir, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse src: %v: %s", err, srcHead)
	}
	cloneHead, err := gitcli.RunForTest(cloneDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse clone: %v: %s", err, cloneHead)
	}
	if strings.TrimSpace(string(srcHead)) != strings.TrimSpace(string(cloneHead)) {
		t.Fatalf("HEAD mismatch: src=%q clone=%q",
			strings.TrimSpace(string(srcHead)),
			strings.TrimSpace(string(cloneHead)))
	}

	// Strong server-side marker: the proxied bundle URL path. The
	// gateway routes bundle downloads under `/_bundle/<hash>`; this
	// path only appears in the trace if the gateway actually advertised
	// a bundle URL AND the client actually attempted to fetch it. A
	// regression that disables bundle-uri server-side (advertise off,
	// signing off, routing off) emits no `/_bundle/` reference at all —
	// the standard pack-fetch fallback completes the clone and the test
	// FAILS as required.
	//
	// We also assert the negative marker: `--keep=fetch-pack`, which
	// fetch-pack.c injects into its `git index-pack` argv ONLY when
	// receiving a pack via the standard pack-protocol fetch path
	// (bundle verification uses the bundle filename as its --keep
	// payload, not "fetch-pack"). Its absence here proves the clone
	// completed via the bundle path, not the fallback pack-fetch.
	//
	// Version sensitivity: this matches on the `--keep=fetch-pack`
	// prefix only. fetch-pack.c's add_index_pack_keep_option emits a
	// `--keep=fetch-pack <pid> on <host>` value, but that exact suffix
	// format is undocumented and has shifted historically — prefix-
	// match keeps the assertion robust across git versions. If a future
	// git release drops the "fetch-pack" tag entirely, the t.Logf of
	// `git --version` above will surface the source of the regression.
	//
	// The substring "bundle" alone is NOT a reliable marker: GIT_TRACE2
	// echoes the clone command's argv (which includes `-c
	// transfer.bundleURI=true`) and emits def_param events for
	// configured parameters, so a server-side regression that fully
	// disables bundle-uri would still produce a trace containing
	// "bundle" via the client config alone.
	traceStr := trace.String()
	if !strings.Contains(traceStr, "/_bundle/") {
		t.Fatalf("GIT_TRACE2/GIT_TRACE_CURL output does not contain "+
			"`/_bundle/` — client never fetched a bundle from the "+
			"gateway's proxied route; bundle-uri path not exercised:\n%s",
			traceStr)
	}
	if strings.Contains(traceStr, "--keep=fetch-pack") {
		t.Fatalf("GIT_TRACE2 output contains `--keep=fetch-pack` — "+
			"standard pack-fetch fallback fired; clone did NOT "+
			"complete via the bundle path:\n%s", traceStr)
	}
}

// TestBundleURI_ForcePushDropsBundle is the regression: after a
// force-push diverges the tip from what the bundle covers, a re-clone
// with bundle-uri enabled must still succeed and the trace must show
// the standard pack path fired.
func TestBundleURI_ForcePushDropsBundle(t *testing.T) {
	skipIfNoGit(t)

	ctx, cancel := context.WithTimeout(context.Background(), bundleURITestTimeout)
	t.Cleanup(cancel)

	t.Logf("git version: %s", gitVersion(ctx))

	tenant := "fx"
	repoID := "single_commit_force"

	// Build source fixture.
	srcDir := filepath.Join(t.TempDir(), "src")
	build := fixtures.Registry["single_commit"]
	if build == nil {
		t.Fatalf("fixture \"single_commit\" not in registry")
	}
	build(t, srcDir)
	gitFsck(t, srcDir)

	store := newTestStore(t)

	if _, err := importer.Import(ctx, store, importer.Options{
		SourceDir: srcDir, Tenant: tenant, Repo: repoID, Actor: "harness",
	}); err != nil {
		t.Fatalf("importer.Import: %v", err)
	}

	r, err := repo.Open(ctx, store, tenant, repoID)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	k, err := keys.NewRepo(tenant, repoID)
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	// Generate a bundle covering the initial tip.
	report, err := maintpkg.Run(ctx, store, r, k, maintpkg.RunOptions{
		BundleOnly: true,
		Force:      true,
	})
	if err != nil {
		t.Fatalf("maintenance.Run: %v", err)
	}
	if report.BundleResult == nil || !report.BundleResult.Generated {
		t.Fatalf("BundleResult not generated; want a fresh bundle for the regression")
	}

	// Stand up a gateway that exercises bundle-uri + admin-token push.
	// We need a writable auth store here (force-push goes through
	// receive-pack), so unlike the happy-path test we use the
	// admin-token store and also mark the repo public_read so the
	// re-clone (without creds) succeeds — the bundle path is what the
	// test is asserting on, not auth coverage.
	authStore, adminUser, adminToken := newDiffharnessAuthStoreWithAdminToken(t, tenant, repoID)
	if err := authStore.SetRepoPublic(context.Background(), tenant, repoID, true); err != nil {
		t.Fatalf("SetRepoPublic: %v", err)
	}

	ts := startBundleURIGateway(t, store, authStore)

	repoURL := ts.URL + "/" + tenant + "/" + repoID + ".git"
	pushURL := withDiffharnessBasicAuth(repoURL, adminUser, adminToken)

	// Force-push a divergent tip:
	//  1. Clone the gateway repo into a fresh working dir.
	//  2. Reset HEAD to an unrelated history and add a divergent commit.
	//  3. Push --force back to main.
	work := filepath.Join(t.TempDir(), "work")
	cloneWork := exec.CommandContext(ctx, "git", "clone", "-c", "protocol.version=2",
		pushURL, work)
	// pushURL embeds admin-token basic auth. Scrub GIT_TRACE* env vars so
	// git can't echo the credentialed URL into CombinedOutput() via a
	// developer's pre-set trace var. redactURLCreds covers args; this
	// covers `out`.
	cloneWork.Env = credSafeGitEnv()
	if out, err := cloneWork.CombinedOutput(); err != nil {
		t.Fatalf("git clone work: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"-C", work, "config", "user.name", "diffharness"},
		{"-C", work, "config", "user.email", "harness@diff"},
		// Replace history with an orphan branch carrying a single
		// commit that has a different OID than the original.
		{"-C", work, "checkout", "--orphan", "diverge"},
		{"-C", work, "rm", "-rf", "."},
	} {
		if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", redactURLCreds(args), err, out)
		}
	}
	// Author a new file so the orphan branch has content.
	if err := os.WriteFile(filepath.Join(work, "diverged.txt"), []byte("divergent\n"), 0o644); err != nil {
		t.Fatalf("write diverged.txt: %v", err)
	}
	for _, args := range [][]string{
		{"-C", work, "add", "diverged.txt"},
		{"-C", work, "commit", "-m", "divergent root"},
		// Move the divergent commit back onto main.
		{"-C", work, "branch", "-M", "main"},
		// Force-push: the new main tip is unrelated to the bundle's tip.
		// pushURL carries embedded basic-auth credentials; redactURLCreds
		// rewrites them to "***:***" before logging so a failed force-push
		// doesn't leak the admin token into test output.
		{"-C", work, "push", "--force", pushURL, "main:main"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		// Only the push args carry an embedded credentialed URL. Apply
		// credSafeGitEnv to it so a pre-set GIT_TRACE_CURL or similar
		// can't cause git to echo pushURL (with admin token) into
		// CombinedOutput. Other args in this loop don't reference
		// pushURL, so leaving their env at the default is fine.
		if len(args) >= 3 && args[2] == "push" {
			cmd.Env = credSafeGitEnv()
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", redactURLCreds(args), err, out)
		}
	}

	// Re-clone with bundle-uri enabled. The clone must succeed; the
	// trace must contain a standard-pack-path indicator. Either the
	// gateway suppresses the now-stale bundle (advertise empty) or the
	// client downloads it and discards it on fingerprint mismatch —
	// either way the pack fetch is what completes the clone.
	cloneDir := filepath.Join(t.TempDir(), "clone")
	cmd := exec.CommandContext(ctx, "git", "clone",
		"-c", "protocol.version=2",
		"-c", "transfer.bundleURI=true",
		repoURL, cloneDir)
	cmd.Env = append(credSafeGitEnv(), "GIT_TRACE2=2")
	var trace strings.Builder
	cmd.Stdout = &trace
	cmd.Stderr = &trace
	if err := cmd.Run(); err != nil {
		t.Fatalf("git clone (post-force-push) failed: %v\n%s", err, trace.String())
	}

	// Assert the cloned HEAD matches the new (divergent) tip — i.e. the
	// clone really did transfer the latest state, not the stale bundle.
	wantHead, err := gitcli.RunForTest(work, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse work: %v: %s", err, wantHead)
	}
	gotHead, err := gitcli.RunForTest(cloneDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse clone: %v: %s", err, gotHead)
	}
	if strings.TrimSpace(string(wantHead)) != strings.TrimSpace(string(gotHead)) {
		t.Fatalf("post-force-push HEAD mismatch: want=%q got=%q\ntrace:\n%s",
			strings.TrimSpace(string(wantHead)),
			strings.TrimSpace(string(gotHead)),
			trace.String())
	}

	traceStr := trace.String()
	// Standard-pack-path indicator: `--keep=fetch-pack <pid> on <host>`
	// is the keepfile argument that git's fetch-pack.c
	// (add_index_pack_keep_option) injects when receiving a pack via the
	// pack-protocol fetch path. Bundle verification ALSO runs index-pack
	// internally, but uses the bundle filename as its --keep payload, not
	// "fetch-pack". So a match on "--keep=fetch-pack" guarantees the
	// standard pack-fetch fallback fired rather than the client silently
	// accepting a stale bundle.
	//
	// Version sensitivity: we match on the `--keep=fetch-pack` prefix
	// only. The `<pid> on <host>` suffix is undocumented and has shifted
	// across git versions — prefix-only matching keeps this assertion
	// robust. The t.Logf of `git --version` at the top of this test
	// makes a future git-version regression easy to spot in CI logs.
	//
	// We don't rely on "Receiving objects" or "remote: Counting" here:
	// neither reliably appears in GIT_TRACE2 for HTTP clones of tiny
	// fixtures against the in-process gateway — both originate from
	// progress reporting that's suppressed without a TTY.
	if !strings.Contains(traceStr, "--keep=fetch-pack") {
		t.Fatalf("GIT_TRACE2 output shows no standard-pack-path indicator; "+
			"fallback may not have fired:\n%s", traceStr)
	}
}
