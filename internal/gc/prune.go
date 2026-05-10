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
// Storage transients (ErrNotFound, ErrVersionMismatch) on individual
// deletes are tolerated and the loop continues. Other errors abort
// the prune and return wrapped — the caller (gc.Run) treats these as
// non-fatal sweep housekeeping issues and demotes them to warnings
// (see gc.Run's PruneMarks call site for the policy).
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
