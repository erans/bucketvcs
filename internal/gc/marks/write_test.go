package marks_test

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestWrite_PutIfAbsentSucceedsOnce(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	k, _ := keys.NewRepo("acme", "site")
	ctx := context.Background()
	r := marks.Record{SchemaVersion: 1, MarkID: "mk_01HZSAMPLE", StartedAt: time.Now()}

	if err := marks.Write(ctx, store, k, r); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := marks.Write(ctx, store, k, r); err == nil {
		t.Fatal("second Write must fail (PutIfAbsent semantics)")
	}
}
