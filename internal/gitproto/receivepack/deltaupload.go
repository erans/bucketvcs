package receivepack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ErrDeltaCollision is returned when the destination .bvrd key already
// contains bytes that differ from what we want to write. This indicates
// a SHA-256 collision (practically impossible) or storage corruption.
// Pushes should be aborted; the operator must investigate.
var ErrDeltaCollision = errors.New("receivepack: .bvrd key collision against pre-existing bytes")

// uploadDelta encodes d, content-addresses it to a .bvrd key, and
// uploads via PutIfAbsent. Returns the IndexRef to append to
// manifest.Indexes.Reachability.Deltas.
//
// Idempotent: if the same bytes already exist at the key (same hash),
// the upload is treated as a no-op and success is returned.
// If different bytes exist at the same SHA-256 key, ErrDeltaCollision
// is returned (this should never happen in practice).
func uploadDelta(ctx context.Context, store storage.ObjectStore, k *keys.Repo, d deltaindex.Delta) (manifest.IndexRef, error) {
	bts, err := deltaindex.Encode(d)
	if err != nil {
		return manifest.IndexRef{}, fmt.Errorf("encode .bvrd: %w", err)
	}
	sum := sha256.Sum256(bts)
	hash := hex.EncodeToString(sum[:])
	key := k.ReachabilityDeltaKey(hash)

	_, err = store.PutIfAbsent(ctx, key, bytes.NewReader(bts), nil)
	if err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			// Verify that the existing bytes match. Same SHA-256 means same
			// bytes (content-addressed), so this should always pass.
			existing, readErr := readDeltaObject(ctx, store, key)
			if readErr != nil {
				return manifest.IndexRef{}, fmt.Errorf("read existing .bvrd: %w", readErr)
			}
			if !bytes.Equal(existing, bts) {
				return manifest.IndexRef{}, ErrDeltaCollision
			}
			// Same bytes — idempotent success.
		} else {
			return manifest.IndexRef{}, fmt.Errorf("put .bvrd: %w", err)
		}
	}

	return manifest.IndexRef{
		Key:       key,
		Hash:      hash,
		SizeBytes: int64(len(bts)),
	}, nil
}

// readDeltaObject reads all bytes from the given store key.
func readDeltaObject(ctx context.Context, store storage.ObjectStore, key string) ([]byte, error) {
	obj, err := store.Get(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	defer obj.Body.Close()
	return io.ReadAll(obj.Body)
}
