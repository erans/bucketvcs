# Ref Sharding Operator Guide

**TL;DR:** Most repos do nothing. A repo with more than ~10k refs may benefit from a one-shot manual migration via `bucketvcs reshard-refs`. Once migrated, the repo cannot go back without a future deshard CLI (deferred).

## What changed

Ref sharding introduces an optional second representation for ref state:

- **Inline (v1, default):** every ref lives in the root manifest under `body.refs`. Fast for small repos.
- **Sharded (v2, opt-in):** refs live in `manifest/ref-shards/<sha256>.json` shard objects; the root manifest just references them by content hash. Scales to millions of refs.

New repos still default to inline. The root manifest schema version bumps from 1 to 2 so binaries without sharding support refuse to read v2 manifests (fail-closed via `SchemaGate`).

## When to migrate

The threshold is informal: if `bucketvcs inspect-manifest` shows the body size growing past a few hundred KB (~5–10k refs), inline mode starts dominating push latency. Below that, inline is faster (zero shard IO).

## How to migrate

```
bucketvcs reshard-refs --store=<URL> --repo=<tenant>/<repo>
```

The CLI:
1. Reads the current root manifest.
2. Hashes every ref into a 256-bucket sharded layout (`ref_sharding: "hash_v1"`).
3. Writes each non-empty shard via `PutIfAbsent` (content-addressed, so racing writers are idempotent).
4. CAS-publishes a new root manifest with `RefShards` populated and `Refs` cleared.

The command is idempotent — re-running it on a v2 repo exits zero with `noop`.

### Pre-flight checklist

- **Backups.** v2 → v1 is not reversible today (no `deshard-refs` yet). Take a manifest snapshot before running.
- **Concurrent pushes.** The migration does NOT acquire a maintenance lease. Pushes during the migration may force a retry. Quiesce automation if possible.
- **Latency.** Shard writes are sequential. For a repo with ~10k refs sharded across ~256 buckets on a high-latency backend (GCS/Azure cross-region), expect ~256 × RTT before the CAS attempt. For localfs / same-region cloud the cost is negligible. The migration is one-shot and idempotent, so retrying after a transient timeout is safe.
- **Retention window.** Failed migrations leave orphan shard objects in `manifest/ref-shards/`. They are content-addressed; GC sweeps them after retention.

### Concurrent push behavior

If a push wins the root CAS race during a reshard:
- The reshard exits non-zero with `concurrent mutation`.
- The shard objects already written are orphans — they survive until the next GC sweep after retention.
- Operator retries the command. The retry sees the new manifest version; if the racing push happened to bump to v2 (unlikely without an explicit reshard), the second invocation no-ops.

## How to verify

After a successful reshard:

```
bucketvcs inspect-manifest --store=<URL> --repo=<tenant>/<repo> --json | jq '.ref_sharding,.ref_shards | length'
```

Expected: `"hash_v1"` and a non-zero shard count.

To list all refs through the new layout:

```
bucketvcs export --store=<URL> --repo=<tenant>/<repo> --dest=/tmp/check
cd /tmp/check && git for-each-ref
```

## What older binaries see

A binary without sharding support reading a v2 manifest fails with `ErrUnsupportedSchema` from the `SchemaGate` check. This is fail-closed — there is no silent misinterpretation hazard. Operators with mixed-version fleets must upgrade every binary that touches a given repo before resharding it. The schema bump is global — every repo created by a sharding-capable build emits `schema_version: 2`, even if it never uses sharded refs. Binaries without sharding support cannot read any repo created by a sharding-capable build.

## What gets stored where

```
tenants/<t>/repos/<r>/manifest/
├── root.json                      ← still the only commit point
└── ref-shards/
    ├── sha256-<hash>.json         ← one immutable object per non-empty shard
    └── sha256-<hash>.json
```

Old shard objects from before a push (when a shard's content changed) become orphans and GC away after retention.

## Limits and deferred work

- **Automatic threshold-driven resharding** — deferred.
- **Layout-change resharding** (e.g., 256 → 4096 shards) — deferred.
- **Hot-shard avoidance** (spec's "keep protected/default branches in an explicit shard") — deferred.
- **Per-namespace shard counts** — deferred.
- **`deshard-refs` reverse migration** — deferred. v2 → v1 today requires hand-editing the manifest off the critical path.

See `docs/superpowers/specs/m12-ref-sharding-spec.md` §11 for the full deferred list.

**All-refs-deleted downgrade.** A push that deletes every ref in a v2 (sharded) repo silently emits an inline-empty body (no `RefSharding`, no `RefShards`) — manifest invariants forbid `RefSharding="hash_v1"` with zero shards. The repo therefore transiently returns to inline layout; the next push that grows refs back will go through the inline code path. No data loss, but operators tracking sharding-state should expect this for force-delete-all-refs flows. Re-running `reshard-refs` after a subsequent ref-growth push re-promotes to v2.

## Failure modes

| Symptom | Cause | Action |
|---|---|---|
| `concurrent mutation` exit | Push won the CAS race | Retry the CLI |
| `ErrShardCorrupt` from any read | Shard object bytes don't match recorded hash | Operator investigation; this is a tampering canary, not retry |
| `ErrUnsupportedSchema` from a binary | Binary without sharding support reading a v2 manifest | Upgrade the binary |
| Reshard "stuck" at the same version | CAS retry loop exhausted | Check for sustained push pressure; rerun with quiesced traffic |
