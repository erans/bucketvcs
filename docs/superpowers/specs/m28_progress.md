# M28 — Embedded Litestream Authdb Replication — progress

Date merged: 2026-06-05
Merge: PR #11, squash commit `defbf57` (`M28: Embedded Litestream authdb replication (#11)`)
Follow-ups: PR #12 `0bfd2a1` (authdb hosting guide + durability notes), PR #13 `1a04e8e` (litestream edge-case suite + operator-guide discoveries)
Tag: `v0.4.0`
Spec: `docs/superpowers/specs/2026-06-05-embedded-litestream-authdb-replication-design.md`
Plan: `docs/superpowers/plans/2026-06-05-embedded-litestream-authdb-replication.md`

## Summary

M28 makes the embedded SQLite authdb durable by embedding Litestream v0.5.11
(pinned) inside `bucketvcs serve`. A custom litestream `ReplicaClient`
implemented over `storage.ObjectStore` ships every authdb WAL transaction into
a reserved `sys/authdb/` prefix of the object store (~1 s RPO) — one
implementation covers all four canonical backends plus BYOB, using the
adapters' own credentials and CAS primitives. On boot, a missing local authdb
is restored from the bucket automatically (fail-closed on storage faults); a
CAS single-writer lease at `sys/authdb/lease.json` guards the replica lineage
against split-brain. New `bucketvcs authdb restore` (including point-in-time
recovery by `--timestamp`/`--txid`) and `bucketvcs authdb replica-status` CLI.

## Components

- `internal/authreplica/client.go` — litestream `ReplicaClient` over
  `ObjectStore`: key layout `sys/authdb/ltx/<level>/<ltx-filename>`,
  CAS-synthesized last-writer-wins PUT (`PutIfAbsent` → `Head` +
  `PutIfVersionMatches` retry loop), per-key conditional deletes (deliberately
  avoids the R2 batch `DeleteObjects` silent-failure bug), ranged reads via
  `GetRange`, `ErrNotFound` → `os.ErrNotExist` mapping.
- `internal/authreplica/lease.go` — CAS lease (TTL default 60 s, heartbeat
  TTL/3): `PutIfAbsent` acquire, expired-holder takeover via
  `PutIfVersionMatches`, holder-naming refusal errors, exported `LeaseDoc`.
- `internal/authreplica/runner.go` — two-phase lifecycle matching serve's
  boot order: `Prepare` (lease + `EnsureExists` restore-iff-missing, BEFORE
  `sqlitestore.Open`) and `StartReplication` (litestream store with levels
  L0 / L1=30 s / L2=5 m, AFTER the authdb file exists). Lease loss stops
  replication but not the server. Ordered shutdown: heartbeat → litestream
  store close → authdb close → lease release.
- Serve wiring — `--auth-db-replica=off|auto|<storage-url>` (default off; env
  `BUCKETVCS_AUTH_DB_REPLICA`), `--auth-db-replica-lease-ttl`,
  `--auth-db-replica-skip-restore`. Hard-rejected (exit 2) for
  postgres/libsql authdbs (`sqlitestore.IsNonSQLiteValue`) and M26
  replica-serve mode. DB path routed through `sqlitestore.SQLitePath` so
  litestream tracks exactly the file sqlite opens. Single-owner cleanup
  (early-exit guard / armed real defer) keeps LIFO ordering correct on every
  exit path.
- CLI — `bucketvcs authdb restore` (`--timestamp`/`--txid` mutually
  exclusive; `--force` also clears stale `-wal/-shm/-txid` sidecars, whose
  replay would silently corrupt a restored DB) and `authdb replica-status`
  (NDJSON: lease holder + per-level files/bytes/max_txid/latest).

## Semantics

- Replication is **fail-open** (storage blips never take down auth);
  restore-on-boot is **fail-closed** (an unreadable replica refuses startup —
  the error deliberately does not suggest the skip-restore escape hatch).
- Audit events: `authdb.replica.restored` (only on actual restore),
  `.lease_takeover`, `.lease_lost`, `.replication_stopped`. Metric:
  `authdb_replica_lease_renew_errors_total`. The spec's three sync metrics
  were consciously descoped (documented as not-emitted; `replica-status`
  covers freshness/file counts).

## Testing

- Conformance suite (adapted from litestream's unimportable harness;
  Apache-2.0 attributed) across localfs + S3/MinIO + GCS/fake-gcs +
  Azurite + real GCS.
- Real-litestream integration test: replicate → wipe disk → restore.
- 4-phase smoke `scripts/authdb-replica-smoke-localfs.sh` (kill -9
  durability, PITR, lease contention, true multi-process CLI writes) —
  verified on localfs, fake-gcs, and a real GCS bucket; the GCS runs exercise
  the genuine CAS lease rejection.
- Edge-case suite (`internal/authreplica/edgecase_test.go`, PR #13) encoding
  empirically discovered litestream truths:
  - Resuming replication against a **stale local authdb silently corrupts the
    replica lineage** (no write-time error; surfaces later as a malformed
    image) — now a §7 operator-guide warning, and the reason the lease +
    restore-only-when-missing design exists.
  - Replica **level-0 LTX files are never pruned** by litestream (L0
    retention is local-only) — lifecycle rules documented for all backends.
  - Transient store faults recover on the next ~1 s sync tick (fail-open
    verified under fault injection).
  - `Pos().TXID` equals the insert count exactly; PITR resolves at-or-before
    the request, never forward.

## Review trail

Per-task spec + quality reviews (subagent-driven), 6 roborev refine rounds
(~27 findings fixed, 1 dismissed with upstream source evidence). Notable
catches: stale-WAL corruption hazard on `--force` restore, `sqlite:` DSN path
divergence, a LIFO defer inversion that leaked the lease, phantom restored
events on clean restarts.

## Deferred

Sync metrics (`authdb_replica_sync_errors_total`, `_last_sync_unix`,
`_ltx_files`), VFS read replicas, compaction/retention tuning knobs,
multi-replica destinations (litestream v0.5 is single-replica), non-sqlite
backends (postgres/libsql bring their own durability).
