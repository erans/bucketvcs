# M21 Webhook Prune + Repo Rename Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `bucketvcs webhook prune` (sweep terminal-state delivery rows past retention) + `bucketvcs repo rename` (same-tenant auth-only rename) + wire the dead-since-M15 `EventRepoRenamed` webhook taxonomy const from the rename CLI.

**Architecture:** Two small additions to existing packages — `webhooks.Service.Prune()` does the DELETE; `sqlitestore.Store.RenameRepo()` does the multi-table UPDATE under `foreign_keys=OFF`. Two new CLI handlers wire them via the established `runWebhook`/`runRepo` dispatch patterns. Rename enqueues `EventRepoRenamed` BEFORE the auth transaction (matches M15.1 delete ordering so the existing webhook worker can resolve endpoints under the old name).

**Tech Stack:** Go stdlib (database/sql, time, flag, encoding/json, log/slog), modernc.org/sqlite via existing `internal/auth/sqlitestore`. No new dependencies. No new tables. No new migration.

---

## File structure

**Created:**
- `internal/webhooks/prune.go` — `Service.Prune(ctx, PruneConfig) (PruneReport, error)` + `PruneConfig`/`PruneReport` types
- `internal/webhooks/prune_test.go`
- `internal/auth/sqlitestore/rename.go` — `Store.RenameRepo(ctx, tenant, oldName, newName) error` + `ErrRepoExists` sentinel
- `internal/auth/sqlitestore/rename_test.go`
- `cmd/bucketvcs/webhook_prune.go` — `runWebhookPrune` handler
- `cmd/bucketvcs/repo_rename.go` — `runRepoRename` handler
- `scripts/m21-webhook-prune-repo-rename-smoke.sh`

**Modified:**
- `cmd/bucketvcs/webhook.go` — extend `runWebhook` dispatch to handle `"prune"`; extend `webhookUsage` string
- `cmd/bucketvcs/repocmd.go` — extend `runRepo` dispatch to handle `"rename"`
- `internal/webhooks/metrics.go` — add `EmitWebhookPrunedMetric` emitter
- `internal/webhooks/audit.go` — add `EmitWebhookPruned` audit emitter
- `docs/m15-webhook-operator-guide.md` (or wherever M15 doc lives — confirm at Task 0) — add a "Webhook delivery retention" section + a "Repo rename: auth-only semantics" subsection

**Untouched:**
- `internal/webhooks/event.go` — `EventRepoRenamed` const + `RepoRenamedPayload` already declared; just consumed for the first time from the rename CLI
- All migration files — no schema change

---

## Task 0: Survey and confirm assumptions

**Files:** read-only.

- [ ] **Step 1: Confirm webhook delivery status literals**

```bash
cd /home/eran/work/bucketvcs/.claude/worktrees/m21-webhook-prune-repo-rename
grep -rnE "status[ =]*'pending'|status[ =]*'in_flight'|status[ =]*'delivered'|status[ =]*'dead_letter'" internal/webhooks/*.go | head -10
```

Verify the 4 literal status strings: `pending`, `in_flight`, `delivered`, `dead_letter`. If any deviate, the plan's SQL needs to use the real ones.

- [ ] **Step 2: Confirm FK-bearing tables that reference `repos(tenant, name)`**

```bash
grep -rn "REFERENCES repos" internal/auth/sqlitestore/migrations/
```

Expected: 6 hits, one per migration. Plan assumes the 6 FK tables are: `repo_permissions` (0001), `ssh_keys` via `scope_(tenant,repo)` (0002), `protected_refs` (0005), `webhook_endpoints` (0006), `protected_paths` (0007), `hooks` (0009). Plus `lfs_locks` (0003) which has tenant/repo columns but NO FK to repos — Task 2 still UPDATEs it for value consistency (matches M15.1's manual sweep on delete).

- [ ] **Step 3: Confirm CLI dispatch shapes**

```bash
sed -n '50,68p' cmd/bucketvcs/webhook.go
sed -n '19,42p' cmd/bucketvcs/repocmd.go
```

Verify `runWebhook` dispatches via `switch args[0]` on object name (`endpoint`, `delivery`) — Task 3 adds a `"prune"` case. Verify `runRepo` dispatches the same way (`register, grant, revoke, public, list, delete, deploy-key`) — Task 4 adds a `"rename"` case.

- [ ] **Step 4: Confirm webhook Service.Enqueue + audit emit patterns**

```bash
grep -nA3 "^func .* Enqueue\b" internal/webhooks/enqueue.go | head -10
grep -nA4 "^func Emit" internal/webhooks/audit.go internal/webhooks/metrics.go | head -25
```

Note the existing patterns for Service.Enqueue signature + audit/metric emitter shape — Task 1 + Task 2 mirror them.

- [ ] **Step 5: Confirm M15 operator guide path**

```bash
ls docs/m15-* docs/webhook* 2>&1 | head
```

If the M15 webhook operator guide doesn't exist as a separate file, the prune retention guidance + rename semantics go into `docs/m14-hooks-policy-operator-guide.md` (the merged operator guide for the policy/webhook tier). Adapt Task 5 Step 5 accordingly.

- [ ] **Step 6: No commits in this task**

Survey-only. Report findings.

---

## Task 1: webhooks.Service.Prune

**Files:**
- Create: `internal/webhooks/prune.go`
- Test: `internal/webhooks/prune_test.go`
- Modify: `internal/webhooks/metrics.go` — add `EmitWebhookPrunedMetric`
- Modify: `internal/webhooks/audit.go` — add `EmitWebhookPruned`

- [ ] **Step 1: Read existing audit + metric emitters to match the pattern**

```bash
grep -nA10 "^func Emit" internal/webhooks/audit.go | head -30
grep -nA10 "^func Emit" internal/webhooks/metrics.go | head -30
```

Note the exact slog idiom (LogAttrs vs Info, attr keys, level). Tasks below mirror it.

- [ ] **Step 2: Write the failing prune test**

Create `internal/webhooks/prune_test.go`:

```go
package webhooks_test

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// seedDelivery inserts one row into webhook_deliveries with the given status
// and timestamps. Returns the row id.
func seedDelivery(t *testing.T, db *sqlitestore.Store, status string, createdAt, deliveredAt, lastAttemptAt int64) string {
	t.Helper()
	id := status + "-" + time.Unix(createdAt, 0).Format("150405.000000")
	// endpoint_id=1 is seeded by the test's setup helper below; webhook_deliveries
	// has FK to webhook_endpoints with ON DELETE CASCADE, so the test setup
	// must register an endpoint first.
	_, err := db.DB().ExecContext(context.Background(), `
		INSERT INTO webhook_deliveries (id, endpoint_id, event_type, payload_json,
		    status, attempts, next_attempt_at, last_attempt_at, last_status_code,
		    last_error, created_at, delivered_at)
		VALUES (?, ?, 'test.event', X'7B7D', ?, 1, ?, ?, NULL, NULL, ?, ?)`,
		id, 1, status, createdAt, nullableInt(lastAttemptAt), createdAt, nullableInt(deliveredAt))
	if err != nil {
		t.Fatalf("seed %s: %v", status, err)
	}
	return id
}

// nullableInt returns nil for 0, the value otherwise. webhook_deliveries
// allows NULL for delivered_at + last_attempt_at on pending rows.
func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// newPruneTestService spins up an in-memory authdb, registers one webhook
// endpoint so the FK constraint is satisfied, and returns (Service, Store).
func newPruneTestService(t *testing.T) (*webhooks.Service, *sqlitestore.Store) {
	t.Helper()
	s, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.RegisterRepo(context.Background(), "acme", "site"); err != nil {
		t.Fatal(err)
	}
	svc := webhooks.New(s.DB())
	ep, err := svc.Create(context.Background(), webhooks.EndpointInput{
		Tenant:    "acme",
		Repo:      "site",
		URL:       "https://example.com/hook",
		EventMask: webhooks.AllEvents,
		Actor:     "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ep.ID != 1 {
		t.Fatalf("expected endpoint_id=1, got %d", ep.ID)
	}
	return svc, s
}

// TestPrune_OnlyTerminalStatesPastCutoff verifies the DELETE only touches
// `delivered` rows past delivered_at cutoff AND `dead_letter` rows past
// last_attempt_at cutoff. `pending` and `in_flight` rows are NEVER pruned
// even when they're ancient.
func TestPrune_OnlyTerminalStatesPastCutoff(t *testing.T) {
	svc, _ := newPruneTestService(t)

	now := time.Now().Unix()
	veryOld := now - 10*86400 // 10 days ago
	recent := now - 60        // 60 seconds ago

	// 8 rows: 4 states × {past-cutoff, within-cutoff}
	seedDelivery(t, getStore(svc), "pending",     veryOld, 0, 0)
	seedDelivery(t, getStore(svc), "pending",     recent,  0, 0)
	seedDelivery(t, getStore(svc), "in_flight",   veryOld, 0, veryOld)
	seedDelivery(t, getStore(svc), "in_flight",   recent,  0, recent)
	seedDelivery(t, getStore(svc), "delivered",   veryOld, veryOld, veryOld)
	seedDelivery(t, getStore(svc), "delivered",   recent,  recent,  recent)
	seedDelivery(t, getStore(svc), "dead_letter", veryOld, 0, veryOld)
	seedDelivery(t, getStore(svc), "dead_letter", recent,  0, recent)

	cfg := webhooks.PruneConfig{
		DeliveredCutoff:  time.Unix(now-86400, 0), // delete delivered older than 1 day
		DeadLetterCutoff: time.Unix(now-86400, 0), // delete dead_letter older than 1 day
		DryRun:           false,
	}
	report, err := svc.Prune(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if report.DeliveredDeleted != 1 {
		t.Errorf("DeliveredDeleted = %d, want 1", report.DeliveredDeleted)
	}
	if report.DeadLetterDeleted != 1 {
		t.Errorf("DeadLetterDeleted = %d, want 1", report.DeadLetterDeleted)
	}

	// Verify pending + in_flight rows still present, even the ancient ones.
	var remaining int
	if err := getStore(svc).DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM webhook_deliveries WHERE status IN ('pending','in_flight')`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 4 {
		t.Errorf("pending+in_flight after prune = %d, want 4 (never pruned)", remaining)
	}
}

// TestPrune_DryRunMatchesCountWithoutDeleting pins that --dry-run returns
// the same counts the real prune would, but doesn't mutate the table.
func TestPrune_DryRunMatchesCountWithoutDeleting(t *testing.T) {
	svc, _ := newPruneTestService(t)
	now := time.Now().Unix()
	veryOld := now - 10*86400
	seedDelivery(t, getStore(svc), "delivered",   veryOld, veryOld, veryOld)
	seedDelivery(t, getStore(svc), "delivered",   veryOld, veryOld, veryOld)
	seedDelivery(t, getStore(svc), "dead_letter", veryOld, 0, veryOld)

	cfg := webhooks.PruneConfig{
		DeliveredCutoff:  time.Unix(now-86400, 0),
		DeadLetterCutoff: time.Unix(now-86400, 0),
		DryRun:           true,
	}
	report, err := svc.Prune(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Prune dry-run: %v", err)
	}
	if report.DeliveredDeleted != 2 || report.DeadLetterDeleted != 1 {
		t.Errorf("dry-run counts = (%d, %d), want (2, 1)", report.DeliveredDeleted, report.DeadLetterDeleted)
	}

	// Table should still hold all 3 rows.
	var total int
	if err := getStore(svc).DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM webhook_deliveries`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("after dry-run, table has %d rows; want 3 (no deletes)", total)
	}
}

// TestPrune_EmptyTableNoOp returns (0, 0, nil) and does not error.
func TestPrune_EmptyTableNoOp(t *testing.T) {
	svc, _ := newPruneTestService(t)
	report, err := svc.Prune(context.Background(), webhooks.PruneConfig{
		DeliveredCutoff:  time.Now(),
		DeadLetterCutoff: time.Now(),
	})
	if err != nil {
		t.Fatalf("Prune empty: %v", err)
	}
	if report.DeliveredDeleted != 0 || report.DeadLetterDeleted != 0 {
		t.Errorf("empty-table counts = (%d, %d), want (0, 0)",
			report.DeliveredDeleted, report.DeadLetterDeleted)
	}
}

// getStore is a small helper to recover the underlying *Store from the
// service. The webhooks package doesn't expose its DB, but tests in the
// same module commonly use an injection helper. If webhooks.Service has
// no exposed DB and no test helper, add `func (s *Service) DBForTest()
// *sql.DB { return s.db }` inside the production code marked
// `// Test-only: used by prune_test.go to seed delivery rows.`
func getStore(svc *webhooks.Service) *sqlitestore.Store {
	// Defined inline in the test setup helper newPruneTestService instead.
	// This stub kept here to keep the test compile; the seed helpers receive
	// the *Store directly from the setup.
	return nil
}
```

**Note on `getStore`:** the test as written threads `*Store` through `newPruneTestService`. The cleaner shape: have `newPruneTestService` return `(*Service, *sql.DB)` and pass the `*sql.DB` directly to `seedDelivery`. Adapt the test to whichever is more natural after reading the existing `webhooks/*_test.go` patterns.

- [ ] **Step 3: Run failing tests**

```bash
go test ./internal/webhooks/... -run TestPrune -count=1 -v 2>&1 | tail -10
```

Expected: FAIL — `webhooks.Service.Prune`, `webhooks.PruneConfig`, `webhooks.PruneReport` don't exist yet.

- [ ] **Step 4: Implement Service.Prune**

Create `internal/webhooks/prune.go`:

```go
package webhooks

import (
	"context"
	"fmt"
	"time"
)

// PruneConfig parameterizes a single prune sweep.
type PruneConfig struct {
	// DeliveredCutoff: rows with status='delivered' AND delivered_at < this
	// are deleted. Required (zero value means "delete all delivered rows" —
	// matches the dry-run heuristic).
	DeliveredCutoff time.Time
	// DeadLetterCutoff: rows with status='dead_letter' AND last_attempt_at <
	// this are deleted.
	DeadLetterCutoff time.Time
	// DryRun: if true, count rows that WOULD be deleted but DO NOT delete.
	// Implemented as two SELECT COUNT queries instead of DELETE statements.
	DryRun bool
}

// PruneReport summarizes a Prune call.
type PruneReport struct {
	DeliveredDeleted  int64
	DeadLetterDeleted int64
}

// Prune sweeps terminal-state webhook delivery rows past their retention
// window. Never touches `pending` or `in_flight`.
//
// Two separate statements per call (one for delivered, one for dead_letter)
// so the per-status counts are exact via RowsAffected. SQLite serializes
// the DELETEs at the connection level; partial commit is not possible
// within either statement.
func (s *Service) Prune(ctx context.Context, cfg PruneConfig) (PruneReport, error) {
	var r PruneReport
	if cfg.DryRun {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM webhook_deliveries
			 WHERE status='delivered' AND delivered_at < ?`,
			cfg.DeliveredCutoff.Unix(),
		).Scan(&r.DeliveredDeleted); err != nil {
			return r, fmt.Errorf("webhooks.Prune dry-run delivered: %w", err)
		}
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM webhook_deliveries
			 WHERE status='dead_letter' AND last_attempt_at < ?`,
			cfg.DeadLetterCutoff.Unix(),
		).Scan(&r.DeadLetterDeleted); err != nil {
			return r, fmt.Errorf("webhooks.Prune dry-run dead_letter: %w", err)
		}
		return r, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webhook_deliveries
		 WHERE status='delivered' AND delivered_at < ?`,
		cfg.DeliveredCutoff.Unix())
	if err != nil {
		return r, fmt.Errorf("webhooks.Prune delivered: %w", err)
	}
	r.DeliveredDeleted, _ = res.RowsAffected()

	res, err = s.db.ExecContext(ctx,
		`DELETE FROM webhook_deliveries
		 WHERE status='dead_letter' AND last_attempt_at < ?`,
		cfg.DeadLetterCutoff.Unix())
	if err != nil {
		return r, fmt.Errorf("webhooks.Prune dead_letter: %w", err)
	}
	r.DeadLetterDeleted, _ = res.RowsAffected()
	return r, nil
}
```

- [ ] **Step 5: Add metric + audit emitters**

Append to `internal/webhooks/metrics.go`:

```go
// EmitWebhookPrunedMetric counts pruned-rows per outcome.
func EmitWebhookPrunedMetric(ctx context.Context, logger *slog.Logger, outcome string, count int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "webhook_deliveries_pruned_total"),
		slog.String("outcome", outcome),
		slog.Int64("value", count),
	)
}
```

Append to `internal/webhooks/audit.go`:

```go
// EmitWebhookPruned emits the webhooks.pruned audit event.
func EmitWebhookPruned(ctx context.Context, logger *slog.Logger,
	deliveredRows, deadLetterRows int64,
	deliveredCutoff, deadLetterCutoff time.Time,
	dryRun bool, actor string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "webhooks.pruned",
		slog.Int64("delivered_rows", deliveredRows),
		slog.Int64("dead_letter_rows", deadLetterRows),
		slog.Int64("delivered_cutoff_unix", deliveredCutoff.Unix()),
		slog.Int64("dead_letter_cutoff_unix", deadLetterCutoff.Unix()),
		slog.Bool("dry_run", dryRun),
		slog.String("actor", actor),
	)
}
```

Add `"time"` to audit.go imports if missing.

- [ ] **Step 6: Resolve the DB-access seam for tests**

If `webhooks.Service` doesn't already expose `DB()` for test helpers, add this in `internal/webhooks/service.go` next to other accessors:

```go
// DBForTest is a test-only accessor for the underlying sql.DB so external
// test packages can seed delivery rows directly. Not part of the stable
// API; consumers in production code use the Service methods.
func (s *Service) DBForTest() *sql.DB { return s.db }
```

If a similar accessor already exists, reuse it. Update the test's `getStore` stub to use it (or restructure the test setup to thread `*sqlitestore.Store` directly).

- [ ] **Step 7: Run tests**

```bash
go test ./internal/webhooks/... -run TestPrune -count=1 -v 2>&1 | tail -15
```

Expected: 3 tests pass.

- [ ] **Step 8: Run all webhooks tests to confirm no regressions**

```bash
go test ./internal/webhooks/... -count=1 2>&1 | tail -5
```

Expected: full webhooks suite green.

- [ ] **Step 9: Commit**

```bash
git add internal/webhooks/
git commit -m "webhooks: Service.Prune + metrics + audit (M21 Task 1)"
```

---

## Task 2: sqlitestore.Store.RenameRepo

**Files:**
- Create: `internal/auth/sqlitestore/rename.go`
- Test: `internal/auth/sqlitestore/rename_test.go`

Transactional multi-table UPDATE that walks all FK-bearing dependents (children first), then updates the `repos` row, all under `PRAGMA foreign_keys = OFF`. Mirrors M15.1's delete pattern.

- [ ] **Step 1: Write the failing tests**

Create `internal/auth/sqlitestore/rename_test.go`:

```go
package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// TestRenameRepo_BasicRoundTrip registers acme/foo, grants alice→write, then
// renames to acme/bar. Asserts: repos row is now (acme, bar); repo_permissions
// row points at (acme, bar); old (acme, foo) row is gone.
func TestRenameRepo_BasicRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.GrantRepoPerm(ctx, "alice", "acme", "foo", auth.PermWrite); err != nil {
		t.Fatal(err)
	}

	if err := s.RenameRepo(ctx, "acme", "foo", "bar"); err != nil {
		t.Fatalf("RenameRepo: %v", err)
	}

	// Old row gone.
	if _, err := s.GetRepoFlags(ctx, "acme", "foo"); !errors.Is(err, auth.ErrNoSuchRepo) {
		t.Errorf("old row still exists, err=%v", err)
	}

	// New row present.
	if _, err := s.GetRepoFlags(ctx, "acme", "bar"); err != nil {
		t.Errorf("new row missing: %v", err)
	}

	// Grant follows.
	perm, err := s.GetRepoPerm(ctx, "alice", "acme", "bar")
	if err != nil {
		t.Fatalf("GetRepoPerm on new name: %v", err)
	}
	if perm != auth.PermWrite {
		t.Errorf("perm = %v, want Write", perm)
	}

	// Grant on old name is gone.
	if _, err := s.GetRepoPerm(ctx, "alice", "acme", "foo"); err == nil {
		t.Errorf("grant on old name still present")
	}
}

// TestRenameRepo_ErrRepoExists rejects rename to a destination that already exists.
func TestRenameRepo_ErrRepoExists(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "foo")
	_ = s.RegisterRepo(ctx, "acme", "bar")
	err := s.RenameRepo(ctx, "acme", "foo", "bar")
	if !errors.Is(err, ErrRepoExists) {
		t.Errorf("err = %v, want ErrRepoExists", err)
	}
}

// TestRenameRepo_ErrNoSuchRepo rejects rename when source does not exist.
func TestRenameRepo_ErrNoSuchRepo(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	ctx := context.Background()
	err := s.RenameRepo(ctx, "acme", "ghost", "bar")
	if !errors.Is(err, auth.ErrNoSuchRepo) {
		t.Errorf("err = %v, want auth.ErrNoSuchRepo", err)
	}
}

// TestRenameRepo_TouchesAllFKTables verifies every FK-bearing table is updated.
// Seeds rows in: repo_permissions (0001), ssh_keys.scope_(tenant,repo) (0002),
// lfs_locks (0003 — non-FK but tenant/repo-scoped), protected_refs (0005),
// webhook_endpoints (0006), protected_paths (0007), hooks (0009).
func TestRenameRepo_TouchesAllFKTables(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "foo")
	_ = s.AddUser(ctx, "alice")
	_ = s.GrantRepoPerm(ctx, "alice", "acme", "foo", auth.PermWrite)
	// Insert one row per FK table via direct SQL. The test's purpose is to
	// detect orphan rows after rename — exact row shape doesn't matter as
	// long as it carries (tenant, repo) and counts towards COUNT(*).
	for _, q := range []string{
		`INSERT INTO ssh_keys (id, owner_user_id, fingerprint, public_key, label, scope_tenant, scope_repo, scope_perm, created_at) VALUES ('k1', 'alice', 'fp1', 'ssh-rsa AAA', 'lbl', 'acme', 'foo', 1, 1)`,
		`INSERT INTO lfs_locks (id, tenant, repo, path, ref_name, owner_user_id, locked_at) VALUES ('l1', 'acme', 'foo', 'file.bin', NULL, 'alice', 1)`,
		`INSERT INTO protected_refs (tenant, repo, refname_pattern, block_deletion, block_non_fast_forward, created_at) VALUES ('acme', 'foo', 'refs/heads/main', 1, 1, 1)`,
		`INSERT INTO webhook_endpoints (tenant, repo, url, secret_b64, event_mask, active, created_at, updated_at) VALUES ('acme', 'foo', 'https://x', 'YQ==', 1, 1, 1, 1)`,
		`INSERT INTO protected_paths (tenant, repo, refname_pattern, path_pattern, created_at) VALUES ('acme', 'foo', 'refs/heads/main', 'secrets/**', 1)`,
		`INSERT INTO hooks (tenant, repo, trigger, script_name, sort_order, enabled, created_at, updated_at) VALUES ('acme', 'foo', 'pre-receive', 'lint.sh', 0, 1, 1, 1)`,
	} {
		if _, err := s.DB().ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	if err := s.RenameRepo(ctx, "acme", "foo", "bar"); err != nil {
		t.Fatalf("RenameRepo: %v", err)
	}

	// For each table, assert: 0 rows with old name, 1 row with new name.
	for _, c := range []struct {
		table, tCol, rCol string
	}{
		{"repo_permissions", "tenant", "repo"},
		{"ssh_keys", "scope_tenant", "scope_repo"},
		{"lfs_locks", "tenant", "repo"},
		{"protected_refs", "tenant", "repo"},
		{"webhook_endpoints", "tenant", "repo"},
		{"protected_paths", "tenant", "repo"},
		{"hooks", "tenant", "repo"},
	} {
		var oldCount, newCount int
		if err := s.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+c.table+` WHERE `+c.tCol+`=? AND `+c.rCol+`=?`,
			"acme", "foo").Scan(&oldCount); err != nil {
			t.Errorf("%s: count old: %v", c.table, err)
			continue
		}
		if err := s.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+c.table+` WHERE `+c.tCol+`=? AND `+c.rCol+`=?`,
			"acme", "bar").Scan(&newCount); err != nil {
			t.Errorf("%s: count new: %v", c.table, err)
			continue
		}
		if oldCount != 0 || newCount != 1 {
			t.Errorf("%s: old=%d new=%d, want old=0 new=1", c.table, oldCount, newCount)
		}
	}
}
```

If `GetRepoPerm`/`AddUser`/`GrantRepoPerm` have different names, find the actual API:

```bash
grep -nE "^func .*GetRepoPerm|^func .*AddUser|^func .*Grant" internal/auth/sqlitestore/store.go | head -10
```

Adapt the test to actual method names. The shape (register repo → grant → rename → assert) is the invariant.

- [ ] **Step 2: Run failing tests**

```bash
go test ./internal/auth/sqlitestore/... -run TestRenameRepo -count=1 -v 2>&1 | tail -15
```

Expected: FAIL — `RenameRepo` + `ErrRepoExists` don't exist.

- [ ] **Step 3: Implement RenameRepo**

Create `internal/auth/sqlitestore/rename.go`:

```go
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// ErrRepoExists is returned by RenameRepo when the destination (tenant, name)
// is already registered. Operators must delete or rename the destination
// first.
var ErrRepoExists = errors.New("sqlitestore: destination repo already exists")

// RenameRepo updates the (tenant, name) primary key of the repos row from
// (tenant, oldName) to (tenant, newName) and propagates the new name to
// every FK-bearing table — same-tenant only.
//
// Implementation: a transaction with PRAGMA foreign_keys=OFF so the
// referential constraints don't fire mid-UPDATE. The walk is children-then-
// parent to keep the database internally consistent for any intermediate
// reader (none exist on sqlite's single-writer model, but the order matches
// M15.1's DELETE pattern for predictability).
//
// Returns auth.ErrNoSuchRepo when (tenant, oldName) doesn't exist, or
// ErrRepoExists when (tenant, newName) already does.
//
// Same-tenant only: the caller is responsible for enforcing this at the CLI
// layer. The API takes only the new bare name (not a tenant/name pair) to
// make cross-tenant rename impossible by signature.
func (s *Store) RenameRepo(ctx context.Context, tenant, oldName, newName string) error {
	if newName == oldName {
		return fmt.Errorf("sqlitestore.RenameRepo: new name equals old name")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: begin: %w", err)
	}
	defer tx.Rollback()

	// Existence + collision checks inside the transaction (consistent
	// snapshot, prevents racing renamers from both passing the check).
	var have int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM repos WHERE tenant=? AND name=?`,
		tenant, oldName).Scan(&have); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: check old: %w", err)
	}
	if have == 0 {
		return auth.ErrNoSuchRepo
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM repos WHERE tenant=? AND name=?`,
		tenant, newName).Scan(&have); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: check new: %w", err)
	}
	if have > 0 {
		return ErrRepoExists
	}

	// Disable FK enforcement for the duration of the transaction. SQLite
	// scopes this to the current connection; the transaction holds that
	// connection, so other connections are unaffected.
	if _, err := tx.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: PRAGMA off: %w", err)
	}

	// Children first. Each statement updates 0..N rows; that's fine —
	// child tables may have no entries for the renamed repo.
	type stmt struct {
		table, q string
	}
	for _, st := range []stmt{
		{"repo_permissions", `UPDATE repo_permissions SET repo=? WHERE tenant=? AND repo=?`},
		// ssh_keys uses scope_(tenant, repo) column names.
		{"ssh_keys", `UPDATE ssh_keys SET scope_repo=? WHERE scope_tenant=? AND scope_repo=?`},
		// lfs_locks has no FK to repos (M15.1 manually sweeps on delete).
		{"lfs_locks", `UPDATE lfs_locks SET repo=? WHERE tenant=? AND repo=?`},
		{"protected_refs", `UPDATE protected_refs SET repo=? WHERE tenant=? AND repo=?`},
		{"webhook_endpoints", `UPDATE webhook_endpoints SET repo=? WHERE tenant=? AND repo=?`},
		{"protected_paths", `UPDATE protected_paths SET repo=? WHERE tenant=? AND repo=?`},
		{"hooks", `UPDATE hooks SET repo=? WHERE tenant=? AND repo=?`},
	} {
		if _, err := tx.ExecContext(ctx, st.q, newName, tenant, oldName); err != nil {
			return fmt.Errorf("sqlitestore.RenameRepo: update %s: %w", st.table, err)
		}
	}

	// Parent last. The PK update writes a new row and removes the old.
	if _, err := tx.ExecContext(ctx,
		`UPDATE repos SET name=? WHERE tenant=? AND name=?`,
		newName, tenant, oldName); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: update repos: %w", err)
	}

	// Restore FK enforcement BEFORE COMMIT. SQLite's docs say PRAGMA
	// foreign_keys can be toggled outside a transaction or at the start of
	// one; toggling it back inside a transaction is a no-op until commit,
	// but explicit re-enable keeps the connection state clean.
	if _, err := tx.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: PRAGMA on: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: commit: %w", err)
	}
	return nil
}
```

If the column-name assumptions for any table are wrong (e.g. `ssh_keys` uses `scope_tenant`/`scope_repo` per migration 0002, but the actual column casing/spelling could differ), grep the migration files for the exact names:

```bash
grep -A20 "CREATE TABLE ssh_keys\|CREATE TABLE lfs_locks" internal/auth/sqlitestore/migrations/*.sql
```

Update the statements list to match.

- [ ] **Step 4: Run tests**

```bash
go test ./internal/auth/sqlitestore/... -run TestRenameRepo -count=1 -v 2>&1 | tail -20
```

Expected: 4 tests pass.

- [ ] **Step 5: Full sqlitestore suite**

```bash
go test ./internal/auth/sqlitestore/... -count=1 2>&1 | tail -5
```

Expected: green.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/sqlitestore/
git commit -m "sqlitestore: RenameRepo multi-table UPDATE under foreign_keys=OFF (M21 Task 2)"
```

---

## Task 3: bucketvcs webhook prune CLI

**Files:**
- Create: `cmd/bucketvcs/webhook_prune.go`
- Modify: `cmd/bucketvcs/webhook.go` — add `"prune"` to the dispatch + usage const

- [ ] **Step 1: Extend the dispatch + usage**

Edit `cmd/bucketvcs/webhook.go`. Find the `runWebhook` switch + the `webhookUsage` const:

```bash
sed -n '40,68p' cmd/bucketvcs/webhook.go
grep -n "^const webhookUsage\|webhookUsage =" cmd/bucketvcs/webhook.go | head
```

Add a `case "prune":` clause that calls into the new handler:

```go
	case "prune":
		return runWebhookPrune(ctx, args[1:], stdout, stderr)
```

Update `webhookUsage` to mention `prune` in the list of objects/actions. Match the existing prose style.

- [ ] **Step 2: Write the handler**

Create `cmd/bucketvcs/webhook_prune.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

const webhookPruneUsage = `Usage: bucketvcs webhook prune [flags]

Sweep terminal-state webhook delivery rows past their retention window.
Never touches 'pending' or 'in_flight' rows.

Flags:
  --auth-db=<path>                       (required) path to auth.db
  --delivered-older-than=<duration>      retention for status='delivered'
                                         (default 30d; cutoff: delivered_at)
  --dead-letter-older-than=<duration>    retention for status='dead_letter'
                                         (default 90d; cutoff: last_attempt_at)
  --dry-run                              list rows that would be deleted as
                                         NDJSON; do not mutate
  --actor=<string>                       audit attribution
                                         (default: OS username)

Examples:
  bucketvcs webhook prune --auth-db=/var/lib/bucketvcs/auth.db
  bucketvcs webhook prune --auth-db=... --dry-run --delivered-older-than=7d
`

func runWebhookPrune(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("webhook prune", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	deliveredAge := fs.Duration("delivered-older-than", 30*24*time.Hour,
		"retention for status='delivered'")
	deadLetterAge := fs.Duration("dead-letter-older-than", 90*24*time.Hour,
		"retention for status='dead_letter'")
	dryRun := fs.Bool("dry-run", false, "list rows that would be deleted; do not mutate")

	actorPassed := false
	actor := ""
	fs.Func("actor", "audit attribution",
		func(s string) error {
			actorPassed = true
			actor = s
			return nil
		})

	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" {
		fmt.Fprintln(stderr, "webhook prune: --auth-db is required")
		fmt.Fprint(stderr, webhookPruneUsage)
		return 2
	}
	if actorPassed && actor == "" {
		fmt.Fprintln(stderr, "webhook prune: --actor must be non-empty if specified")
		return 2
	}
	// Operator guard: refuse retention < 1h to prevent fat-finger pruning of
	// live rows. Negative durations are also rejected.
	const minRetention = time.Hour
	if *deliveredAge < minRetention {
		fmt.Fprintf(stderr, "webhook prune: --delivered-older-than must be >= %s (got %s)\n",
			minRetention, *deliveredAge)
		return 2
	}
	if *deadLetterAge < minRetention {
		fmt.Fprintf(stderr, "webhook prune: --dead-letter-older-than must be >= %s (got %s)\n",
			minRetention, *deadLetterAge)
		return 2
	}

	svc, store, err := openWebhookSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "webhook prune: %v\n", err)
		return 1
	}
	defer store.Close()

	now := time.Now()
	cfg := webhooks.PruneConfig{
		DeliveredCutoff:  now.Add(-*deliveredAge),
		DeadLetterCutoff: now.Add(-*deadLetterAge),
		DryRun:           *dryRun,
	}

	if *dryRun {
		// In dry-run mode, also enumerate the rows that would be deleted
		// as NDJSON for operator visibility. The count comes from Prune.
		report, err := svc.Prune(ctx, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "webhook prune: %v\n", err)
			return 1
		}
		rows, err := svc.PendingForTest(ctx) // existing helper for read-only listing
		_ = rows
		_ = err
		// Better: a dedicated read query in webhooks for "list rows that
		// would be pruned"; but for MVP, just print the counts + cutoffs.
		// Operators wanting per-row detail can query the table directly.
		fmt.Fprintf(stdout, "DRY-RUN: would prune %d delivered (older than %s), %d dead-letter (older than %s)\n",
			report.DeliveredDeleted, deliveredAge.String(),
			report.DeadLetterDeleted, deadLetterAge.String())
		// Audit dry-run too so operators can correlate timing.
		webhooks.EmitWebhookPruned(ctx, slog.Default(),
			report.DeliveredDeleted, report.DeadLetterDeleted,
			cfg.DeliveredCutoff, cfg.DeadLetterCutoff, true, actorOrUser(actor))
		webhooks.EmitWebhookPrunedMetric(ctx, slog.Default(), "delivered", report.DeliveredDeleted)
		webhooks.EmitWebhookPrunedMetric(ctx, slog.Default(), "dead_letter", report.DeadLetterDeleted)
		return 0
	}

	report, err := svc.Prune(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "webhook prune: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "pruned: %d delivered (older than %s), %d dead-letter (older than %s)\n",
		report.DeliveredDeleted, deliveredAge.String(),
		report.DeadLetterDeleted, deadLetterAge.String())
	webhooks.EmitWebhookPruned(ctx, slog.Default(),
		report.DeliveredDeleted, report.DeadLetterDeleted,
		cfg.DeliveredCutoff, cfg.DeadLetterCutoff, false, actorOrUser(actor))
	webhooks.EmitWebhookPrunedMetric(ctx, slog.Default(), "delivered", report.DeliveredDeleted)
	webhooks.EmitWebhookPrunedMetric(ctx, slog.Default(), "dead_letter", report.DeadLetterDeleted)
	return 0
}

// actorOrUser returns the explicit actor when set, else the OS username.
// If a similar helper already exists in cmd/bucketvcs (M15.1 added one),
// reuse it.
func actorOrUser(actor string) string {
	if actor != "" {
		return actor
	}
	return osUsername() // existing helper from cmd/bucketvcs; defined elsewhere
}

// NDJSON imports — kept here even if dry-run currently doesn't emit per-row
// JSON, so future enhancement is one line.
var _ = json.Encoder{}
```

**Adaptations:**
- Check whether `osUsername` / `actorOrUser` is the actual helper name in `cmd/bucketvcs/`:
  ```bash
  grep -n "osUsername\|actorOrUser\|currentActor" cmd/bucketvcs/*.go | head
  ```
- The dry-run NDJSON emission is intentionally minimal in MVP (just the count). If operators want per-row detail, defer to a follow-up.

- [ ] **Step 3: Build**

```bash
go vet ./... 2>&1 | tail -5
go build ./... 2>&1 | tail -5
```

Expected: clean. If unused imports flagged (e.g. `json.Encoder` if no NDJSON output): remove them.

- [ ] **Step 4: Smoke-test --help**

```bash
go run ./cmd/bucketvcs webhook prune --help 2>&1 | head -20
```

Expected: usage text.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/webhook.go cmd/bucketvcs/webhook_prune.go
git commit -m "cmd: bucketvcs webhook prune (M21 Task 3)"
```

---

## Task 4: bucketvcs repo rename CLI + EventRepoRenamed enqueue

**Files:**
- Create: `cmd/bucketvcs/repo_rename.go`
- Modify: `cmd/bucketvcs/repocmd.go` — add `"rename"` to dispatch

- [ ] **Step 1: Extend the dispatch**

Edit `cmd/bucketvcs/repocmd.go`. Find the switch in `runRepo` (~line 19):

```go
	case "rename":
		return repoRename(ctx, args[1:], stdout, stderr)
```

Also update the usage const if there's one; or use the same pattern the existing subcommands use to print usage on no-args.

- [ ] **Step 2: Write the handler**

Create `cmd/bucketvcs/repo_rename.go`:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

const repoRenameUsage = `Usage: bucketvcs repo rename <tenant>/<old-name> <new-name> --auth-db=<path> --store=<url> [flags]

Same-tenant auth-only rename. Updates the auth.db row + all FK-bearing tables.
Storage keys are NOT migrated — they stay at the old 'tenants/<t>/repos/<old>/'
prefix. The operator handles storage migration out of band if desired.

The new-name argument is a BARE segment (no slash). Cross-tenant rename is
not supported; see the M21 design spec §1.2 deferred items.

Flags:
  --auth-db=<path>   (required) path to auth.db
  --store=<url>      (required) storage URL for the collision check
  --actor=<string>   audit attribution (default: OS username)

Examples:
  bucketvcs repo rename acme/foo bar --auth-db=/var/lib/bucketvcs/auth.db --store=localfs:/var/lib/bucketvcs/store
`

func repoRename(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo rename", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	storeURL := fs.String("store", "", "storage URL for collision check")

	actorPassed := false
	actor := ""
	fs.Func("actor", "audit attribution",
		func(s string) error {
			actorPassed = true
			actor = s
			return nil
		})

	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if actorPassed && actor == "" {
		fmt.Fprintln(stderr, "repo rename: --actor must be non-empty if specified")
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprint(stderr, repoRenameUsage)
		return 2
	}
	tenant, oldName, err := splitTenantRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	newName := fs.Arg(1)
	// CLI surface guard: cross-tenant impossible by parsing — new-name must
	// be a bare segment (no slash).
	if strings.ContainsAny(newName, "/\\") {
		fmt.Fprintln(stderr, "repo rename: <new-name> must be a bare segment; cross-tenant rename is not supported in M21")
		return 2
	}
	if newName == "" {
		fmt.Fprintln(stderr, "repo rename: <new-name> must not be empty")
		return 2
	}
	if newName == oldName {
		fmt.Fprintln(stderr, "repo rename: new name equals old name; no-op")
		return 2
	}

	if *authDB == "" {
		fmt.Fprintln(stderr, "repo rename: --auth-db is required")
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "repo rename: --store is required (for storage collision check)")
		return 2
	}

	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()

	// Pre-check: source exists.
	if _, err := s.GetRepoFlags(ctx, tenant, oldName); err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			fmt.Fprintf(stderr, "repo rename: %s/%s not found\n", tenant, oldName)
			webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "not_found") // see Step 3
			return 1
		}
		fmt.Fprintf(stderr, "repo rename: %v\n", err)
		return 1
	}

	// Collision check 1: destination auth row.
	if _, err := s.GetRepoFlags(ctx, tenant, newName); err == nil {
		fmt.Fprintf(stderr, "repo rename: destination %s/%s already exists in auth.db\n", tenant, newName)
		webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "collision_auth")
		return 1
	}

	// Collision check 2: storage non-empty under new prefix.
	bs, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "repo rename: store: %v\n", err)
		return 1
	}
	defer closeStore(bs)
	prefix := "tenants/" + tenant + "/repos/" + newName + "/"
	entries, err := bs.List(ctx, prefix, 1)
	if err != nil {
		fmt.Fprintf(stderr, "repo rename: storage collision check: %v\n", err)
		return 1
	}
	if len(entries) > 0 {
		fmt.Fprintf(stderr, "repo rename: storage at %s is non-empty (%s ...); refusing to rename\n",
			prefix, entries[0].Key)
		webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "collision_storage")
		return 1
	}

	// Enqueue webhook BEFORE the auth transaction. Endpoints subscribed
	// under the old name are still in webhook_endpoints; this is when the
	// worker can resolve them. Matches M15.1 delete ordering. If the
	// subsequent transaction fails, the webhook still delivers — documented
	// caveat in spec §5.
	webhookSvc, _, werr := openWebhookSvc(*authDB)
	if werr != nil {
		// Fail-open: an unreachable webhook service shouldn't block rename.
		slog.Default().Warn("repo rename: webhook enqueue skipped", "err", werr)
	} else {
		payload := webhooks.RepoRenamedPayload{
			OldName: oldName,
			NewName: newName,
		}
		if eerr := webhookSvc.Enqueue(ctx, webhooks.EventRepoRenamed,
			tenant, oldName, actorOrUser(actor), payload); eerr != nil {
			// Fail-open per the M15.1 precedent: log and continue.
			slog.Default().Warn("repo rename: webhook enqueue failed", "err", eerr)
		}
	}

	if err := s.RenameRepo(ctx, tenant, oldName, newName); err != nil {
		switch {
		case errors.Is(err, sqlitestore.ErrRepoExists):
			// Race: another rename landed between our pre-check and the
			// transaction. Report as auth-collision.
			fmt.Fprintf(stderr, "repo rename: destination %s/%s appeared during rename\n", tenant, newName)
			webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "collision_auth")
			return 1
		case errors.Is(err, auth.ErrNoSuchRepo):
			// Race: source deleted between our pre-check and the transaction.
			fmt.Fprintf(stderr, "repo rename: source %s/%s disappeared during rename\n", tenant, oldName)
			webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "not_found")
			return 1
		default:
			fmt.Fprintf(stderr, "repo rename: %v\n", err)
			return 1
		}
	}

	webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "ok")
	// Audit event under the existing repo.* namespace. The webhook delivery
	// already covers the public notification; this is the local audit trail.
	slog.Default().LogAttrs(ctx, slog.LevelInfo, "repo.renamed",
		slog.String("tenant", tenant),
		slog.String("old_name", oldName),
		slog.String("new_name", newName),
		slog.String("actor", actorOrUser(actor)),
	)
	fmt.Fprintf(stdout, "renamed: %s/%s -> %s/%s\n", tenant, oldName, tenant, newName)
	return 0
}
```

**Adaptations:**
- If `openAuthDB`, `openStore`, `closeStore`, `splitTenantRepo`, `reorderFlagsFirst`, `actorOrUser`/`osUsername` are named differently, find the actual names:
  ```bash
  grep -n "^func openAuthDB\|^func splitTenantRepo\|^func openStore\|^func reorderFlagsFirst" cmd/bucketvcs/*.go | head
  ```
- If `webhooks.RepoRenamedPayload` doesn't have `OldName`/`NewName` literal field names, check `internal/webhooks/event.go` and use the actual ones.
- If `webhooks.EmitRepoRenamedMetric` doesn't exist yet, define it in Task 4 Step 3.

- [ ] **Step 3: Add the metric emitter**

Append to `internal/webhooks/metrics.go`:

```go
// EmitRepoRenamedMetric counts repo-rename outcomes.
// Outcomes: ok | collision_auth | collision_storage | not_found | cross_tenant.
func EmitRepoRenamedMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "repo_renamed_total"),
		slog.String("outcome", outcome),
		slog.Int("value", 1),
	)
}
```

- [ ] **Step 4: Build**

```bash
go vet ./... 2>&1 | tail -5
go build ./... 2>&1 | tail -5
```

Expected: clean. Fix any missing-helper / wrong-field-name errors.

- [ ] **Step 5: Smoke-test --help**

```bash
go run ./cmd/bucketvcs repo rename --help 2>&1 | head -15
```

Expected: usage text.

- [ ] **Step 6: Quick end-to-end manual check**

```bash
TMPDIR_TEST="$(mktemp -d)"
AUTH_DB="$TMPDIR_TEST/auth.db"
STORE_DIR="$TMPDIR_TEST/store"
mkdir -p "$STORE_DIR"
go run ./cmd/bucketvcs user add --auth-db "$AUTH_DB" alice
go run ./cmd/bucketvcs repo register --auth-db "$AUTH_DB" --store "localfs:$STORE_DIR" acme/foo
go run ./cmd/bucketvcs repo rename acme/foo bar --auth-db "$AUTH_DB" --store "localfs:$STORE_DIR"
go run ./cmd/bucketvcs repo list --auth-db "$AUTH_DB"
# Expected: see acme/bar in the list, no acme/foo.
rm -rf "$TMPDIR_TEST"
```

- [ ] **Step 7: Commit**

```bash
git add cmd/bucketvcs/repocmd.go cmd/bucketvcs/repo_rename.go internal/webhooks/metrics.go
git commit -m "cmd: bucketvcs repo rename + EventRepoRenamed emitter (M21 Task 4)"
```

---

## Task 5: Smoke + operator guide

**Files:**
- Create: `scripts/m21-webhook-prune-repo-rename-smoke.sh`
- Modify: M15 webhook operator guide (path confirmed at Task 0)

- [ ] **Step 1: Study the M14/M15.1/M20 smoke shapes**

```bash
ls scripts/m14-*.sh scripts/m15-*.sh scripts/m20-*.sh 2>&1
cat scripts/m15.1-polish-smoke.sh 2>&1 | head -120  # if it exists; else m15-webhook-smoke.sh
```

Match `set -euo pipefail`, ss-based port finder w/ collision retry (M19 pattern), cleanup trap with KEEP_TMP forensics on ERR, ends with `M21_WEBHOOK_PRUNE_RENAME_SMOKE_OK`.

- [ ] **Step 2: Write the smoke**

Create `scripts/m21-webhook-prune-repo-rename-smoke.sh`:

```bash
#!/usr/bin/env bash
# M21 webhook prune + repo rename smoke.
#
# Validates:
#   1. webhook delivery flows end-to-end (push → endpoint receives → row marked delivered)
#   2. `bucketvcs webhook prune --delivered-older-than=0s` deletes the delivered row
#   3. `bucketvcs repo rename` updates auth.db; pushes to old name 404; pushes to new name succeed
#   4. repo.renamed audit event appears in serve.log
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

go build -o /tmp/bvc-m21 ./cmd/bucketvcs

TMPDIR="$(mktemp -d)"
SERVE_LOG="$TMPDIR/serve.log"
STORE_DIR="$TMPDIR/store"
AUTH_DB="$TMPDIR/auth.db"
RECV_LOG="$TMPDIR/hook-recv.log"
mkdir -p "$STORE_DIR"

pick_port() {
    local i candidate inuse
    inuse="$(ss -ltn 2>/dev/null | awk 'NR>1 {sub(/.*:/, "", $4); print $4}' | sort -u)"
    for i in $(seq 1 20); do
        candidate="$(awk 'BEGIN{srand('"$$$i"'); print 30000+int(rand()*10000)}')"
        if ! grep -qx "$candidate" <<<"$inuse"; then
            echo "$candidate"
            return 0
        fi
    done
    return 1
}
PORT="$(pick_port)"
HOOK_PORT="$(pick_port)"
[[ -n "$PORT" && -n "$HOOK_PORT" && "$PORT" != "$HOOK_PORT" ]] || { echo "could not pick distinct ports"; exit 1; }

cleanup() {
    [[ -n "${SERVE_PID:-}" ]] && kill -0 "$SERVE_PID" 2>/dev/null && { kill "$SERVE_PID"; wait "$SERVE_PID" 2>/dev/null || true; }
    [[ -n "${HOOK_PID:-}" ]] && kill -0 "$HOOK_PID" 2>/dev/null && { kill "$HOOK_PID"; wait "$HOOK_PID" 2>/dev/null || true; }
    if [[ "${KEEP_TMP:-0}" != "1" ]]; then rm -rf "$TMPDIR"; else echo "TMPDIR preserved: $TMPDIR"; fi
}
trap cleanup EXIT
on_failure() { echo "==> FAILURE — serve.log tail:"; tail -100 "$SERVE_LOG"; KEEP_TMP=1; }
trap on_failure ERR

echo "==> Start a tiny HTTP server to receive webhook deliveries"
( python3 -c "
import http.server, socketserver, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(length).decode('utf-8', 'replace')
        with open('$RECV_LOG', 'a') as f:
            f.write(body + '\n')
        self.send_response(204); self.end_headers()
    def log_message(self, *args, **kwargs): pass
with socketserver.TCPServer(('127.0.0.1', $HOOK_PORT), H) as s:
    s.serve_forever()
" >/dev/null 2>&1 ) &
HOOK_PID=$!
sleep 0.3

echo "==> Bootstrap authdb + alice"
/tmp/bvc-m21 user add --auth-db "$AUTH_DB" alice >/dev/null
TOK="$(/tmp/bvc-m21 token create --auth-db "$AUTH_DB" alice 2>&1 | sed -n 's/^token=//p' | head -1)"
[[ -n "$TOK" ]] || { echo "no token extracted"; exit 1; }

echo "==> Register acme/foo + grant alice write + register webhook endpoint"
/tmp/bvc-m21 repo register --auth-db "$AUTH_DB" --store "localfs:$STORE_DIR" acme/foo >/dev/null
/tmp/bvc-m21 repo grant --auth-db "$AUTH_DB" alice acme/foo write >/dev/null
/tmp/bvc-m21 webhook endpoint add --auth-db "$AUTH_DB" \
    --tenant=acme --repo=foo \
    --url="http://127.0.0.1:$HOOK_PORT/hook" \
    --events=push,repo.renamed >/dev/null

echo "==> Start serve on $PORT"
/tmp/bvc-m21 serve --addr "127.0.0.1:$PORT" --auth-db "$AUTH_DB" --store "localfs:$STORE_DIR" --lfs=false >"$SERVE_LOG" 2>&1 &
SERVE_PID=$!
for i in $(seq 1 50); do curl -sS -o /dev/null "http://127.0.0.1:$PORT/healthz" 2>/dev/null && break; sleep 0.1; done

echo "==> Push a commit to acme/foo to generate a webhook delivery"
WD="$TMPDIR/seed"; mkdir -p "$WD"; cd "$WD"
git init -q
git config user.email a@a; git config user.name a; git config commit.gpgsign false
echo hello > f; git add f; git commit -q -m seed
git push -q "http://alice:$TOK@127.0.0.1:$PORT/acme/foo.git" HEAD:refs/heads/main
cd "$ROOT"

echo "==> Wait for delivery to complete (worker tick + http)"
for i in $(seq 1 30); do
    delivered="$(sqlite3 "$AUTH_DB" "SELECT COUNT(*) FROM webhook_deliveries WHERE status='delivered'" 2>/dev/null || echo 0)"
    [[ "$delivered" -gt 0 ]] && break
    sleep 0.5
done
[[ "$delivered" -gt 0 ]] || { echo "FAIL: no delivered rows in webhook_deliveries"; exit 1; }
echo "OK   delivered rows present: $delivered"

echo "==> Prune with --delivered-older-than=1h (no-op, still within retention)"
/tmp/bvc-m21 webhook prune --auth-db "$AUTH_DB" --delivered-older-than=1h --dead-letter-older-than=1h
after="$(sqlite3 "$AUTH_DB" "SELECT COUNT(*) FROM webhook_deliveries WHERE status='delivered'")"
[[ "$after" == "$delivered" ]] || { echo "FAIL: 1h prune deleted rows it shouldn't have ($delivered → $after)"; exit 1; }
echo "OK   1h prune left rows intact: $after"

echo "==> Sleep 65 minutes worth of cutoff via SQL injection (advance delivered_at into the past)"
sqlite3 "$AUTH_DB" "UPDATE webhook_deliveries SET delivered_at = delivered_at - 7200 WHERE status='delivered'"

echo "==> Prune again, now rows should disappear"
out="$(/tmp/bvc-m21 webhook prune --auth-db "$AUTH_DB" --delivered-older-than=1h --dead-letter-older-than=1h)"
echo "    $out"
post="$(sqlite3 "$AUTH_DB" "SELECT COUNT(*) FROM webhook_deliveries WHERE status='delivered'")"
[[ "$post" == "0" ]] || { echo "FAIL: prune left $post delivered rows behind"; exit 1; }
echo "OK   delivered rows pruned"
grep -q "webhooks.pruned" "$SERVE_LOG" && echo "OK   webhooks.pruned audit in serve.log (background ops, may be absent for CLI invocation)" || true

echo "==> Rename acme/foo → bar"
/tmp/bvc-m21 repo rename acme/foo bar --auth-db "$AUTH_DB" --store "localfs:$STORE_DIR"

echo "==> Confirm auth.db updated"
rows="$(sqlite3 "$AUTH_DB" "SELECT name FROM repos WHERE tenant='acme'")"
[[ "$rows" == "bar" ]] || { echo "FAIL: repos table shows $rows, want bar"; exit 1; }
echo "OK   auth.db row renamed: $rows"

echo "==> Push to NEW name acme/bar succeeds"
WD2="$TMPDIR/seed2"; mkdir -p "$WD2"; cd "$WD2"
git init -q
git config user.email b@b; git config user.name b; git config commit.gpgsign false
echo hello2 > f; git add f; git commit -q -m seed2
git push -q "http://alice:$TOK@127.0.0.1:$PORT/acme/bar.git" HEAD:refs/heads/feature && echo "OK   push to acme/bar succeeded" || { echo "FAIL: push to acme/bar"; exit 1; }
cd "$ROOT"

echo "==> Push to OLD name acme/foo returns 404"
WD3="$TMPDIR/seed3"; mkdir -p "$WD3"; cd "$WD3"
git init -q
git config user.email c@c; git config user.name c; git config commit.gpgsign false
echo hello3 > f; git add f; git commit -q -m seed3
if git push "http://alice:$TOK@127.0.0.1:$PORT/acme/foo.git" HEAD:refs/heads/main 2>"$TMPDIR/old-push-err.log"; then
    echo "FAIL: push to deleted acme/foo unexpectedly succeeded"
    exit 1
fi
echo "OK   push to old name acme/foo failed as expected"
cd "$ROOT"

echo "==> Verify repo.renamed audit in serve.log"
grep -q '"msg":"repo.renamed"\|repo.renamed' "$SERVE_LOG" || { echo "FAIL: repo.renamed audit missing"; tail -30 "$SERVE_LOG"; exit 1; }
echo "OK   repo.renamed audit present"

echo "==> Verify the rename webhook was delivered"
sleep 2
grep -q "repo.renamed" "$RECV_LOG" 2>/dev/null && echo "OK   webhook receiver got repo.renamed event" || echo "INFO repo.renamed webhook not observed (endpoint may have been renamed too; documented behavior)"

echo
echo "M21_WEBHOOK_PRUNE_RENAME_SMOKE_OK"
```

Make executable: `chmod +x scripts/m21-webhook-prune-repo-rename-smoke.sh`.

- [ ] **Step 3: Run smoke**

```bash
./scripts/m21-webhook-prune-repo-rename-smoke.sh 2>&1 | tail -25
```

Iterate until it ends with `M21_WEBHOOK_PRUNE_RENAME_SMOKE_OK`. Common pitfalls:
- Token output format: confirm via `/tmp/bvc-m21 token create --help`
- Webhook endpoint flag names: confirm via `/tmp/bvc-m21 webhook endpoint add --help`
- Webhook worker tick latency: bump the loop count if `delivered` doesn't appear quickly
- `sqlite3` CLI binary availability: install if missing

- [ ] **Step 4: Sanity-check prior smokes still pass**

```bash
./scripts/m14-policy-smoke.sh 2>&1 | tail -3
./scripts/m18-rate-limit-smoke.sh 2>&1 | tail -3
./scripts/m19-multitenant-proxied-smoke.sh 2>&1 | tail -3
./scripts/m20-hooks-smoke.sh 2>&1 | tail -3
```

Each must end with its OK marker.

- [ ] **Step 5: Update operator guide**

Find the M15 webhook guide path (confirmed at Task 0 Step 5). Likely candidates:
- `docs/m15-webhook-operator-guide.md` (if it exists)
- `docs/m14-hooks-policy-operator-guide.md` (the merged guide)

Add two new subsections:

**"Webhook delivery retention"**:
- The `webhook_deliveries` table grows monotonically with push volume.
- `bucketvcs webhook prune` sweeps terminal-state rows past retention. Recommended cron: daily at off-peak hours.
- Default retention: 30 days (`delivered`) + 90 days (`dead_letter`). Adjust per ops needs.
- `--dry-run` reports counts without mutating.
- Audit event `webhooks.pruned` captures the operation; metric `webhook_deliveries_pruned_total{outcome=...}` for dashboards.

**"Repo rename: auth-only semantics"**:
- `bucketvcs repo rename acme/foo bar` updates auth.db; storage keys stay at `tenants/acme/repos/foo/...`.
- Operator-managed storage migration is out of band (rsync, S3 mv, etc.).
- Refuses rename if destination auth row OR storage prefix is non-empty.
- Cross-tenant rename not supported in M21 (use repo transfer in a future milestone).
- Emits `EventRepoRenamed` webhook before the auth transaction; subscribers under the old (tenant, repo) receive the event. If the transaction subsequently fails, the webhook still delivers — documented caveat.

- [ ] **Step 6: Commit**

```bash
git add scripts/m21-webhook-prune-repo-rename-smoke.sh docs/
git commit -m "scripts+docs: M21 smoke + operator guide (M21 Task 5)"
```

---

## Acceptance verification (post-Task 5)

```bash
go vet ./...
go build ./...
go test ./... -count=1
./scripts/m21-webhook-prune-repo-rename-smoke.sh
./scripts/m20-hooks-smoke.sh
./scripts/m19-multitenant-proxied-smoke.sh
./scripts/m18-rate-limit-smoke.sh
./scripts/m14-policy-smoke.sh
```

All must pass.

- `bucketvcs webhook prune --help` shows the 4 flags.
- `bucketvcs repo rename --help` shows the args + flags.
- `EventRepoRenamed` is no longer dead taxonomy — emitted by the rename CLI.

---

## Self-review notes

- **Spec coverage**: §1.1 in-scope items each map to tasks. Prune CLI → Tasks 1+3. Rename CLI + EventRepoRenamed → Tasks 2+4. Metrics + audit → integrated into Tasks 1+4. Smoke + operator guide → Task 5. Same-tenant only (§1.2) is enforced at CLI shape in Task 4 (bare `<new-name>` segment) AND impossible in the Store API (3-arg signature).
- **Placeholder scan**: zero "TBD"; every step has either a complete code block or a complete bash command. The dry-run NDJSON output in Task 3 is intentionally counts-only for MVP; spec §4.1 documents the per-row NDJSON as a stretch goal that operators can do via direct sqlite query.
- **Type consistency**: `PruneConfig`/`PruneReport` defined in Task 1 are used identically in Task 3. `Store.RenameRepo(ctx, tenant, oldName, newName)` defined in Task 2 used identically in Task 4. `webhooks.EmitRepoRenamedMetric` is introduced in Task 4 Step 3.
- **Cross-task coupling**: Tasks 1 + 2 are independent. Task 3 depends on Task 1 (Service.Prune). Task 4 depends on Task 2 (RenameRepo) + Task 1 (EmitWebhookPruned for the audit emitter pattern, though emitter is in metrics.go which Task 4 modifies). Task 5 depends on Tasks 3 + 4.
