# Authdb replication (operator guide)

This guide explains how to replicate bucketvcs's embedded SQLite **auth
database** into object storage and restore it on boot, using Litestream
embedded inside the gateway. The authdb holds users, tokens, scopes, repo
metadata, policy rules, webhook endpoints/deliveries, LFS locks, and quotas —
everything except the Git object data itself. Replication continuously ships
the WAL to a reserved prefix in an object store so that a gateway whose local
disk is lost can restore the authdb from the bucket and keep authenticating
the same tokens.

Git object data is **not** covered by this feature and does not need to be: it
already lives in the object store and is written with compare-and-swap
manifests, so it is durable and consistent independently. This guide covers the
authdb only.

---

## 1. What it does

When enabled, each `bucketvcs serve` process:

1. Acquires a single-writer **lease** in the object store (so only one node
   replicates the authdb lineage at a time).
2. **Restores the authdb from the replica iff the local file is missing**
   (disaster recovery on a fresh disk; an existing local file is never
   overwritten on boot).
3. Streams the SQLite WAL into the object store via embedded Litestream while
   it serves.
4. On graceful shutdown, performs a final sync, closes the replica, and
   releases the lease.

**Recovery point objective (RPO).** Litestream ships WAL transactions roughly
**1 second** after they commit, so the worst-case data loss on an abrupt total
loss of the local disk is about one second of authdb writes (recently created
tokens, grants, locks, etc.). Compaction runs in the background at level 1
every 30 s and level 2 every 5 minutes; a periodic snapshot is taken at the
litestream snapshot level. None of these change the ~1 s RPO — they only keep
the LTX file count bounded.

**What is and is NOT covered:**

| Data | Covered by this feature? | Why |
|---|---|---|
| Auth DB (users, tokens, scopes, repos, policy, webhooks, LFS locks, quotas) | **Yes** | This is the SQLite file being replicated |
| Git object data (packs, refs, bundles, LFS objects) | No (not needed) | Already in the object store, CAS-safe and durable on its own |
| PostgreSQL / libSQL auth backends | No | Those backends bring their own durability/replication; replication is rejected for them (see §2) |

---

## 2. Enabling replication

Replication is **off by default**. It applies only to the embedded SQLite
authdb backend, and only on the primary gateway.

### 2.1 `--auth-db-replica=auto` (replicate into the system bucket)

The simplest setup reuses the `--store` bucket and writes the replica under the
reserved `sys/authdb/` prefix:

```bash
bucketvcs serve \
  --store s3://operator-bucket/repos \
  --auth-db /var/lib/bucketvcs/auth.db \
  --auth-db-replica=auto \
  --addr :8080
```

`auto` requires `--store`. The replica lives at `sys/authdb/` inside that same
bucket. This is the recommended configuration for most deployments — one
bucket, one set of credentials, the authdb replica safely segregated from repo
data (see §3).

### 2.2 `--auth-db-replica=<storage-url>` (dedicated replica location)

To put the replica in a different bucket (or a different backend entirely),
pass a storage URL instead of `auto`:

```bash
bucketvcs serve \
  --store s3://operator-bucket/repos \
  --auth-db /var/lib/bucketvcs/auth.db \
  --auth-db-replica 's3://authdb-backups/prod' \
  --addr :8080
```

bucketvcs opens that URL as its own store and writes the replica under
`sys/authdb/` within it. Any backend that passes object-store conformance
(localfs, S3/R2, GCS, Azure Blob) is valid here.

### 2.3 `--auth-db-replica-lease-ttl` (lease validity window)

```bash
--auth-db-replica-lease-ttl 60s   # default
```

This is the validity window of the single-writer lease (see §4). The heartbeat
renews the lease every **TTL/3**. After an unclean crash (kill -9, power loss)
the lease survives until this TTL expires, after which the next gateway can take
it over. A shorter TTL means faster takeover after a crash, at the cost of more
frequent renewal writes; the 60 s default is a reasonable balance. Do not set it
below a few seconds.

### 2.4 `--auth-db-replica-skip-restore` (escape hatch)

```bash
--auth-db-replica-skip-restore
```

By default, if the local authdb file is **missing** at boot, bucketvcs restores
it from the replica and **refuses to start if the restore fails** (fail-closed —
booting with an empty authdb while a replica exists would fork history). This
flag skips the restore-on-boot step entirely.

**Only safe when** the local file is missing **and** you know the replica
location is empty — for example, the very first start of a brand-new deployment
whose bucket has never held a replica. In that situation restore-on-boot has
nothing to restore and (depending on the backend) may error; the flag lets the
gateway start with a fresh empty authdb and begin replicating it. Do **not** use
this flag as a way to ignore a restore failure on a node that previously had
data — that silently discards the replica.

### 2.5 Validation (exit code 2)

`serve` rejects the following at startup with a diagnostic and exit code 2:

- **Non-SQLite authdb.** `postgres://`, `postgresql://`, `libsql://`,
  `http://`, `https://` auth backends are rejected:
  *"replication is for the embedded sqlite backend; libsql/postgres bring their
  own durability"*. Use the backend's native replication instead.
- **Replica-serve mode.** Combining `--auth-db-replica` with `--replica-of`
  (an M26 regional read replica) is rejected: *"not allowed in replica-serve
  mode (--replica-of); only the primary replicates the authdb"*. Only the write
  region's primary replicates the authdb.
- **`auto` without `--store`.** `--auth-db-replica=auto` requires `--store`.

---

## 3. The reserved `sys/` prefix

The authdb replica lives under a reserved top-level prefix in the bucket:

```
sys/authdb/ltx/<level>/<min>-<max>.ltx     # the replicated WAL (LTX files)
sys/authdb/lease.json                       # the single-writer lease
```

`sys/` is **reserved for system data**. Repository data and garbage collection
operate entirely under `tenants/` and never read, write, list, or sweep
anything under `sys/`. This separation is deliberate and load-bearing:

- **GC never touches `sys/`.** The replica can never be mistaken for unreachable
  repo data and swept. You do not need to exclude it from GC manually.
- **Do not put tenant data under `sys/`.** It is bucketvcs's namespace; keep
  your own objects out of it.

**localfs appearance.** On the localfs backend, object bodies are stored under
`<root>/objects/` with a `.meta` sidecar per object. So the replica objects
appear on disk as:

```
<root>/objects/sys/authdb/ltx/0/...ltx
<root>/objects/sys/authdb/ltx/0/...ltx.meta
<root>/objects/sys/authdb/lease.json
<root>/objects/sys/authdb/lease.json.meta
```

The `.meta` sidecars and the `objects/` interposition are expected localfs
internals, not corruption. `replica-status` and `authdb restore` skip the
`.meta` files automatically.

---

## 4. Single-writer rule and the lease

Exactly one gateway may replicate a given authdb lineage at a time. Two
gateways replicating the same lineage would interleave WAL segments and corrupt
the replica (split-brain). The lease at `sys/authdb/lease.json` enforces this
with object-store compare-and-swap (CAS) primitives — no external coordinator.

### 4.1 Lease lifecycle

- **Acquire (boot).** `PutIfAbsent` on `lease.json`. If it already exists and
  the holder's lease has **expired** (`renewed_at + ttl_s` is in the past), the
  new node takes over via `PutIfVersionMatches` on the stale version. If the
  holder is still **live**, `serve` refuses to start.
- **Renew (runtime).** The heartbeat re-CASes `lease.json` every TTL/3. A
  successful renew extends `renewed_at`.
- **Release (graceful shutdown).** The lease is deleted as the last step of
  shutdown, so the next start sees a clean slate and acquires immediately.

**Refusal on a live holder.** If you start a second gateway against a store
whose lease is held by a live node, startup fails (exit 1) with a message that
names the current holder so you can find it:

```
authreplica: lease held by another instance: instance=<hex-id> host=<hostname> pid=<pid> renewed_at=<RFC3339>
```

(On the **localfs** backend you will usually hit the root `.lock` guard first —
see §4.3 — and never reach the lease check.)

**Takeover after a crash.** kill -9 / power loss does not run the graceful
release, so `lease.json` survives in the bucket with a still-valid `renewed_at`.
The next gateway must wait out the remaining `--auth-db-replica-lease-ttl`
before its CAS takeover succeeds; until then it refuses to start with the
"lease held by another instance" message above. This is the single-writer
guarantee working as designed — give it the TTL to expire, or shorten the TTL
if faster recovery matters more than renewal-write frequency.

### 4.2 RUNBOOK: a node lost its lease (`authdb.replica.lease_lost`) — ACT ON IT

> **This is the most important operational note in this guide.**

If a running gateway's lease is taken over by another node (for example, a
network partition let a second node believe the first was dead and CAS the
lease away), the original node emits the audit event
**`authdb.replica.lease_lost`** at ERROR, stops its own replication
(`authdb.replica.replication_stopped`), and **keeps serving requests**.

The lease protects the *replica lineage*, not the *local database*. So the node
that lost the lease continues to accept and apply authdb writes locally — **but
those writes are no longer replicated.** Two consequences:

1. If that node later dies, every authdb write it made after losing the lease is
   **permanently lost** — it never reached the replica.
2. The two nodes have **diverged**: each applied authdb writes the other (and the
   replica) never saw.

**Therefore treat `authdb.replica.lease_lost` as actionable, not informational:**

- **Alert on the audit event** `authdb.replica.lease_lost` (and
  `authdb.replica.replication_stopped`). Page on it.
- **Drain and restart the affected node.** A restart re-contends for the lease
  cleanly: it either re-acquires (and resumes replicating) or refuses to start
  because the new holder is live — either outcome is correct and unambiguous,
  and the divergence window is closed.
- Do not leave a lease-lost node serving indefinitely. The longer it runs, the
  more unreplicated, divergent writes accumulate.

### 4.3 localfs-specific: stale `.lock` after an unclean crash

> **Applies to the localfs backend only.** Cloud backends (S3/R2, GCS, Azure)
> have no such file; their single-writer guard is the CAS lease, which expires
> on its own after `--auth-db-replica-lease-ttl`.

The localfs backend protects its root with a **pidfile lock** at `<root>/.lock`,
created `O_CREATE|O_EXCL` when the store is opened. This lock is **not** an
advisory `flock` tied to a file descriptor — it is a plain file on disk, so it
**survives kill -9 and power loss**. After an unclean crash the stale `.lock`
remains and any subsequent `serve` (or `authdb restore` / `replica-status`)
against that root fails with a "root is already locked by another instance"
error even though no process holds it.

**Recovery:** after confirming no bucketvcs process is actually using that root,
remove the stale lock before re-serving or restoring:

```bash
rm -f <root>/.lock
```

This is the localfs analog of waiting out the CAS lease TTL on a cloud backend.
On localfs you must clear `.lock` *and* (if a dead node's lease is still inside
the TTL) wait out `--auth-db-replica-lease-ttl` for the lease takeover — see
§4.1.

---

## 5. Restore and point-in-time recovery

`bucketvcs authdb restore` rebuilds an authdb file from the replica. Use it for
disaster recovery (restore to the standard authdb path) or to make a read-only
inspection copy (`--output`).

### 5.1 Disaster recovery to the standard path

After total loss of the local disk, the gateway restores automatically on the
next boot (§1). To restore manually to the standard authdb location:

```bash
bucketvcs authdb restore \
  --replica=auto --store s3://operator-bucket/repos \
  --auth-db /var/lib/bucketvcs/auth.db \
  --force
```

`--replica` accepts `auto` (with `--store`, reuses the system bucket's
`sys/authdb/` prefix) or a storage URL, mirroring the `serve` flag.

### 5.2 Inspection copies (`--output`)

To restore to an arbitrary path without touching the live authdb (e.g. to
inspect historical state):

```bash
bucketvcs authdb restore \
  --replica 's3://authdb-backups/prod' \
  --output /tmp/inspect.db
```

### 5.3 Point in time (`--timestamp` / `--txid`)

Restore an upper-bounded copy of history:

```bash
# Everything up to and including a wall-clock instant (RFC3339):
bucketvcs authdb restore --replica=auto --store s3://operator-bucket/repos \
  --output /tmp/before-incident.db \
  --timestamp 2026-06-05T13:45:00Z

# Everything up to and including a specific transaction id (hex):
bucketvcs authdb restore --replica=auto --store s3://operator-bucket/repos \
  --output /tmp/at-txid.db \
  --txid 0000000000000a1f
```

`--timestamp` resolution is bounded by the replica's LTX timestamps (≈ sync
interval, ~1 s). Use `--output` for PITR copies so you can compare against the
live authdb before promoting one.

### 5.4 Overwrite and sidecar semantics (`--force` / `--if-not-exists`)

`restore` **refuses to overwrite an existing target** by default (exit 2):

```
authdb restore: <path> exists; pass --force to overwrite or --if-not-exists to no-op
```

- `--force` removes the existing target **and its stale `-wal`, `-shm`, and
  `-txid` sidecars** before restoring. This sidecar cleanup is important: a
  leftover `-wal`/`-shm` from the *previous* database would be replayed over the
  freshly restored file on the next WAL-mode open and **silently corrupt it**.
  Always use `--force` (not a manual `rm` of just the `.db`) when overwriting.
- `--if-not-exists` exits 0 without restoring if the target already exists —
  useful in idempotent provisioning scripts.

### 5.5 Reading replica status (`replica-status`)

`bucketvcs authdb replica-status` reports the current lease holder and the LTX
files present at each level. Output is NDJSON: an optional first line for the
lease (omitted if no `lease.json` exists), then one line per **non-empty** level
0..9 (level 9 is litestream's snapshot level).

```bash
bucketvcs authdb replica-status --replica=auto --store s3://operator-bucket/repos
```

Sample output:

```json
{"lease":{"instance_id":"3f9c1a...","hostname":"gw-1","pid":4812,"renewed_at":"2026-06-05T13:44:58Z","ttl_s":60}}
{"level":0,"files":7,"bytes":81920,"max_txid":"0000000000000a1f","latest":"2026-06-05T13:44:59Z"}
{"level":1,"files":2,"bytes":40960,"max_txid":"00000000000009e0","latest":"2026-06-05T13:42:30Z"}
{"level":9,"files":1,"bytes":131072,"max_txid":"0000000000000980","latest":"2026-06-05T13:30:00Z"}
```

How to read it:

- **`lease`** — who holds the single-writer lease right now. A stale
  `renewed_at` (older than `ttl_s` seconds ago) means the holder is dead and the
  lease is takeable. **No `lease` line** means no node currently (or recently)
  held it — normal for a replica that has never been served by a live node.
- **`max_txid` at level 0** — the newest transaction that has reached the
  replica. Compare against your last known write to gauge replication lag.
- **`files` per level** — the LTX file count. If level-0 `files` grows without
  bound and never compacts down, see the R2 note in §6.

---

## 6. Backend notes

### 6.1 Cloudflare R2 and S3

The replica client deletes obsolete LTX files with **per-key conditional
deletes** (`Head` + `DeleteIfVersionMatches`), never the batch `DeleteObjects`
API. This deliberately sidesteps the documented R2 `DeleteObjects`
silent-failure behavior, where a batch delete can report success while leaving
objects behind.

Even so, as **belt-and-suspenders on R2**:

- **Add an object-lifecycle rule** scoped to the `sys/authdb/` prefix to expire
  old LTX objects, so any that escape compaction are reclaimed by the provider.
  A retention comfortably longer than your PITR window is appropriate.
- **Watch the LTX file counts** in `replica-status` (§5.5). Litestream issue
  #976 reported LTX accumulation on R2; a level-0 `files` count that climbs and
  never compacts down is the signal to investigate (and the reason the lifecycle
  rule above is recommended).

### 6.2 localfs

- Objects appear under `<root>/objects/sys/authdb/...` with `.meta` sidecars
  (§3) — expected, not corruption.
- The root `.lock` pidfile survives unclean crashes and must be cleared by hand
  before re-serving/restoring (§4.3).
- localfs adds a second, stronger single-writer guard (the root lock) that fires
  before the lease on a *live* second `serve`; the lease alone is the guard on
  cloud backends.

---

## 7. Failure modes

| Situation | Behavior | Operator action |
|---|---|---|
| **Replication/sync error while serving** (transient store error, renew blip) | **Fail-open: the gateway keeps serving.** A lease *renew* error is metered (`authdb_replica_lease_renew_errors_total`) and logged WARN; replication retries on the next heartbeat. | None usually; if sustained, check store connectivity and watch the metric. |
| **Restore-on-boot fails** (local file missing, restore errors) | **Fail-closed: `serve` refuses to start** rather than run with an empty authdb that could fork history. | Fix the store/credentials and restart; or, only if the replica is genuinely empty and the local file is missing, pass `--auth-db-replica-skip-restore` (§2.4). |
| **Lease held by a live node** at boot | `serve` exits naming the holder (instance/host/pid/renewed_at). | Confirm the named node is the intended writer; stop it or point the new node elsewhere. |
| **Lease survives a kill -9** | `lease.json` persists until the TTL expires; the next start takes over via CAS once expired. | Wait out `--auth-db-replica-lease-ttl`, or shorten the TTL. On localfs also clear `<root>/.lock` (§4.3). |
| **Lease lost while serving** (`authdb.replica.lease_lost`) | Replication **stops**, the node **keeps serving**, and its subsequent authdb writes are **not replicated**. | **Runbook §4.2: alert, then drain/restart the node.** Do not ignore. |
| **LTX position mismatch after unclean shutdown** | Restore/replay may report an LTX position or txid mismatch (a partial WAL segment from the moment of the crash). | Use `replica-status` to read the consistent `max_txid` at level 0, then `authdb restore --output <copy> --txid <that-max>` to a copy, verify it, and promote it with `--force` (which clears stale `-wal/-shm/-txid` sidecars — §5.4). Do not hand-edit sidecars. |

---

## 8. Metrics and audit events

The following are **actually emitted** by the replication subsystem today
(verified in `internal/authreplica/runner.go`):

### Metrics

| Metric | Type | Emitted when |
|---|---|---|
| `authdb_replica_lease_renew_errors_total` | counter (value 1 per event) | A lease **renew** fails with a transient (non-takeover) error; the heartbeat will retry. Accompanied by a WARN log. |

### Audit events

| Event | Level | Meaning |
|---|---|---|
| `authdb.replica.restored` | INFO | Restore-on-boot recreated the local authdb from the replica (carries `db_path`, `duration_ms`). |
| `authdb.replica.lease_lost` | ERROR | This node's lease was taken over by another instance (carries `instance_id`). **Actionable — see §4.2.** |
| `authdb.replica.replication_stopped` | ERROR | Replication stopped because the lease was lost; the node keeps serving. Always paired with `lease_lost`. |

**Not emitted in this release (do not alert on these — they do not exist yet).**
The following names appeared in early planning but are **not** implemented today:
`authdb_replica_sync_errors_total`, `authdb_replica_last_sync_unix`,
`authdb_replica_ltx_files`, and an `authdb.replica.lease_takeover` audit event.
For sync freshness and LTX file counts, query `bucketvcs authdb replica-status`
(§5.5) instead — `max_txid`/`latest` give you replication position and recency,
and the per-level `files` count gives you LTX accumulation. Lease takeover is
observable from the consumer side via `authdb.replica.lease_lost` on the node
that *lost* the lease.

---

## 9. Verifying your setup

A localfs end-to-end smoke covering durability through total local-disk loss,
point-in-time restore, and single-writer mutual exclusion ships at:

```bash
scripts/authdb-replica-smoke-localfs.sh
```

Override the backend to run the same phases against MinIO/S3:

```bash
SMOKE_STORE_URL='s3://bucket/prefix' scripts/authdb-replica-smoke-localfs.sh
```

(The direct-filesystem assertions and the stale-`.lock` manipulation run only
for the localfs default; the `replica-status`/`restore` CLI assertions run on
any backend.)

---

## 10. Limitations

- **Single replica destination.** Replication targets exactly one location
  (`auto` or one storage URL). Fan-out to multiple replicas is not supported.
- **Embedded SQLite backend only.** PostgreSQL and libSQL authdb backends are
  rejected (§2.5); use their native replication.
- **Primary only.** The authdb is replicated only by the write-region primary.
  M26 regional read replicas (`--replica-of`) cannot replicate the authdb, and
  combining the two flags is rejected at startup.
- **BYOB buckets never hold the authdb.** Per-tenant bring-your-own-bucket
  storage is a data plane for that tenant's repos only; the authdb replica lives
  in the operator's system store (`--store`, or the explicit
  `--auth-db-replica` URL), never in a tenant bucket.
- **Compaction and snapshot levels are not operator-tunable this release.** The
  level-1 (30 s) / level-2 (5 m) compaction intervals and the litestream
  snapshot level are fixed; only the lease TTL and restore behavior are
  configurable.
</content>
</invoke>
