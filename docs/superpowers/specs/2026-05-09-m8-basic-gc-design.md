# M8 — Basic Garbage Collection

Date: 2026-05-09
Status: design draft (brainstormed; awaiting user review)
Source spec sections: §25, §31, §32, §33.1, §33.5, §43.6, §44.13
Decomposition source: `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md` (M8 row)

## 0. Executive summary

M8 ships `bucketvcs gc`: an operator-driven CLI that conservatively reclaims four categories of orphaned storage from a bucketvcs repo:

1. Tx records left by lost CAS attempts (M1's documented promise).
2. Canonical packs uploaded by import/push that crashed before the manifest committed (§33.1).
3. Canonical packs that were once referenced by a manifest but a force-push or branch deletion made unreachable (§43.6).
4. Stale reachability indexes (`.bvom`, `.bvcg`, reachability JSON) that the current manifest no longer points at.

It uses an immutable mark-record / wait-retention / re-read-current-manifest / sweep-record protocol per §25, with per-candidate `first_seen_unreachable_at` carryover so retention is measured from the time an object became unreachable (§43.6).

Sweep-target choices made in brainstorming and explicitly out of M8:

- **In-binary multipart cleanup** is deferred. §33.5 explicitly accepts "lifecycle policy *or* cleanup worker." We document per-cloud bucket lifecycle recipes in the operator guide; in-binary cleanup is a future milestone with a focused `ObjectStore` surface extension (`ListIncompleteMultipartUploads`, `AbortMultipart`).
- **Stale `packs/generated/` GC** is deferred — `keys.GeneratedPackKey` is constructor-only in the tree today; nothing emits dynamic packs yet. Pairs with whoever first writes there.
- **Object-level GC and repack** belong to M9 per the decomposition.

A small, backward-compatible patch to M1 (`tx.WriteCommitMarker`) is the only cross-milestone change M8 introduces — it makes M1's documented orphan-tx behavior honest.

## 1. Scope

### 1.1 In scope

| ID | Sweep target | Driving spec section |
|----|-------------|----------------------|
| A | Orphan tx records (lost CAS attempts, failed `Create` retries) | §6, §8, §43.6 (audit trail integrity) |
| B | Orphan canonical packs from §33.1 (uploaded, never referenced) | §33.1 |
| C | Unreachable canonical packs from §43.6 (force-push / branch delete) | §43.6 |
| D | Stale indexes (`object-map/`, `commit-graph/`, `reachability/`) not pointed at by current manifest | §25, §15.3 |

Operational deliverables:

- `bucketvcs gc` CLI subcommand (§35 list).
- Per-cloud "bucket lifecycle policy for incomplete multipart uploads" recipes appended to `m5-cloud-quickstart.md` and `m7-cloud-quickstart.md` (covers §33.5 via the lifecycle-policy branch).
- Optional localfs startup janitor for incomplete multipart sessions, only if M0/M5/M7 localfs multipart implementation does not already clean tempfiles on process exit. Verified during implementation; if already clean, no janitor is shipped.
- `internal/gc/` package family.
- `tx.WriteCommitMarker` and call-site additions in `internal/repo` (Phase 0 of the implementation plan).
- §31 audit emission via `audit=true`-tagged structured log lines (durable audit-store lands in M15).
- §32 metrics.

### 1.2 Explicitly out of scope (deferred)

- Object-level GC inside packs / repack pipeline — **M9**.
- Stale `packs/generated/` GC — paired with the milestone that introduces dynamic-pack writers.
- In-binary multipart cleanup requiring `ObjectStore` surface extension — focused future milestone.
- Active-session and signed-URL marking (§25 steps 3, 4) — covered by retention-window dominance over realistic clone/URL lifetimes; revisited if a serve-integrated GC mode is added later.
- Serve-integrated background GC — a `bucketvcs serve` extension; future milestone.
- Cross-process GC leases — not needed for the CLI-only single-writer model.
- Manifest archival enabling §25 step 2 ("mark from recent manifests" beyond the current one) — replaced for M8 by retention window + sweep-time re-read of current manifest. Revisited when manifest archival lands.
- Bundle GC (§16.3) — `BundleEntry` is an M11 placeholder; the live-set construction recognizes the manifest field but the iteration is empty today.
- Ref-shard GC — `manifest/ref-shards/` is M12 territory; live-set construction recognizes the field but the iteration is empty today.

## 2. Operational model

### 2.1 CLI-only

GC runs as a one-shot process invoked via `bucketvcs gc …`. Operators schedule it via cron, Kubernetes CronJob, systemd timer, or equivalent.

Rationale (from brainstorming):

- §35 explicitly lists `bucketvcs gc` as part of the OSS CLI surface; the contract is the CLI.
- The "active sessions / signed URLs" marking that an in-process model would enable (§25 steps 3, 4) is replaced by the operational rule **retention window > max realistic session lifetime**. The default 7-day retention dominates any realistic clone or signed-URL TTL, satisfying the spec's safety intent without cross-process plumbing.
- Cron / CronJob / systemd timer is the universal scheduler. An in-binary scheduler reinvents that on every host.
- Single-process ownership means the §43.6 safety story is retention + re-read, not a distributed lease.
- Keeps `bucketvcs serve` performance-critical surface free of GC scheduling concerns.

### 2.2 Multi-repo invocation

`bucketvcs gc` accepts `--repo=<tenant>/<repo>` (single-repo mode) **or** `--all-repos` (enumerate `tenants/*/repos/*` and gc each in sequence). The two flags are mutually exclusive and exactly one is required.

`--all-repos` discovers repos by `List(prefix="tenants/", delimiter="/")` to enumerate tenants, then `List(prefix="tenants/<t>/repos/", delimiter="/")` to enumerate repos. No new index is needed; the adapter contract's existing prefix-list with delimiter is sufficient.

Per-repo failures in `--all-repos` mode are isolated: the failing repo is logged with its `repo_id` and the run continues with the remaining repos. The final summary names the failed repos and the process exits with code `1` if any repo failed.

### 2.3 Concurrency

`--max-concurrency=N` controls per-repo concurrent `DeleteIfVersionMatches` calls in the sweep phase (default `1` for predictable cost). Cross-repo parallelism in `--all-repos` mode is sequential in M8; future enhancement if needed.

Multiple concurrent `bucketvcs gc` invocations against the same repo are not protected by an explicit lease in M8. The §43.6 safety properties hold regardless because:

- Mark records are written via `PutIfAbsent` on a unique `mk_<ulid>` key — concurrent marks produce two distinct records, neither overwrites the other.
- Sweep operates on a specific mark record's candidate list; two concurrent sweeps acting on the same mark record will race on `DeleteIfVersionMatches` per key — the loser sees `version_mismatch` (or `not_found`) and skips; no incorrect deletion.
- Sweep records likewise use `PutIfAbsent` on `sw_<ulid>` keys.

Operators are advised to schedule single-instance gc per repo (cron's standard idiom), but accidental double-runs are safe.

## 3. M1 prerequisite — `tx.WriteCommitMarker`

### 3.1 Problem

M1's `repo.Commit` mints a fresh `tx_id` per attempt and writes the tx record **before** the CAS. The `internal/repo/repo.go` design comments call out: "Lost attempts leave orphan tx records on disk for M8 GC."

To make that statement honest, M8 must distinguish a *winning* tx record (must keep — it is the durable audit trail of a committed push) from a *losing* one (sweepable). The current state gives only `latest_tx` from the *current* manifest. Between two gc runs, dozens of winning `tx_id`s could have come and gone as `latest_tx`, with no on-disk evidence of which were winners.

### 3.2 Solution

Add `tx.WriteCommitMarker(ctx, store, txKey)`: best-effort `PutIfAbsent` of an empty body at the sibling key `<txKey>.commit`. Concretely, since `keys.TxRecordKey(txID)` returns `<repo>/tx/<txID>.json`, the marker key is `<repo>/tx/<txID>.json.commit`. See §3.5 for the full key shape and rationale.

Call sites:

1. `repo.Commit` — after a successful `manifest.CASRoot`, before returning the winning `txID`.
2. `repo.Create` — after the root `PutIfAbsent` succeeds, before returning the new `Repo`.

A failure to write the commit marker is logged and ignored. The CAS has already committed; we do not roll back a successful push because of a marker write hiccup.

### 3.3 Backwards compatibility — the "first marker found" gate

Old tx records written by pre-M8 binaries have no commit marker. Treating absence-of-marker as "orphan" would falsely sweep historical winners.

GC handles this with a per-repo gate `tx_orphan_sweep_armed`:

```
tx_orphan_sweep_armed = ∃ at least one tx record in this repo that has a .commit sibling
```

When `tx_orphan_sweep_armed = false`, tx orphan sweeping is skipped for the repo and the mark record records the disarm. Once a single post-M8 commit lands (and writes a marker), the gate flips to `true` permanently for that repo (mark-record state persists across gc runs).

This preserves the audit trail of pre-M8 commits forever — those tx records are simply never classified as orphans, even if they happen to satisfy retention. Acceptable cost: a small constant of pre-M8 winning tx records is retained until manual cleanup, which a future repair tool (M16) can handle precisely.

### 3.4 Crash window between CAS-success and marker-write

If `repo.Commit` crashes after `CASRoot` returns success but before `WriteCommitMarker` runs, the repo has a winning tx record with no marker. To prevent that record from being misclassified as orphan:

```
orphan tx = tx_record_exists
        AND no .commit sibling
        AND tx_id != current_manifest.latest_tx
        AND tx_orphan_sweep_armed = true
        AND age > retention
```

The `tx_id != current_manifest.latest_tx` clause protects the most recent commit specifically — which is the only commit that could be in the crash window at any given moment. Once another push lands, the at-risk `tx_id` is no longer current.

For M8 itself, the at-risk crash window is bounded operationally by the retention window: a winner whose marker write crashed and which was superseded as `latest_tx` could become a candidate sweep target at age > retention. We accept this bounded false-orphan risk in exchange for the simplicity of best-effort marker-write. Mitigation if needed: the operator can run `bucketvcs gc --mark-only` and inspect the candidate list before letting a sweep proceed. Precise reconstruction of which tx records were winners across history is a future M16 repair-tool concern, not M8's.

### 3.5 Schema

Commit-marker objects are zero-byte. Their existence is the signal. The key is the existing tx record key with `.commit` appended:

```
tx/tx_<ulid>.json          # tx record body
tx/tx_<ulid>.json.commit   # zero-byte marker
```

Using `.json.commit` rather than `tx_<ulid>.commit` keeps both objects under the same prefix segment so the M8 List of `tx/` returns them adjacent and a single sort groups marker-with-record.

### 3.6 Test additions in M1

Three new tests in `internal/repo/repo_test.go`:

1. `TestCommit_WritesCommitMarkerOnSuccess` — successful commit produces both record and marker.
2. `TestCreate_WritesCommitMarkerOnSuccess` — successful create produces both.
3. `TestCommit_MarkerWriteFailureDoesNotFailCommit` — inject a marker-write failure; verify Commit returns success and the missing marker is logged.

## 4. Architecture and package layout

### 4.1 Directory layout

```
internal/gc/
    gc.go            # Run, RunOptions, RunReport — top-level orchestration
    liveset.go       # build live-set from current manifest
    discover.go      # list candidates per category, subtract live-set
    mark.go          # mark phase: carryover + write immutable mark record
    sweep.go         # sweep phase: re-read manifest, filter, DeleteIfVersionMatches
    retention.go     # duration arithmetic, defaults, threshold warnings
    marks/           # immutable mark-record schema, reader, writer
        record.go
        record_test.go
        write.go
        read.go
    sweeps/          # immutable sweep-record schema, reader, writer
        record.go
        record_test.go
        write.go
        read.go
    gctest/          # test fixtures shared with cmd/bucketvcs tests
        fixtures.go

cmd/bucketvcs/
    gc.go            # CLI subcommand wiring
    gc_test.go

docs/
    m8-gc-operator-guide.md   # retention, lifecycle recipes, cron examples
```

`internal/gc/` is a sibling of `internal/repo` and `internal/storage`. It depends on both:

- `internal/storage` for `ObjectStore.List`, `Head`, `DeleteIfVersionMatches`, `Get` (to read the previous mark record), `PutIfAbsent` (to write mark and sweep records).
- `internal/repo` for `repo.Open`, `repo.ReadRoot`, and `keys.Repo` (path constructors). GC never calls `repo.Commit` — sweep is direct `DeleteIfVersionMatches`, mark/sweep records are direct writes outside the manifest.

### 4.2 Why `internal/gc/` (not `internal/repo/gc/` or `internal/maintenance/gc/`)

- Sibling placement matches the existing convention (`internal/repo`, `internal/pack`, `internal/storage`, `internal/auth`, etc.).
- M8 GC reads the manifest but never mutates via `repo.Commit`. Putting it under `repo/` overstates coupling.
- We do not have M9/M11/M16 designs yet; pre-grouping under `internal/maintenance/` is speculative. M9 repack is more likely to live in `internal/pack/repack` (pack engine extension); M16 repair is doctor-style, different shape.
- Easy to refactor `internal/gc/` → `internal/maintenance/gc/` later if a real grouping emerges.

## 5. Live-set construction

For one repo, on each gc run, computed once at the start of the mark phase from the current root manifest:

```
view = repo.ReadRoot(ctx)                     # header + body + ObjectVersion + size
header = view.Header
body   = parse(view.Body)                     # manifest.Body

live = {
  keys.RootManifestKey(),
  keys.TxRecordKey(header.LatestTx),
  keys.TxRecordKey(header.LatestTx) + ".commit",
}

for each PackEntry p in body.Packs:
    live ∪= { p.PackKey, p.IdxKey }
    if p has BitmapKey field set (M2 may emit; future-proofed):
        live ∪= { p.BitmapKey }

if body.Indexes.ObjectMap != nil:
    live ∪= { body.Indexes.ObjectMap.Key }
if body.Indexes.CommitGraph != nil:
    live ∪= { body.Indexes.CommitGraph.Key }

# Forward-compatible: future fields recognized, currently empty
for each BundleEntry b in body.Bundles:        # M11 placeholder
    live ∪= { b.BundleKey, b.BundleManifestKey }
for each shard s in body.RefShards (when added by M12):
    live ∪= { s.Key }
```

Properties:

- The live-set is a flat set of full storage keys.
- It is **prefix-rooted** to `keys.Repo.Prefix()`. GC never adds, removes, or considers any key outside the repo it is processing.
- `manifest_object_version` (the `view.Version.Token` from `ObjectStore.Head`) is captured separately and recorded in the mark record as `current_manifest_object_version`. Sweep does *not* use it for safety decisions (sweep does its own re-read), but it is a useful audit field when reasoning about which manifest version a mark was computed against.

## 6. Candidate discovery

For each sweep-target category, list the relevant prefix and subtract the live-set.

### 6.1 Orphan tx records

```
tx_listing = List(prefix=keys.TxPrefix(), delimiter="")
for each entry in tx_listing:
    if entry.Key ends with ".commit":
        record_marker_for(entry.Key without ".commit")
        continue
    if entry.Key in live:                                 # current latest_tx
        continue
    if marker_recorded_for(entry.Key):                    # winning tx, already committed
        continue
    candidates.tx_records.add(entry.Key)
```

Then filter by `tx_orphan_sweep_armed`:

```
tx_orphan_sweep_armed = (any marker observed in this listing)
                     OR (previous mark record had tx_orphan_sweep_armed=true)
if tx_orphan_sweep_armed = false:
    drop all candidates.tx_records (record reason "tx_sweep_disarmed" in mark record)
```

Once a repo has any `.commit` marker on disk, `tx_orphan_sweep_armed` is permanently true (carried via the previous mark record; once observed, never unobserved).

### 6.2 Orphan canonical packs

```
pack_listing = List(prefix=keys.Prefix() + "packs/canonical/", delimiter="")
for each entry in pack_listing:
    if entry.Key in live:
        continue
    candidates.canonical_packs.add(entry.Key)
```

This naturally captures `.pack`, `.idx`, and `.bitmap` triples — each is its own listing entry; if the manifest references `p.PackKey` and `p.IdxKey`, both are in `live`; a stray `.idx` from a crashed import without a manifest entry is correctly classified as a candidate.

### 6.3 Stale indexes

```
for each prefix in {
    keys.Prefix() + "indexes/object-map/",
    keys.Prefix() + "indexes/commit-graph/",
    keys.Prefix() + "indexes/reachability/",
}:
    listing = List(prefix=prefix, delimiter="")
    for each entry in listing:
        if entry.Key in live:
            continue
        candidates.indexes.add(entry.Key)
```

### 6.4 Pagination and large repos

Each `List` is paginated via `ListOptions.ContinuationToken` until `NextToken == ""`. Pagination is opaque to discovery — `discover.go` accumulates all pages into a single slice per category for the mark record.

For very large repos this can be memory-bounded — a 10M-pack repo would accumulate ~10M strings (~1 GB at ~100 bytes/entry). M8 ships with the simple in-memory accumulation; if a deployment reports OOM, future enhancement is a streaming mark writer that emits `.json.zst` shards. Not in M8 scope; documented as a known limit in the operator guide.

## 7. Mark phase

### 7.1 Mark record schema

```
{
  "schema_version": 1,
  "mark_id": "mk_<ulid>",
  "previous_mark_id": "mk_<ulid>" | null,
  "started_at": "<RFC3339Nano>",
  "completed_at": "<RFC3339Nano>",
  "current_manifest_version": <uint64>,
  "current_manifest_object_version": "<token>",
  "retention_seconds": <int>,
  "tx_orphan_sweep_armed": <bool>,
  "candidates": {
    "tx_records":      [ { "key": "...", "first_seen_unreachable_at": "..." } ],
    "canonical_packs": [ { "key": "...",
                           "first_seen_unreachable_at": "...",
                           "last_seen_reachable_at": "..." | null,
                           "mark_manifest_version": <uint64> } ],
    "indexes":         [ { "key": "...", "first_seen_unreachable_at": "..." } ]
  }
}
```

Field semantics:

- `mark_id` — fresh ULID minted at the start of the mark phase.
- `previous_mark_id` — ULID of the most recent prior mark record (None on first run).
- `current_manifest_version`, `current_manifest_object_version` — the manifest snapshot the live-set was computed from. Audit only; sweep does its own re-read.
- `retention_seconds` — the run's retention window. Recorded so a sweep run with `--sweep-only` against an old mark uses the retention recorded at mark time, not the operator's current flag.
- `tx_orphan_sweep_armed` — the §3.3 gate for this repo.
- For each candidate, `first_seen_unreachable_at` is the carryover-or-now timestamp.
- `last_seen_reachable_at` (canonical packs only) is set on the run that observes the transition `was-in-live-set in prev → not-in-live-set in this run`. Otherwise carried forward unchanged. Null if never observed.
- `mark_manifest_version` (canonical packs only) is set on the run that introduces the candidate (either at first sighting, or carried forward unchanged).

### 7.2 Carryover algorithm

For each candidate `k` discovered in run R (under any category):

```
prev = read_previous_mark_record(repo)            # see §7.3
if prev exists AND k in prev.candidates.<category>:
    entry = prev.entry(k)
    first_seen_unreachable_at = entry.first_seen_unreachable_at
    if category == canonical_packs:
        last_seen_reachable_at = entry.last_seen_reachable_at  # unchanged
        mark_manifest_version  = entry.mark_manifest_version   # unchanged
else:
    first_seen_unreachable_at = R.started_at
    if category == canonical_packs:
        # If we know the candidate was reachable in the immediately previous mark
        # but is not in this one, populate last_seen_reachable_at; otherwise null.
        if prev exists AND k WAS in prev.live (we did not store live; see below):
            last_seen_reachable_at = prev.completed_at
        else:
            last_seen_reachable_at = null
        mark_manifest_version = R.current_manifest_version
```

The "was k in prev.live" check is awkward — we do not persist the live-set in the mark record (it would balloon in a 100k-pack repo). The practical proxy: `last_seen_reachable_at` is set only on a transition that the *current* mark phase can detect, which means we set it when `k` is now a candidate but `k` was *not* a candidate in the previous mark record — i.e., between `prev.completed_at` and `R.started_at`, `k` left the live-set. We attribute the transition time to `prev.completed_at` (a lower bound — actual transition was somewhere between `prev.completed_at` and `R.started_at`).

Where we cannot infer a transition (first run, or `k` was in a category-of-its-own absent from prev), we leave `last_seen_reachable_at` null. This honors §43.6's SHOULD without synthesizing data.

### 7.3 Reading the previous mark record

```
listing = List(prefix=keys.GCMarkKey-prefix, delimiter="")
sort listing by Key descending  # ULIDs sort lexicographically by time
prev = first entry, if any
read prev via ObjectStore.Get; parse JSON
```

Because mark IDs are ULIDs, lexicographic sort is time-ordered. We trim mark records to the most recent 10 in the prune step (§9), so the `List` here is bounded.

### 7.4 Writing the mark record

```
mark_id = mint ULID
mark = build_record(...)
bytes = canonical_json_marshal(mark)
PutIfAbsent(keys.GCMarkKey(mark_id), bytes)
```

`PutIfAbsent` ensures concurrent gc invocations cannot collide on the same mark_id (vanishingly unlikely with ULID, but the contract is enforced).

## 8. Sweep phase

### 8.1 Algorithm

```
mark = read_target_mark_record(repo)              # most recent unless --sweep-only specified
view = repo.ReadRoot(ctx)                          # FRESH read at start of sweep (§25 step 8)
fresh_live = build_live_set(view)
now = current time

deleted, skipped, errors = empty lists

for category in { tx_records, canonical_packs, indexes }:
    for candidate in mark.candidates[category]:

        # 1. Re-reachable check (§25 step 9, §43.6 newly-committed-wins)
        if candidate.key in fresh_live:
            skipped.add(candidate.key, reason="revived")
            continue

        # 2. Retention gate (§43.6 retention measured from time-unreachable)
        age = now - candidate.first_seen_unreachable_at
        if age < retention_window:
            skipped.add(candidate.key, reason="retention_not_met")
            continue

        # 3. Tx-sweep-armed gate (only relevant for tx_records)
        if category == tx_records AND mark.tx_orphan_sweep_armed == false:
            skipped.add(candidate.key, reason="tx_sweep_disarmed")
            continue

        # 4. Get current ObjectVersion to scope the delete
        meta, err = Head(candidate.key)
        if err == ErrNotFound:
            skipped.add(candidate.key, reason="not_found")
            continue
        if err != nil:
            errors.add(candidate.key, err)
            continue

        # 5. Conditional delete
        err = DeleteIfVersionMatches(candidate.key, meta.Version)
        if err == ErrVersionMismatch:
            skipped.add(candidate.key, reason="version_mismatch")
            continue
        if err == ErrNotFound:
            skipped.add(candidate.key, reason="not_found")
            continue
        if err != nil:
            errors.add(candidate.key, err)
            continue

        # 6. For tx records: defensively attempt to delete the .commit sibling.
        # In normal operation no marker exists for a candidate tx record (a
        # candidate must not have had a marker at mark time, and losing CAS
        # attempts never get a later marker via repo.Commit). The delete is
        # idempotent and silent on NotFound; it exists to clean up any stray
        # marker introduced out-of-band (e.g., a future repair tool).
        if category == tx_records:
            err = best_effort_delete(candidate.key + ".commit")
            # not counted as deletion, not counted as error

        deleted.add(candidate.key)

# Write immutable sweep record at gc/sweeps/sw_<ulid>.json
write_sweep_record(deleted, skipped, errors, mark.mark_id)
```

`retention_window` for sweep is `mark.retention_seconds`, not the operator's current `--retention` flag. This means an old `--mark-only` run pinned its retention into the record, and a later `--sweep-only` honors that pinned value rather than retroactively shortening or lengthening it.

### 8.2 Sweep record schema

```
{
  "schema_version": 1,
  "sweep_id": "sw_<ulid>",
  "mark_id": "mk_<ulid>",
  "started_at": "<RFC3339Nano>",
  "completed_at": "<RFC3339Nano>",
  "current_manifest_version": <uint64>,
  "current_manifest_object_version": "<token>",
  "deleted": {
    "tx_records": [ "key", ... ],
    "canonical_packs": [ "key", ... ],
    "indexes": [ "key", ... ]
  },
  "skipped": [
    { "key": "...", "category": "...", "reason": "revived" | "retention_not_met" | "version_mismatch" | "not_found" | "tx_sweep_disarmed" }
  ],
  "errors": [
    { "key": "...", "category": "...", "error": "..." }
  ]
}
```

`current_manifest_version` and `current_manifest_object_version` are the values from the **fresh re-read** at the start of sweep, NOT the values from the mark. An auditor comparing mark and sweep records can see exactly how far the manifest advanced between mark and sweep.

### 8.3 Sweep concurrency within a repo

`--max-concurrency=N` (default 1) controls the worker pool size for the per-candidate loop. Each worker independently does Head → DeleteIfVersionMatches; results merge into shared deleted/skipped/errors slices via a mutex.

Default 1 keeps cost predictable across cloud backends (each Delete is one round-trip; serial sweep is bandwidth-friendly). Operators with large unreachable sets and low-latency-storage budgets can dial up.

## 9. §43.6 correctness story

The headline scenario:

```
Time   Push                                       GC
-----  -----                                      ---
T0     manifest v=V refs pack X                   mark phase: reads V, X is in live, skipped
T1     force-push commits v=V+1 dropping X        mark already done; X is candidate only if mark
                                                  ran AFTER T1
T2                                                mark record persisted, await retention
T3+r   (concurrent push)                          sweep starts: re-reads manifest, sees v=V+K
       commits v=V+K+1 reviving X                 sweep's fresh_live computed from v=V+K (misses revival)
T3+r+ε                                            Head(X) → version v_X
                                                  DeleteIfVersionMatches(X, v_X) → success
                                                  X is deleted; revival pointed at a now-missing object
```

This race is fundamental to the model and the spec acknowledges it implicitly via the retention window. Mitigations:

1. **Default 7-day retention** dominates any plausible "drop pack X then re-push X 7 days later" pattern. Force-pushes and revivals within a week of each other produce the right safety margin operationally.
2. **Sweep re-reads the manifest as close to the Delete as possible** — but a TOCTOU window remains between re-read and per-key Delete.
3. **`DeleteIfVersionMatches`** would catch a concurrent re-upload at the same key with different bytes (different version) — but packs are content-addressed and not rewritten in place, so the version is unchanged on revival. The retention window is the only defense against the same-content revival case.
4. **Audit fields** (`mark_manifest_version`, `first_seen_unreachable_at`, sweep record's `current_manifest_version`) make post-incident reasoning exact: an operator can see exactly which manifest version made the pack unreachable, when GC first observed it, and which manifest version was current at sweep time.
5. **`DeleteIfVersionMatches` reports `ErrNotFound` and `ErrVersionMismatch` separately** so the sweep record distinguishes "concurrent-deleted-by-someone-else" (`not_found`) from "concurrent-modified" (`version_mismatch`).

The operator guide documents the race window honestly and recommends:

- Run gc during low-activity windows where possible.
- Do not set `--retention` below 24h without a specific reason; the warning is intentional.
- For repos with active force-push workflows (rare in production), increase retention proportionally to the typical "drop then revive" gap.

## 10. CLI

### 10.1 Surface

```
bucketvcs gc \
    --store=<scheme://...>            (required, existing convention)
    --repo=<tenant>/<repo>            (xor with --all-repos)
    --all-repos                       (xor with --repo)
    --retention=<duration>            (default 168h; warn-stderr if < 24h)
    --max-orphan-age=<duration>       (advisory; off by default)
    --max-concurrency=<n>             (per-repo sweep workers; default 1)
    --mark-only                       (run mark, skip sweep)
    --sweep-only                      (skip mark, sweep most recent existing mark)
    --dry-run                         (compute candidates, write nothing, delete nothing)
    --format=text|json                (default text)
```

Mutual exclusions:
- `--repo` xor `--all-repos` (one required).
- `--mark-only` xor `--sweep-only`.
- `--dry-run` overrides write/delete in either phase: no mark record, no sweep record, no Deletes.

`--max-orphan-age` is purely advisory — when set, the run emits a warning per candidate whose `first_seen_unreachable_at` exceeds the threshold. Useful as a "GC is not keeping up" alarm.

### 10.2 Output (text format)

Per repo:

```
repo tenant/repo @ manifest v1234
  mark    mk_01HZ...   candidates: tx=12 packs=3 indexes=1            (1.2s)
  sweep   sw_01HZ...   deleted: tx=8 packs=2 indexes=1
                       skipped: revived=0 retention=7 vmismatch=0 notfound=1
                       errors: 0                                       (2.4s)
```

In `--all-repos` mode, an aggregate summary follows the per-repo lines:

```
total: 14 repos | deleted: tx=104 packs=27 indexes=12 | errors_total=0
```

### 10.3 Output (json format)

One line per repo, NDJSON-style for `jq` piping:

```
{"repo_id":"tenant/repo","mark_id":"mk_...","sweep_id":"sw_...",
 "manifest_version":1234,"deleted":{"tx_records":8,"canonical_packs":2,"indexes":1},
 "skipped":{"revived":0,"retention_not_met":7,"version_mismatch":0,"not_found":1,"tx_sweep_disarmed":0},
 "errors":[],"mark_duration_seconds":1.24,"sweep_duration_seconds":2.41}
```

In `--all-repos` mode, a final aggregate line follows.

### 10.4 Exit codes

| Code | Meaning |
|------|---------|
| 0 | Clean run; zero errors and zero `version_mismatch` |
| 1 | Operational error (store unreachable, manifest schema unsupported, repo not found, invalid flags) |
| 2 | Ran successfully but left work behind (any `version_mismatch` or per-key errors) |

Exit 2 is intentionally distinct from exit 1 — a cron monitor can treat exit 2 as "investigate later" rather than "page someone."

## 11. Observability

### 11.1 Structured logs (slog)

Adopting the M3+ `log/slog` convention. All gc log lines carry `subsystem=gc` and `repo_id` where applicable.

| Event | Fields | Audit-tagged |
|------|--------|--------------|
| `gc.run.started` | `repo_id`, `mark_only`, `sweep_only`, `dry_run`, `retention_seconds` | no |
| `gc.run.completed` | `repo_id`, `mark_id`, `sweep_id`, `deleted_total`, `skipped_total`, `errors_total`, `duration_seconds` | no |
| `gc.mark.completed` | `repo_id`, `mark_id`, `manifest_version`, `candidate_counts{type}` | **yes** |
| `gc.sweep.completed` | `repo_id`, `sweep_id`, `mark_id`, `deleted_counts{type}`, `skipped_counts{reason}`, `errors_count` | **yes** |
| `gc.sweep.error` | `repo_id`, `key`, `category`, `error` | no |
| `gc.disarmed` | `repo_id`, `reason` (e.g., "no commit markers observed") | no |

Audit-tagged events satisfy §31 today via structured logs; M15 will route `audit=true` lines into the durable audit-store without changing call sites.

We do **not** emit per-deleted-key INFO lines — at M8's expected scale (thousands of orphan tx records on a busy repo) that would dominate logs. Per-key TRACE only when explicitly enabled.

### 11.2 Metrics

| Name | Type | Labels |
|------|------|--------|
| `gc_run_duration_seconds` | histogram | `phase=mark\|sweep`, `repo_id` |
| `gc_candidates_total` | counter | `type=tx_records\|canonical_packs\|indexes`, `repo_id` |
| `gc_deleted_total` | counter | `type`, `repo_id` |
| `gc_skipped_total` | counter | `reason=revived\|retention_not_met\|version_mismatch\|not_found\|tx_sweep_disarmed`, `repo_id` |
| `gc_errors_total` | counter | `repo_id` |

Wired through whatever metrics convention M3 established (verified during implementation).

## 12. Failure handling

| Scenario | Behavior |
|----------|----------|
| Crash during mark, no record written | Next gc run starts fresh; previous run had no on-disk effect. |
| Crash after mark, before sweep | Mark record exists, no sweep record. Next gc run with `--sweep-only` sweeps that mark; default mode produces a fresh mark whose carryover correctly reuses the prior mark. |
| Crash mid-sweep | Some candidates deleted, no sweep record. Next mark observes the deleted ones simply as not-present; remaining candidates carry forward. The partial sweep is visible in metrics/logs but not in any sweep record (we choose all-or-nothing for the immutable audit record). |
| `version_mismatch` on a Delete | Skip, log, count, continue. Next run re-marks; if still unreachable, re-attempt. |
| `--all-repos` with one repo failing | Per-repo failure isolated; logged with `repo_id`; run continues; final summary names failed repos; exit code `1` if any repo failed. |
| Manifest schema unsupported (`ErrUnsupportedSchema`) | Per-repo failure (same isolation as above). |
| Manifest missing (`ErrRepoNotFound`) | In `--all-repos` mode: the repo was concurrently deleted between discovery and processing — log and skip. In `--repo=...` mode: exit code 1. |

## 13. Testing strategy

### 13.1 Unit tests (`internal/gc/`)

- `liveset.go` — building from synthetic `manifest.Body` values; nil/empty handling; M11/M12 placeholder fields.
- `discover.go` — candidate discovery from mocked `List` paginations; live-set subtraction correctness; tx_records-vs-markers parsing.
- `mark.go` — carryover from mocked previous mark records; first-run behavior; tx_orphan_sweep_armed transitions.
- `sweep.go` — filter precedence (revived > retention > tx_disarmed > version checks); error classification.
- `retention.go` — duration parsing, threshold warnings, default value.

### 13.2 Integration tests (`internal/gc/` and `cmd/bucketvcs/`)

Against the existing localfs `ObjectStore`:

- `TestGC_OrphanTxRecord_SweptAfterRetention` — drive concurrent CAS contention via `repo.Commit` to produce real orphan tx records, sweep them after retention; verify only orphans deleted, winners' records and markers preserved.
- `TestGC_OrphanPack_FromCrashedImport` — write a pack via `PutIfAbsent` outside any manifest commit, sweep it.
- `TestGC_UnreachablePack_FromForcePush` — push pack X, force-push to drop it, sweep after retention.
- `TestGC_PushDuringSweep_43_6` — between mark and sweep, do a push that revives a candidate; verify it is skipped with reason="revived".
- `TestGC_RetentionNotMet_DefersDeletion` — sweep immediately after mark; verify all candidates skipped with reason="retention_not_met".
- `TestGC_FirstRun_TxOrphanSweepDisarmed` — run gc against a repo with no commit markers; verify tx orphans are NOT swept and the disarm is recorded.
- `TestGC_AllRepos_SequentialFailureIsolation` — three repos, middle repo's manifest deliberately corrupted; verify other two complete and exit code is 1.
- `TestGC_DryRun_NoEffect` — dry-run produces output but writes no mark/sweep records and deletes no candidates.
- `TestGC_MarkOnly_ThenSweepOnly_ProducesSameOutcomeAsCombined` — split-phase invocation equivalence.
- `TestGC_RetentionPinnedAtMarkTime` — `--mark-only` with retention=168h, then `--sweep-only` with retention=1s; verify sweep honors the pinned 168h.
- `TestGC_PrunesMarkRecordsBeyond10` — produce 12 marks, verify only most recent 10 retained.

### 13.3 Conformance suite extension

Add `RunPropertyGCSafety(t, factory)` to `internal/repo/internal/` (sibling to the existing `RunPropertyManifestVersionMonotonic`). It exercises:

- A reduced version of `TestGC_PushDuringSweep_43_6` against any `ObjectStore` factory.
- A reduced version of `TestGC_OrphanTxRecord_SweptAfterRetention`.

M5 (s3compat / R2 / S3) and M7 (gcs / azureblob) cloud-conformance jobs pick this up automatically the way they pick up `RunPropertyManifestVersionMonotonic`.

### 13.4 Differential harness

N/A — GC is not a Git-protocol-visible operation; nothing to compare against upstream git.

## 14. Operator guide (`docs/m8-gc-operator-guide.md`)

To be written alongside implementation. Outline:

1. What `bucketvcs gc` does and does not do.
2. Recommended cron schedule (nightly, off-peak; or weekly for low-activity repos).
3. The retention-window decision: defaults, when to lengthen, the warning under 24h.
4. The §43.6 race window — honest description plus mitigations.
5. Per-cloud bucket lifecycle policy recipes for incomplete multipart uploads (covers §33.5 via the lifecycle branch):
   - AWS S3: lifecycle rule "Abort incomplete multipart uploads after N days."
   - Cloudflare R2: equivalent lifecycle config.
   - GCS: lifecycle rule with `AbortIncompleteMultipartUpload` action.
   - Azure Blob: lifecycle policy `delete_uncommitted_blob_after`.
6. localfs operational notes: no lifecycle equivalent; multipart cleanup via process restart (verified during implementation).
7. Reading mark and sweep records for post-incident analysis.
8. Exit code interpretation and recommended cron alerting.
9. Known limit: in-memory candidate accumulation for very large repos (millions of pack files) — future work.

## 15. Open questions resolved at M8

From the decomposition document:

- §44.13 (default GC retention window): **7 days** (`168h`), matching the spec's hosted-floor language; configurable via `--retention`; warning emitted below 24h.

## 16. Open questions deferred past M8

- §44.4 (how much serving path is pure Go in v1) — unaffected by GC; continues to be answered iteratively across M2/M3.
- Active-session and signed-URL marking via in-process state — deferred to a serve-integrated GC milestone if/when load patterns demand it. M8 relies on retention-window dominance.
- In-binary multipart cleanup (`ListIncompleteMultipartUploads` + `AbortMultipart` ObjectStore extension) — focused future milestone with a dedicated adapter-surface design.
- Sub-millisecond TOCTOU between sweep re-read and per-key Delete — bounded by retention window; not addressed structurally in M8.
- Per-deleted-key audit lineage to a future auditor (M15) — M8 emits `audit=true` log lines; M15 routes them.

## 17. Acceptance criteria

M8 ships when:

1. `bucketvcs gc --repo=…` and `bucketvcs gc --all-repos` work against the localfs adapter end-to-end.
2. All unit and integration tests in §13 pass with `-race`.
3. `RunPropertyGCSafety` passes against the localfs factory in CI; it is wired into the M5/M7 cloud-conformance jobs (which run nightly + on `workflow_dispatch` per existing convention) and passes against R2 / S3 / GCS / Azurite emulators in those jobs.
4. M1 patch (`tx.WriteCommitMarker`) merged with its three new tests; existing M1 tests unchanged.
5. `m5-cloud-quickstart.md` and `m7-cloud-quickstart.md` updated with the bucket-lifecycle recipes per §14.5.
6. `docs/m8-gc-operator-guide.md` written.
7. Exit-code, JSON-output, and metric names verified in `cmd/bucketvcs/gc_test.go`.
8. `bucketvcs gc --help` documents every flag with the defaults stated here.

After M8: §35 OSS-scope minimum is complete (per the decomposition's promise). First OSS release candidate.
