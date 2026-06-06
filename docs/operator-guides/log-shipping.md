# Usage & activity log shipping (operator guide)

This guide covers **log shipping**: `bucketvcs serve` continuously ships two
durable NDJSON streams into the object store under the reserved `sys/logs/`
prefix — **activity** (the audit trail: who did what) and **usage** (operation
metering: bytes and durations for fetches, pushes, LFS transfers, and bundle /
pack serves). Both streams are batched through a crash-safe local spool and
gzipped on upload.

Shipping is **on by default** whenever `--store` is configured. Pass
`--log-shipping=off` to restore the previous stderr-only behavior.

The companion design document is
`docs/superpowers/specs/2026-06-05-usage-activity-log-shipping-design.md`.

Production readiness summary:

- Activity stream (audit events captured via a `log/slog` tap) — **shipped**.
- Usage stream (typed metering records at the gateway / LFS / proxied sites) — **shipped**.
- Crash-safe spool with boot-time leftover shipping — **shipped**.
- Bounded spool (drop-oldest at cap with ERROR + metric) — **shipped**.
- Per-instance file naming for multinode (no coordination) — **shipped**.
- Querying / UI over shipped logs, downstream analytics tooling — **deferred**
  (the layout is analytics-scannable; bring your own query engine).

---

## 1. What it does

When enabled, each `bucketvcs serve` process runs an in-process **shiplog
engine**:

1. A fanout `slog.Handler` taps the default logger: every record passes through
   to stderr unchanged, and records tagged `audit=true` are *additionally*
   serialized into the **activity** stream.
2. The gateway, LFS, and proxied-URL handlers call a typed `UsageEvent` API at
   the points that know the byte counts and durations, feeding the **usage**
   stream.
3. Each stream appends NDJSON lines to a local **active spool file**. The file
   **rotates** after `--log-ship-max-events` events *or* `--log-ship-interval`
   since its first event, whichever comes first.
4. A ship loop (one goroutine, ~5 s tick) gzips each rotated file and
   `PutIfAbsent`s it into the bucket, deleting the local copy only after a
   successful upload.
5. On graceful shutdown, intake stops, non-empty active files rotate, and
   everything pending ships (bounded by `--shutdown-timeout`) **before** the
   object store closes. Anything still unshipped stays in the spool for the
   next boot.

Shipping is **fail-open**: the request path never blocks on logging. A full
intake queue drops the event (counted); a store outage leaves files in the
bounded spool and retries on the next tick. Neither affects serving.

### 1.1 The two streams

**Activity** — the existing audit taxonomy. The activity stream contains
*exactly* the slog records that the codebase tags `audit=true`; it is not an
exhaustive copy of every log line. Examples of events that appear there include
`bundle.uri.advertised`, `proxied.url.served`, and the `authdb.replica.*`
lifecycle events. Each record is serialized as `{ts, level, event, ...attrs}`
with the audit attributes passed through faithfully:

```json
{"ts":"2026-06-05T21:30:45.123Z","level":"INFO","event":"proxied.url.served","kind":"bundle","tenant":"acme","repo":"app","bytes_served":386,"status_code":200,"range_request":false}
```

> **Note.** Some structured log lines that read like audit events on the
> console (for example `policy.ref.rejected` and `auth.scope.denied`) are
> emitted *without* the `audit=true` tag and therefore do **not** appear in the
> shipped activity stream. The activity stream is the `audit=true`-tagged
> taxonomy, nothing more and nothing less — treat the console log as the
> superset.

**Usage** — a fixed, versioned metering schema (`v:1`) emitted by new
instrumentation. No pre-existing log line carried bytes/duration metering:

```json
{"v":1,"ts":"2026-06-05T21:30:45.123Z","kind":"fetch","tenant":"acme","repo":"app","actor":"alice","transport":"https","bytes":48211904,"duration_ms":2113,"status":"ok"}
```

The usage `kind` is one of: `fetch`, `push`, `lfs_upload`, `lfs_download`,
`bundle_serve`, `pack_serve`. A few semantics worth knowing:

- **`fetch` is request-level, not clone-level.** Protocol-v2 `ls-refs` and
  `fetch` are separate HTTP POSTs, and each emits its own `fetch` usage record.
  A single `git clone` therefore typically produces **two or more** `fetch`
  records. There is no `clone` kind — clone and fetch are indistinguishable at
  the transport layer, so both meter as `fetch`. Aggregate by `(tenant, repo,
  actor)` over a time window rather than counting records as operations.
- **`push` status reflects the transport outcome, not per-ref policy
  verdicts.** A push that the gateway accepted at the transport layer meters as
  `push` with `status:"ok"` even if one or more refs were rejected by a
  protected-ref / protected-path rule — the bytes were still received over the
  wire, and a single push can carry a mix of accepted and rejected refs. Use
  the activity stream (and webhooks) for per-ref policy decisions; use the usage
  stream for bandwidth.
- **`actor` is `"anonymous"` on token-authed paths that carry no user
  identity.** LFS verify and the proxied bundle/pack serves authenticate with a
  short-lived HMAC token bound to `(tenant, repo, oid|hash)` rather than a user
  credential, so their usage records carry `actor:"anonymous"` by design.

---

## 2. Enabling and disabling

Shipping is on by default. The flags:

| Flag | Default | Notes |
|---|---|---|
| `--log-shipping` | `on` | `off` restores stderr-only behavior. `on` requires `--store`. Env: `BUCKETVCS_LOG_SHIPPING` |
| `--log-ship-max-events` | `1000` | Rotate + ship a spool file after this many events |
| `--log-ship-interval` | `15m` | Rotate + ship a *non-empty* spool file this long after its first event |
| `--log-spool-dir` | state dir | Local spool directory for unshipped logs; **one per instance** (see §5). Env: `BUCKETVCS_LOG_SPOOL_DIR` |
| `--log-spool-max-bytes` | `256MB` | Cap on unshipped spool bytes; oldest pending file is dropped at the cap |

`--log-spool-dir` resolves like the authdb path: the flag wins, then
`BUCKETVCS_LOG_SPOOL_DIR`, then `$XDG_STATE_HOME/bucketvcs/log-spool`, then
`$HOME/.local/state/bucketvcs/log-spool`.

```bash
# Default: ships to sys/logs/ of --store.
bucketvcs serve --store s3://operator-bucket/repos --auth-db /var/lib/bucketvcs/auth.db --addr :8080

# Disable entirely.
bucketvcs serve --store s3://operator-bucket/repos --auth-db /var/lib/bucketvcs/auth.db --addr :8080 --log-shipping=off
```

---

## 3. The reserved `sys/` prefix and key layout

Shipped logs are time-partitioned under the reserved `sys/logs/` prefix:

```
sys/logs/activity/<YYYY>/<MM>/<DD>/<HHMMSS>-<instance8>-<seq6>.ndjson.gz
sys/logs/usage/<YYYY>/<MM>/<DD>/<HHMMSS>-<instance8>-<seq6>.ndjson.gz
```

- `<instance8>` is 8 hex chars of a per-boot `crypto/rand` instance ID, so
  multinode gateways never collide on a key; readers merge by timestamp.
- `<seq6>` is the per-stream rotation sequence within one boot.

`sys/` is **reserved for system data**. Repository data and garbage collection
operate entirely under `tenants/` and never read, write, list, or sweep
anything under `sys/` — so shipped logs can never be mistaken for unreachable
repo data and swept by GC. Keep your own objects out of `sys/`; it is
bucketvcs's namespace.

On localfs the files appear at `<root>/objects/sys/logs/...` (the `objects/`
interposition is the expected localfs internal, not corruption).

BYOB note: logs always target the **system** store (`--store`), never tenant
buckets.

### 3.1 Retention

There is no built-in retention. Use a bucket **object-lifecycle rule** scoped
to the `sys/logs/` prefix to expire old log objects, exactly as recommended for
`sys/authdb/ltx/` in the [replication guide](authdb-replication.md). A
retention comfortably longer than however far back you need to query usage /
audit data is appropriate. The activity and usage streams have very different
volumes; if your provider supports prefix-scoped rules you can set separate
retentions for `sys/logs/activity/` and `sys/logs/usage/`.

---

## 4. Delivery semantics

- **At-least-once.** A local spool file is deleted only after a successful PUT.
  A crash *between* the PUT and the local delete re-ships the same file on the
  next boot. Keys are deterministic per file, so the re-ship targets the same
  key; `PutIfAbsent` makes it a no-op (an `ErrAlreadyExists` is treated as
  success). Consumers must therefore tolerate re-reading an **identical**
  object — duplicates are byte-for-byte the same file under the same key.
- **What a hard crash can lose.** There is no fsync-per-event (this is an audit
  trail, not a write-ahead log). A `kill -9` / OOM / power loss can lose only
  the OS-buffer tail of the *active* (not-yet-rotated) spool file. Everything
  already rotated to a pending file survives on disk and ships on the next boot.
- **Bounded spool.** If the store is unreachable, pending files accumulate up
  to `--log-spool-max-bytes` (default 256 MB). At the cap the engine drops the
  **oldest** pending file, logs an ERROR (`shiplog: spool cap exceeded —
  dropping oldest pending file`), and increments
  `shiplog_dropped_files_total`. A bucket outage degrades the trail but never
  fills the disk or blocks serving.
- **Empty files never ship.** An idle stream's active file is never rotated and
  never uploaded — an idle gateway produces no log objects at all.

---

## 5. Multinode

There is **no coordination** between gateways. Each `serve` process:

- picks a fresh random `<instance8>` ID at boot, so its file keys never collide
  with another node's;
- ships its own files independently;
- adopts and ships **any** leftover file it finds in its spool dir at boot
  (deliberate — a dead instance's spooled files must not strand).

That last point is why the operator rule is **one spool dir per serve
instance**. Two live instances sharing a spool dir is the one misconfiguration
that can double-ship (each would adopt the other's in-progress files). On a
single host running multiple gateways, give each its own `--log-spool-dir`.

Readers reconstruct the global timeline by listing across all `<instance8>`
files and merging on the `ts` field.

---

## 6. Consuming the logs

The files are gzipped NDJSON, so any JSON tool works after `gunzip`.

Pull a day's usage and total bytes per tenant/repo with `jq`:

```bash
# Download the day's usage objects (S3 example) and stream them through jq.
aws s3 cp --recursive s3://operator-bucket/repos/sys/logs/usage/2026/06/05/ ./usage/
for f in ./usage/*.ndjson.gz; do gunzip -c "$f"; done \
  | jq -r 'select(.kind=="fetch" or .kind=="push") | [.tenant, .repo, .kind, .bytes] | @tsv' \
  | awk -F'\t' '{b[$1"/"$2"\t"$3]+=$4} END{for (k in b) print k, b[k]}'
```

Tail the activity (audit) stream for a single repo:

```bash
for f in ./activity/*.ndjson.gz; do gunzip -c "$f"; done \
  | jq -c 'select(.repo=="app" or .repo_id=="acme/app")'
```

Because the layout is `Hive`-style date partitions of gzipped NDJSON, it loads
directly into Athena / BigQuery / DuckDB external tables (partition on
`year`/`month`/`day`, one table per stream) without a transform step.

---

## 7. Metrics

The engine emits cumulative `shiplog_*` metric lines through its **base**
logger (never back through the tap, to avoid a self-feeding loop). Each is
emitted from the ship loop only when the counter changed since the last tick,
and once more at shutdown to flush the final values:

| Metric | Meaning |
|---|---|
| `shiplog_shipped_files_total` | Spool files successfully uploaded |
| `shiplog_shipped_events_total` | NDJSON lines (events) successfully uploaded |
| `shiplog_ship_errors_total` | Per-file upload failures (each retried on a later tick) |
| `shiplog_dropped_events_total` | Events dropped because the intake queue was full |
| `shiplog_dropped_files_total` | Pending files dropped by the spool cap |

The shipper emits **no audit events of its own** (that would feed itself). A
non-zero `shiplog_dropped_events_total` or `shiplog_dropped_files_total` is the
signal to investigate (store outage, undersized spool cap, or an event-rate
spike outrunning the intake queue).

---

## 8. Limitations

- **No querying or UI.** Shipping writes durable objects; bring your own query
  engine (see §6). Per-tenant file partitioning is also out of scope — `tenant`
  is a field in every record, so partition at query time.
- **Activity is the `audit=true` taxonomy only.** General (non-audit) log lines
  are not shipped; stderr remains the place for those.
- **LFS direct-mode bytes are negotiated, not transferred.** In direct transfer
  mode the object bytes flow between the client and the storage backend's signed
  URL, not through the gateway — so the gateway cannot observe the actual
  transferred byte count. The `lfs_download` usage record reports the **sum of
  the negotiated object sizes** from the batch response (`status:"negotiated"`,
  `objects:N`), with errored objects excluded. This is the honest upper bound on
  what the client was authorized to transfer, not a wire measurement. The
  `lfs_upload` record (emitted at verify success) reports the verified object
  size.
- **No fsync-per-event durability** (see §4) — a hard crash can lose the active
  file's OS-buffer tail.

---

## 9. Smoke test

`scripts/logship-smoke-localfs.sh` exercises the full path against a localfs
store: fast-rotation shipping of real push/clone usage records plus a
bundle-uri-proxied audit event (`PHASE1_SHIP_OK`), crash-leftover shipping
across a `kill -9` + restart (`PHASE2_LEFTOVER_OK`), and an idle serve that
ships nothing (`PHASE3_IDLE_OK`), ending in `LOGSHIP_SMOKE_OK`.
