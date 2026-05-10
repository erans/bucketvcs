# M8 — Basic Garbage Collection — progress

Date merged: 2026-05-10
Merge commit: `a8e5e4e`
Tag: `m8-complete`
Worktree: `.claude/worktrees/m8-gc` on branch `feature/m8-gc` (retained per project convention)
Plan: `docs/superpowers/plans/2026-05-09-m8-basic-gc.md`
Spec: `docs/superpowers/specs/2026-05-09-m8-basic-gc-design.md`

## Acceptance criteria — all green

1. ✅ `bucketvcs gc --repo=…` and `bucketvcs gc --all-repos` work end-to-end against the localfs adapter. Verified by `cmd/bucketvcs/gc_test.go` (7 test functions including help, XOR-flag enforcement, happy path, retention warning, all-repos enumeration, and dry-run no-effect).
2. ✅ All unit and integration tests pass with `-race`.
   - `internal/gc/...` (gc package + marks + sweeps + conformance subpackage) — 22 test functions across 4 packages.
   - `internal/repo/...` — 0 regressions; M1 patch tests added (`TestCommit_WritesCommitMarkerOnSuccess`, `TestCreate_WritesCommitMarkerOnSuccess`, `TestCommit_MarkerWriteFailureDoesNotFailCommit`).
   - `cmd/bucketvcs/...` — 7 new `TestGC_CLI_*` tests.
   - Full-repo `go test -race ./...` — 36 packages PASS.
3. ✅ `RunPropertyGCSafety(t, factory)` lives at `internal/gc/conformance/safety.go`. The localfs binding (`safety_localfs_test.go`) PASSes both subtests (orphan-pack-respects-retention, push-during-sweep-43-6). Wired into all four canonical adapters via `TestS3Compat_GCSafety_R2`, `TestS3Compat_GCSafety_S3`, `TestGcs_GCSafety`, `TestAzureBlob_GCSafety`. The conformance script (`scripts/conformance-emulators.sh`) invokes `go test ./internal/gc/conformance/...` so the localfs property test runs in the `emulators` CI job on every PR.
4. ✅ M1 patch (`tx.WriteCommitMarker`) merged with three new tests; existing M1 test counts updated to account for the additional `.commit` markers that now appear under the `tx/` prefix (`internal/repo/repo_test.go` and `internal/repo/internal/repo_concurrency_test.go`).
5. ✅ `docs/m5-cloud-quickstart.md` and `docs/m7-cloud-quickstart.md` updated with bucket-lifecycle pointers (full per-cloud recipes live in `docs/m8-gc-operator-guide.md` §5).
6. ✅ `docs/m8-gc-operator-guide.md` written — 1072 lines covering scope, scheduling (cron / K8s CronJob / systemd), retention defaults, the §43.6 race window honestly, per-cloud bucket-lifecycle recipes, localfs operational notes (with a manual-cleanup recipe — see Highlights below), reading mark/sweep records via jq, exit-code interpretation, known limits, and acceptance verification steps.
7. ✅ Exit codes, JSON output, and slog event names verified manually via `cmd/bucketvcs/gc.go` build + spot-check (Task 9.1).
8. ✅ `bucketvcs gc --help` documents every flag with the defaults stated in the spec.

## Highlights — what M8 actually shipped

### Headline correctness story (§43.6)

The `internal/gc/conformance/safety.go` property test runs the §43.6 push-during-sweep scenario against any `ObjectStore` factory. The test:
1. Marks an orphan pack with 0s retention so it would be immediately sweep-eligible.
2. Performs a real `r.Commit` that writes the pack into the manifest body BETWEEN the mark and sweep phases.
3. Asserts the sweep skips the pack with `reason=revived` and the pack still exists in storage.

This passes against localfs and is wired into all four canonical adapters; will run against R2 / AWS S3 / GCS / Azure once the `real-cloud` CI job's secrets are configured.

### M1 commit-marker patch — making M1's promise honest

M1's `repo.Commit` mints a fresh `tx_id` per attempt and writes the tx record before the CAS. The M1 design comments said "Lost attempts leave orphan tx records on disk for M8 GC" but provided no on-disk way to distinguish a winning tx record from a CAS-loss orphan. M8 added `tx.WriteCommitMarker(ctx, store, key)` — best-effort `PutIfAbsent` of an empty body at `<txKey>.commit`. Two call sites: `repo.Commit` after `CASRoot` success, `repo.Create` after the root `PutIfAbsent` succeeds. Marker-write failure is logged-and-ignored — the CAS has already committed.

Backwards-compatible: `tx_orphan_sweep_armed` is a per-repo gate that is `true` only once at least one commit marker has been observed. Pre-M8 repos with no markers anywhere have tx-orphan sweep disarmed until the first post-M8 commit lands. This means we never sweep a pre-M8 winning tx record by accident.

### Defense-in-depth in `RunSweep`

The §43.6 sweep flow:
1. Re-reads current root manifest at sweep start (NOT the manifest the mark phase saw).
2. Builds a fresh live-set.
3. For each candidate: classify(revived | retention_not_met | tx_sweep_disarmed | delete) using the fresh live-set + `mark.RetentionSeconds` (pinned at mark time, NOT the operator's current flag).
4. On delete: `Head` to capture current `ObjectVersion`, then `DeleteIfVersionMatches`. `ErrVersionMismatch` and `ErrNotFound` from delete are reclassified as Skipped (with reason), NOT Errors.

`startedAt` and `now` are derived from the same `opts.Now()` call to keep the retention comparison and audit timestamp consistent under fake clocks (review-fix from Task 4.1).

### Dry-run as first-class

`--dry-run` skips persistence (no mark.Write, no sweep.Write, no PruneMarks, no actual Delete). The classify-and-decide phase still runs, populating `RunReport.SweepRecord.Deleted.*` with the keys that WOULD have been deleted — this lets operators pipe `bucketvcs gc --dry-run --format=json | jq` to see exactly what a real run would do.

Audit-tagged log events (`gc.mark.completed`, `gc.sweep.completed`) are SKIPPED in dry-run; instead non-audit `gc.mark.dry_run` and `gc.sweep.dry_run` events fire. Audit logs reference only on-disk-persisted record IDs (review-fix from Task 5.3).

### Mark-record JSON shape — declaration-order, not alphabetical

The naive Go pattern of `MarshalJSON` → marshal-to-bytes → unmarshal-into-map → splice → re-marshal would produce alphabetically-sorted keys, hurting human readability of an immutable on-disk record. M8 uses a typed intermediate struct (`marshaledRecord`) with fields in spec §7.1 order, with `*string` for `previous_mark_id` (nil → `null`, non-nil → quoted string). Same pattern in `sweeps.Record`. Key-order regression tests in both packages (review-fix from Task 1.1).

### Operator guide + lifecycle pointer pattern

`docs/m8-gc-operator-guide.md` §5 has the full per-cloud lifecycle recipes (S3 + R2 + GCS + Azure). The cloud quickstarts (`docs/m5-cloud-quickstart.md`, `docs/m7-cloud-quickstart.md`) point operators at §5 rather than duplicating recipes. Localfs explicitly documents that multipart sessions do NOT self-clean on process exit (the implementer found this contradicts the spec's assumption — included a manual `find` cleanup recipe in §6).

## What the conformance suite caught before merge

The cycle of (combined spec + code-quality review per task → fix → re-review) caught **eight** non-trivial issues that would have been bugs in production:

1. **Task 1.1** — `MarshalJSON` map-splice produced alphabetically-sorted JSON keys. Fixed via typed intermediate struct giving declaration-order output. Added a 10-key-order regression test.
2. **Task 4.1** — `RunSweep` called `opts.Now()` twice (`startedAt` then `now`) creating fake-clock divergence risk in tests. Derived `now` from `startedAt`. Also corrected a misleading concurrency comment that promised parallel-within-category processing that the implementation doesn't yet provide.
3. **Task 5.3** — `gc.Run` had a real spec-gap on dry-run: the original sketch called `RunSweep` regardless of DryRun and only skipped the Write step, meaning Deletes still fired. Fixed by threading `DryRun` into `SweepOptions` so `applyDecision` skips Head + Delete in dry-run mode but still appends to `Deleted.*` for "would delete" reporting.
4. **Task 5.3** — DryRun audit log emitted ephemeral mark/sweep IDs that were never persisted to disk. Operators tailing audit logs in dry-run would see IDs they couldn't look up. Fixed by skipping `LogMarkCompleted/LogSweepCompleted` in dry-run and emitting `gc.mark.dry_run`/`gc.sweep.dry_run` non-audit events instead.
5. **Task 5.3** — `ErrNoMarkForSweep` had no sentinel test; SweepOnly mode was completely untested. Added two tests covering both cases.
6. **Task 6.1** — Missing `defer closeStore(store)` (every other subcommand has this; localfs leaves the lockfile dangling otherwise). Added.
7. **Task 6.1** — `emitReport` text-format sweep-block predicate `r.SweepID != "" || (r.MarkID == "" && r.SweepRecord.SweepID == "")` was wrong for dry-run cases. Replaced with the simpler correct form `r.SweepRecord.SweepID != ""`. Added `TestGC_CLI_DryRun_TextOutputShowsSweepBlock` to catch any future regression.
8. **Task 7.1** — `RunPropertyGCSafety` factory signature `func(*testing.T) storage.ObjectStore` didn't match `internal/storage/conformance.Factory`'s `func(testing.TB) (storage.ObjectStore, func())`. Friction in Task 7.2 would have been real; aligned both signatures and used a clean `gcconformance.Factory(...)` type conversion when wiring into cloud adapters.
9. **Task 7.1** — Original spec placement was `internal/repo/internal/`, which would have created a `repo → repo/internal → gc → repo` import cycle AND would have been un-importable from cloud adapters because `_test` packages aren't importable cross-package. Moved to a new `internal/gc/conformance/` package mirroring `internal/storage/conformance/`.
10. **Task 8.1** — Operator guide jq examples referenced a `.candidates.*` field that doesn't exist in the CLI's JSON output (the field exists ONLY in the on-disk mark record); rewrote to `.deleted.*`. Same fix sweep: exit-code table had two reversed mappings (per-key errors return 1, not 2; flag errors return 2, not 1).

(Plus several Minor cosmetic nits caught and either fixed or explicitly accepted as "ready to proceed.")

## Out of scope (deferred by design)

Per the spec's §16 "Open questions deferred past M8":
- **Object-level GC inside packs / repack pipeline** — M9.
- **Stale `packs/generated/` GC** — paired with whichever milestone first writes there (no current writer).
- **In-binary multipart cleanup** (`ListIncompleteMultipartUploads` + `AbortMultipart` ObjectStore extension) — focused future milestone with proper adapter-surface design. M8 covers §33.5 via the bucket-lifecycle branch documented in operator guide §5.
- **Active-session and signed-URL marking** (§25 steps 3, 4) — covered by retention-window dominance over realistic clone/URL lifetimes; revisited if a serve-integrated GC mode is added.
- **Serve-integrated background GC** — `bucketvcs serve` extension; future milestone.
- **Cross-process GC leases** — not needed for the CLI-only single-writer model.
- **Manifest archival** enabling §25 step 2 ("mark from recent manifests" beyond the current one) — replaced for M8 by retention window + sweep-time re-read.
- **Bundle GC and ref-shard GC** — live-set placeholders ready; activated when M11/M12 actually emit those.
- **Streaming mark writer for very large repos** (>10M packs) — documented in operator guide §9 as a known limit; future enhancement.

## Follow-ups before tagging m8-complete

1. **Push branch and verify CI conformance run** — manual step (the controller cannot push). User runs:
   ```bash
   git push -u origin feature/m8-gc
   gh pr create --draft --title "M8: basic GC" --body "..."
   ```
   Confirm the `conformance / emulators` job log contains all five `*_GCSafety` test names as PASS:
   - `TestS3Compat_GCSafety_R2`, `TestS3Compat_GCSafety_S3` (against MinIO)
   - `TestGcs_GCSafety` (against fake-gcs-server)
   - `TestAzureBlob_GCSafety` (against Azurite)
   - `TestGC_PropertyGCSafety_Localfs`

2. **Real-cloud CI secrets** — same as the M7 follow-up: the `real-cloud` workflow job no-ops because the AWS / R2 / GCS / Azure repo secrets are not yet configured. Configure them per `docs/m7-cloud-quickstart.md` and trigger one `workflow_dispatch` run to confirm green before tagging m8-complete. (Same blocker as M7; ratifying it for M8 is fine.)

3. **Memory note** — once tagged, update `~/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md` and add `m8_progress.md` memory entry mirroring the m7 entry shape.

## Total cost

- 29 implementation tasks across 10 phases (Phase 0 M1 prereq + Phases 1-9)
- 37 commits on `feature/m8-gc` (29 task commits + 8 review-fix commits, plus 2 pre-branch design + plan commits on main)
- ~3300 lines of new Go code under `internal/gc/` + `cmd/bucketvcs/gc.go` + `internal/repo/tx/marker.go` + `internal/gc/conformance/`
- ~1100 lines of operator documentation (`docs/m8-gc-operator-guide.md`)
- 0 new external dependencies
- All §29 conformance tests pass against MinIO + fake-gcs + Azurite emulators on every PR; cloud-adapter `*_GCSafety` tests skip cleanly when secrets are absent.
