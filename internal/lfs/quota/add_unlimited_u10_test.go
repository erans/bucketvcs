package quota_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
)

// Wave-3 U-10 regression coverage: Add's doc says "No-op when no quota row
// exists", but the pre-fix implementation inserted the quota_credits row
// unconditionally (the used_bytes UPDATE just affected 0 rows). That orphan
// credit outlived the unlimited period: after a later `quota set`, a
// re-upload/re-verify of the same oid hit the stale credit (n == 0) and never
// incremented used_bytes — a durable under-count until manual reconcile.
//
// These tests pin the fixed semantics:
//   1. Add with no quota row leaves no credit and touches no counter.
//   2. upload-while-unlimited → set → re-verify the same oid counts the
//      object exactly once (no under-count, no double-count).
//   3. Set creating a fresh row sweeps pre-existing orphan credits (the
//      residue path for deployments that carried rows written by the buggy
//      Add), so the next verify of a previously-credited oid re-counts once.

// countCredits returns the number of quota_credits rows for the tenant.
func countCredits(t *testing.T, db sqlitestore.Querier, tenant string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM quota_credits WHERE tenant = ?`, tenant).Scan(&n); err != nil {
		t.Fatalf("count quota_credits for %q: %v", tenant, err)
	}
	return n
}

func TestAdd_UnlimitedLeavesNoCredit_U10(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()

	// Add for a tenant with no quota row: must be a clean no-op — no error,
	// no credit row, no quota row materialized.
	if err := svc.Add(ctx, "acme", "oid-unlimited", 512); err != nil {
		t.Fatalf("Add with no quota row: %v, want nil", err)
	}
	if n := countCredits(t, db, "acme"); n != 0 {
		t.Errorf("after Add with no quota row: quota_credits rows=%d, want 0 (orphan credit is the U-10 under-count seed)", n)
	}
	st, err := svc.Get(ctx, "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.Exists {
		t.Errorf("Add with no quota row must not materialize a quota row; Get.Exists=true")
	}
}

func TestAdd_UploadWhileUnlimited_Set_ReverifyCountsOnce_U10(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()

	// 1. Object uploaded + verified while the tenant is unlimited: Add is a
	//    no-op and leaves no credit behind.
	if err := svc.Add(ctx, "acme", "oidA", 120); err != nil {
		t.Fatalf("Add while unlimited: %v", err)
	}
	if n := countCredits(t, db, "acme"); n != 0 {
		t.Fatalf("precondition: unlimited-period Add must leave no credit; got %d", n)
	}

	// 2. Operator sets a quota for the tenant.
	if err := svc.Set(ctx, "acme", 1<<20); err != nil {
		t.Fatalf("Set: %v", err)
	}
	st, err := svc.Get(ctx, "acme")
	if err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	if st.UsedBytes != 0 {
		t.Fatalf("after Set: UsedBytes=%d, want 0 (Set must not double-count)", st.UsedBytes)
	}

	// 3. Re-upload/re-verify of the same oid: the fresh verify must count the
	//    object exactly once — no stale credit may suppress the increment.
	if err := svc.Add(ctx, "acme", "oidA", 120); err != nil {
		t.Fatalf("Add after Set: %v", err)
	}
	st, _ = svc.Get(ctx, "acme")
	if st.UsedBytes != 120 {
		t.Errorf("after re-verify: UsedBytes=%d, want 120 (U-10 under-count if 0)", st.UsedBytes)
	}

	// 4. A second verify replay of the same oid must not double-count (per-oid
	//    idempotency is preserved for limited tenants).
	if err := svc.Add(ctx, "acme", "oidA", 120); err != nil {
		t.Fatalf("Add replay: %v", err)
	}
	st, _ = svc.Get(ctx, "acme")
	if st.UsedBytes != 120 {
		t.Errorf("after replay: UsedBytes=%d, want 120 (double-count)", st.UsedBytes)
	}
	if n := countCredits(t, db, "acme"); n != 1 {
		t.Errorf("after re-verify: quota_credits rows=%d, want 1", n)
	}
}

func TestSet_SweepsOrphanCredits_U10(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()

	// Simulate residue from the buggy Add (or a raw import): a quota_credits
	// row exists for a tenant that never had a quota row.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO quota_credits (tenant, oid, bytes, recorded_at)
		VALUES ('acme', 'orphan-oid', 512, 1)`); err != nil {
		t.Fatalf("seed orphan credit: %v", err)
	}
	if n := countCredits(t, db, "acme"); n != 1 {
		t.Fatalf("precondition: want 1 orphan credit, got %d", n)
	}

	// Set creating a fresh row sweeps the orphans and starts UsedBytes at 0;
	// reconcile remains the authoritative re-sync for historical bytes.
	if err := svc.Set(ctx, "acme", 1<<20); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if n := countCredits(t, db, "acme"); n != 0 {
		t.Errorf("after Set: quota_credits rows=%d, want 0 (orphans swept at row creation)", n)
	}
	st, err := svc.Get(ctx, "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.UsedBytes != 0 {
		t.Errorf("after Set: UsedBytes=%d, want 0", st.UsedBytes)
	}

	// The previously-orphaned oid now re-credits exactly once on its next
	// verify.
	if err := svc.Add(ctx, "acme", "orphan-oid", 512); err != nil {
		t.Fatalf("Add after sweep: %v", err)
	}
	st, _ = svc.Get(ctx, "acme")
	if st.UsedBytes != 512 {
		t.Errorf("after re-credit: UsedBytes=%d, want 512", st.UsedBytes)
	}
}

// TestSet_UpdatePreservesCredits pins that the sweep above only happens when
// Set creates a NEW row: re-running `quota set` on an existing row must keep
// the live credits (deleting them would let a re-verify double-count bytes
// already reflected in used_bytes).
func TestSet_UpdatePreservesCredits_U10(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	if err := svc.Set(ctx, "acme", 1<<20); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := svc.Add(ctx, "acme", "oidA", 100); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Operator tightens the limit.
	if err := svc.Set(ctx, "acme", 1<<19); err != nil {
		t.Fatalf("Set update: %v", err)
	}
	st, err := svc.Get(ctx, "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.LimitBytes != 1<<19 {
		t.Errorf("LimitBytes=%d, want %d", st.LimitBytes, int64(1<<19))
	}
	if st.UsedBytes != 100 {
		t.Errorf("UsedBytes=%d, want 100 (Set update must not reset usage)", st.UsedBytes)
	}
	if n := countCredits(t, db, "acme"); n != 1 {
		t.Errorf("after Set update: quota_credits rows=%d, want 1 (credits preserved)", n)
	}
	// Replay still idempotent.
	if err := svc.Add(ctx, "acme", "oidA", 100); err != nil {
		t.Fatalf("Add replay: %v", err)
	}
	st, _ = svc.Get(ctx, "acme")
	if st.UsedBytes != 100 {
		t.Errorf("after replay: UsedBytes=%d, want 100", st.UsedBytes)
	}
}
