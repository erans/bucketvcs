package gc_test

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestPruneMarks_KeepsLast10(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	k, _ := keys.NewRepo("acme", "site")
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		id := "mk_01HZ" + string(rune('A'+i))
		_ = marks.Write(ctx, store, k, marks.Record{SchemaVersion: 1, MarkID: id, StartedAt: time.Now()})
	}
	if err := gc.PruneMarks(ctx, store, k, 10); err != nil {
		t.Fatalf("PruneMarks: %v", err)
	}
	got, err := marks.List(ctx, store, k)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("after prune got %d, want 10", len(got))
	}
}
