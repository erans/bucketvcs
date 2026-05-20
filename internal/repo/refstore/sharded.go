package refstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ShardedRefStore wraps Body.RefShards plus an ObjectStore. Lookup
// fetches the single shard whose bucket matches the refname; List
// parallel-fetches all shards and merges. Every fetched shard is
// content-hash verified against body.RefShards[i].Hash before parse —
// ErrShardCorrupt is fatal. Stage builds the push-time delta: groups
// updates by shard, loads only the affected shards (parallel), applies
// the updates, and returns the new []ShardWrite + new []RefShard for
// the caller to publish atomically.
type ShardedRefStore struct {
	store  storage.ObjectStore
	keys   *keys.Repo
	shards []manifest.RefShard
}

func newShardedRefStore(s storage.ObjectStore, k *keys.Repo, body *manifest.Body) *ShardedRefStore {
	// Defensive copy of the shard list so callers can mutate body
	// without corrupting the store's view.
	cp := make([]manifest.RefShard, len(body.RefShards))
	copy(cp, body.RefShards)
	return &ShardedRefStore{store: s, keys: k, shards: cp}
}

// Mode returns ModeSharded.
func (s *ShardedRefStore) Mode() Mode { return ModeSharded }

// Lookup hashes refname to its shard, fetches that one shard, verifies
// the content hash, and returns (oid, exists) from the shard map.
// Missing shard (refname's bucket not present in body.RefShards) is
// not an error — it means "no refs with that shard exist" which
// implies "refname not present."
//
// ErrShardCorrupt is returned if the fetched shard's sha256 does not
// match body.RefShards[i].Hash. Callers MUST treat this as fatal —
// retrying may amplify damage.
func (s *ShardedRefStore) Lookup(ctx context.Context, refname string) (string, bool, error) {
	sid := ShardKey(refname)
	for i := range s.shards {
		if s.shards[i].Shard != sid {
			continue
		}
		refs, err := s.fetchShard(ctx, s.shards[i])
		if err != nil {
			return "", false, err
		}
		oid, ok := refs[refname]
		return oid, ok, nil
	}
	return "", false, nil
}

// fetchShard reads one shard object from the store, verifies its
// content hash, and returns the parsed map. Used by Lookup, List, and
// Stage (Phase 3). Hash verification is the tampering canary required
// by the spec.
func (s *ShardedRefStore) fetchShard(ctx context.Context, ref manifest.RefShard) (map[string]string, error) {
	obj, err := s.store.Get(ctx, ref.Key, nil)
	if err != nil {
		return nil, fmt.Errorf("refstore: fetch shard %s (%s): %w", ref.Shard, ref.Key, err)
	}
	defer obj.Body.Close()
	raw, err := io.ReadAll(obj.Body)
	if err != nil {
		return nil, fmt.Errorf("refstore: read shard %s body: %w", ref.Shard, err)
	}
	got := hashShardContent(raw)
	if got != ref.Hash {
		return nil, fmt.Errorf("%w: shard %s want %s got %s", ErrShardCorrupt, ref.Shard, ref.Hash, got)
	}
	var refs map[string]string
	if err := json.Unmarshal(raw, &refs); err != nil {
		return nil, fmt.Errorf("refstore: parse shard %s: %w", ref.Shard, err)
	}
	return refs, nil
}

// List parallel-fetches every shard listed in body.RefShards,
// verifies each one's content hash, and returns the merged map.
// On the first error the group's context is cancelled, which aborts
// any not-yet-issued store.Get calls; in-flight body reads run to
// completion (acceptable: shards are small).
func (s *ShardedRefStore) List(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string)
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	for i := range s.shards {
		shard := s.shards[i]
		g.Go(func() error {
			refs, err := s.fetchShard(gctx, shard)
			if err != nil {
				return err
			}
			mu.Lock()
			for k, v := range refs {
				out[k] = v
			}
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// Stage groups updates by shard, loads only the affected shards from
// the store (parallel; same hash-verification as Lookup/List), applies
// the updates to those shards in memory, computes the new canonical
// content + hash for each, and returns the full Stage.
//
// Untouched shards are forwarded into NewRefShards verbatim (same
// hash/key) so the new body still references them. A shard that
// becomes empty after applied deletions is dropped from NewRefShards
// entirely; no "{}" shard object is written.
//
// Deletion convention matches InlineRefStore.Stage: empty OID or
// 40-zero nullOIDHex deletes; any other 40-hex OID upserts.
func (s *ShardedRefStore) Stage(ctx context.Context, updates map[string]string) (Stage, error) {
	if len(updates) == 0 {
		// No changes: return the existing shard list as-is.
		out := make([]manifest.RefShard, len(s.shards))
		copy(out, s.shards)
		return Stage{Mode: ModeSharded, NewRefShards: out}, nil
	}

	// Group updates by shard ID.
	updatesByShard := make(map[string]map[string]string)
	for refname, oid := range updates {
		sid := ShardKey(refname)
		if updatesByShard[sid] == nil {
			updatesByShard[sid] = make(map[string]string)
		}
		updatesByShard[sid][refname] = oid
	}

	// Index existing shards by ID for O(1) lookup.
	existingByShard := make(map[string]manifest.RefShard, len(s.shards))
	for _, sh := range s.shards {
		existingByShard[sh.Shard] = sh
	}

	// Load existing contents for every affected shard (parallel).
	loadedRefs := make(map[string]map[string]string, len(updatesByShard))
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	for sid := range updatesByShard {
		sid := sid
		existing, exists := existingByShard[sid]
		if !exists {
			// No existing shard for this ID; start from empty.
			mu.Lock()
			loadedRefs[sid] = map[string]string{}
			mu.Unlock()
			continue
		}
		g.Go(func() error {
			refs, err := s.fetchShard(gctx, existing)
			if err != nil {
				return err
			}
			mu.Lock()
			loadedRefs[sid] = refs
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return Stage{}, err
	}

	// Apply updates per shard, compute new content + hash for changed shards.
	var writes []ShardWrite
	var newShards []manifest.RefShard

	// Sort shard IDs for deterministic output across runs.
	changedIDs := make([]string, 0, len(updatesByShard))
	for sid := range updatesByShard {
		changedIDs = append(changedIDs, sid)
	}
	sort.Strings(changedIDs)

	for _, sid := range changedIDs {
		refs := loadedRefs[sid]
		for refname, oid := range updatesByShard[sid] {
			if oid == "" || oid == nullOIDHex {
				delete(refs, refname)
				continue
			}
			refs[refname] = oid
		}
		if len(refs) == 0 {
			// Shard emptied — drop it entirely (no "{}" object).
			continue
		}
		contents, err := marshalShardContent(refs)
		if err != nil {
			return Stage{}, fmt.Errorf("refstore: marshal shard %s: %w", sid, err)
		}
		hash := hashShardContent(contents)
		// If the new content's hash matches what's already stored at this
		// shard ID, reuse the existing key and skip the PUT (idempotent
		// re-stage during CAS retry).
		existing, ok := existingByShard[sid]
		unchanged := ok && existing.Hash == hash
		var key string
		if unchanged {
			key = existing.Key
		} else {
			key = s.keys.RefShardKey(hash)
		}
		newShards = append(newShards, manifest.RefShard{
			Shard:    sid,
			Key:      key,
			Hash:     hash,
			RefCount: len(refs),
		})
		if !unchanged {
			writes = append(writes, ShardWrite{
				Shard:    sid,
				Key:      key,
				Hash:     hash,
				Contents: contents,
			})
		}
	}

	// Forward untouched shards (those not in updatesByShard).
	for _, sh := range s.shards {
		if _, touched := updatesByShard[sh.Shard]; touched {
			continue
		}
		newShards = append(newShards, sh)
	}

	// Sort newShards by Shard ID for canonical body shape.
	sort.Slice(newShards, func(i, j int) bool { return newShards[i].Shard < newShards[j].Shard })

	return Stage{
		Mode:            ModeSharded,
		NewShardObjects: writes,
		NewRefShards:    newShards,
	}, nil
}
