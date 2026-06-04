# Multi-node concurrency hardening (operator guide)

This guide covers running the PostgreSQL-backed metadata/auth DB safely behind
**multiple gateway nodes simultaneously**. It explains how to enable multi-node
deployments, what is race-safe (and how), and the caveats that remain.

SQLite and libSQL backends are unaffected: they remain single-node. This guide
applies only to the `postgres://` / `postgresql://` backend.

Production readiness summary:

- Multi-node Postgres deployments: every node is a safe webhook worker, quota
  counter, and rename participant — **shipped**.
- Webhook delivery: exactly-once claiming via `FOR UPDATE SKIP LOCKED` — **shipped**.
- LFS quota: per-upload idempotency via `quota_credits` unique PK — **shipped**.
- Repo rename: concurrency-safe via `RowsAffected` guard; exactly one concurrent
  rename wins — **shipped**.
- Connection pool size configurable via `--auth-db-max-conns` (default 10);
  Postgres honors it, SQLite/libSQL always use 1 — **shipped**.
- Rate limiter: remains in-memory per node; use a proxy/LB for a global limit —
  **unchanged (by design)**.
- SQLite / libSQL: still single-node regardless of `--auth-db-max-conns` —
  **unchanged**.

---

## 1. Overview

The Postgres backend can run behind a single gateway node with
`MaxOpenConns(1)`, which keeps the webhook-claim loop and in-process quota dedup
ring correct on one node. Multi-node operation removes that constraint.

You can run **N gateway nodes** (any N ≥ 1) all pointing at the same
`postgres://…` DB. No leader election, no extra coordination service, and no
configuration beyond the flags below is needed. Each node is a fully active
worker for every operation.

SQLite and libSQL remain single-node. A bare path, `file://`, `sqlite://`,
`libsql://`, `https://`, and `http://` schemes all continue to be forced to
`MaxOpenConns(1)` regardless of what `--auth-db-max-conns` is set to.

---

## 2. Enabling multi-node

### 2.1 Topology

Run N gateway instances, each with the same `--auth-db` Postgres URL:

```bash
export BUCKETVCS_DB_AUTH_TOKEN='<strong-password>'

# Node 1
bucketvcs serve \
  --auth-db 'postgres://bv@db.internal:5432/bucketvcs_auth?sslmode=require' \
  --auth-db-max-conns 10 \
  --listen :8080 \
  ...

# Node 2 (identical flags, different host / container)
bucketvcs serve \
  --auth-db 'postgres://bv@db.internal:5432/bucketvcs_auth?sslmode=require' \
  --auth-db-max-conns 10 \
  --listen :8080 \
  ...
```

Put an HTTP load balancer (nginx, HAProxy, a cloud LB) in front of the nodes.
There is no sticky-session requirement; any node can handle any request.

### 2.2 Connection pool sizing

`--auth-db-max-conns` sets `MaxOpenConns` for the Postgres connection pool on
each node. The total connections to Postgres is (nodes × max-conns). Size the
pool so that the aggregate does not exceed `max_connections` on the Postgres
server (default 100 for many distributions).

| Nodes | `--auth-db-max-conns` | Peak Postgres connections |
|------:|----------------------:|-------------------------:|
| 2     | 10 (default)          | ≤ 20                     |
| 4     | 10 (default)          | ≤ 40                     |
| 8     | 10 (default)          | ≤ 80                     |
| 8     | 5                     | ≤ 40                     |

The default of 10 is safe for small clusters. Reduce it if you run many nodes
against a Postgres instance with a low `max_connections` setting.

**SQLite / libSQL are unaffected.** The flag is accepted for those backends but
has no effect; the pool is always capped at 1 for single-writer correctness.

### 2.3 Startup confirmation

On startup each node logs:

```
INFO authdb opened backend=postgres
```

Absence of this line, or `backend=sqlite` / `backend=libsql`, means the node
did not select the Postgres backend and multi-node safety guarantees do not
apply.

---

## 3. What is now race-safe (and how)

### 3.1 Webhook delivery — `FOR UPDATE SKIP LOCKED`

The webhook worker on each node attempts to claim pending deliveries with:

```sql
SELECT id FROM webhook_deliveries
WHERE status = 'pending' AND next_attempt_at <= now()
ORDER BY next_attempt_at
LIMIT 32
FOR UPDATE SKIP LOCKED
```

`SKIP LOCKED` makes rows that are already claimed by another node's transaction
invisible to the current claimer. Exactly one node claims and delivers each
webhook event. There is no distributed lock, no coordination table, and no
leader; every node is an equal participant.

Under SQLite and libSQL the claim path continues to use the existing
serialized-write approach, which is correct for single-node deployments.

### 3.2 LFS quota counting — `quota_credits` unique PK

Before this hardening, an in-process dedup ring prevented a verify-replay from
incrementing `used_bytes` twice. That dedup was node-local, so two concurrent
nodes could each count the same upload.

A `quota_credits` table provides a unique PK on `(tenant, oid)`:

```sql
INSERT INTO quota_credits (tenant, oid, bytes)
VALUES ($1, $2, $3)
ON CONFLICT (tenant, oid) DO NOTHING
```

A `quota_credits` row is inserted at verify-success; the byte increment to
`quotas.used_bytes` happens inside the same transaction only when the INSERT
succeeds. Replayed verifies (same `(tenant, oid)`) hit the unique constraint,
the INSERT silently does nothing, and `used_bytes` is not incremented again.

This guarantee holds across all N nodes simultaneously. The in-process ring is
removed entirely.

**Upgrade note:** objects pushed and verified *before* the `quota_credits` table
existed have no `quota_credits` rows. Their subsequent deletion by `gc --lfs`
will not decrement `used_bytes`. Run `bucketvcs quota reconcile` immediately
after upgrading (and periodically thereafter) to correct any drift — see §4.3.

### 3.3 Repo rename — `RowsAffected` guard

Under Postgres `READ COMMITTED` (the default isolation level), a `SELECT
COUNT(*)` existence check acquires no row lock, so two concurrent renames of the
same repo could both observe the repo as existing and both proceed. The
existence pre-check is replaced with a guarded `UPDATE`:

```sql
UPDATE repos SET name = $new WHERE tenant = $t AND name = $old
```

Exactly one concurrent `UPDATE` will match the row; the other will find 0
`RowsAffected` and return `ErrNoSuchRepo`. The winning node's rename completes;
the losing node's caller receives an error and can retry or surface it to the
user. The operation is now atomic at the database level on all isolation levels
Postgres supports.

---

## 4. Caveats

### 4.1 Rate limiter remains per-node

The credential-failure rate limiter (token-bucket per client IP) is
in-memory per gateway node. With N nodes the effective burst a single IP can
reach before being throttled is approximately **N × Burst** (default Burst=10,
so 2 nodes → up to 20 failures before either node throttles).

To enforce a cluster-wide rate limit, front the gateways with a rate-limiting
reverse proxy or load balancer (nginx `limit_req_zone`, HAProxy stick-tables,
cloud WAF, etc.).

This is by design — the cost of a distributed rate limiter outweighs the
benefit for most deployments.

### 4.2 SQLite / libSQL remain single-node

Regardless of the `--auth-db-max-conns` value, SQLite and libSQL backends are
always opened with `MaxOpenConns(1)`. This is not a limitation of the multi-node
support; it is a fundamental property of SQLite's file-level locking model and
of the libSQL remote-write-serialization contract.

Do not run multiple gateways against the same SQLite file or the same libSQL
endpoint. Use the Postgres backend for multi-node deployments.

### 4.3 Quota drift after upgrading from a single-node release

The `quota_credits` table was introduced with multi-node support. Objects
uploaded and verified by an earlier single-node release have no corresponding
`quota_credits` rows. When those objects are later swept by `gc --lfs`, their
bytes will not be decremented from `used_bytes`, causing `used_bytes` to drift
above the true value.

Correct this with:

```bash
export BUCKETVCS_DB_AUTH_TOKEN='<password>'
bucketvcs quota reconcile --auth-db 'postgres://bv@host/bucketvcs_auth?sslmode=require' \
  --tenant <tenant>
```

Run this command once immediately after upgrading from a single-node release,
and schedule it periodically (e.g. weekly) as an ongoing correction. `reconcile` walks live LFS
objects in object storage, recomputes the true byte total, and updates
`used_bytes` atomically. It is safe to run while the gateway is live.

### 4.4 Quota byte columns widened to BIGINT (automatic, no action)

On PostgreSQL `INTEGER` is 32-bit (max ~2.1 GB), so the quota byte columns
(`quotas.limit_bytes`, `quotas.used_bytes`, and `quota_credits.bytes`) would
overflow for LFS objects or tenant totals above ~2 GB. Migration `0012` widens
all three to `BIGINT` and applies **automatically** on the first gateway start
after upgrading — no operator action is required, and the `>= 0` CHECK
constraints are preserved. (SQLite/libSQL were never affected: their `INTEGER`
storage is already 64-bit. The SQLite `0012` is a no-op that only advances
`schema_version` to keep both backends in lockstep.) After upgrading you can
confirm with `psql`:

```sql
SELECT column_name, data_type FROM information_schema.columns
 WHERE table_name IN ('quotas','quota_credits') AND column_name LIKE '%bytes%';
-- expect data_type = bigint for all three
```

---

## 5. Supported PostgreSQL versions

BucketVCS targets **PostgreSQL 14 and later**. The CI matrix covers:

| Version  | CI job                                  | Cadence     |
|----------|-----------------------------------------|-------------|
| 14       | `postgres conformance (pg14)`           | per-commit  |
| 18       | `postgres conformance (pg18)`           | per-commit  |

Both jobs run the full conformance suite plus a set of live concurrency tests:

- **Webhook claiming:** two concurrent workers race to claim the same delivery
  batch; each delivery is confirmed delivered exactly once.
- **Quota idempotency:** two concurrent verify-replays for the same `(tenant,
  oid)` are issued; `used_bytes` is confirmed incremented exactly once.
- **Rename winner:** two concurrent rename requests for the same repo are
  issued; exactly one succeeds, the other returns `ErrNoSuchRepo`.

These tests reproduce the race conditions multi-node operation eliminates and
prove they cannot recur.

---

## 6. Verifying your deployment

1. **Startup log:** confirm each node logs `authdb opened backend=postgres`.

2. **Pool size:** start with the default (`--auth-db-max-conns 10`) and monitor
   the Postgres `pg_stat_activity` view:

   ```sql
   SELECT count(*) FROM pg_stat_activity WHERE datname = 'bucketvcs_auth';
   ```

   The count should stay below nodes × max-conns even under load.

3. **Webhook delivery smoke:** create a webhook endpoint, trigger a push from
   two nodes simultaneously, and confirm the webhook is delivered exactly once.

4. **Quota smoke:** upload the same LFS object from two nodes concurrently and
   confirm `bucketvcs quota show` reports the expected byte total (not double).

5. **CI:** the per-commit `postgres conformance (pg14)` and `postgres conformance
   (pg18)` jobs run automatically on every push to `main`. A green run confirms
   multi-node safety has not regressed.
