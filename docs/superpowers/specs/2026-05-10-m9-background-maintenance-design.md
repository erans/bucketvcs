# M9 — Background maintenance (repack + index refresh)

Date: 2026-05-10
Status: design draft (brainstormed; awaiting user review)
Source spec sections: §15.1, §15.3, §17, §21, §32, §35, §43.6
Decomposition source: `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md` (M9 row)
Predecessor: M8 (`docs/superpowers/specs/2026-05-09-m8-basic-gc-design.md`)

## 0. Executive summary

M9 ships `bucketvcs maintenance`: an operator-driven CLI that runs a single full repack per repo, refreshes the commit-graph (`.bvcg`) and object-map (`.bvom`) indexes against the new pack, and CAS-merges the result into the root manifest.

The pipeline is push-safe via the same retention-window dominance model M8 established (§43.6): late-arriving push packs survive the CAS-merge, old packs become unreachable from the manifest, M8 GC reclaims them after retention. After a successful run the manifest contains exactly one canonical pack plus any concurrent-push packs, indexes that cover the new pack, and pack metadata bounded by construction.

Bitmap (`.bitmap`) generation is split off into a focused successor milestone ("M9.5"). The §15.3 bitmap-coverage trigger ships inert in M9.

Cross-milestone changes: none expected as contract changes. Implementation may grow one additive helper on `internal/pack`; no manifest schema bump.

## 1. Scope

### 1.1 In scope

| ID | Deliverable | Spec |
|----|-------------|------|
| A | Reachability handled by `git rev-list --all --objects` inside `gitcli.PackObjectsAll` against a locally-materialized bare repo (importer's pattern); no Go-side walker needed | §14, §21 |
| B | Single full-repack output: one new `packs/canonical/<hash>.pack` + `.idx` per successful run, produced via `gitcli.PackObjectsAll` against a temp bare repo materialized from the current canonical packs | §15.1, §21 |
| C | Fresh commit-graph (`.bvcg`) built from the same ref tips | §15.3, §21 |
| D | Fresh object-map (`.bvom`) covering the new pack | §15.3, §21 |
| E | CAS-merge protocol that preserves concurrent-push packs (§43.6 / §17) | §43.6, §17 |
| F | §15.3 force-repack triggers: recent-pack count, total-pack count, manifest pack-metadata size | §15.3 |
| G | `bucketvcs maintenance` CLI: `--repo` / `--all-repos`, `--force`, `--dry-run`, JSON / text output, exit codes mirroring M8 | §35 |
| H | §32 metrics + audit log emission consistent with M8 | §32 |
| I | Operator guide: scheduling recipes, threshold tuning, interaction with `bucketvcs gc` | §35 |

### 1.2 Out of scope (deferred to focused successors)

- **Bitmap (`.bitmap`) generation** — own milestone ("M9.5"). The §15.3 "bitmap coverage" trigger and the "force repack when bitmap coverage falls below threshold" rule ship inert in M9 (metric reports `absent`, threshold not configured). Pure-Go EWAH writer + pseudo-merge encoding lands when its own brainstorm does.
- **Generated pack retirement (`packs/generated/`)** — no writer exists in tree (M8 confirmed this). M9 does not introduce one. Pairs with whoever first emits dynamic packs (§16.2).
- **Cache pack retirement (`packs/cache/`)** — no writer. Same disposition.
- **Geometric / tiered repack** — not needed at OSS v1 scale; full repack is the M9 baseline. A successor can add a second strategy under the same entry point without re-architecting.
- **Object-to-pack lookup-latency trigger** (§15.3 trigger #4) — requires fetch-path latency measurement we do not have. Wired-but-inert via a stub probe; the threshold field is omitted from the public flag set so operators do not configure it before it works.
- **In-serve background scheduler** — clean follow-up; M9 structures the package so a future scheduler calls the same `maintenance.Run` entry point.
- **Maintenance leases / cross-process coordination** — single-process invocation model, same safety story as M8.
- **Bundle generation** — M11.
- **Reachability base + delta index model** (§14.1–§14.4) — M10.

## 2. Operational model

### 2.1 CLI-only

`bucketvcs maintenance` runs as a one-shot process. Operators schedule it via cron, Kubernetes CronJob, systemd timer, or equivalent. Single-process invocation; no daemon, no in-binary scheduler.

Rationale (carried from brainstorming):

- §35 lists `bucketvcs maintenance` (alongside `bucketvcs gc`) as part of the OSS CLI surface; the contract is the CLI.
- The §43.6 / §17 push-during-maintenance correctness story is solved by retention dominance + CAS-merge, not by cross-process leases.
- Cron / CronJob / systemd timer is the universal scheduler. An in-binary scheduler reinvents it on every host.
- Keeps `bucketvcs serve`'s performance-critical surface free of maintenance scheduling concerns.
- The package surface is shaped so a future serve-integrated scheduler can call `maintenance.Run` without re-architecting.

### 2.2 Multi-repo invocation

`bucketvcs maintenance` accepts `--repo=<tenant>/<repo>` (single-repo mode) **or** `--all-repos` (enumerate `tenants/*/repos/*` and maintain each in sequence). The two flags are mutually exclusive and exactly one is required.

`--all-repos` discovers repos by `List(prefix="tenants/", delimiter="/")` to enumerate tenants, then `List(prefix="tenants/<t>/repos/", delimiter="/")` to enumerate repos. No new index needed.

Per-repo failures in `--all-repos` mode are isolated: the failing repo is logged with its `repo_id` and the run continues with the remaining repos. The final summary names failed repos and the process exits with code `1` if any repo failed.

### 2.3 Concurrency posture

Two operators running `bucketvcs maintenance` concurrently against the same repo are a "don't do that, but it is safe" case:

- Both walk independently from possibly different `T0` snapshots.
- Both upload new packs / indexes to content-addressed keys (collision-free if same content; independent if different).
- One wins the manifest CAS. The other re-reads, finds its own `T0` superseded, restarts at Phase 0 (which sees the just-completed run, evaluates thresholds against the post-run manifest, and almost certainly returns no-op).
- The losing run's uploads become tx-orphan / canonical-pack-orphan / stale-index candidates for the next M8 GC run after retention.

No lease, no advisory lock. Same posture as `bucketvcs gc`. Documented in the operator guide as "scheduling a single timer per repo per cluster is sufficient; concurrent runs are safe but waste IO."

## 3. Architecture

### 3.1 Package layout

```
internal/maintenance/
  doc.go                      package overview
  options.go                  RunOptions, threshold config, defaults
  run.go                      Run(ctx, store, k, opts) entry point — single repo
  thresholds.go               §15.3 trigger evaluation against current manifest
  pipeline.go                 orchestrates the 7 phases (Phase 0–6)
  materialize.go              download P0 packs into a temp bare repo; write packed-refs + HEAD
  repack.go                   gitcli.PackObjectsAll wrapper; tmpdir lifecycle
  indexes.go                  rebuilds .bvcg + .bvom against the new local pack
  upload.go                   PutIfAbsent the four artifacts to canonical keys
  casmerge.go                 the CAS-merge attempt loop; produces M_new from (M0, M_now, our-output)
  audit.go                    structured log emission, mirrors internal/gc/audit.go
  metrics.go                  §32 metric names + helpers
  conformance/                MaintenanceSafety property test against any ObjectStore factory
  mtest/                      shared test fixtures (manifest+pack synthesizers, ObjectStore recorder)

cmd/bucketvcs/maintenance.go  CLI subcommand (cobra), mirrors gc.go shape
```

### 3.2 Responsibilities and dependencies

| Package | Reads from | Writes to | Used by |
|---------|-----------|-----------|---------|
| `internal/maintenance` | `internal/repo`, `internal/repo/manifest`, `internal/pack`, `internal/objindex`, `internal/commitgraph`, `internal/storage` | `packs/canonical/<new>.pack`+`.idx`, `indexes/object-map/<new>.bvom`, `indexes/commit-graph/<new>.bvcg`, manifest CAS | `cmd/bucketvcs/maintenance.go`, future serve-integrated scheduler |
| `internal/maintenance/conformance` | maintenance entry point + an `ObjectStore` factory | n/a | wired into `internal/storage/conformance` aggregator and into each adapter test, mirroring `RunPropertyGCSafety` |

### 3.3 Key design choices

- **Stateless entry point.** `maintenance.Run(ctx, store, k, opts) (Report, error)` takes an `ObjectStore` and a `keys.Repo` — the same surface as `gc.Run`. No package globals, no config files. The CLI builds `opts` from flags and calls it.
- **One repo per call.** `--all-repos` loops in the CLI, exactly like `bucketvcs gc --all-repos`. Per-repo failures are isolated.
- **Reuse, do not fork.** Pack production reuses `gitcli.PackObjectsAll` (the importer's pattern from M2 — git CLI does the rev-list + pack-objects work; reachability is implicit in `--all`). Index building stays pure Go via `objindex.Build` and `commitgraph.Build`, opened against the new local pack via the importer's `newLocalFilePackStore` adapter (or an equivalent thin helper if it stays unexported). The novel code in M9 is `materialize.go`, `pipeline.go`, `casmerge.go`, and `thresholds.go`.
- **Pure-Go pack writer is out of scope.** The §40.3 promotion rule reserves pure-Go pack writing for its own milestone after differential-harness shadow time. M9 follows the existing convention: git CLI for pack writes, pure Go for everything else.
- **No new manifest fields.** All output lands in existing slots: `Packs`, `Indexes.ObjectMap`, `Indexes.CommitGraph`. Zero schema-version changes.
- **No required cross-milestone changes.** If profiling during implementation surfaces a needed primitive (e.g. a streaming object-source enumerator on `internal/pack`), it lands as an additive method (no contract change). Anything larger surfaces as a Phase-0 patch step in the plan, reviewed independently — same posture M8 took for `tx.WriteCommitMarker`.

### 3.4 Interaction with M8 GC

M9 produces; M8 reclaims. After a successful M9 CAS-merge:

- Old canonical packs are no longer in `manifest.Packs` → M8 sweep target C (force-push / branch-delete unreachable canonical packs) picks them up after retention.
- Old `.bvcg` and `.bvom` are no longer in `manifest.Indexes` → M8 sweep target D (stale indexes) picks them up.
- No new sweep targets; no changes to `internal/gc`.

The two CLIs are independent and idempotent. Operator runbook orders them as `gc` after `maintenance` in a typical schedule, but neither requires the other.

## 4. Maintenance pipeline

A single `maintenance.Run` call executes seven phases. Phases 4 and 6 are the only ones that touch durable manifest state; phases 1–3 work in a per-run temp directory.

### 4.1 Phase 0 — Load & gate

```
1. Read manifest (header + body, version-checked) → M0, manifestVersion v0
2. Evaluate thresholds against M0 (see §5)
3. If !opts.Force && !triggered: emit "no-op" report, exit 0
4. Snapshot ref tips T0 := M0.Body.Refs (map[ref]commit_oid)
   Snapshot default branch D0 := M0.Body.DefaultBranch
   Snapshot pack set P0 := list of M0.Body.Packs entries (Key + IdxKey)
```

If `T0` is empty (newly-initialized repo with no refs) or `P0` is empty, the run is a no-op regardless of triggers — there is nothing to repack.

### 4.2 Phase 1 — Materialize bare repo

```
 5. Create temp dir <tmp> with os.MkdirTemp("", "bucketvcs-maint-")
 6. mkdir <tmp>/bare.git/objects/pack
 7. For each (PackKey, IdxKey) in P0:
      Get(ctx, PackKey) → stream to <tmp>/bare.git/objects/pack/pack-<oid>.pack
      Get(ctx, IdxKey)  → stream to <tmp>/bare.git/objects/pack/pack-<oid>.idx
    where <oid> is the pack's content hash (already part of the canonical key).
 8. Write <tmp>/bare.git/HEAD with "ref: refs/heads/<D0>\n"
 9. Write <tmp>/bare.git/packed-refs from T0 (one "<oid> <ref>\n" line per entry,
    sorted by ref name for determinism)
10. Write <tmp>/bare.git/config (minimal: [core] repositoryformatversion=0)
11. Run gitcli.Fsck(ctx, <tmp>/bare.git, true) — fail-fast on corruption
```

Memory bound: streaming downloads, no in-memory accumulation. Disk bound: total of `P0` pack sizes plus the soon-to-be-produced consolidated pack (≈ same order of magnitude pre-compression).

If any pack download fails, the run aborts with a wrapped error before any writes; the temp dir is deleted in a deferred cleanup.

### 4.3 Phase 2 — Repack (git CLI)

```
12. prefix := <tmp>/out/pack
    mkdir <tmp>/out
13. packID, err := gitcli.PackObjectsAll(ctx, <tmp>/bare.git, prefix)
    Produces: <tmp>/out/pack-<packID>.pack and <tmp>/out/pack-<packID>.idx
14. Stat the pack file → packSizeBytes
```

`packID` is git's trailing SHA-1 over the pack bytes. As the importer's comments document, repeated repacks of the same reachable set typically yield different `packID`s because delta search is non-deterministic across threads — so PutIfAbsent in Phase 4 normally succeeds.

### 4.4 Phase 3 — Index rebuild (pure Go)

```
15. Open the new local pack: copy `internal/importer/buildcommit.go`'s
    small unexported `newLocalFilePackStore` adapter into
    `internal/maintenance/localpack.go` (≈30 lines, two methods on a
    type that satisfies `storage.ObjectStore` against a single local
    file pair). Then `pack.Open(ctx, store, "p.pack", "p.idx")`.
16. bvomBytes, err := objindex.Build(reader, packID)
    bvomHash := sha256(bvomBytes) → hex
17. tips := buildTipsFromRefs(ctx, reader, T0)
    bvcgBytes, err := commitgraph.Build(ctx, reader, tips)
    bvcgHash := sha256(bvcgBytes) → hex
18. Compute object count: reader.Idx().Count()
```

The `.bvom` covers exactly the new pack. The `.bvcg` is built from `T0`'s commit tips (annotated tags dereferenced via the same loop importer uses, capped at depth 16).

### 4.5 Phase 4 — Upload artifacts

```
19. PutIfAbsent <tmp>/out/pack-<packID>.pack → k.CanonicalPackKey(packID)
20. PutIfAbsent <tmp>/out/pack-<packID>.idx  → k.PackIdxKey(packID, "canonical")
21. PutIfAbsent bvomBytes                    → k.ObjectMapKey(bvomHash)
22. PutIfAbsent bvcgBytes                    → k.CommitGraphKey(bvcgHash)
```

ErrAlreadyExists on .bvom / .bvcg is benign (content-addressed → same bytes already there). ErrAlreadyExists on the pack key indicates a content collision against pre-existing bytes; per importer's analysis, this is either an orphan from a crashed prior run, a replay, or a deterministic-repack collision. The run aborts with a wrapped error rather than risk an offset-mismatch between our local .bvom (built against our specific bytes) and what the canonical key now resolves to.

### 4.6 Phase 5 — Tx record

```
23. txID := "maint-" + ulid.Make()
24. body := tx.Body{Type: "maintenance", Actor: opts.Actor, Extra: { ... see §4.7 }}
25. tx.Write(ctx, store, k.TxRecordKey(txID), tx.Header{...}, body)
    via PutIfAbsent. Tx kind "maintenance" is new; M1's tx record schema
    accepts free-form body fields and M8 GC's tx-orphan classifier reads
    the .commit sibling, not the body.
```

The Extra block carries: `m0_version`, `ref_tip_snapshot` (T0), `repacked_pack_keys` (sorted P0.Key list), `new_pack` (key, hash, size_bytes, object_count), `new_object_map` (key, hash), `new_commit_graph` (key, hash).

### 4.7 Phase 6 — CAS-merge

```
26. Re-read manifest → M_now, version v_now
27. If v_now == v0:
      Build M_new with:
        Packs         = [new_pack_entry]
        Indexes       = { ObjectMap: new_bvom_ref,
                          CommitGraph: new_bvcg_ref }
        Refs          = M_now.Refs        (preserved unchanged)
        DefaultBranch = M_now.DefaultBranch
        Bundles       = M_now.Bundles      (preserved unchanged)
    Else (v_now > v0): a push or another maintenance ran:
      Late_packs := M_now.Packs filtered by PackKey ∉ {p.Key for p in P0}
      Build M_new with:
        Packs         = [new_pack_entry] ++ Late_packs
        Indexes       = { ObjectMap: new_bvom_ref,
                          CommitGraph: new_bvcg_ref }
        Refs          = M_now.Refs
        DefaultBranch = M_now.DefaultBranch
        Bundles       = M_now.Bundles
28. CAS write manifest (If-Match v_now). On success: phase 7.
    On CAS conflict: re-read, retry merge (bounded retries, default 5).
    Bounded retry exhaustion → fail run with non-zero exit; the upload
    artifacts in phases 4–5 remain in the bucket and become tx-orphan +
    canonical-pack-orphan candidates for the next M8 GC run.
```

Two correctness notes on the merge:

1. The new indexes (`new_bvom_ref`, `new_bvcg_ref`) are *correct as accelerators*: they cover exactly the objects in the new pack. They are *incomplete* with respect to `Late_packs` if any. This is fine: §14 indexes are accelerators, not authority — the fetch path falls back to scanning packs when an oid misses the index. This is the same posture as a fresh push that does not touch the indexes (current state today).
2. Refs at CAS time (`M_now.Refs`) are preserved verbatim. We never write ref state in M9; we only write pack and index state. A ref that advanced during the run points at a commit that is either (a) reachable from T0 — already in our new pack — or (b) added by a concurrent push — already in a `Late_packs` member. Either way, `M_new` is reachability-complete.

### 4.8 Phase 7 — Commit marker + audit

```
29. tx.WriteCommitMarker(ctx, store, k.CommitMarkerKey(txID))  (best-effort)
30. Cleanup <tmp>: deferred os.RemoveAll
31. Emit audit log entry: kind=maintenance.completed, with the same
    fields as the tx body plus before/after pack count and manifest
    pack-metadata size.
32. Emit §32 metrics (durations, byte counts, retry counts, outcome).
33. Return Report{...} to the caller.
```

If Phase 7's commit-marker write fails, the run still reports success — the manifest CAS in Phase 6 is the durable commit point. M8's tx-orphan sweep handles missing commit markers identically to push tx records.

The temp directory is deleted via a deferred `os.RemoveAll` at the start of `Run`, so failure at any phase still cleans up.

## 5. §15.3 thresholds

### 5.1 Threshold model

```go
type Thresholds struct {
    RecentPackCount   int   // default 1000  (§15.3 trigger #1)
    TotalPackCount    int   // default 10000 (§15.3 trigger #2)
    ManifestPackBytes int64 // default 8 << 20 = 8 MiB (§15.3 trigger #3)
    // BitmapCoverage  inert in M9; field omitted to avoid implying support
    // LookupLatency   inert in M9; field omitted, ditto
}
```

Defaults are the spec's recommended values. Each is overridable via CLI flag (`--recent-pack-threshold`, `--total-pack-threshold`, `--manifest-pack-bytes-threshold`). Setting any to `0` disables that specific trigger; setting all to `0` makes the run a no-op unless `--force` is set, which is an explicit way to say "I just want the run, skip threshold checks."

### 5.2 Trigger evaluation

```
recentPackCount := number of canonical packs in M0 created within
                   recent-window (default 24h, --recent-window-hours flag).
                   Determined from object-store creation_time, not from
                   manifest data — matches §15.3's intent that "recent"
                   means freshly-pushed.
totalPackCount  := len(M0.Body.Packs)
manifestPackB   := byte size of json.Marshal(M0.Body.Packs)

triggered := recentPackCount > T.RecentPackCount
          || totalPackCount  > T.TotalPackCount
          || manifestPackB   > T.ManifestPackBytes
```

The Report includes the trigger evaluation result regardless of outcome (no-op runs still report which thresholds were checked and how close to the limit). This is the operator's signal for tuning.

## 6. CLI surface

```
bucketvcs maintenance --store=<URL>
   { --repo=<tenant>/<name> | --all-repos }
   [--force]
   [--dry-run]
   [--recent-pack-threshold=N]
   [--total-pack-threshold=N]
   [--manifest-pack-bytes-threshold=N]
   [--recent-window-hours=H]
   [--cas-retry=N]                 (default 5)
   [--output=text|json]            (default text)
   [-v / --verbose]
```

Mirrors `bucketvcs gc` flag conventions exactly. Sub-1-hour `--recent-window-hours` is rejected with exit 2 (same intent as M8's sub-1s retention rejection: defaults exist for a reason).

`--dry-run`:
- Phase 0 + threshold evaluation + reachability walk (Phase 1) run normally.
- Phases 2–6 do not write anything.
- Report includes `would_repack: true/false`, projected new-pack object count, projected manifest-pack-bytes after run.
- Text output prefixes lines with `[DRY RUN]` (mirrors M8's marker convention).
- Exit code 0; never 1.

`--force`:
- Skips threshold evaluation; always proceeds to Phase 1.
- Useful for: post-import warm-up, scheduled weekly run regardless of activity, manual operator intervention after a known event.

### 6.1 Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success (or dry-run completed) |
| 1 | At least one repo failed in `--all-repos` mode, or single repo run failed after CAS-retry exhaustion / object-store error |
| 2 | Invalid flags (mutually exclusive flags both set, sub-1h window, missing required flag, malformed `--repo`) |

### 6.2 Output

Text mode is dense and operator-readable, mirroring `bucketvcs gc`'s style. JSON mode is a single object per repo (or an array of objects in `--all-repos`) with the same fields the audit log emits, enabling direct piping into log aggregation. JSON arrays are normalized to `[]` (not `null`) when empty — same lesson as M8's roborev round-5 finding.

Representative text-mode summary line:

```
acme/web: triggered=recent_pack_count(1247>1000); repack 2.3 GiB → 1.8 GiB;
  pack count 1247 → 1; manifest pack bytes 9.4 MiB → 187 KiB;
  cas attempts 1; duration 47s
```

## 7. Observability

### 7.1 §32 metrics

Names follow the M8 convention (`bucketvcs_<area>_<noun>_<unit>`):

| Metric | Type | Labels |
|--------|------|--------|
| `bucketvcs_maintenance_runs_total` | counter | `outcome` ∈ {success, noop, failed_walk, failed_pack_write, failed_cas, failed_other} |
| `bucketvcs_maintenance_run_duration_seconds` | histogram | `outcome` |
| `bucketvcs_maintenance_objects_packed_total` | counter | — |
| `bucketvcs_maintenance_pack_bytes_in` | counter | — |
| `bucketvcs_maintenance_pack_bytes_out` | counter | — |
| `bucketvcs_maintenance_cas_attempts` | histogram | — |
| `bucketvcs_maintenance_threshold_recent_pack_count` | gauge | post-run snapshot |
| `bucketvcs_maintenance_threshold_total_pack_count` | gauge | post-run snapshot |
| `bucketvcs_maintenance_threshold_manifest_pack_bytes` | gauge | post-run snapshot |

The three threshold gauges are emitted on every run regardless of triggered / no-op outcome — they are the operator's "how close to forced action" signal.

### 7.2 Audit events

Two event kinds, both consistent with M8's `audit=true`-tagged structured-log shape (durable audit-store remains M15's responsibility):

```
maintenance.started   { repo_id, run_id, manifest_version_at_start,
                        ref_tip_count, threshold_eval, dry_run }
maintenance.completed { repo_id, run_id, outcome,
                        before/after metrics,
                        new_pack_key, new_pack_object_count,
                        repacked_pack_keys (sorted),
                        new_object_map_key, new_commit_graph_key,
                        cas_attempts, duration_ms }
```

## 8. Testing strategy

| Tier | Location | What it covers |
|------|----------|----------------|
| Unit | `internal/maintenance/*_test.go` | thresholds.go decision table; casmerge.go body construction (table-driven over before/after body shapes); pipeline.go phase ordering with a fake ObjectStore; materialize.go bare-repo layout invariants (packed-refs format, HEAD content, file presence) |
| Integration | `internal/maintenance/integration_test.go` against localfs | full pipeline on a synthesized small repo (10 commits, 5 refs, 3 packs); roundtrip into a `git fsck`-clean export; differential test that import → maintenance → export is reachability-equivalent to import → export |
| Conformance | `internal/maintenance/conformance/safety.go` exposing `RunPropertyMaintenanceSafety(t, factory)` | the §43.6-style invariant property (below), wired into the cross-adapter aggregator |

### 8.1 The maintenance safety property

> For any sequence of (push, maintenance, gc) operations interleaved against the same repo, the manifest at every CAS-committed step is reachability-complete: every commit referenced by `manifest.Refs` is reachable through `manifest.Packs` at that step.

Four interleavings are exercised explicitly:

1. **maintenance solo** — single run on a steady-state repo; manifest converges to one canonical pack.
2. **push during walk** — push lands between Phase 1 (walk) and Phase 5 (CAS-merge); merge keeps the late pack; reachability holds.
3. **maintenance during gc retention window** — gc's mark sees old packs as unreachable (because maintenance just CAS-merged); sweep skips them until retention; meanwhile a fetch session reading the old manifest at a stable key still works.
4. **two maintenances racing** — one wins CAS, the other re-reads, sees its own work superseded, restarts at Phase 0; neither corrupts the manifest.

Wired into the four canonical adapters (localfs, s3compat, gcs, azureblob) via the same conformance aggregator pattern M8 used.

### 8.2 Differential harness contribution

M9 adds one differential test to the existing `internal/diffharness` suite:

> A repo round-tripped through `import → maintenance → export` is `git fsck`-clean and produces an object inventory identical to the same repo round-tripped through `import → export` (no maintenance step). I.e., M9 is a no-op on observable Git semantics.

This is the protection against pack-writer regressions (delta selection, oid ordering, header encoding).

## 9. Documentation deliverables

- **`docs/m9-maintenance-operator-guide.md`** (mirrors `docs/m8-gc-operator-guide.md`):
  - Scheduling recipes (cron / CronJob / systemd timer) for `maintenance` + `gc` in sequence.
  - Threshold tuning rationale, with worked examples for small, medium, and hot-large repos.
  - The "what changes after a maintenance run" walkthrough (manifest before / after, GC interaction).
  - JSON output schema reference.
  - Failure-mode runbook: CAS exhaustion, walk failure, partial upload (M8 GC reclaims).
- **`docs/m5-cloud-quickstart.md`** and **`docs/m7-cloud-quickstart.md`**: append a one-line note that `bucketvcs maintenance` benefits from the same lifecycle policies already documented for M8 multipart cleanup.
- **`README.md`**: one-line addition to the CLI surface table.

## 10. Acceptance criteria

1. `bucketvcs maintenance --repo` reduces a synthesized 1000-pack repo to one canonical pack with fresh `.bvom` + `.bvcg`, no objects lost, `git fsck`-clean export.
2. `bucketvcs maintenance --all-repos` iterates per-repo with isolated failures and a final summary; exits `1` if any repo failed.
3. `RunPropertyMaintenanceSafety` passes against all four canonical adapters (localfs, s3compat, gcs, azureblob).
4. Differential harness `import → maintenance → export` test passes.
5. The four explicit interleavings (solo / push-during-walk / gc-during-retention / two-maintenances-racing) pass.
6. Operator guide published; quickstarts updated; README updated.
7. §32 metrics emitted on every run; audit events emitted on every run.
8. CAS-merge tested against synthetic version-collision injection.
9. `--dry-run` end-to-end test produces no writes (verified against an `ObjectStore` recorder).

## 11. Followups (each its own brainstorm)

| Item | Why it is a clean unit | Triggering signal |
|------|----------------------|-------------------|
| Bitmap (`.bitmap`) generation ("M9.5") | Pure-Go EWAH writer + reachability bitmap derivation + pseudo-merge encoding is a focused, well-bounded chunk. Plugs into M9's pipeline at Phase 3 and adds the §15.3 bitmap-coverage trigger. | After M9 ships and operators report fetch-CPU pain on large repos. |
| Generated-pack writer + retention | Pairs with the dynamic-pack feature (§16.2). Whoever introduces dynamic packs introduces the retention contract for them. | M11 (bundles) likely brings this with it, or its own milestone. |
| In-serve background scheduler | Wraps `maintenance.Run`; adds per-repo queue, periodic timer, post-push hook. No changes to M9 internals. | When operators want maintenance without external cron. |
| Geometric / tiered repack | Adds a second code path inside `pipeline.go` selectable via flag; full repack stays the default. | When full-repack runtime exceeds maintenance windows on the largest deployments. |
| Object-to-pack lookup-latency trigger (§15.3 trigger #4) | Requires a fetch-path latency-measurement substrate. | When §32 metric framework grows enough to expose request-level histograms. |
| Maintenance leases | Only needed if the deferred multi-writer mode (§26.3) lands. | Beyond OSS v1. |

## 12. Open questions resolved during brainstorming

- **Bitmaps in M9 or split off?** Split into M9.5. The §15.3 trigger ships inert.
- **Trigger model: CLI / in-serve / daemon?** CLI-only, mirroring M8.
- **Repack shape: full / geometric / tiered?** Full repack (one pack out per successful run). Geometric and tiered are explicit followups.
- **Push-during-repack correctness?** CAS-merge keeps concurrent push packs; old packs become unreachable and M8 GC reclaims after retention. No new safety primitives.
- **Pack production: pure-Go writer or git CLI?** Git CLI via `gitcli.PackObjectsAll` against a locally-materialized bare repo. Same pattern as M2's importer. Pure-Go pack writing is its own future milestone (§40.3 promotion rule). Reachability decision is delegated to git's `rev-list --all --objects` inside `PackObjectsAll`.
- **Cross-milestone changes?** None expected as contract changes. `internal/maintenance/localpack.go` is a duplicate-and-shrink of importer's unexported `newLocalFilePackStore` (≈30 lines); if it becomes shared it can be promoted to a small sibling package later, but M9 doesn't need that.
