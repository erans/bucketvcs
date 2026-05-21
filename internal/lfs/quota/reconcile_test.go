package quota_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestReconcile_DryRunDoesNotWrite(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	_ = svc.Set(ctx, "acme", 1000)
	_ = svc.Add(ctx, "acme", "fake-oid", 500) // counter at 500

	store, err := localfs.Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()
	// Seed storage with a single 7-byte LFS object for tenant acme.
	prefix := lfs.RepoLFSPrefix("acme", "demo")
	oid := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := store.PutIfAbsent(ctx, prefix+oid, bytes.NewReader([]byte("7-bytes")), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rep, err := svc.Reconcile(ctx, store, "acme", true /*dryRun*/)
	if err != nil {
		t.Fatalf("Reconcile dry-run: %v", err)
	}
	if rep.BeforeBytes != 500 || rep.AfterBytes != 7 || rep.DriftBytes != -493 || !rep.DryRun {
		t.Errorf("dry-run report = %+v, want before=500 after=7 drift=-493 dryRun=true", rep)
	}
	// Counter is unchanged.
	got, _ := svc.Get(ctx, "acme")
	if got.UsedBytes != 500 {
		t.Errorf("dry-run wrote counter: UsedBytes=%d, want 500", got.UsedBytes)
	}
}

func TestReconcile_WritesCounter(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	_ = svc.Set(ctx, "acme", 1000)
	_ = svc.Add(ctx, "acme", "fake-oid", 500)

	store, _ := localfs.Open(filepath.Join(t.TempDir(), "store"))
	defer store.Close()
	prefix := lfs.RepoLFSPrefix("acme", "demo")
	oid := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, _ = store.PutIfAbsent(ctx, prefix+oid, bytes.NewReader([]byte("7-bytes")), nil)

	rep, err := svc.Reconcile(ctx, store, "acme", false)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.AfterBytes != 7 {
		t.Errorf("AfterBytes=%d, want 7", rep.AfterBytes)
	}
	got, _ := svc.Get(ctx, "acme")
	if got.UsedBytes != 7 {
		t.Errorf("after Reconcile: UsedBytes=%d, want 7", got.UsedBytes)
	}
}

func TestReconcile_TenantWithoutRow_NoOp(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	store, _ := localfs.Open(filepath.Join(t.TempDir(), "store"))
	defer store.Close()
	rep, err := svc.Reconcile(context.Background(), store, "no-such", false)
	if err != nil {
		t.Fatalf("Reconcile no-row: %v", err)
	}
	if rep.BeforeBytes != 0 || rep.AfterBytes != 0 {
		t.Errorf("no-row report = %+v, want all zeros", rep)
	}
}

func TestReconcile_SumsAcrossRepos(t *testing.T) {
	db := openTestDB(t)
	svc := quota.New(db, nil)
	ctx := context.Background()
	_ = svc.Set(ctx, "acme", 1000)
	store, _ := localfs.Open(filepath.Join(t.TempDir(), "store"))
	defer store.Close()
	put := func(repo string, oid string, body []byte) {
		_, _ = store.PutIfAbsent(ctx, lfs.RepoLFSPrefix("acme", repo)+oid, bytes.NewReader(body), nil)
	}
	put("alpha", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []byte("5byte"))
	put("beta", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", []byte("eleven-byts"))
	rep, err := svc.Reconcile(ctx, store, "acme", false)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.AfterBytes != 16 {
		t.Errorf("AfterBytes=%d, want 16 (5+11)", rep.AfterBytes)
	}
}
