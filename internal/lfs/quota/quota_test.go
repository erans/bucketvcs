package quota_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
)

// openTestDB returns a fresh in-memory authdb (via sqlitestore.Open)
// with all migrations applied. We use the same Open() path as
// production so we get WAL/foreign-key/driver behaviour the real
// service sees.
//
// Note: relies on sqlitestore.Open() calling db.SetMaxOpenConns(1) so
// the same ":memory:" instance is reused across queries (the modernc
// sqlite driver gives each connection its own private in-memory DB,
// so MaxOpenConns=1 is what keeps these tests coherent). If that
// invariant ever changes, switch this helper to a file-backed temp
// DB instead.
func openTestDB(t *testing.T) sqlitestore.Querier {
	t.Helper()
	store, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store.DB()
}

func TestService_SetGetClear(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()

	// Get on missing tenant: Exists=false, no error.
	got, err := svc.Get(ctx, "acme")
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if got.Exists {
		t.Errorf("Get missing: Exists=true, want false")
	}

	// Set creates a row.
	if err := svc.Set(ctx, "acme", 100<<30); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err = svc.Get(ctx, "acme")
	if err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	if !got.Exists {
		t.Fatalf("after Set: Exists=false")
	}
	if got.LimitBytes != 100<<30 {
		t.Errorf("LimitBytes=%d, want %d", got.LimitBytes, int64(100)<<30)
	}
	if got.UsedBytes != 0 {
		t.Errorf("UsedBytes=%d, want 0", got.UsedBytes)
	}

	// Set is idempotent / updating.
	if err := svc.Set(ctx, "acme", 200<<30); err != nil {
		t.Fatalf("Set update: %v", err)
	}
	got, _ = svc.Get(ctx, "acme")
	if got.LimitBytes != 200<<30 {
		t.Errorf("after Set update: LimitBytes=%d, want %d", got.LimitBytes, int64(200)<<30)
	}

	// Clear removes the row.
	if err := svc.Clear(ctx, "acme"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	got, _ = svc.Get(ctx, "acme")
	if got.Exists {
		t.Errorf("after Clear: Exists=true, want false")
	}
}

func TestService_List(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	if err := svc.Set(ctx, "acme", 100); err != nil {
		t.Fatalf("Set acme: %v", err)
	}
	if err := svc.Set(ctx, "beta", 200); err != nil {
		t.Fatalf("Set beta: %v", err)
	}
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(list)=%d, want 2", len(list))
	}
	tenants := map[string]int64{}
	for _, s := range list {
		tenants[s.Tenant] = s.LimitBytes
	}
	if tenants["acme"] != 100 || tenants["beta"] != 200 {
		t.Errorf("List = %+v, want acme=100 beta=200", tenants)
	}
}

func TestService_SetRejectsNegativeLimit(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	if err := svc.Set(context.Background(), "acme", -1); err == nil {
		t.Errorf("Set with negative limit returned nil; want error")
	}
}

func TestService_CheckBatch_NoRowIsUnlimited(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	// No row for acme → unlimited → any size accepted.
	if err := svc.CheckBatch(ctx, "acme", 1<<60); err != nil {
		t.Errorf("CheckBatch no-row: %v, want nil", err)
	}
}

func TestService_CheckBatch_Boundaries(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	if err := svc.Set(ctx, "acme", 100); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// At exactly the limit (0 used, 100 requested, limit 100): accept.
	if err := svc.CheckBatch(ctx, "acme", 100); err != nil {
		t.Errorf("CheckBatch at boundary: %v, want nil", err)
	}
	// One byte over: reject.
	err := svc.CheckBatch(ctx, "acme", 101)
	var qerr *quota.QuotaError
	if !errors.As(err, &qerr) {
		t.Fatalf("CheckBatch over boundary: %T %v, want *QuotaError", err, err)
	}
	if qerr.Tenant != "acme" || qerr.CurrentBytes != 0 ||
		qerr.LimitBytes != 100 || qerr.RequestedBytes != 101 {
		t.Errorf("QuotaError fields = %+v", qerr)
	}
}

func TestService_AddIncrementsUsed(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	_ = svc.Set(ctx, "acme", 1000)
	if err := svc.Add(ctx, "acme", "oid1", 50); err != nil {
		t.Fatalf("Add oid1: %v", err)
	}
	if err := svc.Add(ctx, "acme", "oid2", 75); err != nil {
		t.Fatalf("Add oid2: %v", err)
	}
	got, _ := svc.Get(ctx, "acme")
	if got.UsedBytes != 125 {
		t.Errorf("UsedBytes=%d, want 125", got.UsedBytes)
	}
}

func TestService_AddIsIdempotentWithinRing(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	_ = svc.Set(ctx, "acme", 1000)
	// Same (tenant, oid) added twice — must count once.
	_ = svc.Add(ctx, "acme", "oid1", 50)
	_ = svc.Add(ctx, "acme", "oid1", 50)
	got, _ := svc.Get(ctx, "acme")
	if got.UsedBytes != 50 {
		t.Errorf("UsedBytes=%d, want 50 (idempotent on duplicate oid)", got.UsedBytes)
	}
}

func TestService_AddNoOpWhenNoRow(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	// No row → Add is a no-op (no error).
	if err := svc.Add(ctx, "acme", "oid1", 50); err != nil {
		t.Errorf("Add with no row: %v, want nil", err)
	}
}

func TestService_SubtractFloorsAtZero(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	_ = svc.Set(ctx, "acme", 1000)
	_ = svc.Add(ctx, "acme", "oid1", 50)
	// Subtract more than used → floor at 0, not negative.
	if err := svc.Subtract(ctx, "acme", "oid1", 200); err != nil {
		t.Fatalf("Subtract: %v", err)
	}
	got, _ := svc.Get(ctx, "acme")
	if got.UsedBytes != 0 {
		t.Errorf("UsedBytes=%d, want 0 (floored)", got.UsedBytes)
	}
}

func TestService_SubtractNoOpWhenNoRow(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	if err := svc.Subtract(context.Background(), "acme", "oid1", 50); err != nil {
		t.Errorf("Subtract with no row: %v, want nil", err)
	}
}

func TestService_AddDoubleCountsAfterRingEviction(t *testing.T) {
	// Pins the documented bound from design spec §6.2: beyond the
	// ring's capacity (1024 entries), a re-Add of an already-seen
	// (tenant, oid) is no longer deduplicated. Reconcile is the
	// safety net for this drift. This test exists so a future cap
	// change or LRU policy change is noticed.
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	_ = svc.Set(ctx, "acme", 1<<30)

	// First Add lands.
	const firstOID = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := svc.Add(ctx, "acme", firstOID, 10); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	// Fill the ring with 1024 distinct OIDs so the first one evicts.
	for i := 1; i <= 1024; i++ {
		oid := fmt.Sprintf("%064x", i)
		if err := svc.Add(ctx, "acme", oid, 1); err != nil {
			t.Fatalf("Add filler %d: %v", i, err)
		}
	}
	// Re-Add the original — it has been evicted, so it counts again.
	if err := svc.Add(ctx, "acme", firstOID, 10); err != nil {
		t.Fatalf("Add evicted: %v", err)
	}
	got, _ := svc.Get(ctx, "acme")
	// First (10) + 1024 fillers (1024) + re-added evicted (10) = 1044.
	if got.UsedBytes != 1044 {
		t.Errorf("UsedBytes=%d, want 1044 (10 first + 1024 fillers + 10 re-added after eviction)", got.UsedBytes)
	}
}

func TestService_AddRejectsNegativeBytes(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	if err := svc.Add(context.Background(), "acme", "oid", -1); err == nil {
		t.Errorf("Add with negative bytes returned nil; want error")
	}
}

func TestService_SubtractRejectsNegativeBytes(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	if err := svc.Subtract(context.Background(), "acme", "oid", -1); err == nil {
		t.Errorf("Subtract with negative bytes returned nil; want error")
	}
}

func TestService_CheckBatch_DoesNotOverflow(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	// Limit is small but used is near MaxInt64. The naive `used+req >
	// limit` compare would overflow to negative and silently accept.
	// Set used_bytes manually via the underlying DB (since the
	// public API floors at 0 on Subtract).
	_ = svc.Set(ctx, "acme", 100)
	if _, err := db.ExecContext(ctx, `UPDATE quotas SET used_bytes = ? WHERE tenant = ?`,
		int64(math.MaxInt64-10), "acme"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := svc.CheckBatch(ctx, "acme", 1000)
	if err == nil {
		t.Errorf("CheckBatch under overflow accepted; want *QuotaError")
	}
	var qerr *quota.QuotaError
	if !errors.As(err, &qerr) {
		t.Errorf("err type=%T; want *QuotaError", err)
	}
}

func TestService_CheckBatch_RejectsNegativeRequest(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	_ = svc.Set(context.Background(), "acme", 100)
	if err := svc.CheckBatch(context.Background(), "acme", -1); err == nil {
		t.Errorf("CheckBatch with negative request accepted; want error")
	}
}
