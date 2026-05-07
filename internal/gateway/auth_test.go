package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newTestServerWithAuth(t *testing.T, mode AuthMode, token string) *httptest.Server {
	t.Helper()
	store, _ := localfs.Open(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	srv, err := NewServer(store, Options{
		MirrorDir: t.TempDir(),
		Version:   "test",
		AuthMode:  mode,
		AuthToken: token,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func TestAuth_AnonymousMode_AllowsBoth(t *testing.T) {
	ts := newTestServerWithAuth(t, AuthAnonymous, "")
	for _, svc := range []string{"git-upload-pack", "git-receive-pack"} {
		resp, err := http.Get(ts.URL + "/acme/demo.git/info/refs?service=" + svc)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		// 501 (stub) is acceptable — we just want != 401.
		if resp.StatusCode == 401 {
			t.Fatalf("svc=%s: got 401 in anonymous mode", svc)
		}
	}
}

func TestAuth_WriteOnlyMode_AllowsAnonRead_RejectsAnonWrite(t *testing.T) {
	ts := newTestServerWithAuth(t, AuthWriteOnly, "secret")
	resp, _ := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-upload-pack")
	if resp.StatusCode == 401 {
		t.Fatalf("anon read in write-only mode: 401")
	}
	resp, _ = http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-receive-pack")
	if resp.StatusCode != 401 {
		t.Fatalf("anon write in write-only mode: got %d, want 401", resp.StatusCode)
	}
}

func TestAuth_AllMode_RequiresTokenForBoth(t *testing.T) {
	ts := newTestServerWithAuth(t, AuthAll, "secret")
	for _, svc := range []string{"git-upload-pack", "git-receive-pack"} {
		resp, _ := http.Get(ts.URL + "/acme/demo.git/info/refs?service=" + svc)
		if resp.StatusCode != 401 {
			t.Fatalf("svc=%s anon: got %d, want 401", svc, resp.StatusCode)
		}
	}
	for _, svc := range []string{"git-upload-pack", "git-receive-pack"} {
		req, _ := http.NewRequest("GET", ts.URL+"/acme/demo.git/info/refs?service="+svc, nil)
		req.SetBasicAuth("bucketvcs", "secret")
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode == 401 {
			t.Fatalf("svc=%s with token: 401", svc)
		}
	}
}

func TestAuth_AllMode_RejectsWrongToken(t *testing.T) {
	ts := newTestServerWithAuth(t, AuthAll, "secret")
	req, _ := http.NewRequest("GET", ts.URL+"/acme/demo.git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("bucketvcs", "wrong")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("wrong token: got %d, want 401", resp.StatusCode)
	}
}

func TestAuth_AllMode_RejectsWrongUser(t *testing.T) {
	ts := newTestServerWithAuth(t, AuthAll, "secret")
	req, _ := http.NewRequest("GET", ts.URL+"/acme/demo.git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("notbucketvcs", "secret")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("wrong user: got %d, want 401", resp.StatusCode)
	}
}

func TestAuth_AllMode_PostReceivePackAlsoRequiresToken(t *testing.T) {
	ts := newTestServerWithAuth(t, AuthAll, "secret")
	resp, _ := http.Post(ts.URL+"/acme/demo.git/git-receive-pack", "application/octet-stream", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("POST receive-pack anon: got %d, want 401", resp.StatusCode)
	}
}
