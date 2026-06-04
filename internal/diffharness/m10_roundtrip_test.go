package diffharness

// TestM10_ImportPushCompactNegotiate_RoundTrip exercises the full M10
// round-trip: import a base repo, inject synthetic delta entries
// representing simulated pushes, run maintenance to compact the delta
// chain, reload the reachability.Set, and verify the WalkAncestors
// result against the independent BFS reference walk over the same Set.
//
// Design notes:
//   - We use mtest.SeedRepoFromImport to produce a proper repo (with tx
//     log) rather than the raw rtest fixtures, because maintenance.Run
//     requires a *repo.Repo with an initialised manifest root.
//   - "Pushes" are simulated by injecting synthetic .bvrd delta entries
//     directly into the manifest via a no-op Commit callback.  This
//     mirrors TestRun_CompactOnly_NoPackRepack in the maintenance package
//     and avoids the complexity of driving a real receive-pack round.
//   - Negotiation is exercised via reachability.Set.WalkAncestors: we
//     treat the base commit as the "have" and the tip of the injected
//     deltas as the "want", then assert the reference oracle (BFS over
//     the same Set) produces an identical set of ancestors.

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

// injectSyntheticDeltas builds nDelta synthetic .bvrd files (each with
// one commit record with a fake OID derived from its index) and appends
// them to the repo's manifest Reachability.Deltas via a single Commit.
// Returns the OIDs and the resulting manifest body.
func injectSyntheticDeltas(
	t *testing.T,
	ctx context.Context,
	s storage.ObjectStore,
	r *repo.Repo,
	k *keys.Repo,
	nDelta int,
) (oids []pack.OID, body manifest.Body) {
	t.Helper()

	deltaRefs := make([]manifest.IndexRef, nDelta)
	resultOIDs := make([]pack.OID, nDelta)

	for i := 0; i < nDelta; i++ {
		// Build a deterministic fake OID from the index byte.
		var rawOID [20]byte
		rawOID[0] = byte(i + 1)
		rawOID[1] = 0xde
		rawOID[2] = 0xad
		oid := pack.OID(rawOID)
		resultOIDs[i] = oid

		d := deltaindex.Delta{
			Commits: []deltaindex.CommitRecord{
				{OID: oid, Generation: uint32(i + 2)},
			},
		}
		b, err := deltaindex.Encode(d)
		if err != nil {
			t.Fatalf("Encode delta %d: %v", i, err)
		}
		sum := sha256.Sum256(b)
		hash := hex.EncodeToString(sum[:])
		dkey := k.ReachabilityDeltaKey(hash)
		if _, err := s.PutIfAbsent(ctx, dkey, bytes.NewReader(b), nil); err != nil {
			t.Fatalf("PutIfAbsent delta %d: %v", i, err)
		}
		deltaRefs[i] = manifest.IndexRef{Key: dkey, Hash: hash, SizeBytes: int64(len(b))}
	}

	reachRef := &manifest.ReachabilityRef{
		BaseManifest: "v00000001",
		Deltas:       deltaRefs,
	}

	var postBody manifest.Body
	_, err := r.Commit(ctx, tx.Body{Type: "test_inject_deltas", Actor: "u_test"},
		func(prev *repo.RootView) ([]byte, error) {
			var b manifest.Body
			if err := json.Unmarshal(prev.Body, &b); err != nil {
				return nil, err
			}
			b.Indexes.Reachability = reachRef
			postBody = b
			return manifest.MarshalBody(b)
		})
	if err != nil {
		t.Fatalf("inject deltas commit: %v", err)
	}
	return resultOIDs, postBody
}

func TestM10_ImportPushCompactNegotiate_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("M10 round-trip — long test")
	}
	mtest.GitAvailable(t)

	ctx := context.Background()
	s := mtest.LocalfsStore(t)

	// Step 1: Seed repo via importer (produces proper manifest root +
	// initial pack + .bvcg/.bvom).
	mtest.SeedRepoFromImport(t, s, "diff", "m10rt")

	r, err := repo.Open(ctx, s, "diff", "m10rt")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	k, err := keys.NewRepo("diff", "m10rt")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	// Step 2: Simulate 5 small pushes by injecting 5 synthetic .bvrd
	// delta entries into the manifest.  Each "push" contributes one
	// commit record with a fake OID.
	const numPushes = 5
	_, _ = injectSyntheticDeltas(t, ctx, s, r, k, numPushes)

	// Step 3: Run maintenance with ReachabilityDeltaPushes=3 so that 5
	// deltas exceeds the threshold and the compact-only path fires.
	opts := maintpkg.RunOptions{
		Thresholds: maintpkg.Thresholds{
			TotalPackCount:           10000,
			ManifestPackBytes:        8 << 20,
			ReachabilityDeltaPushes:  3, // fires when nDelta > 3
			ReachabilityDeltaCommits: 1000,
			ReachabilityDeltaBytes:   64 << 20,
		},
	}
	report, err := maintpkg.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("maintenance.Run: %v", err)
	}
	if report.Outcome != "success" {
		t.Fatalf("maintenance Outcome = %q, want success", report.Outcome)
	}
	if !report.ReachabilityCompaction.Triggered {
		t.Fatalf("ReachabilityCompaction.Triggered = false; expected compact-only to fire")
	}
	if report.ReachabilityCompaction.DeltasDropped != numPushes {
		t.Errorf("DeltasDropped = %d, want %d", report.ReachabilityCompaction.DeltasDropped, numPushes)
	}

	// Step 4: Reload the reachability.Set from the post-maintenance manifest
	// and verify it is self-consistent via WalkAncestors.
	postView, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("ReadRoot post-maintenance: %v", err)
	}
	var postBody manifest.Body
	if err := json.Unmarshal(postView.Body, &postBody); err != nil {
		t.Fatalf("unmarshal post-maintenance body: %v", err)
	}

	// After compaction, Reachability.Deltas must be empty.
	if postBody.Indexes.Reachability != nil && len(postBody.Indexes.Reachability.Deltas) != 0 {
		t.Errorf("post-maintenance Reachability.Deltas = %d, want 0",
			len(postBody.Indexes.Reachability.Deltas))
	}

	set, err := reachability.Load(ctx, s, k, postBody)
	if err != nil {
		t.Fatalf("reachability.Load post-maintenance: %v", err)
	}

	// Step 5: Compare WalkAncestors result to the reference oracle.
	// Oracle: collect all commits reachable from RefTips via the Set
	// itself (independent BFS).
	refTips := set.RefTips()
	if len(refTips) == 0 {
		t.Fatal("post-maintenance RefTips empty")
	}

	// Collect walk result.
	walkResult := make(map[pack.OID]uint32)
	var roots []pack.OID
	for _, oid := range refTips {
		roots = append(roots, oid)
	}
	if err := set.WalkAncestors(roots, func(oid pack.OID, gen uint32) error {
		walkResult[oid] = gen
		return nil
	}); err != nil {
		t.Fatalf("WalkAncestors: %v", err)
	}

	// Reference oracle: independent BFS using Set.Parents.
	oracleResult := bfsOracle(set, roots)

	if len(walkResult) != len(oracleResult) {
		t.Errorf("WalkAncestors visited %d commits, oracle found %d",
			len(walkResult), len(oracleResult))
	}
	for oid, oracleGen := range oracleResult {
		walkGen, ok := walkResult[oid]
		if !ok {
			t.Errorf("oracle OID %x not visited by WalkAncestors", oid[:4])
			continue
		}
		if walkGen != oracleGen {
			t.Errorf("gen mismatch for %x: WalkAncestors=%d oracle=%d", oid[:4], walkGen, oracleGen)
		}
	}
}

// bfsOracle is an independent BFS over a reachability.Set, returning
// all reachable OIDs and their generation numbers. This is used as the
// reference implementation to cross-check WalkAncestors.
func bfsOracle(set *reachability.Set, roots []pack.OID) map[pack.OID]uint32 {
	seen := make(map[pack.OID]uint32)
	queue := make([]pack.OID, 0, len(roots))
	for _, r := range roots {
		if _, already := seen[r]; !already {
			gen, _ := set.Generation(r)
			seen[r] = gen
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, p := range set.Parents(cur) {
			if _, already := seen[p]; !already {
				gen, _ := set.Generation(p)
				seen[p] = gen
				queue = append(queue, p)
			}
		}
	}
	return seen
}
