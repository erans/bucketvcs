// Package conformance is the GC property conformance test suite. It is a
// regular Go package (not a _test package) so it can be imported from any
// adapter's _test.go file, exactly as internal/storage/conformance works.
//
// Cloud adapter tests (Task 7.2) plug their own factory into
// RunPropertyGCSafety to verify GC safety properties against live backends.
package conformance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// RunPropertyGCSafety verifies that:
//
//  1. An orphan pack younger than the retention window is NOT swept.
//  2. After the retention window expires, an orphan pack IS swept.
//  3. A pack revived by a concurrent push between mark and sweep is NOT
//     deleted (§43.6).
//
// The factory must return a fresh, empty ObjectStore on each call.
func RunPropertyGCSafety(t *testing.T, factory func(t *testing.T) storage.ObjectStore) {
	t.Helper()

	// Property 1 + 2: retention window respected.
	//
	// Phase A: mark with 3600s retention → sweep must skip (retention_not_met).
	// Phase B: same candidates, 0s retention, sweep clock 2h ahead → must delete.
	//
	// We construct phase B's mark directly from phase A's record to avoid
	// RunMark's normalization of RetentionSeconds<=0 → DefaultRetention, and
	// advance the sweep clock so the candidate's age exceeds 0s.
	t.Run("orphan_pack_respects_retention", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		r, err := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		k, _ := keys.NewRepo("acme", "site")
		if _, err := store.PutIfAbsent(ctx, k.CanonicalPackKey("orphan"), strings.NewReader(""), nil); err != nil {
			t.Fatalf("seed orphan: %v", err)
		}

		// Phase A: mark with 3600s retention; sweep must skip the candidate.
		markA, err := gc.RunMark(ctx, store, r, gc.MarkOptions{Now: time.Now, RetentionSeconds: 3600})
		if err != nil {
			t.Fatalf("RunMark A: %v", err)
		}
		if err := marks.Write(ctx, store, k, markA); err != nil {
			t.Fatalf("marks.Write A: %v", err)
		}
		repA, err := gc.RunSweep(ctx, store, r, markA, gc.SweepOptions{Now: time.Now})
		if err != nil {
			t.Fatalf("RunSweep A: %v", err)
		}
		if len(repA.Deleted.CanonicalPacks) != 0 {
			t.Fatalf("phase A: retention not honored: deleted %v", repA.Deleted.CanonicalPacks)
		}

		// Phase B: copy markA candidates, set RetentionSeconds=0, and advance
		// the sweep clock 2 hours so now.Sub(firstSeen) >> 0s. This tests that
		// RunSweep correctly switches from "skip" to "delete" when the retention
		// window is met, without requiring a real-time sleep.
		markB := markA
		markB.RetentionSeconds = 0
		futureNow := func() time.Time { return time.Now().Add(2 * time.Hour) }
		repB, err := gc.RunSweep(ctx, store, r, markB, gc.SweepOptions{Now: futureNow})
		if err != nil {
			t.Fatalf("RunSweep B: %v", err)
		}
		if len(repB.Deleted.CanonicalPacks) != 1 {
			t.Fatalf("phase B: expected 1 delete, got %d (skipped=%+v)", len(repB.Deleted.CanonicalPacks), repB.Skipped)
		}
	})

	// Property 3: §43.6 — a push that references a pack between mark and sweep
	// causes the sweep to classify the pack as "revived" and NOT delete it.
	//
	// We use a future sweep clock so the candidate is sweep-eligible regardless
	// of the retention window (making the test non-trivial: without the revival
	// push the pack would be deleted, confirming the §43.6 guard is what saves it).
	t.Run("push_during_sweep_does_not_delete_revived_pack", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		r, err := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		k, _ := keys.NewRepo("acme", "site")
		packKey := k.CanonicalPackKey("racing-pack")
		if _, err := store.PutIfAbsent(ctx, packKey, strings.NewReader(""), nil); err != nil {
			t.Fatalf("seed pack: %v", err)
		}

		// Mark: observe the pack as an orphan candidate.
		mark, err := gc.RunMark(ctx, store, r, gc.MarkOptions{Now: time.Now, RetentionSeconds: 3600})
		if err != nil {
			t.Fatalf("RunMark: %v", err)
		}
		if err := marks.Write(ctx, store, k, mark); err != nil {
			t.Fatalf("marks.Write: %v", err)
		}

		// Concurrent push references the pack between mark and sweep (revival).
		_, err = r.Commit(ctx, tx.Body{Type: "push", Actor: "u_test"},
			func(prev *repo.RootView) ([]byte, error) {
				var body manifest.Body
				if err := json.Unmarshal(prev.Body, &body); err != nil {
					return nil, err
				}
				body.Packs = append(body.Packs, manifest.PackEntry{
					PackID:  "racing",
					PackKey: packKey,
					IdxKey:  packKey + ".idx",
				})
				return manifest.MarshalBody(body)
			},
		)
		if err != nil {
			t.Fatalf("revival commit: %v", err)
		}

		// Sweep with a future clock that makes the candidate retention-eligible,
		// so the ONLY reason it survives is the §43.6 liveness re-check.
		markWithZeroRetention := mark
		markWithZeroRetention.RetentionSeconds = 0
		futureNow := func() time.Time { return time.Now().Add(2 * time.Hour) }
		rep, err := gc.RunSweep(ctx, store, r, markWithZeroRetention, gc.SweepOptions{Now: futureNow})
		if err != nil {
			t.Fatalf("RunSweep: %v", err)
		}
		for _, deletedKey := range rep.Deleted.CanonicalPacks {
			if deletedKey == packKey {
				t.Fatalf("§43.6 violation: revived pack was deleted")
			}
		}
		// Verify the pack is classified as "revived", not just silently skipped.
		foundRevived := false
		for _, s := range rep.Skipped {
			if s.Key == packKey && s.Reason == "revived" {
				foundRevived = true
			}
		}
		if !foundRevived {
			t.Fatalf("expected revived skip entry for %s, got %+v", packKey, rep.Skipped)
		}
		if _, err := store.Head(ctx, packKey); err != nil {
			t.Fatalf("revived pack must still exist: %v", err)
		}
	})
}
