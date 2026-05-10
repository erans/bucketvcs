package gc_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/gc/gctest"
	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestRun_DryRun_NoEffect(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	k, _ := keys.NewRepo("acme", "site")
	gctest.PutEmpty(t, store, k.CanonicalPackKey("orphan"))

	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))

	// Phase 1: mark-only run writes the mark record with firstSeenUnreachableAt = now.
	// DryRun is false so the mark is persisted to disk for phase 2.
	_, err := gc.Run(ctx, store, r, gc.RunOptions{
		MarkOnly:  true,
		Retention: time.Second,
		Logger:    logger,
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("MarkOnly Run: %v", err)
	}

	// Sleep so the orphan pack ages past the 1s retention floor before sweep.
	time.Sleep(1100 * time.Millisecond)

	// Phase 2: sweep-only dry-run against the mark written in phase 1.
	// now - firstSeenUnreachableAt ≈ 1.1s > 1s → would-delete branch fires.
	rep, err := gc.Run(ctx, store, r, gc.RunOptions{
		SweepOnly: true,
		DryRun:    true,
		Retention: time.Second,
		Logger:    logger,
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("SweepOnly DryRun Run: %v", err)
	}
	if rep.SweepID != "" {
		t.Errorf("dry-run wrote sweep_id=%q, want empty", rep.SweepID)
	}
	// Pack should still exist (dry-run must not delete anything).
	if _, err := store.Head(ctx, k.CanonicalPackKey("orphan")); err != nil {
		t.Errorf("dry-run deleted pack: %v", err)
	}
	// "Would delete" branch must have fired: the orphan pack aged past retention
	// and should appear in Deleted, not Skipped.
	if len(rep.SweepRecord.Deleted.CanonicalPacks) != 1 {
		t.Errorf("dry-run would-delete canonical_packs = %d, want 1", len(rep.SweepRecord.Deleted.CanonicalPacks))
	}
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
	if !errors.Is(err, gc.ErrInvalidPhaseCombo) {
		t.Fatalf("err = %v, want ErrInvalidPhaseCombo", err)
	}
}

func TestRun_SweepOnly_NoPriorMark_ReturnsErrNoMarkForSweep(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	_, err := gc.Run(ctx, store, r, gc.RunOptions{
		SweepOnly: true,
		Retention: time.Second,
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		Now:       time.Now,
	})
	if !errors.Is(err, gc.ErrNoMarkForSweep) {
		t.Fatalf("err = %v, want ErrNoMarkForSweep", err)
	}
}

func TestRun_SweepOnly_WithExistingMark_RunsSweep(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})

	// Phase 1: produce a mark via mark-only.
	mrep, err := gc.Run(ctx, store, r, gc.RunOptions{
		MarkOnly:  true,
		Retention: time.Second,
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("MarkOnly Run: %v", err)
	}
	if mrep.MarkID == "" {
		t.Fatal("MarkOnly produced empty MarkID")
	}

	// Phase 2: sweep-only against that mark.
	srep, err := gc.Run(ctx, store, r, gc.RunOptions{
		SweepOnly: true,
		Retention: time.Second,
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("SweepOnly Run: %v", err)
	}
	if srep.SweepID == "" {
		t.Error("SweepOnly produced empty SweepID")
	}
	if srep.MarkID != mrep.MarkID {
		t.Errorf("SweepOnly MarkID = %q, want %q", srep.MarkID, mrep.MarkID)
	}
}

func TestRun_CorruptManifestBody_AbortsCleanly(t *testing.T) {
	base, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	if _, err := repo.Create(ctx, base, "acme", "site", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Wrap base to corrupt the root manifest body on Get, so that the schema
	// gate (which only checks header fields) passes but BuildLiveSet's
	// json.Unmarshal into manifest.Body fails on the body fields.
	manifestKey := "tenants/acme/repos/site/manifest/root.json"
	wrapped := &corruptManifestStore{ObjectStore: base, manifestKey: manifestKey}

	// repo.Open uses the wrapped store; schema gate passes (header is valid),
	// so Open succeeds. r.ReadRoot inside RunMark will return the corrupt body.
	r, err := repo.Open(ctx, wrapped, "acme", "site")
	if err != nil {
		t.Fatalf("Open with corrupt wrapper: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))
	_, err = gc.Run(ctx, wrapped, r, gc.RunOptions{
		Retention: time.Hour,
		Logger:    logger,
		Now:       time.Now,
	})
	if err == nil {
		t.Fatal("expected gc.Run to return an error when manifest body is corrupt")
	}
	if !strings.Contains(err.Error(), "build live set") && !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention build live set or parse failure; got: %v", err)
	}

	// Verify NO mark record was written (gc.Run must have aborted before marks.Write).
	k, _ := keys.NewRepo("acme", "site")
	if _, merr := marks.ReadLatest(ctx, base, k); !errors.Is(merr, marks.ErrNotFound) {
		t.Errorf("expected no mark record after abort; got err=%v", merr)
	}
}

// corruptManifestStore wraps an ObjectStore and replaces the root manifest
// body with a corrupt "packs" field type on Get. The header fields remain
// valid so the §43.7 schema gate passes, but BuildLiveSet's body unmarshal
// into manifest.Body (which expects []PackEntry) will fail.
type corruptManifestStore struct {
	storage.ObjectStore
	manifestKey string
}

func (c *corruptManifestStore) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	if key != c.manifestKey {
		return c.ObjectStore.Get(ctx, key, opts)
	}
	// Valid header fields; "packs" is a string instead of an array so that
	// json.Unmarshal into manifest.Body.Packs []PackEntry fails.
	corrupt := `{"schema_version":1,"min_reader_version":"0.1.0","repo_id":"site","repo_format":{"object_format":"sha1","compatibility":["sha1"]},"manifest_version":1,"latest_tx":"","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","packs":"not-an-array"}`
	return &storage.Object{
		Body:     io.NopCloser(strings.NewReader(corrupt)),
		Metadata: storage.ObjectMetadata{Key: key, Version: storage.ObjectVersion{Token: "v1"}},
	}, nil
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }
