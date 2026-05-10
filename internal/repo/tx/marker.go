package tx

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// WriteCommitMarker writes a zero-byte object at markerKey via
// PutIfAbsent. The marker's existence signals that the sibling tx
// record (at strings.TrimSuffix(markerKey, ".commit")) was the winner
// of a §8 CAS that committed a manifest version.
//
// Best-effort semantics:
//   - storage.ErrAlreadyExists is treated as success (idempotent on
//     duplicate calls).
//   - Other errors are returned unchanged so callers can decide whether
//     to log-and-continue (the production policy in repo.Commit /
//     repo.Create — the CAS already committed; the marker is an audit
//     aid, not a correctness guarantee).
func WriteCommitMarker(ctx context.Context, s storage.ObjectStore, markerKey string) error {
	_, err := s.PutIfAbsent(ctx, markerKey, strings.NewReader(""), nil)
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrAlreadyExists) {
		return nil
	}
	return fmt.Errorf("tx: write commit marker %s: %w", markerKey, err)
}
