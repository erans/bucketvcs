// Package manifesttest provides test helpers for constructing
// manifest bodies (including sharded layouts) without duplicating
// shard-marshal logic. Importing refstore is fine here because this
// package is consumed by tests outside the refstore→manifest cycle.
package manifesttest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// MakeShardedBody builds a v2 Body, writing every shard object to
// store at the appropriate key. Tolerates storage.ErrAlreadyExists
// (content-addressing makes the write idempotent).
func MakeShardedBody(ctx context.Context, store storage.ObjectStore, k *keys.Repo, defaultBranch string, refs map[string]string) (manifest.Body, error) {
	perShard := map[string]map[string]string{}
	for name, oid := range refs {
		sid := refstore.ShardKey(name)
		if perShard[sid] == nil {
			perShard[sid] = map[string]string{}
		}
		perShard[sid][name] = oid
	}
	var shards []manifest.RefShard
	for sid, sr := range perShard {
		contents, hash, err := refstore.MarshalAndHash(sr)
		if err != nil {
			return manifest.Body{}, fmt.Errorf("manifesttest: marshal shard %s: %w", sid, err)
		}
		key := k.RefShardKey(hash)
		if _, err := store.PutIfAbsent(ctx, key, bytes.NewReader(contents), nil); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
			return manifest.Body{}, fmt.Errorf("manifesttest: PutIfAbsent %s: %w", key, err)
		}
		shards = append(shards, manifest.RefShard{
			Shard:    sid,
			Key:      key,
			Hash:     hash,
			RefCount: len(sr),
		})
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i].Shard < shards[j].Shard })
	return manifest.Body{
		DefaultBranch: defaultBranch,
		RefShards:     shards,
		RefSharding:   "hash_v1",
		Packs:         []manifest.PackEntry{},
		Bundles:       []manifest.BundleEntry{},
	}, nil
}
