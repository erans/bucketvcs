package tx_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestWriteCommitMarker_CreatesZeroByteSibling(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	ctx := context.Background()
	markerKey := "tenants/acme/repos/site/tx/tx_01HZSAMPLE.json.commit"

	if err := tx.WriteCommitMarker(ctx, store, markerKey); err != nil {
		t.Fatalf("WriteCommitMarker: %v", err)
	}

	obj, err := store.Get(ctx, markerKey, nil)
	if err != nil {
		t.Fatalf("Get marker: %v", err)
	}
	defer obj.Body.Close()
	if obj.Metadata.Size != 0 {
		t.Fatalf("marker size = %d, want 0", obj.Metadata.Size)
	}
}

func TestWriteCommitMarker_IdempotentOnExisting(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	ctx := context.Background()
	key := "tenants/acme/repos/site/tx/tx_01HZSAMPLE.json.commit"

	// Pre-create with non-zero contents so we can detect any overwrite.
	if _, err := store.PutIfAbsent(ctx, key, strings.NewReader("not-empty"), nil); err != nil {
		t.Fatalf("seed PutIfAbsent: %v", err)
	}

	// Second write must NOT return an error and must NOT overwrite.
	if err := tx.WriteCommitMarker(ctx, store, key); err != nil {
		t.Fatalf("WriteCommitMarker on existing key: %v", err)
	}

	obj, err := store.Get(ctx, key, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	if obj.Metadata.Size == 0 {
		t.Fatalf("WriteCommitMarker overwrote existing object")
	}
	_ = storage.ObjectMetadata{} // keep the import live if linters complain
}
