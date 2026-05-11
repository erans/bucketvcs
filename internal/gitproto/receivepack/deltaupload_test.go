package receivepack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newTestKeys(t *testing.T) *keys.Repo {
	t.Helper()
	k, err := keys.NewRepo("t", "r")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}
	return k
}

func makeDelta11() deltaindex.Delta {
	var oid pack.OID
	oid[0] = 0x11
	return deltaindex.Delta{
		Commits: []deltaindex.CommitRecord{{OID: oid, Generation: 4}},
	}
}

func mustPut(t *testing.T, store storage.ObjectStore, key string, data []byte) {
	t.Helper()
	if _, err := store.PutIfAbsent(context.Background(), key, bytes.NewReader(data), nil); err != nil {
		t.Fatalf("PutIfAbsent(%s): %v", key, err)
	}
}

func TestUploadDelta_HappyPath(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	k := newTestKeys(t)
	d := makeDelta11()

	ref, err := uploadDelta(ctx, store, k, d)
	if err != nil {
		t.Fatalf("uploadDelta: %v", err)
	}
	if ref.Key == "" {
		t.Error("ref.Key is empty")
	}
	if ref.Hash == "" {
		t.Error("ref.Hash is empty")
	}
	if ref.SizeBytes == 0 {
		t.Error("ref.SizeBytes is zero")
	}
}

func TestUploadDelta_Idempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	k := newTestKeys(t)
	d := makeDelta11()

	ref1, err := uploadDelta(ctx, store, k, d)
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}

	// Second upload of same delta should succeed idempotently.
	ref2, err := uploadDelta(ctx, store, k, d)
	if err != nil {
		t.Fatalf("idempotent upload: %v", err)
	}
	if ref1.Key != ref2.Key || ref1.Hash != ref2.Hash {
		t.Errorf("idempotent upload produced different ref: %+v vs %+v", ref1, ref2)
	}
}

func TestUploadDelta_KeyCollisionDifferentBytes(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	k := newTestKeys(t)
	d := makeDelta11()

	// Compute what key uploadDelta would use.
	bts, err := deltaindex.Encode(d)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	sum := sha256.Sum256(bts)
	hash := hex.EncodeToString(sum[:])
	key := k.ReachabilityDeltaKey(hash)

	// Pre-populate with different bytes at the same key.
	mustPut(t, store, key, []byte("not the right bytes"))

	_, err = uploadDelta(ctx, store, k, d)
	if !errors.Is(err, ErrDeltaCollision) {
		t.Fatalf("want ErrDeltaCollision, got %v", err)
	}
}

func TestUploadDelta_KeyPrefix(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	k := newTestKeys(t)
	d := makeDelta11()

	ref, err := uploadDelta(ctx, store, k, d)
	if err != nil {
		t.Fatalf("uploadDelta: %v", err)
	}
	// Key must be under the reachability-delta prefix.
	prefix := k.ReachabilityDeltaPrefix()
	if len(ref.Key) <= len(prefix) || ref.Key[:len(prefix)] != prefix {
		t.Errorf("key %q does not start with prefix %q", ref.Key, prefix)
	}
}
