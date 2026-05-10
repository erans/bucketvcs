package sweeps_test

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc/sweeps"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestWrite_PutIfAbsent(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	k, _ := keys.NewRepo("acme", "site")
	ctx := context.Background()
	r := sweeps.Record{SchemaVersion: 1, SweepID: "sw_01HZ", MarkID: "mk_01HZ", StartedAt: time.Now()}
	if err := sweeps.Write(ctx, store, k, r); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sweeps.Write(ctx, store, k, r); err == nil {
		t.Fatal("second Write must fail (PutIfAbsent)")
	}
}
