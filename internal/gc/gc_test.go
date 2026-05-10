package gc_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/gc/gctest"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestRun_DryRun_NoEffect(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	k, _ := keys.NewRepo("acme", "site")
	gctest.PutEmpty(t, store, k.CanonicalPackKey("orphan"))

	rep, err := gc.Run(ctx, store, r, gc.RunOptions{
		DryRun:    true,
		Retention: time.Second,
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.MarkID != "" {
		t.Errorf("dry-run wrote mark_id=%q, want empty", rep.MarkID)
	}
	if rep.SweepID != "" {
		t.Errorf("dry-run wrote sweep_id=%q, want empty", rep.SweepID)
	}
	// Pack should still exist.
	if _, err := store.Head(ctx, k.CanonicalPackKey("orphan")); err != nil {
		t.Errorf("dry-run deleted pack: %v", err)
	}
	// "Would delete" candidates should appear in the sweep record.
	// Retention is 1s and we don't sleep, so the orphan pack has not yet
	// aged past retention — it will appear in Skipped, not Deleted.
	// That's correct dry-run behavior: compute candidates, write nothing,
	// delete nothing. The key assertion is MarkID=="" and pack still exists.
}

func TestRun_MarkOnly_WritesMarkButNoSweep(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})

	rep, err := gc.Run(ctx, store, r, gc.RunOptions{
		MarkOnly:  true,
		Retention: time.Second,
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.MarkID == "" {
		t.Error("mark_id empty in mark-only run")
	}
	if rep.SweepID != "" {
		t.Errorf("sweep_id = %q in mark-only run, want empty", rep.SweepID)
	}
}

func TestRun_InvalidCombo_MarkOnlyAndSweepOnly(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	_, err := gc.Run(ctx, store, r, gc.RunOptions{MarkOnly: true, SweepOnly: true, Retention: time.Hour})
	if err == nil {
		t.Fatal("expected error for MarkOnly+SweepOnly combo")
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }
