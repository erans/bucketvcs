# Repo Rename Aliases + Redirects Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** After a repo rename, keep the old name resolving to the renamed repo — web UI 302-redirects, and HTTPS-git / SSH-git / LFS transparently resolve — and fix two latent `RenameRepo` orphan bugs.

**Architecture:** New `repo_aliases` table (migration 0018) holds flat single-hop `(tenant, old_name) → target_name` rows, maintained inside the existing `RenameRepo`/register/delete transactions (chain-flattened so resolution is one lookup). A new `auth.RepoAliasResolver` optional interface (implemented only by `*sqlitestore.Store`) is type-asserted by each entrypoint; resolution maps a missing `(tenant, name)` to its live target, with auth always enforced on the canonical repo.

**Tech Stack:** Go, sqlite/libsql/postgres authdb, the existing auth-store transaction patterns, `internal/gateway` (HTTPS+LFS), `internal/sshd`, `internal/web`, `cmd/bucketvcs`.

**Spec:** `docs/superpowers/specs/2026-06-08-repo-rename-alias-redirect-design.md`

---

## File Structure

**Create:**
- `internal/auth/sqlitestore/migrations/0018_repo_aliases.sql` — the table.
- `internal/auth/sqlitestore/aliases.go` — `ResolveAlias`, `ListAliases`, `Alias` type, alias-mutation helpers used by rename/register/delete.
- `internal/auth/sqlitestore/aliases_test.go` — store-level tests.
- `cmd/bucketvcs/repo_alias.go` — `repo alias list|remove` CLI.

**Modify:**
- `internal/auth/aliasresolver.go` (new small file) — the `RepoAliasResolver` interface + `Alias` re-export, in package `auth`.
- `internal/auth/sqlitestore/rename.go` — alias bookkeeping + 2 orphan-bug UPDATEs.
- `internal/auth/sqlitestore/store.go` — `RegisterRepo`/`RegisterRepoIfNew` shadow an alias.
- `internal/auth/sqlitestore/deletecascade.go` — alias cleanup row in `cascadeStmts`.
- `internal/gateway/auth.go` — alias resolution in `RunAuth` (HTTPS git + LFS).
- `internal/gateway/metrics.go` (or nearest gateway metrics file) — `EmitRepoAliasResolvedMetric`.
- `internal/sshd/session.go` — alias resolution + stderr deprecation hint.
- `internal/web/browse.go`, `internal/web/reposettings.go` — 302 on alias hit.
- `cmd/bucketvcs/repocmd.go` — wire `alias` subcommand.
- `docs/operator-guides/` repo-rename doc — document redirect behavior.

---

## Task 1: Migration + alias store methods

**Files:**
- Create: `internal/auth/sqlitestore/migrations/0018_repo_aliases.sql`
- Create: `internal/auth/sqlitestore/aliases.go`
- Test: `internal/auth/sqlitestore/aliases_test.go`

- [ ] **Step 1: Create the migration**

Create `internal/auth/sqlitestore/migrations/0018_repo_aliases.sql`:

```sql
-- Repo rename aliases: (tenant, old_name) -> current live target_name.
-- No FK to repos: old_name must NOT be a live repo, and target cleanup is
-- handled explicitly by RenameRepo / RegisterRepo / DeleteRepoCascade.
CREATE TABLE repo_aliases (
    tenant      TEXT NOT NULL,
    old_name    TEXT NOT NULL,
    target_name TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (tenant, old_name)
);
CREATE INDEX idx_repo_aliases_target ON repo_aliases(tenant, target_name);
```

(Confirm the migrations are embedded/applied automatically — the existing files 0001–0017 are picked up by `RunMigrations`; 0018 follows the same convention with no code change needed to register it. If migrations are listed explicitly anywhere, add 0018 there.)

- [ ] **Step 2: Write the failing test**

Create `internal/auth/sqlitestore/aliases_test.go`:

```go
package sqlitestore

import (
	"context"
	"testing"
)

func TestResolveAlias_HitMiss(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "app"); err != nil {
		t.Fatal(err)
	}
	// No alias yet → miss.
	if _, ok, err := s.ResolveAlias(ctx, "acme", "old"); err != nil || ok {
		t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
	}
	// Insert one directly via the helper and resolve it.
	if err := s.insertAliasForTest(ctx, "acme", "old", "app"); err != nil {
		t.Fatal(err)
	}
	target, ok, err := s.ResolveAlias(ctx, "acme", "old")
	if err != nil || !ok || target != "app" {
		t.Fatalf("resolve: target=%q ok=%v err=%v", target, ok, err)
	}
}

func TestListAliases(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "app")
	_ = s.insertAliasForTest(ctx, "acme", "old1", "app")
	_ = s.insertAliasForTest(ctx, "acme", "old2", "app")
	_ = s.insertAliasForTest(ctx, "acme", "other", "different")
	got, err := s.ListAliases(ctx, "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 aliases targeting app, got %d: %+v", len(got), got)
	}
}
```

`newTestStore(t)` is the existing sqlitestore test helper — find it in the package's other `_test.go` files (e.g. `store_test.go`) and use the same constructor. If the helper has a different name, use whatever the package's existing tests use to get a migrated `*Store`.

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run 'ResolveAlias_HitMiss|ListAliases' -v`
Expected: FAIL — `ResolveAlias`/`ListAliases`/`insertAliasForTest` undefined.

- [ ] **Step 4: Implement the alias store methods**

Create `internal/auth/sqlitestore/aliases.go`:

```go
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Alias is one repo rename alias: (Tenant, OldName) currently resolves to
// the live repo Target.
type Alias struct {
	Tenant    string
	OldName   string
	Target    string
	CreatedAt int64
}

// ResolveAlias returns the current live target for a renamed-away name.
// ok is false when there is no alias for (tenant, name). The caller must
// still verify the target is a live repo and enforce auth on it.
func (s *Store) ResolveAlias(ctx context.Context, tenant, name string) (target string, ok bool, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT target_name FROM repo_aliases WHERE tenant=? AND old_name=?`,
		tenant, name)
	switch err := row.Scan(&target); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("sqlitestore.ResolveAlias: %w", err)
	}
	return target, true, nil
}

// ListAliases returns all aliases whose target is (tenant, target), ordered
// by old_name. Returns a nil slice (not error) when none.
func (s *Store) ListAliases(ctx context.Context, tenant, target string) ([]Alias, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant, old_name, target_name, created_at
		   FROM repo_aliases WHERE tenant=? AND target_name=? ORDER BY old_name ASC`,
		tenant, target)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore.ListAliases: %w", err)
	}
	defer rows.Close()
	var out []Alias
	for rows.Next() {
		var a Alias
		if err := rows.Scan(&a.Tenant, &a.OldName, &a.Target, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlitestore.ListAliases scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RemoveAlias deletes one alias by (tenant, old_name). Returns auth.ErrNoSuchRepo-
// style nil when nothing matched (idempotent); reports affected via ok.
func (s *Store) RemoveAlias(ctx context.Context, tenant, oldName string) (removed bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM repo_aliases WHERE tenant=? AND old_name=?`, tenant, oldName)
	if err != nil {
		return false, fmt.Errorf("sqlitestore.RemoveAlias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlitestore.RemoveAlias rows: %w", err)
	}
	return n > 0, nil
}

// insertAliasForTest is a test-only helper to seed an alias row directly.
func (s *Store) insertAliasForTest(ctx context.Context, tenant, oldName, target string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO repo_aliases (tenant, old_name, target_name, created_at)
		 VALUES (?, ?, ?, ?)`,
		tenant, oldName, target, time.Now().Unix())
	return err
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/auth/sqlitestore/ -run 'ResolveAlias_HitMiss|ListAliases' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/sqlitestore/migrations/0018_repo_aliases.sql internal/auth/sqlitestore/aliases.go internal/auth/sqlitestore/aliases_test.go
git commit -m "feat(auth): repo_aliases table + ResolveAlias/ListAliases/RemoveAlias"
```

---

## Task 2: RenameRepo — alias bookkeeping + orphan-bug fixes

**Files:**
- Modify: `internal/auth/sqlitestore/rename.go`
- Test: `internal/auth/sqlitestore/rename_test.go` (append; create if absent)

- [ ] **Step 1: Write the failing tests**

Append to `internal/auth/sqlitestore/rename_test.go` (use the package's existing test-store constructor):

```go
func TestRenameRepo_CreatesAliasAndFlattensChain(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "a"); err != nil {
		t.Fatal(err)
	}
	// a -> b
	if err := s.RenameRepo(ctx, "acme", "a", "b"); err != nil {
		t.Fatalf("rename a->b: %v", err)
	}
	if tgt, ok, _ := s.ResolveAlias(ctx, "acme", "a"); !ok || tgt != "b" {
		t.Fatalf("alias a should target b, got %q ok=%v", tgt, ok)
	}
	// b -> c : the 'a' alias must flatten to c.
	if err := s.RenameRepo(ctx, "acme", "b", "c"); err != nil {
		t.Fatalf("rename b->c: %v", err)
	}
	if tgt, ok, _ := s.ResolveAlias(ctx, "acme", "a"); !ok || tgt != "c" {
		t.Fatalf("alias a should flatten to c, got %q ok=%v", tgt, ok)
	}
	if tgt, ok, _ := s.ResolveAlias(ctx, "acme", "b"); !ok || tgt != "c" {
		t.Fatalf("alias b should target c, got %q ok=%v", tgt, ok)
	}
}

func TestRenameRepo_RenameBackDropsShadow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "a")
	_ = s.RenameRepo(ctx, "acme", "a", "b") // alias a->b
	if err := s.RenameRepo(ctx, "acme", "b", "a"); err != nil {
		t.Fatalf("rename b->a: %v", err)
	}
	// 'a' is now a live repo again → must NOT be an alias.
	if _, ok, _ := s.ResolveAlias(ctx, "acme", "a"); ok {
		t.Fatal("alias 'a' must be dropped when 'a' becomes a live repo again")
	}
	// 'b' now aliases to 'a'.
	if tgt, ok, _ := s.ResolveAlias(ctx, "acme", "b"); !ok || tgt != "a" {
		t.Fatalf("alias b should target a, got %q ok=%v", tgt, ok)
	}
	// Invariant: no alias old_name is a live repo.
	assertNoAliasShadowsRepo(t, s, "acme")
}

// assertNoAliasShadowsRepo checks the core invariant.
func assertNoAliasShadowsRepo(t *testing.T, s *Store, tenant string) {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT a.old_name FROM repo_aliases a JOIN repos r
		   ON a.tenant=r.tenant AND a.old_name=r.name WHERE a.tenant=?`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		t.Errorf("invariant violated: alias old_name=%q is also a live repo", n)
	}
}

func TestRenameRepo_CarriesOidcAndBuildTriggers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "a")
	// Seed one oidc_trust_rule and one build_trigger for (acme, a).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO oidc_trust_rules (id, tenant, repo, issuer, audience, scopes, ttl_seconds, created_at)
		 VALUES ('r1','acme','a','iss','aud',0,900, strftime('%s','now'))`); err != nil {
		t.Fatalf("seed oidc rule: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO build_triggers (id, tenant, repo, name, kind, config_json, ref_include, ref_exclude,
		    token_mode, token_scopes, token_ttl_seconds, active, created_at)
		 VALUES ('bt1','acme','a','n','generic','{}','[]','[]','none',0,900,1, strftime('%s','now'))`); err != nil {
		t.Fatalf("seed build trigger: %v", err)
	}
	if err := s.RenameRepo(ctx, "acme", "a", "b"); err != nil {
		t.Fatal(err)
	}
	var oidc, bt int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oidc_trust_rules WHERE tenant='acme' AND repo='b'`).Scan(&oidc)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM build_triggers WHERE tenant='acme' AND repo='b'`).Scan(&bt)
	if oidc != 1 || bt != 1 {
		t.Fatalf("rename must carry oidc_trust_rules (%d) and build_triggers (%d) to new name", oidc, bt)
	}
}
```

(If the exact column lists for `oidc_trust_rules` / `build_triggers` differ, adjust the seed INSERTs to the real schema — read migrations 0010 and 0017. The assertion (COUNT at new name == 1) is the point.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/auth/sqlitestore/ -run 'RenameRepo_(CreatesAlias|RenameBack|CarriesOidc)' -v`
Expected: FAIL — no alias is created; oidc/build_triggers rows are orphaned at the old name.

- [ ] **Step 3: Add the two orphan-bug UPDATEs**

In `internal/auth/sqlitestore/rename.go`, add two entries to the `[]stmt{...}` loop (after `hooks`):

```go
		{"hooks", `UPDATE hooks SET repo=? WHERE tenant=? AND repo=?`},
		{"oidc_trust_rules", `UPDATE oidc_trust_rules SET repo=? WHERE tenant=? AND repo=?`},
		{"build_triggers", `UPDATE build_triggers SET repo=? WHERE tenant=? AND repo=?`},
```

- [ ] **Step 4: Add alias bookkeeping after the parent UPDATE**

In `internal/auth/sqlitestore/rename.go`, immediately after the `UPDATE repos SET name=?` block succeeds (the `res, err := tx.ExecContext(...)` + RowsAffected check) and **before** `tx.Commit()`, insert:

```go
	// Alias bookkeeping (A=oldName, B=newName), in order:
	// 1. drop any alias whose old_name == B (B is now a live repo).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM repo_aliases WHERE tenant=? AND old_name=?`, tenant, newName); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: drop shadow alias: %w", err)
	}
	// 2. flatten chains: aliases pointing at A now point at B.
	if _, err := tx.ExecContext(ctx,
		`UPDATE repo_aliases SET target_name=? WHERE tenant=? AND target_name=?`,
		newName, tenant, oldName); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: flatten aliases: %w", err)
	}
	// 3. insert/refresh the A->B alias (ON CONFLICT covers rename-back).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO repo_aliases (tenant, old_name, target_name, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(tenant, old_name) DO UPDATE SET
		   target_name=excluded.target_name, created_at=excluded.created_at`,
		tenant, oldName, newName, time.Now().Unix()); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: insert alias: %w", err)
	}
```

Ensure `time` is imported in `rename.go` (add it if not present).

Note on FK deferral: the table-update loop runs under `defer_foreign_keys`; `repo_aliases` has no FK, so these statements are unaffected by it and are safe before commit.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/auth/sqlitestore/ -run 'RenameRepo' -v`
Expected: PASS — including any pre-existing `RenameRepo` tests.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/sqlitestore/rename.go internal/auth/sqlitestore/rename_test.go
git commit -m "feat(auth): RenameRepo maintains aliases + carries oidc_trust_rules/build_triggers"
```

---

## Task 3: Register shadows alias + delete cleans aliases

**Files:**
- Modify: `internal/auth/sqlitestore/store.go` (RegisterRepo, RegisterRepoIfNew)
- Modify: `internal/auth/sqlitestore/deletecascade.go` (cascadeStmts)
- Test: `internal/auth/sqlitestore/aliases_test.go` (append)

- [ ] **Step 1: Write the failing tests**

Append to `internal/auth/sqlitestore/aliases_test.go`:

```go
func TestRegisterRepo_ShadowsAlias(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "a")
	_ = s.RenameRepo(ctx, "acme", "a", "b") // alias a->b
	// Re-create a real repo named 'a' → the alias must be dropped.
	if err := s.RegisterRepo(ctx, "acme", "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ResolveAlias(ctx, "acme", "a"); ok {
		t.Fatal("registering a real repo named 'a' must drop the 'a' alias")
	}
}

func TestDeleteRepoCascade_CleansAliases(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "a")
	_ = s.RenameRepo(ctx, "acme", "a", "b") // alias a->b, live repo b
	if err := s.DeleteRepoCascade(ctx, "acme", "b"); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	// Alias a->b must be gone (target deleted).
	if _, ok, _ := s.ResolveAlias(ctx, "acme", "a"); ok {
		t.Fatal("deleting target repo must remove aliases pointing at it")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/auth/sqlitestore/ -run 'RegisterRepo_ShadowsAlias|DeleteRepoCascade_CleansAliases' -v`
Expected: FAIL — alias survives register and survives delete.

- [ ] **Step 3: Make RegisterRepo / RegisterRepoIfNew shadow an alias**

In `internal/auth/sqlitestore/store.go`, both `RegisterRepo` and `RegisterRepoIfNew` currently do a single `INSERT ... ON CONFLICT DO NOTHING`. Wrap each so the alias for the same name is dropped in the same transaction. Replace `RegisterRepo` with:

```go
func (s *Store) RegisterRepo(ctx context.Context, tenant, name string) error {
	_, err := s.registerRepo(ctx, tenant, name)
	return err
}

// RegisterRepoIfNew is like RegisterRepo but additionally reports whether
// the row was actually created.
func (s *Store) RegisterRepoIfNew(ctx context.Context, tenant, name string) (bool, error) {
	return s.registerRepo(ctx, tenant, name)
}

// registerRepo inserts the repo (idempotent) and, whether or not the insert
// was new, drops any alias of the same name — a live repo always shadows a
// stale alias. Both run in one transaction so the invariant holds atomically.
func (s *Store) registerRepo(ctx context.Context, tenant, name string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("sqlitestore.registerRepo: begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, ?)
		 ON CONFLICT(tenant, name) DO NOTHING`,
		tenant, name, time.Now().Unix())
	if err != nil {
		return false, fmt.Errorf("sqlitestore.registerRepo: insert: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM repo_aliases WHERE tenant=? AND old_name=?`, tenant, name); err != nil {
		return false, fmt.Errorf("sqlitestore.registerRepo: shadow alias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlitestore.registerRepo: rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("sqlitestore.registerRepo: commit: %w", err)
	}
	return n > 0, nil
}
```

(Confirm `fmt` and `time` are imported in `store.go` — they almost certainly are.)

- [ ] **Step 4: Make DeleteRepoCascade clean aliases**

In `internal/auth/sqlitestore/deletecascade.go`, add one entry to the `cascadeStmts` array, BEFORE the final `repos` delete. Note the existing statements bind `(tenant, repo)` as the two `?` params; this one needs the repo name in two places, so use named-free positional repetition — but the cascade loop calls `tx.ExecContext(ctx, st.sql, tenant, repo)` with exactly two args. To delete by `old_name=? OR target_name=?` we'd need three binds. Simplest correct fix: add it as a statement whose two binds are `(tenant, repo)` matching `old_name`, plus a SECOND statement for `target_name`:

```go
	{"repo_aliases (as old_name)", `DELETE FROM repo_aliases WHERE tenant=? AND old_name=?`},
	{"repo_aliases (as target)", `DELETE FROM repo_aliases WHERE tenant=? AND target_name=?`},
	{"repos", `DELETE FROM repos WHERE tenant=? AND name=?`},
```

(Place the two alias deletes immediately before the existing `repos` entry. Each receives `(tenant, repo)` from the loop's `tx.ExecContext(ctx, st.sql, tenant, repo)`, which matches both `?` placeholders.)

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/auth/sqlitestore/ -run 'RegisterRepo_ShadowsAlias|DeleteRepoCascade_CleansAliases' -v`
Expected: PASS.

- [ ] **Step 6: Run the full auth-store package**

Run: `go test ./internal/auth/sqlitestore/`
Expected: PASS (existing register/delete/rename tests still green).

- [ ] **Step 7: Commit**

```bash
git add internal/auth/sqlitestore/store.go internal/auth/sqlitestore/deletecascade.go internal/auth/sqlitestore/aliases_test.go
git commit -m "feat(auth): register shadows alias; delete cascade cleans aliases"
```

---

## Task 4: RepoAliasResolver interface + metric

**Files:**
- Create: `internal/auth/aliasresolver.go`
- Modify: a gateway metrics file (e.g. `internal/gateway/metrics.go`; if none, create `internal/gateway/aliasmetric.go`)

- [ ] **Step 1: Define the optional interface**

Create `internal/auth/aliasresolver.go`:

```go
package auth

import "context"

// RepoAliasResolver is an optional capability: a Store that also resolves
// rename aliases. Entrypoints type-assert their Store to this interface; a
// Store that doesn't implement it simply has no alias resolution (old names
// 404 as before). Implemented by *sqlitestore.Store.
type RepoAliasResolver interface {
	// ResolveAlias returns the current live target for a renamed-away name.
	// ok is false when there is no alias. Callers MUST still verify the
	// target is a live repo and enforce auth on the canonical repo.
	ResolveAlias(ctx context.Context, tenant, name string) (target string, ok bool, err error)
}
```

- [ ] **Step 2: Add the metric emitter**

Add to a gateway metrics file (match the existing `EmitRateLimitMetric` style):

```go
// EmitRepoAliasResolvedMetric logs one repo_alias_resolved_total{transport}
// sample. transport ∈ {ui, https, ssh, lfs}.
func EmitRepoAliasResolvedMetric(ctx context.Context, logger *slog.Logger, transport string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "repo_alias_resolved_total"),
		slog.String("transport", transport),
		slog.Int("value", 1),
	)
}
```

Place it in whichever gateway file already imports `context`/`log/slog` for metrics; if the web package needs it too, it can call the gateway one (export it) or define a sibling — keep a single metric name `repo_alias_resolved_total` regardless.

- [ ] **Step 3: Build**

Run: `go build ./internal/auth/ ./internal/gateway/`
Expected: success (no callers yet).

- [ ] **Step 4: Commit**

```bash
git add internal/auth/aliasresolver.go internal/gateway/
git commit -m "feat(auth): RepoAliasResolver interface + repo_alias_resolved_total metric"
```

---

## Task 5: HTTPS git + LFS alias resolution (gateway)

**Files:**
- Modify: `internal/gateway/auth.go`
- Test: `internal/gateway/auth_test.go` (append; or the nearest gateway test file with a real sqlitestore-backed `auth.Store`)

- [ ] **Step 1: Write the failing test**

Append a test that drives `RunAuth` (or the smallest exported handler that calls it) for a renamed repo. Use a real `*sqlitestore.Store` (so `RepoAliasResolver` is satisfied) seeded with repo `b`, alias `a→b`, and a token granting read on `b`. Assert a request for `a` is served as `b` (e.g. the resolved `rr.Repo == "b"`, or the handler returns 200 rather than 404). Model it on the existing gateway auth tests' construction. Concretely:

```go
func TestRunAuth_ResolvesAlias(t *testing.T) {
	store := newGatewayTestStore(t) // real sqlitestore-backed auth.Store
	ctx := context.Background()
	_ = store.RegisterRepo(ctx, "acme", "b")
	_ = store.RenameRepo(ctx, "acme", "a", "b") // requires 'a' to have existed; instead:
	// Simpler: register 'a', rename to 'b' so alias a->b exists and b is live.
	// (Adjust seeding to whatever the test store exposes.)

	rr := &RoutedRequest{Tenant: "acme", Repo: "a", /* op fields as needed */}
	// ... build an anonymous or token request for a public/granted repo ...
	// Call RunAuth and assert it did NOT 404 and rr.Repo was rewritten to "b".
}
```

Because the gateway test harness shape varies, the implementer should: (a) find how existing `internal/gateway` tests construct a store + `RoutedRequest` + `httptest` request and call `RunAuth`; (b) seed register('a')→rename('a','b'); (c) assert old-name `a` resolves (no 404) and the effective repo is `b`. If `RunAuth` returns before exposing the rewritten name, assert via the response (a granted/public repo returns non-404 for `a`).

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/gateway/ -run 'RunAuth_ResolvesAlias' -v`
Expected: FAIL — old name 404s (no resolution yet).

- [ ] **Step 3: Insert alias resolution in RunAuth**

In `internal/gateway/auth.go`, replace the `GetRepoFlags` choke block:

```go
	flags, err := store.GetRepoFlags(ctx, rr.Tenant, rr.Repo)
	if errors.Is(err, auth.ErrNoSuchRepo) {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
```

with:

```go
	flags, err := store.GetRepoFlags(ctx, rr.Tenant, rr.Repo)
	if errors.Is(err, auth.ErrNoSuchRepo) {
		// Alias fallback: a renamed-away name resolves to its live target.
		if resolver, ok := store.(auth.RepoAliasResolver); ok {
			if target, found, rerr := resolver.ResolveAlias(ctx, rr.Tenant, rr.Repo); rerr == nil && found {
				if f2, e2 := store.GetRepoFlags(ctx, rr.Tenant, target); e2 == nil {
					transport := "https"
					if rr.IsLFS() { // see note below
						transport = "lfs"
					}
					EmitRepoAliasResolvedMetric(ctx, logger, transport)
					rr.Repo = target // serve the canonical repo transparently
					flags = f2
					goto authProceed
				}
			}
		}
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
authProceed:
```

Notes for the implementer:
- If `goto`/label clashes with the surrounding code style, restructure as a small helper `resolveRepoFlags(ctx, store, rr, logger) (auth.RepoFlags, bool)` that returns the flags (rewriting `rr.Repo`) or signals 404 — same logic, no `goto`. Prefer the helper if the function is large.
- `rr.IsLFS()` is illustrative — determine LFS vs git from the existing `RoutedRequest` fields (it already distinguishes operations for `requiredAction`); if there's no clean LFS predicate, pass `"https"` for both and note LFS shares the label, or add the predicate. The metric label is best-effort.
- Auth continues on the rewritten `rr.Repo`, so all permission checks below run against the canonical repo (security rule #1).

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/gateway/ -run 'RunAuth_ResolvesAlias' -v`
Expected: PASS.

- [ ] **Step 5: Run the gateway package**

Run: `go test ./internal/gateway/`
Expected: PASS — existing 404-for-truly-missing tests still pass (no alias → still 404).

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/auth.go internal/gateway/auth_test.go
git commit -m "feat(gateway): resolve rename aliases for HTTPS git + LFS"
```

---

## Task 6: SSH git alias resolution

**Files:**
- Modify: `internal/sshd/session.go`
- Test: `internal/sshd/session_test.go` (append, matching existing SSH test harness)

- [ ] **Step 1: Write the failing test**

Find how existing `internal/sshd` tests construct a session with a real store and invoke command handling. Add a test that seeds register('a')→rename('a','b') and asserts an SSH git command against `a` resolves to `b` (served, not "repository not found"). If the SSH test harness is heavy, at minimum assert the resolution helper rewrites `cmd.Repo` to `b`. Model on existing session tests.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/sshd/ -run 'Alias' -v`
Expected: FAIL.

- [ ] **Step 3: Insert resolution in session.go**

In `internal/sshd/session.go`, replace the `GetRepoFlags` block:

```go
	flags, err := s.opts.Store.GetRepoFlags(ctx, cmd.Tenant, cmd.Repo)
	if err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			sendStderrLine(ch, "bucketvcs: repository not found")
			sendExitStatus(ch, 128)
			return
		}
		sendStderrLine(ch, "bucketvcs: internal error")
		sendExitStatus(ch, 1)
		return
	}
```

with:

```go
	flags, err := s.opts.Store.GetRepoFlags(ctx, cmd.Tenant, cmd.Repo)
	if errors.Is(err, auth.ErrNoSuchRepo) {
		if resolver, ok := s.opts.Store.(auth.RepoAliasResolver); ok {
			if target, found, rerr := resolver.ResolveAlias(ctx, cmd.Tenant, cmd.Repo); rerr == nil && found {
				if f2, e2 := s.opts.Store.GetRepoFlags(ctx, cmd.Tenant, target); e2 == nil {
					sendStderrLine(ch, "bucketvcs: repository renamed to "+cmd.Tenant+"/"+target+"; update your remote")
					cmd.Repo = target
					flags = f2
					err = nil
				}
			}
		}
	}
	if err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			sendStderrLine(ch, "bucketvcs: repository not found")
			sendExitStatus(ch, 128)
			return
		}
		sendStderrLine(ch, "bucketvcs: internal error")
		sendExitStatus(ch, 1)
		return
	}
```

The deprecation hint goes to SSH stderr (allowed by the protocol). Auth below runs on the rewritten `cmd.Repo` (the existing `scope.Repo != cmd.Repo` check and `LookupRepoPerm(..., cmd.Repo)` now compare against the canonical name — correct).

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/sshd/ -run 'Alias' -v`
Expected: PASS.

- [ ] **Step 5: Run the sshd package**

Run: `go test ./internal/sshd/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/sshd/session.go internal/sshd/session_test.go
git commit -m "feat(sshd): resolve rename aliases for SSH git + deprecation hint"
```

---

## Task 7: Web UI 302 redirect on alias hit

**Files:**
- Modify: `internal/web/browse.go`, `internal/web/reposettings.go`
- Test: `internal/web/browse_test.go` (append, matching existing web test harness)

- [ ] **Step 1: Write the failing test**

Find how existing `internal/web` tests construct a `server` with a real sqlitestore-backed `DataStore` and issue requests. Add a test: seed register('a')→rename('a','b'); GET `/acme/a` (browse) → expect **302** with `Location: /acme/b`; GET `/acme/a/settings` → 302 to `/acme/b/settings`. Model on existing browse/settings tests.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/web/ -run 'AliasRedirect' -v`
Expected: FAIL — old name renders 404.

- [ ] **Step 3: Add a shared redirect helper**

In `internal/web/browse.go` (or a small new `internal/web/aliasredirect.go`), add:

```go
// aliasRedirect checks whether (tenant, name) is a rename alias and, if so,
// writes a 302 to the same path with the repo segment swapped to the target.
// Returns true if it handled the request (redirect written). transport label
// is "ui". Auth is NOT bypassed: the redirect just points the browser at the
// canonical URL, which enforces its own auth.
func (s *server) aliasRedirect(w http.ResponseWriter, r *http.Request, tenant, name string) bool {
	resolver, ok := s.store.(auth.RepoAliasResolver)
	if !ok {
		return false
	}
	target, found, err := resolver.ResolveAlias(r.Context(), tenant, name)
	if err != nil || !found {
		return false
	}
	// Confirm the target is live before redirecting (defensive; delete cleans
	// dangling aliases, but never trust that on the hot path).
	if _, e := s.store.GetRepoFlags(r.Context(), tenant, target); e != nil {
		return false
	}
	// Swap the first occurrence of "/{tenant}/{name}" → "/{tenant}/{target}",
	// preserving the trailing sub-path and query.
	oldPrefix := "/" + tenant + "/" + name
	newPrefix := "/" + tenant + "/" + target
	dest := newPrefix + strings.TrimPrefix(r.URL.Path, oldPrefix)
	if r.URL.RawQuery != "" {
		dest += "?" + r.URL.RawQuery
	}
	EmitRepoAliasResolvedMetric(r.Context(), s.logger, "ui")
	http.Redirect(w, r, dest, http.StatusFound) // 302
	return true
}
```

Ensure `internal/web` imports `auth`, `net/http`, `strings`, and has access to `EmitRepoAliasResolvedMetric` (export it from gateway, or define the emitter in a shared place / duplicate the tiny function in web with the same metric name). Keep the metric name identical.

- [ ] **Step 4: Call it at the two 404 sites**

In `internal/web/browse.go` `handleBrowse`, change the `GetVisibleRepo` failure branch:

```go
	if _, err := s.store.GetVisibleRepo(r.Context(), actorFromSession(sess), br.tenant, br.repo); err != nil {
		if s.aliasRedirect(w, r, br.tenant, br.repo) {
			return
		}
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
```

In `internal/web/reposettings.go` `handleRepoSettings`, change the `GetRepoFlags` `ErrNoSuchRepo` branch:

```go
	if _, err := s.store.GetRepoFlags(r.Context(), sr.tenant, sr.repo); err != nil {
		if !errors.Is(err, auth.ErrNoSuchRepo) {
			s.logger.Error("repo settings: existence probe", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		if s.aliasRedirect(w, r, sr.tenant, sr.repo) {
			return
		}
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
```

Anti-enumeration note: the redirect only fires for a real alias to a live repo; everything else still renders the uniform 404. The 302 reveals the new public-or-not name, which is intended (GitHub does the same) and no worse than the canonical URL's own behavior.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/web/ -run 'AliasRedirect' -v`
Expected: PASS.

- [ ] **Step 6: Run the web package**

Run: `go test ./internal/web/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/web/
git commit -m "feat(web): 302-redirect old repo names to their rename target"
```

---

## Task 8: CLI — repo alias list|remove

**Files:**
- Create: `cmd/bucketvcs/repo_alias.go`
- Modify: `cmd/bucketvcs/repocmd.go`
- Test: `cmd/bucketvcs/repo_alias_test.go`

- [ ] **Step 1: Wire the subcommand**

In `cmd/bucketvcs/repocmd.go`, add to the `switch sub` in `runRepo`:

```go
	case "alias":
		return runRepoAlias(ctx, rest, stdout, stderr)
```

and update the usage line to include `alias`.

- [ ] **Step 2: Write the failing test**

Create `cmd/bucketvcs/repo_alias_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRepoAlias_ListAndRemove(t *testing.T) {
	db := newCLITestAuthDB(t) // existing CLI test helper for a temp authdb path
	ctx := context.Background()

	// Seed: register a, rename a->b (creates alias a->b).
	mustRunRepo(t, ctx, "register", "--auth-db="+db, "acme/a")
	mustRunRepo(t, ctx, "rename", "--auth-db="+db, "acme/a", "b")

	// list → shows alias a->b
	var out bytes.Buffer
	if code := runRepo(ctx, []string{"alias", "list", "--auth-db=" + db, "acme/b", "--format=json"}, &out, &out); code != 0 {
		t.Fatalf("alias list exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), `"alias":"a"`) || !strings.Contains(out.String(), `"target":"b"`) {
		t.Fatalf("alias list missing a->b: %s", out.String())
	}

	// remove → alias gone
	out.Reset()
	if code := runRepo(ctx, []string{"alias", "remove", "--auth-db=" + db, "acme/a"}, &out, &out); code != 0 {
		t.Fatalf("alias remove exit=%d out=%s", code, out.String())
	}
	out.Reset()
	_ = runRepo(ctx, []string{"alias", "list", "--auth-db=" + db, "acme/b", "--format=json"}, &out, &out)
	if strings.Contains(out.String(), `"alias":"a"`) {
		t.Fatalf("alias should be gone after remove: %s", out.String())
	}
}
```

Adapt `newCLITestAuthDB`/`mustRunRepo` to the package's existing CLI test helpers (find how other `cmd/bucketvcs` tests run subcommands and create a temp authdb). If no `mustRunRepo` helper exists, call `runRepo(ctx, []string{...}, &buf, &buf)` directly and assert exit 0.

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./cmd/bucketvcs/ -run 'RepoAlias_ListAndRemove' -v`
Expected: FAIL — `unknown subcommand "alias"` / undefined `runRepoAlias`.

- [ ] **Step 4: Implement the CLI**

Create `cmd/bucketvcs/repo_alias.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

func runRepoAlias(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo alias <list|remove>")
		return 2
	}
	switch args[0] {
	case "list":
		return repoAliasList(ctx, args[1:], stdout, stderr)
	case "remove":
		return repoAliasRemove(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "repo alias: unknown action %q\n", args[0])
		return 2
	}
}

// splitTenantRepo parses "tenant/name". Returns ok=false on malformed input.
func splitTenantRepo(s string) (tenant, name string, ok bool) {
	t, n, found := strings.Cut(s, "/")
	if !found || t == "" || n == "" || strings.Contains(n, "/") {
		return "", "", false
	}
	return t, n, true
}

func repoAliasList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo alias list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	format := fs.String("format", "text", "Output format: text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo alias list --auth-db=<path> <tenant>/<name> [--format=text|json]")
		return 2
	}
	tenant, name, ok := splitTenantRepo(fs.Arg(0))
	if !ok {
		fmt.Fprintln(stderr, "repo alias list: argument must be <tenant>/<name>")
		return 2
	}
	store, closeFn, err := openSqliteStore(*authDB) // existing CLI helper to open *sqlitestore.Store
	if err != nil {
		fmt.Fprintf(stderr, "repo alias list: %v\n", err)
		return 1
	}
	defer closeFn()
	aliases, err := store.ListAliases(ctx, tenant, name)
	if err != nil {
		fmt.Fprintf(stderr, "repo alias list: %v\n", err)
		return 1
	}
	if len(aliases) == 0 {
		if *format == "json" {
			return 0
		}
		fmt.Fprintf(stdout, "tenant=%s  repo=%s  (no aliases)\n", tenant, name)
		return 0
	}
	for _, a := range aliases {
		if *format == "json" {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"tenant":     a.Tenant,
				"alias":      a.OldName,
				"target":     a.Target,
				"created_at": time.Unix(a.CreatedAt, 0).UTC().Format(time.RFC3339),
			})
			continue
		}
		fmt.Fprintf(stdout, "tenant=%s  alias=%s  target=%s  created=%s\n",
			a.Tenant, a.OldName, a.Target, time.Unix(a.CreatedAt, 0).UTC().Format(time.RFC3339))
	}
	return 0
}

func repoAliasRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo alias remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo alias remove --auth-db=<path> <tenant>/<old-name>")
		return 2
	}
	tenant, oldName, ok := splitTenantRepo(fs.Arg(0))
	if !ok {
		fmt.Fprintln(stderr, "repo alias remove: argument must be <tenant>/<old-name>")
		return 2
	}
	store, closeFn, err := openSqliteStore(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "repo alias remove: %v\n", err)
		return 1
	}
	defer closeFn()
	removed, err := store.RemoveAlias(ctx, tenant, oldName)
	if err != nil {
		fmt.Fprintf(stderr, "repo alias remove: %v\n", err)
		return 1
	}
	if !removed {
		fmt.Fprintf(stderr, "repo alias remove: no alias %s/%s\n", tenant, oldName)
		return 1
	}
	fmt.Fprintf(stdout, "removed alias %s/%s\n", tenant, oldName)
	return 0
}
```

`openSqliteStore` is illustrative — use whatever the existing `repo` CLI commands use to open the authdb as a `*sqlitestore.Store` (look at `repoRegister`/`repoRename` in the package; reuse that exact open/close helper). `ListAliases`/`RemoveAlias` are concrete methods on `*sqlitestore.Store`, so no interface needed here.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./cmd/bucketvcs/ -run 'RepoAlias_ListAndRemove' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/bucketvcs/repo_alias.go cmd/bucketvcs/repocmd.go cmd/bucketvcs/repo_alias_test.go
git commit -m "feat(cli): repo alias list|remove"
```

---

## Task 9: Documentation

**Files:**
- Modify: the repo-rename operator guide (find it: `grep -rl "repo rename\|repo_rename\|RenameRepo" docs/operator-guides/`; likely `docs/operator-guides/repo-management.md` or similar — if no dedicated page exists, add a "Repo rename & redirects" section to the most relevant operator guide).

- [ ] **Step 1: Document the redirect behavior**

Add/extend a section covering:
- After `bucketvcs repo rename <tenant>/<old> <new>`, the old name keeps working: the **web UI 302-redirects** to the new name, and **HTTPS git, SSH git, and LFS transparently resolve** to the renamed repo (SSH prints a "repository renamed… update your remote" hint).
- The redirect **breaks cleanly when the old name is reused**: registering a new repo with the old name shadows (removes) the alias.
- Aliases are kept until the name is reused; inspect/manage with `bucketvcs repo alias list <tenant>/<name>` and `bucketvcs repo alias remove <tenant>/<old-name>`.
- The new metric `repo_alias_resolved_total{transport=ui|https|ssh|lfs}` shows how much old-name traffic still flows (i.e. who hasn't updated remotes).
- **Unchanged caveat:** storage objects are still moved out-of-band (aliasing resolves names, not bytes) — keep the existing storage-migration note.
- Note for maintainers: the long-term direction is an immutable repo-id refactor that would make both rename and the storage move free; the alias layer is the interim solution.

Use this exact prose block as the core (adapt headings to the file):

````markdown
## Repo rename redirects

Renaming a repo (`bucketvcs repo rename <tenant>/<old> <new>`, or the web UI
Settings → Rename form) now leaves the **old name working**:

- **Web UI:** requests to `/{tenant}/{old}/…` return **302** to `/{tenant}/{new}/…`.
- **Git (HTTPS + SSH) and LFS:** clone/fetch/push against the old name resolve
  transparently to the renamed repo. SSH additionally prints
  `repository renamed to <tenant>/<new>; update your remote`.

The redirect is backed by a `repo_aliases` row created at rename time. It
**stops** as soon as the old name is reused: registering a new repo with the old
name removes the alias (a live repo always shadows an alias). Chained renames
(`a→b→c`) keep the oldest alias pointing at the current name.

Manage aliases:

```bash
bucketvcs repo alias list   --auth-db=<path> <tenant>/<name>     # aliases pointing at a repo
bucketvcs repo alias remove --auth-db=<path> <tenant>/<old-name> # drop a redirect early
```

Observe old-name traffic via `repo_alias_resolved_total{transport}`
(`transport ∈ ui|https|ssh|lfs`).

**Storage is still moved out of band** — aliasing resolves *names*, not bytes;
the existing requirement to relocate `tenants/<tenant>/repos/<old>/…` to
`…/<new>/…` is unchanged.
````

- [ ] **Step 2: Commit**

```bash
git add docs/operator-guides/
git commit -m "docs: repo rename redirect behavior + alias CLI"
```

---

## Task 10: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 2: Vet**

Run: `go vet ./internal/auth/... ./internal/gateway/ ./internal/sshd/ ./internal/web/ ./cmd/bucketvcs/`
Expected: no findings.

- [ ] **Step 3: Targeted + full tests**

Run: `go test ./internal/auth/... ./internal/gateway/ ./internal/sshd/ ./internal/web/ ./cmd/bucketvcs/`
Then: `go test ./...`
Expected: PASS, including all new alias tests and all pre-existing tests.

- [ ] **Step 4: Invariant + migration sanity**

Run: `go test ./internal/auth/sqlitestore/ -run 'Alias|Rename|Register|Delete' -v`
Expected: PASS, including `assertNoAliasShadowsRepo` invariant.
Confirm migration count: `ls internal/auth/sqlitestore/migrations/ | tail -1` shows `0018_repo_aliases.sql`.

- [ ] **Step 5: Request code review**

Use the superpowers:requesting-code-review skill (or `/roborev-review-branch`) before merging.
