# M29 — Usage & Activity Log Shipping — progress

Date merged: 2026-06-05
Merge: PR #14, squash commit `2f2bdeb` (`M29: Usage & activity log shipping to the bucket (#14)`)
Follow-up: PR #15 `02fcc52` (observability overview doc + cross-references, 2026-06-06)
Tag: `v0.5.0`
Spec: `docs/superpowers/specs/2026-06-05-usage-activity-log-shipping-design.md`
Plan: `docs/superpowers/plans/2026-06-05-usage-activity-log-shipping.md`

## Summary

M29 gives bucketvcs a durable observability trail: `bucketvcs serve` ships two
gzipped NDJSON streams into the object store under the reserved `sys/logs/`
prefix — **activity** (the `audit=true` event taxonomy, captured by a fanout
slog tap with zero emitter changes) and **usage** (typed operation-metering
records: fetch/push/LFS/bundle/pack serves with tenant, repo, actor, bytes,
duration). Shipping is **on by default** (`--log-shipping=off` to disable),
batched through a crash-safe local spool: rotate at 1000 events OR 15 minutes
since a file's first event, empty files never rotate or ship, leftovers ship on
the next boot, and a bounded spool (256 MB, chronological drop-oldest) keeps a
bucket outage from filling the disk. Delivery is at-least-once
(`PutIfAbsent`; an identical re-ship is a success no-op). Multinode needs no
coordination — per-boot instance IDs in the file names.

The same milestone **normalized the audit taxonomy**: ~40 emit sites across 9
packages (policy, lfs, auth, webhooks, hooks, web, replica, repo-rename) gained
the `audit=true` + `event` convention that earlier milestones had applied
inconsistently — without it, the activity stream would have missed policy
rejections, LFS activity, and auth events entirely.

## Components

- `internal/shiplog/engine.go` — intake channel (non-blocking; full queue or
  closed engine = counted drop), per-stream active spool files, single-writer
  intake goroutine, rotation triggers, single-buffered-write append (a write
  failure abandons the file so a torn line can never corrupt the next record).
- `internal/shiplog/ship.go` — ship loop (5 s tick), gzip→`PutIfAbsent`→delete
  (only after success), boot-time leftover adoption (orphaned actives renamed
  via mtime), chronological pending ordering, bounded-cap eviction, cumulative
  metric emission on change (`shiplog_shipped_files_total`,
  `shiplog_shipped_events_total`, `shiplog_ship_errors_total`,
  `shiplog_dropped_events_total`, `shiplog_dropped_files_total`).
- `internal/shiplog/tap.go` — fanout `slog.Handler`: every record passes
  through to stderr first and unconditionally; `audit=true` records are
  additionally serialized (`{ts, level, event, ...attrs}`, groups nested,
  Time→RFC3339Nano / Duration→ms normalization) into the activity stream.
- `internal/shiplog/usage.go` — `UsageEvent` (schema `v:1`) with kinds
  `fetch` (clone folds in; events are request-level — v2 ls-refs and fetch are
  separate POSTs), `push`, `lfs_upload` (verify-confirmed actual size),
  `lfs_download` (authoritative negotiated sizes, errored objects excluded),
  `bundle_serve`, `pack_serve`.
- Serve wiring — 5 flags (`--log-shipping` default on + env twin,
  `--log-ship-max-events`, `--log-ship-interval`, `--log-spool-dir`,
  `--log-spool-max-bytes`); a concrete stderr `TextHandler` installed as the
  slog default in BOTH modes (tapping the stdlib log↔slog bridge deadlocks);
  shutdown flush ordered before the store closes; engine binds the raw
  operator store (replica gateways ship to their regional bucket).
- Instrumentation — 7 metering sites at the gateway/sshd/LFS layers
  (`internal/gitproto` untouched); shared counting response writer/reader.

## Key fixes from the review trail

- `Enqueue` after `Close` panicked (send on closed channel) → done-channel +
  closed-atomic lifecycle; late events are counted drops.
- gzip `io.Pipe` writer goroutine leaked when a PUT failed without draining
  the body → in-memory buffering (files are bounded by MaxEvents).
- LFS download metering was client-spoofable (claimed sizes, errored objects
  counted) → authoritative `store.Head` sizes, errored objects excluded.
- Tapping `slog.Default()`'s bridge handler deadlocked on the log mutex →
  concrete TextHandler base (console format change documented).
- Lexical pending sort ordered by instance ID, not time → drop-oldest is now
  chronological via the embedded rotation timestamp.

## Testing

Unit suite (rotation/ship/tap/usage incl. regression tests for every fixed
defect), race-clean, 3-phase smoke `scripts/logship-smoke-localfs.sh`
(ship round-trip with real git push/clone, kill -9 leftover shipping,
idle-ships-nothing), M28 interplay verified (`authdb.replica.restored` flows
through the tap into the shipped stream), per-emitter audit-shape tests across
6 packages.

## Review trail

Per-task spec + quality reviews (subagent-driven) + 4 roborev rounds
(~13 findings fixed, 1 accepted-by-design: request-level fetch events).

## Deferred

Querying/UI over shipped logs; shipping CLI-emitted audit events (`gc.*`,
`maintenance.*`, `lfs.gc.*`, `lfs.quota.reconcile`, `repo.renamed` — the tap
lives only in serve); fsync-per-event durability; per-tenant file
partitioning; built-in retention (bucket lifecycle rules are the answer);
Prometheus `/metrics` endpoint; normalizing the metric attribute key
(`metric_name` vs `name` drift across ~21 emitters, documented in the
observability guide).
