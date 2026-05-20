// Package refstore abstracts ref reads, writes, and staging behind a
// single interface (RefStore) so callers do not need to know whether
// the underlying root manifest stores refs inline (v1) or in
// content-addressed shards (v2).
//
// Two implementations:
//
//   - InlineRefStore wraps Body.Refs directly. Lookup and List are
//     pure in-memory; Stage records the merged map for the caller.
//     Fully implemented from M12 Phase 1.
//
//   - ShardedRefStore wraps Body.RefShards plus an ObjectStore. Lookup
//     fetches exactly one shard (keyed by the first byte of sha256(refname))
//     and verifies the sha256 content hash before parsing. List
//     parallel-fetches every shard listed in the body via errgroup,
//     verifies each shard's sha256 content hash (returning ErrShardCorrupt
//     on mismatch — this is a tampering canary, not a retryable error),
//     and returns the merged ref map. Stage hash-buckets all updates into
//     256 shard slots, merges each slot with existing shard contents, and
//     returns a ShardWrite list for changed shards plus a new []RefShard
//     slice; unchanged shards are reused by key (idempotent content-
//     addressed reuse). Empty shards are dropped — no "{}" object is
//     written. All three methods (Lookup, List, Stage) are fully
//     implemented as of M12 Phase 3.
//
// The ShardKey function (sha256 of refname, first byte hex) is the
// only sharding strategy M12 ships. The ref_sharding string in the
// body schema gates future strategies; UnmarshalBody rejects unknown
// values.
//
// Push integration: the caller mints a Stage from updates, writes
// stage.NewShardObjects via PutIfAbsent inside Repo.Commit's buildBody
// callback (before returning the new body bytes), and assigns
// stage.NewRefShards to the new body. The root manifest CAS remains
// the only commit point; orphan shard objects from aborted pushes are
// content-addressed and become GC candidates after retention.
package refstore
