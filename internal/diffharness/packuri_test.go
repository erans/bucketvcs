package diffharness

// packuri_test.go drives upstream `git clone` against an in-process
// gateway with packfile-uri enabled to exercise the M11 packfile-uri
// end-to-end path.
//
//   - TestPackURI_ClientUsesPackURI: happy path. Maintenance produces a
//     single canonical pack (with PackChecksum set); a fresh clone with
//     `-c fetch.uriProtocols=https` and protocol-v2 succeeds, HEAD
//     matches upstream, and the GIT_TRACE2+GIT_TRACE_CURL trace shows
//     the client actually downloaded a pack from the gateway's
//     `/_pack/<sha1>` proxied route. The proxied path is the only
//     reliable marker that the packfile-uri exchange occurred end-to-end
//     — substring "packfile-uri" could appear from client-config echo,
//     so we need a server-side signal. We use only the positive
//     `/_pack/` URL marker (no negative `--keep=fetch-pack` assertion)
//     because the inline-pack-elision via `--keep-pack` (Phase 10.2)
//     makes the inline pack empty/near-empty; git's argv emission for
//     `--keep=fetch-pack` on a trivial pack varies by version, so a
//     negative test would be brittle across git versions. The positive
//     marker is the unambiguous signal that the gateway's pack-uri path
//     fired.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// packURITestTimeout bounds every `git` child this test spawns. Without
// a per-command deadline, a gateway deadlock during clone or a stuck
// fetch-pack waiting on protocol bytes would block until Go's test
// timeout fires. 60s is generous for these tiny fixtures.
const packURITestTimeout = 60 * time.Second

// startPackURIGateway constructs an in-process gateway with packfile-uri
// enabled (auto mode, proxied-URL fallback wired) and bundle-uri
// disabled, so the only acceleration exercised is pack-uri. Uses the
// same preallocated-listener trick as startBundleURIGateway so
// ProxiedBaseURL can be set before NewServer is called.
//
// As of M19 the gateway computes storage keys directly via
// internal/repo/keys (no ProxiedKeyResolver indirection); the (tenant,
// repo) the URLBuilder embeds in each minted URL identifies the repo.
func startPackURIGateway(t *testing.T, store storage.ObjectStore, authStore auth.Store) *httptest.Server {
	t.Helper()

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
		BundleURIEnabled:     false,
		PackURIEnabled:       true,
		PackURIMode:          gateway.URIModeAuto,
		ProxiedURLSigningKey: signingKey,
		ProxiedBaseURL:       baseURL,
		PackURITTL:           time.Hour,
	})
	if err != nil {
		_ = l.Close()
		t.Fatalf("gateway.NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ts := &httptest.Server{Listener: l, Config: &http.Server{Handler: srv}}
	ts.Start()
	t.Cleanup(ts.Close)
	if ts.URL != baseURL {
		t.Fatalf("ts.URL=%q != baseURL=%q (preallocated-listener trick failed)",
			ts.URL, baseURL)
	}
	return ts
}

// TestPackURI_ClientUsesPackURI drives the happy path: a single
// canonical pack produced by full repack maintenance, advertised via
// the v2 packfile-uris capability, downloaded by upstream `git clone -c
// fetch.uriProtocols=https`.
func TestPackURI_ClientUsesPackURI(t *testing.T) {
	skipIfNoGit(t)

	ctx, cancel := context.WithTimeout(context.Background(), packURITestTimeout)
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

	r, err := repo.Open(ctx, store, tenant, repoID)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	k, err := keys.NewRepo(tenant, repoID)
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	// Force a full repack so the resulting manifest has exactly one
	// canonical pack with PackChecksum set. Without Force, maintenance
	// might no-op on this tiny fixture; without a full repack (i.e. with
	// BundleOnly), the manifest would retain the import-time pack(s)
	// and the EvaluatePackURIAdvertise precondition (single canonical
	// pack) could fail.
	if _, err := maintpkg.Run(ctx, store, r, k, maintpkg.RunOptions{
		Force: true,
	}); err != nil {
		t.Fatalf("maintenance.Run: %v", err)
	}

	// Verify post-maintenance precondition: exactly one canonical pack
	// with a non-empty PackChecksum. This is the gate that
	// EvaluatePackURIAdvertise checks; if it doesn't hold, the
	// gateway will not advertise a packfile-uri and the clone assertion
	// below will fail with a confusing trace. Asserting here puts the
	// failure at the right layer.
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("repo.ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("unmarshal manifest body: %v", err)
	}
	if len(body.Packs) != 1 {
		t.Fatalf("want exactly 1 canonical pack after Force repack, got %d: %+v",
			len(body.Packs), body.Packs)
	}
	if body.Packs[0].PackChecksum == "" {
		t.Fatalf("want PackChecksum non-empty after Force repack, got empty: %+v", body.Packs[0])
	}

	ts := startPackURIGateway(t, store, newDiffharnessAuthStore(t, tenant, repoID))

	// Clone with packfile-uri enabled. Both protocol.version=2 and
	// fetch.uriProtocols=https are required: protocol-v2 is where the
	// packfile-uris capability lives, and the client only honors
	// packfile-uri offers whose scheme is on the uriProtocols allow-list.
	// GIT_TRACE2=2 records subprocess child_start argv (which includes
	// the pack-URL the client downloads to a sideband index-pack), and
	// GIT_TRACE_CURL=1 records the GET URL itself. Both surfaces
	// independently echo the gateway's `/_pack/<sha1>` route.
	cloneDir := filepath.Join(t.TempDir(), "clone")
	cmd := exec.CommandContext(ctx, "git", "clone",
		"-c", "protocol.version=2",
		"-c", "fetch.uriProtocols=https",
		ts.URL+"/"+tenant+"/"+repoID+".git", cloneDir)
	cmd.Env = append(credSafeGitEnv(), "GIT_TRACE2=2", "GIT_TRACE_CURL=1")
	var trace strings.Builder
	cmd.Stdout = &trace
	cmd.Stderr = &trace
	if err := cmd.Run(); err != nil {
		t.Fatalf("git clone failed: %v\n%s", err, trace.String())
	}

	// Correctness: cloned HEAD matches source HEAD.
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

	// Strong server-side marker: the proxied pack URL path. The gateway
	// routes pack downloads under `/_pack/<sha1>`; this path only
	// appears in the trace if the gateway actually advertised a
	// packfile-uri AND the client actually fetched it via HTTP. A
	// regression that disables pack-uri server-side (advertise off,
	// signing off, routing off) emits no `/_pack/` reference — the
	// inline pack alone completes the clone and the test FAILS as
	// required.
	//
	// We deliberately do NOT use `--keep=fetch-pack` as a negative
	// marker (unlike the bundle-uri test). Phase 10.2 wires
	// `--keep-pack=pack-<PackID>.pack` into gitcli.PackObjectsForFetch,
	// so the inline pack is now empty/near-empty when packfile-uri
	// fires. Git's argv emission for `--keep=fetch-pack` on such a
	// trivial pack varies across versions, making a negative assertion
	// brittle. The `/_pack/` positive marker is the unambiguous signal
	// that the gateway's pack-uri path fired.
	//
	// We also avoid relying on the substring "packfile-uri" alone:
	// GIT_TRACE2 emits def_param events for `fetch.uriProtocols=https`
	// and may echo the clone argv, neither of which proves a server-
	// side exchange occurred.
	traceStr := trace.String()
	if !strings.Contains(traceStr, "/_pack/") {
		t.Fatalf("GIT_TRACE2/GIT_TRACE_CURL output does not contain "+
			"`/_pack/` — client never fetched a pack from the "+
			"gateway's proxied route; packfile-uri path not exercised:\n%s",
			traceStr)
	}

	// Forensic logging: surface the trace lines that mention `/_pack/`
	// so a future failure (or a curious reader of a passing run) can
	// see exactly which pkt-line / child_start carried the proxied URL,
	// without grepping the full trace. Capped at 5 lines to keep log
	// output bounded.
	var packLines []string
	for _, line := range strings.Split(traceStr, "\n") {
		if strings.Contains(line, "/_pack/") {
			packLines = append(packLines, line)
			if len(packLines) >= 5 {
				break
			}
		}
	}
	for _, line := range packLines {
		t.Logf("pack-uri trace: %s", line)
	}
}
