# M14 Hooks and Policy (Tier 1: protected refs) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-repo protected-ref rules to the M4 authdb and enforce them in `receivepack.completeReceivePack` before manifest CAS commit; reject deletions and non-fast-forward pushes that match a glob-protected refname pattern.

**Architecture:** New `internal/policy` package wraps a `protected_refs` table on the M4 authdb sqlite. `Service.CheckUpdate` matches each ref update's refname against the repo's rules via stdlib `path.Match` and runs `git merge-base --is-ancestor` against the local bare for force-push detection. A new step 8b in `completeReceivePack` calls CheckUpdate per accepted update; rejections set the per-ref `ng <refname> protected-branch: ...` status that the existing atomic/non-atomic batch handling propagates. M14 is fully opt-in via `nil` `EngineRequest.Policy` (pre-M14 behavior preserved).

**Tech Stack:** Go, sqlite (M4 authdb), stdlib `path.Match` for globs, `os/exec` for `git merge-base --is-ancestor`, slog for metrics + audit.

**Spec:** `docs/superpowers/specs/2026-05-21-m14-hooks-policy-design.md`

**Worktree:** `/home/eran/work/bucketvcs/.claude/worktrees/m14-hooks-policy` (branch `worktree-m14-hooks-policy`).

---

## Pre-flight

- [ ] **Step 0.1: Verify branch + spec presence**

```bash
cd /home/eran/work/bucketvcs/.claude/worktrees/m14-hooks-policy
git rev-parse --abbrev-ref HEAD
ls docs/superpowers/specs/2026-05-21-m14-hooks-policy-design.md
```

Expected: branch `worktree-m14-hooks-policy`; spec file exists.

- [ ] **Step 0.2: Baseline test suite + smokes pass**

```bash
go test ./... -count=1 2>&1 | grep -E "^FAIL" | head
go vet ./...
bash scripts/m13.5-lfs-quota-smoke.sh 2>&1 | tail -2
```

Expected: zero FAILs (or only the documented importer/gitcli flake; re-run alone to confirm); vet clean; smoke ends `M13.5_LFS_QUOTA_SMOKE_OK`.

---

### Task 1: Migration 0005 + Service core (Add/List/Remove)

**Files:**
- Create: `internal/auth/sqlitestore/migrations/0005_protected_refs.sql`
- Create: `internal/policy/policy.go`
- Create: `internal/policy/policy_test.go`
- Create: `internal/policy/doc.go`
- Modify: `internal/auth/sqlitestore/store_test.go` (bump schema_version assertion 4→5)

- [ ] **Step 1.1: Write the migration**

Create `internal/auth/sqlitestore/migrations/0005_protected_refs.sql`:

```sql
CREATE TABLE protected_refs (
    tenant            TEXT NOT NULL,
    repo              TEXT NOT NULL,
    refname_pattern   TEXT NOT NULL,
    block_deletion    INTEGER NOT NULL DEFAULT 1 CHECK (block_deletion IN (0,1)),
    block_force_push  INTEGER NOT NULL DEFAULT 1 CHECK (block_force_push IN (0,1)),
    created_at        INTEGER NOT NULL,
    PRIMARY KEY (tenant, repo, refname_pattern),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);

INSERT INTO schema_version (version, applied_at) VALUES (5, strftime('%s','now'));
```

- [ ] **Step 1.2: Bump the schema_version assertion in store_test.go**

Locate the existing assertion (it currently asserts `MAX(version) == 4` from M13.5's bump) and change to 5:

```bash
grep -n "MAX(version)" internal/auth/sqlitestore/store_test.go
```

Edit that line: replace `4` with `5`. The standard pattern for any migration-adding change.

- [ ] **Step 1.3: Verify migrations apply cleanly**

```bash
cd /home/eran/work/bucketvcs/.claude/worktrees/m14-hooks-policy
go test ./internal/auth/sqlitestore/... -count=1 -v 2>&1 | tail -15
```

Expected: existing migration tests pass with the bumped assertion; the new 0005 migration is picked up by the embedded fs loader.

- [ ] **Step 1.4: Write the package doc**

Create `internal/policy/doc.go`:

```go
// Package policy implements per-repo protected-ref rules backed by
// the M4 authdb sqlite. See docs/superpowers/specs/2026-05-21-m14-hooks-policy-design.md
// for the design.
//
// Tier 1 of the §23 hooks-and-policy roadmap: ref-level rules only
// (block deletion, block force-push). Tier 2 (file size, path
// restrictions, author/email rules, commit-message regex) and
// Tier 3 (external hooks / webhooks) are explicitly deferred.
//
// The package is fully opt-in: callers that don't wire a Service
// into receivepack.EngineRequest.Policy see exactly pre-M14
// behaviour (no enforcement, no metric emissions).
package policy
```

- [ ] **Step 1.5: Write the failing unit tests for Add/List/Remove**

Create `internal/policy/policy_test.go`:

```go
package policy_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/policy"
)

// openTestDB returns a fresh on-disk authdb with migrations applied,
// pre-seeded with a (tenant, repo) row so the FK on protected_refs
// is satisfiable. Mirrors the M13.5 quota tests' shape.
func openTestDB(t *testing.T, tenant, repo string) *sql.DB {
	t.Helper()
	tmp := t.TempDir()
	store, err := sqlitestore.Open(filepath.Join(tmp, "auth.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	if _, err := db.Exec(
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		tenant, repo,
	); err != nil {
		t.Fatalf("seed repo row: %v", err)
	}
	return db
}

func TestService_AddListRemove(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	ctx := context.Background()

	// Initial: empty list.
	got, err := svc.List(ctx, "acme", "site")
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List empty: len=%d, want 0", len(got))
	}

	// Add two rules.
	if err := svc.Add(ctx, policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	}); err != nil {
		t.Fatalf("Add main: %v", err)
	}
	if err := svc.Add(ctx, policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/release/*",
		BlockDeletion:  true, BlockForcePush: false,
	}); err != nil {
		t.Fatalf("Add release: %v", err)
	}

	// List returns both, ordered by pattern.
	got, err = svc.List(ctx, "acme", "site")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List: len=%d, want 2; got=%+v", len(got), got)
	}
	if got[0].RefnamePattern != "refs/heads/main" {
		t.Errorf("List[0].Pattern=%q, want refs/heads/main", got[0].RefnamePattern)
	}
	if !got[0].BlockDeletion || !got[0].BlockForcePush {
		t.Errorf("List[0] toggles=%v/%v, want true/true", got[0].BlockDeletion, got[0].BlockForcePush)
	}
	if got[1].RefnamePattern != "refs/heads/release/*" {
		t.Errorf("List[1].Pattern=%q, want refs/heads/release/*", got[1].RefnamePattern)
	}
	if got[1].BlockForcePush {
		t.Errorf("List[1].BlockForcePush=true, want false")
	}
	// CreatedAt populated.
	if got[0].CreatedAt.IsZero() {
		t.Errorf("List[0].CreatedAt is zero")
	}

	// Remove one rule.
	if err := svc.Remove(ctx, "acme", "site", "refs/heads/main"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, _ = svc.List(ctx, "acme", "site")
	if len(got) != 1 || got[0].RefnamePattern != "refs/heads/release/*" {
		t.Errorf("after Remove: %+v, want only release rule", got)
	}

	// Remove non-existent pattern is a no-op (no error).
	if err := svc.Remove(ctx, "acme", "site", "refs/heads/nonexistent"); err != nil {
		t.Errorf("Remove non-existent: %v, want nil", err)
	}

	// List for unknown repo returns empty, not error.
	got, err = svc.List(ctx, "no-such", "repo")
	if err != nil {
		t.Errorf("List unknown repo: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List unknown repo: len=%d, want 0", len(got))
	}

	// Suppress unused-import warning when only timing.Time is used in helper.
	_ = time.Time{}
}

func TestService_AddIsIdempotent(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	ctx := context.Background()
	ref := policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	}
	if err := svc.Add(ctx, ref); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	// Re-Add with different toggles → updates the existing row.
	ref.BlockForcePush = false
	if err := svc.Add(ctx, ref); err != nil {
		t.Fatalf("Add update: %v", err)
	}
	got, _ := svc.List(ctx, "acme", "site")
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (update, not insert)", len(got))
	}
	if got[0].BlockForcePush {
		t.Errorf("after Add update: BlockForcePush=true, want false")
	}
}

func TestService_AddRejectsMalformedPattern(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	// `[` opens a character class that never closes; path.Match
	// returns ErrBadPattern.
	ref := policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/[broken",
		BlockDeletion:  true, BlockForcePush: true,
	}
	if err := svc.Add(context.Background(), ref); err == nil {
		t.Errorf("Add malformed pattern returned nil; want error")
	}
}

func TestService_AddRejectsEmptyPattern(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	ref := policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "",
		BlockDeletion:  true, BlockForcePush: true,
	}
	if err := svc.Add(context.Background(), ref); err == nil {
		t.Errorf("Add empty pattern returned nil; want error")
	}
}
```

- [ ] **Step 1.6: Confirm tests fail (`package not defined`)**

```bash
go test ./internal/policy/... -count=1 2>&1 | tail -10
```

Expected: build error `package policy: no Go files` (or similar).

- [ ] **Step 1.7: Implement the Service core**

Create `internal/policy/policy.go`:

```go
package policy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"time"
)

// ProtectedRef is one row in the protected_refs table.
type ProtectedRef struct {
	Tenant         string
	Repo           string
	RefnamePattern string
	BlockDeletion  bool
	BlockForcePush bool
	CreatedAt      time.Time
}

// Service wraps the protected_refs table on the authdb. All methods
// are safe for concurrent use; sqlite's single-writer model serializes
// writes.
type Service struct {
	db     *sql.DB
	logger *slog.Logger
}

// New constructs a Service. logger may be nil; emissions added in
// later tasks will fall back to slog.Default() at emission time.
func New(db *sql.DB, logger *slog.Logger) *Service {
	return &Service{db: db, logger: logger}
}

// Add creates or updates a protected-ref rule. Validates the glob
// pattern via path.Match before INSERT — malformed patterns reject
// at Add time so they can't silently break receive-pack later.
func (s *Service) Add(ctx context.Context, r ProtectedRef) error {
	if r.RefnamePattern == "" {
		return fmt.Errorf("policy: refname_pattern must not be empty")
	}
	if _, err := path.Match(r.RefnamePattern, ""); err != nil {
		return fmt.Errorf("policy: invalid refname_pattern %q: %w", r.RefnamePattern, err)
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO protected_refs
			(tenant, repo, refname_pattern, block_deletion, block_force_push, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant, repo, refname_pattern) DO UPDATE SET
			block_deletion   = excluded.block_deletion,
			block_force_push = excluded.block_force_push
	`, r.Tenant, r.Repo, r.RefnamePattern, boolToInt(r.BlockDeletion), boolToInt(r.BlockForcePush), now)
	if err != nil {
		return fmt.Errorf("policy add %q/%q %q: %w", r.Tenant, r.Repo, r.RefnamePattern, err)
	}
	return nil
}

// List returns every rule for (tenant, repo) ordered by pattern.
func (s *Service) List(ctx context.Context, tenant, repo string) ([]ProtectedRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tenant, repo, refname_pattern, block_deletion, block_force_push, created_at
		FROM protected_refs
		WHERE tenant = ? AND repo = ?
		ORDER BY refname_pattern
	`, tenant, repo)
	if err != nil {
		return nil, fmt.Errorf("policy list %q/%q: %w", tenant, repo, err)
	}
	defer rows.Close()
	var out []ProtectedRef
	for rows.Next() {
		var (
			r              ProtectedRef
			blockDel       int
			blockFP        int
			createdAt      int64
		)
		if err := rows.Scan(&r.Tenant, &r.Repo, &r.RefnamePattern, &blockDel, &blockFP, &createdAt); err != nil {
			return nil, fmt.Errorf("policy list scan: %w", err)
		}
		r.BlockDeletion = blockDel != 0
		r.BlockForcePush = blockFP != 0
		r.CreatedAt = time.Unix(createdAt, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// Remove deletes the rule whose pattern matches exactly (no glob
// expansion of the pattern itself). Removing a non-existent pattern
// is a no-op.
func (s *Service) Remove(ctx context.Context, tenant, repo, pattern string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM protected_refs WHERE tenant = ? AND repo = ? AND refname_pattern = ?`,
		tenant, repo, pattern,
	)
	if err != nil {
		return fmt.Errorf("policy remove %q/%q %q: %w", tenant, repo, pattern, err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ErrNotFound is reserved for future callers that need to distinguish
// "no rule" from "no rows". Currently unused but exported so the API
// shape is stable.
var ErrNotFound = errors.New("policy: not found")
```

- [ ] **Step 1.8: Run tests to verify pass**

```bash
go test ./internal/policy/... -count=1 -v 2>&1 | tail -30
```

Expected: 4 tests PASS (TestService_AddListRemove, TestService_AddIsIdempotent, TestService_AddRejectsMalformedPattern, TestService_AddRejectsEmptyPattern).

- [ ] **Step 1.9: go vet + build**

```bash
go vet ./internal/policy/... ./internal/auth/sqlitestore/...
go build ./...
```

Expected: clean.

- [ ] **Step 1.10: Commit**

```bash
git add internal/auth/sqlitestore/migrations/0005_protected_refs.sql \
        internal/auth/sqlitestore/store_test.go \
        internal/policy/
git commit -m "policy: Service core + 0005 protected_refs migration (M14 Task 1)"
```

---

### Task 2: CheckUpdate + force-push detection

**Files:**
- Modify: `internal/policy/policy.go` (add CheckUpdate + PolicyError + git merge-base helper)
- Create: `internal/policy/policy_check_test.go`

- [ ] **Step 2.1: Write failing tests for CheckUpdate**

Create `internal/policy/policy_check_test.go`:

```go
package policy_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/policy"
)

const nullOID = "0000000000000000000000000000000000000000"

// makeBareWithFFChain creates a bare repo with two commits on
// refs/heads/main where commit2 is a descendant of commit1.
// Returns (bareDir, commit1OID, commit2OID).
//
// The bare is created via `git init --bare`, with the commit chain
// generated in a sibling working dir and pushed. This gives us real
// OIDs and a graph that `git merge-base --is-ancestor` can verify.
func makeBareWithFFChain(t *testing.T) (string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	work := filepath.Join(tmp, "work")

	mustRun := func(dir, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v in %s failed: %v\n%s", name, args, dir, err, out)
		}
		return string(out)
	}

	mustRun(tmp, "git", "init", "--bare", bare)
	mustRun(tmp, "git", "init", "-q", "-b", "main", work)
	mustRun(work, "git", "config", "user.email", "t@example.com")
	mustRun(work, "git", "config", "user.name", "t")

	mustRun(work, "git", "commit", "--allow-empty", "-qm", "c1")
	commit1 := chomp(mustRun(work, "git", "rev-parse", "HEAD"))
	mustRun(work, "git", "commit", "--allow-empty", "-qm", "c2")
	commit2 := chomp(mustRun(work, "git", "rev-parse", "HEAD"))

	mustRun(work, "git", "push", "-q", bare, "main:refs/heads/main")
	return bare, commit1, commit2
}

func chomp(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

func TestCheckUpdate_NoRulesIsAccept(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	bare, c1, c2 := makeBareWithFFChain(t)
	// No rules in DB → any update accepts.
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c1, c2); err != nil {
		t.Errorf("CheckUpdate with no rules: %v, want nil", err)
	}
}

func TestCheckUpdate_NonMatchingPatternIsAccept(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/release/*",
		BlockDeletion:  true, BlockForcePush: true,
	})
	// refs/heads/main does NOT match refs/heads/release/* → accept.
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c1, c2); err != nil {
		t.Errorf("CheckUpdate non-match: %v, want nil", err)
	}
}

func TestCheckUpdate_DeletionBlocked(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	bare, c1, _ := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	})
	err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c1, nullOID)
	var perr *policy.PolicyError
	if !errors.As(err, &perr) {
		t.Fatalf("CheckUpdate deletion: %T %v, want *PolicyError", err, err)
	}
	if perr.Reason != "deletion blocked" {
		t.Errorf("Reason=%q, want 'deletion blocked'", perr.Reason)
	}
	if perr.MatchedPattern != "refs/heads/main" {
		t.Errorf("MatchedPattern=%q", perr.MatchedPattern)
	}
}

func TestCheckUpdate_DeletionAllowedWhenToggleOff(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	bare, c1, _ := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  false, BlockForcePush: true,
	})
	// Toggle off → deletion accepted.
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c1, nullOID); err != nil {
		t.Errorf("CheckUpdate deletion with toggle off: %v, want nil", err)
	}
}

func TestCheckUpdate_FastForwardAccepted(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	})
	// c1 → c2 is fast-forward (c2 is descendant of c1) → accept.
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c1, c2); err != nil {
		t.Errorf("CheckUpdate FF: %v, want nil", err)
	}
}

func TestCheckUpdate_NonFFRejected(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	})
	// c2 → c1 is non-FF (c1 is NOT a descendant of c2) → reject.
	err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c2, c1)
	var perr *policy.PolicyError
	if !errors.As(err, &perr) {
		t.Fatalf("CheckUpdate non-FF: %T %v, want *PolicyError", err, err)
	}
	if perr.Reason != "non-fast-forward push blocked" {
		t.Errorf("Reason=%q, want 'non-fast-forward push blocked'", perr.Reason)
	}
}

func TestCheckUpdate_NonFFAllowedWhenToggleOff(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: false,
	})
	// Toggle off → non-FF accepted.
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c2, c1); err != nil {
		t.Errorf("CheckUpdate non-FF with toggle off: %v, want nil", err)
	}
}

func TestCheckUpdate_NewRefCreationAccepted(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	bare, _, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	})
	// Old-OID = nullOID is new ref creation → not a deletion, not a
	// non-FF; accept (Tier 1 doesn't have a block_create toggle).
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", nullOID, c2); err != nil {
		t.Errorf("CheckUpdate new ref creation: %v, want nil", err)
	}
}

func TestCheckUpdate_GlobMatchesMultiple(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/release/*",
		BlockDeletion:  true, BlockForcePush: true,
	})
	// refs/heads/release/v1 matches; non-FF → reject.
	err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/release/v1", c2, c1)
	var perr *policy.PolicyError
	if !errors.As(err, &perr) {
		t.Errorf("CheckUpdate glob match non-FF: want *PolicyError; got %T %v", err, err)
	}
	if perr != nil && perr.MatchedPattern != "refs/heads/release/*" {
		t.Errorf("MatchedPattern=%q, want refs/heads/release/*", perr.MatchedPattern)
	}
}

func TestCheckUpdate_GlobDoesNotCrossSlash(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/*",
		BlockDeletion:  true, BlockForcePush: true,
	})
	// refs/heads/release/v1 does NOT match refs/heads/* (the * doesn't
	// cross the /). Non-FF should succeed.
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/release/v1", c2, c1); err != nil {
		t.Errorf("CheckUpdate non-cross-slash: %v, want nil", err)
	}
}

func TestCheckUpdate_FirstMatchingRejectionWins(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db, nil)
	bare, c1, c2 := makeBareWithFFChain(t)
	// Two rules: the specific one allows force-push, the general one
	// blocks it. ORDER BY pattern => the specific rule comes first
	// alphabetically (refs/heads/main < refs/heads/*). The general
	// rule (force-push blocked) should reject.
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  false, BlockForcePush: false,
	})
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/*",
		BlockDeletion:  true, BlockForcePush: true,
	})
	// Non-FF to refs/heads/main → ANY blocking rule rejects.
	err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c2, c1)
	var perr *policy.PolicyError
	if !errors.As(err, &perr) {
		t.Errorf("CheckUpdate any-rule-blocks: want *PolicyError; got %T %v", err, err)
	}
}
```

- [ ] **Step 2.2: Confirm tests fail (`CheckUpdate undefined`)**

```bash
go test ./internal/policy/... -count=1 -run CheckUpdate 2>&1 | tail -10
```

Expected: build error mentioning `svc.CheckUpdate undefined` and `policy.PolicyError undefined`.

- [ ] **Step 2.3: Add CheckUpdate + PolicyError + git helper to policy.go**

Append to `internal/policy/policy.go` (after the existing methods, before the `ErrNotFound` var declaration is fine):

```go
// PolicyError is returned by CheckUpdate when a ref update is rejected.
// Callers (receive-pack step 8b) use errors.As to recover the structured
// fields for the `ng <refname> protected-branch: <reason>` report-status
// line and for the policy.ref.rejected audit event.
type PolicyError struct {
	Refname        string
	MatchedPattern string
	Reason         string // "deletion blocked" | "non-fast-forward push blocked"
	OldOID         string
	NewOID         string
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("protected-branch: %s by pattern %s (refname=%s)",
		e.Reason, e.MatchedPattern, e.Refname)
}

// MetricOutcome returns the value used as the {outcome} label on
// policy_refs_check_total when this error is the cause of rejection.
// Stable across rule changes — operators rely on this for alerts.
func (e *PolicyError) MetricOutcome() string {
	switch e.Reason {
	case "deletion blocked":
		return "blocked_deletion"
	case "non-fast-forward push blocked":
		return "blocked_force_push"
	default:
		return "blocked_other"
	}
}

// CheckUpdate runs all matching rules against one ref update.
// bareDir is the local bare repository directory used for fast-forward
// detection via `git merge-base --is-ancestor`. oldOID and newOID use
// the receivepack convention: a 40-zero hex string ("0000...") means
// "absent" (new ref creation when in oldOID; ref deletion when in
// newOID). Returns *PolicyError on rejection, or nil to accept. ANY
// matching rule that blocks the operation triggers rejection.
//
// On non-policy errors (sqlite read failure, git subprocess failure),
// returns the underlying error wrapped — caller surfaces these as
// `internal-error` rather than `protected-branch` status lines.
func (s *Service) CheckUpdate(ctx context.Context, tenant, repo, bareDir string,
	refname, oldOID, newOID string) error {

	rules, err := s.List(ctx, tenant, repo)
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		return nil
	}

	const nullHex = "0000000000000000000000000000000000000000"
	isDeletion := newOID == nullHex
	isCreation := oldOID == nullHex
	isUpdate := !isDeletion && !isCreation

	for _, r := range rules {
		matched, merr := path.Match(r.RefnamePattern, refname)
		if merr != nil {
			// Malformed pattern at lookup time (the Add-time guard
			// should have caught it, but a future direct-SQL edit
			// might bypass that). Treat as internal error.
			return fmt.Errorf("policy: pattern %q invalid: %w", r.RefnamePattern, merr)
		}
		if !matched {
			continue
		}
		if isDeletion && r.BlockDeletion {
			return &PolicyError{
				Refname: refname, MatchedPattern: r.RefnamePattern,
				Reason: "deletion blocked",
				OldOID: oldOID, NewOID: newOID,
			}
		}
		if isUpdate && r.BlockForcePush {
			isFF, err := isFastForward(ctx, bareDir, oldOID, newOID)
			if err != nil {
				return fmt.Errorf("policy: merge-base check for %s: %w", refname, err)
			}
			if !isFF {
				return &PolicyError{
					Refname: refname, MatchedPattern: r.RefnamePattern,
					Reason: "non-fast-forward push blocked",
					OldOID: oldOID, NewOID: newOID,
				}
			}
		}
		// New-ref creation is never rejected by Tier 1 rules.
	}
	return nil
}

// isFastForward reports whether oldOID is an ancestor of newOID in
// the local bare. Calls `git merge-base --is-ancestor <old> <new>`:
//
//	exit 0 -> ancestor (fast-forward; ok to update)
//	exit 1 -> not ancestor (non-FF; reject)
//	exit 2 or other -> error (corrupt bare, missing OID, etc.)
func isFastForward(ctx context.Context, bareDir, oldOID, newOID string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "--no-replace-objects", "-C", bareDir,
		"merge-base", "--is-ancestor", oldOID, newOID)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		switch ee.ExitCode() {
		case 1:
			return false, nil
		default:
			return false, fmt.Errorf("merge-base --is-ancestor exit=%d: %s",
				ee.ExitCode(), stderr.String())
		}
	}
	return false, fmt.Errorf("merge-base --is-ancestor: %w (stderr: %s)", err, stderr.String())
}
```

Add the necessary imports (replace the existing import block):

```go
import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path"
	"time"
)
```

- [ ] **Step 2.4: Run tests to verify pass**

```bash
go test ./internal/policy/... -count=1 -v -run CheckUpdate 2>&1 | tail -30
```

Expected: 10 CheckUpdate tests PASS (no-rules-is-accept, non-matching-accept, deletion-blocked, deletion-allowed-when-toggle-off, FF-accepted, non-FF-rejected, non-FF-allowed-when-toggle-off, new-ref-creation-accepted, glob-matches-multiple, glob-does-not-cross-slash, first-matching-rejection-wins).

- [ ] **Step 2.5: Full policy package suite + vet + build**

```bash
go test ./internal/policy/... -count=1 2>&1 | tail -5
go vet ./...
go build ./...
```

Expected: all 14 tests pass (4 from Task 1 + 10 new); vet clean; build clean.

- [ ] **Step 2.6: Commit**

```bash
git add internal/policy/
git commit -m "policy: CheckUpdate + PolicyError + git merge-base force-push detection (M14 Task 2)"
```

---

### Task 3: Wire into receive-pack (step 8b + EngineRequest field)

**Files:**
- Modify: `internal/gitproto/receivepack/engine.go` (add Policy field to EngineRequest)
- Modify: `internal/gitproto/receivepack/complete.go` (add step 8b)
- Modify (or create): `internal/gitproto/receivepack/policy_test.go` (integration test)

- [ ] **Step 3.1: Inspect existing EngineRequest + completeReceivePack shape**

```bash
cd /home/eran/work/bucketvcs/.claude/worktrees/m14-hooks-policy
grep -n "type EngineRequest" internal/gitproto/receivepack/engine.go
grep -n "Step 5\|Step 6\|Step 7\|Step 8\|Step 9\|precheckUpdates" internal/gitproto/receivepack/complete.go
```

You should see EngineRequest carries Ctx/Tenant/Repo/Actor/Stdin/Stdout/Stderr/ProtocolVersion/AgentVersion/Store/Mirror. completeReceivePack should have annotations for Step 5 (precheck), Step 7 (IndexPackStrict), Step 8 (connectivity), Step 9 (build refUpdates).

- [ ] **Step 3.2: Add Policy field to EngineRequest**

Edit `internal/gitproto/receivepack/engine.go`. Add to the EngineRequest struct (after Mirror):

```go
	// Policy is OPTIONAL. When non-nil, completeReceivePack runs
	// step 8b (M14 protected-ref enforcement) before BuildAndCommit.
	// nil means no enforcement (pre-M14 behavior). Type is
	// *policy.Service from internal/policy.
	Policy *policy.Service
```

Add the import:

```go
	"github.com/bucketvcs/bucketvcs/internal/policy"
```

- [ ] **Step 3.3: Add Step 8b to completeReceivePack**

Open `internal/gitproto/receivepack/complete.go`. Find the existing "Step 8: connectivity" block (around line 158-196) that ends with the second `if rp.PackPath == "" && len(out) > 0 { ... }` check. After that block but BEFORE "Step 9: build the refUpdates map" (around line 198), insert step 8b:

```go
	// Step 8b: M14 policy enforcement. For each accepted update,
	// walk the repo's protected_refs rules; reject the update if any
	// matching rule blocks the operation. Opt-in via eng.Policy=nil.
	if eng.Policy != nil {
		for i, u := range rp.Updates {
			if statuses[i] != "" {
				continue
			}
			err := eng.Policy.CheckUpdate(ctx, tenant, repoID,
				m.BareDir(), u.Refname, u.OldOID, u.NewOID)
			if err == nil {
				policy.EmitRefCheckMetric(ctx, eng.Logger(), "ok")
				continue
			}
			var perr *policy.PolicyError
			if errors.As(err, &perr) {
				statuses[i] = "ng " + u.Refname + " " + perr.Error()
				policy.EmitRefCheckMetric(ctx, eng.Logger(), perr.MetricOutcome())
				policy.EmitRefRejected(ctx, eng.Logger(), tenant, repoID, perr, actorNameFromEng(eng))
				continue
			}
			// Non-policy error from CheckUpdate (sqlite read failure,
			// git subprocess failure). Failing closed: a policy lookup
			// failure CANNOT silently allow a write to a protected ref.
			statuses[i] = "ng " + u.Refname + " internal-error: " + err.Error()
			policy.EmitRefCheckMetric(ctx, eng.Logger(), "internal_error")
		}
		// Atomic-batch poisoning: if any policy rejection landed and
		// the client requested atomic mode, mark every empty status
		// as atomic-batch-failed. Mirrors the existing precheck atomic
		// handling at line ~120.
		if rp.IsAtomic && anyStatusNonEmpty(statuses) && !allStatusesNonEmpty(statuses) {
			for i, u := range rp.Updates {
				if statuses[i] == "" {
					statuses[i] = "ng " + u.Refname + " atomic-batch-failed"
				}
			}
			writeReceiveReport(w, "ok", statuses, rp.Caps)
			return
		}
		// If everything failed (e.g., the only ref was rejected),
		// short-circuit with the report — nothing to commit.
		if allStatusesNonEmpty(statuses) {
			writeReceiveReport(w, "ok", statuses, rp.Caps)
			return
		}
	}
```

The helpers `anyStatusNonEmpty` and `allStatusesNonEmpty` go at the bottom of `complete.go`:

```go
func anyStatusNonEmpty(statuses []string) bool {
	for _, s := range statuses {
		if s != "" {
			return true
		}
	}
	return false
}

func allStatusesNonEmpty(statuses []string) bool {
	for _, s := range statuses {
		if s == "" {
			return false
		}
	}
	return true
}
```

If `actorNameFromEng` doesn't already exist in this file, add it (search first):

```bash
grep -n "actorNameFromEng\|actorName :=" internal/gitproto/receivepack/complete.go
```

If you only see the local `actorName := "anonymous"` block (around line 263), factor it into a helper:

```go
func actorNameFromEng(eng *EngineRequest) string {
	if a := eng.Actor; a != nil {
		switch {
		case a.Name != "":
			return a.Name
		case a.UserID != "":
			return a.UserID
		}
	}
	return "anonymous"
}
```

And replace the existing inline `actorName := "anonymous" ... switch` block at line 263 with `actorName := actorNameFromEng(eng)`.

Also confirm `eng.Logger()` exists. If EngineRequest doesn't expose a Logger accessor, look for an existing per-request logger and adapt:

```bash
grep -n "Logger\|slog\." internal/gitproto/receivepack/engine.go internal/gitproto/receivepack/complete.go | head
```

If there's no built-in logger on EngineRequest, add one:

```go
	// Logger is OPTIONAL. nil falls back to slog.Default() at emission time.
	Logger *slog.Logger
```

and define a small accessor at the bottom of engine.go:

```go
import "log/slog"

func (e *EngineRequest) loggerOrDefault() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}
```

Then `policy.EmitRefCheckMetric(ctx, eng.loggerOrDefault(), ...)`.

Add `"errors"` to the imports of complete.go if not already present.

- [ ] **Step 3.4: Stub the policy emitter functions used by step 8b**

These will be fully fleshed out in Task 5 with proper metric/audit emission. For now, add minimal no-op stubs to `internal/policy/policy.go` so the receive-pack code compiles:

```go
// EmitRefCheckMetric is a no-op stub; Task 5 wires it to the
// real slog-based metric emission.
func EmitRefCheckMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	// Intentional no-op until Task 5.
}

// EmitRefRejected is a no-op stub; Task 5 wires it to the real
// slog-based audit emission.
func EmitRefRejected(ctx context.Context, logger *slog.Logger, tenant, repo string, perr *PolicyError, actor string) {
	// Intentional no-op until Task 5.
}
```

These let Task 3 land an integration test that exercises the dispatch logic without coupling to the observability machinery — Task 5 swaps in real bodies without re-touching the receive-pack wiring.

- [ ] **Step 3.5: Write a failing integration test for step 8b**

Create `internal/gitproto/receivepack/policy_test.go`:

```go
package receivepack

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/policy"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// TestStep8b_BlocksDeletion exercises the receive-pack step 8b for a
// branch-deletion update against a repo with a matching protected_refs
// rule. The full receive-pack request is synthesized using the existing
// helpers in this package. Assertion is on the per-ref `ng` status
// returned in the report.
func TestStep8b_BlocksDeletion(t *testing.T) {
	tmp := t.TempDir()
	storeURL := "localfs:" + filepath.Join(tmp, "store")
	authDB := filepath.Join(tmp, "auth.db")

	// Init the storage + create the repo.
	store, err := localfs.Open(filepath.Join(tmp, "store"))
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	r, err := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	_ = r
	_ = storeURL

	// Open the authdb + add a protected_refs rule.
	authStore, err := sqlitestore.Open(authDB)
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	defer authStore.Close()
	// Seed the repos row so the FK on protected_refs is satisfied.
	if _, err := authStore.DB().Exec(
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		"acme", "site",
	); err != nil {
		t.Fatalf("seed repos: %v", err)
	}
	pol := policy.New(authStore.DB(), nil)
	if err := pol.Add(ctx, policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	}); err != nil {
		t.Fatalf("policy.Add: %v", err)
	}

	// The remainder of this test (synthesizing a real receive-pack
	// request body, calling Service, asserting on the report's
	// `ng refs/heads/main protected-branch: deletion blocked ...`
	// line) requires the existing test scaffolding in this package.
	// Inspect the sibling test files for the receive-pack-request
	// builder pattern:
	//
	//   grep -rn "func Test.*ReceivePack\|writeUpdates\|completeReceivePack" \
	//       internal/gitproto/receivepack/*_test.go | head
	//
	// Use that builder to craft a delete-refs/heads/main update with
	// the bare repo's current main OID as old-oid and the null OID
	// as new-oid, then call completeReceivePack directly (it's
	// package-internal) with eng.Policy = pol. Capture stdout in a
	// bytes.Buffer; assert the report contains:
	//   ng refs/heads/main protected-branch: deletion blocked
	//
	// If no such builder exists, the simplest path is to add a
	// test-only constructor in this file that wires a Mirror, the
	// EngineRequest, and a single update command directly.

	_ = bytes.Buffer{}
}

// Note: a happy-path companion (FF update to a protected ref accepts)
// is intentionally NOT in this file. The smoke (Task 5.5) exercises
// the full happy path end-to-end via a real `bucketvcs serve` + `git
// push`, which is a stronger signal than a unit test of step 8b's
// internal dispatch. If the implementer chose option (b) or (c) above
// (skipping the heavy integration test entirely), the smoke is the
// only end-to-end coverage; that's acceptable for Tier 1.
```

The test bodies above intentionally point at "use the existing receive-pack test scaffolding." If a builder helper doesn't exist (likely, since the package's tests are fairly low-level), you'll need to either:

(a) build one up out of pktline helpers + `parseReceivePackRequest`, OR
(b) defer the integration test to a higher level (test the whole gateway request/response cycle in `internal/gateway/`), OR
(c) ship a smaller unit test in `internal/policy/` that asserts CheckUpdate's behavior is correctly routed via step 8b's logic, plus a smoke test in Task 5 that exercises the full path end-to-end.

For Task 3's commit, **option (c)** is acceptable: rely on the smoke (Task 5) for end-to-end coverage and write a lighter integration test here that just verifies the wiring (eng.Policy=nil → no enforcement; eng.Policy=non-nil → CheckUpdate called) using mocks.

Write the lighter test if the heavy one is intractable. Either way, the implementer reports which path was taken.

- [ ] **Step 3.6: Run tests + vet + build**

```bash
go test ./internal/gitproto/receivepack/... -count=1 -v -run Step8b 2>&1 | tail -20
go test ./internal/policy/... -count=1 2>&1 | tail -5
go test ./... -count=1 2>&1 | grep -E "^FAIL" | head
go vet ./...
go build ./...
```

Expected: TestStep8b_* passes (or is documented-skipped); rest of the suite green; vet + build clean.

- [ ] **Step 3.7: Commit**

```bash
git add internal/gitproto/receivepack/engine.go \
        internal/gitproto/receivepack/complete.go \
        internal/gitproto/receivepack/policy_test.go \
        internal/policy/policy.go
git commit -m "receivepack: wire policy.Service via step 8b (M14 Task 3)"
```

---

### Task 4: CLI (`bucketvcs policy refs add/list/remove`)

**Files:**
- Create: `cmd/bucketvcs/policy.go`
- Create: `cmd/bucketvcs/policy_test.go`
- Modify: `cmd/bucketvcs/main.go` (route `policy` to `runPolicy`)

- [ ] **Step 4.1: Inspect existing CLI dispatch shape**

```bash
grep -n "case \"quota\"\|case \"gc\"\|case \"user\":" cmd/bucketvcs/main.go
```

The convention is `case "X": return runX(ctx, rest, stdout, stderr)`. Match that exactly.

- [ ] **Step 4.2: Add the dispatch line + update help text**

Edit `cmd/bucketvcs/main.go`:

```go
	case "policy":
		return runPolicy(ctx, rest, stdout, stderr)
```

If main.go has a usage list (e.g., the top-of-file string listing subcommands), append `policy` to it alphabetically.

- [ ] **Step 4.3: Write the failing CLI tests**

Create `cmd/bucketvcs/policy_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// tempAuthDBWithRepo returns a fresh authdb path with a (tenant, repo)
// row pre-seeded so the protected_refs FK is satisfiable.
func tempAuthDBWithRepo(t *testing.T, tenant, repo string) string {
	t.Helper()
	authDB := filepath.Join(t.TempDir(), "auth.db")
	// Open + migrate the authdb, then insert the repos row.
	store, err := sqlitestore.Open(authDB)
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		tenant, repo,
	); err != nil {
		t.Fatalf("seed repos: %v", err)
	}
	return authDB
}

func TestPolicy_CLI_HelpExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPolicy(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--help exit=%d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "refs") {
		t.Errorf("--help missing 'refs' subcommand; got: %s", stdout.String())
	}
}

func TestPolicy_CLI_UnknownSubcommandIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPolicy(context.Background(), []string{"bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("unknown subcommand exit=%d, want 2", code)
	}
}

func TestPolicy_CLI_RefsAddListRemove_Roundtrip(t *testing.T) {
	authDB := tempAuthDBWithRepo(t, "acme", "site")

	// add: default flags → both toggles ON
	var s1, e1 bytes.Buffer
	if code := runPolicy(context.Background(),
		[]string{"refs", "add",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--pattern=refs/heads/main"},
		&s1, &e1); code != 0 {
		t.Fatalf("add: code=%d stderr=%s", code, e1.String())
	}

	// list: text format includes pattern and both toggles
	var s2, e2 bytes.Buffer
	if code := runPolicy(context.Background(),
		[]string{"refs", "list",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site"},
		&s2, &e2); code != 0 {
		t.Fatalf("list: code=%d stderr=%s", code, e2.String())
	}
	out := s2.String()
	for _, want := range []string{
		"pattern=refs/heads/main",
		"block_deletion=true",
		"block_force_push=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q in %s", want, out)
		}
	}

	// remove
	var s3, e3 bytes.Buffer
	if code := runPolicy(context.Background(),
		[]string{"refs", "remove",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--pattern=refs/heads/main"},
		&s3, &e3); code != 0 {
		t.Fatalf("remove: code=%d stderr=%s", code, e3.String())
	}

	// list now empty
	var s4, e4 bytes.Buffer
	_ = runPolicy(context.Background(),
		[]string{"refs", "list",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site"},
		&s4, &e4)
	if !strings.Contains(s4.String(), "no protected refs") {
		t.Errorf("list after remove missing 'no protected refs' hint; got: %s", s4.String())
	}
}

func TestPolicy_CLI_AddAllowFlagsLooseProtection(t *testing.T) {
	authDB := tempAuthDBWithRepo(t, "acme", "site")
	// With --allow-deletion --allow-force-push, the row has both
	// toggles = false.
	var stdout, stderr bytes.Buffer
	if code := runPolicy(context.Background(),
		[]string{"refs", "add",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--pattern=refs/heads/lab/*",
			"--allow-deletion", "--allow-force-push"},
		&stdout, &stderr); code != 0 {
		t.Fatalf("add: code=%d stderr=%s", code, stderr.String())
	}
	var lstdout, lstderr bytes.Buffer
	_ = runPolicy(context.Background(),
		[]string{"refs", "list",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site"},
		&lstdout, &lstderr)
	out := lstdout.String()
	for _, want := range []string{
		"block_deletion=false",
		"block_force_push=false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q; got: %s", want, out)
		}
	}
}

func TestPolicy_CLI_AddRejectsMalformedPattern(t *testing.T) {
	authDB := tempAuthDBWithRepo(t, "acme", "site")
	var stdout, stderr bytes.Buffer
	code := runPolicy(context.Background(),
		[]string{"refs", "add",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--pattern=refs/heads/[broken"},
		&stdout, &stderr)
	if code != 2 {
		t.Fatalf("add malformed: code=%d, want 2; stderr=%s", code, stderr.String())
	}
}

func TestPolicy_CLI_ListJSONFormat(t *testing.T) {
	authDB := tempAuthDBWithRepo(t, "acme", "site")
	_ = runPolicy(context.Background(),
		[]string{"refs", "add",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--pattern=refs/heads/main"},
		&bytes.Buffer{}, &bytes.Buffer{})

	var stdout, stderr bytes.Buffer
	if code := runPolicy(context.Background(),
		[]string{"refs", "list",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--format=json"},
		&stdout, &stderr); code != 0 {
		t.Fatalf("list json: code=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		`"tenant":"acme"`,
		`"repo":"site"`,
		`"pattern":"refs/heads/main"`,
		`"block_deletion":true`,
		`"block_force_push":true`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("json missing %q in: %s", want, out)
		}
	}
}
```

- [ ] **Step 4.4: Confirm tests fail (`runPolicy undefined`)**

```bash
go test ./cmd/bucketvcs/... -count=1 -run TestPolicy 2>&1 | tail -10
```

Expected: `undefined: runPolicy`.

- [ ] **Step 4.5: Implement runPolicy**

Create `cmd/bucketvcs/policy.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/policy"
)

const policyUsage = `Usage: bucketvcs policy <object> <action> [flags]

Objects + actions:
  refs add    --auth-db=<path> --tenant=<t> --repo=<r> --pattern=<glob>
              [--allow-deletion] [--allow-force-push]
  refs list   --auth-db=<path> --tenant=<t> --repo=<r> [--format=text|json]
  refs remove --auth-db=<path> --tenant=<t> --repo=<r> --pattern=<glob>

Defaults: a freshly-added rule blocks both deletion and force-push.
Pass --allow-deletion / --allow-force-push to loosen specific protections.

Patterns use stdlib path.Match globs: '*' matches one segment (does not
cross '/'); '?' matches one character; '[abc]' character classes.
Recursive '**' is NOT supported — add multiple rules for nested namespaces.

Exit codes:
  0  ok
  1  operational error (db unreachable, ...)
  2  usage error (bad flags, malformed pattern, ...)
`

func runPolicy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, policyUsage)
		return 2
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, policyUsage)
		return 0
	}
	switch args[0] {
	case "refs":
		return runPolicyRefs(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "policy: unknown object %q\n%s", args[0], policyUsage)
		return 2
	}
}

func runPolicyRefs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "policy refs: action required (add|list|remove)")
		return 2
	}
	switch args[0] {
	case "add":
		return runPolicyRefsAdd(ctx, args[1:], stdout, stderr)
	case "list":
		return runPolicyRefsList(ctx, args[1:], stdout, stderr)
	case "remove":
		return runPolicyRefsRemove(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "policy refs: unknown action %q (want add|list|remove)\n", args[0])
		return 2
	}
}

func runPolicyRefsAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy refs add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	pattern := fs.String("pattern", "", "Refname glob (required, e.g. refs/heads/main)")
	allowDel := fs.Bool("allow-deletion", false, "Allow deletion (default: block)")
	allowFP := fs.Bool("allow-force-push", false, "Allow force-push (default: block)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" || *pattern == "" {
		fmt.Fprintln(stderr, "policy refs add: --auth-db, --tenant, --repo, --pattern required")
		return 2
	}
	svc, store, err := openPolicySvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "policy refs add: %v\n", err)
		return 1
	}
	defer store.Close()
	err = svc.Add(ctx, policy.ProtectedRef{
		Tenant:         *tenant,
		Repo:           *repo,
		RefnamePattern: *pattern,
		BlockDeletion:  !*allowDel,
		BlockForcePush: !*allowFP,
	})
	if err != nil {
		// Distinguish usage (bad pattern) from operational (db) errors.
		if isUsageError(err) {
			fmt.Fprintf(stderr, "policy refs add: %v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "policy refs add: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant=%s  repo=%s  pattern=%s  block_deletion=%t  block_force_push=%t\n",
		*tenant, *repo, *pattern, !*allowDel, !*allowFP)
	return 0
}

func runPolicyRefsList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy refs list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	format := fs.String("format", "text", "Output format: text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" {
		fmt.Fprintln(stderr, "policy refs list: --auth-db, --tenant, --repo required")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "policy refs list: --format must be text|json (got %q)\n", *format)
		return 2
	}
	svc, store, err := openPolicySvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "policy refs list: %v\n", err)
		return 1
	}
	defer store.Close()
	rules, err := svc.List(ctx, *tenant, *repo)
	if err != nil {
		fmt.Fprintf(stderr, "policy refs list: %v\n", err)
		return 1
	}
	if len(rules) == 0 {
		if *format == "json" {
			fmt.Fprintln(stdout, "[]")
			return 0
		}
		fmt.Fprintf(stdout, "tenant=%s  repo=%s  (no protected refs)\n", *tenant, *repo)
		return 0
	}
	for _, r := range rules {
		if *format == "json" {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"tenant":           r.Tenant,
				"repo":             r.Repo,
				"pattern":          r.RefnamePattern,
				"block_deletion":   r.BlockDeletion,
				"block_force_push": r.BlockForcePush,
				"created_at":       r.CreatedAt.Format(time.RFC3339),
			})
			continue
		}
		fmt.Fprintf(stdout,
			"tenant=%s  repo=%s  pattern=%s  block_deletion=%t  block_force_push=%t  created=%s\n",
			r.Tenant, r.Repo, r.RefnamePattern, r.BlockDeletion, r.BlockForcePush,
			r.CreatedAt.Format(time.RFC3339))
	}
	return 0
}

func runPolicyRefsRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy refs remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	pattern := fs.String("pattern", "", "Refname glob to remove (required, exact match)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" || *pattern == "" {
		fmt.Fprintln(stderr, "policy refs remove: --auth-db, --tenant, --repo, --pattern required")
		return 2
	}
	svc, store, err := openPolicySvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "policy refs remove: %v\n", err)
		return 1
	}
	defer store.Close()
	if err := svc.Remove(ctx, *tenant, *repo, *pattern); err != nil {
		fmt.Fprintf(stderr, "policy refs remove: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant=%s  repo=%s  pattern=%s  removed\n", *tenant, *repo, *pattern)
	return 0
}

func openPolicySvc(path string) (*policy.Service, *sqlitestore.Store, error) {
	store, err := sqlitestore.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open authdb: %w", err)
	}
	return policy.New(store.DB(), nil), store, nil
}

// isUsageError reports whether the error from policy.Service.Add is
// due to a malformed user-supplied value (empty pattern, bad glob) vs
// an operational failure (sqlite). The current Service surfaces both
// kinds via fmt.Errorf with distinct prefixes; key off "must not be
// empty" or "invalid refname_pattern" substrings as a stable signal.
func isUsageError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{
		"must not be empty",
		"invalid refname_pattern",
	} {
		if containsString(msg, s) {
			return true
		}
	}
	return false
}

func containsString(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		(len(haystack) > len(needle) && (haystack[:len(needle)] == needle ||
			containsString(haystack[1:], needle))))
}
```

(The hand-rolled `containsString` exists only to avoid importing `strings` in this file twice — if you already import `strings`, just use `strings.Contains` and delete the helper.)

- [ ] **Step 4.6: Run tests + vet + build**

```bash
go test ./cmd/bucketvcs/... -count=1 -v -run TestPolicy 2>&1 | tail -25
go vet ./...
go build ./...
```

Expected: 6 TestPolicy tests pass; vet + build clean.

- [ ] **Step 4.7: Commit**

```bash
git add cmd/bucketvcs/policy.go cmd/bucketvcs/policy_test.go cmd/bucketvcs/main.go
git commit -m "cmd/policy: refs add/list/remove subcommands (M14 Task 4)"
```

---

### Task 5: Observability + smoke + operator guide + squash/tag/memory

**Files:**
- Create: `internal/policy/metrics.go` (real `EmitRefCheckMetric`)
- Create: `internal/policy/audit.go` (real `EmitRefRejected`)
- Modify: `internal/policy/policy.go` (delete the no-op stubs from Task 3.4)
- Create: `internal/policy/metrics_test.go`
- Create: `internal/policy/audit_test.go`
- Create: `scripts/m14-policy-smoke.sh`
- Create: `docs/m14-hooks-policy-operator-guide.md`

- [ ] **Step 5.1: Move EmitRefCheckMetric to metrics.go (real body)**

Create `internal/policy/metrics.go`:

```go
package policy

import (
	"context"
	"log/slog"
)

// emitMetric logs a structured metric in the same shape used by
// internal/lfs/metrics.go and internal/maintenance/log.go: an info-
// level "metric" record with attributes metric_name (string), value
// (int64), plus key/value pairs from kvs.
func emitMetric(ctx context.Context, logger *slog.Logger, name string, value int64, kvs ...any) {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []slog.Attr{
		slog.String("metric_name", name),
		slog.Int64("value", value),
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		k, ok := kvs[i].(string)
		if !ok {
			continue
		}
		attrs = append(attrs, slog.Any(k, kvs[i+1]))
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric", attrs...)
}

// EmitRefCheckMetric increments policy_refs_check_total{outcome}.
// Emitted once per Step 8b ref-update check.
// outcome is one of: "ok", "blocked_deletion", "blocked_force_push",
// "internal_error".
//
// Exported for cross-package use (receivepack calls it).
func EmitRefCheckMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	emitMetric(ctx, logger, "policy_refs_check_total", 1, "outcome", outcome)
}
```

Delete the no-op stub `EmitRefCheckMetric` from `internal/policy/policy.go` (Task 3.4 added it).

- [ ] **Step 5.2: Move EmitRefRejected to audit.go (real body)**

Create `internal/policy/audit.go`:

```go
package policy

import (
	"context"
	"log/slog"
)

// EmitRefRejected records a "policy.ref.rejected" audit event when
// step 8b blocks a ref update. Fires only on rejection — accepted
// pushes already emit the existing receive-pack audit.
//
// Attrs match design spec §7.2.
//
// Exported for cross-package use (receivepack calls it).
func EmitRefRejected(ctx context.Context, logger *slog.Logger, tenant, repo string, perr *PolicyError, actor string) {
	if logger == nil {
		logger = slog.Default()
	}
	if perr == nil {
		return
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "policy.ref.rejected",
		slog.String("event", "policy.ref.rejected"),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("refname", perr.Refname),
		slog.String("matched_pattern", perr.MatchedPattern),
		slog.String("reason", perr.Reason),
		slog.String("actor", actor),
		slog.String("old_oid", perr.OldOID),
		slog.String("new_oid", perr.NewOID),
	)
}
```

Delete the no-op stub `EmitRefRejected` from `internal/policy/policy.go`.

- [ ] **Step 5.3: Add unit tests for both emitters**

Create `internal/policy/metrics_test.go`:

```go
package policy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func captureLogger(buf *bytes.Buffer) *slog.Logger {
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h)
}

func TestEmitRefCheckMetric_AllOutcomes(t *testing.T) {
	for _, outcome := range []string{"ok", "blocked_deletion", "blocked_force_push", "internal_error"} {
		var buf bytes.Buffer
		EmitRefCheckMetric(context.Background(), captureLogger(&buf), outcome)
		line := buf.String()
		for _, want := range []string{
			`"metric_name":"policy_refs_check_total"`,
			`"value":1`,
			`"outcome":"` + outcome + `"`,
		} {
			if !strings.Contains(line, want) {
				t.Errorf("[%s] missing %q in %s", outcome, want, line)
			}
		}
	}
}
```

Create `internal/policy/audit_test.go`:

```go
package policy

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestEmitRefRejected_Shape(t *testing.T) {
	var buf bytes.Buffer
	perr := &PolicyError{
		Refname:        "refs/heads/main",
		MatchedPattern: "refs/heads/main",
		Reason:         "non-fast-forward push blocked",
		OldOID:         "deadbeef00000000000000000000000000000000",
		NewOID:         "cafebabe00000000000000000000000000000000",
	}
	EmitRefRejected(context.Background(), captureLogger(&buf), "acme", "site", perr, "alice")
	line := buf.String()
	for _, want := range []string{
		`"msg":"policy.ref.rejected"`,
		`"event":"policy.ref.rejected"`,
		`"tenant":"acme"`,
		`"repo":"site"`,
		`"refname":"refs/heads/main"`,
		`"matched_pattern":"refs/heads/main"`,
		`"reason":"non-fast-forward push blocked"`,
		`"actor":"alice"`,
		`"old_oid":"deadbeef00000000000000000000000000000000"`,
		`"new_oid":"cafebabe00000000000000000000000000000000"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitRefRejected_NilPolicyErrorIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	EmitRefRejected(context.Background(), captureLogger(&buf), "acme", "site", nil, "alice")
	if buf.Len() != 0 {
		t.Errorf("nil PolicyError emitted: %s", buf.String())
	}
}
```

- [ ] **Step 5.4: Run tests + vet**

```bash
go test ./internal/policy/... -count=1 -v 2>&1 | tail -30
go test ./internal/gitproto/receivepack/... -count=1 2>&1 | grep -E "FAIL|^ok"
go vet ./...
```

Expected: every test PASS. (Step 8b will now emit real metrics + audit events through the production emitter bodies, because the function signatures didn't change between stubs and real bodies.)

- [ ] **Step 5.5: Write the smoke script**

Create `scripts/m14-policy-smoke.sh`:

```bash
#!/usr/bin/env bash
# scripts/m14-policy-smoke.sh
#
# End-to-end smoke for M14 protected refs against localfs:
#   1. Build the bucketvcs binary.
#   2. Init an empty repo + authdb.
#   3. Push an initial commit to refs/heads/main via `bucketvcs serve`
#      + a real git client.
#   4. Add a protected_refs rule blocking deletion + force-push on
#      refs/heads/main.
#   5. Attempt a fast-forward push → assert ACCEPT.
#   6. Attempt a force-push → assert REJECT with the protected-branch
#      reason in stderr.
#   7. Attempt a branch deletion → assert REJECT.
#   8. Remove the rule; re-attempt the force-push → assert ACCEPT.
#
# Exits with M14_POLICY_SMOKE_OK on success. Skips with exit 77 if
# go or git is missing.

set -euo pipefail

if ! command -v go  >/dev/null 2>&1; then echo "SKIP: go not on PATH"; exit 77; fi
if ! command -v git >/dev/null 2>&1; then echo "SKIP: git not on PATH"; exit 77; fi
if ! command -v curl >/dev/null 2>&1; then echo "SKIP: curl not on PATH"; exit 77; fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Building bucketvcs"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"
chmod +x "$BIN"

ROOT="$(mktemp -d)"
STORE="localfs:$ROOT/store"
AUTHDB="$ROOT/auth.db"
TENANT="acme"
REPO="m14smoke"
PORT="$(awk 'BEGIN{srand(); print 30000+int(rand()*10000)}')"
URL="http://127.0.0.1:$PORT"

PID=""
cleanup() {
    rc=$?
    if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true
    fi
    if [[ "$rc" -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "M14_POLICY_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT)" >&2
    fi
    rm -f "$BIN"
    exit "$rc"
}
trap cleanup EXIT

echo "==> Init repo"
"$BIN" init --store="$STORE" "$TENANT" "$REPO" >/dev/null

echo "==> Create user + token"
"$BIN" user create --auth-db="$AUTHDB" alice >/dev/null
TOKEN_JSON="$("$BIN" token create --auth-db="$AUTHDB" --user=alice --label=smoke --format=json)"
TOKEN="$(echo "$TOKEN_JSON" | sed -nE 's/.*"secret":"([^"]+)".*/\1/p')"
"$BIN" repo grant --auth-db="$AUTHDB" --user=alice "$TENANT/$REPO" write >/dev/null

echo "==> Start gateway"
"$BIN" serve --store="$STORE" --auth-db="$AUTHDB" --listen="127.0.0.1:$PORT" \
    >"$ROOT/gateway.log" 2>&1 &
PID=$!
sleep 0.5  # gateway startup

CLONE_URL="http://alice:$TOKEN@127.0.0.1:$PORT/$TENANT/$REPO.git"

echo "==> Push initial commit to refs/heads/main"
WORK="$ROOT/work"
git init -q -b main "$WORK"
( cd "$WORK"
  git config user.email t@example.com
  git config user.name t
  git commit -q --allow-empty -m "initial"
  git push -q "$CLONE_URL" main:refs/heads/main )

C1="$(cd "$WORK" && git rev-parse HEAD)"

echo "==> Add a protected_refs rule"
"$BIN" policy refs add --auth-db="$AUTHDB" --tenant="$TENANT" --repo="$REPO" \
    --pattern=refs/heads/main

echo "==> Push a fast-forward commit (expect ACCEPT)"
( cd "$WORK"
  git commit -q --allow-empty -m "ff"
  git push -q "$CLONE_URL" main:refs/heads/main )
echo "    FF accepted"

C2="$(cd "$WORK" && git rev-parse HEAD)"

echo "==> Attempt a force-push (expect REJECT)"
( cd "$WORK"
  git reset --hard "$C1" -q
  git commit -q --allow-empty -m "alt"
  if git push -q --force "$CLONE_URL" main:refs/heads/main 2>"$ROOT/forcepush.err"; then
      echo "FAIL: force-push was accepted"
      cat "$ROOT/forcepush.err" >&2
      exit 1
  fi
  if ! grep -q "protected-branch: non-fast-forward" "$ROOT/forcepush.err"; then
      echo "FAIL: force-push error missing protected-branch reason"
      cat "$ROOT/forcepush.err" >&2
      exit 1
  fi
  echo "    Force-push rejected with: $(grep protected-branch "$ROOT/forcepush.err" | head -1)"
)

echo "==> Attempt a deletion (expect REJECT)"
( cd "$WORK"
  if git push -q "$CLONE_URL" :refs/heads/main 2>"$ROOT/delete.err"; then
      echo "FAIL: deletion was accepted"
      cat "$ROOT/delete.err" >&2
      exit 1
  fi
  if ! grep -q "protected-branch: deletion" "$ROOT/delete.err"; then
      echo "FAIL: deletion error missing protected-branch reason"
      cat "$ROOT/delete.err" >&2
      exit 1
  fi
  echo "    Deletion rejected with: $(grep protected-branch "$ROOT/delete.err" | head -1)"
)

echo "==> Remove the rule + retry the force-push (expect ACCEPT)"
"$BIN" policy refs remove --auth-db="$AUTHDB" --tenant="$TENANT" --repo="$REPO" \
    --pattern=refs/heads/main
( cd "$WORK"
  git push -q --force "$CLONE_URL" main:refs/heads/main
  echo "    Force-push accepted after rule removal"
)

echo "==> M14 protected-refs smoke: OK"
```

Make executable and run:

```bash
chmod +x scripts/m14-policy-smoke.sh
bash scripts/m14-policy-smoke.sh 2>&1 | tail -15
```

Expected: ends with `M14_POLICY_SMOKE_OK`.

If `bucketvcs serve`'s exact flag names differ (e.g., `--auth-db` vs `--auth-store`, `--listen` vs `--bind`), confirm via `bucketvcs serve --help` and adapt the smoke script.

- [ ] **Step 5.6: Write the operator guide**

Create `docs/m14-hooks-policy-operator-guide.md`. Use the M13 LFS operator guide's structure as a model. The guide should cover:

- §1: Overview (what's in M14, what's deferred to Tier 2/3)
- §2: CLI reference (`policy refs add/list/remove` with examples and exit codes)
- §3: Pattern semantics (path.Match, no `**`, examples)
- §4: Enforcement model (when does step 8b run, atomic vs non-atomic batch interaction, fail-closed posture)
- §5: Observability (the 1 metric + 1 audit event; example slog lines)
- §6: Failure modes table (mirrors the one in the design spec §8)
- §7: Common operator recipes (protect main, protect all release branches, allow CI to force-push to release/* but block deletion)
- §8: Migration / opt-out (nil EngineRequest.Policy)
- §9: Deferred work (everything from spec §10, with trigger conditions per the M13 LFS guide §8 pattern)
- §10: FAQ (e.g., "why doesn't `*` cross `/`?", "what happens if I drop the authdb mid-push?")

Aim for ~400-600 lines; the M13 LFS guide is 1000+ but M14's surface is smaller.

- [ ] **Step 5.7: Run all tests + smokes**

```bash
go test ./... -count=1 2>&1 | grep -E "^FAIL" | head
go vet ./...
bash scripts/m14-policy-smoke.sh    2>&1 | tail -3
bash scripts/m13.5-lfs-quota-smoke.sh 2>&1 | tail -3
bash scripts/m13.4-lfs-gc-smoke.sh    2>&1 | tail -3
bash scripts/m13.3-lfs-locks-smoke.sh 2>&1 | tail -3
bash scripts/m13-lfs-smoke-local.sh   2>&1 | tail -3
bash scripts/m12-reshard-smoke.sh     2>&1 | tail -3
```

Expected: every smoke green. New M14 smoke ends `M14_POLICY_SMOKE_OK`; every prior smoke unchanged.

- [ ] **Step 5.8: Commit observability + smoke + operator guide**

```bash
git add internal/policy/ \
        scripts/m14-policy-smoke.sh \
        docs/m14-hooks-policy-operator-guide.md
git commit -m "policy: observability + smoke + operator guide (M14 Task 5)"
```

- [ ] **Step 5.9: Whole-branch roborev review**

```bash
roborev review --branch --reasoning maximum --wait
```

Address findings per the M1+ protocol (memory file `m1_review_protocol.md`). Loop on diminishing returns (Medium+ severity findings worth fixing; Lows triaged; stop when only nits remain or recurring deferred items dominate).

- [ ] **Step 5.10: Exit worktree + squash to main**

Use `ExitWorktree` with `action: "keep"`.

```bash
cd /home/eran/work/bucketvcs
git merge --squash worktree-m14-hooks-policy
git status --short
git diff --cached --stat | tail -20
```

Commit:

```bash
git commit -m "$(cat <<'EOF'
M14: Hooks and policy — protected refs (squash of N commits on worktree-m14-hooks-policy)

Per spec §23 (Hooks, policy, and server-side validation).

Adds per-repo protected-ref rules enforced in the receive-pack
pipeline. Tier 1 of the §23 roadmap: ref-level rules only
(block deletion, block force-push) matched by stdlib path.Match
globs. Tier 2 (file size, path restrictions, author/email regex,
commit-message regex, signed-commit verification) and Tier 3
(external hooks / HTTP webhooks) are explicitly deferred.

CLI:
  bucketvcs policy refs add    --auth-db=X --tenant=T --repo=R --pattern=PAT [--allow-deletion] [--allow-force-push]
  bucketvcs policy refs list   --auth-db=X --tenant=T --repo=R [--format=text|json]
  bucketvcs policy refs remove --auth-db=X --tenant=T --repo=R --pattern=PAT

Enforcement: new step 8b in completeReceivePack runs after the
existing connectivity check and before the refUpdates map build.
For each accepted update, walks the matching protected_refs rows
and rejects via per-ref `ng <refname> protected-branch: <reason>`
status. Deletion detection: newOID == NullOIDHex. Force-push
detection: `git merge-base --is-ancestor` against the local bare
(exit 0 = FF, exit 1 = non-FF, other = internal error).

Schema (migration 0005): protected_refs table with tenant/repo
FK to repos and CHECK constraints on the boolean toggles.

Failure-closed posture: authdb read failures on policy lookup
surface as `ng <refname> internal-error: ...` rather than silent
accepts. Contrast with M13.5 quota where Add failures log and
proceed (quota is post-commit accounting; policy is pre-commit
gating).

Backward compatibility: EngineRequest.Policy defaults to nil
→ no enforcement, no metric emissions. Pre-M14 deployments
unchanged.

Observability:
  - policy_refs_check_total{outcome=ok|blocked_deletion|blocked_force_push|internal_error}
  - Audit: policy.ref.rejected (fires only on rejection)

Verified:
  - go test ./... clean
  - go vet ./... clean
  - scripts/m14-policy-smoke.sh: real git push against `bucketvcs serve`
    exercising FF / force-push / delete / rule-removal scenarios
  - M11/M12/M12.1/M13/M13.3/M13.4/M13.5 smokes still pass
  - N roborev whole-branch review rounds; findings resolved per M1+
    protocol with documented diminishing-returns termination

Deferred:
  - Tier 2 rule families (file size, path restrictions, author email
    regex, commit message regex, signed-commit verification)
  - Tier 3 external hooks (shell scripts, HTTP webhooks)
  - Post-receive infrastructure (natural fit for §24 webhooks)
  - Tenant-level default rules
  - block_create toggle (depends on identity-based gating)
  - Recursive glob (**)
  - Bulk rule operations
  - Audit on accept

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 5.11: Tag + verify on main + cleanup**

```bash
git tag m14-protected-refs
git tag -l | grep -E "^m1[34]"
go test ./... -count=1 2>&1 | grep -E "^FAIL" | head
bash scripts/m14-policy-smoke.sh 2>&1 | tail -3
git worktree remove .claude/worktrees/m14-hooks-policy
git branch -D worktree-m14-hooks-policy
```

Expected: tag listed; sweep clean; smoke OK; worktree gone.

- [ ] **Step 5.12: Memory updates**

Write `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m14_progress.md`:

```markdown
---
name: m14-progress
description: M14 Tier 1 protected refs — per-repo block-deletion / block-force-push rules enforced in receive-pack step 8b. Completed 2026-05-21, commit <SHA>, tag m14-protected-refs.
metadata:
  type: project
---

# M14: Hooks and policy — Tier 1 (protected refs)

**Status:** Merged to main.
**Commit:** <SHA>.
**Tag:** m14-protected-refs (2026-05-21).

## What changed (one-liner)
`bucketvcs policy refs add/list/remove` manages per-repo block-deletion / block-force-push rules; new step 8b in `completeReceivePack` enforces them after the existing connectivity check, returning per-ref `ng <refname> protected-branch: ...` statuses via the standard receive-pack report.

## Architectural decisions
- **State** on M4 authdb sqlite (migration 0005 `protected_refs` table with CHECK constraints on the boolean toggles and an FK to repos(tenant, name) ON DELETE CASCADE).
- **Package** `internal/policy` with `Service.{Add, List, Remove, CheckUpdate}` + `*PolicyError` typed error.
- **Optionality** via nil `EngineRequest.Policy` — pre-M14 behavior with zero behavioral change.
- **Force-push detection** via `git merge-base --is-ancestor` against the local bare (subprocess, not in-process reachability walk). Same approach as M9 / M13.4's other git-binary calls.
- **Glob via stdlib `path.Match`** (no external dep). `*` does NOT cross `/`. Operators add multiple rows for recursive matching.
- **Add-time pattern validation**: Service.Add calls `path.Match(pattern, "")` to verify the pattern; malformed → error before INSERT, so receive-pack never sees a broken pattern.
- **First-rejection-wins is implicit**: ANY matching rule that blocks rejects. Order of evaluation is ORDER BY refname_pattern.
- **Failure-closed**: authdb read failure on policy lookup surfaces as `internal-error` rather than silent accept.

## Verification
- go test ./... clean; go vet ./... clean
- scripts/m14-policy-smoke.sh: real `bucketvcs serve` + `git push` exercising FF / force-push / delete / rule-removal scenarios
- M11/M12/M12.1/M13/M13.3/M13.4/M13.5 smokes still pass
- N roborev whole-branch review rounds

## Deferred
- Tier 2 rule families (file size, path restrictions, author/email regex, commit-message regex, signed-commit verification)
- Tier 3 external hooks (shell scripts, HTTP webhooks)
- Post-receive infrastructure (natural fit for §24 webhooks)
- Tenant-level default rules
- block_create toggle
- Recursive globs (**)
- Bulk rule operations
- Audit-on-accept (existing receive-pack audit already covers accepted pushes)

## Key files
- `internal/auth/sqlitestore/migrations/0005_protected_refs.sql`
- `internal/policy/` (policy.go, policy_test.go, policy_check_test.go, metrics.go, metrics_test.go, audit.go, audit_test.go, doc.go)
- `cmd/bucketvcs/policy.go` + `policy_test.go`
- `internal/gitproto/receivepack/engine.go` (`EngineRequest.Policy` field)
- `internal/gitproto/receivepack/complete.go` (step 8b + helpers)
- `internal/gitproto/receivepack/policy_test.go` (integration test)
- `scripts/m14-policy-smoke.sh`
- `docs/m14-hooks-policy-operator-guide.md` (new — separate from the M13 LFS guide)
```

Update `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md` — append after the M13.5 entry:

```markdown
- [M14 protected refs (Tier 1) merged to main](m14_progress.md) — commit <SHA>, tag m14-protected-refs (2026-05-21); per-repo block-deletion / block-force-push rules in a new protected_refs table on the M4 authdb (migration 0005). Step 8b enforcement in completeReceivePack after connectivity check, before refUpdates map build. Force-push detection via `git merge-base --is-ancestor`. `bucketvcs policy refs add/list/remove` CLI with path.Match globs (no `**`). 1 metric + 1 audit event + localfs smoke (real `bucketvcs serve` + git push). Failure-closed on authdb errors. Tier 2 (file size, path restrictions, author/email, commit message, signed commits), Tier 3 (external hooks / webhooks), tenant defaults, block_create, recursive globs, bulk ops all deferred.
```

Substitute the actual squash SHA into both files.

- [ ] **Step 5.13: Final state check**

```bash
git -C /home/eran/work/bucketvcs branch --show-current   # expect: main
git -C /home/eran/work/bucketvcs log --oneline -3
git -C /home/eran/work/bucketvcs tag -l | grep -E "^m1[34]"
```

Expected: top commit is the squash; `m14-protected-refs` listed alongside the M13 series tags.

Done.
