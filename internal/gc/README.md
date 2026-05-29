# `internal/gc`

Garbage collection for bucketvcs repositories: a two-phase mark/sweep protocol
that reclaims storage from orphaned objects without endangering in-flight writes.

## Purpose

`internal/gc` implements the §25 / §43.6 / §33.1 mark/sweep protocol for
repository-level GC. A mark phase snapshots the live set — all objects reachable
from the current manifest plus a configurable retention window — and writes an
immutable mark record to the bucket. A sweep phase reads that mark record,
computes the complement against the current bucket listing, and deletes objects
outside the retention window. Both records are immutable and keyed by ULID, so a
crash at any point leaves the bucket in a consistent state that a subsequent GC
run can continue from or supersede.

The protocol targets four categories of orphaned objects:

1. **Orphan tx records** — Transaction records left by CAS-race losers in
   `repo.Commit`. Each commit writes a tx record before racing to swap in the
   new manifest; only one racer wins per round, leaving losing tx records in
   `tx/` indefinitely. GC sweeps them after the retention window.

2. **Orphan canonical packs** — Pack files (`.pack`, `.idx`, optional
   `.bitmap`) uploaded to the canonical packs prefix but never committed into
   a manifest entry, typically because the importer crashed between upload and
   manifest CAS.

3. **Unreachable canonical packs** — Packs that were once referenced in the
   manifest but are no longer reachable from any commit in the history window.
   History depth is bounded by `MarkOpts.HistoryDepth`.

4. **Stale indexes** — Reachability-index blobs whose corresponding pack no
   longer exists in the live set after sweep.

## Pointer to spec

`docs/superpowers/specs/2026-05-09-m8-basic-gc-design.md`

## What this package does NOT own

- **Object-level GC inside packs / repack pipeline** — M9.
- **Stale `packs/generated/` GC** — paired with whoever first writes there.
- **In-binary multipart cleanup** — bucket lifecycle (operator guide §5) or a
  future `ObjectStore` extension milestone.
- **Active-session and signed-URL marking** — covered by retention-window
  dominance; an upload that started within the retention window will not be
  swept even if it is not yet in the manifest.
- **Cross-process GC leases** — not needed for the CLI-only model; the operator
  schedules GC externally and is responsible for serialization.
- **Manifest archival** — relies on the retention window for now; old root
  manifests are not explicitly archived.
- **Bundle GC and ref-shard GC** — live-set placeholders are ready in
  `liveset.go`; they will be activated when M11 (bundles) and M12 (ref shards)
  emit the corresponding bucket keys.

## Conformance bar

`RunPropertyGCSafety(t, factory)` lives in `internal/gc/conformance/`, parameterized
over a `storage.ObjectStore` factory. Cloud adapters at M5/M7 plug in via the
factory pattern, mirroring `internal/storage/conformance.Run`. Two property tests
are required:

1. **`§25#orphan-pack-respects-retention`** — a pack uploaded within the
   retention window is never swept, even if it is absent from the manifest.
2. **`§43.6#push-during-sweep-doesn't-delete-revived`** — a pack that was in
   the mark's dead set but is re-committed into the manifest between mark and
   sweep is not deleted by the sweep.

## Public API surface

Top-level orchestration:

- `gc.Run(ctx, s, r, opts) (RunReport, error)` — runs a full mark+sweep cycle
  for a single repo. Returns a `RunReport` summarising counts and sizes swept.
- `gc.RunMark(ctx, s, r, opts) (marks.Record, error)` — mark phase only.
  Writes an immutable mark record and returns it.
- `gc.RunSweep(ctx, s, r, mark, opts) (sweeps.Record, error)` — sweep phase
  only. Requires a completed mark record; writes an immutable sweep record.

Multi-repo:

- `gc.DiscoverRepos(ctx, s) ([]RepoRef, error)` — enumerates all repos in the
  bucket, used by `bucketvcs gc --all-repos`.

Record persistence (`internal/gc/marks`, `internal/gc/sweeps`):

- `marks.Write`, `marks.ReadLatest`, `marks.ReadByID`, `marks.List`
- `sweeps.Write`

Lifecycle:

- `gc.PruneMarks(ctx, s, r, opts) error` — deletes all but the last N mark
  records for a repo, keeping storage used by GC metadata bounded.

Audit logging:

- `gc.LogMarkCompleted(ctx, rec)` — emits a structured log line tagged
  `audit=gc.mark.completed` with mark ID, counts, and elapsed time.
- `gc.LogSweepCompleted(ctx, rec)` — emits a structured log line tagged
  `audit=gc.sweep.completed` with sweep ID, bytes reclaimed, and elapsed time.

Sentinel errors:

- `gc.ErrInvalidPhaseCombo` — returned when the caller requests an impossible
  mark/sweep combination (e.g., sweep-only with no prior mark).
- `gc.ErrNoMarkForSweep` — returned when `RunSweep` cannot find the mark record
  referenced by the caller.
- `marks.ErrNotFound` — returned by `marks.ReadByID` / `marks.ReadLatest` when
  no mark record exists.

## CLI

`cmd/bucketvcs/gc.go` is the only entry point exposed to operators. It is a
one-shot command intended to be run under an external scheduler (cron, cloud
scheduler, CI). See [`docs/operator-guides/gc.md`](../../docs/operator-guides/gc.md)
for scheduling, tuning, audit-log interpretation, and exit-code alerting.
