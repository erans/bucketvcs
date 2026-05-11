package maintenance_test

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

func TestRun_HappyPathProducesOnePackWithFreshIndexes(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	opts := maintenance.RunOptions{Force: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", report.Outcome)
	}
	if report.AfterPackCount != 1 {
		t.Errorf("AfterPackCount = %d, want 1", report.AfterPackCount)
	}
	if report.NewPackKey == "" {
		t.Errorf("NewPackKey empty")
	}
	if report.NewObjectMapKey == "" || report.NewCommitGraphKey == "" {
		t.Errorf("index keys empty: %+v", report)
	}

	// Verify post-Run manifest state.
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Packs) != 1 {
		t.Errorf("post-Run manifest has %d packs, want 1", len(body.Packs))
	}
	if body.Indexes.ObjectMap == nil || body.Indexes.CommitGraph == nil {
		t.Errorf("post-Run manifest missing indexes")
	}
}

func TestRun_NoOpWhenThresholdsNotTriggered(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	opts := maintenance.RunOptions{} // no Force, default thresholds; 1-pack repo won't trigger anything
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "noop" {
		t.Errorf("Outcome = %q, want noop", report.Outcome)
	}
	if report.AfterPackCount != report.BeforePackCount {
		t.Errorf("noop changed pack count: %d → %d", report.BeforePackCount, report.AfterPackCount)
	}
}

func TestRun_DryRunWritesNothing(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	preView, _ := r.ReadRoot(ctx)
	preVer := preView.Header.ManifestVersion

	opts := maintenance.RunOptions{Force: true, DryRun: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", report.Outcome)
	}
	if !report.DryRun {
		t.Errorf("Report.DryRun = false")
	}
	postView, _ := r.ReadRoot(ctx)
	if postView.Header.ManifestVersion != preVer {
		t.Errorf("dry-run mutated manifest: %d → %d", preVer, postView.Header.ManifestVersion)
	}
}

func TestRun_VersionBumpDuringPipelineRetriesCAS(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	// The hook fires during the first buildBody callback and before CAS,
	// performing a no-op commit on the same repo, which bumps the manifest
	// version without altering the body content. Maintenance's first CAS
	// attempt will see version mismatch after the hook fires, retry, and
	// succeed on the second pass because the body it constructs is
	// identical (the late commit added no packs to filter against P0Keys).
	hookFired := false
	opts := maintenance.RunOptions{
		Force: true,
		BetweenRepackAndCAS: func() {
			if hookFired {
				return // only fire once
			}
			hookFired = true
			r2, err := repo.Open(ctx, s, "acme", "site")
			if err != nil {
				t.Fatalf("hook: open repo: %v", err)
			}
			if _, err := r2.Commit(ctx,
				tx.Body{Type: "test_bump", Actor: "u_test"},
				func(prev *repo.RootView) ([]byte, error) {
					return prev.Body, nil
				}); err != nil {
				t.Fatalf("hook: bump commit: %v", err)
			}
		},
	}

	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", report.Outcome)
	}
	if report.CASAttempts < 2 {
		t.Errorf("CASAttempts = %d, want >= 2 (first attempt should hit version mismatch)", report.CASAttempts)
	}
	if report.AfterPackCount != 1 {
		t.Errorf("AfterPackCount = %d, want 1 (no concurrent push added a pack)", report.AfterPackCount)
	}
}

// seedDeltaRefs uploads nDelta synthetic .bvrd files to s and returns a
// ReachabilityRef with those deltas. Each .bvrd has 1 commit record.
func seedDeltaRefs(t *testing.T, ctx context.Context, s storage.ObjectStore, k *keys.Repo, nDelta int) *manifest.ReachabilityRef {
	t.Helper()
	deltas := make([]manifest.IndexRef, nDelta)
	for i := range deltas {
		// Use a unique OID per delta so each delta maps to a distinct
		// deltaIndex entry. All-zero OIDs would collapse to one entry and
		// hide delta-index correctness issues.
		var oid pack.OID
		binary.BigEndian.PutUint32(oid[0:4], uint32(i+1))
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
		deltas[i] = manifest.IndexRef{Key: dkey, Hash: hash, SizeBytes: int64(len(b))}
	}
	return &manifest.ReachabilityRef{
		BaseManifest: "v00000001",
		Deltas:       deltas,
	}
}

func TestRun_CompactOnly_NoPackRepack(t *testing.T) {
	mtest.GitAvailable(t)
	ctx := context.Background()
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	// Read pre-run manifest to capture the existing ObjectMap key.
	preView, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var preBody manifest.Body
	if err := json.Unmarshal(preView.Body, &preBody); err != nil {
		t.Fatal(err)
	}
	var preObjectMapKey string
	if preBody.Indexes.ObjectMap != nil {
		preObjectMapKey = preBody.Indexes.ObjectMap.Key
	}

	// Inject 150 delta entries (> ReachabilityDeltaPushes=100) into manifest.
	// The repo has 1 pack (below TotalPackCount=10000 and ManifestPackBytes thresholds).
	reachRef := seedDeltaRefs(t, ctx, s, k, 150)

	_, injErr := r.Commit(ctx, tx.Body{Type: "test_inject_deltas", Actor: "u_test"},
		func(prev *repo.RootView) ([]byte, error) {
			var body manifest.Body
			if err := json.Unmarshal(prev.Body, &body); err != nil {
				return nil, err
			}
			body.Indexes.Reachability = reachRef
			return manifest.MarshalBody(body)
		})
	if injErr != nil {
		t.Fatalf("inject deltas: %v", injErr)
	}

	// Use default thresholds — pack thresholds won't fire (1 pack), but
	// reachability pushes (150 > 100) should fire the compact-only path.
	opts := maintenance.RunOptions{}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", report.Outcome)
	}

	// Compact-only: triggered, deltas dropped.
	if !report.ReachabilityCompaction.Triggered {
		t.Errorf("ReachabilityCompaction.Triggered = false, want true")
	}
	if report.ReachabilityCompaction.DeltasDropped != 150 {
		t.Errorf("DeltasDropped = %d, want 150", report.ReachabilityCompaction.DeltasDropped)
	}

	// Post-run manifest: Packs unchanged (still 1), Reachability.Deltas == [].
	postView, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var postBody manifest.Body
	if err := json.Unmarshal(postView.Body, &postBody); err != nil {
		t.Fatal(err)
	}
	if len(postBody.Packs) != 1 {
		t.Errorf("post-run Packs = %d, want 1 (unchanged)", len(postBody.Packs))
	}
	if postBody.Indexes.Reachability == nil {
		t.Errorf("post-run Reachability is nil, want empty delta list")
	} else if len(postBody.Indexes.Reachability.Deltas) != 0 {
		t.Errorf("post-run Reachability.Deltas = %d, want 0 (all consumed)",
			len(postBody.Indexes.Reachability.Deltas))
	}

	// Pack repack should NOT have run.
	if report.NewPackKey != "" {
		t.Errorf("NewPackKey = %q, want empty (no repack)", report.NewPackKey)
	}

	// Compact-only must NOT produce a new .bvom (it would reference a
	// locally-built pack that is never uploaded — dangling pack-id reference).
	if report.NewObjectMapKey != "" {
		t.Errorf("NewObjectMapKey = %q, want empty (compact-only preserves .bvom)", report.NewObjectMapKey)
	}
	// .bvcg IS refreshed.
	if report.NewCommitGraphKey == "" {
		t.Errorf("NewCommitGraphKey is empty, want a new .bvcg key")
	}

	// Post-run manifest: ObjectMap key must be UNCHANGED (same as pre-run).
	var postObjectMapKey string
	if postBody.Indexes.ObjectMap != nil {
		postObjectMapKey = postBody.Indexes.ObjectMap.Key
	}
	if postObjectMapKey != preObjectMapKey {
		t.Errorf("post-run ObjectMap key = %q, want %q (unchanged by compact-only)",
			postObjectMapKey, preObjectMapKey)
	}
}

// TestRun_CompactOnly_RepackPathWinsWhenPackTriggered confirms that when
// BOTH reachability and pack thresholds fire, the repack path takes
// priority (Force=true acts as an explicit repack trigger here).
func TestRun_CompactOnly_RepackWinsOnPackTrigger(t *testing.T) {
	mtest.GitAvailable(t)
	ctx := context.Background()
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	// Inject 150 deltas to enable reachability trigger.
	reachRef := seedDeltaRefs(t, ctx, s, k, 150)
	_, _ = r.Commit(ctx, tx.Body{Type: "test_inject_deltas", Actor: "u_test"},
		func(prev *repo.RootView) ([]byte, error) {
			var body manifest.Body
			if err := json.Unmarshal(prev.Body, &body); err != nil {
				return nil, err
			}
			body.Indexes.Reachability = reachRef
			return manifest.MarshalBody(body)
		})

	// Force=true → repack path wins over compact-only.
	opts := maintenance.RunOptions{Force: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run with Force: %v", err)
	}
	if report.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", report.Outcome)
	}
	// When Force triggers repack, a new pack key should be set.
	if report.NewPackKey == "" {
		t.Errorf("Force run: NewPackKey empty, expected repack to produce a pack")
	}
}
