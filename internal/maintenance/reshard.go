package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ErrConcurrentMutation indicates that a concurrent push won the root
// CAS during a Reshard call. The shard objects this Reshard wrote are
// orphaned (content-addressed; GC sweeps them after retention). The
// operator can retry — the next call will see the new manifest version
// and either re-target it or no-op if it's already v2.
var ErrConcurrentMutation = errors.New("maintenance: concurrent mutation during reshard")

// ReshardOptions configures a Reshard run.
type ReshardOptions struct {
	// Actor is the principal recorded in the tx record. Defaults to
	// "u_op" if empty (matches the maintenance convention).
	Actor string

	// BetweenSnapshotAndCAS is a test hook fired between the body
	// snapshot and the root CAS. Production callers leave it nil.
	BetweenSnapshotAndCAS func()
}

// ReshardReport summarises one Reshard call.
type ReshardReport struct {
	// Outcome is one of: "success" (resharded), "noop" (already v2),
	// "empty_v1_kept" (empty repo, v1 layout preserved, no CAS attempted),
	// "failed_concurrent_mutation", "failed_other".
	Outcome string

	// RefCount on success: number of v1 refs migrated.
	// On noop: total ref count across the already-v2 body's shards
	// (sum of RefShards[i].RefCount).
	// On empty_v1_kept: zero.
	RefCount int

	// ShardCount on success: number of non-empty shards produced.
	// On noop: number of existing shards in the already-v2 body
	// (informational — see RefCount asymmetry above).
	// On empty_v1_kept: zero.
	ShardCount int

	// ManifestVersionFrom / ManifestVersionTo bracket the CAS. Both
	// equal on noop and empty_v1_kept (no CAS attempted in either).
	ManifestVersionFrom uint64
	ManifestVersionTo   uint64

	DurationMS int64
}

// Reshard converts a v1 (inline) repo to v2 (sharded) by reading the
// current refs, sharding them via the hash_v1 strategy, writing each
// shard object via PutIfAbsent, then CAS-publishing a new root
// manifest with RefShards populated and Refs cleared.
//
// On an already-v2 repo this is a no-op (Outcome="noop"; no CAS
// attempted).
//
// On an empty repo (zero refs), no shard objects are written and no
// CAS is attempted — manifest.Body's invariants forbid RefSharding
// set without RefShards, so an empty repo cannot be marked v2. The
// report's Outcome is "empty_v1_kept" so operators can distinguish
// this from a real migration.
//
// On a concurrent push winning the CAS race, Reshard returns
// ErrConcurrentMutation. The already-written shard objects are
// orphans; GC sweeps them after retention. Operators retry — the
// retry sees the new manifest version and either no-ops (if the
// concurrent push happened to bump to v2, unlikely without M12
// involvement) or re-targets the new version.
//
// Tx-type policy: the tx record's Type is "reshard_refs". The
// tx.Body schema documents Type as a free-form classifier (see
// internal/repo/tx/record.go); existing maintenance ops use
// snake_case values like "maintenance_bundle" and
// "maintenance_backfill_pack_checksum", so "reshard_refs" matches
// that convention.
func Reshard(ctx context.Context, store storage.ObjectStore, r *repo.Repo, k *keys.Repo, opts ReshardOptions) (report ReshardReport, err error) {
	start := time.Now()
	report = ReshardReport{Outcome: "failed_other"}
	// Named return values ensure the deferred mutation is observed
	// by callers — with unnamed returns the value would be copied
	// before the defer runs.
	defer func() { report.DurationMS = time.Since(start).Milliseconds() }()

	view, err := r.ReadRoot(ctx)
	if err != nil {
		return report, fmt.Errorf("maintenance.Reshard: read root: %w", err)
	}
	report.ManifestVersionFrom = view.Header.ManifestVersion

	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		return report, fmt.Errorf("maintenance.Reshard: unmarshal body: %w", err)
	}

	// Already-v2: noop. Sum the existing shard ref counts so the
	// audit/log entry shows real coverage rather than "refs=0 shards=N",
	// which would mislead a reader into thinking the repo is empty.
	if len(body.RefShards) > 0 {
		report.Outcome = "noop"
		report.ManifestVersionTo = view.Header.ManifestVersion
		report.ShardCount = len(body.RefShards)
		total := 0
		for i := range body.RefShards {
			total += body.RefShards[i].RefCount
		}
		report.RefCount = total
		return report, nil
	}

	report.RefCount = len(body.Refs)

	// Empty v1 repo: no migration possible. The validator forbids the
	// v2 marker without RefShards, so we cannot promote — and there is
	// nothing to publish in a new CAS round either. Report the
	// outcome explicitly so operators see this differs from a real
	// migration.
	if len(body.Refs) == 0 {
		report.Outcome = "empty_v1_kept"
		report.ManifestVersionTo = view.Header.ManifestVersion
		return report, nil
	}

	// Build the sharded layout directly from body.Refs by grouping
	// refs into shards and producing one immutable shard object per
	// non-empty bucket. This mirrors the path manifesttest.MakeShardedBody
	// takes (refstore.ShardKey + MarshalAndHash + keys.RefShardKey), which
	// is the canonical hash_v1 build sequence. We don't go through
	// ShardedRefStore.Stage here because refstore.New returns an inline
	// store when len(body.RefShards) == 0 — its API can't represent
	// "promote v1 → v2 in one shot".
	perShard := map[string]map[string]string{}
	for refname, oid := range body.Refs {
		sid := refstore.ShardKey(refname)
		if perShard[sid] == nil {
			perShard[sid] = map[string]string{}
		}
		perShard[sid][refname] = oid
	}
	type shardWrite struct {
		Key      string
		Contents []byte
	}
	var newShards []manifest.RefShard
	var writes []shardWrite
	for sid, refs := range perShard {
		contents, hash, err := refstore.MarshalAndHash(refs)
		if err != nil {
			return report, fmt.Errorf("maintenance.Reshard: marshal shard %s: %w", sid, err)
		}
		key := k.RefShardKey(hash)
		newShards = append(newShards, manifest.RefShard{
			Shard:    sid,
			Key:      key,
			Hash:     hash,
			RefCount: len(refs),
		})
		writes = append(writes, shardWrite{Key: key, Contents: contents})
	}
	sort.Slice(newShards, func(i, j int) bool { return newShards[i].Shard < newShards[j].Shard })
	report.ShardCount = len(newShards)

	// Write all shard objects before the root CAS. Content-addressing
	// makes each write idempotent — a concurrent writer producing the
	// same contents collapses to a single object.
	for _, w := range writes {
		if cerr := ctx.Err(); cerr != nil {
			return report, fmt.Errorf("maintenance.Reshard: cancelled mid shard-write: %w", cerr)
		}
		_, err := store.PutIfAbsent(ctx, w.Key, bytes.NewReader(w.Contents), nil)
		if err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
			return report, fmt.Errorf("maintenance.Reshard: PutIfAbsent shard %s: %w", w.Key, err)
		}
	}

	if opts.BetweenSnapshotAndCAS != nil {
		opts.BetweenSnapshotAndCAS()
	}

	// Build the new body and CAS-publish it.
	actor := opts.Actor
	if actor == "" {
		actor = "u_op"
	}

	// Start from the existing body and only mutate ref-related fields,
	// so any future fields added to manifest.Body are forwarded
	// implicitly rather than dropped on the floor.
	emit := func() manifest.Body {
		newBody := body
		newBody.Refs = nil
		newBody.RefShards = newShards
		newBody.RefSharding = "hash_v1"
		return newBody
	}

	_, err = r.Commit(ctx, tx.Body{Type: "reshard_refs", Actor: actor}, func(prev *repo.RootView) ([]byte, error) {
		// Inside the callback: we MUST re-check that the body is still v1
		// and matches what we staged. If concurrent mutation has occurred,
		// abort by returning ErrConcurrentMutation; Repo.Commit's retry loop
		// would retry forever if we don't fail-fast here.
		if prev.Header.ManifestVersion != view.Header.ManifestVersion {
			return nil, ErrConcurrentMutation
		}
		return manifest.MarshalBody(emit())
	})
	if err != nil {
		if errors.Is(err, ErrConcurrentMutation) {
			report.Outcome = "failed_concurrent_mutation"
			return report, ErrConcurrentMutation
		}
		return report, fmt.Errorf("maintenance.Reshard: commit: %w", err)
	}

	view2, err := r.ReadRoot(ctx)
	if err != nil {
		// Reshard committed; the version readback is informational.
		// Estimate ManifestVersionTo = From+1 so JSON consumers don't
		// see an indistinguishable zero. Don't fail the call.
		report.Outcome = "success"
		report.ManifestVersionTo = report.ManifestVersionFrom + 1
		return report, nil
	}
	report.ManifestVersionTo = view2.Header.ManifestVersion
	report.Outcome = "success"
	return report, nil
}
