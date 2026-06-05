# Turso / libSQL metadata backend (operator guide)

This guide covers backing the BucketVCS **metadata/auth database**
with [Turso](https://turso.tech) / [libSQL](https://github.com/tursodatabase/libsql)
instead of a local SQLite file. It explains how to enable it, how
backend selection works, the caveats you must understand before
deploying, and how to migrate existing data.
For choosing between backends (SQLite, SQLite + replication, Turso/libSQL, and
PostgreSQL), see the [authdb hosting guide](authdb-hosting.md).

Production readiness summary:

- libSQL/Turso backend for the auth DB, selected by `--auth-db` URL scheme — **shipped**.
- Pure-Go driver (`libsql-client-go`); `CGO_ENABLED=0` cross-builds stay green — **shipped**.
- Auth token via `BUCKETVCS_DB_AUTH_TOKEN` env (never a CLI argument) — **shipped**.
- All 10 migrations apply over libSQL; behavioral parity proven by a gated conformance suite — **shipped**.
- SQLite (modernc, pure-Go) remains the zero-dependency default — **unchanged**.
- Multi-node-safe webhook claiming / quota updates, connection pooling > 1, embedded replicas / offline mode — **out of scope for libSQL (see §4)**. For multi-node deployments, use the PostgreSQL backend (`docs/postgres.md`, `docs/multinode.md`).

---

## 1. Overview

BucketVCS keeps two very different kinds of state:

- **Git object data** — packs, indexes, bundles, LFS objects — lives in object
  storage (S3 / R2 / GCS / Azure / localfs). The auth-DB backend choice does **not** touch this.
- **Metadata / auth** — users, tokens, repos, permissions, protected
  refs/paths, hooks, webhooks, OIDC issuers/rules, LFS locks, quotas — lives in
  the auth DB. This is what the libSQL backend lets you move off SQLite.

Because libSQL speaks SQLite's SQL dialect, the existing schema, migrations, and
~45 store methods run essentially unchanged. Selecting Turso is purely an
operator configuration change; no data model or consumer behavior changes.

**Why bother?** A local SQLite file ties the gateway's auth state to one host's
disk. Pointing the auth DB at a managed Turso database decouples that state from
the gateway host, which simplifies backups and host replacement.

---

## 2. Enabling

The backend is chosen by the **scheme of the existing `--auth-db` value** —
there is no new selection flag, and the change applies to `serve` and to every
`bucketvcs` subcommand that takes `--auth-db` (`user`, `token`, `repo`,
`policy`, `webhook`, `quota`, `oidc`).

### 2.1 Worked example (Turso)

```bash
# 1. Create a database and a token with the Turso CLI.
turso db create bucketvcs-auth
turso db show bucketvcs-auth --url          # e.g. libsql://bucketvcs-auth-acme.turso.io
turso db tokens create bucketvcs-auth       # prints the auth token

# 2. Export the token (NEVER pass it on the command line — it would leak via ps).
export BUCKETVCS_DB_AUTH_TOKEN='<token-from-step-1>'

# 3. Point any subcommand or the gateway at the libSQL URL.
bucketvcs serve --auth-db 'libsql://bucketvcs-auth-acme.turso.io' ...
```

On startup the gateway logs the backend it selected:

```
INFO authdb opened backend=libsql
```

(For the default SQLite path this reads `backend=sqlite`.) Migrations are
applied automatically the first time the DB is opened, exactly as with SQLite.

### 2.2 Self-hosted sqld

You can also point `--auth-db` at a self-hosted [sqld](https://github.com/tursodatabase/libsql)
instance over `http(s)://`. The auth token is **optional** for sqld instances
that run without authentication:

```bash
bucketvcs serve --auth-db 'http://sqld.internal:8080' ...
```

---

## 3. How backend selection works

`--auth-db` is parsed as a URL and dispatched by scheme:

| `--auth-db` value | Backend |
|-------------------|---------|
| bare path (`/var/lib/bucketvcs/auth.db`) | SQLite (default) |
| `file:/path/to/auth.db` | SQLite |
| `sqlite:/path/to/auth.db` | SQLite |
| `C:\data\auth.db` (Windows drive path) | SQLite |
| `libsql://<db>.turso.io` | libSQL |
| `https://<db>.turso.io` | libSQL |
| `http://sqld.internal:8080` | libSQL |

Token resolution for the libSQL backend, in order:

1. `BUCKETVCS_DB_AUTH_TOKEN` env (preferred; takes precedence).
2. An `authToken=` query parameter already on the URL (dev convenience).
3. None — allowed. For `libsql://` URLs a startup WARN is logged (Turso almost
   always requires a token); the connection surfaces a clear auth error if the
   server actually requires one. Self-hosted no-auth sqld works without a token.

The token is **never** accepted as a CLI argument.

---

## 4. Caveats (read before deploying)

1. **Remote libSQL only.** No embedded replica / offline mode. Those require the
   cgo `go-libsql` driver, which would break the `CGO_ENABLED=0`
   cross-compilation the release pipeline depends on. The libSQL backend uses the
   pure-Go remote client exclusively.
2. **Single-node only.** Multi-node-safe webhook claiming and DB-atomic
   quota updates are not available on libSQL. Running multiple gateway nodes
   against one Turso DB is **not race-safe** — deploy a single gateway node, or
   use the PostgreSQL backend for multi-node (`docs/multinode.md`).
3. **`MaxOpenConns(1)`.** The libSQL backend keeps one connection to preserve the
   single-writer serialization the store code assumes (quota ring-lock,
   webhook claim). This bounds throughput.
4. **Rate limiter stays per-node.** The credential-failure limiter is
   in-memory per node regardless of DB backend.

---

## 5. Migrating existing SQLite data into Turso

This is done out of band; BucketVCS does not automate it.

```bash
# 1. Dump the existing SQLite auth DB.
sqlite3 /var/lib/bucketvcs/auth.db .dump > authdump.sql

# 2. Load it into the Turso/libSQL database.
turso db shell bucketvcs-auth < authdump.sql

# 3. Point --auth-db at the libSQL URL (see §2) and restart.
```

Because the schema is identical, the dump loads as-is. After loading, the next
gateway start applies any not-yet-present migrations idempotently.

---

## 6. Verifying

- **Startup log:** `authdb opened backend=libsql` confirms the libSQL backend
  was selected.
- **Smoke:** create a user/token against the libSQL URL and authenticate a
  `git ls-remote`, exactly as you would against SQLite — behavior is identical.
- **CI:** the nightly `conformance` workflow runs a gated suite
  (`go test -tags libsql -run TestLibsqlConformance`) against a pinned sqld
  container, asserting migrations, UNIQUE→conflict mapping, CHECK enforcement,
  FK cascade, and token + OIDC round-trips all match SQLite.

---

## 7. Multi-node and PostgreSQL

Multi-node deployments and a SQL-dialect layer for other databases live in the
PostgreSQL backend rather than libSQL:

- PostgreSQL backend with a SQL-dialect layer (timestamps, upserts,
  autoincrement, `$N` placeholders, FK deferral, SQLSTATE error codes).
- Multi-node concurrency hardening (`SELECT … FOR UPDATE SKIP LOCKED` webhook
  claiming, DB-atomic quota updates) for shared-DB multi-gateway deployments.
- Connection pooling beyond a single connection.

The `Backend` seam is the extension point these build on. See `docs/postgres.md`
and `docs/multinode.md`.
