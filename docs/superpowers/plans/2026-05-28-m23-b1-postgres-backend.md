# M23 B1: PostgreSQL metadata backend + dialect layer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a single-node PostgreSQL backend to the `internal/auth/sqlitestore` metadata store, selected by the `--auth-db` URL scheme, by extending the M23 Phase A `Backend` seam with a SQL-dialect layer (placeholder rebinding, SQLSTATE error classification, a few divergent-construct helpers, and a Postgres migration set).

**Architecture:** The existing `?`-flavored SQL stays single-sourced; a `Querier` wrapper rebinds `?`→`$N` at one chokepoint so the store and its 6 sibling packages need no per-call edits. A `postgresBackend` (pgx via the `database/sql` stdlib adapter, pure-Go) implements the extended `Backend` interface. Postgres uses `MaxOpenConns(1)` — single-node, exactly the Phase A (libSQL) posture; multi-node hardening is the separate B2.

**Tech Stack:** Go 1.25, `github.com/jackc/pgx/v5` (stdlib adapter, pure-Go, `CGO_ENABLED=0`), `database/sql`, the existing `splitSQLStatements` splitter, dockerized `postgres:17` for nightly conformance.

**Spec:** `docs/superpowers/specs/2026-05-28-m23-b1-postgres-backend-design.md`

---

## Background the implementer needs

- The store package is `internal/auth/sqlitestore`. It hosts a `Backend` interface (Phase A) with `Name() string`, `Open() (*sql.DB, error)`, `ApplyMigration(tx *sql.Tx, body string) error`, two implementations (`sqliteBackend`, `libsqlBackend`), and `resolveBackend(value string) (Backend, error)` selecting by URL scheme. See `internal/auth/sqlitestore/backend.go`.
- `Store` is `struct { db *sql.DB; backend Backend }`. `Open(value)` resolves the backend, opens the DB, runs migrations, returns `*Store`. `Store.DB() *sql.DB` exposes the handle to 6 sibling packages that write SQL against the same authdb: `internal/webhooks`, `internal/policy`, `internal/hooks`, `internal/lfs/locks`, `internal/lfs/quota`. Each takes `db *sql.DB` in its `New(...)`/`NewStore(...)` constructor.
- All SQL uses `?` positional placeholders. Postgres needs `$1,$2,…`.
- Error classification today: free functions `isUniqueViolation`, `isCheckViolation`, `isFingerprintUniqueViolation` in `store.go` (substring matching), plus byte-identical copies of `isUniqueViolation` in `internal/webhooks/service.go` and `internal/lfs/locks/store.go`.
- Transactions are used in exactly two places: `internal/webhooks/worker.go` `claim(...)` (`db.BeginTx`) and `internal/auth/sqlitestore/rename.go` `RenameRepo` (`s.db.BeginTx`, plus `PRAGMA defer_foreign_keys = TRUE`).
- Timestamps are unix int64 in INTEGER columns. Most are produced Go-side via `time.Now().Unix()`; ~4 SQL sites use `strftime('%s','now')`: `RevokeSSHKey`, `TouchSSHKeyUsage` (store.go), `AddOIDCIssuer`, `AddOIDCRule` (oidc.go).
- `internal/lfs/quota/quota.go` `Subtract` uses scalar `MAX(used_bytes - ?, 0)` (SQLite scalar form; Postgres needs `GREATEST`).
- `internal/webhooks/service.go` inserts `webhook_endpoints` and reads the new id via `res.LastInsertId()` (Postgres needs `RETURNING id`).
- The 10 SQLite migrations are embedded from `internal/auth/sqlitestore/migrations/*.sql`.

## File Structure

- **Modify** `internal/auth/sqlitestore/backend.go` — extend `Backend` interface (Rebind, classifiers, dialect helpers); implement the new methods on `sqliteBackend`; extend `resolveBackend`.
- **Create** `internal/auth/sqlitestore/querier.go` — `Querier` interface, `dbWrap`, `txWrap` (rebinding handles + classifier/helper delegation).
- **Create** `internal/auth/sqlitestore/backend_postgres.go` — `postgresBackend`.
- **Create** `internal/auth/sqlitestore/migrations_postgres/*.sql` (10 files) + embed.
- **Modify** `internal/auth/sqlitestore/backend_libsql.go` — implement the new interface methods (identity/sqlite-form).
- **Modify** `internal/auth/sqlitestore/store.go` — `Store.db` becomes `*dbWrap`; `Store.DB()` returns `Querier`; route classifier calls through `s.backend`; route divergent SQL through helpers.
- **Modify** `internal/auth/sqlitestore/oidc.go`, `rename.go` — strftime→`NowSeconds()`, PRAGMA→`DeferForeignKeys`.
- **Modify** sibling packages (`internal/webhooks/{service,worker,reclaim,prune,enqueue}.go`, `internal/policy/{policy,paths}.go`, `internal/hooks/store.go`, `internal/lfs/locks/store.go`, `internal/lfs/quota/quota.go`) — handle type `*sql.DB` → `sqlitestore.Querier`; the divergent sites use helpers.
- **Create** `internal/auth/sqlitestore/backend_postgres_test.go`, `conformance_backend_pg_test.go` (or extend the existing conformance file).
- **Modify** `.github/workflows/conformance.yml` — add a `postgres` job.
- **Create** `docs/m23-b1-postgres-operator-guide.md`.

---

## Task 0: Spike — add pgx and pin its behaviors

**Files:**
- Modify: `go.mod`, `go.sum`

This de-risks the design. It adds the dependency, proves it's pure-Go, and records the SQLSTATE behaviors Tasks 4/6 rely on. Commits only the dependency.

- [ ] **Step 1: Add the pgx stdlib driver**

```bash
go get github.com/jackc/pgx/v5/stdlib@latest
```
Expected: resolves. Confirm in `go.mod` the module is `github.com/jackc/pgx/v5`. If `go get` pulls any cgo-requiring transitive dependency, STOP and report.

- [ ] **Step 2: Prove `CGO_ENABLED=0` cross-compilation with the driver imported**

Create a throwaway file `internal/auth/sqlitestore/zz_spike_pgx.go`:
```go
package sqlitestore

import _ "github.com/jackc/pgx/v5/stdlib"
```
Run:
```bash
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -o /dev/null ./cmd/bucketvcs && echo "OK $t" || echo "FAIL $t"; done
```
Expected: `OK` for all five. If any `FAIL` with a cgo error, STOP and report (the feature is not viable under the release constraints).

- [ ] **Step 3: Bring up a local Postgres and capture SQLSTATE behaviors**

```bash
docker run -d --name bv-pg -p 5433:5432 -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=bv postgres:17
# wait for readiness
for i in $(seq 1 30); do docker exec bv-pg pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
```
Write a temporary `internal/auth/sqlitestore/zz_spike_test.go` (DELETED at end of task) that opens `sql.Open("pgx", "postgres://postgres:pw@127.0.0.1:5433/bv?sslmode=disable")` and records, via `errors.As(err, &pgErr)` (`pgErr *pgconn.PgError`):
1. The `pgErr.Code` for a UNIQUE violation (create a table with a UNIQUE column, insert a dup). Expect `23505`.
2. The `pgErr.Code` for a CHECK violation (insert a row violating a `CHECK`). Expect `23514`.
3. Whether a multi-statement `Exec("CREATE TABLE a(x int); CREATE TABLE b(y int);")` succeeds or errors (determines whether the splitter is strictly required — ship it regardless).
4. That `BIGINT GENERATED BY DEFAULT AS IDENTITY` + `INSERT … RETURNING id` returns the new id.

Record the observed codes in the Task 0 commit message.

- [ ] **Step 4: Clean up the spike, commit the dependency**

Delete `zz_spike_pgx.go` and `zz_spike_test.go`. Stop the container: `docker rm -f bv-pg`. Run `go mod tidy`.
```bash
git add go.mod go.sum
git commit -m "M23 B1: add pure-Go pgx/v5 stdlib driver dependency

Spike findings: UNIQUE=SQLSTATE <code>, CHECK=SQLSTATE <code>; multi-statement
Exec <supported|rejected>; CGO_ENABLED=0 cross-build green for all 5 targets."
```

---

## Task 1: Extend the `Backend` interface + implement on sqlite/libsql

**Files:**
- Modify: `internal/auth/sqlitestore/backend.go`
- Modify: `internal/auth/sqlitestore/backend_libsql.go`
- Test: `internal/auth/sqlitestore/backend_dialect_test.go` (create)

Add the dialect methods to the interface and implement them for the two existing backends (the postgres impl comes in Task 4). The SQLite/libSQL forms preserve today's behavior exactly.

- [ ] **Step 1: Write the failing test**

Create `internal/auth/sqlitestore/backend_dialect_test.go`:
```go
package sqlitestore

import (
	"errors"
	"testing"
)

func TestSqliteDialect_Identity(t *testing.T) {
	b := sqliteBackend{}
	if got := b.Rebind("SELECT ? WHERE x = ?"); got != "SELECT ? WHERE x = ?" {
		t.Fatalf("sqlite Rebind must be identity, got %q", got)
	}
	if b.NowSeconds() != "strftime('%s','now')" {
		t.Fatalf("sqlite NowSeconds = %q", b.NowSeconds())
	}
	if got := b.Greatest("used_bytes - ?", "0"); got != "MAX(used_bytes - ?, 0)" {
		t.Fatalf("sqlite Greatest = %q", got)
	}
}

func TestSqliteDialect_Classifiers(t *testing.T) {
	b := sqliteBackend{}
	if !b.IsUniqueViolation(errors.New("UNIQUE constraint failed: users.name")) {
		t.Fatal("expected unique violation match")
	}
	if !b.IsCheckViolation(errors.New("CHECK constraint failed: ck")) {
		t.Fatal("expected check violation match")
	}
	if b.IsUniqueViolation(errors.New("syntax error")) {
		t.Fatal("false positive unique")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run 'TestSqliteDialect' -v`
Expected: FAIL (compile error — methods undefined).

- [ ] **Step 3: Extend the interface**

In `internal/auth/sqlitestore/backend.go`, replace the `Backend` interface with:
```go
// Backend abstracts the driver-specific concerns that differ between the
// SQLite (modernc), libSQL (Turso), and PostgreSQL backends.
type Backend interface {
	// Name reports the backend for logging: "sqlite" | "libsql" | "postgres".
	Name() string
	// Open opens the *sql.DB with this backend's driver, DSN, and pool
	// config. It does NOT run migrations.
	Open() (*sql.DB, error)
	// ApplyMigration executes one migration file body within tx.
	ApplyMigration(tx *sql.Tx, body string) error

	// Rebind converts a ?-placeholder query to the backend's placeholder
	// style. sqlite/libsql: identity. postgres: ?→$1,$2,… (literal-aware).
	Rebind(query string) string
	// IsUniqueViolation / IsCheckViolation classify constraint errors.
	IsUniqueViolation(err error) bool
	IsCheckViolation(err error) bool
	// IsFingerprintUniqueViolation reports a UNIQUE violation specifically on
	// the ssh_keys.fingerprint constraint.
	IsFingerprintUniqueViolation(err error) bool

	// NowSeconds returns a SQL expression yielding the current unix time in
	// seconds: sqlite "strftime('%s','now')"; postgres
	// "EXTRACT(EPOCH FROM now())::bigint".
	NowSeconds() string
	// Greatest returns a SQL expression for max(expr, floor): sqlite
	// "MAX(expr, floor)"; postgres "GREATEST(expr, floor)".
	Greatest(expr, floor string) string
	// DeferForeignKeys defers FK checks to COMMIT for the given tx. sqlite
	// execs "PRAGMA defer_foreign_keys = TRUE"; postgres is a no-op because
	// its FKs are declared DEFERRABLE INITIALLY DEFERRED.
	DeferForeignKeys(tx *sql.Tx) error
	// InsertReturningID runs an INSERT and returns the generated integer id.
	// sqlite execs then uses LastInsertId; postgres appends " RETURNING id"
	// and scans. The table's surrogate key MUST be named "id".
	InsertReturningID(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error)
}
```
Add `"context"` to the `backend.go` import block.

- [ ] **Step 4: Implement on `sqliteBackend`**

Append to `internal/auth/sqlitestore/backend.go`:
```go
func (sqliteBackend) Rebind(query string) string { return query }

func (sqliteBackend) IsUniqueViolation(err error) bool   { return sqliteIsUnique(err) }
func (sqliteBackend) IsCheckViolation(err error) bool    { return sqliteIsCheck(err) }
func (sqliteBackend) IsFingerprintUniqueViolation(err error) bool {
	return sqliteIsUnique(err) &&
		(strings.Contains(err.Error(), "ssh_keys.fingerprint") ||
			strings.Contains(err.Error(), "fingerprint"))
}

func (sqliteBackend) NowSeconds() string { return "strftime('%s','now')" }
func (sqliteBackend) Greatest(expr, floor string) string {
	return "MAX(" + expr + ", " + floor + ")"
}
func (sqliteBackend) DeferForeignKeys(tx *sql.Tx) error {
	_, err := tx.Exec("PRAGMA defer_foreign_keys = TRUE")
	return err
}
func (sqliteBackend) InsertReturningID(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// sqliteIsUnique / sqliteIsCheck are the substring matchers shared by the
// sqlite and libsql backends (libSQL surfaces the same SQLite error text).
func sqliteIsUnique(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "UNIQUE constraint failed") ||
		strings.Contains(m, "constraint failed: UNIQUE")
}
func sqliteIsCheck(err error) bool {
	return err != nil && strings.Contains(err.Error(), "CHECK constraint failed")
}
```
Add `"context"` if not already imported.

- [ ] **Step 5: Implement on `libsqlBackend`**

Append to `internal/auth/sqlitestore/backend_libsql.go` (libSQL == SQLite dialect, so it reuses the sqlite forms):
```go
func (libsqlBackend) Rebind(query string) string { return query }

func (libsqlBackend) IsUniqueViolation(err error) bool { return sqliteIsUnique(err) }
func (libsqlBackend) IsCheckViolation(err error) bool  { return sqliteIsCheck(err) }
func (libsqlBackend) IsFingerprintUniqueViolation(err error) bool {
	return sqliteIsUnique(err) &&
		(strings.Contains(err.Error(), "ssh_keys.fingerprint") ||
			strings.Contains(err.Error(), "fingerprint"))
}
func (libsqlBackend) NowSeconds() string { return "strftime('%s','now')" }
func (libsqlBackend) Greatest(expr, floor string) string {
	return "MAX(" + expr + ", " + floor + ")"
}
func (libsqlBackend) DeferForeignKeys(tx *sql.Tx) error {
	_, err := tx.Exec("PRAGMA defer_foreign_keys = TRUE")
	return err
}
func (libsqlBackend) InsertReturningID(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
```
Ensure `"context"` and `"strings"` are imported in `backend_libsql.go`.

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/auth/sqlitestore/ -run 'TestSqliteDialect' -v && go build ./...`
Expected: PASS; build OK. (The old free functions `isUniqueViolation`/`isCheckViolation`/`isFingerprintUniqueViolation` in store.go still exist and still compile — they are removed in Task 3 when call sites move to the backend.)

- [ ] **Step 7: Commit**

```bash
git add internal/auth/sqlitestore/backend.go internal/auth/sqlitestore/backend_libsql.go internal/auth/sqlitestore/backend_dialect_test.go
git commit -m "M23 B1: extend Backend with dialect methods (Rebind, classifiers, helpers) for sqlite/libsql"
```

---

## Task 2: `Querier` rebind wrapper + rewire Store and sibling packages

**Files:**
- Create: `internal/auth/sqlitestore/querier.go`
- Modify: `internal/auth/sqlitestore/store.go`, `rename.go`, `oidc.go`
- Modify: `internal/webhooks/{service,worker,reclaim,prune,enqueue}.go`, `internal/policy/{policy,paths}.go`, `internal/hooks/store.go`, `internal/lfs/locks/store.go`, `internal/lfs/quota/quota.go`

This is the plumbing task: one rebinding chokepoint so no query string needs `?`→`$N` edits. For sqlite/libsql `Rebind` is identity, so behavior is byte-for-byte unchanged — the existing tests are the regression guard.

- [ ] **Step 1: Create the `Querier` wrapper**

Create `internal/auth/sqlitestore/querier.go`:
```go
package sqlitestore

import (
	"context"
	"database/sql"
)

// Querier is the rebinding SQL-access surface used by the store and its sibling
// packages (webhooks, policy, hooks, lfs locks/quota). Every method rebinds the
// query to the backend's placeholder style before delegating. It also carries
// the backend's error classifiers so callers can map constraint errors without
// a separate backend reference.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	IsUniqueViolation(err error) bool
	IsCheckViolation(err error) bool
}

// dbWrap wraps *sql.DB + Backend. It is what Store holds and what Store.DB()
// returns.
type dbWrap struct {
	db      *sql.DB
	backend Backend
}

func (w *dbWrap) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return w.db.ExecContext(ctx, w.backend.Rebind(q), args...)
}
func (w *dbWrap) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return w.db.QueryContext(ctx, w.backend.Rebind(q), args...)
}
func (w *dbWrap) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return w.db.QueryRowContext(ctx, w.backend.Rebind(q), args...)
}
func (w *dbWrap) IsUniqueViolation(err error) bool { return w.backend.IsUniqueViolation(err) }
func (w *dbWrap) IsCheckViolation(err error) bool  { return w.backend.IsCheckViolation(err) }

func (w *dbWrap) Close() error { return w.db.Close() }
func (w *dbWrap) raw() *sql.DB { return w.db }

// BeginTx begins a transaction and returns a rebinding *txWrap.
func (w *dbWrap) BeginTx(ctx context.Context, opts *sql.TxOptions) (*txWrap, error) {
	tx, err := w.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &txWrap{tx: tx, backend: w.backend}, nil
}

// txWrap wraps *sql.Tx + Backend with the same rebinding surface.
type txWrap struct {
	tx      *sql.Tx
	backend Backend
}

func (w *txWrap) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return w.tx.ExecContext(ctx, w.backend.Rebind(q), args...)
}
func (w *txWrap) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return w.tx.QueryContext(ctx, w.backend.Rebind(q), args...)
}
func (w *txWrap) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return w.tx.QueryRowContext(ctx, w.backend.Rebind(q), args...)
}
func (w *txWrap) IsUniqueViolation(err error) bool { return w.backend.IsUniqueViolation(err) }
func (w *txWrap) IsCheckViolation(err error) bool  { return w.backend.IsCheckViolation(err) }
func (w *txWrap) Commit() error   { return w.tx.Commit() }
func (w *txWrap) Rollback() error { return w.tx.Rollback() }
func (w *txWrap) raw() *sql.Tx    { return w.tx }
```

- [ ] **Step 2: Rewire `Store` to hold a `*dbWrap`**

In `internal/auth/sqlitestore/store.go`, change the struct and `Open`/`Close`/`DB`:
```go
type Store struct {
	db      *dbWrap
	backend Backend
}
```
In `Open`, after `RunMigrations(db, b)` succeeds, build the wrap:
```go
	return &Store{db: &dbWrap{db: db, backend: b}, backend: b}, nil
```
Note `RunMigrations(db, b)` still takes the raw `*sql.DB` (migrations are authored per-dialect, not rebound). Update `Close` and `DB`:
```go
func (s *Store) Close() error { return s.db.Close() }

// DB returns the rebinding Querier for sibling packages that attach tables to
// the same authdb (webhooks, policy, hooks, lfs locks/quota).
func (s *Store) DB() Querier { return s.db }
```
All existing `s.db.ExecContext/QueryContext/QueryRowContext` call sites in store.go compile unchanged (dbWrap has the same method set). `s.db.BeginTx` in `rename.go` now returns `*txWrap` (same method set), so its `tx.ExecContext` / `tx.Commit` / `tx.Rollback` calls compile unchanged.

- [ ] **Step 3: Update sibling-package handle types**

In each sibling package, change the constructor parameter and struct field from `*sql.DB` to `sqlitestore.Querier`, and import `github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore`. Exact edits:

- `internal/webhooks/service.go`: `type Service struct { db *sql.DB … }` → `db sqlitestore.Querier`; `func New(db *sql.DB)` → `func New(db sqlitestore.Querier)`.
- `internal/webhooks/worker.go`: `func claim(ctx context.Context, db *sql.DB, batch int)` → `db sqlitestore.Querier`. The `db.BeginTx(...)` inside `claim` must become a `*txWrap`; since `Querier` does not expose `BeginTx`, change `claim`'s signature to take the concrete `*sqlitestore.Querier`'s begin capability. **Resolution:** add `BeginTx` to the `Querier` interface is wrong (txWrap is unexported). Instead, expose a small exported tx-runner on the Querier:

  In `querier.go`, add an exported interface and method so siblings can run transactions without touching unexported types:
  ```go
  // Tx is the rebinding transaction surface handed to RunInTx callbacks.
  type Tx interface {
  	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
  	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
  	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
  }
  // TxRunner is implemented by *dbWrap; RunInTx runs fn inside a transaction,
  // committing on nil error and rolling back otherwise.
  type TxRunner interface {
  	RunInTx(ctx context.Context, fn func(tx Tx) error) error
  }
  ```
  Implement on `*dbWrap`:
  ```go
  func (w *dbWrap) RunInTx(ctx context.Context, fn func(tx Tx) error) error {
  	tx, err := w.BeginTx(ctx, &sql.TxOptions{})
  	if err != nil {
  		return err
  	}
  	if err := fn(tx); err != nil {
  		_ = tx.Rollback()
  		return err
  	}
  	return tx.Commit()
  }
  ```
  (`*txWrap` satisfies `Tx`.) Add `TxRunner` to the `Querier` interface:
  ```go
  type Querier interface {
  	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
  	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
  	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
  	RunInTx(ctx context.Context, fn func(tx Tx) error) error
  	IsUniqueViolation(err error) bool
  	IsCheckViolation(err error) bool
  }
  ```
  Then rewrite `webhooks/worker.go` `claim` to use `db.RunInTx`:
  ```go
  func claim(ctx context.Context, db sqlitestore.Querier, batch int) ([]claimedRow, error) {
  	var out []claimedRow
  	err := db.RunInTx(ctx, func(tx sqlitestore.Tx) error {
  		rows, err := tx.QueryContext(ctx,
  			`SELECT d.id, d.endpoint_id, d.event_type, d.payload_json, d.attempts,
  			        e.url, e.secret
  			 FROM webhook_deliveries d
  			 JOIN webhook_endpoints e ON e.id = d.endpoint_id
  			 WHERE d.status='pending' AND d.next_attempt_at <= ?
  			   AND e.active=1
  			 ORDER BY d.next_attempt_at
  			 LIMIT ?`,
  			time.Now().Unix(), batch)
  		if err != nil {
  			return err
  		}
  		var claimed []claimedRow
  		for rows.Next() {
  			var r claimedRow
  			if err := rows.Scan(&r.ID, &r.EndpointID, &r.EventType, &r.PayloadJSON, &r.Attempts, &r.URL, &r.Secret); err != nil {
  				rows.Close()
  				return err
  			}
  			claimed = append(claimed, r)
  		}
  		rows.Close()
  		now := time.Now().Unix()
  		for i := range claimed {
  			claimed[i].Attempts++
  			if _, err := tx.ExecContext(ctx,
  				`UPDATE webhook_deliveries
  				   SET status='in_flight', last_attempt_at=?, attempts=?
  				 WHERE id=?`,
  				now, claimed[i].Attempts, claimed[i].ID); err != nil {
  				return err
  			}
  		}
  		out = claimed
  		return nil
  	})
  	return out, err
  }
  ```
- `internal/webhooks/reclaim.go`: `func Reclaim(ctx context.Context, db *sql.DB, …)` → `db sqlitestore.Querier`.
- `internal/webhooks/prune.go`, `enqueue.go`: any `*sql.DB` param/field → `sqlitestore.Querier`.
- `internal/policy/policy.go`, `paths.go`: `db *sql.DB` field + `New(db *sql.DB)` → `sqlitestore.Querier`.
- `internal/hooks/store.go`: `db *sql.DB` field + `NewStore(db *sql.DB)` → `sqlitestore.Querier`.
- `internal/lfs/locks/store.go`: `db *sql.DB` field → `sqlitestore.Querier`.
- `internal/lfs/quota/quota.go`: `db *sql.DB` field + `New(db *sql.DB, …)` → `sqlitestore.Querier`.

For each, the call sites that previously passed `store.DB()` (a `*sql.DB`) now pass `store.DB()` (a `Querier`) — unchanged at the call site since `Store.DB()` return type changed. **Update the gateway/CLI wiring and each package's `*_test.go` `openTestDB` helper** to match: tests that construct `New(rawSqlDB)` must wrap. Provide a test helper exported from sqlitestore:
```go
// NewTestQuerier wraps a raw *sql.DB as a sqlite-backed Querier for tests in
// sibling packages. Test-only convenience.
func NewTestQuerier(db *sql.DB) Querier { return &dbWrap{db: db, backend: sqliteBackend{}} }
```
Add this to `querier.go`. Sibling `*_test.go` files that previously did `svc := New(db)` with a raw `*sql.DB` now do `New(sqlitestore.NewTestQuerier(db))`. Sibling tests that pass the raw db to helper funcs (e.g. `seedDelivery(t, db, …)`, `countByStatus`, `mustExec`) may keep using the raw `*sql.DB` for their own seeding (those helpers can stay `*sql.DB`), but anything calling the production `New`/`Reclaim`/`claim` must pass a `Querier`.

- [ ] **Step 4: Build + full test on sqlite**

Run: `go build ./... && go test ./internal/... 2>&1 | tail -20`
Expected: build OK; all tests PASS (sqlite Rebind is identity → no behavior change). Fix any remaining `*sql.DB`-vs-`Querier` type errors the compiler reports until green.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "M23 B1: Querier rebind wrapper; route store + 6 sibling packages through it"
```

---

## Task 3: Route divergent constructs through dialect helpers (sqlite still green)

**Files:**
- Modify: `internal/auth/sqlitestore/store.go`, `oidc.go`, `rename.go`
- Modify: `internal/lfs/quota/quota.go`, `internal/webhooks/service.go`
- Modify: `internal/auth/sqlitestore/store.go` (remove old free-function classifiers; route to backend)

All edits keep SQLite behavior identical; they just stop hard-coding SQLite-only SQL. Verified by the existing suite.

- [ ] **Step 1: Unify `INSERT OR IGNORE` → `ON CONFLICT DO NOTHING`**

In `store.go` `RegisterRepo` and `RegisterRepoIfNew`, change:
```
INSERT OR IGNORE INTO repos (tenant, name, public_read, created_at) VALUES (?, ?, 0, ?)
```
to:
```
INSERT INTO repos (tenant, name, public_read, created_at) VALUES (?, ?, 0, ?)
ON CONFLICT(tenant, name) DO NOTHING
```
Both SQLite and Postgres accept this; `RowsAffected()==0` on conflict is preserved on both.

- [ ] **Step 2: Route `strftime` sites through `NowSeconds()`**

In `store.go` `RevokeSSHKey` and `TouchSSHKeyUsage`, and `oidc.go` `AddOIDCIssuer` and `AddOIDCRule`, replace the literal `strftime('%s','now')` embedded in the SQL with `" + s.backend.NowSeconds() + "` string concatenation. Example (RevokeSSHKey):
```go
_, err := s.db.ExecContext(ctx,
	`UPDATE ssh_keys SET revoked_at = `+s.backend.NowSeconds()+` WHERE id = ?`,
	id)
```
(The expression is a constant SQL fragment, not user input — safe to concatenate.)

- [ ] **Step 3: Route the quota clamp through `Greatest()`**

In `internal/lfs/quota/quota.go` `Subtract`, the service holds a `sqlitestore.Querier` but needs the backend's `Greatest`. Add a backend accessor: the quota `Service` should store the backend. **Simplest:** pass the SQL fragment via a tiny method on `Querier`. Add to the `Querier` interface and `dbWrap`/`txWrap`:
```go
	Greatest(expr, floor string) string
```
`dbWrap.Greatest` / `txWrap.Greatest` delegate to `w.backend.Greatest(expr, floor)`. Then in `Subtract`:
```go
q := `UPDATE quotas SET used_bytes = ` + s.db.Greatest("used_bytes - ?", "0") +
	`, updated_at = ? WHERE tenant = ?`
_, err := s.db.ExecContext(ctx, q, delta, now, tenant)
```
(Add `Greatest` to the `Querier` interface declaration too.)

- [ ] **Step 4: Route the webhook-endpoint insert through `InsertReturningID`**

`InsertReturningID` is a `Backend` method that needs a `*sql.Tx`. Expose it on the tx surface instead so callers use it within `RunInTx`. Add to the `Tx` interface and `txWrap`:
```go
	InsertReturningID(ctx context.Context, query string, args ...any) (int64, error)
```
`txWrap.InsertReturningID` delegates to `w.backend.InsertReturningID(ctx, w.tx, query, args...)`. In `internal/webhooks/service.go`, rewrite the endpoint insert (currently `res, _ := db.ExecContext(...); id, _ := res.LastInsertId()`) to run inside `db.RunInTx` and call `tx.InsertReturningID`:
```go
var id int64
err := s.db.RunInTx(ctx, func(tx sqlitestore.Tx) error {
	var e error
	id, e = tx.InsertReturningID(ctx,
		`INSERT INTO webhook_endpoints (tenant, repo, url, secret, active, created_at)
		 VALUES (?, ?, ?, ?, 1, ?)`,
		tenant, repo, url, secret, now)
	return e
})
```
(Add `InsertReturningID` to the `Tx` interface declaration.)

- [ ] **Step 5: Route `RenameRepo` FK deferral through `DeferForeignKeys`**

In `internal/auth/sqlitestore/rename.go`, replace the literal `PRAGMA defer_foreign_keys = TRUE` exec with a call through the backend, using the raw tx. Since `RenameRepo` begins its tx via `s.db.BeginTx` (now `*txWrap`), expose the deferral on `txWrap`:
add to `txWrap`:
```go
func (w *txWrap) DeferForeignKeys() error { return w.backend.DeferForeignKeys(w.tx) }
```
and in `rename.go`, replace the PRAGMA exec with `if err := tx.DeferForeignKeys(); err != nil { … }`. (sqlite execs the PRAGMA; postgres no-ops because its FKs are `DEFERRABLE INITIALLY DEFERRED`.)

- [ ] **Step 6: Move classifier call sites to the backend; delete the free functions**

In `store.go`, replace every call to `isUniqueViolation(err)` → `s.backend.IsUniqueViolation(err)`, `isCheckViolation(err)` → `s.backend.IsCheckViolation(err)`, `isFingerprintUniqueViolation(err)` → `s.backend.IsFingerprintUniqueViolation(err)` (grep to find all sites). Delete the three free-function definitions. In `internal/webhooks/service.go` and `internal/lfs/locks/store.go`, replace their local `isUniqueViolation` copies with `s.db.IsUniqueViolation(err)` (the `Querier` exposes it) and delete the copies.

- [ ] **Step 7: Build + full test on sqlite**

Run: `go build ./... && go test ./internal/... 2>&1 | tail -20`
Expected: build OK; all PASS. SQLite behavior is unchanged (helpers return the SQLite forms).

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "M23 B1: route divergent SQL (upsert/now/greatest/returning-id/defer-fk) + error classification through the backend"
```

---

## Task 4: `postgresBackend`

**Files:**
- Create: `internal/auth/sqlitestore/backend_postgres.go`
- Modify: `internal/auth/sqlitestore/backend.go` (resolveBackend)
- Test: `internal/auth/sqlitestore/backend_postgres_test.go` (create)

- [ ] **Step 1: Write the failing tests**

Create `internal/auth/sqlitestore/backend_postgres_test.go`:
```go
package sqlitestore

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresRebind(t *testing.T) {
	b := postgresBackend{}
	cases := map[string]string{
		"SELECT 1":                       "SELECT 1",
		"WHERE a = ?":                     "WHERE a = $1",
		"VALUES (?, ?, ?)":                "VALUES ($1, $2, $3)",
		"WHERE a = ? AND b = '?lit' OR c = ?": "WHERE a = $1 AND b = '?lit' OR c = $2", // ? in literal untouched
	}
	for in, want := range cases {
		if got := b.Rebind(in); got != want {
			t.Fatalf("Rebind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPostgresClassifiers(t *testing.T) {
	b := postgresBackend{}
	if !b.IsUniqueViolation(&pgconn.PgError{Code: "23505"}) {
		t.Fatal("23505 should be unique violation")
	}
	if !b.IsCheckViolation(&pgconn.PgError{Code: "23514"}) {
		t.Fatal("23514 should be check violation")
	}
	if b.IsUniqueViolation(errors.New("plain")) {
		t.Fatal("plain error must not classify")
	}
	if !b.IsFingerprintUniqueViolation(&pgconn.PgError{Code: "23505", ConstraintName: "ssh_keys_fingerprint_key"}) {
		t.Fatal("fingerprint constraint should match")
	}
	if b.IsFingerprintUniqueViolation(&pgconn.PgError{Code: "23505", ConstraintName: "users_name_key"}) {
		t.Fatal("non-fingerprint constraint must not match")
	}
}

func TestPostgresDialectForms(t *testing.T) {
	b := postgresBackend{}
	if b.NowSeconds() != "EXTRACT(EPOCH FROM now())::bigint" {
		t.Fatalf("NowSeconds = %q", b.NowSeconds())
	}
	if got := b.Greatest("used_bytes - ?", "0"); got != "GREATEST(used_bytes - ?, 0)" {
		t.Fatalf("Greatest = %q", got)
	}
}

func TestResolveBackend_Postgres(t *testing.T) {
	for _, v := range []string{"postgres://u@h/db", "postgresql://u@h/db"} {
		b, err := resolveBackend(v)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if b.Name() != "postgres" {
			t.Fatalf("%s: backend=%s want postgres", v, b.Name())
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run 'TestPostgres|TestResolveBackend_Postgres' -v`
Expected: FAIL (compile error — `postgresBackend` undefined).

- [ ] **Step 3: Implement `postgresBackend`**

Create `internal/auth/sqlitestore/backend_postgres.go`:
```go
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// postgresBackend is the PostgreSQL backend (pgx via the database/sql stdlib
// adapter — pure-Go, preserves CGO_ENABLED=0). Phase B1: single-node
// (MaxOpenConns(1)); multi-node hardening is B2.
type postgresBackend struct {
	dsn string
}

// newPostgresBackend builds the backend from a postgres://|postgresql:// URL.
// The password is resolved OFF the CLI: BUCKETVCS_DB_AUTH_TOKEN env (precedence),
// else standard libpq mechanisms (PGPASSWORD/.pgpass) honored by pgx. A password
// embedded in the URL is allowed but warns (visible to other processes).
func newPostgresBackend(rawURL string) (Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse url: %w", err)
	}
	if tok := os.Getenv(dbAuthTokenEnv); tok != "" {
		user := u.User.Username()
		u.User = url.UserPassword(user, tok) // env password takes precedence
	} else if _, hasPw := u.User.Password(); hasPw {
		slog.Default().Warn("postgres URL embeds a password; prefer "+dbAuthTokenEnv+" or PGPASSWORD (URL is visible to other processes)",
			"host", u.Host)
	}
	return postgresBackend{dsn: u.String()}, nil
}

func (postgresBackend) Name() string { return "postgres" }

func (b postgresBackend) Open() (*sql.DB, error) {
	db, err := sql.Open("pgx", b.dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // B1 single-node; B2 raises this with concurrency hardening
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return db, nil
}

func (postgresBackend) ApplyMigration(tx *sql.Tx, body string) error {
	for _, stmt := range splitSQLStatements(body) {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("postgres: exec statement %q: %w", truncate(stmt, 80), err)
		}
	}
	return nil
}

// Rebind converts ? placeholders to $1,$2,… skipping ? inside single-quoted
// string literals. Our migrations/queries contain no ? in literals, but the
// scan is literal-aware to stay safe.
func (postgresBackend) Rebind(query string) string {
	var sb strings.Builder
	sb.Grow(len(query) + 8)
	n := 0
	inLit := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			inLit = !inLit
			sb.WriteByte(c)
		case c == '?' && !inLit:
			n++
			sb.WriteByte('$')
			sb.WriteString(itoa(n))
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func (postgresBackend) pgErr(err error) *pgconn.PgError {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}
func (b postgresBackend) IsUniqueViolation(err error) bool {
	pe := b.pgErr(err)
	return pe != nil && pe.Code == "23505"
}
func (b postgresBackend) IsCheckViolation(err error) bool {
	pe := b.pgErr(err)
	return pe != nil && pe.Code == "23514"
}
func (b postgresBackend) IsFingerprintUniqueViolation(err error) bool {
	pe := b.pgErr(err)
	return pe != nil && pe.Code == "23505" && strings.Contains(pe.ConstraintName, "fingerprint")
}

func (postgresBackend) NowSeconds() string { return "EXTRACT(EPOCH FROM now())::bigint" }
func (postgresBackend) Greatest(expr, floor string) string {
	return "GREATEST(" + expr + ", " + floor + ")"
}

// DeferForeignKeys is a no-op on postgres: the schema declares its FKs
// DEFERRABLE INITIALLY DEFERRED, so checks already defer to COMMIT.
func (postgresBackend) DeferForeignKeys(tx *sql.Tx) error { return nil }

// InsertReturningID appends RETURNING id and scans the generated key.
func (postgresBackend) InsertReturningID(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	// query carries ? placeholders; ApplyMigration/dbWrap rebind elsewhere, but
	// this runs the raw tx, so rebind here.
	q := postgresBackend{}.Rebind(query) + " RETURNING id"
	var id int64
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}
```
Note: `dbAuthTokenEnv` is the const already defined in `backend_libsql.go` (`"BUCKETVCS_DB_AUTH_TOKEN"`), same package. `truncate` and `splitSQLStatements` already exist in the package.

**Caveat for `InsertReturningID` on the sqlite/libsql side:** those impls receive the already-rebound query from `txWrap`? No — `txWrap.InsertReturningID` calls `backend.InsertReturningID(ctx, w.tx, query, args...)` with the RAW (un-rebound) query. So each backend's `InsertReturningID` is responsible for its own rebinding: sqlite/libsql do not rebind (identity) and use the query as-is with `LastInsertId`; postgres rebinds + appends RETURNING. This is consistent with the code above. Ensure the sqlite/libsql `InsertReturningID` (Task 1) execs the raw query unchanged (correct — identity rebind).

- [ ] **Step 4: Wire `resolveBackend`**

In `backend.go` `resolveBackend`, add the postgres branch before the libsql check:
```go
func resolveBackend(value string) (Backend, error) {
	if isPostgresValue(value) {
		return newPostgresBackend(value)
	}
	if isLibsqlValue(value) {
		return newLibsqlBackend(value)
	}
	return sqliteBackend{path: sqlitePath(value)}, nil
}

func isPostgresValue(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
		return true
	default:
		return false
	}
}
```

- [ ] **Step 5: Run tests + cross-build gate**

Run:
```bash
go test ./internal/auth/sqlitestore/ -run 'TestPostgres|TestResolveBackend' -v
go build ./...
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -o /dev/null ./cmd/bucketvcs && echo "OK $t"; done
```
Expected: PASS; build OK; all five `OK` (pgx is pure-Go).

- [ ] **Step 6: Commit**

```bash
git add internal/auth/sqlitestore/backend_postgres.go internal/auth/sqlitestore/backend.go internal/auth/sqlitestore/backend_postgres_test.go
git commit -m "M23 B1: postgresBackend (pgx stdlib, $N rebind, SQLSTATE classifiers, dialect helpers)"
```

---

## Task 5: Postgres migration set

**Files:**
- Create: `internal/auth/sqlitestore/migrations_postgres/0001_init.sql` … `0010_oidc.sql`
- Modify: `internal/auth/sqlitestore/schema.go` (or `backend_postgres.go`) to embed + select the Postgres set
- Test: `internal/auth/sqlitestore/migrations_pg_test.go` (create)

The Postgres migrations are a hand-translation of `internal/auth/sqlitestore/migrations/*.sql`. **Translation rules applied to every file:**

1. `BLOB` → `BYTEA`.
2. `INTEGER PRIMARY KEY AUTOINCREMENT` → `BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY`.
3. `strftime('%s','now')` → `EXTRACT(EPOCH FROM now())::bigint`.
4. The column named `trigger` → quoted `"trigger"` in DDL (the Go queries already use `"trigger"` after Task 3? — they do not; the existing queries use bare `trigger`. **Also** quote it in the hooks queries in `internal/hooks/store.go` so both dialects accept it: SQLite accepts `"trigger"` too. Do this rewrite in Task 5 Step 3.)
5. Every `FOREIGN KEY (...) REFERENCES ...` clause gains `DEFERRABLE INITIALLY DEFERRED`.
6. All other types/constraints (`TEXT`, `INTEGER`, `CHECK (... IN (0,1))`, partial index `... WHERE`, composite PKs) are copied verbatim — they are valid Postgres.
7. The `schema_version` table DDL + each migration's trailing `INSERT INTO schema_version (version, applied_at) VALUES (N, strftime('%s','now'))` is mirrored with rule 3 applied.

- [ ] **Step 1: Translate all 10 migration files**

For each `internal/auth/sqlitestore/migrations/NNNN_*.sql`, create `internal/auth/sqlitestore/migrations_postgres/NNNN_*.sql` (same filename) applying rules 1-7. The files needing non-trivial attention:

- **0002_ssh_keys.sql:** `public_key BLOB NOT NULL` → `BYTEA`; FK on `user_id` → add `DEFERRABLE INITIALLY DEFERRED`; copy the multi-column XOR `CHECK` and `scope_perm CHECK (... IN ('read','write'))` verbatim.
- **0006_webhooks.sql:** `id INTEGER PRIMARY KEY AUTOINCREMENT` → `BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY`; `payload_json BLOB NOT NULL` → `BYTEA`; FK `endpoint_id` → `BIGINT` referencing the IDENTITY column + `DEFERRABLE INITIALLY DEFERRED`; copy the partial index `CREATE INDEX webhook_deliveries_due ON webhook_deliveries (status, next_attempt_at) WHERE status = 'pending';` verbatim (valid Postgres).
- **0009_hooks.sql:** quote `"trigger"` in the column DDL and the `CHECK ("trigger" IN ('pre-receive','post-receive'))`; FK on `(tenant, repo)` → `DEFERRABLE INITIALLY DEFERRED`.
- **0010_oidc.sql:** the three `ALTER TABLE tokens ADD COLUMN …` port as-is (Postgres allows `ADD COLUMN … CHECK`); the `_oidc` seed `INSERT` ports with rule 3; FK clauses → `DEFERRABLE INITIALLY DEFERRED`.
- **0001/0004/0005/0007/0008:** apply rules 3 (schema_version footer) + 5 (FKs) + 2 (none have AUTOINCREMENT except 0006) — these are otherwise identical text. 0008 is `ALTER TABLE tokens ADD COLUMN scopes INTEGER NOT NULL DEFAULT 0` + its schema_version insert — port directly with rule 3.

(Read each SQLite source file and apply the rules; do not invent schema — the column sets must match exactly so the shared Go scan code works.)

- [ ] **Step 2: Embed + select the Postgres migration set**

`schema.go` currently embeds `migrations/*.sql` and `RunMigrations(db, backend)` iterates them. Make the migration source backend-dependent. Add to `backend.go` interface? No — keep it local to schema.go. In `schema.go`:
```go
//go:embed migrations/*.sql
var sqliteMigrations embed.FS

//go:embed migrations_postgres/*.sql
var postgresMigrations embed.FS

// migrationsFor returns the embedded migration FS + dir for the backend.
func migrationsFor(b Backend) (fs.FS, string) {
	if b.Name() == "postgres" {
		return postgresMigrations, "migrations_postgres"
	}
	return sqliteMigrations, "migrations"
}
```
Update `RunMigrations` to read filenames from `migrationsFor(backend)` instead of the hard-coded sqlite embed. The per-file apply still calls `backend.ApplyMigration(tx, body)`. (Confirm the existing embed var name in schema.go and adapt; the sqlite set keeps its current behavior.)

- [ ] **Step 3: Quote `trigger` in hooks queries**

In `internal/hooks/store.go`, change every bare `trigger` column reference in SQL to `"trigger"` (SQLite and Postgres both accept the double-quoted identifier). Grep the file for `trigger` in query strings.

- [ ] **Step 4: Write the splitter test over the Postgres set**

Create `internal/auth/sqlitestore/migrations_pg_test.go`:
```go
package sqlitestore

import (
	"testing"
)

func TestPostgresMigrationsSplit(t *testing.T) {
	entries, err := postgresMigrations.ReadDir("migrations_postgres")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 10 {
		t.Fatalf("expected 10 postgres migrations, got %d", len(entries))
	}
	for _, e := range entries {
		body, err := postgresMigrations.ReadFile("migrations_postgres/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		stmts := splitSQLStatements(string(body))
		if len(stmts) == 0 {
			t.Fatalf("%s: no statements", e.Name())
		}
		for _, s := range stmts {
			if s == "" {
				t.Fatalf("%s: empty statement", e.Name())
			}
		}
	}
}
```

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/auth/sqlitestore/ -run 'TestPostgresMigrationsSplit' -v && go build ./... && go test ./internal/... 2>&1 | tail -10`
Expected: PASS; build OK; sqlite suite still green (the hooks `"trigger"` quoting is sqlite-compatible).

- [ ] **Step 6: Commit**

```bash
git add internal/auth/sqlitestore/migrations_postgres internal/auth/sqlitestore/schema.go internal/auth/sqlitestore/migrations_pg_test.go internal/hooks/store.go
git commit -m "M23 B1: Postgres migration set (BYTEA/IDENTITY/EXTRACT/DEFERRABLE FKs) + backend-selected migration source"
```

---

## Task 6: Backend conformance suite (gated, Postgres)

**Files:**
- Create: `internal/auth/sqlitestore/conformance_pg_test.go`

A behavioral suite run against a live Postgres, proving Postgres is a true drop-in. Gated by `//go:build postgres` so it never runs per-commit; Task 7 supplies `BUCKETVCS_POSTGRES_URL`. Mirrors the Phase A libSQL conformance file (`conformance_backend_test.go`).

- [ ] **Step 1: Write the suite**

Create `internal/auth/sqlitestore/conformance_pg_test.go`:
```go
//go:build postgres

package sqlitestore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func openPostgres(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("BUCKETVCS_POSTGRES_URL")
	if url == "" {
		t.Skip("BUCKETVCS_POSTGRES_URL not set")
	}
	s, err := Open(url)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.backend.Name() != "postgres" {
		t.Fatalf("backend=%s, want postgres", s.backend.Name())
	}
	return s
}

func TestPostgresConformance(t *testing.T) {
	s := openPostgres(t)
	ctx := context.Background()

	if _, err := s.GetUserByName(ctx, "_oidc"); err != nil {
		t.Fatalf("migrations did not apply (no _oidc user): %v", err)
	}
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.CreateUser(ctx, "alice", false); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("dup user: want ErrConflict, got %v", err)
	}
	if err := s.RegisterRepo(ctx, "acme", "web"); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	u, err := s.GetUserByName(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Grant(ctx, "alice", "acme", "web", "write"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	actor := &auth.Actor{UserID: u.ID, Name: "alice"}
	if perm, err := s.LookupRepoPerm(ctx, actor, "acme", "web"); err != nil || perm != auth.PermWrite {
		t.Fatalf("perm=%v err=%v want write", perm, err)
	}

	tok, id, secret, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateToken(ctx, id, u.ID, hash, "lap", nil, auth.ScopeRepoWrite, "", "", ""); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if gotActor, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok}); err != nil || gotActor == nil || gotActor.Name != "alice" {
		t.Fatalf("verify: actor=%v err=%v", gotActor, err)
	}

	// CHECK enforcement: scope_perm CHECK on tokens (migration 0010) → must be
	// classified by the postgres SQLSTATE matcher.
	exp := time.Now().Unix() + 900
	err = s.CreateToken(ctx, "BADPERMTOKEN0000000000AA", "_oidc", hash, "x", &exp,
		auth.ScopeRepoRead, "acme", "web", "BOGUS")
	if err == nil {
		t.Fatal("CHECK on scope_perm should reject 'BOGUS'")
	}
	if !s.backend.IsCheckViolation(err) {
		t.Fatalf("postgres CHECK error not matched by IsCheckViolation: %v", err)
	}

	// OIDC mint round-trips.
	mint, err := s.MintOIDCToken(ctx, MintOIDCParams{
		Tenant: "acme", Repo: "web", Perm: auth.PermWrite,
		Scopes: auth.ScopeRepoWrite, TTLSeconds: 900, Label: "oidc:gh:sub",
	})
	if err != nil {
		t.Fatalf("mint oidc: %v", err)
	}
	if _, _, scope, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "x", Password: mint}); err != nil || scope == nil || scope.Repo != "web" {
		t.Fatalf("verify minted: scope=%v err=%v", scope, err)
	}

	// FK cascade: deleting the repo removes its permission rows.
	if err := s.DeleteRepo(ctx, "acme", "web"); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	if perm, _ := s.LookupRepoPerm(ctx, actor, "acme", "web"); perm != auth.PermNone {
		t.Fatalf("after repo delete, perm=%v want none (cascade)", perm)
	}

	// Rename works single-node on postgres (deferred FKs).
	if err := s.RegisterRepo(ctx, "acme", "old"); err != nil {
		t.Fatalf("register old: %v", err)
	}
	if err := s.Grant(ctx, "alice", "acme", "old", "write"); err != nil {
		t.Fatalf("grant old: %v", err)
	}
	if err := s.RenameRepo(ctx, "acme", "old", "new"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if perm, _ := s.LookupRepoPerm(ctx, actor, "acme", "new"); perm != auth.PermWrite {
		t.Fatalf("after rename, perm on new=%v want write", perm)
	}
}
```
(Verify `RenameRepo`'s exact signature in `rename.go` — `RenameRepo(ctx, tenant, oldName, newName)` — and adjust if it differs.)

- [ ] **Step 2: Verify it builds under the tag and skips without a URL**

Run: `go test -tags postgres ./internal/auth/sqlitestore/ -run TestPostgresConformance -v`
Expected: compiles and SKIPs (no `BUCKETVCS_POSTGRES_URL`). Confirm the default build is unaffected: `go test ./internal/auth/sqlitestore/`.

- [ ] **Step 3: Run against a local Postgres (developer check)**

```bash
docker run -d --name bv-pg -p 5433:5432 -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=bv postgres:17
for i in $(seq 1 30); do docker exec bv-pg pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
BUCKETVCS_POSTGRES_URL="postgres://postgres@127.0.0.1:5433/bv?sslmode=disable" \
  BUCKETVCS_DB_AUTH_TOKEN="pw" \
  go test -tags postgres ./internal/auth/sqlitestore/ -run TestPostgresConformance -v
docker rm -f bv-pg
```
Expected: PASS. If error classification fails (UNIQUE/CHECK not mapped), re-check the SQLSTATE codes from Task 0 against the matcher.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/sqlitestore/conformance_pg_test.go
git commit -m "M23 B1: gated Postgres conformance suite (behavioral parity vs postgres:17)"
```

---

## Task 7: Nightly CI job for the Postgres conformance

**Files:**
- Modify: `.github/workflows/conformance.yml`

The `conformance` workflow runs nightly + on demand. Add a job that boots Postgres and runs the gated suite, mirroring the Phase A `libsql` job.

- [ ] **Step 1: Add the job**

Append under `jobs:` in `.github/workflows/conformance.yml`:
```yaml
  postgres:
    name: postgres conformance
    runs-on: ubuntu-latest
    timeout-minutes: 15
    services:
      postgres:
        image: postgres:17
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

- [ ] **Step 2: Validate the workflow YAML**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/conformance.yml')); print('yaml ok')"`
Expected: `yaml ok`.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/conformance.yml
git commit -m "M23 B1: nightly Postgres conformance CI job (postgres:17 service)"
```

---

## Task 8: Operator guide

**Files:**
- Create: `docs/m23-b1-postgres-operator-guide.md`

- [ ] **Step 1: Write the guide**

Create `docs/m23-b1-postgres-operator-guide.md` in the style of `docs/m23-turso-operator-guide.md`, covering:
- **What it is:** back the metadata DB with PostgreSQL; Git data stays in object storage; SQLite remains the zero-dependency default; **single-node only in B1** (multi-node is B2).
- **Enabling:** `--auth-db postgres://user@host:5432/dbname?sslmode=require`; password via `BUCKETVCS_DB_AUTH_TOKEN` (precedence) or standard `PGPASSWORD`/`~/.pgpass`; a worked example. Applies to `serve` and every `bucketvcs` subcommand that takes `--auth-db`.
- **Selection table:** bare path / `sqlite:` / `file:` → SQLite; `libsql://` / `http(s)://` → libSQL; `postgres://` / `postgresql://` → Postgres. The password is read from env, never the command line.
- **Caveats (prominent):** (1) single-node only — multi-node-safe webhook claiming + quota updates are B2; running multiple gateway nodes against one Postgres DB is not yet race-safe; (2) `MaxOpenConns(1)` throughput bound; (3) rate-limiter stays per-node; (4) the `internal/auth/sqlitestore` package name is a historical misnomer (hosts three backends).
- **Migrating existing data:** dump SQLite (`sqlite3 auth.db .dump`), hand-adapt to Postgres or use the empty Postgres DB and re-create users/tokens via the CLI; point `--auth-db` at the Postgres URL.
- **Verifying:** `serve` logs `authdb opened backend=postgres` at startup.

- [ ] **Step 2: Commit**

```bash
git add docs/m23-b1-postgres-operator-guide.md
git commit -m "M23 B1: PostgreSQL operator guide"
```

---

## Final verification

- [ ] **Per-commit suite + build + cross-compile**

Run:
```bash
go build ./... && go test ./internal/... && go vet ./internal/...
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -o /dev/null ./cmd/bucketvcs && echo "OK $t"; done
gofmt -l internal/ cmd/
```
Expected: all PASS; all five `OK`; gofmt prints nothing.

- [ ] **Gated Postgres suite (with docker)** — as in Task 6 Step 3: PASS.

- [ ] **libSQL conformance still green** — `BUCKETVCS_LIBSQL_URL` run (Phase A) still PASSES (the interface extension + Querier wrapper must not regress libSQL).

- [ ] **Update memory index** — add an `m23_b1_progress.md` topic file + a short MEMORY.md line once merged.

---

## Self-review notes (for the implementer)

- **Spec coverage:** Backend interface extension (Task 1) ↔ spec §2.1; Querier chokepoint (Task 2) ↔ §2.2; divergent-construct helpers + classifier routing (Task 3) ↔ §2.3; postgresBackend driver/DSN/password/rebind/SQLSTATE (Task 4) ↔ §3; Postgres migration set (Task 5) ↔ §4; resolveBackend (Task 4) ↔ §5; error handling (Tasks 3,4) ↔ §7; conformance (Tasks 6,7) ↔ §9.2; unit tests (Tasks 1,4,5) ↔ §9.1; cross-compile (Tasks 0,4,Final) ↔ §9.3; caveats + guide (Task 8) ↔ §8; acceptance ↔ §10.
- **Deviation from spec, intentional:** the spec listed 3 dialect helpers; this plan adds `DeferForeignKeys` and `InsertReturningID` as backend methods (the spec mentioned both as needs but framed deferred-FK rename under B2). B1 makes single-node rename + the webhook-endpoint insert *work* on Postgres (functional parity, exercised by conformance); the **multi-node race-safety** of webhook claiming and quota idempotency remains B2.
- **`Open` signature unchanged**, so no store consumer changes — re-verify with `grep -rn 'sqlitestore.Open(' --include='*.go'` after Task 4.
- **The one invasive change** is the sibling-package handle type (`*sql.DB` → `sqlitestore.Querier`, Task 2). For sqlite/libsql `Rebind` is identity, so the existing sqlite test suites are the regression guard — keep them green at every task.
- **Known follow-on (B2):** `FOR UPDATE SKIP LOCKED` webhook claiming; DB-level quota verify-replay idempotency; raise `MaxOpenConns`; distributed/again-per-node rate limiter decision; concurrent-writer rename safety.
