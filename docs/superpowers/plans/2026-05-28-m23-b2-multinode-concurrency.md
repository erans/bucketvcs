# M23 B2: Multi-node concurrency hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a PostgreSQL-backed bucketvcs metadata DB safe to share across multiple gateway nodes with a connection pool > 1 — race-free webhook claiming, cross-node quota idempotency, configurable pooling, and a PG 14+ CI matrix.

**Architecture:** Extend the B1 `Backend`/`Querier` seam with one capability flag (`SupportsSkipLocked`); branch the webhook claim to a Postgres `FOR UPDATE SKIP LOCKED` statement; replace the quota in-process dedup ring with a `quota_credits` table gated by a unique PK; make the connection pool size a functional `Open` option (Postgres default 10, sqlite/libsql forced 1). sqlite/libsql keep today's single-writer behavior.

**Tech Stack:** Go 1.25, the B1 dialect layer, `quota_credits` migration (both dialect sets), dockerized `postgres:14` + `postgres:18` for conformance.

**Spec:** `docs/superpowers/specs/2026-05-28-m23-b2-multinode-concurrency-design.md`

---

## Background the implementer needs

- B1 (merged `ab3bf07`) added a `Backend` interface in `internal/auth/sqlitestore/backend.go` (`Name`/`Open`/`ApplyMigration`/`Rebind`/`IsUniqueViolation`/`IsCheckViolation`/`IsFingerprintUniqueViolation`/`NowSeconds`/`Greatest`/`DeferForeignKeys`/`InsertReturningID`), three impls (`sqliteBackend`, `libsqlBackend`, `postgresBackend`), and `resolveBackend(value)` selecting by URL scheme.
- A `Querier`/`Tx` rebind wrapper lives in `internal/auth/sqlitestore/querier.go`. `Querier` = `ExecContext`/`QueryContext`/`QueryRowContext`/`RunInTx(ctx, func(tx Tx) error)`/`Greatest`/`IsUniqueViolation`/`IsCheckViolation`. `Tx` = `ExecContext`/`QueryContext`/`QueryRowContext`/`InsertReturningID`. Both `dbWrap` and `txWrap` delegate dialect calls to `backend`. `NewTestQuerier(*sql.DB) Querier` wraps a raw DB as sqlite-backed for tests. Every method rebinds `?`→`$N` for postgres (identity for sqlite/libsql).
- `Store.Open(value string)` is at `store.go:25`. CLI + serve open via `openAuthDB(flag string)` in `cmd/bucketvcs/authdb.go` → `sqlitestore.Open(path)`. serve calls `openAuthDB(*authDB)` at `cmd/bucketvcs/serve.go:221`.
- Migrations: `internal/auth/sqlitestore/migrations/*.sql` (sqlite) + `migrations_postgres/*.sql` (postgres), embedded; `migrationsFor(backend)` in `schema.go` selects the set; `RunMigrations(db, backend)` applies them via `backend.ApplyMigration`. Latest is `0010_oidc.sql`; next is **0011**. Each file ends with `INSERT INTO schema_version (version, applied_at) VALUES (N, <now>)` where `<now>` is `strftime('%s','now')` (sqlite) / `EXTRACT(EPOCH FROM now())::bigint` (postgres).
- Webhook claim: `internal/webhooks/worker.go` `claim(ctx, db sqlitestore.Querier, batch int) ([]claimedRow, error)` uses `db.RunInTx` SELECT-then-UPDATE. `claimedRow` embeds `DeliveryRow{ID string, EndpointID int64, EventType string, PayloadJSON []byte, Status string, Attempts int, NextAttemptAt time.Time}` + `URL string` + `Secret string`. The Scan reads `ID, EndpointID, EventType, PayloadJSON, Attempts, URL, Secret` (7 columns). `Reclaim(ctx, db, threshold)` flips in_flight→pending.
- Quota: `internal/lfs/quota/quota.go` `Service{db sqlitestore.Querier, logger, ring *addRing}`, `New(db, logger)`. `Add` holds `ring` mutex + `Seen`/`Record` then `UPDATE quotas SET used_bytes = used_bytes + ?`. `Subtract` decrements via `s.db.Greatest("used_bytes - ?", "0")` then `ring.Forget`. The `addRing` struct + `newAddRing` + its methods (`Lock`/`Unlock`/`Seen`/`Record`/`Forget`) are all in quota.go.
- B1 added `internal/auth/sqlitestore/migrations_pg_test.go` which asserts **exactly 10** postgres migration files — this MUST be bumped to 11 in Task 4.
- CI: `.github/workflows/ci.yml` has a `postgres-conformance` job pinned to `postgres:17`; `.github/workflows/conformance.yml` has a `postgres` job pinned to `postgres:17`. Both move to a `14`+`18` matrix in Task 5.

## File Structure

- **Modify** `internal/auth/sqlitestore/backend.go` — add `SupportsSkipLocked()` to the interface + the 3 impls; add the `Option`/`WithMaxConns` plumbing + thread max-conns into `resolveBackend`/`newPostgresBackend`.
- **Modify** `internal/auth/sqlitestore/backend_libsql.go`, `backend_postgres.go` — `SupportsSkipLocked()` impls; postgres honors max-conns.
- **Modify** `internal/auth/sqlitestore/querier.go` — expose `SupportsSkipLocked()` on `Querier`/`dbWrap`/`txWrap`.
- **Modify** `internal/auth/sqlitestore/store.go` — `Open(value string, opts ...Option)`.
- **Create** `internal/auth/sqlitestore/migrations/0011_quota_credits.sql` + `migrations_postgres/0011_quota_credits.sql`.
- **Modify** `internal/auth/sqlitestore/migrations_pg_test.go` — expected count 10→11.
- **Modify** `internal/webhooks/worker.go` — branch `claim` on `SupportsSkipLocked`; add `claimSkipLocked`; rename current body to `claimSerialized`.
- **Modify** `internal/lfs/quota/quota.go` — rewrite `Add`/`Subtract` to use `quota_credits`; delete `addRing` + `newAddRing` + the `ring` field.
- **Modify** `cmd/bucketvcs/authdb.go` (`openAuthDB` variadic) + `cmd/bucketvcs/serve.go` (`--auth-db-max-conns`).
- **Modify** `.github/workflows/ci.yml`, `.github/workflows/conformance.yml` — PG 14+18 matrix.
- **Modify** `internal/auth/sqlitestore/conformance_pg_test.go` — concurrency tests.
- **Create** `docs/m23-b2-multinode-operator-guide.md`; **modify** `docs/m23-b1-postgres-operator-guide.md` (retract single-node caveat).

---

## Task 1: `SupportsSkipLocked` backend capability

**Files:**
- Modify: `internal/auth/sqlitestore/backend.go`, `backend_libsql.go`, `backend_postgres.go`, `querier.go`
- Test: `internal/auth/sqlitestore/backend_dialect_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/auth/sqlitestore/backend_dialect_test.go`:
```go
func TestSupportsSkipLocked(t *testing.T) {
	if (sqliteBackend{}).SupportsSkipLocked() {
		t.Fatal("sqlite must not support SKIP LOCKED")
	}
	if (libsqlBackend{}).SupportsSkipLocked() {
		t.Fatal("libsql must not support SKIP LOCKED")
	}
	if !(postgresBackend{}).SupportsSkipLocked() {
		t.Fatal("postgres must support SKIP LOCKED")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run TestSupportsSkipLocked -v`
Expected: FAIL (compile error — method undefined).

- [ ] **Step 3: Add to the `Backend` interface**

In `backend.go`, add to the `Backend` interface (after `InsertReturningID`):
```go
	// SupportsSkipLocked reports whether the backend supports
	// SELECT … FOR UPDATE SKIP LOCKED concurrent row claiming. true for
	// postgres; false for sqlite/libsql (single-writer).
	SupportsSkipLocked() bool
```

- [ ] **Step 4: Implement on the three backends**

In `backend.go` (sqlite): `func (sqliteBackend) SupportsSkipLocked() bool { return false }`
In `backend_libsql.go`: `func (libsqlBackend) SupportsSkipLocked() bool { return false }`
In `backend_postgres.go`: `func (postgresBackend) SupportsSkipLocked() bool { return true }`

- [ ] **Step 5: Expose on `Querier`**

In `querier.go`, add `SupportsSkipLocked() bool` to the `Querier` interface, and:
```go
func (w *dbWrap) SupportsSkipLocked() bool { return w.backend.SupportsSkipLocked() }
func (w *txWrap) SupportsSkipLocked() bool { return w.backend.SupportsSkipLocked() }
```
(Add to `txWrap` too for symmetry even though claim uses the `dbWrap`.)

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/auth/sqlitestore/ -run TestSupportsSkipLocked -v && go build ./...`
Expected: PASS; build OK.

- [ ] **Step 7: Commit**

```bash
git add internal/auth/sqlitestore/backend.go internal/auth/sqlitestore/backend_libsql.go internal/auth/sqlitestore/backend_postgres.go internal/auth/sqlitestore/querier.go internal/auth/sqlitestore/backend_dialect_test.go
git commit -m "M23 B2: SupportsSkipLocked backend capability (postgres true; sqlite/libsql false)"
```

---

## Task 2: Connection pool sizing (`Open` options + `--auth-db-max-conns`)

**Files:**
- Modify: `internal/auth/sqlitestore/backend.go` (Option type, resolveBackend, postgres SetMaxOpenConns), `store.go` (Open variadic), `backend_postgres.go`
- Modify: `cmd/bucketvcs/authdb.go`, `cmd/bucketvcs/serve.go`
- Test: `internal/auth/sqlitestore/backend_postgres_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/auth/sqlitestore/backend_postgres_test.go`:
```go
func TestWithMaxConns(t *testing.T) {
	// Default (no option) → postgres uses 10.
	b, err := resolveBackend("postgres://u@h/db")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.(postgresBackend).maxConns; got != 10 {
		t.Fatalf("default maxConns = %d, want 10", got)
	}
	// Explicit option overrides.
	b2, err := resolveBackend("postgres://u@h/db", WithMaxConns(25))
	if err != nil {
		t.Fatal(err)
	}
	if got := b2.(postgresBackend).maxConns; got != 25 {
		t.Fatalf("maxConns = %d, want 25", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run TestWithMaxConns -v`
Expected: FAIL (compile error — `WithMaxConns`/`maxConns`/variadic `resolveBackend` undefined).

- [ ] **Step 3: Add the `Option` type + thread it through**

In `backend.go`, add near the top (after the `Backend` interface):
```go
// options carries Open-time configuration resolved from functional Options.
type options struct {
	maxConns int // Postgres pool size; 0 means default (10). Ignored by sqlite/libsql.
}

// Option configures Open/resolveBackend.
type Option func(*options)

// WithMaxConns sets the Postgres connection-pool size (SetMaxOpenConns).
// Ignored by sqlite/libsql, which always use a single connection.
func WithMaxConns(n int) Option { return func(o *options) { o.maxConns = n } }

const defaultPostgresMaxConns = 10
```
Change `resolveBackend` to accept options and pass the resolved max-conns to the postgres backend:
```go
func resolveBackend(value string, opts ...Option) (Backend, error) {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	if isPostgresValue(value) {
		return newPostgresBackend(value, o.maxConns)
	}
	if isLibsqlValue(value) {
		return newLibsqlBackend(value)
	}
	return sqliteBackend{path: sqlitePath(value)}, nil
}
```

- [ ] **Step 4: Postgres backend honors max-conns**

In `backend_postgres.go`: add a `maxConns int` field to `postgresBackend`; change `newPostgresBackend` to `func newPostgresBackend(rawURL string, maxConns int) (Backend, error)` and set the field (`if maxConns <= 0 { maxConns = defaultPostgresMaxConns }`) before returning `postgresBackend{dsn: ..., maxConns: maxConns}`. In `Open`, replace `db.SetMaxOpenConns(1)` with `db.SetMaxOpenConns(b.maxConns)`. (sqlite/libsql `Open` keep `SetMaxOpenConns(1)` unchanged.)

- [ ] **Step 5: `Open` variadic**

In `store.go`, change `func Open(value string) (*Store, error)` to `func Open(value string, opts ...Option) (*Store, error)` and pass `opts...` to `resolveBackend(value, opts...)`. Everything else in `Open` is unchanged. (Existing `Open(value)` callers compile unchanged — variadic.)

- [ ] **Step 6: Plumb through `openAuthDB` + serve flag**

In `cmd/bucketvcs/authdb.go`, change `openAuthDB` to forward options:
```go
func openAuthDB(flag string, opts ...sqlitestore.Option) (*sqlitestore.Store, string, error) {
	path, err := resolveAuthDB(flag, realEnv())
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", err
	}
	s, err := sqlitestore.Open(path, opts...)
	if err != nil {
		return nil, "", err
	}
	return s, path, nil
}
```
(Existing `openAuthDB("")` / `openAuthDB(*authDB)` callers are unchanged — variadic.)
In `cmd/bucketvcs/serve.go`, add a flag near the other auth flags (after line ~64):
```go
	authDBMaxConns := fs.Int("auth-db-max-conns", 10, "Max DB connections for the auth/metadata DB (Postgres only; sqlite/libsql always use 1)")
```
and change the open call at line 221 from `openAuthDB(*authDB)` to:
```go
	authS, _, err := openAuthDB(*authDB, sqlitestore.WithMaxConns(*authDBMaxConns))
```

- [ ] **Step 7: Run tests + build**

Run: `go test ./internal/auth/sqlitestore/ -run 'TestWithMaxConns|TestPostgres|TestResolveBackend' -v && go build ./... && go test ./internal/... ./cmd/... 2>&1 | tail -8`
Expected: PASS; build OK; full suite green.

- [ ] **Step 8: Commit**

```bash
git add internal/auth/sqlitestore/backend.go internal/auth/sqlitestore/backend_postgres.go internal/auth/sqlitestore/store.go internal/auth/sqlitestore/backend_postgres_test.go cmd/bucketvcs/authdb.go cmd/bucketvcs/serve.go
git commit -m "M23 B2: configurable pool size (Open WithMaxConns + --auth-db-max-conns; PG default 10, sqlite/libsql forced 1)"
```

---

## Task 3: Webhook claim — `FOR UPDATE SKIP LOCKED` (Postgres)

**Files:**
- Modify: `internal/webhooks/worker.go`

- [ ] **Step 1: Rename the current claim body to `claimSerialized` and add the branch**

In `internal/webhooks/worker.go`, rename the existing `func claim(...)` to `func claimSerialized(...)` (same body), then add a new dispatcher:
```go
// claim transitions up to batch deliveries pending → in_flight, returning them
// with their endpoint URL/secret. Postgres uses FOR UPDATE SKIP LOCKED so
// multiple gateway nodes never claim the same row; sqlite/libsql use the
// serialized SELECT-then-UPDATE (single-writer).
func claim(ctx context.Context, db sqlitestore.Querier, batch int) ([]claimedRow, error) {
	if db.SupportsSkipLocked() {
		return claimSkipLocked(ctx, db, batch)
	}
	return claimSerialized(ctx, db, batch)
}
```

- [ ] **Step 2: Add the Postgres claim**

Add to `worker.go`:
```go
// claimSkipLocked claims rows in a single atomic UPDATE … RETURNING using
// FOR UPDATE SKIP LOCKED in the row-selection subquery. Safe for concurrent
// claimers across nodes. Postgres-only syntax (gated by SupportsSkipLocked).
func claimSkipLocked(ctx context.Context, db sqlitestore.Querier, batch int) ([]claimedRow, error) {
	now := time.Now().Unix()
	rows, err := db.QueryContext(ctx, `
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
		RETURNING d.id, d.endpoint_id, d.event_type, d.payload_json, d.attempts, e.url, e.secret`,
		now, now, batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimedRow
	for rows.Next() {
		var r claimedRow
		if err := rows.Scan(&r.ID, &r.EndpointID, &r.EventType, &r.PayloadJSON, &r.Attempts, &r.URL, &r.Secret); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
```
(The three `?` rebind to `$1,$2,$3` = now, now, batch — in source order: SET last_attempt_at, next_attempt_at filter, LIMIT. The Scan column order matches the current `claimSerialized` scan.)

- [ ] **Step 3: Build + sqlite suite**

Run: `go build ./... && go test ./internal/webhooks/ -v 2>&1 | tail -15`
Expected: build OK; webhooks tests PASS (they run on sqlite → `claimSerialized` path, unchanged behavior).

- [ ] **Step 4: Commit**

```bash
git add internal/webhooks/worker.go
git commit -m "M23 B2: Postgres webhook claim via FOR UPDATE SKIP LOCKED (sqlite/libsql keep serialized claim)"
```

---

## Task 4: Quota idempotency — `quota_credits` table

**Files:**
- Create: `internal/auth/sqlitestore/migrations/0011_quota_credits.sql`, `internal/auth/sqlitestore/migrations_postgres/0011_quota_credits.sql`
- Modify: `internal/auth/sqlitestore/migrations_pg_test.go`, `internal/lfs/quota/quota.go`
- Test: `internal/lfs/quota/quota_test.go`

- [ ] **Step 1: Create the sqlite migration**

`internal/auth/sqlitestore/migrations/0011_quota_credits.sql`:
```sql
-- quota_credits records each counted (tenant, oid) LFS upload so that
-- verify-replay (the same upload arriving twice, possibly on different gateway
-- nodes) increments used_bytes exactly once. The unique PK is the cross-node
-- idempotency point. Rows are removed on sweep/delete (Subtract), keeping the
-- table bounded to currently-counted objects.
CREATE TABLE quota_credits (
    tenant      TEXT    NOT NULL,
    oid         TEXT    NOT NULL,
    bytes       INTEGER NOT NULL,
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY (tenant, oid)
);

INSERT INTO schema_version (version, applied_at) VALUES (11, strftime('%s','now'));
```

- [ ] **Step 2: Create the postgres migration**

`internal/auth/sqlitestore/migrations_postgres/0011_quota_credits.sql`: identical except the footer:
```sql
-- quota_credits records each counted (tenant, oid) LFS upload so that
-- verify-replay (the same upload arriving twice, possibly on different gateway
-- nodes) increments used_bytes exactly once. The unique PK is the cross-node
-- idempotency point. Rows are removed on sweep/delete (Subtract), keeping the
-- table bounded to currently-counted objects.
CREATE TABLE quota_credits (
    tenant      TEXT    NOT NULL,
    oid         TEXT    NOT NULL,
    bytes       INTEGER NOT NULL,
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY (tenant, oid)
);

INSERT INTO schema_version (version, applied_at) VALUES (11, EXTRACT(EPOCH FROM now())::bigint);
```

- [ ] **Step 3: Bump the migration-count test**

In `internal/auth/sqlitestore/migrations_pg_test.go`, change the expected count from 10 to 11 (the `if len(entries) != 10` / `got %d` assertion → `11`).

- [ ] **Step 4: Rewrite `Add` to gate on `quota_credits`**

In `internal/lfs/quota/quota.go`, replace the `Add` method body with:
```go
func (s *Service) Add(ctx context.Context, tenant, oid string, bytes int64) error {
	if bytes < 0 {
		return fmt.Errorf("quota: bytes must be >= 0 (got %d)", bytes)
	}
	if bytes == 0 {
		return nil
	}
	now := time.Now().Unix()
	return s.db.RunInTx(ctx, func(tx sqlitestore.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO quota_credits (tenant, oid, bytes, recorded_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (tenant, oid) DO NOTHING`,
			tenant, oid, bytes, now)
		if err != nil {
			return fmt.Errorf("quota add %q oid=%s: credit: %w", tenant, oid, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("quota add %q oid=%s: rows affected: %w", tenant, oid, err)
		}
		if n == 0 {
			return nil // already credited (this node or another) — idempotent no-op
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE quotas SET used_bytes = used_bytes + ?, updated_at = ?
			WHERE tenant = ?`,
			bytes, now, tenant); err != nil {
			return fmt.Errorf("quota add %q oid=%s: increment: %w", tenant, oid, err)
		}
		return nil
	})
}
```

- [ ] **Step 5: Rewrite `Subtract` to gate on the credit row**

Replace the `Subtract` method body with:
```go
func (s *Service) Subtract(ctx context.Context, tenant, oid string, bytes int64) error {
	if bytes < 0 {
		return fmt.Errorf("quota: bytes must be >= 0 (got %d)", bytes)
	}
	if bytes == 0 {
		return nil
	}
	now := time.Now().Unix()
	clamp := s.db.Greatest("used_bytes - ?", "0")
	return s.db.RunInTx(ctx, func(tx sqlitestore.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM quota_credits WHERE tenant = ? AND oid = ?`, tenant, oid)
		if err != nil {
			return fmt.Errorf("quota subtract %q oid=%s: uncredit: %w", tenant, oid, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("quota subtract %q oid=%s: rows affected: %w", tenant, oid, err)
		}
		if n == 0 {
			return nil // not credited — nothing to subtract (idempotent; reconcile is the backstop)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE quotas SET used_bytes = `+clamp+`, updated_at = ? WHERE tenant = ?`,
			bytes, now, tenant); err != nil {
			return fmt.Errorf("quota subtract %q oid=%s: decrement: %w", tenant, oid, err)
		}
		return nil
	})
}
```

- [ ] **Step 6: Delete the in-process ring**

In `internal/lfs/quota/quota.go`: remove the `ring *addRing` field from `Service`; remove `ring: newAddRing(1024)` from `New`; delete the `addRing` struct, `newAddRing`, and the `Lock`/`Unlock`/`Seen`/`Record`/`Forget` methods. Remove the now-unused `sync` import if nothing else uses it (grep first).

- [ ] **Step 7: Update quota tests for credit-gated semantics**

In `internal/lfs/quota/quota_test.go`, the test DB must now have the `quota_credits` table — confirm `openTestDB` runs `RunMigrations` (it opens a real `sqlitestore`, so 0011 applies automatically). Add/adjust a test asserting Add idempotency via the table:
```go
func TestAddIdempotentViaCredits(t *testing.T) {
	svc, _ := newQuotaTestService(t) // existing helper; adjust name to match the file
	ctx := context.Background()
	if err := svc.Set(ctx, "acme", 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := svc.Add(ctx, "acme", "oidA", 100); err != nil {
		t.Fatal(err)
	}
	if err := svc.Add(ctx, "acme", "oidA", 100); err != nil { // replay
		t.Fatal(err)
	}
	st, err := svc.Get(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if st.UsedBytes != 100 {
		t.Fatalf("used=%d want 100 (replay must not double-count)", st.UsedBytes)
	}
	// Subtract removes the credit and decrements once; a second subtract is a no-op.
	if err := svc.Subtract(ctx, "acme", "oidA", 100); err != nil {
		t.Fatal(err)
	}
	if err := svc.Subtract(ctx, "acme", "oidA", 100); err != nil {
		t.Fatal(err)
	}
	st, _ = svc.Get(ctx, "acme")
	if st.UsedBytes != 0 {
		t.Fatalf("used=%d want 0", st.UsedBytes)
	}
}
```
(Adjust the helper/constructor names and the `State` field name `UsedBytes` to match the actual code in `quota.go`/`quota_test.go` — grep `func.*Get` and the `State` struct first.)

- [ ] **Step 8: Build + test**

Run: `gofmt -l internal/lfs/quota/ internal/auth/sqlitestore/ && go build ./... && go test ./internal/lfs/quota/ ./internal/auth/sqlitestore/ -v 2>&1 | tail -20`
Expected: gofmt clean; build OK; quota + sqlitestore tests PASS (0011 applies; ring removed; idempotency now via the table).

- [ ] **Step 9: Commit**

```bash
git add internal/auth/sqlitestore/migrations/0011_quota_credits.sql internal/auth/sqlitestore/migrations_postgres/0011_quota_credits.sql internal/auth/sqlitestore/migrations_pg_test.go internal/lfs/quota/quota.go internal/lfs/quota/quota_test.go
git commit -m "M23 B2: quota_credits table for cross-node verify-replay idempotency; remove in-process dedup ring"
```

---

## Task 5: PostgreSQL 14+ CI version matrix

**Files:**
- Modify: `.github/workflows/ci.yml`, `.github/workflows/conformance.yml`

- [ ] **Step 1: Matrix the per-commit job**

In `.github/workflows/ci.yml`, change the `postgres-conformance` job to a matrix. Replace its `runs-on:` + `services:` header so it reads:
```yaml
  postgres-conformance:
    name: postgres conformance (pg${{ matrix.pg }})
    runs-on: ubuntu-latest
    timeout-minutes: 15
    strategy:
      fail-fast: false
      matrix:
        pg: ["14", "18"]
    services:
      postgres:
        image: postgres:${{ matrix.pg }}
        env:
          POSTGRES_PASSWORD: pw
          POSTGRES_DB: bv
        ports:
          - 5432:5432
        options: >-
          --health-cmd "pg_isready -U postgres"
          --health-interval 5s
          --health-timeout 5s
          --health-retries 10
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.25"
          cache: true
      - name: Postgres conformance
        env:
          BUCKETVCS_POSTGRES_URL: "postgres://postgres@127.0.0.1:5432/bv?sslmode=disable"
          BUCKETVCS_DB_AUTH_TOKEN: "pw"
        run: go test -tags postgres -count=1 ./internal/auth/sqlitestore/ -run TestPostgresConformance -v
```

- [ ] **Step 2: Matrix the nightly job**

In `.github/workflows/conformance.yml`, apply the same change to the `postgres` job: add `strategy: {fail-fast: false, matrix: {pg: ["14","18"]}}`, set `name: postgres conformance (pg${{ matrix.pg }})`, and `image: postgres:${{ matrix.pg }}`. Keep its existing steps/env.

- [ ] **Step 3: Validate YAML**

Run:
```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml')); yaml.safe_load(open('.github/workflows/conformance.yml')); print('yaml ok')"
```
Expected: `yaml ok`.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml .github/workflows/conformance.yml
git commit -m "M23 B2: Postgres CI matrix 14 + 18 (per-commit + nightly) for the 14+ support promise"
```

---

## Task 6: Concurrency conformance tests (gated, live Postgres)

**Files:**
- Modify: `internal/auth/sqlitestore/conformance_pg_test.go`

These prove the multi-node guarantees against a live Postgres with pool>1. They are `//go:build postgres` gated (the file already has the tag) and run in the 14+18 matrix.

- [ ] **Step 1: Add the concurrency tests**

Append to `internal/auth/sqlitestore/conformance_pg_test.go`. NOTE: this file is `package sqlitestore` and cannot import `internal/webhooks`/`internal/lfs/quota` (they import sqlitestore — cycle). So exercise the behaviors via direct SQL through a pool-of->1 store handle (the `*Store.DB()` Querier), which is exactly what those packages run. Use `openPostgresMaxConns` to get a pool>1.

```go
// openPostgresMaxConns opens the live PG store with a pool > 1 so concurrent
// goroutines genuinely use multiple connections.
func openPostgresMaxConns(t *testing.T, n int) *Store {
	t.Helper()
	url := os.Getenv("BUCKETVCS_POSTGRES_URL")
	if url == "" {
		t.Skip("BUCKETVCS_POSTGRES_URL not set")
	}
	s, err := Open(url, WithMaxConns(n))
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPGConcurrentWebhookClaim seeds N pending deliveries and claims them from
// two concurrent goroutines using FOR UPDATE SKIP LOCKED; asserts every row is
// claimed exactly once (no double-claim).
func TestPGConcurrentWebhookClaim(t *testing.T) {
	s := openPostgresMaxConns(t, 4)
	ctx := context.Background()
	db := s.DB()

	// Seed one endpoint + 50 pending deliveries.
	var epID int64
	if err := db.RunInTx(ctx, func(tx Tx) error {
		id, e := tx.InsertReturningID(ctx,
			`INSERT INTO webhook_endpoints (tenant, repo, url, secret, active, created_at)
			 VALUES ('t','r','http://x','s',1,0)`)
		epID = id
		return e
	}); err != nil {
		t.Fatal(err)
	}
	const total = 50
	for i := 0; i < total; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO webhook_deliveries
			   (id, endpoint_id, event_type, payload_json, status, attempts, next_attempt_at, created_at)
			 VALUES (?, ?, 'push', '\x00', 'pending', 0, 0, 0)`,
			fmtID(i), epID); err != nil {
			t.Fatal(err)
		}
	}

	claimQ := `
		UPDATE webhook_deliveries d
		   SET status='in_flight', last_attempt_at=0, attempts=d.attempts+1
		  FROM webhook_endpoints e
		 WHERE e.id = d.endpoint_id
		   AND d.id IN (
		       SELECT d2.id FROM webhook_deliveries d2
		         JOIN webhook_endpoints e2 ON e2.id = d2.endpoint_id
		        WHERE d2.status='pending' AND d2.next_attempt_at <= 9999999999 AND e2.active=1
		        ORDER BY d2.next_attempt_at LIMIT 7 FOR UPDATE SKIP LOCKED)
		RETURNING d.id`

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				rows, err := db.QueryContext(ctx, claimQ)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				got := 0
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						t.Errorf("scan: %v", err)
						return
					}
					got++
					mu.Lock()
					seen[id]++
					mu.Unlock()
				}
				rows.Close()
				if got == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("claimed %d distinct rows, want %d", len(seen), total)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("row %s claimed %d times (want 1)", id, c)
		}
	}
}

// TestPGConcurrentQuotaCredit increments the same (tenant, oid) from two
// goroutines via the quota_credits ON CONFLICT gate; asserts used_bytes lands
// at exactly one increment.
func TestPGConcurrentQuotaCredit(t *testing.T) {
	s := openPostgresMaxConns(t, 4)
	ctx := context.Background()
	db := s.DB()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO quotas (tenant, limit_bytes, used_bytes, updated_at)
		 VALUES ('qt', 1000000, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	addOnce := func() error {
		return db.RunInTx(ctx, func(tx Tx) error {
			res, err := tx.ExecContext(ctx,
				`INSERT INTO quota_credits (tenant, oid, bytes, recorded_at)
				 VALUES ('qt','oid1',100,0) ON CONFLICT (tenant,oid) DO NOTHING`)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				return nil
			}
			_, err = tx.ExecContext(ctx,
				`UPDATE quotas SET used_bytes = used_bytes + 100 WHERE tenant='qt'`)
			return err
		})
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = addOnce() }()
	}
	wg.Wait()

	var used int64
	if err := db.QueryRowContext(ctx, `SELECT used_bytes FROM quotas WHERE tenant='qt'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 100 {
		t.Fatalf("used_bytes=%d want 100 (concurrent replay must count once)", used)
	}
}

// TestPGConcurrentRename runs two concurrent renames of the same repo; exactly
// one succeeds and the other returns an error, with no orphaned permission rows.
func TestPGConcurrentRename(t *testing.T) {
	s := openPostgresMaxConns(t, 4)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "ru", false); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterRepo(ctx, "rt", "src"); err != nil {
		t.Fatal(err)
	}
	if err := s.Grant(ctx, "ru", "rt", "src", "write"); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.RenameRepo(ctx, "rt", "src", "dst") }()
	}
	wg.Wait()
	close(errs)
	ok, failed := 0, 0
	for e := range errs {
		if e == nil {
			ok++
		} else {
			failed++
		}
	}
	if ok != 1 || failed != 1 {
		t.Fatalf("concurrent rename: ok=%d failed=%d, want 1/1", ok, failed)
	}
	u, _ := s.GetUserByName(ctx, "ru")
	actor := &auth.Actor{UserID: u.ID, Name: "ru"}
	if perm, _ := s.LookupRepoPerm(ctx, actor, "rt", "dst"); perm != auth.PermWrite {
		t.Fatalf("after rename perm on dst=%v want write", perm)
	}
	if perm, _ := s.LookupRepoPerm(ctx, actor, "rt", "src"); perm != auth.PermNone {
		t.Fatalf("src perm=%v want none (no orphan)", perm)
	}
}

func fmtID(i int) string { return "dlv" + strconv.Itoa(i) }
```
Add imports to the file as needed: `"strconv"`, `"sync"` (and `"github.com/bucketvcs/bucketvcs/internal/auth"` is already imported). Verify `RenameRepo`, `CreateUser`, `RegisterRepo`, `Grant`, `LookupRepoPerm`, `GetUserByName` signatures against the source (they were verified in B1; unchanged).

NOTE on the seed payload `'\x00'`: if Postgres rejects that BYTEA literal, use `decode('00','hex')` or a parameterized `?` arg with `[]byte{0}` instead. Confirm during the live run.

- [ ] **Step 2: Build under the tag + skip without URL**

Run: `go test -tags postgres ./internal/auth/sqlitestore/ -run 'TestPGConcurrent' -v`
Expected: compiles, SKIPs (no URL). Also confirm default build unaffected: `go test ./internal/auth/sqlitestore/`.

- [ ] **Step 3: Run against live Postgres (both 14 and 18)**

```bash
for V in 14 18; do
  docker rm -f bv-pg >/dev/null 2>&1
  docker run -d --name bv-pg -p 5440:5432 -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=bv postgres:$V >/dev/null 2>&1
  for i in $(seq 1 30); do docker exec bv-pg pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
  echo "=== postgres:$V ==="
  BUCKETVCS_POSTGRES_URL="postgres://postgres@127.0.0.1:5440/bv?sslmode=disable" BUCKETVCS_DB_AUTH_TOKEN="pw" \
    go test -tags postgres -count=1 ./internal/auth/sqlitestore/ -run 'TestPostgresConformance|TestPGConcurrent' -v 2>&1 | tail -25
  docker rm -f bv-pg >/dev/null 2>&1
done
```
Expected: PASS on both 14 and 18. If the BYTEA seed literal fails, switch to a parameterized `[]byte{0}` arg as noted. If a concurrency assertion fails, the corresponding production path has a real race — fix it before proceeding.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/sqlitestore/conformance_pg_test.go
git commit -m "M23 B2: live-Postgres concurrency conformance (claim, quota credit, rename) under pool>1"
```

---

## Task 7: Operator guide (multi-node) + retract B1 single-node caveats

**Files:**
- Create: `docs/m23-b2-multinode-operator-guide.md`
- Modify: `docs/m23-b1-postgres-operator-guide.md`

- [ ] **Step 1: Write the B2 guide**

Create `docs/m23-b2-multinode-operator-guide.md` in the style of `docs/m23-b1-postgres-operator-guide.md`, covering:
- **What it is:** Postgres-backed metadata DB is now safe across multiple gateway nodes (pool>1). SQLite/libSQL remain single-node.
- **Enabling multi-node:** run N gateway nodes against one `postgres://…` DB; set `--auth-db-max-conns` per node (default 10) to size each node's pool. No leader election or extra coordination needed.
- **What is now race-safe:** webhook delivery (each delivered once via `FOR UPDATE SKIP LOCKED`), LFS quota counting (each upload counted once via `quota_credits`), repo rename (serialized by Postgres).
- **Caveats:** (1) rate limiting is per-node (effective burst ≈ N×Burst) — front with a proxy/LB for a global limit; (2) sqlite/libsql stay single-node regardless of `--auth-db-max-conns`; (3) **upgrade note:** objects counted before B2 have no `quota_credits` rows, so their later deletion will not auto-decrement `used_bytes` — run `bucketvcs quota reconcile` after upgrading (and periodically) to correct drift.
- **Supported versions:** PostgreSQL 14+ (CI tests 14 and 18).
- **Verifying:** startup log `authdb opened backend=postgres`; the nightly + per-commit `postgres conformance (pg14/pg18)` jobs prove multi-node safety.

- [ ] **Step 2: Retract the B1 single-node caveat**

In `docs/m23-b1-postgres-operator-guide.md`, update the prominent "single-node only in B1" caveat to note that **B2 lifts it for Postgres** (multi-node is now supported; see the B2 guide), and that `MaxOpenConns` is now configurable via `--auth-db-max-conns`. Leave the SQLite/libSQL single-node statements intact.

- [ ] **Step 3: Commit**

```bash
git add docs/m23-b2-multinode-operator-guide.md docs/m23-b1-postgres-operator-guide.md
git commit -m "M23 B2: multi-node operator guide; retract B1 single-node Postgres caveat"
```

---

## Final verification

- [ ] **Per-commit suite + build + cross-compile**

Run:
```bash
go build ./... && go test ./internal/... ./cmd/... && go vet ./internal/... ./cmd/...
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -o /dev/null ./cmd/bucketvcs && echo "OK $t"; done
gofmt -l internal/auth/sqlitestore/ internal/webhooks/ internal/lfs/quota/ cmd/bucketvcs/
```
Expected: all PASS; all five `OK`; gofmt prints nothing for the touched dirs.

- [ ] **Gated Postgres suite (14 + 18)** — as in Task 6 Step 3: PASS on both.

- [ ] **libSQL conformance still green** — `BUCKETVCS_LIBSQL_URL` run still PASSES (claim/quota unchanged for libsql).

- [ ] **Update memory index** — add `m23_b2_progress.md` topic + short MEMORY.md line; update `ci-backend-coverage.md` (PG matrix 14+18) once merged.

---

## Self-review notes (for the implementer)

- **Spec coverage:** SupportsSkipLocked (Task 1) ↔ §3; pool sizing (Task 2) ↔ §5; SKIP LOCKED claim (Task 3) ↔ §3; quota_credits (Task 4) ↔ §4; CI matrix (Task 5) ↔ §6; concurrency tests (Task 6) ↔ §10.2; operator guide + retraction (Task 7) ↔ §1.1/§8; rate-limiter (no code) ↔ §8; rename safety (Task 6 test) ↔ §7.
- **`Open(value)` stays backward-compatible** (variadic options) — re-verify CLI callers compile after Task 2.
- **The conformance tests run direct SQL** (not the webhooks/quota packages) to avoid the import cycle (those packages import sqlitestore). They replicate the exact production statements — keep them in sync if Task 3/4 SQL changes.
- **Upgrade transition:** pre-B2 objects have no credit rows → `Subtract` won't decrement them; `quota reconcile` is the documented backstop (Task 7 Step 1, caveat 3).
- **No new dependencies** → `CGO_ENABLED=0` cross-build stays green.
- **Known follow-on:** distributed rate limiting; read-replica routing; package rename — all still deferred.
