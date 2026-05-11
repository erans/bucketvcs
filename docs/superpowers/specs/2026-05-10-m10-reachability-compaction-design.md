# M10 — Reachability compaction (design)

Status: draft for implementation planning
Date: 2026-05-10
Spec sections: §14.1, §14.2, §14.3, §14.4
Decomposition row: M10 "Reachability compaction — base + delta index model, partitioning for large repos, compaction CAS protocol"

## 0. Goal and scope

M10 makes the per-repo reachability index a first-class hot-path artifact:

1. Each push produces a small immutable **delta index** (`.bvrd`) recording its commits, parents, generation numbers, ref-tip diff, and new pack IDs.
2. `bucketvcs maintenance` learns to **compact** the delta chain into a fresh base index (`.bvcg` v2 + `.bvom`) without necessarily repacking, gated by §14.2 bounds.
3. The fetch hot path runs **negotiation** (the `want`/`have` round-trip) from these index objects in pure Go — cold gateways no longer materialize the mirror just to answer "do you have commit X."

What M10 explicitly does **not** ship: partitioned base indexes, warm-pool routing, trees/blobs in deltas, bitmaps inside deltas, mirror-less pack delivery. Each of these has a designated follow-up milestone (see §6).

The cold-fetch SLO target is §14.3's contract:

> A cold gateway should load a small bounded number of index objects before negotiation. It should not perform one object-store range GET per commit or delta-base hop.

For a freshly-maintained repo (single base + zero deltas), the gateway issues exactly two index reads (`.bvcg` v2 + `.bvom`) before negotiation. For a repo mid-cycle with N pending deltas, it issues `2 + N` reads, with N bounded by the §14.2 thresholds.

## 1. Brainstorming decisions

These choices were made during brainstorming and govern the rest of the spec.

| ID | Question | Choice |
|----|----------|--------|
| Q1 | How far does M10 reach toward the fetch hot path? | **B — Index model + read-only adoption.** Wire the new index into upload-pack negotiation. Pack delivery still uses the mirror. |
| Q2 | Partitioning + warm pools | **A — Monolithic only.** Single-file base index per manifest + delta files. Partitioning and warm pools deferred to a later milestone. |
| Q3 | Delta-index content and write timing | **C — Negotiation-essential content now, format-extensible.** Push-time-written. Each `.bvrd` contains commits + parents + gen numbers + ref-tip diff + pack IDs; reserved length-prefixed slots for trees/blobs/tags and bitmaps so M11 / M9.5 can extend without a format break. |
| Q4 | Topology vs existing `.bvom` / `.bvcg` | **B — Layer.** Keep `.bvom` and `.bvcg`. Bump `.bvcg` to v2 (adds generation numbers). Add new `.bvrd` delta files. Compaction = "rebuild `.bvom` + `.bvcg` from scratch (M9 path) + drop consumed deltas." |
| Q5 | Compaction ownership / CLI shape | **A — Extend `bucketvcs maintenance`.** Phase-0 evaluates pack-thresholds and reachability-thresholds independently; index-only refresh becomes a distinct outcome alongside repack-only and full. |
| Q6 | Read-path realization | **A — Negotiation pre-step + lazy full mirror.** Pure-Go negotiation from `.bvcg` + `.bvrd`; mirror materialized after negotiation, pack delivery via existing `git pack-objects`. |

Two derived constraints carried forward without an explicit question:

- **CAS protocol** mirrors M9's CAS-merge pattern: at commit time, re-read `M_now` and preserve any deltas that landed during compaction. They remain valid against the new base by the same reachability-superset argument that lets M9 preserve concurrent push packs.
- **Delta ordering** is the slice order in `Indexes.Reachability.Deltas`. No predecessor-hash linking — the manifest is the source of truth.

## 2. Wire format

### 2.1 `.bvcg` v2 (extend existing format)

`.bvcg` v1 today: header (magic "BVCG", version=1, n_commits, n_tips, reserved 12B), tip table, commit records (oid + n_parents + parents), string table, SHA-256 trailer.

v2 adds a `generation_number` (u32) to each commit record, immediately after `oid` and before `n_parents`. Magic stays "BVCG"; version becomes 2.

Generation rule: `gen(c) = 1 + max(gen(parents))`, root commits have `gen = 1`. Computed in topological order during build.

The v1 reader stays in place. Callers that only need commits + parents continue working. Callers that need `gen` upgrade through a new `Reader.GenerationOf(oid) (uint32, ok)` that returns `(0, false)` on v1 files. M9 maintenance rebuilds existing repos to v2 on the next maintenance run; old `.bvcg` v1 files become unreferenced and are reclaimed by M8 GC after retention.

### 2.2 `.bvrd` (new — reachability delta)

One file per push. Immutable, content-addressed.

```
header (32 bytes):
  magic        "BVRD"  (4B)
  version      u32     (=1)
  n_commits    u32
  n_reftips    u32
  n_packs      u32
  reserved     12B

commits (sorted by oid):
  oid                  20B
  generation_number    u32
  n_parents            u8
  parents              n_parents * 20B

reftips:
  ref_name_offset      u32   (-> string table)
  new_oid              20B
  old_oid              20B   (zero for ref-create)

packs (refs into manifest.Packs):
  pack_id              20B

reserved sections (length-prefixed, currently u32=0):
  trees_blobs_tags     // Q3=C extension slot for M11
  bitmap               // M9.5 extension slot

strtab:
  NUL-terminated UTF-8 ref names

trailer (32 bytes):
  SHA-256 over preceding bytes
```

Storage key: `tenants/<t>/repos/<r>/indexes/reachability-delta/<hash>.bvrd`. Content hash is the SHA-256 of the file body (matches `.bvom` / `.bvcg` convention).

### 2.3 Manifest schema

`internal/repo/manifest/body.go` extends `Indexes` and adds a size field to `IndexRef`:

```go
type IndexRef struct {
    Key       string `json:"key"`
    Hash      string `json:"hash"`
    SizeBytes int64  `json:"size_bytes,omitempty"`  // NEW — O(1) threshold evaluation
}

type Indexes struct {
    ObjectMap    *IndexRef         `json:"object_map,omitempty"`
    CommitGraph  *IndexRef         `json:"commit_graph,omitempty"`
    Reachability *ReachabilityRef  `json:"reachability,omitempty"`
}

type ReachabilityRef struct {
    BaseManifest string     `json:"base_manifest"`
    Deltas       []IndexRef `json:"deltas"`
}
```

`SizeBytes` is `omitempty` for backward compatibility: legacy `IndexRef` JSON without the field decodes cleanly (size=0). The maintenance Phase-0 byte-threshold check uses these values directly — no HEAD requests required. Receive-pack populates `SizeBytes` when adding a new delta; maintenance populates it for newly-built `.bvom` / `.bvcg`.

`Reachability.BaseManifest` records the manifest version that produced the current `(ObjectMap, CommitGraph)` pair. Paper-trail field — used for sanity checks and debugging, never as a storage key. The base is implicit: `(ObjectMap, CommitGraph)` is the base, `Reachability.Deltas` is the chain above it.

`Reachability` is `omitempty`. Legacy repos imported pre-M10 have `Reachability == nil`; their first post-M10 maintenance run populates the field.

## 3. Produce path (push)

`internal/gitproto/receivepack` grows a new step between pack ingest and manifest CAS:

1. Open the just-ingested pack with `internal/pack.Reader`.
2. Build a `parent_oid -> gen` lookup by streaming the manifest's `.bvcg` v2 plus any current `Reachability.Deltas`. Cost: bounded by the §14.2 threshold (≤1000 commits / 100 pushes / 64 MiB).
3. Walk new commits in topological order, deriving `gen(c) = 1 + max(gen(parents))`. Commits whose parents are also in the new pack resolve transitively.
4. Build the ref-tip diff from the receive-pack command list (commands already supply `(ref, old, new)` per ref).
5. Encode `.bvrd` bytes, hash, upload to `indexes/reachability-delta/<hash>.bvrd`.
6. In the manifest body sent to CAS: append `IndexRef{Key, Hash}` to `Indexes.Reachability.Deltas`. `ObjectMap`, `CommitGraph`, and `BaseManifest` are unchanged — they're still the base.

Failure semantics: if `.bvrd` build or upload fails, **abort the push** with a clear error. We do not allow a push to land without its delta — that's the §14.3 SLO contract. There is no "fall back to stale-index" path on the write side; the read side has a fallback (§5.4).

Push-CAS retry: if the receive-pack CAS loses (concurrent push won), the loser re-reads `M_now`. Generation numbers for new commits may shift if the winner introduced ancestors. The `.bvrd` is rebuilt and re-uploaded (new hash); the previous `.bvrd` becomes an orphan reclaimed by M8 GC.

## 4. Compact path (maintenance)

`internal/maintenance` Phase 0 grows three threshold checks alongside the existing pack thresholds:

| Flag | Default | Spec |
|------|---------|------|
| `--reachability-delta-commits` | 1000 | §14.2 |
| `--reachability-delta-pushes`  | 100  | §14.2 |
| `--reachability-delta-bytes`   | 64 MiB | §14.2 |

Threshold evaluation is cheap-first, same convention as M9: bytes (sum of `Reachability.Deltas[i].SizeBytes`, populated at push time per §2.3) and pushes (`len(Deltas)`) are O(1) on the manifest body; commits (`sum(n_commits in each .bvrd header)`) requires reading the headers (one range GET per delta, only if cheaper triggers haven't fired).

Phase-0 outcomes:

| Pack threshold? | Reachability threshold? | Action |
|------------------|--------------------------|--------|
| No  | No  | no-op |
| Yes | No  | repack + full index refresh (existing M9 path) |
| No  | Yes | **index-only refresh** (new path) |
| Yes | Yes | repack + full index refresh |

The "index-only refresh" path runs `internal/maintenance/indexes.go`'s rebuild against the current pack list, uploads new `.bvom` + `.bvcg` v2, and CAS-commits — no `pack-objects` call.

### 4.1 CAS-merge body builder

The M9 CAS-merge body builder extends with one new clause:

```
M_new = {
  Refs:    M_now.Refs,
  Packs:   [new_pack] ++ (M_now.Packs - P_consumed),   // M9 — unchanged
  Indexes: {
    ObjectMap:    newly_built (or unchanged for compact-only of M_now's exact pack list),
    CommitGraph:  newly_built v2 (or unchanged),
    Reachability: {
      BaseManifest: M_now.Version,
      Deltas:       M_now.Reachability.Deltas[len(consumed_deltas):],
    },
  },
}
```

`consumed_deltas` is the *prefix* of `Reachability.Deltas` observed when compaction started. Any deltas that landed during our work (the suffix beyond `len(consumed)`) stay in the chain — they remain valid because the new base's reachability is a superset of what they referenced.

For compact-only runs the `Packs` clause is a no-op: `new_pack` is omitted and `P_consumed` is empty, so `Packs = M_now.Packs` byte-for-byte. The CAS-merge surfaces only swap `Indexes`.

CAS loss: same as M9 — abort, retry. Worst case: indefinite retry under push storm, bounded by `--cas-retry`. Orphaned `.bvom` / `.bvcg` / candidate manifest are GC-eligible after retention.

### 4.2 What "compact-only" produces vs "repack + refresh"

- **Compact-only**: `Packs` unchanged from `M_now.Packs`. `.bvom` and `.bvcg` are rebuilt against that exact pack set. Cost is dominated by `.bvom` build (walks every pack's index — bounded by total object count). For a single-pack repo post-maintenance, this is a quick re-emit.
- **Repack + refresh**: Existing M9 flow. `.bvom` / `.bvcg` produced as a side effect of having a single consolidated pack.

In both cases, `Reachability.Deltas` is truncated by `len(consumed_deltas)`.

## 5. Read path (cold-fetch negotiation)

### 5.1 New package: `internal/reachability`

```go
package reachability

type Set struct { /* base + deltas + omap views */ }

func Load(ctx context.Context, store storage.ObjectStore, body manifest.Body) (*Set, error)

func (s *Set) Has(oid OID) bool
func (s *Set) Parents(oid OID) []OID
func (s *Set) Generation(oid OID) (uint32, bool)
func (s *Set) WalkAncestors(roots []OID, visit func(OID, uint32) error) error
func (s *Set) RefTips() map[string]OID
func (s *Set) ObjectPack(oid OID) (packID OID, ok bool)   // delegates to .bvom view
```

Deltas shadow the base: lookup order is "latest delta → earlier deltas → base." A commit appearing in any delta returns that delta's gen number. Ref tips are computed by replaying deltas in order over the base's tip set.

`Load` issues a bounded number of range GETs: `.bvcg` + `.bvom` (base) + N deltas. N is bounded by §14.2 thresholds. Each `.bvrd` is small (KB-scale) — the network cost is dominated by latency, not bandwidth.

### 5.2 Upload-pack engine split

`internal/gitproto/uploadpack` splits the request flow:

1. **Negotiate** (new, pure-Go): drive Git v2 `want`/`have`/`done` against a `reachability.Set` instead of against a materialized mirror.
   - Maintain a `wants` frontier and a `haves` frontier.
   - For each `have <oid>`: if `Set.Has(oid)`, emit `ACK <oid> common`. Use generation-aware walk to prune.
   - On `done`: compute `shipping_commits = wants \ ancestors_of(haves)` via gen-bounded walk.
   - Produce `ShippingPlan{Commits []OID, Refs map[string]OID}`.

2. **Deliver** (existing path, lazily invoked): `internal/mirror.Manager.EnsureReady(ctx, repo)` is moved from request-pre-step to post-negotiate step. After negotiation, the mirror is materialized; `git pack-objects --revs --stdout` runs against it with `ShippingPlan.Commits` fed on stdin; pack bytes streamed back to the client.

The mirror still exists and still serves as the pack-delivery substrate. M10's win is shifting the mirror materialization from "before negotiation" to "after negotiation" — which means a no-op fetch (client has everything) never materializes the mirror at all.

### 5.3 Differential validation: `bucketvcs negotiate`

A new debug subcommand:

```
bucketvcs negotiate --store=<url> --repo=<t>/<r> \
    --wants=<oid>[,<oid>...] --haves=<oid>[,<oid>...] \
    [--output=text|json]
```

Runs the pure-Go negotiation engine and prints the shipping plan. Used by the differential harness to assert parity with `git upload-pack` (§7.2). Documented in the operator guide as a diagnostics tool, not a hot-path command.

### 5.4 Fallback to eager mirror

If `Indexes.Reachability == nil` (legacy repo not yet maintained at M10) OR if any `.bvrd` load returns an error OR if `BaseManifest` doesn't match the manifest version that produced `(ObjectMap, CommitGraph)`, the engine logs a structured warning (`level=warn, event=reachability.fallback, reason=...`) and falls through to the M9-era eager-mirror path: materialize before negotiation, run `git upload-pack` as today.

This keeps the deployment safe across the maintenance-cycle migration window and any future data corruption.

## 6. Concurrency, failure modes, and edge cases

| Scenario | Resolution |
|----------|------------|
| **Push ↔ push** | Serialized per-repo by M3's `internal/repo/tx`. CAS loser rebuilds its `.bvrd` (gens may shift) and retries. |
| **Push ↔ compaction** | CAS-merge in §4.1. Pushes landing during compaction keep their deltas in the new chain (suffix). |
| **Compaction ↔ compaction** | Only one wins the manifest CAS; the loser's uploaded `.bvom` / `.bvcg` / manifest candidate orphan. M8 GC reclaims after retention. |
| **Compaction ↔ M8 GC** | GC's live-set walk extended to include `indexes/reachability-delta/<hash>.bvrd` entries listed in any manifest within retention. Sweep prefix `indexes/reachability-delta/` added to the sweep-eligible list. New interleaving `compaction_during_mark` added to `RunPropertyGCSafety`. |
| **Stale-base detection** | `Set.Load` asserts `ReachabilityRef.BaseManifest == manifest.Version` for the pinned `(ObjectMap, CommitGraph)`. Mismatch → fallback (§5.4). Impossible by construction; defensive check. |
| **Delta chain too long** | Gateway serves regardless of length. Operator should run maintenance. Logged warning + counter `bucketvcs_reachability_delta_chain_length` surfaced via `inspect-manifest --json`. |
| **Gen-number inconsistency push vs compact** | Compacted `.bvcg` v2 wins. Transient inconsistency invisible to clients (gen numbers are an internal hint, never on the wire). |
| **Migration day-1** | Repos imported pre-M10 have `Reachability == nil`. First push: receive-pack builds `.bvrd` against `.bvcg` v1 (treats parent gens as 0), commits new gens normally. First maintenance: full rebuild to v2, sets `BaseManifest = M_now.Version`, `Deltas = []`. After first maintenance, repo is fully on M10. |
| **Permanent fallback** | If `Indexes.Reachability` is absent or `.bvrd` load fails, fall through to eager mirror with a warning. Only viable cold-fetch story for misconfigured repos. |

## 7. Testing strategy

### 7.1 Package unit tests

- `internal/commitgraph` v2: golden tests for new on-disk format; v1 reader still passes; gen-number property test (random DAG generator, assert `gen(c) = 1 + max(gen(parents))`).
- `internal/reachability/deltaindex` (new): roundtrip encode/decode; content-hash stability; reject malformed (truncated trailer, bad magic, version mismatch).
- `internal/reachability.Set`: load base + deltas, `Has` / `Parents` / `Generation` / `WalkAncestors` against a fixture set with known answers; shadow semantics across multiple deltas; `ObjectPack` delegation.

### 7.2 Differential harness

`internal/diffharness` extends with `ImportPushCompactNegotiateExportAndCompare`:

```
for each fixture in registry (16+ fixtures):
  import fixture into a fresh repo
  apply N synthetic pushes (each producing a .bvrd)
  run compaction once
  for each (wants, haves) probe in the fixture's probe set:
    negotiate via pure-Go engine
    negotiate via git upload-pack against materialized mirror
    assert identical ShippingPlan
  export and compare round-trip
```

New fixtures to add:

- `many-small-pushes` — exercises long delta chain (50+ deltas before compaction).
- `force-push-mid-chain` — `.bvrd` with `old_oid` not an ancestor of `new_oid`.
- `tag-pushes-between-commits` — annotated and lightweight tags in their own pushes.
- `octopus-merge` — multi-parent commits stress gen-number computation.

### 7.3 Conformance suite

`internal/reachability/conformance.RunPropertyReachabilitySafety` — property test over backend adapters, mirroring M8's `RunPropertyGCSafety` shape. Interleavings:

- `push_during_compaction` — CAS-merge correctness.
- `two_compactions` — only one wins; loser orphans cleanly.
- `compaction_during_mark` — GC interleave (also added to M8's matrix as a cross-milestone exercise).
- `negotiation_during_compaction` — cold gateway reads while base swaps; load must be atomic-per-manifest-version.

Wired into `internal/storage/conformance` for all 4 canonical backends (localfs, s3compat, gcs, azureblob).

### 7.4 End-to-end

- CLI: `bucketvcs negotiate` against a real localfs repo reproduces `git upload-pack`'s shipping decisions for a captured wants/haves transcript.
- Maintenance compact-only path: pack thresholds untripped + reachability thresholds tripped → manifest CAS swaps `.bvcg` / `.bvom` and drops the consumed delta prefix without producing a new pack. Assert `Packs` list unchanged byte-for-byte.

### 7.5 Benchmarks (non-gating, documented in operator guide)

- Negotiation latency: pure-Go engine vs eager-mirror path, cold (no caches), across delta-chain lengths {0, 10, 100}.
- Pack count = 1 (post-maintenance) and pack count = 10 (mid-cycle).

## 8. Operator surface

### 8.1 CLI changes

**`bucketvcs maintenance`** — 3 new flags:

- `--reachability-delta-commits` (default 1000)
- `--reachability-delta-pushes` (default 100)
- `--reachability-delta-bytes` (default 64 MiB, accepts `K`/`M`/`G` suffixes)

`--force` continues to bypass all thresholds (now including reachability). New JSON report field:

```json
"reachability_compaction": {
  "triggered": true,
  "trigger_reason": "delta-pushes",
  "deltas_dropped": 47,
  "base_swapped": false
}
```

**`bucketvcs inspect-manifest`** — extend JSON output:

```json
"reachability": {
  "base_manifest": "v00000123",
  "delta_chain_length": 14,
  "delta_chain_bytes": 1240832,
  "delta_files": [
    { "key": "...", "hash": "...", "size_bytes": 88032 },
    ...
  ]
}
```

**`bucketvcs negotiate`** (new debug subcommand) — documented in §5.3.

### 8.2 Docs

- New `docs/m10-reachability-operator-guide.md` mirroring M8 / M9 guides:
  - Threshold tuning (busy repo vs idle repo).
  - Cron cadence guidance: compaction tends to fire more often than repack; recommend hourly cron with thresholds, weekly with `--force`.
  - Troubleshooting `reachability.fallback` warnings.
  - Interpreting the new `inspect-manifest` fields.
  - Expected `.bvrd` sizes (typical small-push: ~5–20 KB).
- Update `docs/m9-maintenance-operator-guide.md` cross-referencing the new thresholds and the index-only-refresh outcome.
- Update top-level `README` package list to mention `internal/reachability` + the M10 cold-fetch property.

## 9. Out of scope (deferred by design)

Carry forward to later milestones:

- **Partitioned base index** (§14.4) — M10.5 or a dedicated large-repo milestone when a real workload demands it.
- **Warm-pool routing** (§14.4) — same.
- **Trees / blobs / tags in delta** (Q3=C reserved slots) — M11 alongside bundle URIs, or M9.5 alongside bitmaps.
- **Bitmaps inside `.bvrd`** — M9.5.
- **Pure-Go pack-objects / mirror-less delivery** — M11+.
- **Bloom filters for path-aware history acceleration** (§14 mention) — not on the roadmap.
- **Auto-compaction inside `bucketvcs serve`** (push-time trigger when threshold crossed) — clean follow-up after M9.5 / M10.5.

## 10. Follow-ups carried from earlier milestones

These predate M10 and remain open:

1. **Push branch + draft PR** to verify the conformance/emulators CI job runs the new `RunPropertyReachabilitySafety` tests. (Carried from M7/M8/M9.)
2. **Real-cloud CI secrets** so the nightly job actually exercises R2 / S3 / GCS / Azure. (Carried from M7/M8/M9.)

## 11. Package layout summary

New:

- `internal/reachability/` — `Set`, `Load`, walk primitives.
- `internal/reachability/deltaindex/` — `.bvrd` encode/decode/read.
- `internal/reachability/conformance/` — `RunPropertyReachabilitySafety` factory.
- `cmd/bucketvcs/negotiate.go` — debug subcommand.
- `docs/m10-reachability-operator-guide.md`.

Modified:

- `internal/commitgraph/` — v2 format + reader/writer.
- `internal/repo/manifest/body.go` — `Indexes.Reachability` field, `ReachabilityRef` type, new `SizeBytes` field on `IndexRef`.
- `internal/repo/keys/keys.go` — new `ReachabilityDeltaKey(hash)` helper.
- `internal/gitproto/receivepack/` — delta build + upload + manifest patch.
- `internal/gitproto/uploadpack/` — negotiate-before-materialize split.
- `internal/maintenance/` — Phase-0 reachability thresholds, index-only-refresh path, CAS-merge body extension.
- `internal/gc/` — live-set walk extended to `.bvrd`; sweep prefix added; `compaction_during_mark` interleaving.
- `internal/diffharness/` — `ImportPushCompactNegotiateExportAndCompare` + new fixtures.
- `cmd/bucketvcs/maintenance.go` — new flags + JSON report field.
- `cmd/bucketvcs/inspect-manifest.go` — new JSON output fields.
- `docs/m9-maintenance-operator-guide.md` — cross-references.
- `README.md` — package list + M10 property.

External dependencies: **zero new**.
