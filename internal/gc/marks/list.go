package marks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// List returns all mark IDs in this repo sorted by ULID descending
// (newest first). Mark records keyed by k.GCMarkKey(<mark_id>) where
// keys ends in "<mark_id>.json".
func List(ctx context.Context, s storage.ObjectStore, k *keys.Repo) ([]string, error) {
	prefix := k.Prefix() + "gc/marks/"
	var ids []string
	var token string
	for {
		page, err := s.List(ctx, prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return nil, fmt.Errorf("marks: list: %w", err)
		}
		for _, obj := range page.Objects {
			rest := strings.TrimPrefix(obj.Key, prefix)
			if !strings.HasSuffix(rest, ".json") {
				continue
			}
			id := strings.TrimSuffix(rest, ".json")
			if id == "" {
				continue
			}
			ids = append(ids, id)
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}
