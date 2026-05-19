package lfs

import (
	"context"
	"errors"
	"fmt"
)

// ErrVerifySizeMismatch is returned by Verify when the stored object
// exists but its size does not match the claimed size. The handler
// maps it to HTTP 422.
var ErrVerifySizeMismatch = errors.New("lfs: verify size mismatch")

// ErrVerifyNotFound is returned by Verify when the stored object is
// absent. The handler maps it to HTTP 404.
var ErrVerifyNotFound = errors.New("lfs: verify object not found")

// Verify confirms that an object with the given oid exists in the
// store and has the claimed size. Returns:
//
//   - nil: object exists and size matches.
//   - ErrVerifySizeMismatch: object exists with a different size.
//   - ErrVerifyNotFound: object is missing.
//   - other error: backend failure.
//
// Verify is pure logic over Store.Head; no side effects, no HTTP.
func Verify(ctx context.Context, store *Store, oid string, claimedSize int64) error {
	size, exists, err := store.Head(ctx, oid)
	if err != nil {
		return fmt.Errorf("lfs.Verify: head: %w", err)
	}
	if !exists {
		return ErrVerifyNotFound
	}
	if size != claimedSize {
		return fmt.Errorf("%w: stored=%d, claimed=%d", ErrVerifySizeMismatch, size, claimedSize)
	}
	return nil
}
