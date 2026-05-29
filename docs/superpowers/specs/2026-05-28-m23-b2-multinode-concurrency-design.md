# M23 B2: Multi-node concurrency hardening

Date: 2026-05-28
Status: design

## 1. Goals

Make a **PostgreSQL-backed** bucketvcs metadata database safe to share across
**multiple gateway nodes** with a connection pool greater than one. M23 B1
shipped a working Postgres backend but kept `MaxOpenConns(1)` and the
single-writer assumptions the existing code relied on; B2 removes those
assumptions for the shared-state operations that would otherwise race, and lets
operators run N gateway nodes against one Postgres database.

This is **B2** of the M23 Phase B effort (B1 = Postgres backend + dialect layer,
already merged at `ab3bf07`, tag `m23-b1-postgres-complete`). B2 builds on the
B1 `Backend`/`Querier` seam.

Git object data is unaffected. SQLite and libSQL remain **single-node** backends
(WAL single-writer); B2's multi-node guarantees apply to **Postgres only**.
SQLite stays the zero-dependency default.

### 1.1 In scope (B2)

- **Webhook claim**: a Postgres `FOR UPDATE SKIP LOCKED` claim strategy so
  concurrent workers on different nodes never claim the same delivery row.
  sqlite/libsql keep today's SELECT-then-UPDATE (single-writer safe).
- **Quota verify-replay idempotency**: a `quota_credits` table (unique per
  `(tenant, oid)`) replacing the in-process dedup ring, so the same upload
  counted on two nodes increments `used_bytes` exactly once. Applied on all
  backends.
- **Connection pool sizing**: a `WithMaxConns` option + `--auth-db-max-conns`
  flag (Postgres default 10); sqlite/libsql always forced to 1.
- **PostgreSQL 14+ support**, guaranteed by a `postgres:14` + `postgres:18` CI
  matrix on both the per-commit and nightly conformance jobs.
- **Concurrent-rename safety**: verified (no code change expected) with a
  concurrency test.
- Multi-node operator guide section; retraction of B1's single-node caveats for
  Postgres.

### 1.2 Out of scope (deferred)

- **Distributed rate limiting** — the M18 limiter stays in-memory per-node;
  documented (effective burst ≈ N×Burst across N nodes). Operators needing a
  global limit front the gateway with a rate-limiting proxy/LB.
- **Leader election / sharded workers** — unnecessary: `SKIP LOCKED` makes every
  node a safe webhook worker with no coordination.
- **Read-replica routing / read/write splitting.**
- **Renaming the `internal/auth/sqlitestore` package** (still a misnomer; still
  YAGNI).

## 2. Architecture overview

```
N gateway nodes ──► one Postgres DB (pool size M per node)
   each node:
     webhook worker ─ claim via FOR UPDATE SKIP LOCKED ─► no double-claim
     LFS verify     ─ quota Add gated by quota_credits unique PK ─► counted once
     repo rename    ─ deferred-FK tx + MVCC row locks ─► serialized safely
   auth rate limiter: in-memory per node (per-node burst; documented)
```

B2 changes are localized to: the `Backend` interface (one capability flag), the
webhook claim path, the quota service, a new migration on both dialect sets, and
`Open`'s option plumbing. No store consumer signature changes beyond the new
optional `Open` variadic options.

## 3. Webhook claim: `FOR UPDATE SKIP LOCKED` (Postgres)

`internal/webhooks/worker.go`.

Add to the `Backend` interface (and expose via `Querier`):

```go
// SupportsSkipLocked reports whether the backend supports
// SELECT … FOR UPDATE SKIP LOCKED concurrent row claiming. true for postgres;
// false for sqlite/libsql (single-writer; they claim under serialization).
SupportsSkipLocked() bool
```
sqlite/libsql return `false`; postgres returns `true`.

`claim(ctx, db Querier, batch)` branches on `db.SupportsSkipLocked()`:

- **Postgres path** — one atomic claim-and-mark statement (written with `?`,
  rebound to `$N`):
  ```sql
  UPDATE webhook_deliveries d
     SET status='in_flight', last_attempt_at=?, attempts=d.attempts+1
    FROM webhook_endpoints e
   WHERE e.id = d.endpoint_id
     AND d.id IN (
         SELECT d2.id
           FROM webhook_deliveries d2
           JOIN webhook_endpoints e2 ON e2.id = d2.endpoint_id
          WHERE d2.status='pending' AND d2.next_attempt_at <= ? AND e2.active=1
          ORDER BY d2.next_attempt_at
          LIMIT ?
          FOR UPDATE SKIP LOCKED
     )
  RETURNING d.id, d.endpoint_id, d.event_type, d.payload_json, d.attempts, e.url, e.secret
  ```
  The inner `SELECT … FOR UPDATE SKIP LOCKED` locks exactly the rows this node
  will claim and skips rows another node already locked; the outer `UPDATE …
  RETURNING` flips them to `in_flight` and returns the claimed rows with their
  endpoint fields. Atomic, no double-claim across nodes.

- **sqlite/libsql path** — the current `RunInTx` SELECT-then-per-row-UPDATE,
  unchanged. Safe under their single-writer model.

`attempts` increments identically on both paths. `Reclaim` (in_flight→pending
after a threshold) is unchanged and remains the crash-recovery net on both
paths.

## 4. Quota idempotency: `quota_credits` table

New migration (next sequential number after 0010, in BOTH `migrations/` and
`migrations_postgres/`):

```sql
CREATE TABLE quota_credits (
    tenant      TEXT    NOT NULL,
    oid         TEXT    NOT NULL,
    bytes       INTEGER NOT NULL,
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY (tenant, oid)
);
```
(No FK — `quotas` is keyed by tenant only; `quota_credits` is per object. The
Postgres version applies the standard B1 rules: it has no BLOB/IDENTITY/strftime,
so it is identical text in both sets except the `schema_version` footer's
`strftime`→`EXTRACT(EPOCH FROM now())::bigint`.)

`internal/lfs/quota/quota.go`:

- **`Add(ctx, tenant, oid, bytes)`** — runs in one `RunInTx`:
  1. `INSERT INTO quota_credits (tenant, oid, bytes, recorded_at) VALUES (?,?,?,?)
     ON CONFLICT (tenant, oid) DO NOTHING` (both dialects accept it).
  2. If `RowsAffected() == 1` (newly credited): `UPDATE quotas SET used_bytes =
     used_bytes + ?, updated_at = ? WHERE tenant = ?`.
  3. If `0` (already credited on this or another node): no-op.
  The unique PK is the cross-node coordination point — exactly one node's insert
  wins and performs the increment.
- **`Subtract(ctx, tenant, oid, bytes)`** (M13.4 sweep) — in one `RunInTx`:
  delete the credit row as the gate, and only if it existed decrement: `UPDATE
  quotas SET used_bytes = <Greatest("used_bytes - ?", "0")>, updated_at = ?
  WHERE tenant = ?`. Implementation reads the gate via `SELECT … ; DELETE …` or
  `DELETE` + `RowsAffected()` within the tx (both dialect-safe; avoids relying on
  `DELETE … RETURNING`).
- The in-process `addRing` struct, its `sync.Mutex`, and `newAddRing` are
  **removed** — the DB guard supersedes them on all backends.
- **`Reconcile`** keeps recomputing `used_bytes` from storage as the drift
  backstop (unchanged); it does not need to rebuild `quota_credits`.

The table is bounded to the set of currently-counted objects (one row per live
credited object; removed on sweep) — no retention job needed.

## 5. Connection pool sizing

`internal/auth/sqlitestore`:

- `Open(value string, opts ...Option)` — variadic options; existing
  `Open(value)` callers are unchanged (B1 invariant preserved). `Option` is a
  functional option; `WithMaxConns(n int) Option` records the desired Postgres
  pool size.
- `postgresBackend` receives the max-conns value (via `resolveBackend`/
  `newPostgresBackend`) and calls `db.SetMaxOpenConns(n)` (default 10 when
  unset). `sqliteBackend`/`libsqlBackend` **always** `SetMaxOpenConns(1)`
  regardless of the option (WAL single-writer; honoring >1 would break their
  serialization assumptions).
- `cmd/bucketvcs/serve.go` adds `--auth-db-max-conns` (default 10) and passes
  `WithMaxConns(flag)` to `Open`. Other CLI subcommands use the default (their
  workloads are short-lived and single-connection is fine).

## 6. PostgreSQL 14+ support + CI version matrix

- All B1+B2 SQL features are ≤ PG 10 (`FOR UPDATE SKIP LOCKED` PG 9.5,
  `GENERATED … AS IDENTITY` PG 10, `ON CONFLICT` 9.5, `GREATEST`/`EXTRACT`/
  `DEFERRABLE`/partial indexes/`BYTEA` older). **Supported range: PostgreSQL
  14+.**
- `.github/workflows/ci.yml` `postgres-conformance` job → matrix
  `pg: ["14", "18"]` (service image `postgres:${{ matrix.pg }}`), runs per-commit.
- `.github/workflows/conformance.yml` `postgres` job → same `14` + `18` matrix,
  nightly.
- This supersedes B1's `postgres:17` pin (both jobs move to the matrix).

## 7. Concurrent-rename safety

`RenameRepo` (rename.go) already wraps its multi-table UPDATE in a single
transaction with deferred FKs (B1). With pool>1:
- Within-tx atomicity is preserved (`database/sql` binds a transaction to one
  connection).
- Cross-transaction safety rests on Postgres MVCC, the existing collision guard
  (refuse if the destination auth row exists), and the row locks the multi-table
  `UPDATE` takes.

**No code change is expected.** B2 adds a concurrency test: two concurrent
renames of the same repo — exactly one succeeds, the other fails cleanly with the
collision (or not-found) error, and no orphaned child rows remain.

## 8. Rate limiter (no code change)

The M18 credential-failure limiter stays in-memory per node. Documented caveat:
effective burst across N nodes ≈ N×Burst. Operators needing an exact global
limit place a rate-limiting proxy/load balancer in front of the gateways. This
is unchanged from B1.

## 9. Error handling

- Postgres claim under contention: `SKIP LOCKED` returns fewer rows, never an
  error — the worker simply processes what it claimed.
- Quota `Add` conflict (`RowsAffected()==0`) is the normal idempotent path, not
  an error.
- Pool exhaustion surfaces as the standard `database/sql` connection-wait
  behavior (bounded by context/deadline); no new error taxonomy.
- All existing `ErrConflict`/constraint mapping is preserved (proven by
  conformance on all backends).

## 10. Testing

### 10.1 Per-commit unit tests (no DB)
- `SupportsSkipLocked()` returns true for postgres, false for sqlite/libsql.
- Quota `Add` idempotency logic (newly-credited vs already-credited branch).
- `WithMaxConns` option plumbing (postgres honors it; sqlite/libsql forced 1).
- New migration splits cleanly (both dialect sets).

### 10.2 Conformance suite (gated, live Postgres — the real proof)
Extends `conformance_pg_test.go`, run with `MaxOpenConns>1` against live
Postgres in the `14`+`18` matrix:
- **Concurrent webhook claim**: two pools claim from a seeded backlog; assert no
  delivery row is claimed twice and all are claimed.
- **Concurrent quota Add**: two goroutines/pools `Add` the same `(tenant, oid)`;
  assert `used_bytes` increments exactly once.
- **Concurrent rename**: two concurrent same-repo renames; assert exactly one
  succeeds and no orphaned rows.
- Existing B1 PG conformance assertions still pass.

### 10.3 sqlite/libsql conformance
The sqlite full suite + libSQL conformance stay green (claim/quota behavior is
unchanged for them: `SupportsSkipLocked()==false`, and the credits-table path is
single-writer-correct).

### 10.4 Cross-compile gate
5-target `CGO_ENABLED=0` build stays green (no new dependencies).

## 11. Acceptance criteria

- N gateway nodes against one Postgres DB with pool>1 deliver each webhook once,
  count each LFS upload once, and rename repos without corruption — proven by the
  concurrency conformance tests in the 14+18 matrix.
- `--auth-db-max-conns` controls the Postgres pool; sqlite/libsql remain at 1.
- All migrations apply on Postgres 14 and 18.
- `go build ./...`, full `go test`, `go vet`, and the 5-target `CGO_ENABLED=0`
  cross-build stay green; per-commit CI (incl. the PG matrix) green.
- sqlite/libsql behavior unchanged; SQLite remains the zero-dependency default.
- Operator guide documents multi-node deployment; B1's single-node Postgres
  caveats are retracted.

## 12. Open questions (resolved by sensible defaults)

- **Default pool size** — 10 (operator-tunable via `--auth-db-max-conns`).
- **Credits table retention** — none needed; bounded by sweep-delete.
- **`DELETE … RETURNING` vs SELECT+DELETE in Subtract** — use the
  dialect-safe SELECT/`RowsAffected` form to avoid depending on RETURNING
  semantics across engines (decided at implementation).
