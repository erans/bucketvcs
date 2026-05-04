package tx

import (
	"context"
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Write marshals the record and stores it at key with PutIfAbsent
// (create-only). Returns storage.ErrAlreadyExists if the key already
// has an object — under normal operation this is impossible because
// txID is a fresh ULID, but the test suite can provoke it.
func Write(ctx context.Context, s storage.ObjectStore, key string, h Header, b Body) error {
	bytes, err := Marshal(h, b)
	if err != nil {
		return err
	}
	if _, err := s.PutIfAbsent(ctx, key, strings.NewReader(string(bytes)), nil); err != nil {
		return fmt.Errorf("tx: write %s: %w", key, err)
	}
	return nil
}
