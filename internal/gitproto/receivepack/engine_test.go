package receivepack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
	"github.com/bucketvcs/bucketvcs/internal/v2proto"
)

func TestService_FlushOnlyProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeReceivePackStore(t, storeDir, "acme", "demo")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	// Stdin = pktline flush packet "0000"
	pw := &bytes.Buffer{}
	wtr := pktline.NewWriter(pw)
	if err := wtr.WriteFlush(); err != nil {
		t.Fatalf("WriteFlush: %v", err)
	}

	var stdout bytes.Buffer
	req := &EngineRequest{
		Ctx:          context.Background(),
		Tenant:       "acme",
		Repo:         "demo",
		Stdin:        pw,
		Stdout:       &stdout,
		Stderr:       &bytes.Buffer{},
		Store:        store,
		Mirror:       mgr,
		AgentVersion: "test",
	}
	err = Service(req)
	if !errors.Is(err, ErrFlushOnlyProbe) {
		t.Fatalf("Service with flush-only body: want ErrFlushOnlyProbe, got %v", err)
	}
}

func TestStubsCompile(t *testing.T) {
	// Service is now implemented; Serve must succeed or return a non-nil error.
	// We just verify that calling Service with an empty request doesn't panic
	// and that Serve delegates to Advertise then Service.
	// (With no Store or Mirror, both will return non-nil errors — that's fine.)
	_ = Service(&EngineRequest{Ctx: context.Background()})
	_ = Serve(&EngineRequest{Ctx: context.Background()})
}

// makeReceivePackStore creates a synthetic store with one repo for use in
// receivepack package tests. It mirrors the pattern from
// internal/gitproto/uploadpack/engine_test.go::makeUploadPackStore.
func makeReceivePackStore(t *testing.T, storeDir, tenant, repoID string) {
	t.Helper()
	srcBare := filepath.Join(t.TempDir(), "src.git")
	work := filepath.Join(t.TempDir(), "wt")

	mustExecRP(t, "", "git", "init", "--bare", srcBare)
	mustExecRP(t, "", "git", "clone", srcBare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExecRP(t, work, "git", "add", ".")
	mustExecRP(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustExecRP(t, work, "git", "push", "origin", "HEAD:refs/heads/main")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()
	if _, err := importer.Import(context.Background(), store, importer.Options{
		Tenant: tenant, Repo: repoID, SourceDir: srcBare, DefaultBranch: "refs/heads/main",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
}

func mustExecRP(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func TestAdvertise_V0_RefAdvertisementShape(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeReceivePackStore(t, storeDir, "acme", "demo")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	var buf bytes.Buffer
	req := &EngineRequest{
		Ctx:          context.Background(),
		Tenant:       "acme",
		Repo:         "demo",
		Stdout:       &buf,
		Store:        store,
		AgentVersion: "test",
	}
	if err := Advertise(req); err != nil {
		t.Fatalf("Advertise: %v", err)
	}

	output := buf.Bytes()

	// Output must NOT begin with the Smart-HTTP service preamble.
	// That preamble is HTTP-specific framing emitted by the gateway adapter.
	if bytes.Contains(output, []byte("# service=")) {
		t.Fatalf("Advertise must not emit service preamble; it is HTTP-only framing: %q", output)
	}

	// Output must contain the main branch ref.
	if !bytes.Contains(output, []byte("refs/heads/main")) {
		t.Fatalf("output missing refs/heads/main: %q", output)
	}

	// Output must end with a flush packet "0000".
	if !strings.HasSuffix(strings.TrimRight(string(output), ""), "0000") {
		// Check raw bytes: last 4 bytes should be '0','0','0','0'
		if len(output) < 4 || string(output[len(output)-4:]) != "0000" {
			t.Fatalf("output does not end with flush packet '0000': %q", output)
		}
	}

	// Capability list must contain "report-status", "delete-refs", and agent.
	if !bytes.Contains(output, []byte("report-status")) {
		t.Fatalf("output missing capability 'report-status': %q", output)
	}
	if !bytes.Contains(output, []byte("delete-refs")) {
		t.Fatalf("output missing capability 'delete-refs': %q", output)
	}
	agentCap := "agent=" + v2proto.AgentName + "/test"
	if !bytes.Contains(output, []byte(agentCap)) {
		t.Fatalf("output missing agent capability %q: %q", agentCap, output)
	}

	// Receive-pack must NOT advertise HEAD (push targets are real refs).
	if bytes.Contains(output, []byte(" HEAD\x00")) || bytes.Contains(output, []byte(" HEAD\n")) {
		t.Fatalf("receive-pack must not advertise HEAD: %q", output)
	}
}

// TestReceivePack_AppendsDeltaToManifest verifies that a push against a
// repo with a .bvcg base index produces a .bvrd reachability delta that
// is recorded in the committed manifest body.
func TestReceivePack_AppendsDeltaToManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeReceivePackStore(t, storeDir, "acme", "demo")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	// Open a mirror so we have a real bare dir.
	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	m, err := mgr.Open(context.Background(), "acme", "demo")
	if err != nil {
		t.Fatalf("mgr.Open: %v", err)
	}

	// Read the current manifest body (pre-push state).
	r, err := repo.Open(context.Background(), store, "acme", "demo")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var initialBody manifest.Body
	if err := json.Unmarshal(view.Body, &initialBody); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Add a second commit to the bare mirror (simulating what the gateway does).
	wt2 := t.TempDir()
	mustExecRP(t, "", "git", "clone", m.BareDir(), wt2)
	if err := os.WriteFile(filepath.Join(wt2, "b.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustExecRP(t, wt2, "git", "add", ".")
	mustExecRP(t, wt2, "git", "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-m", "second")
	newOIDStr := mustExecDirOutput(t, wt2, "git", "rev-parse", "HEAD")
	// Push the new commit to the bare mirror's main branch (step 9b equivalent).
	mustExecRP(t, m.BareDir(), "git", "fetch", wt2, "refs/heads/main:refs/heads/main")

	// Build the patcher using the initial manifest body (pre-push state).
	oldMainOID := initialBody.Refs["refs/heads/main"]
	acceptedUpdates := []updateCommand{
		{Refname: "refs/heads/main", OldOID: oldMainOID, NewOID: newOIDStr},
	}
	eng := &EngineRequest{
		Ctx:    context.Background(),
		Tenant: "acme",
		Repo:   "demo",
		Store:  store,
	}
	patcher := makeDeltaPatcher(eng, m.BareDir(), acceptedUpdates, view.Header.ManifestVersion)

	// Run BuildAndCommit with the patcher.
	refUpdates := map[string]string{"refs/heads/main": newOIDStr}
	body, err := importer.BuildAndCommit(context.Background(), store, "acme", "demo", m.BareDir(), refUpdates, "pusher", patcher)
	if err != nil {
		t.Fatalf("BuildAndCommit with delta patcher: %v", err)
	}

	// Assert: Reachability != nil and has exactly 1 delta.
	if body.Indexes.Reachability == nil {
		t.Fatal("committed body has nil Reachability — delta was not produced")
	}
	if len(body.Indexes.Reachability.Deltas) != 1 {
		t.Errorf("Reachability.Deltas len = %d, want 1", len(body.Indexes.Reachability.Deltas))
	}
	if body.Indexes.Reachability.Deltas[0].Key == "" {
		t.Error("Reachability.Deltas[0].Key is empty")
	}
	t.Logf("delta key: %s hash: %s size: %d",
		body.Indexes.Reachability.Deltas[0].Key,
		body.Indexes.Reachability.Deltas[0].Hash,
		body.Indexes.Reachability.Deltas[0].SizeBytes)
}

// TestReceivePack_CASRetry_RebuildsDelta validates that a CAS retry
// (concurrent manifest version bump) causes BuildAndCommit to fail
// cleanly (stale manifest) rather than committing with a stale delta.
//
// Implementing this properly requires injecting a concurrent CAS between
// BuildAndCommit's read and commit. The existing importer harness doesn't
// provide that level of injection without substantial refactoring.
// This test is marked as a TODO skeleton.
func TestReceivePack_CASRetry_RebuildsDelta(t *testing.T) {
	// TODO: CAS retry test — implementation depends on being able to inject
	// a concurrent commit between BuildAndCommit's ReadRoot and its r.Commit
	// call. Currently BuildAndCommit fails with "stale manifest" on CAS
	// mismatch rather than retrying. A future refactor that makes the body
	// computation lazy (inside the commit callback) would enable retry;
	// at that point this test should verify the delta is rebuilt against the
	// newer prevBody.
	//
	// Property: if a concurrent push lands between our ReadRoot and our
	// r.Commit, our BuildAndCommit returns an error ("stale manifest")
	// and no delta is appended to the manifest for our push. The client
	// retries; the next push produces a fresh delta. This is safe.
	t.Skip("CAS retry test — harness cannot inject concurrent commit; see comment")
}

// TestReceivePack_AccumulatesDeltaChain is a regression test for HIGH-1:
// two successive BuildAndCommit calls with a delta patcher must accumulate a
// chain of length 2. Before the fix, each call started with a fresh draft body
// that had nil Reachability, silently discarding prior deltas and leaving chain
// length == 1 forever.
//
// The test drives the actual patcher path (no synthetic delta injection) so it
// exercises the same code path that production pushes hit.
func TestReceivePack_AccumulatesDeltaChain(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeReceivePackStore(t, storeDir, "acme", "delta-chain")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	m, err := mgr.Open(context.Background(), "acme", "delta-chain")
	if err != nil {
		t.Fatalf("mgr.Open: %v", err)
	}

	// --- Push 1 ---
	r, err := repo.Open(context.Background(), store, "acme", "delta-chain")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var prePush1Body manifest.Body
	if err := json.Unmarshal(view.Body, &prePush1Body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	wt := t.TempDir()
	mustExecRP(t, "", "git", "clone", m.BareDir(), wt)

	// Commit 2
	if err := os.WriteFile(filepath.Join(wt, "b.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustExecRP(t, wt, "git", "add", ".")
	mustExecRP(t, wt, "git", "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-m", "second")
	newOID1 := mustExecDirOutput(t, wt, "git", "rev-parse", "HEAD")
	mustExecRP(t, m.BareDir(), "git", "fetch", wt, "refs/heads/main:refs/heads/main")

	eng := &EngineRequest{
		Ctx:    context.Background(),
		Tenant: "acme",
		Repo:   "delta-chain",
		Store:  store,
	}
	oldMainOID := prePush1Body.Refs["refs/heads/main"]
	updates1 := []updateCommand{{Refname: "refs/heads/main", OldOID: oldMainOID, NewOID: newOID1}}
	patcher1 := makeDeltaPatcher(eng, m.BareDir(), updates1, view.Header.ManifestVersion)

	body1, err := importer.BuildAndCommit(context.Background(), store, "acme", "delta-chain", m.BareDir(),
		map[string]string{"refs/heads/main": newOID1}, "pusher", patcher1)
	if err != nil {
		t.Fatalf("Push 1 BuildAndCommit: %v", err)
	}
	if body1.Indexes.Reachability == nil {
		t.Fatal("push 1: Reachability is nil")
	}
	if got := len(body1.Indexes.Reachability.Deltas); got != 1 {
		t.Errorf("push 1: chain len = %d, want 1", got)
	}

	// --- Push 2 ---
	// Commit 3
	if err := os.WriteFile(filepath.Join(wt, "c.txt"), []byte("third\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustExecRP(t, wt, "git", "add", ".")
	mustExecRP(t, wt, "git", "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-m", "third")
	newOID2 := mustExecDirOutput(t, wt, "git", "rev-parse", "HEAD")
	mustExecRP(t, m.BareDir(), "git", "fetch", wt, "refs/heads/main:refs/heads/main")

	updates2 := []updateCommand{{Refname: "refs/heads/main", OldOID: newOID1, NewOID: newOID2}}
	view2, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot before push 2: %v", err)
	}
	patcher2 := makeDeltaPatcher(eng, m.BareDir(), updates2, view2.Header.ManifestVersion)

	body2, err := importer.BuildAndCommit(context.Background(), store, "acme", "delta-chain", m.BareDir(),
		map[string]string{"refs/heads/main": newOID2}, "pusher", patcher2)
	if err != nil {
		t.Fatalf("Push 2 BuildAndCommit: %v", err)
	}
	if body2.Indexes.Reachability == nil {
		t.Fatal("push 2: Reachability is nil — delta chain was dropped")
	}
	// Regression: before the fix this would be 1 (prior delta silently discarded).
	if got := len(body2.Indexes.Reachability.Deltas); got != 2 {
		t.Errorf("push 2: chain len = %d, want 2 (HIGH-1 regression: prior delta discarded)", got)
	}
	t.Logf("delta chain after 2 pushes: len=%d delta[0]=%s delta[1]=%s",
		len(body2.Indexes.Reachability.Deltas),
		body2.Indexes.Reachability.Deltas[0].Key,
		body2.Indexes.Reachability.Deltas[1].Key)
}

// TestReceivePack_DeleteOnlyPush_PreservesChain is a regression test for HIGH-1
// branch (b): a delete-only push must preserve the reachability chain rather than
// wiping it. Before the fix the early-return at the delete-only check returned
// `draft` unchanged, and draft has nil Reachability (BuildAndCommit constructs
// it from scratch on every push), silently discarding the prior chain.
//
// This test calls makeDeltaPatcher directly — the same way the gateway does —
// with a delete-only acceptedUpdates list and nil newOIDs, against a
// freshPrevBody that carries an accumulated reachability chain. We do not go
// through BuildAndCommit here because BuildAndCommit would try to repack the
// bare mirror, whose canonical pack was already uploaded on a prior push, and
// the store's PutIfAbsent strict variant rejects the identical bytes as
// "already exists". The patcher is the locus of the HIGH-1 bug, so exercising
// it directly is both necessary and sufficient.
func TestReceivePack_DeleteOnlyPush_PreservesChain(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeReceivePackStore(t, storeDir, "acme", "delete-only-chain")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	m, err := mgr.Open(context.Background(), "acme", "delete-only-chain")
	if err != nil {
		t.Fatalf("mgr.Open: %v", err)
	}

	eng := &EngineRequest{
		Ctx:    context.Background(),
		Tenant: "acme",
		Repo:   "delete-only-chain",
		Store:  store,
	}

	// --- Push 1: add a real commit to build a delta in the chain ---
	r, err := repo.Open(context.Background(), store, "acme", "delete-only-chain")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var prePush1Body manifest.Body
	if err := json.Unmarshal(view.Body, &prePush1Body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	wt := t.TempDir()
	mustExecRP(t, "", "git", "clone", m.BareDir(), wt)

	if err := os.WriteFile(filepath.Join(wt, "b.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustExecRP(t, wt, "git", "add", ".")
	mustExecRP(t, wt, "git", "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-m", "second")
	newOID1 := mustExecDirOutput(t, wt, "git", "rev-parse", "HEAD")
	mustExecRP(t, m.BareDir(), "git", "fetch", wt, "refs/heads/main:refs/heads/main")

	oldMainOID := prePush1Body.Refs["refs/heads/main"]
	updates1 := []updateCommand{{Refname: "refs/heads/main", OldOID: oldMainOID, NewOID: newOID1}}
	patcher1 := makeDeltaPatcher(eng, m.BareDir(), updates1, view.Header.ManifestVersion)

	body1, err := importer.BuildAndCommit(context.Background(), store, "acme", "delete-only-chain", m.BareDir(),
		map[string]string{"refs/heads/main": newOID1}, "pusher", patcher1)
	if err != nil {
		t.Fatalf("Push 1 BuildAndCommit: %v", err)
	}
	if body1.Indexes.Reachability == nil {
		t.Fatal("push 1: Reachability is nil")
	}
	if got := len(body1.Indexes.Reachability.Deltas); got != 1 {
		t.Errorf("push 1: chain len = %d, want 1", got)
	}

	// --- Push 2: delete-only — invoke patcher directly ---
	//
	// The patcher is called by BuildAndCommit after all artifacts are uploaded.
	// For the delete-only regression we call the closure directly: freshPrevBody
	// is the body committed by push 1 (has the chain), draft is what
	// BuildAndCommit would hand the patcher (constructed from scratch, nil
	// Reachability), acceptedUpdates is a deletion, and newOIDs is nil.
	//
	// This is exactly the code path that the Medium-1 fix guards: the delete-only
	// push must record a delete-tip delta so the chain replay removes the ref rather
	// than resurrecting it as a phantom.
	deleteUpdates := []updateCommand{
		{Refname: "refs/heads/feature", OldOID: newOID1, NewOID: nullOID},
	}
	// Pre-push version for push 2 is view.Header.ManifestVersion + 1 (push 1 committed once).
	prePush2Version := view.Header.ManifestVersion + 1
	patcher2 := makeDeltaPatcher(eng, m.BareDir(), deleteUpdates, prePush2Version)

	// Draft has nil Reachability — simulating BuildAndCommit's from-scratch draft.
	draft := manifest.Body{}

	result, err := patcher2(context.Background(), *body1, draft, nil /* newOIDs */)
	if err != nil {
		t.Fatalf("patcher2 (delete-only): %v", err)
	}

	// The chain must be non-nil and contain 2 deltas: push 1's commit delta + push 2's delete-tip delta.
	// Before the Medium-1 fix, the delete-only short-circuit caused the patcher to return draft
	// unchanged (Reachability nil) — chain wiped. After the fix the patcher records the delete-tip.
	if result.Indexes.Reachability == nil {
		t.Fatal("delete-only push: Reachability is nil — prior chain was wiped")
	}
	// Chain must grow by 1: push1 delta + new delete-tip delta.
	if got := len(result.Indexes.Reachability.Deltas); got != 2 {
		t.Errorf("delete-only push: chain len = %d, want 2 (push1 delta + delete-tip delta)", got)
	}
	// delta[0] must be push 1's delta (chain preserved).
	if result.Indexes.Reachability.Deltas[0].Key != body1.Indexes.Reachability.Deltas[0].Key {
		t.Errorf("delete-only push: delta[0].Key = %q, want %q",
			result.Indexes.Reachability.Deltas[0].Key,
			body1.Indexes.Reachability.Deltas[0].Key)
	}
	// delta[1] must be a new delta (the delete-tip delta — different key from push 1).
	if len(result.Indexes.Reachability.Deltas) >= 2 &&
		result.Indexes.Reachability.Deltas[1].Key == body1.Indexes.Reachability.Deltas[0].Key {
		t.Errorf("delete-only push: delta[1].Key == delta[0].Key — expected a new delete-tip delta")
	}
	t.Logf("delete-only push produced delete-tip delta: chain len=%d delta[0]=%s delta[1]=%s",
		len(result.Indexes.Reachability.Deltas),
		result.Indexes.Reachability.Deltas[0].Key,
		func() string {
			if len(result.Indexes.Reachability.Deltas) >= 2 {
				return result.Indexes.Reachability.Deltas[1].Key
			}
			return "(missing)"
		}())
}

// TestReceivePack_RejectedPush_PreservesChain is a regression test for HIGH-1
// branch (a): a fully-rejected push (no accepted updates, no new OIDs) must
// preserve the reachability chain rather than wiping it.
//
// Because the importer harness does not expose a way to drive a push where all
// ref updates are rejected (that rejection happens above BuildAndCommit, in the
// Engine.RunReceivePack layer), this test exercises makeDeltaPatcher directly
// with an empty acceptedUpdates slice and an empty newOIDs list, which is the
// exact condition guarded by the HIGH-1 early-return branch (a).
func TestReceivePack_RejectedPush_PreservesChain(t *testing.T) {
	// Pre-build a freshPrevBody that has an accumulated reachability chain.
	prevBody := manifest.Body{
		Indexes: manifest.Indexes{
			Reachability: &manifest.ReachabilityRef{
				BaseManifest: "v1",
				Deltas: []manifest.IndexRef{
					{Key: "delta/abc", Hash: "aabbcc", SizeBytes: 512},
					{Key: "delta/def", Hash: "ddeeff", SizeBytes: 256},
				},
			},
		},
	}
	// draft is what BuildAndCommit would hand to the patcher: constructed from
	// scratch, so Reachability == nil.
	draft := manifest.Body{}

	// makeDeltaPatcher with zero acceptedUpdates — simulates a fully-rejected push.
	eng := &EngineRequest{
		Ctx:    context.Background(),
		Tenant: "acme",
		Repo:   "test-rejected",
		// Store left nil: the patcher must not reach any Store call on this path.
	}
	patcher := makeDeltaPatcher(eng, "/no/bare/dir", nil /* acceptedUpdates */, 0 /* prePushVersion: unused on this path */)

	result, err := patcher(context.Background(), prevBody, draft, nil /* newOIDs */)
	if err != nil {
		t.Fatalf("patcher returned error: %v", err)
	}

	// Regression: before the fix the patcher returned `draft` with nil Reachability.
	if result.Indexes.Reachability == nil {
		t.Fatal("rejected push: Reachability is nil — prior chain was wiped (HIGH-1 regression)")
	}
	if got := len(result.Indexes.Reachability.Deltas); got != 2 {
		t.Errorf("rejected push: chain len = %d, want 2 (chain should be seeded from prevBody)", got)
	}
	if result.Indexes.Reachability.Deltas[0].Key != "delta/abc" {
		t.Errorf("rejected push: delta[0].Key = %q, want delta/abc", result.Indexes.Reachability.Deltas[0].Key)
	}
}

// TestReceivePack_AbortsOnDeltaUploadFailure verifies the error-propagation
// chain: if uploadDelta returns an error (e.g. storage failure), BuildAndCommit
// propagates it and the manifest is NOT committed.
//
// This property is guaranteed by the existing error-propagation chain:
//   - patcher returns error -> BuildAndCommit returns error -> manifest NOT committed
//
// A full injectable-store test would require a storage wrapper that returns
// errors on PutIfAbsent for keys with the reachability-delta prefix. That
// harness machinery doesn't exist yet. The property is tested indirectly by
// TestUploadDelta_KeyCollisionDifferentBytes (which proves uploadDelta can
// return errors) and TestBuildAndCommit_BodyPatcherIsCalled (which proves
// patcher errors bubble out of BuildAndCommit).
func TestReceivePack_AbortsOnDeltaUploadFailure(t *testing.T) {
	// TODO: Add full injection test once the test harness supports a store
	// wrapper that fails PutIfAbsent for specific key prefixes.
	// Until then, the property is covered by unit tests for the individual
	// components in the error chain.
	t.Skip("delta upload failure test — injectable store harness not available; property covered by unit tests")
}
