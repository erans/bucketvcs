# Repositories (operator guide)

This guide covers the repository lifecycle: registering repos, managing access,
renaming (with old-name redirects), and deleting. All operations are driven by
the `bucketvcs repo` CLI against the auth database; some also touch the storage
backend.

```
bucketvcs repo <register|grant|revoke|public|list|delete|rename|deploy-key|alias>
```

Every mutating subcommand accepts an optional `--actor=<string>` that is recorded
in the audit log and (where applicable) the emitted webhook payload; if omitted,
a default actor is recorded. All subcommands require `--auth-db=<path>`.

---

## 1. Register

```
bucketvcs repo register <tenant>/<repo> --auth-db=<path> [--store <url>] [--no-init]
```

Registration inserts the `(tenant, name)` row in the auth database and, unless
`--no-init` is given, initializes the repo's storage (an empty manifest) via
`--store`. Use `--no-init` for a registry-only entry when storage will be
populated separately; otherwise `--store` is required.

First-time registration emits the `repo.created` webhook. Re-registering an
existing repo is a no-op (no webhook). Registering a name that is currently a
rename **alias** removes that alias — a live repo always shadows an alias
(see §4).

---

## 2. Access control

Repos are private by default: only users with an explicit grant (or global
admins) can read or write. Make a repo world-readable with `public`.

```
bucketvcs repo grant  <user> <tenant>/<repo> <read|write|admin> --auth-db=<path>
bucketvcs repo revoke <user> <tenant>/<repo>                     --auth-db=<path>
bucketvcs repo public <tenant>/<repo> <on|off>                   --auth-db=<path>
bucketvcs repo list                                             --auth-db=<path>
```

- `grant` sets a user's permission level (`read` < `write` < `admin`). The repo
  must already be registered.
- `revoke` removes a user's grant.
- `public on` sets public-read (anonymous clone/fetch); `public off` reverts to
  private. Public-read never grants write.
- `list` enumerates registered repos.

Per-repo **deploy keys** (SSH keys scoped to one repo) are managed with
`bucketvcs repo deploy-key …` — see the SSH operator guide.

---

## 3. Rename (auth-only semantics)

```
bucketvcs repo rename <tenant>/<old-name> <new-name> \
    --auth-db=<path> --store=<url> [--actor=<string>]
```

The rename CLI updates **auth.db only**. Storage keys at
`tenants/<tenant>/repos/<old-name>/...` are NOT migrated by this command — the
operator moves them out of band (`aws s3 mv`, `gsutil mv`, etc.) AND rewrites the
absolute key references in the manifest body (`pack_key`, `idx_key`, index keys
all contain the old prefix). See the storage runbook in §3.4.

`--store` opens the backend only for a destination-prefix collision probe; on
localfs this acquires an exclusive whole-bucket lock, so the gateway must be
stopped during the rename. Cloud backends have no such lock.

### 3.1 What the CLI does atomically

A single sqlite transaction over the `RenameRepo` helper updates every
FK-bearing dependent table plus a small set of repo-scoped tables without FKs:

- `repos(tenant, name)` — the primary row
- `repo_permissions` — user grants (FK)
- `ssh_keys` — per-repo deploy keys (FK; columns `scope_tenant`, `scope_repo`)
- `protected_refs` — ref protection rules (FK)
- `protected_paths` — path protection rules (FK)
- `hooks` — pre/post-receive hook rules (FK)
- `webhook_endpoints` — endpoint registrations (FK)
- `oidc_trust_rules` — OIDC token-exchange trust rules (FK)
- `build_triggers` — CI build triggers (FK)
- `lfs_locks` — active locks (no FK to `repos`; updated for value consistency)
- `webhook_deliveries` — historical rows are joined by `endpoint_id` FK; they
  follow the endpoint row automatically (no separate UPDATE)

NOT touched by `RenameRepo`:

- `quotas` — keyed by `tenant` only (no `repo` column). Same-tenant rename leaves
  the tenant-wide byte counter unchanged.

The transaction runs with `PRAGMA defer_foreign_keys = TRUE` so intermediate
states (rows pointing at a not-yet-renamed row) are tolerated until COMMIT.

### 3.2 Refusal conditions

The CLI refuses (exit 1, no auth mutation, no webhook delivery) if:

- Source `<tenant>/<old-name>` does not exist (`not_found` outcome metric).
- Destination `<tenant>/<new-name>` already exists in auth.db (`collision_auth`).
- Destination storage prefix `tenants/<tenant>/repos/<new-name>/` is non-empty —
  a `List(prefix, MaxKeys=1)` probe detects leftover blobs (`collision_storage`).
- `<new-name>` contains `/` or `\` — cross-tenant rename is not supported
  (`cross_tenant`). A future "transfer" verb will handle cross-tenant motion.

Successful rename emits the `ok` outcome metric.

### 3.3 Webhook ordering — at-least-once before commit

The `repo.renamed` webhook is enqueued **BEFORE** the auth transaction runs
(matching the `repo.deleted` precedent): endpoints scoped to `(tenant, old-name)`
are still present in `webhook_endpoints` when `Enqueue` resolves subscribers; the
rename moves those rows to the new name in the same transaction, and a worker
reading `webhook_endpoints` AFTER the rename would not match the old payload key.

Consequence: if the auth transaction subsequently fails, the webhook still
delivers. Treat `repo.renamed` as an **at-least-once** signal and reconcile by
querying current state (`bucketvcs repo list`) rather than assuming the rename
committed. If enqueue itself fails, a `webhooks.enqueue_failed` audit fires and
the rename proceeds fail-open.

### 3.4 Storage migration runbook

After `bucketvcs repo rename <tenant>/<old> <new>` succeeds:

1. Stop the gateway (or rely on the localfs single-writer lock during the
   auth-rename step; cloud backends require a controlled cutover).
2. Move the storage tree:
   `aws s3 mv s3://bucket/tenants/<tenant>/repos/<old>/ s3://bucket/tenants/<tenant>/repos/<new>/ --recursive`
   (or backend-equivalent).
3. Rewrite absolute path references in the manifest body — every `pack_key`,
   `idx_key`, and `indexes.*.key` field embeds the old prefix. Download
   `tenants/<tenant>/repos/<new>/manifest/root.json`, sed-replace
   `tenants/<tenant>/repos/<old>/` → `tenants/<tenant>/repos/<new>/`, and PUT it
   back atomically.
4. Restart the gateway. Push/clone against the new name should now succeed; the
   old name redirects (see §4).

A future release may automate this via a `bucketvcs storage rename` helper that
respects the manifest indirection. For now it is an operator runbook.

### 3.5 Limits

- Same-tenant only. Cross-tenant motion requires a separate verb.
- No undo. The rename commits with the sqlite transaction; reverse it with a
  second `repo rename`.
- LFS quotas are per-tenant, so a same-tenant rename leaves the tenant-wide byte
  counter unaffected. After an out-of-band storage migration, run
  `bucketvcs quota reconcile --tenant=<tenant>` to correct any drift.
- The `webhook_endpoints` row for the old name is migrated to the new name;
  subscribers keep their endpoint ID and secret. To surface the new name as a
  different endpoint, `webhook endpoint rotate-secret --id=<N>` after the rename.

---

## 4. Rename redirects & aliases

After a rename (CLI or the web UI Settings → Rename form), the **old name keeps
working**:

- **Web UI:** requests to `/{tenant}/{old}/…` return **302** to
  `/{tenant}/{new}/…` (sub-path and query preserved).
- **Git (HTTPS + SSH) and LFS:** clone/fetch/push against the old name resolve
  transparently to the renamed repo. SSH additionally prints
  `bucketvcs: repository renamed to <tenant>/<new>; update your remote`.

The redirect is backed by a `repo_aliases` row created at rename time. It
**stops** as soon as the old name is reused: registering a new repo with the old
name removes the alias (a live repo always shadows an alias). Chained renames
(`a→b→c`) keep the oldest alias resolving to the current name.

Authorization is unchanged: an alias resolves the *name* but auth is still
enforced on the canonical repo — a private target stays private.

Manage aliases:

```bash
bucketvcs repo alias list   --auth-db=<path> <tenant>/<name>     # aliases resolving to this repo
bucketvcs repo alias remove --auth-db=<path> <tenant>/<old-name> # drop a redirect early
```

Observe old-name traffic via the `repo_alias_resolved_total{transport}` metric
(`transport` ∈ `ui|https|ssh`; LFS shares the `https` label).

**Storage is still moved out of band** — aliasing resolves *names*, not bytes.
The §3.4 requirement to relocate `tenants/<tenant>/repos/<old>/…` is unchanged.

---

## 5. Delete

```
bucketvcs repo delete <tenant>/<repo> --auth-db=<path> [--purge-storage --store <url>] [--actor=<name>]
```

Delete removes the repo and its auth-db dependents (grants, deploy-key scopes,
protected refs/paths, hooks, OIDC rules, build triggers, LFS locks, and any
rename aliases pointing at or named after the repo). With `--purge-storage`
(which requires `--store`), it also iterates and deletes the repo's storage
objects; without it, storage is left in place for manual cleanup.

Delete emits the `repo.deleted` webhook. `webhook_endpoints` rows are
intentionally left in place so the `repo.deleted` delivery can drain through
them; prune them afterward with `bucketvcs webhook endpoint remove` if desired.

---

## 6. Observability

- `repo_alias_resolved_total{transport=ui|https|ssh}` — old-name traffic that
  resolved through a rename alias (LFS shares the `https` label). High volume
  long after a rename means clients haven't updated their remotes.
- Rename outcomes are counted with the `ok | not_found | collision_auth |
  collision_storage | cross_tenant` metric (§3.2).
- Lifecycle webhooks: `repo.created`, `repo.renamed`, `repo.deleted` — see the
  webhooks guide for delivery, signing, and retry semantics.
