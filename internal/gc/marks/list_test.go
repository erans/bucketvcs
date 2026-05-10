package marks_test

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestList_ReturnsULIDsDescending(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	k, _ := keys.NewRepo("acme", "site")
	ctx := context.Background()
	for _, id := range []string{"mk_01HZ001", "mk_01HZ003", "mk_01HZ002"} {
		if err := marks.Write(ctx, store, k, marks.Record{SchemaVersion: 1, MarkID: id, StartedAt: time.Now()}); err != nil {
			t.Fatalf("Write %s: %v", id, err)
		}
	}
	got, err := marks.List(ctx, store, k)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	want := []string{"mk_01HZ003", "mk_01HZ002", "mk_01HZ001"}
	for i, id := range want {
		if got[i] != id {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], id)
		}
	}
}
