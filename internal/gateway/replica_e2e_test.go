package gateway

// Two-bucket end-to-end tests for the M26 read-replica story (spec §26.2).
//
// These drive the GATEWAY layer directly (like the other gateway tests) rather
// than runServe: serve's replica mode requires postgres and production code
// carries no test escape hatch. Serve's flag plumbing is already covered by
// Task 6's validation tests. Here we wire a real replica.Controller over two
// real localfs buckets — a "canonical" write-region bucket and a "regional"
// replica bucket — with provider replication simulated by copying the
// canonical directory tree into the regional one (filepath.WalkDir + copy).
//
// The gateway's store is a fallback.Store (regional over canonical); the
// gate is the live replica.Controller. The injected clock (a shared *time.Time
// the helper advances) lets a test push the controller past its lag budget /
// check TTL without sleeping.
//
// Fetch mechanism: we shell out to the real `git` binary (git clone/fetch
// against the httptest server) — the strongest available assertion that the
// whole object pipeline (advertisement + upload-pack negotiation + pack
// materialization through the fallback store) works. Tests that only need the
// advertisement assert at the HTTP/protocol level (GET info/refs). Real-git
// tests mirror the suite's skip discipline (skipIfNoGit / testing.Short).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/replica"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage/fallback"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// replicaE2EBudget is the lag budget all e2e replicas share. Large enough that
// scenario steps never trip it incidentally; tests trip it by advancing the
// injected clock.
const replicaE2EBudget = 5 * time.Minute

// e2eHarness bundles the two on-disk bucket trees, an injectable clock, and
// constructors for primary + replica gateways over them.
type e2eHarness struct {
	t            *testing.T
	tenant, repo string
	canonDir     string // canonical bucket directory tree
	regDir       string // regional bucket directory tree

	mu     sync.Mutex
	now    time.Time                   // injected clock; advance via advance()
	stores map[string]*localfs.Localfs // dir -> shared handle (localfs locks per-dir)
}

// storeFor opens (once) and returns a shared localfs handle for dir. localfs
// takes an exclusive per-directory lock, so the harness must never open the
// same directory twice — every constructor routes through this cache. Cleanup
// is registered on the root test (h.t) so a handle survives subtests that
// finish before the parent.
func (h *e2eHarness) storeFor(dir string) *localfs.Localfs {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stores == nil {
		h.stores = map[string]*localfs.Localfs{}
	}
	if s, ok := h.stores[dir]; ok {
		return s
	}
	s, err := localfs.Open(dir)
	if err != nil {
		h.t.Fatalf("localfs.Open(%s): %v", dir, err)
	}
	h.stores[dir] = s
	h.t.Cleanup(func() { _ = s.Close() })
	return s
}

// replicaE2E builds the harness: it seeds the canonical bucket with a tiny git
// repo via makeRepoInStore, then replicates it into the regional bucket. The
// returned harness exposes replicate(), advance(), and per-mode server
// constructors. The seeded ref tip is captured from the canonical root
// manifest.
func replicaE2E(t *testing.T) *e2eHarness {
	t.Helper()
	h := &e2eHarness{
		t:        t,
		tenant:   "acme",
		repo:     "demo",
		canonDir: t.TempDir(),
		regDir:   t.TempDir(),
		now:      time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	}
	makeRepoInStore(t, h.canonDir, h.tenant, h.repo)
	h.replicate()
	return h
}

// nowFunc returns the controller clock reader (reads the shared injected time).
func (h *e2eHarness) nowFunc() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

// advance moves the injected clock forward by d.
func (h *e2eHarness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

// replicate copies the entire canonical bucket tree into the regional tree,
// overwriting whatever is there — the localfs analogue of provider
// async replication catching up. Pure Go (filepath.WalkDir + copy).
func (h *e2eHarness) replicate() {
	h.t.Helper()
	copyTree(h.t, h.canonDir, h.regDir)
}

// tipOID reads the default-branch tip OID from a bucket directory's root
// manifest. Used to assert advertisements carry the expected commit.
func (h *e2eHarness) tipOID(t *testing.T, bucketDir string) string {
	t.Helper()
	store := h.storeFor(bucketDir)
	r, err := repo.Open(context.Background(), store, h.tenant, h.repo)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		t.Fatalf("UnmarshalBody: %v", err)
	}
	branch := body.DefaultBranch
	if branch == "" {
		branch = "refs/heads/main"
	}
	oid := body.Refs[branch]
	if oid == "" {
		t.Fatalf("no tip for %s in %s (refs=%v)", branch, bucketDir, body.Refs)
	}
	return oid
}

// newController builds a real replica.Controller over the two buckets in the
// given mode, wired to the harness's injected clock. CheckInterval is tiny so
// the TTL never blocks a scenario step. The two stores it holds are
// independent of the fallback store the gateway serves from.
func (h *e2eHarness) newController(t *testing.T, mode replica.Mode) *replica.Controller {
	t.Helper()
	canon := h.storeFor(h.canonDir)
	reg := h.storeFor(h.regDir)
	return replica.NewController(replica.ControllerConfig{
		Mode:          mode,
		LagBudget:     replicaE2EBudget,
		CheckInterval: time.Millisecond,
		Regional:      reg,
		Canonical:     canon,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:           h.nowFunc,
	})
}

// fallbackStore composes regional over canonical with the routing that matches
// the freshness mode (RootFromRegional for bounded-stale, RootFromCanonical
// for strong-current) — exactly what serve wires.
func (h *e2eHarness) fallbackStore(t *testing.T, mode replica.Mode) *fallback.Store {
	t.Helper()
	canon := h.storeFor(h.canonDir)
	reg := h.storeFor(h.regDir)
	routing := fallback.RootFromRegional
	if mode == replica.ModeStrongCurrent {
		routing = fallback.RootFromCanonical
	}
	return fallback.New(reg, canon, routing, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// replicaServer stands up a replica gateway: fallback store + live controller
// gate + write-region pointer. ctrl is returned so the caller can read its
// Snapshot for /healthz/replica.
func (h *e2eHarness) replicaServer(t *testing.T, mode replica.Mode) (*httptest.Server, *replica.Controller) {
	t.Helper()
	ctrl := h.newController(t, mode)
	store := h.fallbackStore(t, mode)
	srv, err := NewServer(store, Options{
		MirrorDir: t.TempDir(),
		Version:   "test",
		AuthStore: newPermissiveAuthStore(t, h.tenant, h.repo),
		Replica: &replica.GatewayConfig{
			WriteRegionURL: "https://gw-us.example",
			Gate:           ctrl,
			Health:         ctrl.Snapshot,
		},
	})
	if err != nil {
		t.Fatalf("NewServer (replica): %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, ctrl
}

// primaryServer stands up a non-replica gateway over the CANONICAL bucket — a
// real write region. Used to (a) compare advertisements against the replica
// and (b) push a second commit that bumps canonical's manifest_version so the
// lag signal is real.
func (h *e2eHarness) primaryServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := h.storeFor(h.canonDir)
	srv, err := NewServer(store, Options{
		MirrorDir: t.TempDir(),
		Version:   "test",
		AuthStore: newPermissiveAuthStore(t, h.tenant, h.repo),
	})
	if err != nil {
		t.Fatalf("NewServer (primary): %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// repoURL is the .git URL for the harness's seeded repo on the given server.
func (h *e2eHarness) repoURL(ts *httptest.Server) string {
	return ts.URL + "/" + h.tenant + "/" + h.repo + ".git"
}

// pushSecondCommitToCanonical clones the canonical primary, adds a commit on
// the default branch, and pushes it back — bumping the canonical root
// manifest_version. This makes canonical strictly ahead of regional (a REAL
// replication lag) until the next replicate(). Returns the new tip OID.
func (h *e2eHarness) pushSecondCommitToCanonical(t *testing.T) string {
	t.Helper()
	primary := h.primaryServer(t)
	work := filepath.Join(t.TempDir(), "wt2")
	helper := writeCredentialHelper(t, "perm", "perm")

	out, err := gitWithHelper(t, helper, "clone", h.repoURL(primary), work)
	if err != nil {
		t.Fatalf("clone canonical primary: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	mustExecGW(t, work, "git", "add", ".")
	mustExecGW(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "v2")
	if out, err := gitWithHelper(t, helper, "-C", work, "push", "origin", "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("push v2 to canonical: %v\n%s", err, out)
	}
	return h.tipOID(t, h.canonDir)
}

// copyTree recursively copies src into dst (files + dirs), overwriting.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copyTree %s -> %s: %v", src, dst, err)
	}
}

// deletePacks removes every *.pack file under dir; returns the count removed.
func deletePacks(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".pack") {
			if err := os.Remove(path); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("deletePacks under %s: %v", dir, err)
	}
	return n
}

// getInfoRefs performs GET info/refs?service=<svc> against ts (v2 for
// upload-pack) and returns status + body.
func getInfoRefs(t *testing.T, ts *httptest.Server, repoURL, svc string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", repoURL+"/info/refs?service="+svc, nil)
	req.SetBasicAuth("perm", "perm")
	if svc == "git-upload-pack" {
		req.Header.Set("Git-Protocol", "version=2")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET info/refs %s: %v", svc, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// uploadPackTip POSTs a v2 ls-refs to ts and returns the advertised OID for
// refname (or "" if absent). This is a protocol-level "what tip does the
// server advertise" probe that does not depend on cloning.
func uploadPackTip(t *testing.T, ts *httptest.Server, repoURL, refname string) string {
	t.Helper()
	// v2 command: "command=ls-refs\n" delim "peel\n" "ref-prefix <ref>\n" flush.
	var b bytes.Buffer
	writePkt := func(s string) { b.WriteString(pktLine(s)) }
	writePkt("command=ls-refs\n")
	b.WriteString("0001") // delim-pkt
	writePkt("peel\n")
	writePkt("ref-prefix " + refname + "\n")
	b.WriteString("0000") // flush-pkt

	req, _ := http.NewRequest("POST", repoURL+"/git-upload-pack", bytes.NewReader(b.Bytes()))
	req.SetBasicAuth("perm", "perm")
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST upload-pack ls-refs: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	// Each ls-refs line is "<oid> <refname>"; find the one for refname.
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, " "+refname) {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				// strip any leading pkt-line length prefix from the oid field
				oid := fields[0]
				if len(oid) > 40 {
					oid = oid[len(oid)-40:]
				}
				return oid
			}
		}
	}
	return ""
}

// pktLine encodes s as a git pkt-line (4-hex length prefix + payload).
func pktLine(s string) string {
	n := len(s) + 4
	const hex = "0123456789abcdef"
	buf := []byte{hex[(n>>12)&0xf], hex[(n>>8)&0xf], hex[(n>>4)&0xf], hex[n&0xf]}
	return string(buf) + s
}

// -----------------------------------------------------------------------------
// Scenarios
// -----------------------------------------------------------------------------

// TestReplicaE2EFetchBothModes: after replicate(), both strong-current and
// bounded-stale replicas serve the seeded tip in their upload-pack
// advertisement — identical to the primary over canonical.
func TestReplicaE2EFetchBothModes(t *testing.T) {
	skipIfNoGit(t)
	h := replicaE2E(t)
	tip := h.tipOID(t, h.canonDir)

	primary := h.primaryServer(t)
	primTip := uploadPackTip(t, primary, h.repoURL(primary), "refs/heads/main")
	if primTip != tip {
		t.Fatalf("primary advertised tip %q want %q", primTip, tip)
	}

	for _, tc := range []struct {
		name string
		mode replica.Mode
	}{
		{"strong-current", replica.ModeStrongCurrent},
		{"bounded-stale", replica.ModeBoundedStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := h.replicaServer(t, tc.mode)
			status, body := getInfoRefs(t, ts, h.repoURL(ts), "git-upload-pack")
			if status != http.StatusOK {
				t.Fatalf("info/refs status=%d want 200; body=%q", status, body)
			}
			if !bytes.Contains(body, []byte("version 2")) {
				t.Fatalf("advertisement missing 'version 2': %q", body)
			}
			got := uploadPackTip(t, ts, h.repoURL(ts), "refs/heads/main")
			if got != tip {
				t.Fatalf("%s replica advertised tip %q want %q", tc.name, got, tip)
			}
		})
	}
}

// TestReplicaE2EPushRefusedWithPointer: both info/refs?service=git-receive-pack
// and POST git-receive-pack are refused with 403 + "read-only replica" + the
// write-region URL.
func TestReplicaE2EPushRefusedWithPointer(t *testing.T) {
	h := replicaE2E(t)
	ts, _ := h.replicaServer(t, replica.ModeBoundedStale)
	repoURL := h.repoURL(ts)

	// GET info/refs?service=git-receive-pack
	status, body := getInfoRefs(t, ts, repoURL, "git-receive-pack")
	if status != http.StatusForbidden {
		t.Fatalf("info/refs receive-pack status=%d want 403; body=%q", status, body)
	}
	if !strings.Contains(string(body), "read-only replica") || !strings.Contains(string(body), "https://gw-us.example") {
		t.Fatalf("info/refs receive-pack body missing refusal markers: %q", body)
	}

	// POST git-receive-pack
	preq, _ := http.NewRequest("POST", repoURL+"/git-receive-pack", strings.NewReader(""))
	preq.SetBasicAuth("perm", "perm")
	preq.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	presp, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatalf("POST receive-pack: %v", err)
	}
	pbody, _ := io.ReadAll(presp.Body)
	presp.Body.Close()
	if presp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST receive-pack status=%d want 403; body=%q", presp.StatusCode, pbody)
	}
	if !strings.Contains(string(pbody), "read-only replica") || !strings.Contains(string(pbody), "https://gw-us.example") {
		t.Fatalf("POST receive-pack body missing refusal markers: %q", pbody)
	}
}

// TestReplicaE2EFallbackServesMissingPack (the replication-ordering race):
// after replicate(), delete every *.pack under the REGIONAL tree, then a real
// git clone against the bounded-stale replica must SUCCEED — the fallback
// store supplies the missing packs from canonical. A plain GET info/refs still
// 200s.
func TestReplicaE2EFallbackServesMissingPack(t *testing.T) {
	skipIfNoGit(t)
	h := replicaE2E(t)
	removed := deletePacks(t, h.regDir)
	if removed < 1 {
		t.Fatalf("expected >=1 pack under regional tree, removed %d (seed produced no pack?)", removed)
	}

	ts, _ := h.replicaServer(t, replica.ModeBoundedStale)
	repoURL := h.repoURL(ts)

	// Plain GET info/refs still 200s (root manifest read served fine).
	status, body := getInfoRefs(t, ts, repoURL, "git-upload-pack")
	if status != http.StatusOK {
		t.Fatalf("info/refs status=%d want 200; body=%q", status, body)
	}

	// Real clone must succeed via canonical fallback for the pack bytes.
	dst := filepath.Join(t.TempDir(), "clone")
	helper := writeCredentialHelper(t, "perm", "perm")
	out, err := gitWithHelper(t, helper, "clone", repoURL, dst)
	if err != nil {
		t.Fatalf("clone against fallback replica failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "a.txt")); err != nil {
		t.Fatalf("expected a.txt in fallback clone: %v", err)
	}
}

// TestReplicaE2EBoundedStaleUnhealthyAndRecovery: push a second commit to
// canonical ONLY (regional stays behind), advance past the lag budget → the
// bounded-stale replica reports 503 "replica unhealthy" and /healthz/replica
// shows repos_lagging >= 1. Then replicate() (regional catches up), advance
// past the check TTL → 200 again and the NEW tip is advertised.
func TestReplicaE2EBoundedStaleUnhealthyAndRecovery(t *testing.T) {
	skipIfNoGit(t)
	h := replicaE2E(t)
	oldTip := h.tipOID(t, h.canonDir)

	// Canonical moves ahead; regional stays at the seeded state.
	newTip := h.pushSecondCommitToCanonical(t)
	if newTip == oldTip {
		t.Fatalf("second push did not advance the tip (%s)", newTip)
	}

	ts, _ := h.replicaServer(t, replica.ModeBoundedStale)
	repoURL := h.repoURL(ts)

	// First touch: tracks the repo, samples lag (canonical=3 > regional=2 →
	// laggedSince set). Still healthy because budget not yet exceeded.
	if status, body := getInfoRefs(t, ts, repoURL, "git-upload-pack"); status != http.StatusOK {
		t.Fatalf("pre-budget info/refs status=%d want 200; body=%q", status, body)
	}

	// Advance past the lag budget → next advertise marks unhealthy.
	h.advance(replicaE2EBudget + time.Minute)
	status, body := getInfoRefs(t, ts, repoURL, "git-upload-pack")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("over-budget info/refs status=%d want 503; body=%q", status, body)
	}
	if !strings.Contains(string(body), "replica unhealthy") {
		t.Fatalf("over-budget body missing 'replica unhealthy': %q", body)
	}

	// /healthz/replica shows the lagging repo.
	snap := getHealthzReplica(t, ts)
	if snap.ReposLagging < 1 {
		t.Fatalf("healthz repos_lagging=%d want >=1 (snap=%+v)", snap.ReposLagging, snap)
	}
	if snap.Mode != "bounded-stale" {
		t.Fatalf("healthz mode=%q want bounded-stale", snap.Mode)
	}

	// Regional catches up; advance past the check TTL so the next advertise
	// re-samples and clears the lag.
	h.replicate()
	h.advance(time.Second) // > CheckInterval (1ms)
	status, body = getInfoRefs(t, ts, repoURL, "git-upload-pack")
	if status != http.StatusOK {
		t.Fatalf("post-recovery info/refs status=%d want 200; body=%q", status, body)
	}
	gotTip := uploadPackTip(t, ts, repoURL, "refs/heads/main")
	if gotTip != newTip {
		t.Fatalf("post-recovery advertised tip %q want new tip %q", gotTip, newTip)
	}
}

// TestReplicaE2EStrongCurrentSeesNewCommitsImmediately: with canonical ahead
// of regional (v2 pushed to canonical, NOT replicated), the strong-current
// replica advertises the NEW tip immediately (root read routes to canonical),
// and a full real-git fetch of that new tip succeeds — the fallback supplies
// the unreplicated pack from canonical.
func TestReplicaE2EStrongCurrentSeesNewCommitsImmediately(t *testing.T) {
	skipIfNoGit(t)
	h := replicaE2E(t)
	newTip := h.pushSecondCommitToCanonical(t) // canonical only; regional stale

	ts, _ := h.replicaServer(t, replica.ModeStrongCurrent)
	repoURL := h.repoURL(ts)

	status, body := getInfoRefs(t, ts, repoURL, "git-upload-pack")
	if status != http.StatusOK {
		t.Fatalf("strong-current info/refs status=%d want 200; body=%q", status, body)
	}
	got := uploadPackTip(t, ts, repoURL, "refs/heads/main")
	if got != newTip {
		t.Fatalf("strong-current advertised tip %q want new tip %q (regional is stale)", got, newTip)
	}

	// Full clone: must succeed even though the v2 pack lives only in canonical.
	dst := filepath.Join(t.TempDir(), "clone")
	helper := writeCredentialHelper(t, "perm", "perm")
	out, err := gitWithHelper(t, helper, "clone", repoURL, dst)
	if err != nil {
		t.Fatalf("strong-current clone failed: %v\n%s", err, out)
	}
	// b.txt is the v2 file — proves the unreplicated commit's tree+blob came
	// through the fallback.
	if _, err := os.Stat(filepath.Join(dst, "b.txt")); err != nil {
		t.Fatalf("expected b.txt (v2 content) in strong-current clone: %v", err)
	}
}

// TestReplicaE2EHealthz: GET /healthz → 200 "ok"; /healthz/replica → JSON
// role=replica with mode matching the configured mode.
func TestReplicaE2EHealthz(t *testing.T) {
	h := replicaE2E(t)
	for _, tc := range []struct {
		mode     replica.Mode
		wantMode string
	}{
		{replica.ModeStrongCurrent, "strong-current"},
		{replica.ModeBoundedStale, "bounded-stale"},
	} {
		t.Run(tc.wantMode, func(t *testing.T) {
			ts, _ := h.replicaServer(t, tc.mode)

			resp, err := http.Get(ts.URL + "/healthz")
			if err != nil {
				t.Fatalf("GET /healthz: %v", err)
			}
			hbody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(hbody), "ok") {
				t.Fatalf("/healthz status=%d body=%q want 200 ok", resp.StatusCode, hbody)
			}

			snap := getHealthzReplica(t, ts)
			if snap.Role != "replica" {
				t.Fatalf("healthz role=%q want replica", snap.Role)
			}
			if snap.Mode != tc.wantMode {
				t.Fatalf("healthz mode=%q want %q", snap.Mode, tc.wantMode)
			}
		})
	}
}

// getHealthzReplica GETs /healthz/replica and decodes the JSON snapshot.
func getHealthzReplica(t *testing.T, ts *httptest.Server) replica.HealthSnapshot {
	t.Helper()
	resp, err := http.Get(ts.URL + "/healthz/replica")
	if err != nil {
		t.Fatalf("GET /healthz/replica: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz/replica status=%d want 200", resp.StatusCode)
	}
	var snap replica.HealthSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode healthz/replica: %v", err)
	}
	return snap
}
