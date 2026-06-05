# Embedded Litestream authdb replication — design

**Date:** 2026-06-05
**Status:** Approved (brainstorm complete, pending implementation plan)

## Problem

The authdb (embedded sqlite, `--auth-db`) is the only durable state in a bucketvcs
deployment that does not live in the object store. Git/LFS data is CAS-safe in the
bucket; the authdb (users, tokens, repos, permissions, LFS locks, quotas, policies,
webhooks, hooks, OIDC, sessions, storage bindings) lives on local disk. On
ephemeral-disk platforms (containers, spot instances) or after disk loss, the authdb
is gone.

Goal: continuously replicate the sqlite authdb into the same object store that holds
repo data (or any bucket we support), and restore it automatically on boot — by
embedding Litestream as a library rather than requiring a sidecar process.

## Decision summary

| Decision | Choice |
|---|---|
| Replication core | Custom litestream `ReplicaClient` backed by `storage.ObjectStore` |
| Destination default | Same bucket as `--store`, reserved prefix `sys/authdb/` (`auto` mode); explicit storage URL also supported |
| Activation | Opt-in flag `--auth-db-replica`, default `off` |
| Restore | Auto restore-iff-missing on serve boot (`EnsureExists`) + explicit `bucketvcs authdb restore` CLI with point-in-time support |
| Split-brain guard | CAS lease object in the bucket (`PutIfAbsent`/`PutIfVersionMatches`), heartbeat + TTL |
| Dependency | `github.com/benbjohnson/litestream` pinned at `v0.5.11` (Apache-2.0) |

### Why a custom ReplicaClient (Approach A)

- One implementation covers all four canonical backends (localfs, s3compat, gcs,
  azureblob) and anything BYOB adds later — the hard requirement.
- Replica traffic uses **our** adapters: our credentials/config (no second credential
  surface), our retry semantics, our key conventions. On localfs, LTX files get
  `.meta` sidecars like any object, so the replica prefix is a well-formed corner of
  the bucket.
- `ObjectStore` primitives map cleanly: `GetRange` ↔ ranged `OpenLTXFile`, `List` ↔
  `LTXFiles`, conditional writes ↔ `WriteLTXFile`.
- Litestream ships a public conformance harness (`RunWithReplicaClient`) — the same
  suite its built-in backends pass — which we run in CI and re-run as the gate on
  every litestream version bump (the library API is explicitly unstable; we pin).

Rejected: **B** — litestream's native s3/gs/abs/file clients (second config surface,
breaks localfs sidecar convention, no BYOB story). **C** — DIY `VACUUM INTO`
snapshots (minutes RPO, no PITR).

## Architecture

New package `internal/authreplica` with three components.

### 1. `Client` — litestream `ReplicaClient` over `ObjectStore` (~300 LOC)

| litestream method | mapping |
|---|---|
| `Init(ctx)` | no-op (idempotent; object stores need no mkdir) |
| `WriteLTXFile(level, min, max, r)` | `PutIfAbsent(key, r)`; on `ErrAlreadyExists` → `Head` + `PutIfVersionMatches` loop (synthesizes unconditional-put semantics atomically) |
| `OpenLTXFile(level, min, max, offset, size)` | `GetRange` (plain `Get` when offset=0, size=0); `storage.ErrNotFound` → `os.ErrNotExist` |
| `LTXFiles(level, seek, useMetadata)` | `List` over the level prefix; parse TXIDs from key names; return `ltx.FileIterator` honoring `seek` |
| `DeleteLTXFiles(infos)` | per key: `Head` + `DeleteIfVersionMatches`; ignore `ErrNotFound`; retry on version mismatch |
| `DeleteAll(ctx)` | `List` + delete loop (tests/reset only) |
| `SetLogger(l)` | wire to our slog at WARN+ with `subsystem=authreplica` |

**Key layout** (new reserved top-level prefix; repo data lives entirely under
`tenants/…`, and GC mark/sweep is per-repo under that prefix, so `sys/` is provably
out of GC's reach):

```
sys/authdb/ltx/<level>/<minTXID 16-hex>-<maxTXID 16-hex>.ltx
sys/authdb/lease.json
```

`auto` resolves to this prefix in the `--store` bucket. An explicit storage URL
(`--auth-db-replica=s3://other-bucket/path`) opens its own adapter through the
existing `cmd/bucketvcs/store.go` URL dispatch and uses the same layout under that
path.

### 2. `Lease` — split-brain guard (~120 LOC)

JSON doc at `sys/authdb/lease.json`: `instance_id` (crypto/rand hex), `hostname`,
`pid`, `renewed_at`, `ttl_s`. TTL default 60s (`--auth-db-replica-lease-ttl`).

- **Acquire:** `PutIfAbsent`. If held: read; expired (`renewed_at + ttl < now`) →
  CAS takeover via `PutIfVersionMatches`; live → serve **fails startup** with an
  error naming the holder.
- **Heartbeat:** goroutine renews every TTL/3 via CAS on the held version.
- **Lost lease** (CAS mismatch on renew — another instance took over): stop
  replication, ERROR log + audit event, **serve keeps serving**. The lease protects
  the replica lineage, not the local DB; killing a live auth server would be worse.
- **Clean shutdown:** delete the lease (`DeleteIfVersionMatches`).

### 3. `Runner` — lifecycle glue (~150 LOC)

Builds `litestream.NewDB(authDBPath)` + `NewReplicaWithClient(db, client)`, calls
`db.EnsureExists(ctx)` **before** `sqlitestore.Open` (restore iff local file
missing, no-op otherwise), wraps `litestream.NewStore` with litestream's default
compaction levels (L0 + 30s L1 + 5m L2 — zero new tuning knobs this milestone), and
follows the existing serve background-goroutine pattern (`serveCtx` cascade, as the
webhook worker does).

Both bucketvcs and litestream v0.5 use `modernc.org/sqlite` — the documented-safe
single-driver combination. Litestream owns WAL checkpointing while serve runs.

## Config surface

Serve flags:

- `--auth-db-replica=off|auto|<storage-url>` — default `off`. Env:
  `BUCKETVCS_AUTH_DB_REPLICA`.
- `--auth-db-replica-lease-ttl=60s` — only tuning knob this milestone.
- `--auth-db-replica-skip-restore` — escape hatch: start with a missing local file
  without restoring (documented as "I know the bucket is empty/gone").

Startup hard-validation (exit 2, fail-fast):

- `--auth-db-replica` with libsql/postgres DSN → error (replication is for the
  embedded sqlite backend; libsql/postgres bring their own durability).
- `--auth-db-replica` in replica-serve mode (M26 read gateway) → error; only the
  primary replicates.
- `auto` without `--store` → error.

BYOB note (docs): the authdb is global; the replica always targets the **system**
store, never tenant buckets.

## Boot and shutdown sequence

Boot (new steps in **bold**):

1. Parse flags → open `--store` → **resolve replica target** (auto → system store +
   `sys/authdb/`; URL → own adapter).
2. **Acquire lease** (fail startup if held and live).
3. **`EnsureExists(ctx)`** — restore latest from bucket iff local authdb missing;
   log restored TXID/timestamp as audit event.
4. `sqlitestore.Open` (unchanged).
5. **litestream `Store.Open`** — replication + compaction goroutines under
   `serveCtx`.
6. Rest of serve as today.

Shutdown: HTTP/SSH drain → `SyncAndWait` (flush final transactions, bounded by
existing `--shutdown-timeout`) → litestream store close → authdb close → lease
delete. (Litestream contract: close its store before app DB connections.)

**Concurrent CLI writes** (`bucketvcs user add` etc. against the same sqlite file
while serve runs): safe — WAL-mode cross-process writes land in the WAL; litestream
monitors the WAL file and ships them. Writes made while serve is down are picked up
at next boot's first sync (file exists → `EnsureExists` no-ops → litestream syncs
forward from its last position).

## New CLI: `bucketvcs authdb`

- `bucketvcs authdb restore --replica=auto|<url> [--output=<path>]
  [--timestamp=<RFC3339> | --txid=<hex>] [--if-not-exists] [--force]` — explicit
  restore / point-in-time recovery to the resolved authdb path (or `--output`).
  Refuses to overwrite an existing file without `--force`. Takes no lease
  (read-only against the replica).
- `bucketvcs authdb replica-status --replica=...` — lists levels, latest TXID, last
  LTX timestamp, file counts, lease holder. (`List` + parse; cheap.)

## Error handling

**Replication is fail-open; restore is fail-closed.**

- **Replication errors** (bucket unreachable, PUT failures): serve keeps serving;
  litestream retries internally; surfaced via metrics + ERROR logs. Same posture as
  M15 webhooks' fail-open enqueue. Local sqlite remains source of truth.
- **Restore errors on boot** (file missing AND restore fails): serve refuses to
  start. Starting empty when a replica exists but is unreadable would silently fork
  history. `--auth-db-replica-skip-restore` is the explicit override.
- **LTX position mismatch** (unclean shutdown, OOM-kill mid-sync): litestream
  auto-recover stays off (its default — it discards point-in-time history). Error
  message includes documented recovery steps. This is a needs-a-human case by
  design.
- **Lease lost mid-run:** stop replication, ERROR + audit, keep serving.

Platform quirks:

- **R2:** `DeleteObjects` batch-delete silent-failure bug + LTX-accumulation report
  (litestream issue #976). Our `DeleteLTXFiles` issues per-key
  `DeleteIfVersionMatches` (not batch), sidestepping the batch-API bug. Operator
  guide recommends an R2 lifecycle rule on `sys/authdb/` as belt-and-suspenders and
  `replica-status` to watch file counts.
- **localfs:** LTX objects get `.meta` sidecars by construction (writes go through
  the adapter).

## Observability

Existing slog conventions (`metric_name` lines; `"audit": true`).

Metrics:

- `authdb_replica_sync_errors_total`
- `authdb_replica_last_sync_unix` (lag derivable)
- `authdb_replica_ltx_files`
- `authdb_replica_lease_renew_errors_total`

Audit events:

- `authdb.replica.restored` (TXID, timestamp, duration)
- `authdb.replica.lease_takeover` (old/new holder)
- `authdb.replica.lease_lost`
- `authdb.replica.replication_stopped`

Litestream's internal `*slog.Logger` is wired to ours at WARN+ with
`subsystem=authreplica`.

## Testing

1. **Litestream conformance harness** — `RunWithReplicaClient` against our `Client`:
   localfs unconditionally; s3compat/gcs/azureblob behind the same env-gating as the
   existing storage conformance suite. Re-run is the upgrade gate for litestream
   version bumps.
2. **Unit tests** — key layout round-trip (level/TXID parse ↔ format),
   `PutIfAbsent` → CAS-overwrite fallback, lease state machine (acquire /
   held-live / held-expired / takeover / renew-race), startup validation matrix
   (postgres DSN, replica-mode, auto-without-store).
3. **End-to-end smokes** (localfs + MinIO):
   - Durability: serve with `--auth-db-replica=auto` → create user/token/repo →
     push → kill -9 → delete local authdb → restart → token still authenticates,
     repo perms intact.
   - PITR: create token A, wait, create token B, `authdb restore --timestamp`
     between → only A exists.
4. **Lease contention** — second serve against the same prefix exits non-zero
   naming the holder; after TTL expiry without heartbeat, takeover succeeds.

## Out of scope (deferred)

- Read replicas / VFS-based followers (litestream VFS is read-path, separate).
- Replicating anything other than the authdb.
- Compaction/retention tuning knobs.
- Multi-replica destinations (litestream v0.5 is single-replica per DB).
- Automatic lease-fatal mode (configurable "shut down serve on lease loss").
- Postgres/libsql replication (they bring their own durability).

## Risks

- **Litestream library API instability** (maintainers' explicit warning; v0.3→v0.5
  changed the entire interface). Mitigation: pin `v0.5.11`; conformance harness
  re-run gates upgrades; our integration surface is one package
  (`internal/authreplica`).
- **LTX retention growth on R2** (issue #976): mitigated by per-key deletes +
  lifecycle-rule guidance + `replica-status` visibility.
- **Restore-then-open ordering bugs** would be severe (forked history). Mitigated by
  fail-closed restore, the lease, and the kill-9 smoke.
