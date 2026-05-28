# M23 B1 — PostgreSQL metadata backend (operator guide)

This guide covers M23 Phase B1: backing the BucketVCS **metadata/auth database**
with [PostgreSQL](https://www.postgresql.org) instead of a local SQLite file. It
explains what shipped, how to enable it, how backend selection works, the B1
caveats you must understand before deploying, and how to migrate existing data.

Production readiness summary:

- PostgreSQL backend for the auth DB, selected by `--auth-db` URL scheme — **shipped**.
- Pure-Go driver (`pgx/v5` stdlib adapter); `CGO_ENABLED=0` cross-builds stay green — **shipped**.
- Password via `BUCKETVCS_DB_AUTH_TOKEN` env or standard libpq (`PGPASSWORD`/`~/.pgpass`); a URL-embedded password is accepted with a WARN — **shipped**.
- All 10 migrations apply over Postgres; behavioral parity proven by a gated conformance suite — **shipped**.
- SQLite (modernc, pure-Go) remains the zero-dependency default — **unchanged**.
- Multi-node concurrency hardening (race-safe webhook claiming, DB-level quota idempotency, `MaxOpenConns > 1`) — **Phase B2 (not shipped)**.

---

## 1. Overview

BucketVCS keeps two very different kinds of state:

- **Git object data** — packs, indexes, bundles, LFS objects — lives in object
  storage (S3 / R2 / GCS / Azure / localfs). M23 does **not** touch this.
- **Metadata / auth** — users, tokens, repos, permissions, protected
  refs/paths, hooks, webhooks, OIDC issuers/rules, LFS locks, quotas — lives in
  the auth DB. This is what M23 B1 lets you move to PostgreSQL.

Because B1 introduces a SQL-dialect layer (`?`→`$N` rebinding, SQLSTATE error
classification, and a handful of divergent-construct helpers), the existing store
methods run on Postgres without per-call edits. Selecting Postgres is purely an
operator configuration change; no data model or consumer behavior changes.

**Why bother?** A local SQLite file ties the gateway's auth state to one host's
disk. Pointing the auth DB at a Postgres instance decouples that state from the
gateway host, which simplifies backups, host replacement, and (in Phase B2)
multi-node deployments.

**Single-node only in B1.** `MaxOpenConns(1)` is kept, matching the M23 Phase A
(libSQL) posture. Multi-node-safe webhook claiming and DB-level quota idempotency
are B2; see §4.

---

## 2. Enabling

The backend is chosen by the **scheme of the existing `--auth-db` value** —
there is no new selection flag, and the change applies to `serve` and to every
`bucketvcs` subcommand that takes `--auth-db` (`user`, `token`, `repo`,
`policy`, `webhook`, `quota`, `oidc`).

### 2.1 Worked example

```bash
# 1. Create a database and a role.
createdb bucketvcs_auth
psql -c "CREATE ROLE bv LOGIN;"
psql -c "GRANT ALL ON DATABASE bucketvcs_auth TO bv;"

# 2. Export the password (NEVER pass it on the command line — it would leak via ps).
export BUCKETVCS_DB_AUTH_TOKEN='<strong-password>'

# 3. Point any subcommand or the gateway at the Postgres URL.
bucketvcs serve \
  --auth-db 'postgres://bv@db.internal:5432/bucketvcs_auth?sslmode=require' \
  ...
```

On startup the gateway logs the backend it selected:

```
INFO authdb opened backend=postgres
```

(For the default SQLite path this reads `backend=sqlite`; for Turso/libSQL it
reads `backend=libsql`.) Migrations are applied automatically the first time the
DB is opened, exactly as with SQLite.

### 2.2 Password resolution order

The password is resolved **off the command line** to avoid exposure via `ps` or
shell history:

1. `BUCKETVCS_DB_AUTH_TOKEN` env (preferred; takes precedence over all others).
2. Standard libpq mechanisms honored by pgx: `PGPASSWORD` env, `~/.pgpass`,
   `PGSERVICEFILE`.
3. A password embedded in the URL — allowed for dev convenience, but logs a
   WARN at startup because it is visible to other processes via `/proc` or `ps`.

The token is **never** accepted as a CLI argument.

### 2.3 TLS / sslmode

For production use, include `?sslmode=require` (or `verify-full`) in the URL to
encrypt the connection between the gateway and the Postgres server. For local
development or Docker-internal setups, `?sslmode=disable` is accepted.

---

## 3. How backend selection works

`--auth-db` is parsed as a URL and dispatched by scheme:

| `--auth-db` value | Backend | Notes |
|-------------------|---------|-------|
| bare path (`/var/lib/bucketvcs/auth.db`) | SQLite (default) | |
| `file:/path/to/auth.db` | SQLite | |
| `sqlite:/path/to/auth.db` | SQLite | |
| `C:\data\auth.db` (Windows drive path) | SQLite | |
| `libsql://<db>.turso.io` | libSQL (Turso) | |
| `https://<db>.turso.io` | libSQL (Turso) | |
| `http://sqld.internal:8080` | libSQL (self-hosted) | |
| `postgres://user@host:5432/db` | PostgreSQL | B1 |
| `postgresql://user@host:5432/db` | PostgreSQL | B1 |

Password resolution for the PostgreSQL backend is described in §2.2. The
password is **never** accepted as a CLI argument.

---

## 4. B1 caveats (read before deploying)

1. **Single-node only.** Multi-node-safe webhook claiming (`SELECT … FOR UPDATE
   SKIP LOCKED`) and DB-level quota verify-replay idempotency are **Phase B2**.
   Running multiple gateway nodes against one Postgres DB is **not yet
   race-safe** — deploy a single gateway node with B1.
2. **`MaxOpenConns(1)`.** B1 keeps one connection to preserve the single-writer
   serialization the current webhook-claim and quota-ring code relies on. This
   bounds throughput; B2 revisits pooling.
3. **Rate limiter stays per-node.** The M18 credential-failure limiter is
   in-memory per gateway node regardless of DB backend.
4. **Package name is a historical misnomer.** The implementation lives in
   `internal/auth/sqlitestore`, which now hosts three backends (sqlite, libsql,
   postgres). The package is not renamed in B1 (renaming would touch all 46
   consumers); this is documented and will be addressed separately.

---

## 5. Migrating existing SQLite data into Postgres

Migration is done out of band; M23 B1 does not automate it. Two approaches:

### 5.1 Fresh Postgres DB, re-create via CLI (recommended for small deployments)

```bash
# 1. Stand up an empty Postgres DB and point --auth-db at it.
#    Migrations apply automatically on first open.
export BUCKETVCS_DB_AUTH_TOKEN='<password>'
bucketvcs user list --auth-db 'postgres://bv@host/bucketvcs_auth?sslmode=require'

# 2. Re-create users, tokens, repos, policies, and webhooks using the
#    bucketvcs CLI — the same commands you used to set up SQLite.
bucketvcs user add --auth-db 'postgres://...' alice
bucketvcs token create --auth-db 'postgres://...' --user alice --scopes repo:write
# ...and so on for each object.
```

This is safe, auditable, and avoids any data-type mismatch between SQLite and
Postgres.

### 5.2 Dump-and-adapt (for larger deployments)

```bash
# 1. Dump the existing SQLite auth DB.
sqlite3 /var/lib/bucketvcs/auth.db .dump > authdump.sql
```

Hand-adapt the dump before loading into Postgres:

- `BLOB` columns → `BYTEA` (affects `ssh_keys.public_key` and
  `webhook_deliveries.payload_json`).
- `INTEGER PRIMARY KEY AUTOINCREMENT` → `BIGINT GENERATED BY DEFAULT AS
  IDENTITY PRIMARY KEY` (affects `webhook_endpoints.id`).
- `strftime('%s','now')` expressions → `EXTRACT(EPOCH FROM now())::bigint`.
- SQLite-specific pragmas and `CREATE TABLE IF NOT EXISTS` idioms may need
  adjustment for Postgres.

```bash
# 2. Load the adapted dump into the Postgres database.
psql -d bucketvcs_auth < authdump_pg.sql

# 3. Point --auth-db at the Postgres URL and restart.
```

Because the Postgres migrations have already been applied by step 1, the
`schema_version` table will gate duplicate migration application; verify its
contents after loading to confirm all 10 migrations are recorded.

---

## 6. Verifying

- **Startup log:** `authdb opened backend=postgres` confirms the Postgres backend
  was selected.
- **Smoke:** create a user and token against the Postgres URL and authenticate a
  `git ls-remote` — behavior is identical to SQLite:

  ```bash
  export BUCKETVCS_DB_AUTH_TOKEN='<password>'
  bucketvcs user add --auth-db 'postgres://bv@host/bucketvcs_auth?sslmode=require' alice
  bucketvcs token create --auth-db 'postgres://...' --user alice --scopes repo:read
  git ls-remote https://alice:<token>@<gateway>/tenant/repo.git
  ```

- **CI:** the nightly `conformance` workflow runs a gated suite
  (`go test -tags postgres -run TestPostgresConformance`) against a pinned
  `postgres:17` container, asserting that all 10 migrations apply, UNIQUE
  constraint violations map to `ErrConflict`, CHECK constraints are enforced,
  FK cascade works on repo deletion, and token + OIDC round-trips all match
  SQLite behavior.

---

## 7. Phase B2 (planned, not in this release)

B1 front-loads the schema changes B2 requires (all foreign keys are declared
`DEFERRABLE INITIALLY DEFERRED` in the Postgres migration set) so B2 will
require no migration edits. The planned B2 work builds directly on B1:

- **Multi-node webhook claiming** via `SELECT … FOR UPDATE SKIP LOCKED`,
  removing the single-writer assumption in the claim path.
- **DB-level quota idempotency** so verify-replay under concurrent pushes is
  atomic at the database level rather than ring-locked in memory.
- **Connection pooling** beyond `MaxOpenConns(1)`.
- **Documentation** of the per-node M18 rate limiter in a multi-gateway context.

The `Backend` seam and `Querier` wrapper introduced in M23 are the extension
points B2 builds on.
