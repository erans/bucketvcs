# Authdb hosting: choosing a backend (operator guide)

bucketvcs keeps two very different kinds of state. **Git object data** — packs,
indexes, bundles, LFS objects — always lives in object storage (`--store`) and
is written with compare-and-swap manifests, so it is durable and consistent on
its own regardless of anything in this guide. The **auth database** (authdb)
holds everything else: users, tokens, scopes, repo registrations, permissions,
protected refs/paths, hooks, webhooks, OIDC issuers/rules, LFS locks, and
quotas.

This guide is about that authdb: the ways you can host it, and how to pick the
right one. It consolidates and links the deeper per-backend guides rather than
repeating them.

---

## 1. The four hosting models

### (a) Embedded SQLite (default)

A local SQLite file selected by passing a bare path to `--auth-db`:

```bash
bucketvcs serve --store ... --auth-db /var/lib/bucketvcs/auth.db --addr :8080
```

Zero setup, zero dependencies, pure-Go. This is the default and is the right
choice for trying bucketvcs out and for many single-node deployments. Its
durability story is simply **the host disk**: if that disk is lost and not
backed up, the authdb is gone (your Git data in the bucket is unaffected, but
the tokens and grants that authorize access to it are not).

### (b) Embedded SQLite + object-store replication

The same embedded SQLite backend, plus continuous replication of the authdb
into an object store via embedded Litestream:

```bash
bucketvcs serve --store ... --auth-db /var/lib/bucketvcs/auth.db \
  --auth-db-replica=auto --addr :8080
```

`--auth-db-replica=auto` ships the SQLite WAL into the `--store` bucket under a
reserved `sys/authdb/` prefix (~1 second RPO), restores the authdb
automatically on boot if the local file is missing, and supports point-in-time
recovery. This is **the sweet spot for single-node production**, especially on
ephemeral or cloud disks where the host can be replaced at any time: you keep
SQLite's zero-dependency simplicity but no longer tie your credentials to one
disk. Still a single writer (one gateway at a time, enforced by a lease).

Full detail — flags, the `sys/` prefix, the single-writer lease, restore/PITR,
backend notes, failure modes — is in the **[authdb replication guide](authdb-replication.md)**.

### (c) Turso / libSQL (managed SQLite off-host)

A managed SQLite-compatible database, selected by a `libsql://` (or
`http(s)://`) `--auth-db` URL:

```bash
export BUCKETVCS_DB_AUTH_TOKEN='<turso-database-token>'
bucketvcs serve --store ... --auth-db 'libsql://<db>.turso.io' --addr :8080
```

This decouples the authdb from the gateway host without you running a database
server: Turso (or a self-hosted sqld) handles durability and backups. Still
**single node** — running multiple gateways against one Turso DB is not
race-safe. See the **[Turso / libSQL guide](turso.md)**.

### (d) PostgreSQL

A PostgreSQL database, selected by a `postgres://` / `postgresql://`
`--auth-db` URL:

```bash
export BUCKETVCS_DB_AUTH_TOKEN='<postgres-password>'
bucketvcs serve --store ... \
  --auth-db 'postgres://bv@db.internal:5432/bucketvcs_auth?sslmode=require' \
  --addr :8080
```

PostgreSQL is the only backend that is **multi-node-safe**: it has race-safe
webhook claiming (`FOR UPDATE SKIP LOCKED`), DB-level quota idempotency
(`quota_credits`), atomic repo rename, and a configurable connection pool
(`--auth-db-max-conns`). It is **required** for:

- Running **multiple gateway nodes** against one authdb — see the
  **[multi-node guide](multinode.md)**.
- **Multi-region read gateways** (`--replica-of`) — see the
  **[multi-region guide](multi-region.md)**.
- **Bring-your-own-bucket** in any multi-node or replica deployment, because all
  nodes must share the encrypted per-tenant bindings — see the
  **[BYOB guide](byob.md)**.

See the **[PostgreSQL guide](postgres.md)** for the full setup.

---

## 2. Decision table

| Model | Nodes | Durability story | Ops burden | Choose when… |
|---|---|---|---|---|
| **(a) Embedded SQLite** | 1 | Host disk only | None | You are trying bucketvcs out, or run a single node and already back up its disk. **The default.** |
| **(b) SQLite + replication** | 1 | Replicated to the bucket (~1 s RPO) + restore-on-boot + PITR | One flag (`--auth-db-replica=auto`) | **Single-node production**, especially on ephemeral/cloud disks. Want durability without running or paying for a separate DB. **Recommended for single-node prod.** |
| **(c) Turso / libSQL** | 1 | Managed by Turso / your sqld | Manage a Turso DB (or run sqld) | You want managed-database operations (backups, host-independence) without running PostgreSQL — but still single node. |
| **(d) PostgreSQL** | N | Managed by your Postgres (replication, PITR, backups) | Run/operate PostgreSQL | You need **multiple gateway nodes**, **multi-region replicas**, or **multi-node BYOB** — or you already operate Postgres and want one place for HA. **Required for multi-node / HA.** |

**Opinionated summary:** start at (a). For single-node production, move to (b) —
it is one flag and removes the single-disk failure mode. Reach for (d) the
moment you need more than one gateway node or any HA/multi-region topology.
Pick (c) when you specifically want a managed database's operational model but
do not want to run PostgreSQL and remain single-node.

---

## 3. Backend selection mechanics

There is **no separate backend flag**. The backend is chosen entirely by the
**scheme of `--auth-db`**, and the choice applies uniformly to `serve` and to
every `bucketvcs` subcommand that takes `--auth-db` (`user`, `token`, `repo`,
`policy`, `webhook`, `quota`, `oidc`):

| `--auth-db` value | Backend |
|---|---|
| bare path (`/var/lib/bucketvcs/auth.db`), `file:`, `sqlite:`, Windows drive path | SQLite (default) |
| `libsql://…`, `https://…turso.io`, `http://sqld.internal:8080` | Turso / libSQL |
| `postgres://…`, `postgresql://…` | PostgreSQL |

The secret for the managed backends always comes from the
`BUCKETVCS_DB_AUTH_TOKEN` environment variable (or, for Postgres, standard libpq
mechanisms) — **never** on the command line. On startup the gateway logs the
backend it selected: `authdb opened backend=sqlite` / `=libsql` / `=postgres`.

**`--auth-db-replica` is orthogonal and SQLite-only.** It controls model (b) —
whether the embedded SQLite authdb is replicated to object storage — and is
independent of the scheme that chooses the backend. It is **rejected at startup
(exit code 2)** for the libSQL and PostgreSQL backends, because those bring
their own durability and replication; combining them with `--auth-db-replica`
is a configuration error, not a no-op. It is also rejected on regional replica
gateways (`--replica-of`): only the write-region primary replicates the authdb.

---

## 4. Migrating between models

bucketvcs does **not** ship a one-command authdb migration tool, and there is no
`bucketvcs migrate` subcommand — do not look for one. What exists:

- **SQLite → PostgreSQL.** Documented out of band in the
  [PostgreSQL guide §5](postgres.md#5-migrating-existing-sqlite-data-into-postgres):
  either re-create users/tokens/repos via the `bucketvcs` CLI against the empty
  Postgres DB (recommended for small deployments), or `sqlite3 … .dump` and
  hand-adapt the SQL (BLOB→BYTEA, autoincrement, BIGINT byte columns, etc.) for
  larger ones.
- **SQLite → Turso / libSQL.** Documented in the
  [Turso guide §5](turso.md#5-migrating-existing-sqlite-data-into-turso): because
  libSQL speaks SQLite's dialect, `sqlite3 … .dump` loads as-is via
  `turso db shell`.
- **Model (a) ↔ (b).** No migration at all — same SQLite file, same backend.
  Enabling (b) is just adding `--auth-db-replica=auto`; disabling it is removing
  the flag. (On first enablement against an empty replica location, see the
  `--auth-db-replica-skip-restore` note in the
  [replication guide §2.4](authdb-replication.md#24---auth-db-replica-skip-restore-escape-hatch).)

In all backend-changing migrations the move is: stand up the target, load/recreate
the data out of band, then re-point `--auth-db` and restart. Migrations apply
automatically the first time each backend is opened.

---

## 5. Common mistakes

- **Expecting `--auth-db-replica` to work with PostgreSQL or libSQL.** It is
  SQLite-only and is **rejected at startup (exit 2)** for those backends. Use the
  managed database's own replication/PITR instead — that is the whole point of
  choosing (c) or (d).
- **Running two `serve` processes against one SQLite file (or one libSQL
  endpoint), on the same host or across hosts.** SQLite's file locking and
  libSQL's write-serialization contract make this unsafe and will corrupt state.
  Models (a), (b), and (c) are **single-node**. For more than one gateway, use
  PostgreSQL (d). (Model (b)'s single-writer lease will in fact *refuse* a second
  replicating gateway — that is the guard working, not a bug.)
- **Reaching for multi-node on SQLite.** Multi-node and multi-region require
  PostgreSQL; SQLite/libSQL are refused at startup on replica gateways. Do not
  try to share a SQLite file over NFS to fake it.
- **Putting tenant data under the `sys/` prefix** when using model (b). That
  prefix is reserved for system data — the authdb replica (`sys/authdb/`) and
  durable shipped logs (`sys/logs/`); keep your own objects out of it (see the
  [replication guide §3](authdb-replication.md#3-the-reserved-sys-prefix) and
  [log shipping §3](log-shipping.md#3-the-reserved-sys-prefix-and-key-layout)).
- **Forgetting durability entirely on a single node.** Model (a)'s durability is
  the host disk. If that is not backed up, add `--auth-db-replica=auto` (b) — it
  is one flag.

---

## 6. See also

- [Authdb replication](authdb-replication.md) — model (b) in depth.
- [Turso / libSQL backend](turso.md) — model (c).
- [PostgreSQL backend](postgres.md) — model (d).
- [Multi-node](multinode.md) and [Multi-region](multi-region.md) — why (d) is
  required.
- [Bring-your-own-bucket](byob.md) — central authdb requirements.
