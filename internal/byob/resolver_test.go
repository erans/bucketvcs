package byob_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/byob"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func openLocalfsStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	lfs, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lfs.Close() })
	return lfs
}

func openDB(t *testing.T) *sqlitestore.Store {
	t.Helper()
	s, err := sqlitestore.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedBinding(t *testing.T, db *sqlitestore.Store, tenant, storeURL string, key []byte) {
	t.Helper()
	plain := []byte(`{}`)
	enc, err := byob.Encrypt(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if err := db.UpsertStorageBinding(context.Background(), sqlitestore.StorageBinding{
		Tenant: tenant, StoreURL: storeURL, Provider: "localfs",
		CredsJSON: enc, CreatedAt: now, UpdatedAt: now, VerifiedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResolverFallsBackToOperator(t *testing.T) {
	db := openDB(t)
	operator := openLocalfsStore(t)
	key := make([]byte, 32)

	r := byob.NewResolver(byob.ResolverConfig{
		AuthDB:    db,
		Operator:  operator,
		EncKey:    key,
		CredsTTL:  time.Hour,
		OpenStore: func(url string, creds []byte) (storage.ObjectStore, error) { return openLocalfsStore(t), nil },
	})
	got, err := r.Resolve(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != operator {
		t.Fatal("expected operator store for unbound tenant")
	}
}

func TestResolverReturnsTenantStore(t *testing.T) {
	db := openDB(t)
	operator := openLocalfsStore(t)
	tenantStore := openLocalfsStore(t)
	key := make([]byte, 32)
	seedBinding(t, db, "acme", "localfs:/tmp/x", key)

	opens := 0
	r := byob.NewResolver(byob.ResolverConfig{
		AuthDB: db, Operator: operator, EncKey: key, CredsTTL: time.Hour,
		OpenStore: func(url string, creds []byte) (storage.ObjectStore, error) {
			opens++
			return tenantStore, nil
		},
	})
	got, err := r.Resolve(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != tenantStore {
		t.Fatal("expected tenant store")
	}
	// Second call must use cache.
	r.Resolve(context.Background(), "acme")
	if opens != 1 {
		t.Fatalf("OpenStore called %d times, want 1 (cached)", opens)
	}
}

func TestResolverInvalidateClearsCache(t *testing.T) {
	db := openDB(t)
	key := make([]byte, 32)
	seedBinding(t, db, "acme", "localfs:/tmp/x", key)

	calls := 0
	r := byob.NewResolver(byob.ResolverConfig{
		AuthDB: db, Operator: openLocalfsStore(t), EncKey: key, CredsTTL: time.Hour,
		OpenStore: func(url string, creds []byte) (storage.ObjectStore, error) {
			calls++
			return openLocalfsStore(t), nil
		},
	})
	r.Resolve(context.Background(), "acme")
	r.Invalidate("acme")
	r.Resolve(context.Background(), "acme")
	if calls != 2 {
		t.Fatalf("expected 2 opens after Invalidate, got %d", calls)
	}
}

func TestResolverConcurrentSameTenant(t *testing.T) {
	db := openDB(t)
	key := make([]byte, 32)
	seedBinding(t, db, "acme", "localfs:/tmp/x", key)

	var mu sync.Mutex
	opens := 0
	r := byob.NewResolver(byob.ResolverConfig{
		AuthDB: db, Operator: openLocalfsStore(t), EncKey: key, CredsTTL: time.Hour,
		OpenStore: func(url string, creds []byte) (storage.ObjectStore, error) {
			mu.Lock()
			opens++
			mu.Unlock()
			return openLocalfsStore(t), nil
		},
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Resolve(context.Background(), "acme") }()
	}
	wg.Wait()
	if opens != 1 {
		t.Fatalf("OpenStore called %d times under concurrent load, want 1", opens)
	}
}

func TestResolverTTLExpiry(t *testing.T) {
	db := openDB(t)
	key := make([]byte, 32)
	seedBinding(t, db, "acme", "localfs:/tmp/x", key)

	now := time.Now()
	calls := 0
	r := byob.NewResolver(byob.ResolverConfig{
		AuthDB: db, Operator: openLocalfsStore(t), EncKey: key, CredsTTL: time.Minute,
		Now: func() time.Time { return now },
		OpenStore: func(url string, creds []byte) (storage.ObjectStore, error) {
			calls++
			return openLocalfsStore(t), nil
		},
	})
	r.Resolve(context.Background(), "acme") // warm
	// Advance past TTL.
	now = now.Add(2 * time.Minute)
	r.Resolve(context.Background(), "acme") // must re-open
	if calls != 2 {
		t.Fatalf("expected 2 opens after TTL expiry, got %d", calls)
	}
}
