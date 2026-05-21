package gc_test

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/lfs/gc"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest/manifesttest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// seedRepoWithLFSObjects creates a fresh bucketvcs repo backed by
// localfs, commits a manifest with no LFS pointers, and seeds the
// LFS storage prefix with the given OIDs (each as a tiny blob).
// Returns the open *repo.Repo + store + bareDir + tenant + repoID.
func seedRepoWithLFSObjects(t *testing.T, orphanOIDs ...string) (*repo.Repo, *localfs.Localfs, string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	store, err := localfs.Open(filepath.Join(tmp, "store"))
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	r, err := repo.Create(ctx, store, "acme", "demo", repo.CreateOptions{Actor: "u_test"})
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	// Commit an empty body — no refs, no packs.
	if _, err := r.Commit(ctx, tx.Body{Type: "smoke", Actor: "u_test"},
		func(_ *repo.RootView) ([]byte, error) {
			return manifest.MarshalBody(manifest.Body{
				DefaultBranch: "refs/heads/main",
				Refs:          map[string]string{},
				Packs:         []manifest.PackEntry{},
				Bundles:       []manifest.BundleEntry{},
			})
		}); err != nil {
		t.Fatalf("r.Commit: %v", err)
	}
	// Seed orphan LFS objects.
	prefix := lfs.RepoLFSPrefix("acme", "demo")
	for _, oid := range orphanOIDs {
		key := prefix + oid
		if _, err := store.PutIfAbsent(ctx, key, bytes.NewReader([]byte("dummy")), nil); err != nil {
			t.Fatalf("seed %s: %v", oid, err)
		}
	}
	// Empty bare dir for BareDir option (no refs to walk).
	bare := filepath.Join(tmp, "bare.git")
	mustRun := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = tmp
		if b, cerr := cmd.CombinedOutput(); cerr != nil {
			t.Fatalf("%s: %v\n%s", name, cerr, b)
		}
	}
	mustRun("git", "init", "--bare", bare)
	return r, store, bare, "acme", "demo"
}

func TestRunMark_RecordsOrphans(t *testing.T) {
	const oid1 = "1111111111111111111111111111111111111111111111111111111111111111"
	const oid2 = "2222222222222222222222222222222222222222222222222222222222222222"
	r, store, bare, _, _ := seedRepoWithLFSObjects(t, oid1, oid2)
	now := time.Unix(1700000000, 0)
	rec, err := gc.RunMark(context.Background(), store, r, gc.MarkOptions{
		Now:              func() time.Time { return now },
		RetentionSeconds: 7 * 24 * 3600,
		BareDir:          bare,
	})
	if err != nil {
		t.Fatalf("RunMark: %v", err)
	}
	if len(rec.Candidates) != 2 {
		t.Fatalf("Candidates len=%d want 2 (got=%v)", len(rec.Candidates), rec.Candidates)
	}
	// Sorted by OID.
	if rec.Candidates[0].OID != oid1 || rec.Candidates[1].OID != oid2 {
		t.Errorf("candidates not sorted: %v", rec.Candidates)
	}
	for _, c := range rec.Candidates {
		if !c.FirstSeenUnreferencedAt.Equal(now.UTC()) {
			t.Errorf("oid %s FirstSeenUnreferencedAt=%v want %v", c.OID, c.FirstSeenUnreferencedAt, now.UTC())
		}
	}
}

func TestRunMark_ExcludesLiveSet(t *testing.T) {
	// Pins the load-bearing invariant of LFS GC: an LFS object that
	// IS referenced by a reachable Git pointer blob must NOT appear
	// in the mark candidates. Uses the same seeding helper as
	// TestBuildLiveSet_FindsBothPointers (two pointer blobs → OIDs
	// aa...aa and bb...bb), plus two extra orphan OIDs in storage
	// that have no pointer references. The mark should record exactly
	// the orphans, not the referenced OIDs.
	bare, livePointerOIDs := seedBareRepoWithPointers(t)

	// Open a fresh bucketvcs repo and seed the LFS prefix with all
	// 4 OIDs (2 live, 2 orphan).
	tmp := t.TempDir()
	store, err := localfs.Open(filepath.Join(tmp, "store"))
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	r, err := repo.Create(ctx, store, "acme", "demo", repo.CreateOptions{Actor: "u_test"})
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	if _, err := r.Commit(ctx, tx.Body{Type: "smoke", Actor: "u_test"},
		func(_ *repo.RootView) ([]byte, error) {
			return manifest.MarshalBody(manifest.Body{
				DefaultBranch: "refs/heads/main",
				Refs:          map[string]string{},
				Packs:         []manifest.PackEntry{},
				Bundles:       []manifest.BundleEntry{},
			})
		}); err != nil {
		t.Fatalf("r.Commit: %v", err)
	}

	const orphan1 = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const orphan2 = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	prefix := lfs.RepoLFSPrefix("acme", "demo")
	for _, oid := range append(livePointerOIDs, orphan1, orphan2) {
		key := prefix + oid
		if _, err := store.PutIfAbsent(ctx, key, bytes.NewReader([]byte("body for "+oid[:8])), nil); err != nil {
			t.Fatalf("seed %s: %v", oid, err)
		}
	}

	rec, err := gc.RunMark(ctx, store, r, gc.MarkOptions{
		Now:              func() time.Time { return time.Unix(1700000000, 0) },
		RetentionSeconds: 7 * 24 * 3600,
		BareDir:          bare,
	})
	if err != nil {
		t.Fatalf("RunMark: %v", err)
	}
	// Exactly the two orphans should be candidates; neither live OID.
	got := map[string]bool{}
	for _, c := range rec.Candidates {
		got[c.OID] = true
	}
	for _, live := range livePointerOIDs {
		if got[live] {
			t.Errorf("live OID %s incorrectly included in mark candidates (would be deleted): %v", live, rec.Candidates)
		}
	}
	if !got[orphan1] || !got[orphan2] {
		t.Errorf("orphan OIDs missing from mark candidates; got=%v want={%s, %s}",
			rec.Candidates, orphan1[:8], orphan2[:8])
	}
	if len(rec.Candidates) != 2 {
		t.Errorf("expected exactly 2 candidates (the orphans), got %d: %v", len(rec.Candidates), rec.Candidates)
	}
}

func TestRunMark_CarriesForwardFirstSeen(t *testing.T) {
	const oid = "3333333333333333333333333333333333333333333333333333333333333333"
	r, store, bare, tenant, repoID := seedRepoWithLFSObjects(t, oid)
	t0 := time.Unix(1700000000, 0)
	rec0, err := gc.RunMark(context.Background(), store, r, gc.MarkOptions{
		Now: func() time.Time { return t0 }, RetentionSeconds: 7 * 24 * 3600, BareDir: bare,
	})
	if err != nil {
		t.Fatalf("RunMark t0: %v", err)
	}
	if err := gc.WriteMark(context.Background(), store, tenant, repoID, rec0); err != nil {
		t.Fatalf("WriteMark t0: %v", err)
	}
	// Second mark a day later — orphan should retain its original FirstSeenUnreferencedAt.
	t1 := t0.Add(24 * time.Hour)
	rec1, err := gc.RunMark(context.Background(), store, r, gc.MarkOptions{
		Now: func() time.Time { return t1 }, RetentionSeconds: 7 * 24 * 3600, BareDir: bare,
	})
	if err != nil {
		t.Fatalf("RunMark t1: %v", err)
	}
	if len(rec1.Candidates) != 1 {
		t.Fatalf("rec1 candidates=%d want 1", len(rec1.Candidates))
	}
	if !rec1.Candidates[0].FirstSeenUnreferencedAt.Equal(t0.UTC()) {
		t.Errorf("FirstSeenUnreferencedAt=%v want carry-forward %v", rec1.Candidates[0].FirstSeenUnreferencedAt, t0.UTC())
	}
}

func TestRunSweep_RetentionHonored(t *testing.T) {
	const oid = "4444444444444444444444444444444444444444444444444444444444444444"
	r, store, bare, _, _ := seedRepoWithLFSObjects(t, oid)
	t0 := time.Unix(1700000000, 0)
	rec, err := gc.RunMark(context.Background(), store, r, gc.MarkOptions{
		Now: func() time.Time { return t0 }, RetentionSeconds: 7 * 24 * 3600, BareDir: bare,
	})
	if err != nil {
		t.Fatalf("RunMark: %v", err)
	}
	// Sweep before retention elapses → no delete.
	report, err := gc.RunSweep(context.Background(), store, r, rec, gc.SweepOptions{
		Now: func() time.Time { return t0.Add(1 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("RunSweep early: %v", err)
	}
	if report.DeletedCount != 0 || report.SkippedRetention != 1 {
		t.Errorf("early sweep deleted=%d skipped=%d want 0/1", report.DeletedCount, report.SkippedRetention)
	}
	// LFS object should still exist.
	prefix := lfs.RepoLFSPrefix("acme", "demo")
	if _, err := store.Head(context.Background(), prefix+oid); err != nil {
		t.Errorf("LFS object missing after early sweep: %v", err)
	}
}

func TestRunSweep_DeletesAfterRetention(t *testing.T) {
	const oid = "5555555555555555555555555555555555555555555555555555555555555555"
	r, store, bare, _, _ := seedRepoWithLFSObjects(t, oid)
	t0 := time.Unix(1700000000, 0)
	rec, err := gc.RunMark(context.Background(), store, r, gc.MarkOptions{
		Now: func() time.Time { return t0 }, RetentionSeconds: 7 * 24 * 3600, BareDir: bare,
	})
	if err != nil {
		t.Fatalf("RunMark: %v", err)
	}
	// Sweep way after retention.
	report, err := gc.RunSweep(context.Background(), store, r, rec, gc.SweepOptions{
		Now: func() time.Time { return t0.Add(8 * 24 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("RunSweep late: %v", err)
	}
	if report.DeletedCount != 1 || report.SkippedRetention != 0 {
		t.Errorf("late sweep deleted=%d skipped=%d want 1/0", report.DeletedCount, report.SkippedRetention)
	}
	prefix := lfs.RepoLFSPrefix("acme", "demo")
	if _, err := store.Head(context.Background(), prefix+oid); err == nil {
		t.Errorf("LFS object still present after late sweep")
	}
}

func TestRunSweep_DryRunDeletesNothing(t *testing.T) {
	const oid = "6666666666666666666666666666666666666666666666666666666666666666"
	r, store, bare, _, _ := seedRepoWithLFSObjects(t, oid)
	t0 := time.Unix(1700000000, 0)
	rec, err := gc.RunMark(context.Background(), store, r, gc.MarkOptions{
		Now: func() time.Time { return t0 }, RetentionSeconds: 7 * 24 * 3600, BareDir: bare,
	})
	if err != nil {
		t.Fatalf("RunMark: %v", err)
	}
	report, err := gc.RunSweep(context.Background(), store, r, rec, gc.SweepOptions{
		Now: func() time.Time { return t0.Add(8 * 24 * time.Hour) }, DryRun: true,
	})
	if err != nil {
		t.Fatalf("RunSweep dry: %v", err)
	}
	if report.DeletedCount != 1 {
		t.Errorf("dry-run report DeletedCount=%d want 1 (logical)", report.DeletedCount)
	}
	if !report.DryRun {
		t.Errorf("DryRun=false on report; want true")
	}
	prefix := lfs.RepoLFSPrefix("acme", "demo")
	if _, err := store.Head(context.Background(), prefix+oid); err != nil {
		t.Errorf("LFS object missing after dry-run sweep: %v", err)
	}
}

func TestLoadRefsFromBody_V2ShardedIncludesAllRefs(t *testing.T) {
	// CRITICAL test for the v2 ref-loading invariant. After M12,
	// body.Refs is empty for any repo that has run reshard-refs;
	// refs live across body.RefShards + per-shard storage objects.
	// If RunMark's helper only reads body.Refs (the pre-fix bug),
	// BuildLiveSet sees zero reachable commits and every LFS object
	// gets swept after retention. Silent data loss for v2 repos.
	//
	// We test the helper directly rather than the full RunMark/
	// Materialize path because the full integration scaffolding (real
	// packs + LFS pointer blobs in a sharded body) is high cost; the
	// helper is the actual locus of the fix.

	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	k, err := keys.NewRepo("acme", "demo")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	const (
		nameA = "refs/heads/main"
		nameB = "refs/heads/release"
	)
	if refstore.ShardKey(nameA) == refstore.ShardKey(nameB) {
		t.Fatalf("fixture collision: %q and %q hash to the same shard", nameA, nameB)
	}
	wantRefs := map[string]string{
		nameA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		nameB: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	body, err := manifesttest.MakeShardedBody(ctx, store, k, nameA, wantRefs)
	if err != nil {
		t.Fatalf("MakeShardedBody: %v", err)
	}
	// Sanity: this is a v2 body (body.Refs empty, RefShards populated).
	if len(body.Refs) != 0 {
		t.Fatalf("v2 body should have body.Refs empty; got %d entries", len(body.Refs))
	}
	if len(body.RefShards) < 2 {
		t.Fatalf("v2 body has %d RefShards; need ≥ 2 for this test", len(body.RefShards))
	}

	gotRefs, err := gc.LoadRefsFromBodyForTest(ctx, store, "acme", "demo", &body)
	if err != nil {
		t.Fatalf("loadRefsFromBody: %v", err)
	}
	if len(gotRefs) != len(wantRefs) {
		t.Errorf("got %d refs, want %d; got=%v", len(gotRefs), len(wantRefs), gotRefs)
	}
	for name, oid := range wantRefs {
		if gotRefs[name] != oid {
			t.Errorf("ref %s: got %q, want %q", name, gotRefs[name], oid)
		}
	}
}

func TestLoadRefsFromBody_InlineRefs(t *testing.T) {
	// Sanity: the inline path keeps working unchanged. body.Refs goes
	// through refstore.List untouched.
	ctx := context.Background()
	body := &manifest.Body{
		Refs: map[string]string{
			"refs/heads/main": "1234567890123456789012345678901234567890",
		},
	}
	gotRefs, err := gc.LoadRefsFromBodyForTest(ctx, nil, "acme", "demo", body)
	if err != nil {
		t.Fatalf("loadRefsFromBody inline: %v", err)
	}
	if got, want := gotRefs["refs/heads/main"], "1234567890123456789012345678901234567890"; got != want {
		t.Errorf("inline ref oid = %q, want %q", got, want)
	}
}
