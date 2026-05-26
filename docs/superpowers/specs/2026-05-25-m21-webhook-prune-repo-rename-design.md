# M21: Webhook prune + auth-only repo rename + EventRepoRenamed emitter

**Status:** Design.
**Date:** 2026-05-25.
**Scope:** Close three M15 / M15.1 deferrals as a single polish milestone: (1) `bucketvcs webhook prune` CLI to sweep terminal-state `webhook_deliveries` rows past their retention window; (2) `bucketvcs repo rename` CLI for same-tenant auth-only rename; (3) wire the existing-but-never-emitted `EventRepoRenamed` taxonomy const from the rename CLI.

## 1. Goals

### 1.1 In scope

- New CLI: `bucketvcs webhook prune --auth-db=... [--delivered-older-than=30d] [--dead-letter-older-than=90d] [--dry-run] [--actor=admin]`. Sweeps `delivered` rows (`delivered_at` past cutoff) and `dead_letter` rows (`last_attempt_at` past cutoff). Never touches `pending` or `in_flight`.
- New CLI: `bucketvcs repo rename <tenant>/<old-name> <new-name> --auth-db=... --store=... [--actor=admin]`. Same-tenant only. Auth-only: storage keys stay at the old `tenants/<t>/repos/<old>/` prefix. Operator handles storage migration out of band if desired.
- Collision guards on rename: refuse if destination repos row exists in auth.db OR any storage key exists under `tenants/<t>/repos/<new-name>/` (cheap `List` with limit=1).
- Webhook enqueue for `EventRepoRenamed` fires from the rename CLI BEFORE the auth.db transaction (matches M15.1 delete ordering — endpoints subscribed under the old name receive the event).
- 2 new metrics + 2 new audit events (under existing `webhooks.*` / `repo.*` namespaces).
- Smoke + unit + integration tests.
- Operator guide section in the existing M14 hooks-policy guide OR a separate `docs/m15-webhook-operator-guide.md` extension covering the prune retention guidance.

### 1.2 Out of scope (deferred)

- **Cross-tenant rename / repo transfer.** `acme/foo → other/bar` requires grants/quotas/permissions reasoning that belongs in a separate "transfer" milestone. M21's CLI shape (`<new-name>` is a bare segment, not `tenant/repo`) makes the cross-tenant fat-finger impossible.
- **Storage-key migration during rename.** No copy-then-delete pass; no background job. Storage stays at the old prefix.
- **`repo_id` surrogate-key indirection refactor.** The architecturally-right answer for cheap rename (visible name decoupled from storage prefix), but a 2x-M20 refactor that touches every `keys.NewRepo` call site and a forward-only migration.
- **Background prune ticker in `bucketvcs serve`.** Prune is CLI-only; operators schedule via cron / systemd / ops playbook.
- **Auto-pruning of orphaned `webhook_endpoints`** (the M15.1 deferred TODO at `repocmd.go:406`). Endpoints orphaned by repo deletion are intentionally left to drain in-flight deliveries; their post-drain cleanup is its own milestone.
- **EventRepoRenamed emission from any source other than the rename CLI.** No API endpoint, no SSH command, no automatic trigger.
- **--force overrides for collision guards.** No `--force-overwrite-storage`. Operators who want collision behavior change handle it manually.

## 2. Architecture

```
internal/webhooks/
  prune.go            (new) — Service.Prune(ctx, cfg) (int, int, error); returns
                              (deliveredDeleted, deadLetterDeleted, err).
  prune_test.go       (new)

internal/auth/sqlitestore/
  rename.go           (new) — Store.RenameRepo(ctx, tenant, oldName, newName) error;
                              transactional multi-table UPDATE; ErrRepoExists +
                              ErrNoSuchRepo sentinels (latter already exists).
  rename_test.go      (new)

cmd/bucketvcs/
  webhook.go          (extend) — add 'prune' subcommand alongside existing
                                 webhook subcommands (endpoint, delivery).
  webhook_prune.go    (new)   — runWebhookPrune handler.
  repocmd.go          (extend) — add 'rename' to the subcommand dispatch.
  repo_rename.go      (new)   — runRepoRename handler.

internal/webhooks/
  metrics.go          (extend) — EmitWebhookPrunedMetric (counter per outcome)
  audit.go            (extend) — EmitWebhookPruned audit emitter
  event.go            (no change) — EventRepoRenamed const + RepoRenamedPayload
                                    already declared from M15

scripts/m21-webhook-prune-repo-rename-smoke.sh   (new)
docs/m15-webhook-operator-guide.md or wherever M15 docs live (extend) — prune
                                                                        retention guidance + rename caveats
```

**Lifecycle:**
1. **Prune**: operator runs `bucketvcs webhook prune`. CLI opens authdb, computes cutoffs (`time.Now().Add(-delivered_retention)` for delivered; same for dead_letter), calls `webhooks.Service.Prune(ctx, cfg)`. Service runs a single `DELETE` with the dual-clause `WHERE` and reports `RowsAffected` broken down by status (via two separate DELETEs for accurate counts). Emits one `webhooks.pruned` audit event. CLI writes the count to stdout.
2. **Rename**: operator runs `bucketvcs repo rename acme/foo bar`. CLI:
   - parses + validates: same-tenant only enforced by CLI arg shape (new-name is a bare segment); rejects `new == old`; rejects cross-tenant.
   - opens authdb + store; constructs `keys.NewRepo(tenant, newName)`.
   - collision check 1: `SELECT 1 FROM repos WHERE tenant=? AND name=?` → if found, exit 1.
   - collision check 2: `store.List(ctx, "tenants/<t>/repos/<newName>/", 1)` → if any entry, exit 1.
   - enqueues `EventRepoRenamed` via M15 `webhookSvc.Enqueue`. Fail-open (log + continue, matches M15 ordering).
   - calls `sqlitestore.Store.RenameRepo(ctx, tenant, oldName, newName)`. Multi-table UPDATE inside a transaction with `foreign_keys=OFF`. Rollback on error.
   - emits `repo.renamed` audit event.
   - writes `renamed: <tenant>/<old> -> <new>` to stdout.

## 3. Data model

**No new tables. No new migration. No new indexes.** Both operations use existing M4 / M15 schema.

The webhook_deliveries table from migration 0006:
```sql
CREATE TABLE webhook_deliveries (
    id              TEXT PRIMARY KEY,
    endpoint_id     INTEGER NOT NULL,
    event_type      TEXT NOT NULL,
    payload_json    BLOB NOT NULL,
    status          TEXT NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_attempt_at INTEGER,
    last_status_code INTEGER,
    last_error      TEXT,
    created_at      INTEGER NOT NULL,
    delivered_at    INTEGER,
    FOREIGN KEY (endpoint_id) REFERENCES webhook_endpoints(id) ON DELETE CASCADE
);
```

Status values that the M15 worker actually writes: `pending`, `in_flight`, `delivered` (terminal success), `dead_letter` (terminal failure). Prune only touches the two terminal states.

The repos table from migration 0001:
```sql
CREATE TABLE repos (
    tenant TEXT NOT NULL,
    name   TEXT NOT NULL,
    ...
    PRIMARY KEY (tenant, name)
);
```

FK-bearing tables that reference `repos(tenant, name)` (all `ON DELETE CASCADE`, no `ON UPDATE`):
`repo_permissions` (0001), `ssh_keys.scope_(tenant,repo)` (0002), `protected_refs` (0005), `webhook_endpoints` (0006), `protected_paths` (0007), `hooks` (0009).

Implementer at Task 0 must also confirm whether `lfs_locks` (0003) and `quotas` (0004) FK to repos or are merely tenant/repo-scoped without FK. If they FK, the rename transaction touches 8 tables; if not, still update the columns for value consistency (M15.1 manually sweeps `lfs_locks` on delete and the same pattern applies here).

## 4. CLI surface

### 4.1 `bucketvcs webhook prune`

```
bucketvcs webhook prune \
    --auth-db=/path/to/auth.db \
    [--delivered-older-than=30d] \
    [--dead-letter-older-than=90d] \
    [--dry-run] \
    [--actor=<string>]
```

- `--auth-db` (required): authdb path.
- `--delivered-older-than` (default `30d`): retention for `status='delivered'`. Cutoff is `delivered_at`. Minimum `1h` (operator guard against fat-fingering).
- `--dead-letter-older-than` (default `90d`): retention for `status='dead_letter'`. Cutoff is `last_attempt_at`. Minimum `1h`.
- `--dry-run`: prints rows that WOULD be deleted as NDJSON (`{id, status, age_seconds, endpoint_id}`); does not mutate.
- `--actor`: audit attribution; uses `fs.Func` closure trick (matches M15.1) to distinguish "flag absent" from `--actor=` (empty).

Exit codes:
- `0`: success (including no-op when 0 rows match)
- `1`: runtime error (sqlite locked, etc.)
- `2`: invalid flags

Stdout (non-dry-run):
```
pruned: 142 delivered (older than 30d), 7 dead-letter (older than 90d)
```

Stdout (dry-run): NDJSON, one object per row.

### 4.2 `bucketvcs repo rename`

```
bucketvcs repo rename <tenant>/<old-name> <new-name> \
    --auth-db=/path/to/auth.db \
    --store=<storage-url> \
    [--actor=<string>]
```

- Positional 1: `<tenant>/<old-name>` (same `splitTenantRepo` helper M15.1 delete uses).
- Positional 2: `<new-name>` — bare segment, no slash. Enforced by parser. This shape makes cross-tenant rename impossible at the CLI surface.
- `--auth-db`: required.
- `--store`: required (for the storage collision check).
- `--actor`: optional, audit attribution.

Exit codes:
- `0`: success
- `1`: runtime error, source not found, collision (auth or storage), transaction failure
- `2`: invalid flags / args (cross-tenant, `new == old`, etc.)

Stdout (success):
```
renamed: acme/foo -> acme/bar
```

Stderr (collision):
```
repo rename: destination acme/bar already exists in auth.db
```

or

```
repo rename: storage at tenants/acme/repos/bar/ is non-empty; refusing to rename
```

## 5. Webhook ordering on rename

The `EventRepoRenamed` enqueue happens AFTER the collision checks succeed but BEFORE the auth.db transaction begins. The M15.1 delete shipped this exact ordering for a related reason: endpoints subscribed under the about-to-disappear `(tenant, repo)` need the row to still exist in auth.db so the worker can resolve them. M21's rename has the same property — the worker matches deliveries to endpoints by `(tenant, repo)`, and at enqueue time the old name is still the canonical one.

If the auth transaction subsequently fails, the enqueued webhook delivery still goes out. The subscribers receive a `repo.renamed` event for a rename that didn't actually happen. **This is documented as a caveat.** Mitigation paths considered and rejected:
- Defer enqueue until after the transaction commits → endpoints subscribed under the old name are gone by then; delivery can't resolve to any endpoint.
- Use a 2-phase enqueue with a pre-commit hook → over-engineering for a polish milestone; transaction failures here are rare (single sqlite write).
- Add a `--dry-run` to rename → useful but separate; can be added later.

Operators who care: run `--dry-run` first (deferred for now) OR diff `webhook_endpoints` count before vs after to catch the rare mismatch.

## 6. Failure modes

| Failure | Behavior |
|---|---|
| `webhook prune --dry-run` against empty table | exit 0, prints `pruned: 0 delivered, 0 dead-letter` (or no NDJSON lines in dry-run); no audit event. |
| `webhook prune` partial DELETE fails (sqlite locked, disk full) | exit 1; partial commit not possible within a single statement; stderr reports the error; no audit event. |
| `webhook prune` with `--delivered-older-than=1m` (< 1h) | exit 2: `webhook prune: --delivered-older-than must be >= 1h (got 1m); set higher to avoid pruning live rows`. |
| `webhook prune` with negative retention (`--delivered-older-than=-1d`) | exit 2: `webhook prune: retention must be positive`. |
| `repo rename` source absent | exit 1: `repo rename: acme/foo not found`. Detected by `SELECT 1 FROM repos`. |
| `repo rename` with destination auth-row exists | exit 1: `repo rename: destination acme/bar already exists in auth.db`. No mutation. |
| `repo rename` with destination storage non-empty | exit 1: `repo rename: storage at tenants/acme/repos/bar/ is non-empty; refusing to rename`. No mutation. |
| `repo rename` cross-tenant attempt (`<new-name>` contains `/`) | exit 2: `repo rename: <new-name> must be a bare segment; cross-tenant rename not supported in M21`. |
| `repo rename` with `new == old` | exit 2: `repo rename: new name equals old name; no-op`. |
| `repo rename` transaction fails mid-flight | rollback; webhook still delivered (documented caveat); exit 1. |
| Concurrent `repo rename` on same (tenant, repo) | sqlite serializes; second call sees source absent → exit 1. |
| `repo rename` `--store` is invalid URL | exit 2 with the standard store-parse error. |
| `repo rename` `--store` is unreachable (S3 401, etc.) | exit 1 with the underlying store error during the collision-check `List`. |

## 7. Observability

### 7.1 Metrics

```
webhook_deliveries_pruned_total{outcome=delivered|dead_letter}    counter
repo_renamed_total{outcome=ok|collision_auth|collision_storage|not_found|cross_tenant}   counter
```

Cardinality bounded by outcome enum (5 values for rename, 2 for prune). No per-tenant labels — the events are operator-initiated and infrequent.

### 7.2 Audit events

| Event | Level | Attrs |
|---|---|---|
| `webhooks.pruned` | INFO | `delivered_rows`, `dead_letter_rows`, `delivered_cutoff_unix`, `dead_letter_cutoff_unix`, `dry_run`, `actor` |
| `repo.renamed` | INFO | `tenant`, `old_name`, `new_name`, `actor` (under existing `repo.*` namespace; const + payload already declared from M15) |

No new event-name prefixes.

## 8. Testing

### 8.1 Unit

`internal/webhooks/prune_test.go`:
- Table-driven: 4 row states (pending, in_flight, delivered, dead_letter) × {within-cutoff, past-cutoff} → 8 rows in. Prune with explicit cutoffs. Assert only past-cutoff terminal-state rows deleted.
- `--dry-run` semantic: same dataset → 0 rows deleted, count returned matches non-dry-run.
- Cutoff `0`: prunes all terminal rows regardless of age (degenerate; useful for tests).

`internal/auth/sqlitestore/rename_test.go`:
- Round-trip: register `acme/foo`, grant alice→write, add protected ref, add hook, add webhook endpoint → rename to `acme/bar` → assert all 7 FK tables now reference `bar`, `acme/foo` row no longer present.
- Collision detection: rename to a name that already exists → ErrRepoExists.
- Cross-tenant rejection (verified at CLI layer): the Store-layer API takes only `(tenant, oldName, newName)`, so cross-tenant isn't representable.
- Concurrent rename: two goroutines, same (tenant, repo) → exactly one succeeds.

### 8.2 Integration

`internal/auth/sqlitestore/rename_integration_test.go`:
- Storage collision detection with a fake `storage.ObjectStore` populated with one key under the new prefix.
- Real localfs store: rename succeeds, storage stays at old prefix (audit-visible).

### 8.3 Smoke

`scripts/m21-webhook-prune-repo-rename-smoke.sh`:
1. Register `acme/foo`, grant alice→write, register a webhook endpoint pointing at a local test http listener
2. Push a small commit → assert delivery row appears, then completes (status=`delivered` + `delivered_at` set within ~3s)
3. Run `bucketvcs webhook prune --delivered-older-than=0s` → assert row deleted, `webhooks.pruned` audit emitted
4. Run `bucketvcs repo rename acme/foo bar` → assert exit 0, `repo.renamed` in serve log, auth.db row updated
5. Push to `acme/bar.git` (the new name) → success
6. Push to `acme/foo.git` (the old name) → 404
7. Echo `M21_WEBHOOK_PRUNE_RENAME_SMOKE_OK`

## 9. Acceptance criteria

- Unit + integration tests pass
- Smoke ends `M21_WEBHOOK_PRUNE_RENAME_SMOKE_OK`
- All prior smokes (M11 / M12 / M13 / M14 / M15.1 / M16 / M17 / M18 / M19 / M20) unaffected
- `bucketvcs webhook prune --help` shows the documented flags
- `bucketvcs repo rename --help` shows the documented args + flags
- Operator guide gains a "Webhook delivery retention" subsection + a "Repo rename: auth-only semantics" subsection
- `EventRepoRenamed` is reachable in production via the rename CLI (was dead taxonomy before)

## 10. Open questions

None — all decisions captured above.
