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
