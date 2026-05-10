package gc

import (
	"context"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// DefaultMarkRecordRetention caps the number of mark records kept on
// disk. Older records are deleted in PruneMarks.
const DefaultMarkRecordRetention = 10

// PruneMarks deletes mark records past the most recent keep records.
// Errors on individual deletes are tolerated (logged via the sweep
// report by the caller); PruneMarks returns the first hard listing
// error if any.
func PruneMarks(ctx context.Context, s storage.ObjectStore, k *keys.Repo, keep int) error {
	if keep < 1 {
		keep = DefaultMarkRecordRetention
	}
	ids, err := marks.List(ctx, s, k)
	if err != nil {
		return err
	}
	if len(ids) <= keep {
		return nil
	}
	for _, id := range ids[keep:] {
		key := k.GCMarkKey(id)
		meta, err := s.Head(ctx, key)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return fmt.Errorf("gc: prune head %s: %w", id, err)
		}
		if err := s.DeleteIfVersionMatches(ctx, key, meta.Version); err != nil {
			if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrVersionMismatch) {
				continue
			}
			return fmt.Errorf("gc: prune delete %s: %w", id, err)
		}
	}
	return nil
}
