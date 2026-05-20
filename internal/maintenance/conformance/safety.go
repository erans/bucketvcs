// Package conformance provides cross-adapter property tests for the
// M9 maintenance pipeline. RunPropertyMaintenanceSafety is the public
// entry point; adapters call it from their own *_test.go to exercise
// the property suite against their ObjectStore implementation.
package conformance

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Factory returns a fresh storage.ObjectStore for one test invocation,
// plus a cleanup function the test should defer or t.Cleanup. Mirrors
// internal/gc/conformance.Factory so cloud adapters can plug in their
// existing factory closures directly.
type Factory func(t testing.TB) (storage.ObjectStore, func())

// RunPropertyMaintenanceSafety verifies §43.6-style invariants against
// any caller-provided ObjectStore factory. Three scenarios:
//
//	solo               — single maintenance run; manifest converges.
//	push_during_walk   — version bump between repack and CAS; M9 retries.
//	two_maintenances   — two sequential runs; final state has 1 pack and
//	                     reachability holds.
//
// Each sub-test calls factory(t) for a fresh store. Tests skip when
// `git` is not on PATH.
//
// NOTE: The spec's `gc_during_retention` scenario is deferred — it
// requires interleaving with M8 GC's mark/sweep semantics and is more
// naturally exercised by an end-to-end integration test (followup).
func RunPropertyMaintenanceSafety(t *testing.T, factory Factory) {
	t.Run("solo", func(t *testing.T) {
		s, cleanup := factory(t)
		defer cleanup()
		runSolo(t, s)
	})
	t.Run("push_during_walk", func(t *testing.T) {
		s, cleanup := factory(t)
		defer cleanup()
		runPushDuringWalk(t, s)
	})
	t.Run("two_maintenances", func(t *testing.T) {
		s, cleanup := factory(t)
		defer cleanup()
		runTwoMaintenances(t, s)
	})
}

func runSolo(t *testing.T, s storage.ObjectStore) {
	mtest.GitAvailable(t)
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
	rep, err := maintenance.Run(ctx, s, r, k, maintenance.RunOptions{Force: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", rep.Outcome)
	}
	if rep.AfterPackCount != 1 {
		t.Errorf("AfterPackCount = %d, want 1", rep.AfterPackCount)
	}
	assertReachable(t, s, r)
}

func runPushDuringWalk(t *testing.T, s storage.ObjectStore) {
	mtest.GitAvailable(t)
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

	hookFired := false
	opts := maintenance.RunOptions{
		Force: true,
		BetweenRepackAndCAS: func() {
			if hookFired {
				return
			}
			hookFired = true
			r2, err := repo.Open(ctx, s, "acme", "site")
			if err != nil {
				t.Fatalf("hook: open: %v", err)
			}
			if _, err := r2.Commit(ctx,
				tx.Body{Type: "test_bump", Actor: "u_test"},
				func(prev *repo.RootView) ([]byte, error) { return prev.Body, nil },
			); err != nil {
				t.Fatalf("hook: bump: %v", err)
			}
		},
	}
	rep, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Outcome != "success" {
		t.Errorf("outcome = %q, want success", rep.Outcome)
	}
	if rep.CASAttempts < 2 {
		t.Errorf("CASAttempts = %d, want >= 2 (first attempt should hit version mismatch)", rep.CASAttempts)
	}
	assertReachable(t, s, r)
}

func runTwoMaintenances(t *testing.T, s storage.ObjectStore) {
	mtest.GitAvailable(t)
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
	for i := 0; i < 2; i++ {
		rep, err := maintenance.Run(ctx, s, r, k, maintenance.RunOptions{Force: true})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if rep.Outcome != "success" && rep.Outcome != "noop" {
			t.Errorf("run %d outcome = %q, want success or noop", i, rep.Outcome)
		}
		if rep.AfterPackCount != 1 {
			t.Errorf("run %d AfterPackCount = %d, want 1", i, rep.AfterPackCount)
		}
	}
	assertReachable(t, s, r)
}

// assertReachable downloads all packs in the current manifest into a
// fresh bare repo and runs `git fsck --full`. If fsck succeeds, every
// commit referenced by manifest.Refs is reachable through manifest.Packs.
func assertReachable(t *testing.T, s storage.ObjectStore, r *repo.Repo) {
	t.Helper()
	ctx := context.Background()
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("Unmarshal body: %v", err)
	}
	if body.RefSharding != "" || len(body.RefShards) > 0 {
		t.Skipf("conformance helper does not support v2 sharded bodies (TODO(M12 follow-up): route through refstore.List)")
	}
	packs := make([]maintenance.PackRef, 0, len(body.Packs))
	for _, p := range body.Packs {
		packs = append(packs, maintenance.PackRef{PackKey: p.PackKey, IdxKey: p.IdxKey})
	}
	bareParent := t.TempDir()
	if err := maintenance.Materialize(ctx, s, maintenance.MaterializeInput{
		BareDir:       bareParent,
		Packs:         packs,
		Refs:          body.Refs,
		DefaultBranch: body.DefaultBranch,
	}); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	bare := filepath.Join(bareParent, "bare.git")
	if err := gitcli.Fsck(ctx, bare, true); err != nil {
		t.Fatalf("post-run fsck failed (reachability invariant violated): %v", err)
	}
}
