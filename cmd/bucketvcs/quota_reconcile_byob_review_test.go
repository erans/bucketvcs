package main

import (
	"context"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

type fixedQuotaResolver struct {
	store storage.ObjectStore
	err   error
}

func (r fixedQuotaResolver) Resolve(context.Context, string) (storage.ObjectStore, error) {
	return r.store, r.err
}

func TestQuotaReconcile_UsesBYOBTenantStore_Review(t *testing.T) {
	ctx := context.Background()
	authS, err := sqlitestore.Open(t.TempDir() + "/auth.db")
	if err != nil {
		t.Fatal(err)
	}
	defer authS.Close()
	svc := quota.New(authS.DB(), nil)
	if err := svc.Set(ctx, "acme", 1<<20); err != nil {
		t.Fatal(err)
	}
	operator, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	tenantStore, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer tenantStore.Close()
	const key = "tenants/acme/repos/app/lfs/objects/aa/aabb"
	if _, err := tenantStore.PutIfAbsent(ctx, key, strings.NewReader("tenant-bytes"), nil); err != nil {
		t.Fatal(err)
	}

	rep, err := reconcileQuotaForTenant(ctx, svc, fixedQuotaResolver{store: tenantStore}, operator, "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AfterBytes != int64(len("tenant-bytes")) {
		t.Fatalf("AfterBytes = %d, want %d", rep.AfterBytes, len("tenant-bytes"))
	}
	state, err := svc.Get(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if state.UsedBytes != rep.AfterBytes {
		t.Fatalf("used bytes = %d, want %d", state.UsedBytes, rep.AfterBytes)
	}
}
