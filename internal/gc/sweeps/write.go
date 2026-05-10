package sweeps

import (
	"bytes"
	"context"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Write marshals r and stores it via PutIfAbsent at keys.GCSweepKey(r.SweepID).
// Returns the storage error wrapped on failure (callers can errors.Is
// against storage.ErrAlreadyExists).
func Write(ctx context.Context, s storage.ObjectStore, k *keys.Repo, r Record) error {
	b, err := r.MarshalJSON()
	if err != nil {
		return fmt.Errorf("sweeps: marshal: %w", err)
	}
	if _, err := s.PutIfAbsent(ctx, k.GCSweepKey(r.SweepID), bytes.NewReader(b), nil); err != nil {
		return fmt.Errorf("sweeps: put: %w", err)
	}
	return nil
}
