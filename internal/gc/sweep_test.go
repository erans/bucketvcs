package gc_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/gc/gctest"
	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestSweep_RetentionNotMet_DefersAll(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	k, _ := keys.NewRepo("acme", "site")
	gctest.PutEmpty(t, store, k.CanonicalPackKey("orphan-pack"))

	mark, err := gc.RunMark(ctx, store, r, gc.MarkOptions{Now: time.Now, RetentionSeconds: 3600})
	if err != nil {
		t.Fatalf("RunMark: %v", err)
	}
	if err := marks.Write(ctx, store, k, mark); err != nil {
		t.Fatalf("marks.Write: %v", err)
	}

	rep, err := gc.RunSweep(ctx, store, r, mark, gc.SweepOptions{Now: time.Now})
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	if len(rep.Deleted.CanonicalPacks) != 0 {
		t.Fatalf("deleted %d, want 0 (retention not met)", len(rep.Deleted.CanonicalPacks))
	}
	if len(rep.Skipped) == 0 {
		t.Fatal("expected skipped entries with retention_not_met reason")
	}
	for _, s := range rep.Skipped {
		if s.Reason != "retention_not_met" {
			t.Errorf("reason = %q, want retention_not_met", s.Reason)
		}
	}
}

func TestSweep_RetentionMet_DeletesOrphan(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	k, _ := keys.NewRepo("acme", "site")
	gctest.PutEmpty(t, store, k.CanonicalPackKey("orphan-pack"))

	// Use a 1-second retention so we can wait it out in test time.
	mark, _ := gc.RunMark(ctx, store, r, gc.MarkOptions{Now: time.Now, RetentionSeconds: 1})
	_ = marks.Write(ctx, store, k, mark)

	time.Sleep(1100 * time.Millisecond)

	rep, err := gc.RunSweep(ctx, store, r, mark, gc.SweepOptions{Now: time.Now})
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	if len(rep.Deleted.CanonicalPacks) != 1 {
		t.Fatalf("deleted=%d, want 1; report=%+v", len(rep.Deleted.CanonicalPacks), rep)
	}
}

func TestSweep_Revived_SkippedWithRevivedReason(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	k, _ := keys.NewRepo("acme", "site")

	packKey := k.CanonicalPackKey("revived-pack")
	gctest.PutEmpty(t, store, packKey)

	// Mark says it is a candidate; old enough to bypass retention.
	mark := marks.Record{
		SchemaVersion:    1,
		MarkID:           "mk_01HZTEST",
		StartedAt:        time.Now().Add(-time.Hour),
		CompletedAt:      time.Now().Add(-time.Hour),
		RetentionSeconds: 1,
		Candidates: marks.Candidates{
			CanonicalPacks: []marks.PackCandidate{
				{Key: packKey, FirstSeenUnreachableAt: time.Now().Add(-time.Hour)},
			},
		},
	}

	// Revive it: push a manifest that references the pack.
	_, err := r.Commit(ctx, makePushTxBody(),
		func(prev *repo.RootView) ([]byte, error) {
			return makeBodyWithPack(prev.Body, packKey)
		},
	)
	if err != nil {
		t.Fatalf("Commit (revival): %v", err)
	}

	rep, err := gc.RunSweep(ctx, store, r, mark, gc.SweepOptions{Now: time.Now})
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	if len(rep.Deleted.CanonicalPacks) != 0 {
		t.Fatalf("deleted=%d, want 0 (was revived)", len(rep.Deleted.CanonicalPacks))
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0].Reason != "revived" {
		t.Fatalf("skipped=%+v, want 1 entry with reason=revived", rep.Skipped)
	}
}

func TestGC_OrphanPack_FromCrashedImport(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	k, _ := keys.NewRepo("acme", "site")

	// Simulate a crashed import: pack uploaded, manifest never updated.
	gctest.PutEmpty(t, store, k.CanonicalPackKey("crashed-import-pack"))

	// Mark with 1s retention.
	rep1, err := gc.Run(ctx, store, r, gc.RunOptions{Retention: time.Second})
	if err != nil {
		t.Fatalf("Run mark: %v", err)
	}
	if len(rep1.SweepRecord.Skipped) == 0 {
		t.Fatal("expected first run to skip due to retention_not_met")
	}

	time.Sleep(1100 * time.Millisecond)

	rep2, err := gc.Run(ctx, store, r, gc.RunOptions{Retention: time.Second})
	if err != nil {
		t.Fatalf("Run second: %v", err)
	}
	if len(rep2.SweepRecord.Deleted.CanonicalPacks) != 1 {
		t.Fatalf("second run deleted %d, want 1", len(rep2.SweepRecord.Deleted.CanonicalPacks))
	}
	if _, err := store.Head(ctx, k.CanonicalPackKey("crashed-import-pack")); err == nil {
		t.Error("orphan pack still present after sweep")
	}
}

func TestGC_PushDuringSweep_43_6(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	k, _ := keys.NewRepo("acme", "site")

	packKey := k.CanonicalPackKey("contended-pack")
	gctest.PutEmpty(t, store, packKey)

	// Mark with 0s retention so sweep is immediately eligible.
	mark, _ := gc.RunMark(ctx, store, r, gc.MarkOptions{Now: time.Now, RetentionSeconds: 0})
	_ = marks.Write(ctx, store, k, mark)

	// Concurrent push references the pack BEFORE sweep runs.
	if _, err := r.Commit(ctx, makePushTxBody(),
		func(prev *repo.RootView) ([]byte, error) {
			return makeBodyWithPack(prev.Body, packKey)
		},
	); err != nil {
		t.Fatalf("Commit (revival before sweep): %v", err)
	}

	rep, err := gc.RunSweep(ctx, store, r, mark, gc.SweepOptions{Now: time.Now})
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	if len(rep.Deleted.CanonicalPacks) != 0 {
		t.Fatal("§43.6 violation: revived pack was deleted")
	}
	foundRevived := false
	for _, s := range rep.Skipped {
		if s.Reason == "revived" && s.Key == packKey {
			foundRevived = true
		}
	}
	if !foundRevived {
		t.Errorf("expected skipped entry with reason=revived for %s, got %+v", packKey, rep.Skipped)
	}
}

func makePushTxBody() tx.Body {
	return tx.Body{Type: "push", Actor: "u_test"}
}

func makeBodyWithPack(prev []byte, packKey string) ([]byte, error) {
	var body manifest.Body
	if err := json.Unmarshal(prev, &body); err != nil {
		return nil, err
	}
	body.Packs = append(body.Packs, manifest.PackEntry{
		PackID:  "revived",
		PackKey: packKey,
		IdxKey:  packKey + ".idx",
	})
	return manifest.MarshalBody(body)
}
