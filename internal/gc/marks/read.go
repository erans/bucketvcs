package marks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ErrNotFound indicates the repo has no mark records on disk.
var ErrNotFound = errors.New("marks: no mark records found")

// ReadLatest returns the most recent (highest-ULID) mark record. If no
// mark records exist, returns ErrNotFound.
func ReadLatest(ctx context.Context, s storage.ObjectStore, k *keys.Repo) (Record, error) {
	ids, err := List(ctx, s, k)
	if err != nil {
		return Record{}, err
	}
	if len(ids) == 0 {
		return Record{}, ErrNotFound
	}
	return ReadByID(ctx, s, k, ids[0])
}

// ReadByID returns the mark record with the given mark_id.
func ReadByID(ctx context.Context, s storage.ObjectStore, k *keys.Repo, id string) (Record, error) {
	obj, err := s.Get(ctx, k.GCMarkKey(id), nil)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("marks: get %s: %w", id, err)
	}
	defer obj.Body.Close()
	body, err := io.ReadAll(obj.Body)
	if err != nil {
		return Record{}, fmt.Errorf("marks: read %s: %w", id, err)
	}
	var r Record
	if err := json.Unmarshal(body, &r); err != nil {
		return Record{}, fmt.Errorf("marks: parse %s: %w", id, err)
	}
	return r, nil
}
