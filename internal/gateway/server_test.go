package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv, err := NewServer(store, Options{
		MirrorDir: t.TempDir(),
		Version:   "0.1-test",
		AuthStore: newAnonymousTestAuthStore(t, "acme", "demo", true),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func TestServer_Healthz(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
}

func TestServer_Banner(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestServer_RejectsBadTenantOrRepo(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	for _, path := range []string{
		"/.git/info/refs",
		"/foo/.git/info/refs",
		"/foo/with space.git/info/refs",
		"/..%2Fetc/x.git/info/refs",
	} {
		resp, err := http.Get(ts.URL + path + "?service=git-upload-pack")
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != 400 && resp.StatusCode != 404 {
			t.Fatalf("path %s: status %d, want 400 or 404", path, resp.StatusCode)
		}
	}
}

func TestServer_UnknownPathReturns404(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	resp, _ := http.Get(ts.URL + "/foo/bar.git/info/wat")
	if resp.StatusCode != 404 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}
