package gc_test

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/gc/gctest"
	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestMark_FirstRun_NoPriorMark_FirstSeenIsNow(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, err := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	k, _ := keys.NewRepo("acme", "site")
	gctest.PutEmpty(t, store, k.CanonicalPackKey("orphan-pack"))

	before := time.Now().Add(-time.Second)
	mark, err := gc.RunMark(ctx, store, r, gc.MarkOptions{Now: time.Now, RetentionSeconds: 60})
	if err != nil {
		t.Fatalf("RunMark: %v", err)
	}
	if len(mark.Candidates.CanonicalPacks) != 1 {
		t.Fatalf("got %d pack candidates, want 1", len(mark.Candidates.CanonicalPacks))
	}
	got := mark.Candidates.CanonicalPacks[0]
	if got.FirstSeenUnreachableAt.Before(before) {
		t.Errorf("FirstSeenUnreachableAt %v < before %v", got.FirstSeenUnreachableAt, before)
	}
	if mark.PreviousMarkID != "" {
		t.Errorf("PreviousMarkID = %q, want empty", mark.PreviousMarkID)
	}
}

func TestMark_SecondRun_CarriesForwardFirstSeen(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	k, _ := keys.NewRepo("acme", "site")
	gctest.PutEmpty(t, store, k.CanonicalPackKey("orphan-pack"))

	first, err := gc.RunMark(ctx, store, r, gc.MarkOptions{Now: time.Now, RetentionSeconds: 60})
	if err != nil {
		t.Fatalf("RunMark first: %v", err)
	}
	if err := marks.Write(ctx, store, k, first); err != nil {
		t.Fatalf("marks.Write: %v", err)
	}
	originalFirstSeen := first.Candidates.CanonicalPacks[0].FirstSeenUnreachableAt

	// Wait an obvious amount.
	time.Sleep(10 * time.Millisecond)

	second, err := gc.RunMark(ctx, store, r, gc.MarkOptions{Now: time.Now, RetentionSeconds: 60})
	if err != nil {
		t.Fatalf("RunMark second: %v", err)
	}
	if second.PreviousMarkID != first.MarkID {
		t.Errorf("PreviousMarkID = %q, want %q", second.PreviousMarkID, first.MarkID)
	}
	got := second.Candidates.CanonicalPacks[0].FirstSeenUnreachableAt
	if !got.Equal(originalFirstSeen) {
		t.Errorf("FirstSeenUnreachableAt = %v, want carry-forward %v", got, originalFirstSeen)
	}
}

func TestMark_TxOrphanSweepArmed_StickyOnceTrue(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	// Create writes a marker, so armed should be true on first mark.
	first, err := gc.RunMark(ctx, store, r, gc.MarkOptions{Now: time.Now, RetentionSeconds: 60})
	if err != nil {
		t.Fatalf("RunMark: %v", err)
	}
	if !first.TxOrphanSweepArmed {
		t.Fatal("first mark must be armed (Create wrote a marker)")
	}
}
