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
- Multi-node concurrency hardening (race-safe webhook claiming, DB-level quota idempotency, `--auth-db-max-conns` pool sizing) — **shipped in Phase B2** (see `docs/multinode.md`).

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

**Multi-node restriction lifted in B2.** B1 kept `MaxOpenConns(1)` for
single-writer correctness. Phase B2 lifts this for the PostgreSQL backend:
multi-node deployments are now supported via `--auth-db-max-conns` (default 10),
`FOR UPDATE SKIP LOCKED` webhook claiming, and `quota_credits` idempotency. See
`docs/multinode.md`. SQLite and libSQL remain single-node.

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

1. **Multi-node restriction lifted in B2 (PostgreSQL only).** Phase B2 adds
   `FOR UPDATE SKIP LOCKED` webhook claiming, `quota_credits` DB-level idempotency,
   and `--auth-db-max-conns` pool sizing. Multiple gateway nodes against one
   Postgres DB are now race-safe. See `docs/multinode.md`.
   SQLite and libSQL remain single-node regardless of this flag.
2. **`MaxOpenConns(1)` was a B1 constraint, now lifted for PostgreSQL.** B1 kept
   one connection to preserve single-writer serialization. B2 makes the pool size
   configurable via `--auth-db-max-conns` (default 10) for the Postgres backend.
   SQLite and libSQL are always capped at 1.
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
- Quota **byte** columns → `BIGINT`, not `INTEGER` (affects
  `quotas.limit_bytes`, `quotas.used_bytes`, and `quota_credits.bytes`). On
  PostgreSQL `INTEGER` is 32-bit (max ~2.1 GB) and overflows for LFS objects or
  tenant totals above ~2 GB; the shipped Postgres schema uses `BIGINT` here
  (migration `0012`). Match that when hand-adapting a dump.
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
contents after loading to confirm all migrations are recorded (the latest
`version` should match the highest-numbered file in the Postgres migration set).

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

## 7. Phase B2 (shipped)

B2 built directly on the B1 schema (all foreign keys are `DEFERRABLE INITIALLY
DEFERRED` in the Postgres migration set, so B2 required no migration edits).
The following items planned at the end of B1 are now complete:

- **Multi-node webhook claiming** via `SELECT … FOR UPDATE SKIP LOCKED`.
- **DB-level quota idempotency** via the `quota_credits` table (unique PK on
  `(tenant, oid)`; replaces the in-process ring).
- **Connection pooling** via `--auth-db-max-conns` (default 10; Postgres only).
- **Documentation** of the per-node M18 rate limiter in a multi-gateway context.

See `docs/multinode.md` for full details, upgrade notes,
and the supported PostgreSQL version matrix.
