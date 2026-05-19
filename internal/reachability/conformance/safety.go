// Package conformance provides a property test for the M10 reachability
// index across backend adapters. Mirrors M8 RunPropertyGCSafety and
// M9 RunPropertyMaintenanceSafety.
package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Factory matches the shape used by the other conformance suites
// (see internal/maintenance/conformance/safety.go for the M9 template).
type Factory func(t testing.TB) (store storage.ObjectStore, cleanup func())

// RunPropertyReachabilitySafety exercises 4 interleavings:
//
//   - solo_compaction               (compact-only path drops all deltas)
//   - push_during_compaction        (CAS-merge correctness)
//   - two_compactions               (only one wins; loser orphans cleanly)
//   - negotiation_during_compaction (cold read while base swaps)
//
// Each interleaving is scaffolded as a sub-test. Full implementations
// land in a follow-up; this milestone provides the conformance surface
// + the simple "solo" baseline.
func RunPropertyReachabilitySafety(t *testing.T, f Factory) {
	t.Run("solo_compaction", func(t *testing.T) {
		store, cleanup := f(t)
		defer cleanup()
		runSoloCompaction(t, store)
	})

	t.Run("push_during_compaction", func(t *testing.T) {
		t.Skip("push_during_compaction: scaffolded; concurrent harness in follow-up")
	})

	t.Run("two_compactions", func(t *testing.T) {
		t.Skip("two_compactions: scaffolded; concurrent harness in follow-up")
	})

	t.Run("negotiation_during_compaction", func(t *testing.T) {
		t.Skip("negotiation_during_compaction: scaffolded; concurrent harness in follow-up")
	})
}

// runSoloCompaction verifies the compact-only maintenance path:
//   - Seeds a repo from a real git import (1 canonical pack).
//   - Injects 150 synthetic .bvrd delta entries into the manifest so that
//     the ReachabilityDeltaPushes threshold fires (default = 100).
//   - Runs maintenance.Run without Force (pack thresholds don't fire for a
//     single-pack repo; only the reachability threshold fires).
//   - Asserts that the post-run manifest has Reachability.Deltas == []:
//     the compact-only path consumed all deltas and reset the chain.
func runSoloCompaction(t *testing.T, s storage.ObjectStore) {
	t.Helper()
	mtest.GitAvailable(t)
	ctx := context.Background()

	const tenant, repoName = "t", "r"
	mtest.SeedRepoFromImport(t, s, tenant, repoName)

	r, err := repo.Open(ctx, s, tenant, repoName)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	k, err := keys.NewRepo(tenant, repoName)
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	// Build and upload 150 synthetic .bvrd delta files. Each delta has one
	// commit record with a unique OID. 150 > 100 (the default
	// ReachabilityDeltaPushes threshold) so the compact-only path fires.
	const nDeltas = 150
	deltas := make([]manifest.IndexRef, nDeltas)
	for i := range deltas {
		// Use a unique OID per delta so that deltaIndex has 150 distinct
		// entries. All-zero OIDs collapse to a single entry, making the
		// delta-index correctness invisible to the test.
		var oid pack.OID
		binary.BigEndian.PutUint32(oid[0:4], uint32(i+1))
		d := deltaindex.Delta{
			Commits: []deltaindex.CommitRecord{
				{OID: oid, Generation: uint32(i + 2)},
			},
		}
		b, err := deltaindex.Encode(d)
		if err != nil {
			t.Fatalf("deltaindex.Encode [%d]: %v", i, err)
		}
		sum := sha256.Sum256(b)
		hash := hex.EncodeToString(sum[:])
		dkey := k.ReachabilityDeltaKey(hash)
		if _, err := s.PutIfAbsent(ctx, dkey, bytes.NewReader(b), nil); err != nil {
			t.Fatalf("PutIfAbsent delta [%d]: %v", i, err)
		}
		deltas[i] = manifest.IndexRef{Key: dkey, Hash: hash, SizeBytes: int64(len(b))}
	}

	// Inject the delta chain into the manifest via a CAS commit.
	_, err = r.Commit(ctx, tx.Body{Type: "test_inject_deltas", Actor: "u_test"},
		func(prev *repo.RootView) ([]byte, error) {
			var body manifest.Body
			if err := json.Unmarshal(prev.Body, &body); err != nil {
				return nil, err
			}
			body.Indexes.Reachability = &manifest.ReachabilityRef{
				BaseManifest: "v00000001",
				Deltas:       deltas,
			}
			return manifest.MarshalBody(body)
		})
	if err != nil {
		t.Fatalf("inject deltas: %v", err)
	}

	// Run maintenance without Force. Pack thresholds won't fire (1-pack
	// repo is well below TotalPackCount=10000 and ManifestPackBytes limits).
	// The ReachabilityDeltaPushes threshold (150 > 100) fires the compact-
	// only path, which rebuilds bvom+bvcg from scratch and drops all deltas.
	//
	// BitmapCoveragePct is explicitly set to 0 (disabled) — the M9.5
	// default is 100, which would fire on this fresh seed (1 pack with
	// no .bitmap yet) and force a repack instead of the compact-only
	// path under test. Use DefaultThresholds + zero-out just the
	// bitmap trigger so future threshold defaults are inherited
	// automatically.
	thr := maintenance.DefaultThresholds()
	thr.BitmapCoveragePct = 0
	report, err := maintenance.Run(ctx, s, r, k, maintenance.RunOptions{Thresholds: thr})
	if err != nil {
		t.Fatalf("maintenance.Run: %v", err)
	}
	if report.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", report.Outcome)
	}

	// Compact-only path must have fired.
	if !report.ReachabilityCompaction.Triggered {
		t.Errorf("ReachabilityCompaction.Triggered = false, want true")
	}
	if report.ReachabilityCompaction.DeltasDropped != nDeltas {
		t.Errorf("DeltasDropped = %d, want %d", report.ReachabilityCompaction.DeltasDropped, nDeltas)
	}

	// No repack must have happened (NewPackKey is only set by the repack path).
	if report.NewPackKey != "" {
		t.Errorf("NewPackKey = %q, want empty (compact-only run must not repack)", report.NewPackKey)
	}

	// Post-run: Reachability delta chain must be empty.
	postView, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("post-run ReadRoot: %v", err)
	}
	var postBody manifest.Body
	if err := json.Unmarshal(postView.Body, &postBody); err != nil {
		t.Fatalf("post-run Unmarshal: %v", err)
	}
	if postBody.Indexes.Reachability == nil {
		t.Errorf("post-run Reachability is nil, want present with empty Deltas")
	} else if len(postBody.Indexes.Reachability.Deltas) != 0 {
		t.Errorf("post-run Reachability.Deltas = %d entries, want 0 (all consumed by compaction)",
			len(postBody.Indexes.Reachability.Deltas))
	}
}
