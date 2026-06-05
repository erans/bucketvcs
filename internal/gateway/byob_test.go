package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// errResolver always returns the configured error from Resolve.
type errResolver struct{ err error }

func (r *errResolver) Resolve(_ context.Context, _ string) (storage.ObjectStore, error) {
	return nil, r.err
}

// tenantRouter routes "tenant-a" to storeA and all other tenants to storeB.
type tenantRouter struct {
	storeA storage.ObjectStore
	storeB storage.ObjectStore
}

func (g *tenantRouter) Resolve(_ context.Context, tenant string) (storage.ObjectStore, error) {
	if tenant == "tenant-a" {
		return g.storeA, nil
	}
	return g.storeB, nil
}

// TestByob_ResolverError_InfoRefs verifies that when StoreResolver returns an
// error, GET /info/refs?service=git-upload-pack returns 500.
func TestByob_ResolverError_InfoRefs(t *testing.T) {
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// permissiveAuthStore accepts any credential — needed because the auth
	// middleware runs before handleInfoRefs and must not 401 the request.
	srv, err := NewServer(store, Options{
		MirrorDir:     t.TempDir(),
		Version:       "test",
		AuthStore:     newPermissiveAuthStore(t, "acme", "demo"),
		StoreResolver: &errResolver{err: errors.New("byob: tenant has no storage configured")},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	for _, svc := range []string{"git-upload-pack", "git-receive-pack"} {
		t.Run(svc, func(t *testing.T) {
			req, _ := http.NewRequest("GET",
				ts.URL+"/acme/demo.git/info/refs?service="+svc, nil)
			req.Header.Set("Git-Protocol", "version=2")
			// SetBasicAuth so the auth middleware passes the request through.
			req.SetBasicAuth("any", "any")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("%s: status=%d, want 500; body=%q", svc, resp.StatusCode, body)
			}
		})
	}
}

// TestByob_ResolverError_UploadPack verifies that when StoreResolver returns an
// error, POST git-upload-pack returns 500.
func TestByob_ResolverError_UploadPack(t *testing.T) {
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	srv, err := NewServer(store, Options{
		MirrorDir:     t.TempDir(),
		Version:       "test",
		AuthStore:     newPermissiveAuthStore(t, "acme", "demo"),
		StoreResolver: &errResolver{err: errors.New("byob: tenant has no storage configured")},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("POST",
		ts.URL+"/acme/demo.git/git-upload-pack", http.NoBody)
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", "version=2")
	req.SetBasicAuth("any", "any")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500; body=%q", resp.StatusCode, body)
	}
}

// TestByob_ResolverError_ReceivePack verifies that when StoreResolver returns
// an error, POST git-receive-pack returns 500.
func TestByob_ResolverError_ReceivePack(t *testing.T) {
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	srv, err := NewServer(store, Options{
		MirrorDir:     t.TempDir(),
		Version:       "test",
		AuthStore:     newPermissiveAuthStore(t, "acme", "demo"),
		StoreResolver: &errResolver{err: errors.New("byob: tenant has no storage configured")},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("POST",
		ts.URL+"/acme/demo.git/git-receive-pack", http.NoBody)
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.SetBasicAuth("perm", "perm")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500; body=%q", resp.StatusCode, body)
	}
}

// TestByob_NilResolver_BackwardCompat verifies that when StoreResolver is nil
// (non-BYOB mode), the gateway behaves exactly as before: GET info/refs for a
// non-existent repo returns 404 (same as the pre-BYOB code path).
func TestByob_NilResolver_BackwardCompat(t *testing.T) {
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// StoreResolver intentionally omitted (nil).
	srv, err := NewServer(store, Options{
		MirrorDir: t.TempDir(),
		Version:   "test",
		AuthStore: newAnonymousTestAuthStore(t, "acme", "demo", true),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// No repo imported: expect 404, not 500. Proves s.store is used unchanged.
	resp, err := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil resolver: status=%d, want 404; body=%q", resp.StatusCode, body)
	}
}

// TestByob_TenantRouter verifies that a BYOB resolver can route two tenants to
// two different stores. tenant-a's repo exists in storeA; acme's repo exists
// in storeB. A request for tenant-a routed correctly to storeA should 200.
func TestByob_TenantRouter(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dirA := t.TempDir()
	dirB := t.TempDir()

	// Import repos BEFORE opening the stores (makeRepoInStore opens+closes internally).
	makeRepoInStore(t, dirA, "tenant-a", "demo")
	makeRepoInStore(t, dirB, "acme", "demo")

	// Open the stores after makeRepoInStore has released its locks.
	storeA, err := localfs.Open(dirA)
	if err != nil {
		t.Fatalf("localfs.Open A: %v", err)
	}
	t.Cleanup(func() { _ = storeA.Close() })
	storeB, err := localfs.Open(dirB)
	if err != nil {
		t.Fatalf("localfs.Open B: %v", err)
	}
	t.Cleanup(func() { _ = storeB.Close() })

	router := &tenantRouter{storeA: storeA, storeB: storeB}

	// Use storeA as the "default" store; the router overrides it per-tenant.
	srv, err := NewServer(storeA, Options{
		MirrorDir:     t.TempDir(),
		Version:       "test",
		AuthStore:     newAnonymousTestAuthStore(t, "tenant-a", "demo", true),
		StoreResolver: router,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// tenant-a/demo lives in storeA → resolver returns storeA → 200 expected.
	req, _ := http.NewRequest("GET",
		ts.URL+"/tenant-a/demo.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET tenant-a: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tenant-a: status=%d, want 200; body=%q", resp.StatusCode, body)
	}
}
