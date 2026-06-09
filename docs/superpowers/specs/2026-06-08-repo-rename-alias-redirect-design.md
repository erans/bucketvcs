# Repo rename aliases + redirects

Date: 2026-06-08
Builds on: M21 repo rename (`bucketvcs repo rename` CLI, web-UI rename form,
atomic `RenameRepo` transaction, `repo.renamed` webhook). This adds the missing
"remember the old name and redirect" layer GitHub provides.

## 1. Goal

After a repo is renamed (`tenant/old` → `tenant/new`), the old name currently
404s everywhere — there is no memory of it. This adds a **rename-alias** layer so
the old name keeps resolving to the renamed repo: the web UI **302**-redirects to
the new name, and HTTPS-git, SSH-git, and LFS **transparently resolve** to the
renamed repo. While we are inside the rename transaction we also fix two latent
bugs where `RenameRepo` orphans dependent rows.

Aliasing is a **name-resolution** concern; it does not move bytes. The existing
M21 caveat — storage objects under `tenants/<t>/repos/<old>/…` are migrated
out-of-band by the operator — is unchanged and orthogonal.

### 1.1 In scope

- New `repo_aliases` table (migration `0018`) + `ResolveAlias` / `ListAliases`
  store methods.
- `RenameRepo` transaction: insert the new alias, flatten alias chains, drop a
  shadowing alias, and **also** carry over `oidc_trust_rules` and
  `build_triggers` (the two orphan-bug fixes).
- Repo register shadows (deletes) any alias of the same name.
- `DeleteRepoCascade` removes aliases by `old_name` or `target_name`.
- Alias resolution at every entrypoint: web UI (302), HTTPS git, SSH git, LFS
  (transparent), with auth always enforced on the canonical repo.
- `bucketvcs repo alias list|remove` CLI; one metric `repo_alias_resolved_total`.
- Tests + operator-guide update.

### 1.2 Out of scope (deferred / documented)

- **Immutable repo-id re-architecture** (Approach C below). It would make rename
  *and* the storage move free, but rewrites the name-based storage prefix and
  every dependent FK — its own major milestone. Documented as the long-term
  direction.
- **Storage object migration on rename** — unchanged from M21 (out-of-band).
- **301 / permanent redirects** — we use 302 (names are reusable; see §4).
- **Guaranteed deprecation banners** in git output — best-effort only (§5).

## 2. Approach (selected: A — rewrite-on-rename, single-hop resolve)

Aliases are stored flat: each `(tenant, old_name)` maps directly to the current
live `target_name`. Chains are flattened at rename time, so resolution is always
a single lookup with no chain-walking and no cycle risk. Alternatives considered:
**B** (chain-following at read time — rejected: read-path loop + dangling
intermediates) and **C** (immutable repo-id — rejected here as a re-architecture;
noted as the long-term path that would also retire the out-of-band storage move).

## 3. Schema & store methods

Migration `internal/auth/sqlitestore/migrations/0018_repo_aliases.sql`:

```sql
CREATE TABLE repo_aliases (
    tenant      TEXT NOT NULL,
    old_name    TEXT NOT NULL,
    target_name TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (tenant, old_name)
);
CREATE INDEX idx_repo_aliases_target ON repo_aliases(tenant, target_name);
```

No FK to `repos`: `old_name` must by definition *not* be a live repo, and target
cleanup is handled explicitly (mirrors the deliberate no-FK choice for
`lfs_locks` / `webhook_endpoints`). The `target_name` index backs the
chain-flatten UPDATE.

Store methods (`internal/auth` interface + `sqlitestore` impl):
- `ResolveAlias(ctx, tenant, name) (target string, ok bool, err error)` — single
  lookup; `ok=false` when there is no alias row.
- `ListAliases(ctx, tenant, target string) ([]Alias, error)` — aliases pointing
  at `target` (for CLI/observability).

Alias **mutations are not standalone public methods** — they happen only inside
`RenameRepo`, repo-register, and `DeleteRepoCascade`, so the invariants below
cannot drift. (`repo alias remove` is the one exception: a targeted delete by
`(tenant, old_name)`.)

**Invariant** (test-enforced): no `repo_aliases.old_name` equals a `repos.name`
for the same tenant.

## 4. RenameRepo transaction (`internal/auth/sqlitestore/rename.go`)

Everything below is added to the existing single transaction. Let A = oldName,
B = newName.

### 4.1 Orphan-bug fixes — extend the dependent-table UPDATE loop

```go
{"oidc_trust_rules", `UPDATE oidc_trust_rules SET repo=? WHERE tenant=? AND repo=?`},
{"build_triggers",   `UPDATE build_triggers   SET repo=? WHERE tenant=? AND repo=?`},
```

(The loop currently updates `repo_permissions`, `ssh_keys`, `lfs_locks`,
`protected_refs`, `webhook_endpoints`, `protected_paths`, `hooks` — and misses
these two. A rename today silently orphans an OIDC trust rule and, post-M30, a
build trigger.)

### 4.2 Alias bookkeeping — after the table updates, in this order

1. **Drop shadow:** `DELETE FROM repo_aliases WHERE tenant=? AND old_name=?`
   (old_name = B) — B is becoming a live repo, so it cannot also be an alias.
2. **Flatten chains:** `UPDATE repo_aliases SET target_name=? WHERE tenant=? AND
   target_name=?` (A → B) — any alias that pointed at the old name now points at
   the new one, keeping every alias one hop from a live repo.
3. **Insert/refresh the new alias:**
   `INSERT INTO repo_aliases (tenant, old_name, target_name, created_at)
   VALUES (?, A, B, now) ON CONFLICT(tenant, old_name) DO UPDATE SET
   target_name=excluded.target_name, created_at=excluded.created_at` — the
   ON CONFLICT covers rename-back (A→B then B→A: a stale `A` alias is overwritten
   rather than colliding).

Order (drop-shadow and flatten before insert) prevents any self-referential or
cyclic alias state.

### 4.3 Register shadows alias (`RegisterRepo` / `RegisterRepoIfNew`)

In the same transaction that inserts a repo named N:
`DELETE FROM repo_aliases WHERE tenant=? AND old_name=?` (old_name = N). A freshly
created real repo always wins over a stale alias of the same name — this is how a
renamed-away name is reused, cleanly retiring the old redirect.

### 4.4 Delete cleans aliases (`DeleteRepoCascade`)

`DELETE FROM repo_aliases WHERE tenant=? AND (old_name=? OR target_name=?)` (both
params = the deleted name) — removes both the deleted repo's own alias slot and
any aliases that pointed at it (so they don't dangle).

## 5. Resolution across entrypoints

A shared step at each entrypoint's repo-resolution point, **before** auth and
repo-open:

```
given (tenant, name) from the route:
  if repos has (tenant, name)                              -> use name (unchanged)
  else if ResolveAlias=B and repos has (tenant, B)         -> alias hit, canonical = B
  else                                                     -> 404 (unchanged)
```

Per entrypoint:
- **Web UI** (`internal/web`): alias hit → **302** to the same path with the name
  segment swapped (`/{tenant}/old/settings/x?q` → `/{tenant}/new/settings/x?q`;
  preserve sub-path + query). Do not serve content at the old URL.
- **HTTPS git** (`internal/gateway`) + **LFS**: transparently resolve to the
  canonical repo and serve normally.
- **SSH git** (`internal/sshd`): same transparent resolution.
- **Deprecation hint (best-effort):** where a transport cleanly allows an
  informational message (SSH stderr; git-HTTP sideband on fetch), emit
  "repository renamed to <new>, update your remote." If a phase doesn't allow it,
  resolve silently. Correct resolution is guaranteed; the nudge is opportunistic.

### 5.1 Security & correctness rules (non-negotiable)

1. **Auth on the canonical repo, never bypassed.** Resolve alias → B, then run
   the normal permission check on `(tenant, B)`. An alias is name indirection,
   not an access grant.
2. **Anti-enumeration preserved.** Missing-with-no-alias returns today's uniform
   404; an alias to a repo the actor can't see returns the same status the
   canonical repo would. Aliases don't change existence-leakage.
3. **Target liveness required.** Resolution succeeds only if the target is a live
   repo; a dangling alias yields 404. (Delete cleanup in §4.4 keeps dangling
   aliases from accumulating, but resolution is defensive regardless.)

## 6. CLI & observability

- `bucketvcs repo alias list <tenant>/<name>` — list aliases resolving to the
  repo (`ListAliases`); NDJSON, consistent with other `repo`/`policy` CLIs.
- `bucketvcs repo alias remove <tenant>/<old-name>` — drop one alias (frees the
  redirect early). Rename creates aliases automatically; there is no `alias add`.
- Metric `repo_alias_resolved_total{transport}` (`transport ∈ {ui,https,ssh,lfs}`)
  incremented on each successful alias resolution — shows how much old-name
  traffic still flows. No new audit event (rename already audits + webhooks).

## 7. Testing

- **Store:** rename inserts the alias; flattens chains (A→B→C leaves `A`
  resolving to the live repo); drops a shadow when the new name was an alias;
  refreshes on rename-back (A→B→A); register shadows an alias; delete removes
  aliases by `old_name` and `target_name`; the two bug-fix UPDATEs carry over
  (regression). Invariant test: no `repo_aliases.old_name` exists in `repos`.
- **Resolution:** `ResolveAlias` hit / miss / dangling-target.
- **Entrypoints:** UI returns 302 to the rewritten path; HTTPS git + SSH + LFS
  transparently serve the canonical repo via the old name; missing-no-alias still
  404s.
- **Security:** alias to a private repo enforces auth on the canonical (anon
  denied identically); anti-enumeration parity.

## 8. Docs

Update the repo-rename operator guide: old name now redirects (UI 302) and
git/SSH/LFS transparently resolve; the redirect breaks cleanly when the old name
is reused (register shadows alias); the `repo alias list|remove` CLI; the new
metric. State that the **out-of-band storage-move caveat is unchanged**
(aliasing resolves names, not bytes), and note the immutable-repo-id refactor as
the documented long-term path that would retire both the manual storage move and
this alias layer.

No schema change beyond migration `0018`; no storage/wire change.
