# M23 (Phase A): Turso / libSQL metadata backend

Date: 2026-05-26
Status: design

## 1. Goals

Let operators back the bucketvcs **metadata/auth database** (the M4 store at
`internal/auth/sqlitestore` — users, tokens, repos, permissions, protected
refs/paths, hooks, webhooks, OIDC rules, LFS locks, quotas) with **Turso /
libSQL** instead of a local SQLite file, selected at runtime. Git object data
is unaffected: it stays in object storage (S3/R2/GCS/Azure/localfs).

This is **Phase A** of a two-phase effort:

- **Phase A (this spec):** introduce a `Backend` seam and add a libSQL/Turso
  backend. Because libSQL speaks SQLite's SQL dialect, the existing SQL and
  migrations run essentially unchanged; the work is driver wiring, migration
  statement-splitting, connection setup, and error classification.
- **Phase B (separate spec, later):** add a PostgreSQL backend (which brings a
  SQL-dialect layer — timestamps, upserts, autoincrement, `$N` placeholders,
  FK deferral, SQLSTATE error codes) **and** the multi-node concurrency
  hardening (e.g. `SELECT … FOR UPDATE SKIP LOCKED` for webhook claiming,
  DB-atomic quota updates) that a shared DB with multiple gateway nodes
  requires. The `Backend` seam built here is the extension point.

### 1.1 In scope (Phase A)

- A `Backend` interface in `internal/auth/sqlitestore` capturing the driver-
  specific concerns: open/connect + pool config, migration application, and
  UNIQUE/CHECK error classification.
- Two implementations: `sqliteBackend` (modernc — exactly today's behavior,
  the default) and `libsqlBackend` (pure-Go libSQL client).
- Backend selection by the scheme of the existing `--auth-db` value; no new
  selection flag. All CLI subcommands and `serve` gain Turso support for free.
- Auth token via `BUCKETVCS_DB_AUTH_TOKEN` env (never a CLI argument).
- A backend-specific migration applier (sqlite: whole-body Exec unchanged;
  libsql: statement-split within the migration transaction).
- A backend-parametrized `Store` **conformance suite** asserting identical
  behavior on SQLite and libSQL.
- Operator guide section + caveats.

### 1.2 Out of scope (deferred)

- **PostgreSQL backend** and the **SQL-dialect layer** — Phase B.
- **Multi-node concurrency hardening** (race-safe webhook claiming, quota
  updates) — Phase B. Phase A assumes single-node deployments (see §8).
- **Embedded libSQL replicas / offline mode** — requires the cgo `go-libsql`
  driver, which would break the `CGO_ENABLED=0` cross-compilation the release
  pipeline depends on. Phase A is **remote libSQL only**.
- **Connection pooling > 1** — Phase A keeps `MaxOpenConns(1)` to preserve the
  single-writer serialization the current code assumes. Phase B revisits.
- **Distributed rate limiting** — the M18 limiter is in-memory per-node and
  stays per-node regardless of DB backend.
- **Migrating existing SQLite data into Turso** — operators do that out of
  band (e.g. `turso db shell … < dump.sql`); documented, not automated.

## 2. Architecture overview

```
--auth-db value ──► resolveBackend(value) ──► Backend
                       │                         ├─ sqliteBackend (modernc, default)
                       │                         └─ libsqlBackend (pure-Go libSQL)
                       ▼
   sqlitestore.Open(value) ──► backend.Open() (*sql.DB) ──► RunMigrations(db, backend)
                                                               └─ backend.ApplyMigration(tx, body)
   Store{db, backend}
     ├─ all ~45 methods: hand-written SQL UNCHANGED (libSQL = SQLite dialect)
     └─ error helpers dispatch: backend.IsUniqueViolation / IsCheckViolation
```

The seam lives inside `internal/auth/sqlitestore` (its only consumer). No new
package. The `auth.Store` interface and every consumer of `*sqlitestore.Store`
are untouched.

### 2.1 The `Backend` interface

`internal/auth/sqlitestore/backend.go`:

```go
// Backend abstracts the driver-specific concerns that differ between the
// SQLite (modernc) and libSQL (Turso) backends. Phase B adds a postgres
// implementation plus a SQL-dialect helper for the divergent statements.
type Backend interface {
    // Name reports the backend for logging: "sqlite" | "libsql".
    Name() string
    // Open opens the *sql.DB with the driver, DSN, and pool config for this
    // backend. It does NOT run migrations.
    Open() (*sql.DB, error)
    // ApplyMigration executes one migration file body within tx. sqlite execs
    // the whole multi-statement body; libsql splits into statements.
    ApplyMigration(tx *sql.Tx, body string) error
    // IsUniqueViolation reports whether err is a UNIQUE-constraint failure.
    IsUniqueViolation(err error) bool
    // IsCheckViolation reports whether err is a CHECK-constraint failure.
    IsCheckViolation(err error) bool
}
```

`Store` gains a `backend Backend` field. The package-level helpers
`isUniqueViolation(err)` and the inline CHECK detection in `store.go` are
replaced by calls to `s.backend.IsUniqueViolation(err)` /
`s.backend.IsCheckViolation(err)`.

### 2.2 `Open` and backend selection

`Open` keeps its single-string signature (so every existing caller is
unchanged) and resolves the backend from the value's scheme:

```go
func Open(value string) (*Store, error) {
    b, err := resolveBackend(value)
    if err != nil { return nil, err }
    db, err := b.Open()
    if err != nil { return nil, err }
    if err := RunMigrations(db, b); err != nil {
        _ = db.Close()
        return nil, fmt.Errorf("migrate: %w", err)
    }
    slog.Default().Info("authdb opened", "backend", b.Name())
    return &Store{db: db, backend: b}, nil
}
```

`resolveBackend`:

| `value` | Backend |
|---------|---------|
| bare path, `file:…`, `sqlite:…` | `sqliteBackend{path}` (default) |
| `libsql://…`, `https://…` (turso) | `libsqlBackend{url, token}` |

Resolution rule: parse as a URL; if the scheme is `libsql`/`http`/`https`,
use `libsqlBackend`; otherwise (no scheme, `file`, `sqlite`, or a Windows
drive path like `C:\…`) treat the whole value as a filesystem path →
`sqliteBackend`. This preserves today's behavior for all existing callers.

## 3. `sqliteBackend` (default, behavior-preserving)

`Open` builds today's modernc DSN exactly as it does now (the `file:` URL with
`_pragma` query params for `journal_mode(WAL)`, `foreign_keys(1)`,
`busy_timeout(5000)`) and sets `MaxOpenConns(1)`. `ApplyMigration` is the
current whole-body `tx.Exec(body)` — **unchanged**, zero risk to the proven
path. `IsUniqueViolation`/`IsCheckViolation` keep the current modernc message
matching (the `"UNIQUE constraint failed"` / `"CHECK constraint failed"`
substring checks already in `store.go`).

## 4. `libsqlBackend`

`internal/auth/sqlitestore/backend_libsql.go`.

**Driver:** `github.com/tursodatabase/libsql-client-go/libsql` — the pure-Go
client (registers the `libsql` database/sql driver; HTTP/WebSocket to
sqld/Turso). New direct dependency. **Must build under `CGO_ENABLED=0` for all
five release targets** (pure Go; the cgo driver is the separate `go-libsql`
package, which we do NOT use). Verified in Task 0.

**Open / DSN:** the libSQL URL plus the auth token. Token resolution order:
1. `BUCKETVCS_DB_AUTH_TOKEN` env (documented path, takes precedence);
2. an `authToken` query param already on the URL (dev convenience);
3. none + a `libsql`/turso host → fail fast: `"libsql auth token required: set BUCKETVCS_DB_AUTH_TOKEN"`.

`MaxOpenConns(1)` in Phase A (preserve single-writer serialization).

**Connection setup:** remote sqld ignores the modernc `_pragma` DSN syntax, so
after connect the backend issues `PRAGMA foreign_keys = ON` as a statement
(sqld honors it). Confirmed in Task 0.

**ApplyMigration:** libSQL-over-HTTP may not accept a multi-statement Exec, so
this splits the file body into individual statements and `Exec`s each within
the caller's `tx`. The splitter (`splitSQLStatements(body) []string`) is
conservative and handles exactly the SQL our migrations use: it splits on
statement-terminating `;`, and there are **no** triggers / `BEGIN…END` blocks
and **no** `;` inside string literals across all 10 migration files (asserted
by a unit test). It strips comments and blank statements.

**Error classification:** sqld surfaces the underlying SQLite errors, so the
libSQL driver's error strings are expected to contain the same
`"UNIQUE constraint failed"` / `"CHECK constraint failed"` substrings — Task 0
captures the exact wording. `IsUniqueViolation`/`IsCheckViolation` match the
libSQL form (still substring matching; no new mechanism). If the wording turns
out identical to modernc, the two backends share one matcher.

## 5. Task 0 spike (de-risk before building)

A short investigation, recorded in the plan, that pins the libSQL driver's
behaviors the design depends on:

1. The driver builds under `CGO_ENABLED=0` for linux/darwin (amd64+arm64) and
   windows/amd64.
2. Whether `Exec` accepts a multi-statement string (determines whether the
   splitter is strictly required or merely defensive — we ship it either way).
3. sqld's default FK enforcement and that `PRAGMA foreign_keys = ON` is
   accepted as a statement.
4. The exact UNIQUE/CHECK error strings the driver returns.

The seam accommodates either outcome of (2) and (4); the spike removes
guesswork from the implementation tasks.

## 6. Data flow (unchanged for callers)

No consumer of the store changes. `cmd/bucketvcs` subcommands (`user`, `token`,
`repo`, `policy`, `webhook`, `quota`, `oidc`, `serve`), the gateway, the LFS
locks store, and tests all call `sqlitestore.Open(value)` and receive a
`*Store` whose methods behave identically regardless of backend. Selecting
Turso is purely an operator config change (`--auth-db libsql://…` +
`BUCKETVCS_DB_AUTH_TOKEN`).

## 7. Error handling

- **Missing/invalid token** for a libSQL URL → `Open` fails fast with an
  actionable message; no partial state.
- **Connect failure** (network, bad host, expired token) → `Open` returns the
  driver error wrapped with context; callers already surface `Open` errors.
- **Migration failure** over libSQL → the per-statement Exec error is returned
  with the migration filename (as today), rolled back within the tx.
- **UNIQUE/CHECK** classification dispatches per backend; all existing
  `ErrConflict` / constraint-mapping behavior is preserved on both backends
  (proven by the conformance suite).

## 8. Phase-A caveats (documented in spec + operator guide)

1. **Remote libSQL only** — no embedded replica / offline mode (pure-Go /
   `CGO_ENABLED=0` constraint).
2. **Single-node assumed** — multi-node-safe webhook claiming and quota updates
   are Phase B; running multiple gateway nodes against one Turso DB is not yet
   race-safe.
3. **`MaxOpenConns(1)`** — preserves single-writer serialization; throughput-
   bounded; revisited in Phase B.
4. **Rate limiter stays per-node** (M18, in-memory).

## 9. Testing

### 9.1 Per-commit unit tests (no DB; run in `ci.yml`)
- `splitSQLStatements` against all 10 migration files: asserts each splits into
  the expected statement count and that no statement is empty/comment-only.
- `resolveBackend`: path / `file:` / `sqlite:` → sqlite; `libsql://` /
  `https://…turso…` → libsql; Windows drive path → sqlite.
- Token resolution: env precedence, URL-embedded token, missing-token error.
- Error classifiers against captured fixture error values for both backends.

### 9.2 Backend conformance suite (gated; nightly)
- A `Store` conformance suite parametrized over `sqliteBackend` and
  `libsqlBackend`, exercising: user/token lifecycle (create, verify, rotate,
  expire, disable), repo register/grant/perm/public/rename (FK cascade),
  protected refs/paths, hooks CRUD, webhook enqueue/claim/replay, quota
  add/decrement/reconcile, OIDC issuer/rule CRUD + mint + sweep, LFS locks.
  Asserts identical results on both backends.
- libSQL target: **sqld in a container** (self-provisioned, like the MinIO/
  Azurite emulator conformance), selected via build tag `//go:build libsql`
  and/or `BUCKETVCS_LIBSQL_URL`. Runs in the **nightly conformance workflow**,
  not per-commit, keeping per-commit CI fast and green.

### 9.3 Cross-compile gate
- The release/CI cross-build matrix must stay green with the libSQL driver
  added (confirms pure-Go, `CGO_ENABLED=0`).

## 10. Acceptance criteria

- `--auth-db libsql://…` + `BUCKETVCS_DB_AUTH_TOKEN` brings up a working
  gateway and CLI against a libSQL/sqld instance; `--auth-db <path>` is
  byte-for-byte today's behavior.
- The `Store` conformance suite passes identically on `sqliteBackend` and
  `libsqlBackend`.
- All 10 migrations apply cleanly over libSQL.
- `go build ./...` and the 5-target `CGO_ENABLED=0` cross-build stay green.
- Per-commit `ci.yml` stays fast/green (libSQL integration is nightly-gated).
- No change to any store consumer; SQLite remains the zero-dependency default.

## 11. Open questions (resolved by Task 0 / sensible defaults)

- **Multi-statement Exec support in the libSQL driver** — ship the splitter
  regardless; Task 0 decides whether it's required or defensive.
- **Exact libSQL error-string wording** — Task 0 captures it; classifier
  matches it (shared matcher if identical to modernc).
- **sqld container image/version for the conformance job** — pick the current
  stable `ghcr.io/tursodatabase/libsql-server` tag during implementation.
