package marks_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestReadLatest_NoMarks_ReturnsErrNotFound(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	k, _ := keys.NewRepo("acme", "site")
	_, err := marks.ReadLatest(context.Background(), store, k)
	if !errors.Is(err, marks.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestReadLatest_ReturnsMostRecent(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	k, _ := keys.NewRepo("acme", "site")
	ctx := context.Background()
	for _, id := range []string{"mk_01HZ001", "mk_01HZ002"} {
		_ = marks.Write(ctx, store, k, marks.Record{SchemaVersion: 1, MarkID: id, StartedAt: time.Now()})
	}
	got, err := marks.ReadLatest(ctx, store, k)
	if err != nil {
		t.Fatalf("ReadLatest: %v", err)
	}
	if got.MarkID != "mk_01HZ002" {
		t.Fatalf("MarkID = %q, want mk_01HZ002", got.MarkID)
	}
}
