# M23 B1: PostgreSQL metadata backend + dialect layer (single-node)

Date: 2026-05-28
Status: design

## 1. Goals

Let operators back the bucketvcs **metadata/auth database** (the M4 store at
`internal/auth/sqlitestore` — users, tokens, repos, permissions, protected
refs/paths, hooks, webhooks, OIDC rules, LFS locks, quotas) with **PostgreSQL**,
selected at runtime by the `--auth-db` URL scheme. Git object data is unaffected:
it stays in object storage (S3/R2/GCS/Azure/localfs).

This is **B1** of the two-part M23 Phase B effort:

- **B1 (this spec):** add a `postgresBackend` plus the **SQL-dialect layer**
  (placeholder rebinding, error-code classification, a few divergent-construct
  helpers, and a Postgres migration set) so the existing `?`-flavored SQL and
  store methods run on Postgres. **Single-node** — `MaxOpenConns(1)` is retained,
  exactly the M23 Phase A (libSQL) posture.
- **B2 (separate spec, later):** the **multi-node concurrency hardening** that a
  shared DB with multiple gateway nodes requires — `SELECT … FOR UPDATE SKIP
  LOCKED` webhook claiming, DB-level quota verify-replay idempotency, deferred-FK
  `RenameRepo`, raising the connection pool above 1, and documenting the
  per-node rate limiter. B1 front-loads the migration changes B2 needs (FKs are
  declared `DEFERRABLE` here) so B2 requires no migration edits.

B1 builds directly on the M23 Phase A `Backend` seam (`Name`/`Open`/
`ApplyMigration`, scheme-based `resolveBackend`); it extends that seam rather
than introducing a new one.

### 1.1 In scope (B1)

- Extend the `Backend` interface with `Rebind`, `IsUniqueViolation`,
  `IsCheckViolation` (error classifiers move from free functions into the
  interface).
- A `postgresBackend` (pgx via the `database/sql` stdlib adapter — pure-Go,
  preserves `CGO_ENABLED=0`).
- A rebind chokepoint: a thin `Querier` wrapper over `*sql.DB`/`*sql.Tx` that
  rebinds `?`→`$N` once, so the ~95 query sites across the store and its 6
  sibling packages need no per-call edits.
- A hand-written Postgres migration set (`migrations_postgres/`).
- Backend selection by the `postgres`/`postgresql` schemes on the existing
  `--auth-db` value; no new flag.
- Postgres password resolved **off the CLI**: `BUCKETVCS_DB_AUTH_TOKEN` env
  (precedence), then standard libpq (`PGPASSWORD`/`.pgpass`); a password in the
  URL is allowed but warns.
- A backend-parametrized `Store` conformance suite asserting identical behavior
  on SQLite, libSQL, and Postgres.
- Operator guide section + caveats.

### 1.2 Out of scope (deferred to B2 or beyond)

- **Multi-node concurrency hardening** (race-safe webhook claiming, DB-level
  quota idempotency, deferred-FK rename, `MaxOpenConns > 1`) — **B2**.
- **Renaming the `internal/auth/sqlitestore` package** — now a misnomer (hosts
  sqlite + libsql + postgres), but renaming touches all 46 consumers; YAGNI,
  documented (§8).
- **Converting boolean-as-INTEGER columns to native `BOOLEAN`** — kept as
  `INTEGER` with `CHECK (… IN (0,1))` to avoid Go scan-side changes (lowest-risk
  port).
- **Migrating existing SQLite data into Postgres** — operators do that out of
  band (`pg_dump`/manual load); documented, not automated.
- **Native `TIMESTAMPTZ` columns** — timestamps stay unix-int64 in `BIGINT`
  columns (the existing convention ports trivially; no read-side changes).

## 2. Architecture overview

```
--auth-db value ──► resolveBackend(value) ──► Backend
                       │   sqlite / libsql (Phase A)  ├─ sqliteBackend (modernc, default)
                       │                              ├─ libsqlBackend (pure-Go libSQL)
                       │   postgres / postgresql ─────┴─ postgresBackend (pgx stdlib)   ← NEW
                       ▼
   sqlitestore.Open(value) ──► backend.Open() (*sql.DB) ──► RunMigrations(db, backend)
                                                               └─ backend.ApplyMigration(tx, body)
   Store{db: Querier{*sql.DB, backend}, backend}
     ├─ all ~45 methods + 6 sibling packages: SQL UNCHANGED; Querier rebinds ?→$N
     ├─ divergent constructs via backend dialect helpers (NowSeconds, Greatest, InsertReturningID)
     └─ error helpers dispatch: backend.IsUniqueViolation / backend.IsCheckViolation
```

The seam lives inside `internal/auth/sqlitestore`. The `auth.Store` interface and
every consumer of `*sqlitestore.Store` are untouched.

### 2.1 `Backend` interface (extended)

`internal/auth/sqlitestore/backend.go`:

```go
type Backend interface {
    // Name reports the backend for logging: "sqlite" | "libsql" | "postgres".
    Name() string
    // Open opens the *sql.DB with the driver, DSN, and pool config for this
    // backend. It does NOT run migrations.
    Open() (*sql.DB, error)
    // ApplyMigration executes one migration file body within tx.
    ApplyMigration(tx *sql.Tx, body string) error

    // --- added in B1 ---

    // Rebind converts a ?-placeholder query to the backend's placeholder style.
    // sqlite/libsql: identity. postgres: ?→$1,$2,…  Literal-aware (skips ? inside
    // single-quoted string literals).
    Rebind(query string) string
    // IsUniqueViolation / IsCheckViolation classify constraint errors. sqlite/
    // libsql: substring match (unchanged from Phase A free functions). postgres:
    // SQLSTATE 23505 / 23514 via errors.As(&pgconn.PgError).
    IsUniqueViolation(err error) bool
    IsCheckViolation(err error) bool

    // Plus the dialect-SQL helpers defined in §2.3 (NowSeconds, Greatest,
    // InsertReturningID) — also Backend methods, kept in §2.3 for readability.
}
```

The package-level free functions `isUniqueViolation`/`isCheckViolation` and the
duplicated copies in `internal/webhooks/service.go` and
`internal/lfs/locks/store.go` are replaced by dispatch through the backend. The
`Querier` wrapper (§2.2) also exposes `IsUniqueViolation`/`IsCheckViolation`
(delegating to its backend), so a sibling package that holds a `Querier` can
classify errors without a separate backend reference.
`isFingerprintUniqueViolation` becomes constraint-name-aware: postgres reads
`pgconn.PgError.ConstraintName == "ssh_keys_fingerprint_key"` (or the named
unique constraint); sqlite/libsql keep the existing substring check.

### 2.2 The `Querier` rebind chokepoint

```go
// Querier is the SQL-access surface the store and its sibling packages use.
// Both *sql.DB and *sql.Tx are wrapped so every query is rebinding-aware.
type Querier interface {
    ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
    QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// dbWrap wraps *sql.DB + backend; txWrap wraps *sql.Tx + backend. Each method
// calls backend.Rebind(query) before delegating to the embedded handle. A
// BeginTx helper returns a txWrap. Both wraps also expose IsUniqueViolation /
// IsCheckViolation (delegating to backend) so error classification travels with
// the handle (see §2.1).
```

`Store.DB()` returns the `Querier` (today it returns `*sql.DB`). The 6 sibling
packages (`internal/webhooks`, `internal/policy`, `internal/hooks`,
`internal/lfs/locks`, `internal/lfs/quota`) change the type of the handle they
receive from `*sql.DB` to `Querier`; their SQL strings are unchanged. For
sqlite/libsql, `Rebind` is identity, so their behavior is byte-for-byte today's.

Code paths that need transactions (e.g. webhook claim, `RenameRepo`,
`RunMigrations`) use the `txWrap` so statements inside a tx are rebinding-aware
too. `RunMigrations` already takes the backend and applies via
`ApplyMigration`, which for postgres uses the Postgres migration set verbatim
(no rebind needed — migration SQL is authored per-dialect).

### 2.3 Divergent-construct helpers

Most divergences collapse to a single shared form; only three need a per-backend
helper, exposed as methods on `Backend` (sqlite/libsql return the SQLite form,
postgres the Postgres form):

- `INSERT OR IGNORE` → rewritten **once** to `INSERT … ON CONFLICT(<cols>) DO
  NOTHING`, which both SQLite and Postgres accept. No per-backend branch. Sites:
  `RegisterRepo`, `RegisterRepoIfNew` (`RowsAffected()==0` on conflict semantics
  hold on both).
- `NowSeconds() string` → `"strftime('%s','now')"` (sqlite/libsql) vs
  `"EXTRACT(EPOCH FROM now())::bigint"` (postgres). ~4 production SQL sites
  (`RevokeSSHKey`, `TouchSSHKeyUsage`, `AddOIDCIssuer`, `AddOIDCRule`). Migration
  footers are authored per-dialect in their own migration sets, so they do not
  use this helper.
- `Greatest(expr, floor string) string` → `"MAX(<expr>, <floor>)"` (sqlite/
  libsql scalar form) vs `"GREATEST(<expr>, <floor>)"` (postgres). 1 site (quota
  `Subtract` clamp).
- `InsertReturningID(...)` → sqlite/libsql use `LastInsertId()`; postgres appends
  `RETURNING id` and scans via `QueryRowContext`. 1 site (`webhook_endpoints`
  insert in `internal/webhooks/service.go`). Modeled as a small backend method so
  the call site is dialect-agnostic.

## 3. `postgresBackend`

`internal/auth/sqlitestore/backend_postgres.go`.

**Driver:** `github.com/jackc/pgx/v5` registered via its `stdlib` adapter
(`database/sql` driver name `"pgx"`). Pure-Go → builds under `CGO_ENABLED=0` for
all five release targets (verified in Task 0). New direct dependency.

**Open / DSN:** parse the `postgres://`/`postgresql://` URL. Password resolution
order (kept off the CLI):
1. `BUCKETVCS_DB_AUTH_TOKEN` env (precedence; injected as the password);
2. standard libpq mechanisms honored by pgx (`PGPASSWORD`, `~/.pgpass`,
   `PGSERVICEFILE`);
3. a password embedded in the URL — allowed for dev, but logs a WARN that it is
   visible to other processes.

`sql.Open("pgx", dsn)`, then `db.SetMaxOpenConns(1)` (single-node; B2 raises
it), then `db.PingContext` to fail fast on a bad DSN/credentials.

**ApplyMigration:** apply the Postgres migration body per-statement within the
caller's `tx` using the Phase A `splitSQLStatements` splitter (the Postgres
migrations contain no triggers / `BEGIN…END` / `;`-in-literal, asserted by a
unit test).

**Rebind:** literal-aware `?`→`$N` (walk the string, increment a counter for each
`?` not inside a single-quoted literal). A unit test asserts every query string
the store issues round-trips, and that `?` inside a literal is left intact.

**Error classification:** `errors.As(err, &pgErr)` then `pgErr.Code == "23505"`
(unique_violation) / `"23514"` (check_violation). `isFingerprintUniqueViolation`
reads `pgErr.ConstraintName`.

## 4. Postgres migration set

`internal/auth/sqlitestore/migrations_postgres/*.sql` (embedded), a hand-written
translation of the 10 SQLite migrations. Per-file divergences:

- `BLOB` → `BYTEA` (`ssh_keys.public_key`, `webhook_deliveries.payload_json`).
- `INTEGER PRIMARY KEY AUTOINCREMENT` → `BIGINT GENERATED BY DEFAULT AS IDENTITY
  PRIMARY KEY` (`webhook_endpoints.id`).
- `strftime('%s','now')` in `schema_version` seed rows → `EXTRACT(EPOCH FROM
  now())::bigint`.
- reserved column `trigger` (hooks table) → quoted `"trigger"` in DDL; the Go
  queries that name it are routed through `Rebind`'s sibling: they must quote it
  for postgres. Since the column name is fixed, the queries use `"trigger"` which
  SQLite also accepts (double-quoted identifiers are valid in both), so the
  query strings can be unified to `"trigger"` once.
- boolean columns stay `INTEGER` with `CHECK (col IN (0,1))` — no Go changes.
- CHECK constraints and the partial index (`… WHERE status='pending'`) port
  as-is (identical syntax).
- timestamps stay `BIGINT` (unix seconds).
- **all FKs declared `DEFERRABLE INITIALLY DEFERRED`** — unused by B1 but
  required by B2's `RenameRepo`; harmless now, saves a B2 migration.
- the reserved `_oidc` user seed row (migration 0010) ports unchanged.
- `schema_version` table + per-migration seed mirrors the SQLite set so
  `RunMigrations`' version gating works identically.

The SQLite/libSQL migrations under `migrations/` are unchanged.

## 5. Backend selection

`resolveBackend` (extended):

| `value` scheme | Backend |
|----------------|---------|
| bare path, `file:`, `sqlite:` | `sqliteBackend` (default) |
| `libsql://`, `http(s)://` | `libsqlBackend` |
| `postgres://`, `postgresql://` | `postgresBackend` (NEW) |

`Open(value string)` signature is unchanged, so all 46 consumers compile
untouched.

## 6. Data flow (unchanged for callers)

No consumer of the store changes. `cmd/bucketvcs` subcommands, the gateway, the
LFS locks/quota stores, the webhook worker, policy/hooks enforcement, and tests
all call `sqlitestore.Open(value)` and receive a `*Store` whose methods behave
identically regardless of backend. Selecting Postgres is purely operator config
(`--auth-db postgres://…` + a password env var).

## 7. Error handling

- **Bad/unreachable DSN or credentials** → `Open` (via `PingContext`) fails fast
  with the wrapped pgx error; callers already surface `Open` errors.
- **Migration failure over Postgres** → the per-statement Exec error is returned
  with the migration filename, rolled back within the tx.
- **UNIQUE/CHECK** classification dispatches per backend (SQLSTATE on postgres);
  all existing `ErrConflict` / constraint-mapping behavior is preserved on all
  three backends (proven by the conformance suite).

## 8. B1 caveats (documented in spec + operator guide)

1. **Single-node assumed** — `MaxOpenConns(1)` preserves the single-writer
   serialization the current webhook-claim and quota-ring code rely on. Running
   multiple gateway nodes against one Postgres DB is **not yet race-safe** — that
   is B2. Deploy a single gateway node with B1.
2. **`MaxOpenConns(1)`** — throughput-bounded; B2 revisits.
3. **Rate limiter stays per-node** (M18, in-memory).
4. **Package name** `internal/auth/sqlitestore` is a misnomer (hosts three
   backends); not renamed in B1 (touches 46 consumers); documented.

## 9. Testing

### 9.1 Per-commit unit tests (no DB; run in `ci.yml`)
- `Rebind` (postgres): every store query string round-trips `?`→`$N`; `?` inside
  a single-quoted literal is left intact; sqlite/libsql `Rebind` is identity.
- `resolveBackend`: `postgres://` / `postgresql://` → postgres; existing
  sqlite/libsql cases unchanged.
- Password resolution: `BUCKETVCS_DB_AUTH_TOKEN` precedence; PG env fallback;
  URL-embedded password accepted with a WARN.
- SQLSTATE classifiers against fixture `pgconn.PgError{Code: "23505"/"23514"}`.
- `splitSQLStatements` against the 10 Postgres migration files (expected
  statement counts; no empty/comment-only statements).

### 9.2 Backend conformance suite (gated; nightly)
- The existing behavioral suite, parametrized to also run against Postgres,
  exercising: user/token lifecycle, repo register/grant/perm/public/rename (FK
  cascade), protected refs/paths, hooks CRUD, webhook enqueue/claim/replay,
  quota add/decrement/reconcile, OIDC issuer/rule CRUD + mint + sweep, LFS locks.
  Asserts identical results on sqlite, libsql, and postgres, including
  UNIQUE→`ErrConflict`, live CHECK→`IsCheckViolation`, and FK cascade.
- Postgres target: **`postgres:17` in a container** (GitHub Actions service),
  selected via build tag and/or `BUCKETVCS_POSTGRES_URL`. Runs in the **nightly
  conformance workflow**, not per-commit.

### 9.3 Cross-compile gate
- The 5-target `CGO_ENABLED=0` matrix must stay green with pgx added (confirms
  pure-Go).

## 10. Acceptance criteria

- `--auth-db postgres://…` + a password env var brings up a working gateway and
  CLI against a Postgres instance; `--auth-db <path>` / `libsql://…` are
  unchanged.
- The `Store` conformance suite passes identically on `sqliteBackend`,
  `libsqlBackend`, and `postgresBackend`.
- All 10 migrations apply cleanly over Postgres.
- `go build ./...` and the 5-target `CGO_ENABLED=0` cross-build stay green.
- Per-commit `ci.yml` stays fast/green (Postgres integration is nightly-gated).
- No change to any store consumer; SQLite remains the zero-dependency default.

## 11. Task 0 spike (de-risk before building)

A short investigation, recorded in the plan, pinning the pgx behaviors B1
depends on:

1. pgx (stdlib adapter) builds under `CGO_ENABLED=0` for linux/darwin
   (amd64+arm64) and windows/amd64.
2. The exact `pgconn.PgError.Code` values returned for UNIQUE / CHECK violations
   (expect `23505` / `23514`) and the `ConstraintName` for the ssh_keys
   fingerprint unique constraint.
3. That a per-statement migration apply over pgx-stdlib works for the Postgres
   migration set (and whether multi-statement Exec is accepted — ship the
   splitter regardless).
4. That `MaxOpenConns(1)` + `database/sql` `BeginTx` give the transactional
   semantics the webhook claim / rename paths assume.

## 12. Open questions (resolved by Task 0 / sensible defaults)

- **pgx version pin** — pick the current stable `jackc/pgx/v5` tag during
  implementation.
- **`postgres:N` container tag for the conformance job** — pin to the current
  stable major (17) during implementation.
- **Exact SQLSTATE / constraint-name wording** — Task 0 captures it; classifier
  matches it.
