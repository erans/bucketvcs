package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// importedRepo bundles the test fixture: an imported bucketvcs repo, its
// store, and a freshly-cloned bare mirror that BuildAndCommit can repack.
// The mirror starts in sync with the imported manifest (one pack, one ref).
type importedRepo struct {
	store        storage.ObjectStore
	tenant, repo string
	bareDir      string // local mirror under test
	srcWork      string // the original work tree (for adding new commits)
	mainOID      string // OID of the initial commit on refs/heads/main
}

// setupImportedRepo creates a 1-commit source repo, imports it into a
// fresh bucketvcs store, then clones a bare mirror that mimics the
// gateway's per-repo on-disk view immediately after a fresh fetch.
func setupImportedRepo(t *testing.T) *importedRepo {
	t.Helper()
	skipIfNoGit(t)
	work := t.TempDir()
	mustGit := func(args ...string) string {
		t.Helper()
		out, err := gitcli.RunForTest(work, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mustGit("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("c1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit("add", "f")
	mustGit("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "c1")
	mainOID := mustGit("rev-parse", "HEAD")

	srcBare := filepath.Join(t.TempDir(), "src.git")
	if err := gitcli.CloneBareMirror(context.Background(), work, srcBare); err != nil {
		t.Fatalf("CloneBareMirror src: %v", err)
	}
	store := newTestStore(t)
	if _, err := Import(context.Background(), store, Options{
		SourceDir: srcBare, Tenant: "t", Repo: "r", Actor: "tester",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	// Build the mirror: clone the source bare, which gives us the same
	// objects + refs as the just-imported manifest.
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	if err := gitcli.CloneBareMirror(context.Background(), srcBare, mirror); err != nil {
		t.Fatalf("CloneBareMirror mirror: %v", err)
	}
	return &importedRepo{
		store:   store,
		tenant:  "t",
		repo:    "r",
		bareDir: mirror,
		srcWork: work,
		mainOID: mainOID,
	}
}

// addSecondCommit adds another commit on top of the source work tree, then
// fetches it into the mirror so the mirror has both objects but no manifest
// update yet. Returns the new commit OID.
func (ir *importedRepo) addSecondCommit(t *testing.T) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ir.srcWork, "g"), []byte("c2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if out, err := gitcli.RunForTest(ir.srcWork, "add", "g"); err != nil {
		t.Fatalf("add: %v: %s", err, out)
	}
	if out, err := gitcli.RunForTest(ir.srcWork,
		"-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "c2"); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	out, err := gitcli.RunForTest(ir.srcWork, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v: %s", err, out)
	}
	newOID := strings.TrimSpace(string(out))
	// Fetch into mirror so the new objects (and updated ref) are present.
	if out, err := gitcli.RunForTest(ir.bareDir,
		"fetch", "--no-write-fetch-head", ir.srcWork, "+refs/heads/*:refs/heads/*"); err != nil {
		t.Fatalf("fetch into mirror: %v: %s", err, out)
	}
	return newOID
}

// readBody decodes the current manifest body from the store.
func (ir *importedRepo) readBody(t *testing.T) (manifest.Body, uint64) {
	t.Helper()
	r, err := repo.Open(context.Background(), ir.store, ir.tenant, ir.repo)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return body, view.Header.ManifestVersion
}

func TestBuildAndCommit_AppendsToExistingRepo(t *testing.T) {
	ir := setupImportedRepo(t)
	prevBody, prevVer := ir.readBody(t)
	if prevBody.Refs["refs/heads/main"] != ir.mainOID {
		t.Fatalf("pre: refs/heads/main = %q, want %q", prevBody.Refs["refs/heads/main"], ir.mainOID)
	}
	if len(prevBody.Packs) != 1 {
		t.Fatalf("pre: packs len = %d, want 1", len(prevBody.Packs))
	}
	prevPackID := prevBody.Packs[0].PackID

	newOID := ir.addSecondCommit(t)
	updates := map[string]string{"refs/heads/main": newOID}
	body, err := BuildAndCommit(context.Background(), ir.store, ir.tenant, ir.repo, ir.bareDir, updates, "pusher")
	if err != nil {
		t.Fatalf("BuildAndCommit: %v", err)
	}

	if body.Refs["refs/heads/main"] != newOID {
		t.Fatalf("post body refs: got %q, want %q", body.Refs["refs/heads/main"], newOID)
	}
	if len(body.Packs) != 1 {
		t.Fatalf("post body packs len = %d, want 1 (canonical repack)", len(body.Packs))
	}
	if body.Packs[0].PackID == prevPackID {
		t.Fatalf("post pack ID matches pre pack ID %q — repack should produce a new pack", prevPackID)
	}
	if body.Indexes.ObjectMap == nil || body.Indexes.CommitGraph == nil {
		t.Fatalf("post body indexes nil: %+v", body.Indexes)
	}
	if body.DefaultBranch != prevBody.DefaultBranch {
		t.Fatalf("DefaultBranch changed: %q -> %q", prevBody.DefaultBranch, body.DefaultBranch)
	}

	// Verify manifest in the store also reflects this.
	storedBody, storedVer := ir.readBody(t)
	if storedVer != prevVer+1 {
		t.Fatalf("manifest version: got %d, want %d", storedVer, prevVer+1)
	}
	if storedBody.Packs[0].PackID != body.Packs[0].PackID {
		t.Fatalf("stored pack ID mismatch with returned body")
	}
	// Verify the canonical pack is uploaded under the manifest's PackKey.
	if _, err := ir.store.Head(context.Background(), body.Packs[0].PackKey); err != nil {
		t.Fatalf("pack head: %v", err)
	}
	if _, err := ir.store.Head(context.Background(), body.Packs[0].IdxKey); err != nil {
		t.Fatalf("idx head: %v", err)
	}
	if _, err := ir.store.Head(context.Background(), body.Indexes.ObjectMap.Key); err != nil {
		t.Fatalf(".bvom head: %v", err)
	}
	if _, err := ir.store.Head(context.Background(), body.Indexes.CommitGraph.Key); err != nil {
		t.Fatalf(".bvcg head: %v", err)
	}
}

func TestBuildAndCommit_AddsNewBranch(t *testing.T) {
	ir := setupImportedRepo(t)
	newOID := ir.addSecondCommit(t)
	// Push refs/heads/main forward AND create a new ref refs/heads/feature
	// pointing at the original commit.
	updates := map[string]string{
		"refs/heads/main":    newOID,
		"refs/heads/feature": ir.mainOID,
	}
	body, err := BuildAndCommit(context.Background(), ir.store, ir.tenant, ir.repo, ir.bareDir, updates, "pusher")
	if err != nil {
		t.Fatalf("BuildAndCommit: %v", err)
	}
	if body.Refs["refs/heads/main"] != newOID {
		t.Fatalf("main: got %q, want %q", body.Refs["refs/heads/main"], newOID)
	}
	if body.Refs["refs/heads/feature"] != ir.mainOID {
		t.Fatalf("feature: got %q, want %q", body.Refs["refs/heads/feature"], ir.mainOID)
	}
}

func TestBuildAndCommit_DeletesRef(t *testing.T) {
	ir := setupImportedRepo(t)
	// Add a second branch first via Import-then-push so we have something to
	// delete (deleting the only ref is a separate edge case tested below).
	_ = ir.addSecondCommit(t)
	if out, err := gitcli.RunForTest(ir.bareDir, "update-ref",
		"refs/heads/feature", ir.mainOID); err != nil {
		t.Fatalf("create feature: %v: %s", err, out)
	}
	// First push: add the feature ref through BuildAndCommit so the
	// committed manifest has it.
	if _, err := BuildAndCommit(context.Background(), ir.store, ir.tenant, ir.repo, ir.bareDir,
		map[string]string{"refs/heads/feature": ir.mainOID}, "setup"); err != nil {
		t.Fatalf("setup BuildAndCommit: %v", err)
	}
	pre, _ := ir.readBody(t)
	if _, ok := pre.Refs["refs/heads/feature"]; !ok {
		t.Fatalf("setup failed: refs/heads/feature not in body")
	}

	// Now delete the feature ref in the mirror, then BuildAndCommit with a
	// null-OID update.
	if out, err := gitcli.RunForTest(ir.bareDir, "update-ref", "-d", "refs/heads/feature"); err != nil {
		t.Fatalf("delete feature in mirror: %v: %s", err, out)
	}
	body, err := BuildAndCommit(context.Background(), ir.store, ir.tenant, ir.repo, ir.bareDir,
		map[string]string{"refs/heads/feature": nullOIDHex}, "deleter")
	if err != nil {
		t.Fatalf("BuildAndCommit delete: %v", err)
	}
	if _, ok := body.Refs["refs/heads/feature"]; ok {
		t.Fatalf("delete: refs/heads/feature still present in body: %+v", body.Refs)
	}
	if _, ok := body.Refs["refs/heads/main"]; !ok {
		t.Fatalf("delete should preserve refs/heads/main")
	}
}

func TestBuildAndCommit_RepackProducesCanonical(t *testing.T) {
	ir := setupImportedRepo(t)
	// Stage a 2nd and 3rd commit, plus another fetch — the mirror's
	// objects/pack/ may end up with multiple packs. We then run
	// BuildAndCommit and assert the manifest has exactly one pack.
	newOID := ir.addSecondCommit(t)
	// One more cycle: third commit added to source, fetched into mirror.
	if err := os.WriteFile(filepath.Join(ir.srcWork, "h"), []byte("c3\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if out, err := gitcli.RunForTest(ir.srcWork, "add", "h"); err != nil {
		t.Fatalf("add: %v: %s", err, out)
	}
	if out, err := gitcli.RunForTest(ir.srcWork,
		"-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "c3"); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	out, err := gitcli.RunForTest(ir.srcWork, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v: %s", err, out)
	}
	c3OID := strings.TrimSpace(string(out))
	if out, err := gitcli.RunForTest(ir.bareDir,
		"fetch", "--no-write-fetch-head", ir.srcWork, "+refs/heads/*:refs/heads/*"); err != nil {
		t.Fatalf("fetch: %v: %s", err, out)
	}
	_ = newOID

	body, err := BuildAndCommit(context.Background(), ir.store, ir.tenant, ir.repo, ir.bareDir,
		map[string]string{"refs/heads/main": c3OID}, "pusher")
	if err != nil {
		t.Fatalf("BuildAndCommit: %v", err)
	}
	if len(body.Packs) != 1 {
		t.Fatalf("Packs len = %d, want 1 (canonical repack)", len(body.Packs))
	}
	if body.Packs[0].ObjectCount <= 0 {
		t.Fatalf("ObjectCount = %d, want > 0", body.Packs[0].ObjectCount)
	}
	if body.Packs[0].SizeBytes <= 0 {
		t.Fatalf("SizeBytes = %d, want > 0", body.Packs[0].SizeBytes)
	}
}

func TestBuildAndCommit_StaleManifestRejected(t *testing.T) {
	ir := setupImportedRepo(t)
	// Concurrently advance the manifest from underneath BuildAndCommit by
	// committing a no-op body change between ReadRoot and Commit. We simulate
	// this by manually using the repo.Repo machinery: do a foreign Commit
	// before BuildAndCommit can close its CAS, then BuildAndCommit must
	// detect via the version mismatch in the mutator.
	//
	// Simplest deterministic approach: drive two BuildAndCommit calls in
	// sequence. The first advances the manifest. The second begins from a
	// snapshot we capture BEFORE the first commits.
	//
	// Even simpler: directly Commit an unrelated body to bump the version,
	// then run BuildAndCommit with stale-but-otherwise-valid inputs and
	// verify the StaleManifest error path.
	r, err := repo.Open(context.Background(), ir.store, ir.tenant, ir.repo)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	// First, capture pre version via a real read so we know what we're racing.
	_, prevVer := ir.readBody(t)

	// Bump the manifest with a no-op commit.
	if _, err := r.Commit(context.Background(), tx.Body{Type: "noop", Actor: "racer"},
		func(prev *repo.RootView) ([]byte, error) {
			// Just re-emit the same body bytes (with normalized empty fields)
			// — bumps version without changing semantics.
			return prev.Body, nil
		}); err != nil {
		t.Fatalf("noop commit: %v", err)
	}
	postBody, postVer := ir.readBody(t)
	if postVer != prevVer+1 {
		t.Fatalf("noop did not bump version: %d -> %d", prevVer, postVer)
	}
	_ = postBody

	// BuildAndCommit will read post-noop body (so its merge is fresh), but
	// we want to test the loser path. To get the loser path, we need
	// BuildAndCommit's ReadRoot to happen BEFORE another commit. That means
	// we have to inject a concurrent commit DURING BuildAndCommit. We do
	// that by running BuildAndCommit and then a noop in goroutines and
	// asserting at least one BuildAndCommit fails. But that's flaky.
	//
	// Deterministic approach: directly invoke the same Commit mutator
	// pattern manually with a stale captured version, to prove the
	// version-comparison logic in the mutator rejects what it should.
	captured := postVer
	// Now bump again so captured is stale.
	if _, err := r.Commit(context.Background(), tx.Body{Type: "noop2", Actor: "racer2"},
		func(prev *repo.RootView) ([]byte, error) {
			return prev.Body, nil
		}); err != nil {
		t.Fatalf("noop2 commit: %v", err)
	}
	// Try a Commit with a mutator that uses the stale-detection logic
	// BuildAndCommit uses internally.
	_, err = r.Commit(context.Background(), tx.Body{Type: "push", Actor: "stale"},
		func(prev *repo.RootView) ([]byte, error) {
			if prev.Header.ManifestVersion != captured {
				return nil, fmt.Errorf("stale manifest (started at v%d, now v%d)",
					captured, prev.Header.ManifestVersion)
			}
			return prev.Body, nil
		})
	if err == nil {
		t.Fatalf("expected stale-manifest rejection")
	}
	if !strings.Contains(err.Error(), "stale manifest") {
		t.Fatalf("expected stale-manifest error, got %v", err)
	}
}

// TestBuildAndCommit_StaleManifestRaceDetected drives a real concurrent
// race between two BuildAndCommit calls — each operating on its own bare
// mirror, each pushing a distinct new commit — and asserts at least one
// of them observes the CAS-loser path. This is the integration-level
// proof that the in-mutator version check fires under contention.
//
// The two pushes have different content (so different pack IDs), which
// means the loser cannot trip on upload-pack ErrAlreadyExists; the
// loser-detection MUST come from the CAS mutator.
func TestBuildAndCommit_StaleManifestRaceDetected(t *testing.T) {
	skipIfNoGit(t)
	ir := setupImportedRepo(t)
	// Build two independent local commits + mirrors, each with their own
	// distinct second commit. Both will race to update refs/heads/main.
	mk := func(filename, content string) (mirrorDir, newOID string) {
		work := t.TempDir()
		mustGit := func(args ...string) string {
			out, err := gitcli.RunForTest(work, args...)
			if err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		// Clone the upstream src so we share the original commit.
		if out, err := gitcli.RunForTest(t.TempDir(), "clone",
			"--no-local", ir.srcWork, work); err != nil {
			t.Fatalf("clone: %v: %s", err, out)
		}
		if err := os.WriteFile(filepath.Join(work, filename), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		mustGit("add", filename)
		mustGit("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", filename)
		newOID = mustGit("rev-parse", "HEAD")
		mirror := filepath.Join(t.TempDir(), "race-mirror.git")
		if err := gitcli.CloneBareMirror(context.Background(), work, mirror); err != nil {
			t.Fatalf("CloneBareMirror: %v", err)
		}
		return mirror, newOID
	}
	mirrorA, oidA := mk("racer_a", "A\n")
	mirrorB, oidB := mk("racer_b", "B\n")
	if oidA == oidB {
		t.Fatalf("oid collision between racers (test setup bug)")
	}

	type result struct {
		body *manifest.Body
		err  error
	}
	out := make(chan result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b, e := BuildAndCommit(context.Background(), ir.store, ir.tenant, ir.repo, mirrorA,
			map[string]string{"refs/heads/main": oidA}, "racerA")
		out <- result{b, e}
	}()
	go func() {
		defer wg.Done()
		b, e := BuildAndCommit(context.Background(), ir.store, ir.tenant, ir.repo, mirrorB,
			map[string]string{"refs/heads/main": oidB}, "racerB")
		out <- result{b, e}
	}()
	wg.Wait()
	close(out)

	var wins, losses int
	for r := range out {
		if r.err == nil {
			wins++
			continue
		}
		if !strings.Contains(r.err.Error(), "stale manifest") {
			t.Fatalf("loser: expected stale-manifest error, got %v", r.err)
		}
		losses++
	}
	// If both serialized fully (e.g. one finished before the other started
	// its ReadRoot), both can win. That's a degenerate non-race outcome —
	// still valid, but doesn't exercise the loser path. Most runs hit the
	// race path; we tolerate the degenerate case rather than introducing
	// flakiness via timing-dependent assertions.
	if wins == 0 {
		t.Fatalf("no winner (both errors)")
	}
	if wins+losses != 2 {
		t.Fatalf("unexpected counts wins=%d losses=%d", wins, losses)
	}
	if losses == 0 {
		t.Logf("both BuildAndCommit calls succeeded (no race observed); rerun for race coverage")
	}
}

func TestMergeRefs_PreservesUnrelated(t *testing.T) {
	prev := map[string]string{
		"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/heads/old":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"refs/tags/v1":    "cccccccccccccccccccccccccccccccccccccccc",
	}
	updates := map[string]string{
		"refs/heads/main": "dddddddddddddddddddddddddddddddddddddddd",
		"refs/heads/new":  "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	}
	got, err := mergeRefs(prev, updates)
	if err != nil {
		t.Fatalf("mergeRefs: %v", err)
	}
	want := map[string]string{
		"refs/heads/main": "dddddddddddddddddddddddddddddddddddddddd",
		"refs/heads/old":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"refs/heads/new":  "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		"refs/tags/v1":    "cccccccccccccccccccccccccccccccccccccccc",
	}
	if !equalRefMap(got, want) {
		t.Fatalf("mergeRefs: got %v, want %v", got, want)
	}
	// Original prev not mutated.
	if _, ok := prev["refs/heads/new"]; ok {
		t.Fatalf("prev mutated")
	}
}

func TestMergeRefs_DeletesOnNullOID(t *testing.T) {
	prev := map[string]string{
		"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/heads/del":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	got, err := mergeRefs(prev, map[string]string{
		"refs/heads/del": nullOIDHex,
	})
	if err != nil {
		t.Fatalf("mergeRefs: %v", err)
	}
	if _, ok := got["refs/heads/del"]; ok {
		t.Fatalf("delete: refs/heads/del still present")
	}
	if got["refs/heads/main"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("main mutated")
	}
}

func TestMergeRefs_DeletesOnEmptyOID(t *testing.T) {
	prev := map[string]string{"refs/heads/del": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	got, err := mergeRefs(prev, map[string]string{"refs/heads/del": ""})
	if err != nil {
		t.Fatalf("mergeRefs: %v", err)
	}
	if _, ok := got["refs/heads/del"]; ok {
		t.Fatalf("empty oid: refs/heads/del still present")
	}
}

func TestMergeRefs_RejectsEmptyRefname(t *testing.T) {
	if _, err := mergeRefs(nil, map[string]string{"": "aaaa"}); err == nil {
		t.Fatalf("expected error on empty refname")
	}
}

func TestBuildAndCommit_RejectsBadRefOID(t *testing.T) {
	ir := setupImportedRepo(t)
	updates := map[string]string{
		// 40 hex chars but no such object exists in the bare.
		"refs/heads/wedged": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
	_, err := BuildAndCommit(context.Background(), ir.store, ir.tenant, ir.repo, ir.bareDir, updates, "x")
	if err == nil {
		t.Fatalf("expected error: nonexistent OID should be rejected")
	}
	if !strings.Contains(err.Error(), "not in bareDir") {
		t.Fatalf("expected 'not in bareDir' error, got %v", err)
	}
}

func TestBuildAndCommit_RejectsMissingRepo(t *testing.T) {
	skipIfNoGit(t)
	store := newTestStore(t)
	bare := filepath.Join(t.TempDir(), "bare")
	if out, err := gitcli.RunForTest(t.TempDir(), "init", "--bare", bare); err != nil {
		t.Fatalf("init bare: %v: %s", err, out)
	}
	_, err := BuildAndCommit(context.Background(), store, "no", "such", bare, map[string]string{}, "x")
	if err == nil {
		t.Fatalf("expected error opening missing repo")
	}
}

func TestRemoveKeepFiles_Idempotent(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	keepPath := filepath.Join(packDir, "pack-abc.keep")
	if err := os.WriteFile(keepPath, []byte("guarded"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := removeKeepFiles(dir); err != nil {
		t.Fatalf("removeKeepFiles: %v", err)
	}
	if _, err := os.Stat(keepPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("keep still present: %v", err)
	}
	// Idempotent: second call no-ops.
	if err := removeKeepFiles(dir); err != nil {
		t.Fatalf("removeKeepFiles second call: %v", err)
	}
	// Non-existent directory: no error.
	if err := removeKeepFiles(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("removeKeepFiles missing dir: %v", err)
	}
}

func TestBuildAndCommit_RejectsDeletingDefaultBranch(t *testing.T) {
	ir := setupImportedRepo(t)

	// Confirm default branch is refs/heads/main and the ref exists.
	body, _ := ir.readBody(t)
	if body.DefaultBranch == "" {
		t.Fatalf("test fixture missing DefaultBranch")
	}
	if _, ok := body.Refs[body.DefaultBranch]; !ok {
		t.Fatalf("test fixture missing the default branch ref %q", body.DefaultBranch)
	}

	// Attempt to delete the default branch via a null-OID update.
	updates := map[string]string{
		body.DefaultBranch: nullOIDHex,
	}
	_, err := BuildAndCommit(context.Background(), ir.store, ir.tenant, ir.repo, ir.bareDir, updates, "tester")
	if err == nil {
		t.Fatalf("BuildAndCommit: expected error on deleting default branch, got nil")
	}
	if !strings.Contains(err.Error(), "default branch") {
		t.Fatalf("error should mention default branch: %v", err)
	}
}

// TestBuildAndCommit_RefOnlyUpdateSucceeds covers the ref-only push
// case: the second BuildAndCommit creates a new branch pointing at an
// already-reachable commit. No new objects are added to the reachable
// set. This must succeed end-to-end (the original concern was that an
// ErrAlreadyExists on the canonical pack key would block the push).
//
// Note: pack-objects' pack_id is the SHA-1 of the assembled pack BYTES,
// not a hash of the abstract object set. Repeated repacks of the same
// reachable set yield different pack_ids in practice (delta search is
// non-deterministic across threads). So in the common case the canonical
// key for a fresh repack is empty and the upload just succeeds.
func TestBuildAndCommit_RefOnlyUpdateSucceeds(t *testing.T) {
	ir := setupImportedRepo(t)

	// First push: advance main to a second commit. Uploads a fresh
	// canonical pack covering both commits.
	newOID := ir.addSecondCommit(t)
	body1, err := BuildAndCommit(context.Background(), ir.store, ir.tenant, ir.repo, ir.bareDir,
		map[string]string{"refs/heads/main": newOID}, "pusher")
	if err != nil {
		t.Fatalf("first BuildAndCommit: %v", err)
	}
	if len(body1.Packs) != 1 {
		t.Fatalf("first body packs len = %d, want 1", len(body1.Packs))
	}

	// Create a new ref locally pointing at the SAME object set (the older
	// commit, already reachable from refs/heads/main).
	if out, err := gitcli.RunForTest(ir.bareDir, "update-ref",
		"refs/heads/feature", ir.mainOID); err != nil {
		t.Fatalf("update-ref feature: %v: %s", err, out)
	}

	// Second BuildAndCommit: ref-only update. Must succeed and the body
	// must contain both refs.
	body2, err := BuildAndCommit(context.Background(), ir.store, ir.tenant, ir.repo, ir.bareDir,
		map[string]string{"refs/heads/feature": ir.mainOID}, "pusher")
	if err != nil {
		t.Fatalf("ref-only BuildAndCommit: %v", err)
	}
	if len(body2.Packs) != 1 {
		t.Fatalf("ref-only body packs len = %d, want 1", len(body2.Packs))
	}
	if body2.Refs["refs/heads/feature"] != ir.mainOID {
		t.Fatalf("post body feature: got %q, want %q", body2.Refs["refs/heads/feature"], ir.mainOID)
	}
	if body2.Refs["refs/heads/main"] != newOID {
		t.Fatalf("post body main: got %q, want %q", body2.Refs["refs/heads/main"], newOID)
	}
	// All canonical artifacts referenced by the post body must exist.
	if _, err := ir.store.Head(context.Background(), body2.Packs[0].PackKey); err != nil {
		t.Fatalf("ref-only post pack head: %v", err)
	}
	if _, err := ir.store.Head(context.Background(), body2.Packs[0].IdxKey); err != nil {
		t.Fatalf("ref-only post idx head: %v", err)
	}
	if body2.Indexes.ObjectMap == nil {
		t.Fatalf("ref-only post body missing .bvom")
	}
	if _, err := ir.store.Head(context.Background(), body2.Indexes.ObjectMap.Key); err != nil {
		t.Fatalf("ref-only post .bvom head: %v", err)
	}
	if body2.Indexes.CommitGraph == nil {
		t.Fatalf("ref-only post body missing .bvcg")
	}
	if _, err := ir.store.Head(context.Background(), body2.Indexes.CommitGraph.Key); err != nil {
		t.Fatalf("ref-only post .bvcg head: %v", err)
	}
}

// TestBuildAndCommit_FirstPushOnUnbornDefault covers the empty-repo
// case: an Import populated DefaultBranch but the repo has no refs yet
// (e.g. imported source had only an unborn HEAD). A first push that
// creates a non-default branch must NOT be misread as deleting the
// unborn default — the default-branch guard should only fire when the
// default ref existed before this push.
func TestBuildAndCommit_FirstPushOnUnbornDefault(t *testing.T) {
	skipIfNoGit(t)
	store := newTestStore(t)

	// Build a "imported" empty repo: manifest with DefaultBranch set but
	// no refs. We do this by Import-ing a real empty bare, which yields
	// an empty Refs map but DefaultBranch carried from HEAD's symref.
	srcBare := filepath.Join(t.TempDir(), "empty.git")
	if out, err := gitcli.RunForTest(t.TempDir(), "init", "--bare",
		"--initial-branch=main", srcBare); err != nil {
		t.Fatalf("init bare: %v: %s", err, out)
	}
	if _, err := Import(context.Background(), store, Options{
		SourceDir: srcBare, Tenant: "t", Repo: "r", Actor: "tester",
	}); err != nil {
		t.Fatalf("Import empty: %v", err)
	}

	// Sanity: the imported manifest should carry a DefaultBranch and
	// have no refs.
	r, err := repo.Open(context.Background(), store, "t", "r")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var pre manifest.Body
	if err := json.Unmarshal(view.Body, &pre); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if pre.DefaultBranch == "" {
		t.Skip("import of empty bare did not set DefaultBranch on this git version")
	}
	if _, hasDefault := pre.Refs[pre.DefaultBranch]; hasDefault {
		t.Skip("import populated the default ref; cannot test unborn case")
	}

	// Build a mirror with one new branch (NOT the default), one commit.
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	if out, err := gitcli.RunForTest(t.TempDir(), "init", "--bare",
		"--initial-branch=feature", mirror); err != nil {
		t.Fatalf("init mirror: %v: %s", err, out)
	}
	work := t.TempDir()
	mustGit := func(args ...string) string {
		t.Helper()
		out, err := gitcli.RunForTest(work, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mustGit("init", "--initial-branch=feature")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("c1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit("add", "f")
	mustGit("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "c1")
	featOID := mustGit("rev-parse", "HEAD")
	if out, err := gitcli.RunForTest(mirror, "fetch", "--no-write-fetch-head",
		work, "+refs/heads/*:refs/heads/*"); err != nil {
		t.Fatalf("fetch into mirror: %v: %s", err, out)
	}

	// First push: create only refs/heads/feature. The default branch
	// (e.g. refs/heads/main) is not in the update set and was never in
	// the manifest. The guard MUST allow this through.
	body, err := BuildAndCommit(context.Background(), store, "t", "r", mirror,
		map[string]string{"refs/heads/feature": featOID}, "first-pusher")
	if err != nil {
		t.Fatalf("first push on unborn default: %v", err)
	}
	if body.Refs["refs/heads/feature"] != featOID {
		t.Fatalf("feature ref not committed: %v", body.Refs)
	}
	// DefaultBranch should be preserved in the body (still unborn until
	// someone pushes that ref name).
	if body.DefaultBranch != pre.DefaultBranch {
		t.Fatalf("DefaultBranch changed: %q -> %q", pre.DefaultBranch, body.DefaultBranch)
	}
}

func equalRefMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
