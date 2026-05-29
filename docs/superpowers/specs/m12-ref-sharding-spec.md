# M12 — Ref scaling via sharded refs

**Status:** design draft for implementation
**Spec reference:** §19 (Ref semantics and ref scaling)
**Date:** 2026-05-19
**Scope:** dual-mode (inline / sharded) read and write paths + one-shot manual reshard CLI. Automatic threshold-driven resharding and layout-change resharding are deferred to a follow-on milestone.

## 1. Background and motivation

Today every ref lives inline in the root manifest:

```go
// internal/repo/manifest/body.go
type Body struct {
    DefaultBranch string            `json:"default_branch"`
    Refs          map[string]string `json:"refs"`
    ...
}
```

For repos with thousands of refs (typical), this is fine. For repos with hundreds of thousands of refs (high-traffic monorepos, PR/CI-heavy projects), the root manifest grows large enough to dominate every push: the root must be re-read, re-written, and CAS'd on every commit, and the JSON body is fully marshalled even when a push only touches one ref.

§19 of the original spec calls for content-addressed ref shards as the answer. M12 implements the read and write paths plus a manual opt-in migration. Automatic ref-count-driven resharding and layout-change resharding (e.g. 16-shard → 256-shard redistribution) stay deferred.

## 2. Scope decisions

| Decision | Outcome |
|---|---|
| §19.2 read/write of sharded refs | **In scope** |
| §19.3 maintenance lease + automatic resharding | **Deferred** |
| §19.3 layout-change resharding | **Deferred** |
| One-shot manual inline→sharded CLI | **In scope** (so M12 has users on day one) |
| Sharding strategy | **Hash-only, N=256 fixed**, tagged `ref_sharding: "hash_v1"` |
| Hybrid namespace+hash strategy | Deferred; schema string leaves room for future strategies |
| GC awareness of ref shards | **In scope** (mandatory — without it, the first GC after a reshard sweeps live shards) |

## 3. Schema

### 3.1 SchemaVersion bump

`internal/repo/manifest/schema.CurrentSchemaVersion` bumps from 1 to 2 in the M12 build. The existing `SchemaGate(h)` function rejects any manifest whose `SchemaVersion > CurrentSchemaVersion`, returning `repoerrs.ErrUnsupportedSchema`. So a pre-M12 binary reading a v2 manifest fails loudly via `SchemaGate` — it never produces wrong results from misinterpreting fields.

- **v1 manifests:** unchanged. `Body.Refs` populated, no `RefShards`, no `RefSharding`. Old binaries reading v1 manifests continue working.
- **v2 manifests:** `Body.RefShards` populated, `Body.RefSharding == "hash_v1"`, `Body.Refs` empty/absent. The writer also sets `Header.MinReaderVersion` to the M12 release version so a pre-M12 binary fails the `SchemaGate` MinReaderVersion comparison (defense-in-depth alongside the SchemaVersion check).

A v2 binary reads both versions and dispatches at construction time.

A small repo can stay on v1 forever; nothing in M12 forces a migration. The only path from v1 to v2 is the explicit reshard CLI.

### 3.2 Body schema additions

```go
type Body struct {
    DefaultBranch string            `json:"default_branch"`
    Refs          map[string]string `json:"refs,omitempty"`           // v1 only
    RefShards     []RefShard        `json:"ref_shards,omitempty"`     // v2 only
    RefSharding   string            `json:"ref_sharding,omitempty"`   // v2 only; must be "hash_v1"
    Packs         []PackEntry       `json:"packs"`
    Indexes       Indexes           `json:"indexes"`
    Bundles       []BundleEntry     `json:"bundles"`
}

type RefShard struct {
    Shard    string `json:"shard"`              // "00".."ff" (2 lowercase hex)
    Key      string `json:"key"`                // "manifest/ref-shards/<hash>.json"
    Hash     string `json:"hash"`               // "sha256-<64 lowercase hex>"
    RefCount int    `json:"ref_count"`          // informational; not load-bearing
}
```

### 3.3 Hybrid-state rejection

`manifest.UnmarshalBody` rejects:

- A body with both `Refs` non-empty AND `RefShards` non-empty → `ErrInvalidManifest: hybrid v1/v2 ref state`.
- A v2 body with `RefSharding != "hash_v1"` → `ErrInvalidManifest: unsupported ref sharding strategy "<value>"` (forward-compat: future strategies bump this gate).
- A v2 body whose `RefShards` contains a `Shard` value outside `"00".."ff"`, duplicate shard keys, or `Hash` not matching `"sha256-<64hex>"` → `ErrInvalidManifest` with the specific reason.

## 4. Architecture

### 4.1 New package: `internal/repo/refstore`

All ref consumers (uploadpack advertise, lsrefs, exporter, receivepack mergeRefs) call this interface; no consumer reads `body.Refs` directly after M12.

```go
package refstore

type RefStore interface {
    Lookup(ctx context.Context, refname string) (oid string, exists bool, err error)
    List(ctx context.Context) (map[string]string, error)
    Stage(updates map[string]string) (Stage, error)
}

// Stage is the precomputed delta from a set of ref updates. It captures
// every shard object that needs to be written and the new RefShards slice
// to publish in the root manifest.
type Stage struct {
    NewShardObjects []ShardWrite      // PutIfAbsent these before root CAS
    NewRefShards    []RefShard        // becomes Body.RefShards on commit
    Mode            Mode              // "inline" or "sharded"
}

type ShardWrite struct {
    Key      string // "manifest/ref-shards/<hash>.json"
    Hash     string // "sha256-..."
    Contents []byte // canonical JSON of map[refname]oid for this shard
}
```

Two implementations:

- `InlineRefStore` — wraps `Body.Refs`. `Lookup` and `List` are pure in-memory ops. `Stage` returns a Stage with `Mode=Inline` and an empty `NewShardObjects` — the inline `Refs` map is staged via a separate field consumed by `repo.Commit` (see §5).
- `ShardedRefStore` — wraps `Body.RefShards` + an `ObjectStore`. `Lookup` reads one shard; `List` parallel-fetches all shards; `Stage` hash-buckets the updates, loads only affected shards, applies the updates, computes new content + content-hash, returns the full Stage.

A factory `refstore.New(ctx, store, body) RefStore` dispatches on `Body.RefShards != nil`.

### 4.2 Hash sharding

```go
// shardKey returns the 2-hex shard identifier for a refname.
// Uses sha256 (not sha1) because shard identifiers should be stable
// regardless of object-hash format and crypto-quality distribution
// matters for hot-shard avoidance.
func shardKey(refname string) string {
    sum := sha256.Sum256([]byte(refname))
    return hex.EncodeToString(sum[:1]) // first byte → "00".."ff"
}
```

256 shards is fixed in M12. A future hybrid or layout-change reshard milestone may introduce variable shard counts; the `ref_sharding` string is the version gate that lets that happen without breaking M12 readers.

### 4.3 Shard object format

Each shard object is a JSON object mapping refname → oid:

```json
{
  "refs/heads/main": "5d7ff70a...",
  "refs/tags/v1.0.0": "8df3ca9e..."
}
```

Canonical serialization:

- Keys sorted lexicographically.
- 2-space indent (matches existing `manifest.MarshalBody`).
- No trailing newline.

This is what makes content-addressing reliable: re-sharding the same ref set produces byte-identical shard objects, and PutIfAbsent on a content-addressed key collapses concurrent identical writes to a no-op.

## 5. Data flow

### 5.1 Read flow

Every consumer that previously did `for name, oid := range body.Refs` switches to:

```go
rs := refstore.New(ctx, store, body)
refs, err := rs.List(ctx)
```

For v1 repos this is zero IO (returns `body.Refs` directly). For v2 repos this parallel-fetches all `body.RefShards` via an errgroup, verifies each shard's sha256 matches `body.RefShards[i].Hash`, and merges the results.

A single-ref lookup (only used by receivepack's old-OID precheck today):

```go
oid, exists, err := rs.Lookup(ctx, refname)
```

For v1: O(1) map access. For v2: hash the refname, fetch the one shard, look up the name.

### 5.2 Write flow (push via receivepack)

```
1. receivepack.complete reads incoming ref updates (refname, oldOID, newOID).
2. For each (refname, oldOID): refstore.Lookup must match (existing precheck logic, now through the interface).
3. Build updates: `map[refname]oid`. Deletion uses the existing `nullOIDHex` (40-zero SHA-1) OR empty-string convention from `internal/importer/buildcommit.go:nullOIDHex` and `mergeRefs`; `refstore.Stage` accepts either form and treats both as "remove this refname from its shard."
4. stage := refstore.Stage(updates)
5. repo.Commit accepts the stage:
   Phase A: parallel PutIfAbsent every stage.NewShardObjects.
            Content-addressed → racing identical writes collapse to no-op.
            ANY single failure aborts the push; no root CAS attempted.
   Phase B: Build new manifest body from snapshot, set:
              body.RefShards = stage.NewRefShards
              body.RefSharding = "hash_v1"
              body.Refs = nil
            (or, for inline mode: body.Refs = updates merged into old.Refs;
            body.RefShards stays nil.)
   Phase C: existing CASRoot.
            On version mismatch: existing retry loop re-reads the snapshot
            and recomputes the stage. Already-written shards are still valid
            (content-addressed) and either get reused or become GC candidates.
```

The shard objects ARE written before the root CAS, which means an aborted push can leave orphan shard objects. They are content-addressed, so they cost only their own bytes and are swept by GC after the retention window.

### 5.3 Reshard flow (one-shot CLI, inline → sharded)

```
1. ReadRoot snapshot. Verify body is v1 (Refs populated, RefShards empty).
   - If v2 already: exit zero with "already sharded" message (idempotent).
2. Hash every ref into a shard; build 256 (or fewer; empty shards skipped) shard contents.
3. PutIfAbsent each non-empty shard in parallel.
4. Build new body:
     body.Refs = nil
     body.RefShards = newShards
     body.RefSharding = "hash_v1"
     header.SchemaVersion = 2
5. CASRoot.
   - On version mismatch: abort with ErrConcurrentMutation. Operator retries.
   - Shards already written stay as orphans → GC sweep after retention.
6. Exit zero on success.
```

The reshard CLI does NOT acquire a maintenance lease and does NOT block pushes. Concurrent pushes during reshard either win their root CAS (in which case the reshard retries on the operator's next invocation) or lose theirs (in which case they retry with the new sharded layout, fully transparently).

### 5.4 Atomicity guarantee

The root CAS remains the only commit point. Half-written shards from a failed push or aborted reshard become GC candidates; readers never see them because the root manifest still points at the old shard set.

A reader observing manifest version T sees:
1. The shard set published at version T (all shard objects existed before that root CAS by Phase A → Phase C ordering).
2. Each shard's content matches its published hash (verified at read time).

A push that races to version T+1 writes new shard objects but does not invalidate the reader's view of version T.

## 6. Error handling

### 6.1 Sentinel errors

```go
var (
    ErrShardCorrupt       = errors.New("refstore: shard content hash mismatch")
    ErrStaleRef           = errors.New("refstore: ref old-OID precheck failed")
    ErrConcurrentMutation = errors.New("refstore: concurrent root mutation; retry")
    ErrNotSharded         = errors.New("refstore: operation requires sharded mode")
    ErrInline             = errors.New("refstore: operation requires inline mode")
)
```

### 6.2 Read-path failures

- `ShardedRefStore.List` parallel-fetches; the first shard error cancels the errgroup and returns the error wrapped with which shard key failed. Advertise paths surface this as 5xx — there is no useful partial answer.
- `ShardedRefStore.List` recomputes each shard's sha256 and compares against the manifest's recorded hash. Mismatch → `ErrShardCorrupt` with shard key + expected hash + actual hash. This is a tampering canary, never best-effort retried.
- `ShardedRefStore.Lookup` on a refname whose computed shard is not in `body.RefShards` returns `(oid="", exists=false, err=nil)`. Empty shards are absent from `RefShards`.

### 6.3 Push-path failures

- `Stage` building fails with `ErrStaleRef{Refname, Want, Got}` if old-OID precheck mismatches. Receivepack returns the conflict on the wire as today.
- Phase A `PutIfAbsent` returns `storage.ErrAlreadyExists` when a shard with that content-addressed key already exists. Because the key includes the sha256 of the contents, an existing object with the same key has identical bytes (collision-resistance of sha256). Phase A explicitly catches `ErrAlreadyExists` via `errors.Is` and treats it as success. Any other backend error fails the whole push; we do not proceed to root CAS with missing shards.
- Phase C `CASRoot` version mismatch → existing retry loop rebuilds Stage from the new snapshot. Already-written shards remain valid (content-addressed and idempotent).

### 6.4 Reshard-CLI failures

- Concurrent push wins root CAS → reshard aborts with `ErrConcurrentMutation`, non-zero exit, message suggests retry.
- Already v2 → exits zero, no-op (idempotent).
- Empty repo (no refs) → bumps to v2 with empty `RefShards`. Allowed.

## 7. GC integration

`internal/gc/discover.go` must enumerate `body.RefShards[*].Key` as live objects when scanning a v2 manifest. Without this, the first GC after a reshard would sweep the live ref shards.

This is a small additive change to the existing live-set builder, not a new mark phase. The shard objects are immutable and content-addressed; they are treated identically to existing live manifest-side objects (packs, bundles, etc.).

The change ships in M12. Operating a v2 manifest without GC awareness is unsafe.

## 8. Component map

### 8.1 Files to create

| Path | Responsibility |
|---|---|
| `internal/repo/refstore/refstore.go` | `RefStore` interface, `Stage` and `ShardWrite` types, `New` factory, `shardKey` helper, sentinel errors |
| `internal/repo/refstore/inline.go` | `InlineRefStore` |
| `internal/repo/refstore/sharded.go` | `ShardedRefStore` (parallel-fetch, hash verification, per-shard Lookup, Stage building) |
| `internal/repo/refstore/marshal.go` | Canonical-JSON encoder for shard objects (sorted keys, 2-space indent, no trailing newline) |
| `internal/repo/refstore/inline_test.go` | TDD coverage for `InlineRefStore` |
| `internal/repo/refstore/sharded_test.go` | TDD coverage for `ShardedRefStore` including corruption detection |
| `internal/repo/refstore/refstore_test.go` | Factory dispatch + hybrid-state rejection |
| `internal/repo/refstore/conformance/` | Property tests asserting equivalence (`Inline(R).List == Sharded(shard(R)).List`), round-trip (`apply(Stage(updates)) == expected`), and determinism (byte-identical shard objects across runs) |
| `internal/maintenance/reshard.go` | Inline→sharded pipeline phase |
| `internal/maintenance/reshard_test.go` | Reshard against localfs fixture |
| `cmd/bucketvcs/maintenance_reshard.go` | CLI wiring (either `bucketvcs maintenance --reshard-refs` flag or `bucketvcs reshard-refs` subcommand; plan stage chooses based on existing `cmd/` patterns) |
| `scripts/m12-reshard-smoke.sh` | End-to-end smoke against localfs: build 5k-ref repo, reshard, push, fetch |

### 8.2 Files to modify

| Path | Change |
|---|---|
| `internal/repo/manifest/header.go` | `SchemaVersion` constant bump 1 → 2 |
| `internal/repo/manifest/body.go` | Add `RefShards`, `RefSharding` fields; mark `Refs` `omitempty`; tighten `UnmarshalBody` with hybrid-state and strategy-string rejections |
| `internal/repo/repo.go` | `Repo.Commit` accepts a `Stage` from refstore; Phase A writes shard objects before the existing CAS path |
| `internal/gitproto/uploadpack/advertise.go` | Switch from `body.Refs` direct access to `refstore.New(...).List(ctx)` |
| `internal/v2proto/lsrefs.go` | Same switch |
| `internal/gitproto/receivepack/advertise.go` | Same switch |
| `internal/gitproto/receivepack/complete.go` | Old-OID precheck via `refstore.Lookup` instead of `body.Refs[refname]` |
| `internal/importer/buildcommit.go` | `mergeRefs` becomes a thin wrapper that calls `refstore.Stage` |
| `internal/exporter/exporter.go` | Iterate `refstore.List(ctx)` instead of `body.Refs` |
| `internal/gc/discover.go` | Add `body.RefShards[*].Key` to the live set when scanning v2 manifests |

The public symbol footprint is small: one new package, one interface, two impls, one Stage type, one shard-key helper, one CLI command.

## 9. Testing strategy

### 9.1 Unit tests (TDD per type)

- `inline_test.go` — Lookup, List, Stage against hand-built v1 bodies. Asserts zero IO (panicking ObjectStore stub).
- `sharded_test.go` — Lookup resolves correct shard; List parallel-fetches and merges; corrupt-hash shard returns `ErrShardCorrupt`; Stage for 3 distinct shards produces 3 ShardWrites; deletion that empties a shard removes it from `NewRefShards`.
- `refstore_test.go` — Factory dispatch; hybrid-state rejection.

### 9.2 Property tests (`internal/repo/refstore/conformance/`)

Mirrors the existing `internal/storage/conformance/` pattern.

- **Equivalence:** for any ref set R, `InlineRefStore(R).List() == ShardedRefStore(shard(R)).List()`. Random-seed driven via `testing/quick`-style fuzzing or a custom generator.
- **Round-trip:** apply Stage(updates), construct a fresh ShardedRefStore from the result, assert its `List()` equals `expected(oldRefs, updates)`.
- **Determinism:** sharding the same ref set twice produces byte-identical shard objects. Property-asserted, not just smoke-tested.

### 9.3 Integration tests

- `internal/repo/repo_reshard_test.go` — full CLI flow against localfs: 1000-ref v1 repo → reshard → assert v2 layout → push a new ref → assert the right shard was rewritten → fetch advertises all 1001 refs.
- `internal/gitproto/receivepack/...` — add one parallel-mode test against a sharded fixture confirming push semantics are observably identical to inline mode.
- `internal/gc/...` — add a test that a v2 manifest's RefShards keys appear in the live set; sweep does not delete them.

### 9.4 Smoke tests

- `scripts/m12-reshard-smoke.sh` — init localfs repo, push 5k refs (small, fast), run reshard, push more refs, fetch full ref list, assert ref count matches.
- No cloud-backend-specific smoke needed; refstore is above the ObjectStore layer and backend-agnostic. Existing conformance covers backend correctness.

## 10. Operational notes

### 10.1 When to opt in

A repo SHOULD reshard when its inline `Refs` map exceeds ~10k entries (root manifest body approaching 1 MB). Below that threshold inline mode is faster (zero shard IO on every operation). The CLI does not enforce a threshold — the operator decides.

### 10.2 Pre-reshard checklist

- Quiesce automated push traffic for the duration (best-effort; reshard tolerates concurrent pushes but they may force a retry).
- Verify the GC retention window is long enough to cover the operator's retry cadence — orphan shards from aborted reshards GC after retention.
- Expect the reshard to take O(seconds) for 100k refs (256 PutIfAbsent calls in parallel + 1 root CAS).

### 10.3 Rollback

There is no automatic v2 → v1 path in M12. To revert, an operator can run a future "deshard" CLI (deferred) or hand-edit the manifest off the critical path. v2 is intended as a one-way migration.

## 11. Out of scope

Explicitly deferred to a follow-on milestone:

- Automatic ref-count-driven resharding (would integrate with `internal/maintenance/thresholds.go`).
- Maintenance-lease coordination for resharding under live push traffic.
- Layout-change resharding (e.g., growing from `hash_v1` 256 shards to a future `namespace_hash_v1` strategy).
- Hot-shard avoidance (the spec's "keep protected/default branches in a small explicit shard" recommendation).
- Per-namespace shard count configuration.
- Multi-region read placement for ref shards.
- `bucketvcs deshard-refs` reverse migration.
