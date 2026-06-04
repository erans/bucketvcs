//go:build postgres

package sqlitestore

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func openPostgres(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("BUCKETVCS_POSTGRES_URL")
	if url == "" {
		t.Skip("BUCKETVCS_POSTGRES_URL not set")
	}
	s, err := Open(url)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.backend.Name() != "postgres" {
		t.Fatalf("backend=%s, want postgres", s.backend.Name())
	}
	return s
}

func TestPostgresConformance(t *testing.T) {
	s := openPostgres(t)
	ctx := context.Background()

	if _, err := s.GetUserByName(ctx, "_oidc"); err != nil {
		t.Fatalf("migrations did not apply (no _oidc user): %v", err)
	}
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.CreateUser(ctx, "alice", false); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("dup user: want ErrConflict, got %v", err)
	}
	if err := s.RegisterRepo(ctx, "acme", "web"); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	u, err := s.GetUserByName(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Grant(ctx, "alice", "acme", "web", "write"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	actor := &auth.Actor{UserID: u.ID, Name: "alice"}
	if perm, err := s.LookupRepoPerm(ctx, actor, "acme", "web"); err != nil || perm != auth.PermWrite {
		t.Fatalf("perm=%v err=%v want write", perm, err)
	}

	tok, id, secret, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateToken(ctx, id, u.ID, hash, "lap", nil, auth.ScopeRepoWrite, "", "", ""); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if gotActor, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok}); err != nil || gotActor == nil || gotActor.Name != "alice" {
		t.Fatalf("verify: actor=%v err=%v", gotActor, err)
	}

	// CHECK enforcement: scope_perm CHECK on tokens (migration 0010) → must be
	// classified by the postgres SQLSTATE matcher.
	exp := time.Now().Unix() + 900
	err = s.CreateToken(ctx, "BADPERMTOKEN0000000000AA", "_oidc", hash, "x", &exp,
		auth.ScopeRepoRead, "acme", "web", "BOGUS")
	if err == nil {
		t.Fatal("CHECK on scope_perm should reject 'BOGUS'")
	}
	if !s.backend.IsCheckViolation(err) {
		t.Fatalf("postgres CHECK error not matched by IsCheckViolation: %v", err)
	}

	// OIDC mint round-trips.
	mint, err := s.MintOIDCToken(ctx, MintOIDCParams{
		Tenant: "acme", Repo: "web", Perm: auth.PermWrite,
		Scopes: auth.ScopeRepoWrite, TTLSeconds: 900, Label: "oidc:gh:sub",
	})
	if err != nil {
		t.Fatalf("mint oidc: %v", err)
	}
	if _, _, scope, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "x", Password: mint}); err != nil || scope == nil || scope.Repo != "web" {
		t.Fatalf("verify minted: scope=%v err=%v", scope, err)
	}

	// FK cascade: deleting the repo removes its permission rows.
	if err := s.DeleteRepo(ctx, "acme", "web"); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	if perm, _ := s.LookupRepoPerm(ctx, actor, "acme", "web"); perm != auth.PermNone {
		t.Fatalf("after repo delete, perm=%v want none (cascade)", perm)
	}

	// Rename works single-node on postgres (deferred FKs).
	if err := s.RegisterRepo(ctx, "acme", "old"); err != nil {
		t.Fatalf("register old: %v", err)
	}
	if err := s.Grant(ctx, "alice", "acme", "old", "write"); err != nil {
		t.Fatalf("grant old: %v", err)
	}
	if err := s.RenameRepo(ctx, "acme", "old", "new"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if perm, _ := s.LookupRepoPerm(ctx, actor, "acme", "new"); perm != auth.PermWrite {
		t.Fatalf("after rename, perm on new=%v want write", perm)
	}
}

// openPostgresMaxConns opens the live PG store with a pool > 1 so concurrent
// goroutines genuinely use multiple connections.
func openPostgresMaxConns(t *testing.T, n int) *Store {
	t.Helper()
	url := os.Getenv("BUCKETVCS_POSTGRES_URL")
	if url == "" {
		t.Skip("BUCKETVCS_POSTGRES_URL not set")
	}
	s, err := Open(url, WithMaxConns(n))
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPGConcurrentWebhookClaim seeds N pending deliveries and claims them from
// two concurrent goroutines using FOR UPDATE SKIP LOCKED; asserts every row is
// claimed exactly once (no double-claim).
func TestPGConcurrentWebhookClaim(t *testing.T) {
	s := openPostgresMaxConns(t, 4)
	ctx := context.Background()
	db := s.DB()

	// webhook_endpoints has an FK to repos(tenant, name); register the repo
	// the endpoint references before seeding.
	if err := s.RegisterRepo(ctx, "t", "r"); err != nil {
		t.Fatal(err)
	}

	// Seed one endpoint + 50 pending deliveries.
	var epID int64
	if err := db.RunInTx(ctx, func(tx Tx) error {
		id, e := tx.InsertReturningID(ctx,
			`INSERT INTO webhook_endpoints (tenant, repo, url, secret, event_mask, active, created_at)
			 VALUES ('t','r','http://x','s',0,1,0)`)
		epID = id
		return e
	}); err != nil {
		t.Fatal(err)
	}
	const total = 50
	for i := 0; i < total; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO webhook_deliveries
			   (id, endpoint_id, event_type, payload_json, status, attempts, next_attempt_at, created_at)
			 VALUES (?, ?, 'push', ?, 'pending', 0, 0, 0)`,
			fmtID(i), epID, []byte{0}); err != nil {
			t.Fatal(err)
		}
	}

	claimQ := `
		UPDATE webhook_deliveries d
		   SET status='in_flight', last_attempt_at=0, attempts=d.attempts+1
		  FROM webhook_endpoints e
		 WHERE e.id = d.endpoint_id
		   AND d.id IN (
		       SELECT d2.id FROM webhook_deliveries d2
		         JOIN webhook_endpoints e2 ON e2.id = d2.endpoint_id
		        WHERE d2.status='pending' AND d2.next_attempt_at <= 9999999999 AND e2.active=1
		        ORDER BY d2.next_attempt_at LIMIT 7 FOR UPDATE SKIP LOCKED)
		RETURNING d.id`

	var mu sync.Mutex
	seen := map[string]int{}
	var claimed int64 // total rows claimed across all goroutines
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				rows, err := db.QueryContext(ctx, claimQ)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				got := 0
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						t.Errorf("scan: %v", err)
						return
					}
					got++
					mu.Lock()
					seen[id]++
					mu.Unlock()
				}
				rows.Close()
				if got > 0 {
					atomic.AddInt64(&claimed, int64(got))
					continue
				}
				// got == 0: this round claimed nothing. Under FOR UPDATE
				// SKIP LOCKED with all rows sharing next_attempt_at, two
				// claimers contend on the same lowest-ordered batch — one
				// wins it, the loser's UPDATE re-checks each row under the
				// committed snapshot (EvalPlanQual), finds status flipped to
				// 'in_flight', and returns zero. That is the *correct,
				// safe* outcome (no double-claim), not "drained". So a
				// zero round only means "stop" once every row is accounted
				// for; otherwise the remaining pending rows are still
				// claimable and we retry. This distinguishes the safety
				// property under test (each row claimed exactly once) from a
				// transient liveness stall caused by claimer contention.
				if atomic.LoadInt64(&claimed) >= total {
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("claimed %d distinct rows, want %d", len(seen), total)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("row %s claimed %d times (want 1)", id, c)
		}
	}
}

// TestPGConcurrentQuotaCredit increments the same (tenant, oid) from two
// goroutines via the quota_credits ON CONFLICT gate; asserts used_bytes lands
// at exactly one increment.
func TestPGConcurrentQuotaCredit(t *testing.T) {
	s := openPostgresMaxConns(t, 4)
	ctx := context.Background()
	db := s.DB()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO quotas (tenant, limit_bytes, used_bytes, updated_at)
		 VALUES ('qt', 1000000, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	addOnce := func() error {
		return db.RunInTx(ctx, func(tx Tx) error {
			res, err := tx.ExecContext(ctx,
				`INSERT INTO quota_credits (tenant, oid, bytes, recorded_at)
				 VALUES ('qt','oid1',100,0) ON CONFLICT (tenant,oid) DO NOTHING`)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				return nil
			}
			_, err = tx.ExecContext(ctx,
				`UPDATE quotas SET used_bytes = used_bytes + 100 WHERE tenant='qt'`)
			return err
		})
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = addOnce() }()
	}
	wg.Wait()

	var used int64
	if err := db.QueryRowContext(ctx, `SELECT used_bytes FROM quotas WHERE tenant='qt'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 100 {
		t.Fatalf("used_bytes=%d want 100 (concurrent replay must count once)", used)
	}
}

// TestPGConcurrentRename runs two concurrent renames of the same repo; exactly
// one succeeds and the other returns an error, with no orphaned permission rows.
func TestPGConcurrentRename(t *testing.T) {
	s := openPostgresMaxConns(t, 4)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "ru", false); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterRepo(ctx, "rt", "src"); err != nil {
		t.Fatal(err)
	}
	if err := s.Grant(ctx, "ru", "rt", "src", "write"); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.RenameRepo(ctx, "rt", "src", "dst") }()
	}
	wg.Wait()
	close(errs)
	ok, failed := 0, 0
	for e := range errs {
		if e == nil {
			ok++
		} else {
			failed++
		}
	}
	if ok != 1 || failed != 1 {
		t.Fatalf("concurrent rename: ok=%d failed=%d, want 1/1", ok, failed)
	}
	u, _ := s.GetUserByName(ctx, "ru")
	actor := &auth.Actor{UserID: u.ID, Name: "ru"}
	if perm, _ := s.LookupRepoPerm(ctx, actor, "rt", "dst"); perm != auth.PermWrite {
		t.Fatalf("after rename perm on dst=%v want write", perm)
	}
	if perm, _ := s.LookupRepoPerm(ctx, actor, "rt", "src"); perm != auth.PermNone {
		t.Fatalf("src perm=%v want none (no orphan)", perm)
	}
}

func fmtID(i int) string { return "dlv" + strconv.Itoa(i) }

// TestPGDeleteRepoCascade asserts the M25 postgres cascade path: every swept
// child row (protected_refs, protected_paths, hooks, repo_permissions,
// deploy-scoped ssh_keys, lfs_locks, oidc_trust_rules + its oidc_rule_claims
// child) is removed, the repos row is gone, and — the M15.1 drain invariant —
// webhook_endpoints + webhook_deliveries rows SURVIVE so a pending repo.deleted
// delivery can still be claimed. Seeding one row per swept table is the
// postgres counterpart to the sqlite
// TestDeleteRepoCascade_SweepsDependentsKeepsWebhooks regression: it would have
// caught the M25 bug where oidc_trust_rules (migration 0010) was absent from
// the shared cascadeStmts sweep.
func TestPGDeleteRepoCascade(t *testing.T) {
	s := openPostgres(t)
	ctx := context.Background()

	// Idempotent cleanup so re-runs against a persistent DB don't conflict
	// on UNIQUE(tenant, repo, url), the repos PK, or ssh_keys.fingerprint.
	for _, q := range []string{
		`DELETE FROM webhook_deliveries WHERE endpoint_id IN
		   (SELECT id FROM webhook_endpoints WHERE tenant=? AND repo=?)`,
		`DELETE FROM webhook_endpoints WHERE tenant=? AND repo=?`,
		`DELETE FROM protected_refs WHERE tenant=? AND repo=?`,
		`DELETE FROM protected_paths WHERE tenant=? AND repo=?`,
		`DELETE FROM hooks WHERE tenant=? AND repo=?`,
		`DELETE FROM lfs_locks WHERE tenant=? AND repo=?`,
		`DELETE FROM repos WHERE tenant=? AND name=?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, "cascade", "pgdel"); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
	// Cleanups that don't key on (tenant, repo).
	for _, q := range []string{
		`DELETE FROM oidc_rule_claims WHERE rule_id='pgdel-r1'`,
		`DELETE FROM oidc_trust_rules WHERE id='pgdel-r1'`,
		`DELETE FROM oidc_issuers WHERE alias='pgdel-gh'`,
		`DELETE FROM ssh_keys WHERE id='pgdel-k1'`,
		`DELETE FROM users WHERE id='pgdel-u1'`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}

	if err := s.RegisterRepo(ctx, "cascade", "pgdel"); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	now := time.Now().Unix()

	// A real user to own the lfs_lock and a repo_permissions grant.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, name, is_admin, created_at) VALUES (?, ?, 0, ?)`,
		"pgdel-u1", "pgdel-u1", now); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// One row per swept child table (only NOT NULL columns without defaults).
	seeds := []struct {
		name string
		sql  string
		args []any
	}{
		{"protected_refs",
			`INSERT INTO protected_refs (tenant, repo, refname_pattern, created_at) VALUES (?, ?, ?, ?)`,
			[]any{"cascade", "pgdel", "refs/heads/main", now}},
		{"protected_paths",
			`INSERT INTO protected_paths (tenant, repo, refname_pattern, path_pattern, created_at) VALUES (?, ?, ?, ?, ?)`,
			[]any{"cascade", "pgdel", "refs/heads/main", "secrets/**", now}},
		{"hooks",
			`INSERT INTO hooks (tenant, repo, "trigger", script_name, sort_order, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, 0, 1, ?, ?)`,
			[]any{"cascade", "pgdel", "pre-receive", "lint.sh", now, now}},
		{"repo_permissions",
			`INSERT INTO repo_permissions (user_id, tenant, repo, perm, granted_at) VALUES (?, ?, ?, ?, ?)`,
			[]any{"pgdel-u1", "cascade", "pgdel", "write", now}},
		{"ssh_keys",
			`INSERT INTO ssh_keys (id, fingerprint, public_key, key_type, label, created_at, user_id, scope_tenant, scope_repo, scope_perm)
			 VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, ?)`,
			[]any{"pgdel-k1", "pgdel-fp1", []byte{0}, "ssh-rsa", "lbl", now, "cascade", "pgdel", "read"}},
		{"lfs_locks",
			`INSERT INTO lfs_locks (id, tenant, repo, path, ref_name, owner_user_id, locked_at) VALUES (?, ?, ?, ?, NULL, ?, ?)`,
			[]any{"pgdel-l1", "cascade", "pgdel", "file.bin", "pgdel-u1", now}},
		{"oidc_issuers",
			`INSERT INTO oidc_issuers (alias, issuer_url, created_at) VALUES (?, ?, ?)`,
			[]any{"pgdel-gh", "https://pgdel.example/oidc", now}},
		{"oidc_trust_rules",
			`INSERT INTO oidc_trust_rules (id, issuer_alias, audience, tenant, repo, scopes, ttl_seconds, created_at) VALUES (?, ?, ?, ?, ?, 0, 900, ?)`,
			[]any{"pgdel-r1", "pgdel-gh", "aud", "cascade", "pgdel", now}},
		{"oidc_rule_claims",
			`INSERT INTO oidc_rule_claims (rule_id, claim_name, claim_value) VALUES (?, ?, ?)`,
			[]any{"pgdel-r1", "sub", "repo:cascade/pgdel"}},
	}
	for _, sd := range seeds {
		if _, err := s.db.ExecContext(ctx, sd.sql, sd.args...); err != nil {
			t.Fatalf("seed %s: %v", sd.name, err)
		}
	}

	var epID int64
	err := s.db.RunInTx(ctx, func(tx Tx) error {
		var e error
		epID, e = tx.InsertReturningID(ctx,
			`INSERT INTO webhook_endpoints (tenant, repo, url, secret, event_mask, active, created_at)
			 VALUES (?, ?, ?, ?, ?, 1, ?)`,
			"cascade", "pgdel", "https://example.invalid/hook", "shh", 1, now)
		return e
	})
	if err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_deliveries (id, endpoint_id, event_type, payload_json, status, next_attempt_at, created_at)
		 VALUES (?, ?, ?, ?, 'pending', ?, ?)`,
		"dlv-pgdel-1", epID, "repo.deleted", []byte(`{}`), now, now); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	if err := s.DeleteRepoCascade(ctx, "cascade", "pgdel"); err != nil {
		t.Fatalf("DeleteRepoCascade on postgres: %v", err)
	}

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM repos WHERE tenant=? AND name=?`, "cascade", "pgdel"); n != 0 {
		t.Errorf("repos row survived: %d", n)
	}
	// Every repo-scoped child table must be empty for (cascade, pgdel).
	swept := []struct{ table, where string }{
		{"protected_refs", `tenant='cascade' AND repo='pgdel'`},
		{"protected_paths", `tenant='cascade' AND repo='pgdel'`},
		{"hooks", `tenant='cascade' AND repo='pgdel'`},
		{"repo_permissions", `tenant='cascade' AND repo='pgdel'`},
		{"ssh_keys", `scope_tenant='cascade' AND scope_repo='pgdel'`},
		{"lfs_locks", `tenant='cascade' AND repo='pgdel'`},
		{"oidc_trust_rules", `tenant='cascade' AND repo='pgdel'`},
		{"oidc_rule_claims", `rule_id='pgdel-r1'`},
	}
	for _, c := range swept {
		if n := count(`SELECT COUNT(*) FROM ` + c.table + ` WHERE ` + c.where); n != 0 {
			t.Errorf("%s not swept: %d (want 0)", c.table, n)
		}
	}
	// M15.1 drain invariant: the webhook rows must survive the cascade.
	if n := count(`SELECT COUNT(*) FROM webhook_endpoints WHERE tenant=? AND repo=?`, "cascade", "pgdel"); n != 1 {
		t.Errorf("webhook_endpoints did not survive: %d (want 1)", n)
	}
	if n := count(`SELECT COUNT(*) FROM webhook_deliveries WHERE endpoint_id=?`, epID); n != 1 {
		t.Errorf("webhook_deliveries did not survive: %d (want 1)", n)
	}
}

// TestPGQuotaBigInt is the regression for the 32-bit INTEGER overflow on the
// quota byte columns (migration 0012 widens them to BIGINT). On PostgreSQL
// INTEGER is 32-bit (max 2147483647 ≈ 2.0 GiB); LFS objects and tenant totals
// routinely exceed that. Before 0012, writing a multi-GB value errored with
// "integer out of range"; after 0012 the BIGINT columns round-trip it exactly.
// Exercises all three byte columns: quotas.limit_bytes, quotas.used_bytes,
// quota_credits.bytes.
func TestPGQuotaBigInt(t *testing.T) {
	s := openPostgres(t)
	ctx := context.Background()
	db := s.DB()

	const big = int64(5_000_000_000)  // ~5 GB, > 2^31-1
	const used = int64(3_000_000_000) // ~3 GB, > 2^31-1

	if _, err := db.ExecContext(ctx,
		`INSERT INTO quotas (tenant, limit_bytes, used_bytes, updated_at) VALUES (?, ?, 0, 0)`,
		"bigt", big); err != nil {
		t.Fatalf("insert >2^31 limit_bytes (INTEGER overflow if not BIGINT): %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO quota_credits (tenant, oid, bytes, recorded_at) VALUES (?, ?, ?, 0)`,
		"bigt", "bigoid", used); err != nil {
		t.Fatalf("insert >2^31 quota_credits.bytes: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE quotas SET used_bytes = ? WHERE tenant = ?`, used, "bigt"); err != nil {
		t.Fatalf("update >2^31 used_bytes: %v", err)
	}

	var gotLimit, gotUsed, gotCredit int64
	if err := db.QueryRowContext(ctx,
		`SELECT limit_bytes, used_bytes FROM quotas WHERE tenant = ?`, "bigt").
		Scan(&gotLimit, &gotUsed); err != nil {
		t.Fatalf("read quotas: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT bytes FROM quota_credits WHERE tenant = ? AND oid = ?`, "bigt", "bigoid").
		Scan(&gotCredit); err != nil {
		t.Fatalf("read quota_credits: %v", err)
	}
	if gotLimit != big || gotUsed != used || gotCredit != used {
		t.Fatalf("byte values truncated: limit=%d used=%d credit=%d, want %d/%d/%d",
			gotLimit, gotUsed, gotCredit, big, used, used)
	}
}
