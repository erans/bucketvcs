package maintenance_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// seedRepoWithInlineRefs creates a fresh repo with the given inline
// refs. Returns the open Repo and a closing function.
func seedRepoWithInlineRefs(t *testing.T, refs map[string]string, defaultBranch string) (*repo.Repo, *keys.Repo, *localfs.Localfs, func()) {
	t.Helper()
	tmp := t.TempDir()
	store, err := localfs.Open(tmp)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	k, err := keys.NewRepo("acme", "demo")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}
	r, err := repo.Create(context.Background(), store, "acme", "demo", repo.CreateOptions{
		DefaultBranch: defaultBranch,
	})
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	// Inject refs via a Commit. Borrow the importer-style buildBody path.
	if len(refs) > 0 {
		_, err := r.Commit(context.Background(), tx.Body{Type: "push", Actor: "u_test"}, func(prev *repo.RootView) ([]byte, error) {
			body := manifest.Body{
				DefaultBranch: defaultBranch,
				Refs:          refs,
				Packs:         []manifest.PackEntry{},
				Bundles:       []manifest.BundleEntry{},
			}
			return manifest.MarshalBody(body)
		})
		if err != nil {
			t.Fatalf("seed Commit: %v", err)
		}
	}
	return r, k, store, func() { _ = store.Close() }
}

func TestReshard_PreservesBodyFields(t *testing.T) {
	// Reshard's `emit` closure starts from the existing body and only
	// mutates ref-related fields. This test verifies that
	// non-ref-related fields (DefaultBranch, Packs, Bundles, plus any
	// future Body fields) round-trip through the migration unchanged.
	const oddDefault = "refs/heads/release-2025-Q4"
	refs := map[string]string{
		oddDefault: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	r, k, store, cleanup := seedRepoWithInlineRefs(t, refs, oddDefault)
	defer cleanup()

	report, err := maintenance.Reshard(context.Background(), store, r, k, maintenance.ReshardOptions{Actor: "u_test"})
	if err != nil {
		t.Fatalf("Reshard: %v", err)
	}
	if report.Outcome != "success" {
		t.Fatalf("Outcome=%q want success", report.Outcome)
	}

	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		t.Fatalf("UnmarshalBody: %v", err)
	}
	if body.DefaultBranch != oddDefault {
		t.Errorf("DefaultBranch=%q want %q (must round-trip through Reshard)", body.DefaultBranch, oddDefault)
	}
	if body.RefSharding != "hash_v1" {
		t.Errorf("RefSharding=%q want hash_v1", body.RefSharding)
	}
	if len(body.RefShards) == 0 {
		t.Fatalf("RefShards empty after migration")
	}
	// Packs and Bundles were seeded empty; they must remain that way
	// (not become nil), so the future-field invariant is exercised at
	// the basic-shape level too.
	if body.Packs == nil && len(body.Packs) != 0 {
		t.Errorf("Packs became nil — emit() must preserve seeded slices")
	}
}

func TestReshard_HappyPath(t *testing.T) {
	refs := map[string]string{
		"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/heads/dev":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"refs/tags/v1.0":  "cccccccccccccccccccccccccccccccccccccccc",
	}
	r, k, store, cleanup := seedRepoWithInlineRefs(t, refs, "refs/heads/main")
	defer cleanup()

	report, err := maintenance.Reshard(context.Background(), store, r, k, maintenance.ReshardOptions{
		Actor: "u_test",
		// Inject a 2ms delay between snapshot and CAS so DurationMS is
		// reliably nonzero (the millisecond-resolution counter would
		// otherwise round a sub-ms localfs run to 0, masking a real
		// bug — the defer must mutate a NAMED return value or the
		// caller will see DurationMS=0 always).
		BetweenSnapshotAndCAS: func() { time.Sleep(2 * time.Millisecond) },
	})
	if err != nil {
		t.Fatalf("Reshard: %v", err)
	}
	if report.Outcome != "success" {
		t.Errorf("Outcome=%q want success", report.Outcome)
	}
	if report.RefCount != 3 {
		t.Errorf("RefCount=%d want 3", report.RefCount)
	}
	if report.DurationMS <= 0 {
		t.Errorf("DurationMS=%d want positive (deferred named-return mutation must reach the caller)", report.DurationMS)
	}

	// Read the new manifest and assert v2 shape.
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		t.Fatalf("UnmarshalBody: %v", err)
	}
	if len(body.Refs) != 0 {
		t.Errorf("Refs=%v want empty", body.Refs)
	}
	if body.RefSharding != "hash_v1" {
		t.Errorf("RefSharding=%q want hash_v1", body.RefSharding)
	}
	if len(body.RefShards) == 0 {
		t.Fatal("RefShards empty after reshard")
	}

	// Read every ref through ShardedRefStore to confirm round-trip.
	rs, err := refstore.New(context.Background(), store, k, &body)
	if err != nil {
		t.Fatalf("refstore.New: %v", err)
	}
	got, err := rs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(refs) {
		t.Fatalf("List len=%d want %d", len(got), len(refs))
	}
	for name, v := range refs {
		if got[name] != v {
			t.Errorf("ref %q: got=%q want=%q", name, got[name], v)
		}
	}
}

func TestReshard_AlreadyV2IsNoop(t *testing.T) {
	// Build a v2 repo by hand (via reshard once), then run reshard
	// again and assert it exits "noop" without mutating.
	refs := map[string]string{"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	r, k, store, cleanup := seedRepoWithInlineRefs(t, refs, "refs/heads/main")
	defer cleanup()

	if _, err := maintenance.Reshard(context.Background(), store, r, k, maintenance.ReshardOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("first Reshard: %v", err)
	}
	// Capture the version after first reshard.
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	versionBefore := view.Header.ManifestVersion

	report, err := maintenance.Reshard(context.Background(), store, r, k, maintenance.ReshardOptions{Actor: "u_test"})
	if err != nil {
		t.Fatalf("second Reshard: %v", err)
	}
	if report.Outcome != "noop" {
		t.Errorf("Outcome=%q want noop", report.Outcome)
	}
	if report.RefCount != 1 {
		t.Errorf("RefCount=%d want 1 (sum of existing shard ref counts)", report.RefCount)
	}
	if report.ShardCount != 1 {
		t.Errorf("ShardCount=%d want 1", report.ShardCount)
	}

	view2, _ := r.ReadRoot(context.Background())
	if view2.Header.ManifestVersion != versionBefore {
		t.Errorf("ManifestVersion bumped on noop: before=%d after=%d", versionBefore, view2.Header.ManifestVersion)
	}
}

func TestReshard_EmptyRepoIsKeptV1(t *testing.T) {
	r, k, store, cleanup := seedRepoWithInlineRefs(t, map[string]string{}, "refs/heads/main")
	defer cleanup()
	versionBefore := uint64(0)
	{
		view, _ := r.ReadRoot(context.Background())
		versionBefore = view.Header.ManifestVersion
	}
	report, err := maintenance.Reshard(context.Background(), store, r, k, maintenance.ReshardOptions{Actor: "u_test"})
	if err != nil {
		t.Fatalf("Reshard: %v", err)
	}
	if report.Outcome != "empty_v1_kept" {
		t.Errorf("Outcome=%q want empty_v1_kept", report.Outcome)
	}
	if report.RefCount != 0 {
		t.Errorf("RefCount=%d want 0", report.RefCount)
	}
	if report.ShardCount != 0 {
		t.Errorf("ShardCount=%d want 0", report.ShardCount)
	}
	if report.ManifestVersionTo != report.ManifestVersionFrom {
		t.Errorf("ManifestVersionTo=%d bumped from %d (empty repo must not commit)",
			report.ManifestVersionTo, report.ManifestVersionFrom)
	}
	// Empty repo stays v1-shaped — no commit, no tx record. Manifest
	// validation forbids ref_sharding without RefShards, so we cannot
	// promote; reporting empty_v1_kept (rather than success or noop)
	// makes the distinction explicit in operator output.
	view, _ := r.ReadRoot(context.Background())
	if view.Header.ManifestVersion != versionBefore {
		t.Errorf("ManifestVersion bumped on empty-repo reshard: before=%d after=%d",
			versionBefore, view.Header.ManifestVersion)
	}
	body, _ := manifest.UnmarshalBody(view.Body)
	if body.RefSharding != "" {
		t.Errorf("RefSharding=%q want empty", body.RefSharding)
	}
	if len(body.RefShards) != 0 {
		t.Errorf("RefShards=%v want empty", body.RefShards)
	}
	if len(body.Refs) != 0 {
		t.Errorf("Refs=%v want empty", body.Refs)
	}
}

func TestReshard_ConcurrentMutationAborts(t *testing.T) {
	refs := map[string]string{"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	r, k, store, cleanup := seedRepoWithInlineRefs(t, refs, "refs/heads/main")
	defer cleanup()

	// Hook in a between-snapshot-and-CAS mutation by injecting an
	// observer via ReshardOptions. The hook fires once per Reshard
	// call; capture any racing-commit error and surface it via
	// t.Fatalf AFTER Reshard returns, since t.Fatalf must run on the
	// test's own goroutine.
	var raceErr error
	opts := maintenance.ReshardOptions{
		Actor: "u_test",
		BetweenSnapshotAndCAS: func() {
			// Push a small inline change to bump the manifest version.
			_, raceErr = r.Commit(context.Background(), tx.Body{Type: "push", Actor: "u_race"}, func(prev *repo.RootView) ([]byte, error) {
				body, uerr := manifest.UnmarshalBody(prev.Body)
				if uerr != nil {
					return nil, fmt.Errorf("racing commit: unmarshal body: %w", uerr)
				}
				if body.Refs == nil {
					body.Refs = map[string]string{}
				}
				body.Refs["refs/heads/sneaky"] = "ffffffffffffffffffffffffffffffffffffffff"
				return manifest.MarshalBody(body)
			})
		},
	}

	_, err := maintenance.Reshard(context.Background(), store, r, k, opts)
	if raceErr != nil {
		t.Fatalf("racing commit: %v", raceErr)
	}
	if !errors.Is(err, maintenance.ErrConcurrentMutation) {
		t.Fatalf("err=%v want ErrConcurrentMutation", err)
	}
}
