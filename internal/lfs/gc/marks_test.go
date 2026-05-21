package gc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/lfs/gc"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestWriteReadLatestMark_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	store, err := localfs.Open(tmp)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rec := gc.MarkRecord{
		SchemaVersion:         1,
		MarkID:                gc.NewMarkID(time.Unix(1700000000, 0)),
		StartedAt:             time.Unix(1700000000, 0).UTC(),
		CompletedAt:           time.Unix(1700000001, 0).UTC(),
		ManifestVersionAtMark: 42,
		RetentionSeconds:      7 * 24 * 3600,
		Candidates: []gc.MarkCandidate{
			{OID: "aa", Key: "tenants/t/repos/r/lfs/objects/aa", SizeBytes: 12, FirstSeenUnreferencedAt: time.Unix(1700000000, 0).UTC()},
		},
	}
	if err := gc.WriteMark(context.Background(), store, "t", "r", rec); err != nil {
		t.Fatalf("WriteMark: %v", err)
	}
	got, err := gc.ReadLatestMark(context.Background(), store, "t", "r")
	if err != nil {
		t.Fatalf("ReadLatestMark: %v", err)
	}
	if got.MarkID != rec.MarkID {
		t.Errorf("MarkID=%q want %q", got.MarkID, rec.MarkID)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].OID != "aa" {
		t.Errorf("Candidates=%v want one aa", got.Candidates)
	}
}

func TestReadLatestMark_NoMarks(t *testing.T) {
	tmp := t.TempDir()
	store, _ := localfs.Open(tmp)
	t.Cleanup(func() { _ = store.Close() })
	_, err := gc.ReadLatestMark(context.Background(), store, "t", "r")
	if !errors.Is(err, gc.ErrNoMarks) {
		t.Errorf("err=%v want ErrNoMarks", err)
	}
}

func TestReadLatestMark_PicksHighestID(t *testing.T) {
	tmp := t.TempDir()
	store, _ := localfs.Open(tmp)
	t.Cleanup(func() { _ = store.Close() })
	// Write 3 marks at different timestamps; latest should win.
	for i, ts := range []int64{1700000000, 1700000200, 1700000100} {
		rec := gc.MarkRecord{
			SchemaVersion: 1,
			MarkID:        gc.NewMarkID(time.Unix(ts, int64(i))),
			StartedAt:     time.Unix(ts, 0).UTC(),
		}
		if err := gc.WriteMark(context.Background(), store, "t", "r", rec); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	got, err := gc.ReadLatestMark(context.Background(), store, "t", "r")
	if err != nil {
		t.Fatalf("ReadLatestMark: %v", err)
	}
	// Latest is the ts=1700000200 record.
	if !got.StartedAt.Equal(time.Unix(1700000200, 0).UTC()) {
		t.Errorf("StartedAt=%v want 1700000200", got.StartedAt)
	}
}
