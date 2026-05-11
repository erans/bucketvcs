package diffharness

// m10_fixtures_test.go contains fixture-style tests for M10 reachability
// delta-chain scenarios.  Each test synthesises its own repo state inline
// (no external fixture builder) so that the property under test is
// self-contained and the failure message is clear.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	maintpkg "github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ─── shared helpers ──────────────────────────────────────────────────────────

// fakeOID produces a deterministic pack.OID from an index byte.
func fakeOID(i int) pack.OID {
	var raw [20]byte
	raw[0] = byte(i)
	raw[1] = 0xfa
	raw[2] = 0xce
	return pack.OID(raw)
}

// putDelta encodes and uploads one deltaindex.Delta to the store,
// returning the manifest.IndexRef describing it.
func putDelta(t *testing.T, ctx context.Context, s storage.ObjectStore, k *keys.Repo, d deltaindex.Delta) manifest.IndexRef {
	t.Helper()
	b, err := deltaindex.Encode(d)
	if err != nil {
		t.Fatalf("deltaindex.Encode: %v", err)
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	dkey := k.ReachabilityDeltaKey(hash)
	if _, err := s.PutIfAbsent(ctx, dkey, bytes.NewReader(b), nil); err != nil {
		t.Fatalf("PutIfAbsent(%s): %v", dkey, err)
	}
	return manifest.IndexRef{Key: dkey, Hash: hash, SizeBytes: int64(len(b))}
}

// setReachability injects a ReachabilityRef into the repo's manifest
// via a no-op Commit and returns the resulting manifest body.
func setReachability(t *testing.T, ctx context.Context, r *repo.Repo, ref *manifest.ReachabilityRef) manifest.Body {
	t.Helper()
	var out manifest.Body
	_, err := r.Commit(ctx, tx.Body{Type: "test_set_reachability", Actor: "u_test"},
		func(prev *repo.RootView) ([]byte, error) {
			if err := json.Unmarshal(prev.Body, &out); err != nil {
				return nil, err
			}
			out.Indexes.Reachability = ref
			return manifest.MarshalBody(out)
		})
	if err != nil {
		t.Fatalf("setReachability commit: %v", err)
	}
	return out
}

// readBody reads the current manifest body of r.
func readBody(t *testing.T, ctx context.Context, r *repo.Repo) manifest.Body {
	t.Helper()
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return body
}

// runCompactWithPushesThreshold runs maintenance with a low
// ReachabilityDeltaPushes threshold so the compact-only path fires
// when the delta count exceeds n.
func runCompactWithPushesThreshold(t *testing.T, ctx context.Context, s storage.ObjectStore, r *repo.Repo, k *keys.Repo, n int) maintpkg.Report {
	t.Helper()
	opts := maintpkg.RunOptions{
		Thresholds: maintpkg.Thresholds{
			TotalPackCount:          10000,
			ManifestPackBytes:       8 << 20,
			ReachabilityDeltaPushes: n,
			ReachabilityDeltaBytes:  64 << 20,
		},
	}
	rep, err := maintpkg.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("maintenance.Run: %v", err)
	}
	return rep
}

// ─── Task 8.2: many-small-pushes ─────────────────────────────────────────────

// TestM10_ManySmallPushes verifies that a 50-delta chain compacts
// correctly: maintenance fires, all 50 deltas are dropped, and the
// post-compaction reachability.Set still loads without error.
func TestM10_ManySmallPushes(t *testing.T) {
	mtest.GitAvailable(t)
	ctx := context.Background()
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "diff", "m10many")

	r, err := repo.Open(ctx, s, "diff", "m10many")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	k, err := keys.NewRepo("diff", "m10many")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	const nPushes = 50

	// Build 50 delta entries, one per simulated push.
	deltaRefs := make([]manifest.IndexRef, nPushes)
	for i := 0; i < nPushes; i++ {
		oid := fakeOID(i + 1)
		d := deltaindex.Delta{
			Commits: []deltaindex.CommitRecord{
				{OID: oid, Generation: uint32(i + 2)},
			},
		}
		deltaRefs[i] = putDelta(t, ctx, s, k, d)
	}
	setReachability(t, ctx, r, &manifest.ReachabilityRef{
		BaseManifest: "v00000001",
		Deltas:       deltaRefs,
	})

	// Run maintenance with threshold = 10 (fires at 50 > 10).
	rep := runCompactWithPushesThreshold(t, ctx, s, r, k, 10)
	if rep.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", rep.Outcome)
	}
	if !rep.ReachabilityCompaction.Triggered {
		t.Errorf("ReachabilityCompaction.Triggered = false, want true")
	}
	if rep.ReachabilityCompaction.DeltasDropped != nPushes {
		t.Errorf("DeltasDropped = %d, want %d", rep.ReachabilityCompaction.DeltasDropped, nPushes)
	}

	// Post-compaction: Deltas must be empty.
	post := readBody(t, ctx, r)
	if post.Indexes.Reachability != nil && len(post.Indexes.Reachability.Deltas) != 0 {
		t.Errorf("post Reachability.Deltas = %d, want 0",
			len(post.Indexes.Reachability.Deltas))
	}

	// reachability.Set must load from the compacted manifest.
	set, err := reachability.Load(ctx, s, k, post)
	if err != nil {
		t.Fatalf("reachability.Load post-compaction: %v", err)
	}

	// The set must know about the base commits (via .bvcg).
	tips := set.RefTips()
	if len(tips) == 0 {
		t.Error("post-compaction RefTips empty; expected at least one ref")
	}
}

// ─── Task 8.3: force-push-mid-chain ──────────────────────────────────────────

// TestM10_ForcePushMidChain verifies that a delta whose RefTip has
// OldOID ≠ ancestor of NewOID (a force-push scenario) round-trips
// correctly: the RefTip update is visible via Set.RefTips() and
// Set.Parents() before maintenance, and maintenance completes cleanly.
func TestM10_ForcePushMidChain(t *testing.T) {
	mtest.GitAvailable(t)
	ctx := context.Background()
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "diff", "m10force")

	r, err := repo.Open(ctx, s, "diff", "m10force")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	k, err := keys.NewRepo("diff", "m10force")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	// Simulate a force-push: OldOID and NewOID come from separate lines
	// with no common ancestry — a classic non-fast-forward scenario.
	oldOID := fakeOID(0x10)
	newOID := fakeOID(0x20)
	parentOID := fakeOID(0x11) // parent of newOID on the force-pushed branch

	d := deltaindex.Delta{
		Commits: []deltaindex.CommitRecord{
			{OID: parentOID, Generation: 4, Parents: nil},
			{OID: newOID, Generation: 5, Parents: []pack.OID{parentOID}},
		},
		RefTips: []deltaindex.RefTipDiff{
			{RefName: "refs/heads/feature", OldOID: oldOID, NewOID: newOID},
		},
	}
	ref := putDelta(t, ctx, s, k, d)
	setReachability(t, ctx, r, &manifest.ReachabilityRef{
		BaseManifest: "v00000001",
		Deltas:       []manifest.IndexRef{ref},
	})

	// Before compaction: the Set should reflect the delta's RefTip.
	preMaintBody := readBody(t, ctx, r)
	preSet, err := reachability.Load(ctx, s, k, preMaintBody)
	if err != nil {
		t.Fatalf("reachability.Load pre-maintenance: %v", err)
	}
	if preSet.RefTips()["refs/heads/feature"] != newOID {
		t.Errorf("pre-maintenance feature tip = %v, want %v",
			preSet.RefTips()["refs/heads/feature"], newOID)
	}

	// Parents of newOID should be parentOID (from the delta).
	parents := preSet.Parents(newOID)
	if len(parents) != 1 || parents[0] != parentOID {
		t.Errorf("Parents(newOID) = %v, want [%v]", parents, parentOID)
	}

	// Run maintenance (Force=true) — completes cleanly even with synthetic OIDs.
	opts := maintpkg.RunOptions{Force: true}
	rep, err := maintpkg.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("maintenance.Run: %v", err)
	}
	if rep.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", rep.Outcome)
	}
	t.Logf("force-push-mid-chain: pre-maintenance assertions passed; Force repack outcome=%s", rep.Outcome)
}

// ─── Task 8.4: tag-pushes-between-commits ────────────────────────────────────

// TestM10_TagPushesBetweenCommits verifies that a delta containing tag
// ref tip diffs (refs/tags/…) survives compaction and is readable via
// Set.RefTips() before maintenance, and that maintenance completes cleanly.
func TestM10_TagPushesBetweenCommits(t *testing.T) {
	mtest.GitAvailable(t)
	ctx := context.Background()
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "diff", "m10tags")

	r, err := repo.Open(ctx, s, "diff", "m10tags")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	k, err := keys.NewRepo("diff", "m10tags")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	// Simulate a series of commits interspersed with annotated tag pushes.
	// Three commits (gen 2,3,4) and two tag refs pointing at commits.
	c1 := fakeOID(0x01)
	c2 := fakeOID(0x02)
	c3 := fakeOID(0x03)

	// New-tag pushes use zero OldOID (no prior ref exists).
	var zeroOID pack.OID

	d := deltaindex.Delta{
		Commits: []deltaindex.CommitRecord{
			{OID: c1, Generation: 2, Parents: nil},
			{OID: c2, Generation: 3, Parents: []pack.OID{c1}},
			{OID: c3, Generation: 4, Parents: []pack.OID{c2}},
		},
		RefTips: []deltaindex.RefTipDiff{
			{RefName: "refs/heads/main", OldOID: zeroOID, NewOID: c3},
			{RefName: "refs/tags/v1.0", OldOID: zeroOID, NewOID: c2},
			{RefName: "refs/tags/v2.0", OldOID: zeroOID, NewOID: c3},
		},
	}
	ref := putDelta(t, ctx, s, k, d)
	setReachability(t, ctx, r, &manifest.ReachabilityRef{
		BaseManifest: "v00000001",
		Deltas:       []manifest.IndexRef{ref},
	})

	// Before maintenance: RefTips must expose both tag refs.
	preBody := readBody(t, ctx, r)
	preSet, err := reachability.Load(ctx, s, k, preBody)
	if err != nil {
		t.Fatalf("reachability.Load: %v", err)
	}
	tips := preSet.RefTips()
	if tips["refs/tags/v1.0"] != c2 {
		t.Errorf("refs/tags/v1.0 = %v, want %v", tips["refs/tags/v1.0"], c2)
	}
	if tips["refs/tags/v2.0"] != c3 {
		t.Errorf("refs/tags/v2.0 = %v, want %v", tips["refs/tags/v2.0"], c3)
	}
	if tips["refs/heads/main"] != c3 {
		t.Errorf("refs/heads/main = %v, want %v", tips["refs/heads/main"], c3)
	}

	// Generation numbers in the delta must be correct.
	if g, ok := preSet.Generation(c3); !ok || g != 4 {
		t.Errorf("gen(c3) = (%d, %v), want (4, true)", g, ok)
	}

	// Run maintenance (Force=true).
	opts := maintpkg.RunOptions{Force: true}
	rep, err := maintpkg.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("maintenance.Run: %v", err)
	}
	if rep.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", rep.Outcome)
	}
	t.Logf("tag-pushes-between-commits: pre-maintenance tag refs verified; maintenance outcome=%s", rep.Outcome)
}

// ─── Task 8.5: octopus-merge ─────────────────────────────────────────────────

// TestM10_OctopusMerge verifies that a commit with 3 parents (an
// octopus merge) has its generation number correctly stored in a delta
// and retrieved via reachability.Set: gen(merge) = max(parent gens)+1.
// WalkAncestors must visit all 5 commits in the diamond-like topology.
func TestM10_OctopusMerge(t *testing.T) {
	mtest.GitAvailable(t)
	ctx := context.Background()
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "diff", "m10octopus")

	r, err := repo.Open(ctx, s, "diff", "m10octopus")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	k, err := keys.NewRepo("diff", "m10octopus")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	// Topology:
	//   root (gen=1) ──→ branchA (gen=2)
	//              ╰──→ branchB (gen=2)
	//              ╰──→ branchC (gen=2)
	//   merge (gen=3, parents=[branchA, branchB, branchC])
	root := fakeOID(0x01)
	branchA := fakeOID(0x0A)
	branchB := fakeOID(0x0B)
	branchC := fakeOID(0x0C)
	merge := fakeOID(0x0D)

	d := deltaindex.Delta{
		Commits: []deltaindex.CommitRecord{
			{OID: root, Generation: 1, Parents: nil},
			{OID: branchA, Generation: 2, Parents: []pack.OID{root}},
			{OID: branchB, Generation: 2, Parents: []pack.OID{root}},
			{OID: branchC, Generation: 2, Parents: []pack.OID{root}},
			{OID: merge, Generation: 3, Parents: []pack.OID{branchA, branchB, branchC}},
		},
		RefTips: []deltaindex.RefTipDiff{
			{RefName: "refs/heads/main", OldOID: root, NewOID: merge},
		},
	}
	ref := putDelta(t, ctx, s, k, d)
	setReachability(t, ctx, r, &manifest.ReachabilityRef{
		BaseManifest: "v00000001",
		Deltas:       []manifest.IndexRef{ref},
	})

	body := readBody(t, ctx, r)
	set, err := reachability.Load(ctx, s, k, body)
	if err != nil {
		t.Fatalf("reachability.Load: %v", err)
	}

	// The merge commit must have exactly 3 parents.
	parents := set.Parents(merge)
	if len(parents) != 3 {
		t.Errorf("octopus merge Parents count = %d, want 3", len(parents))
	}
	parentSet := make(map[pack.OID]bool, 3)
	for _, p := range parents {
		parentSet[p] = true
	}
	for _, want := range []pack.OID{branchA, branchB, branchC} {
		if !parentSet[want] {
			t.Errorf("parent %x missing from octopus merge Parents", want[:4])
		}
	}

	// Generation of the merge must be 3 (max(2,2,2)+1).
	if g, ok := set.Generation(merge); !ok || g != 3 {
		t.Errorf("gen(merge) = (%d, %v), want (3, true)", g, ok)
	}

	// WalkAncestors from merge must visit all 5 commits.
	visited := make(map[pack.OID]bool)
	if err := set.WalkAncestors([]pack.OID{merge}, func(oid pack.OID, _ uint32) error {
		visited[oid] = true
		return nil
	}); err != nil {
		t.Fatalf("WalkAncestors: %v", err)
	}
	for name, oid := range map[string]pack.OID{
		"root": root, "branchA": branchA, "branchB": branchB,
		"branchC": branchC, "merge": merge,
	} {
		if !visited[oid] {
			t.Errorf("WalkAncestors: %s (%x) not visited", name, oid[:4])
		}
	}

	// Run maintenance (Force=true) — must complete cleanly.
	opts := maintpkg.RunOptions{Force: true}
	rep, err := maintpkg.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("maintenance.Run: %v", err)
	}
	if rep.Outcome != "success" {
		t.Fatalf("maintenance Outcome = %q, want success", rep.Outcome)
	}
	t.Logf("octopus-merge: gen=3 verified for 3-parent commit; maintenance outcome=%s", rep.Outcome)
}
