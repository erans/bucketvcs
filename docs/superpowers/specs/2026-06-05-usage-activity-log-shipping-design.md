# Usage & activity log shipping to the bucket — design

**Date:** 2026-06-05
**Status:** Approved (brainstorm complete, pending implementation plan)

## Problem

All bucketvcs logging is `log/slog` to stderr. Audit events (slog lines tagged
`audit=true`, ~30 event types) and metrics (`metric_name` lines) vanish unless
the operator's environment captures stderr. There is no durable audit trail and
no billing-grade usage data anywhere.

Goal: ship two durable NDJSON streams into the object store under the reserved
`sys/` prefix — **activity** (who did what: the existing audit taxonomy) and
**usage** (operation metering: clones/fetches/pushes/LFS transfers with
tenant/repo/actor/bytes/duration) — batched through a local spool with
crash-safe handoff.

## Decision summary

| Decision | Choice |
|---|---|
| Streams | `activity` = existing audit events (captured via slog tap); `usage` = new typed metering records (explicit API at ~6 gateway sites) |
| Format | NDJSON (JSON Lines), gzipped on ship |
| Activation | **On by default** when `--store` is configured; `--log-shipping=off` to disable |
| Rotation | active spool file rotates at **1000 events OR 15 minutes** since first event (configurable); empty file = no rotation, no ship |
| Delivery | at-least-once: local file deleted only after successful PUT; leftovers shipped on next boot |
| Ship failure | retry with backoff; spool dir capped (default 256 MB), oldest pending file dropped at cap with ERROR + metric |
| Capture approach | Hybrid (Approach C): slog tap for activity (zero audit-site changes), explicit typed API for usage |
| Multinode | no coordination — per-boot random instance ID in filenames; every gateway ships its own files |

## Architecture

New package `internal/shiplog`, three units:

### 1. Engine

One per serve process; owns the spool directory, two `Stream`s (`activity`,
`usage`), and the ship loop (one goroutine, ~5 s tick).

- Each stream appends NDJSON lines to an **active spool file**
  `<spool-dir>/<stream>-<instanceID>-<seq>.ndjson`.
- Events arrive over a buffered channel (~4096). **The request path never
  blocks**: channel full → drop + `shiplog_dropped_events_total` + WARN.
  Fail-open everywhere (same posture as webhooks/replication).
- Rotation: close active file → pending queue → fresh active file (seq+1).
- Ship loop: gzip pending file → `PutIfAbsent` → delete local file on success
  only. Failed PUT → file stays pending, backoff retry on later ticks.
- Spool cap (default 256 MB): at cap, drop the OLDEST pending file with ERROR
  log + `shiplog_dropped_files_total`. A bucket outage degrades the trail but
  never fills the disk or blocks serving.

### 2. slog tap (activity feed)

A fanout `slog.Handler` installed around the default handler at serve boot.
Every record passes through to stderr unchanged; records with `audit=true` are
additionally serialized as `{ts, level, event, ...attrs}` into the activity
stream. Zero changes to existing audit call sites; stderr behavior unchanged
by construction. The shipper's own logs are NOT routed back into itself (no
self-feeding loop).

### 3. Usage API (metering feed)

```go
shiplog.Usage(ctx, shiplog.UsageEvent{
    Kind:       shiplog.KindFetch, // clone|fetch|push|lfs_upload|lfs_download|bundle_serve|pack_serve
    Tenant:     "...", Repo: "...", Actor: "...",
    Transport:  "https", // https|ssh
    Bytes:      n, DurationMS: ms, Status: "ok",
})
```

Called from the sites that know the facts: upload-pack completion,
receive-pack completion, LFS batch transfer/verify, bundle-URI serve,
pack-URI serve. This is NEW instrumentation — no existing log line carries
bytes/duration metering.

## Bucket key layout

Time-partitioned under the reserved `sys/` prefix (GC provably never lists
outside `tenants/`; lifecycle-rule friendly; analytics-scannable):

```
sys/logs/activity/<YYYY>/<MM>/<DD>/<HHMMSS>-<instance8>-<seq6>.ndjson.gz
sys/logs/usage/<YYYY>/<MM>/<DD>/<HHMMSS>-<instance8>-<seq6>.ndjson.gz
```

- `instance8` = 8-hex chars of a per-boot crypto/rand instance ID → multinode
  gateways never collide; readers merge by timestamp.
- Written with `PutIfAbsent`; on (theoretical) collision, bump seq and retry.
- BYOB: logs always target the **system** store, never tenant buckets.

## Lifecycle

**Boot:** scan the spool dir BEFORE starting intake; queue and ship any
leftover `*.ndjson` files (from kill -9, OOM, or a shutdown that ran out of
time). Net: the only data a hard crash can lose is the OS-buffer tail of the
active file (no fsync-per-event — explicit non-goal; this is an audit trail,
not a WAL).

**Steady state:** append → rotate at 1000 events / 15 min → ship → delete.
Empty active file after 15 idle minutes: do nothing.

**Graceful shutdown** (inside serve teardown, BEFORE the ObjectStore closes):
stop intake → drain channel to active files → rotate non-empty actives → ship
all pending, bounded by `--shutdown-timeout`. Anything unshipped stays in the
spool for next boot.

**Operator rule:** one spool dir per serve instance. Boot-time leftover
shipping ships ANY pending file found (deliberate — a dead instance's files
must not strand), so two live instances sharing a spool dir is the one
misconfiguration that could double-ship.

## Record schemas

```json
{"ts":"2026-06-05T21:30:45.123Z","level":"INFO","event":"policy.ref.rejected","tenant":"acme","repo":"app","refname":"refs/heads/main","reason":"non_fast_forward"}
```
Activity: the audit slog record, attrs passed through faithfully — the schema
IS the existing audit taxonomy.

```json
{"v":1,"ts":"2026-06-05T21:30:45.123Z","kind":"fetch","tenant":"acme","repo":"app","actor":"alice","transport":"https","bytes":48211904,"duration_ms":2113,"status":"ok"}
```
Usage: fixed schema with `v:1` version field for safe consumer evolution.

## Config surface

| Flag | Default | Notes |
|---|---|---|
| `--log-shipping` | `on` | `off` restores today's stderr-only behavior; `on` requires `--store` |
| `--log-ship-max-events` | `1000` | rotation event threshold |
| `--log-ship-interval` | `15m` | rotation age threshold (since first event in file) |
| `--log-spool-dir` | state dir (beside authdb) | one per instance |
| `--log-spool-max-bytes` | `256MB` | pending-file cap; drop-oldest at cap |

Env-var twins per existing convention (`BUCKETVCS_LOG_SHIPPING`, etc.).

## Observability

slog metric lines (not self-fed): `shiplog_shipped_files_total`,
`shiplog_shipped_events_total`, `shiplog_ship_errors_total`,
`shiplog_dropped_events_total` (channel full), `shiplog_dropped_files_total`
(spool cap). No audit events emitted BY the shipper (avoids the
shipper-audits-its-own-shipping loop).

## Testing

1. **Unit** — rotation triggers (count / age / both / empty-no-op), key
   formatting, bounded-spool eviction order, tap routes only `audit=true`,
   channel-full drop accounting.
2. **Integration (localfs)** — 2,500 events → 2 shipped files + 1 active per
   stream; engine killed with pending files → fresh engine ships leftovers
   exactly once; fault-injecting store → retry-then-success, no delete before
   successful PUT.
3. **Smoke phase** — serve with defaults: push + clone + LFS round-trip,
   SIGTERM, then gunzip + jq assertions on `sys/logs/activity/` and
   `sys/logs/usage/`; idle serve ships nothing.
4. **Conformance** — shipping uses plain `PutIfAbsent` (already covered per
   backend); no new backend matrix.

## Out of scope (deferred)

- Querying/UI over shipped logs; downstream analytics tooling.
- Per-tenant file partitioning (tenant is a field in every record).
- fsync-per-event durability.
- Shipping the general (non-audit) log stream.
- Built-in retention — operators use bucket lifecycle rules on `sys/logs/`
  (same guidance pattern as `sys/authdb/`).
- Prometheus `/metrics` endpoint (separate observability discussion).

## Risks

- **On-by-default** changes upgrade behavior (new writes appear under
  `sys/logs/`): needs a prominent upgrade note (v0.4.0 set the precedent for
  documenting `sys/` expectations).
- **Schema coupling** of activity records to slog attr shapes: accepted —
  attrs ARE the audit contract; migration path exists (audit sites can move
  to an explicit API later without touching the engine).
- **Duplicate files** possible under at-least-once (crash between PUT and
  local delete): consumers must tolerate re-reading an identical file; keys
  are deterministic per file so duplicates are identical objects, and
  `PutIfAbsent` makes the re-ship a no-op against the same key.
