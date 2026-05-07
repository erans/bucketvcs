package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// makeRepoInStore imports a tiny synthetic repo into the storeDir.
func makeRepoInStore(t *testing.T, storeDir, tenant, repoID string) {
	t.Helper()
	srcBare := filepath.Join(t.TempDir(), "src.git")
	work := filepath.Join(t.TempDir(), "wt")

	mustExecGW(t, "", "git", "init", "--bare", srcBare)
	mustExecGW(t, "", "git", "clone", srcBare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExecGW(t, work, "git", "add", ".")
	mustExecGW(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustExecGW(t, work, "git", "push", "origin", "HEAD:refs/heads/main")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()
	if _, err := importer.Import(context.Background(), store, importer.Options{
		Tenant: tenant, Repo: repoID, SourceDir: srcBare, DefaultBranch: "refs/heads/main",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
}

func mustExecGW(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func TestInfoRefs_V2UploadPack(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv, err := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("GET", ts.URL+"/acme/demo.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/x-git-upload-pack-advertisement" {
		t.Fatalf("Content-Type: %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("version 2")) {
		t.Fatalf("body missing 'version 2': %q", body)
	}
	if !bytes.Contains(body, []byte("ls-refs=unborn")) {
		t.Fatalf("body missing 'ls-refs=unborn': %q", body)
	}
}

func TestInfoRefs_ReceivePackV0(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-receive-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/x-git-receive-pack-advertisement" {
		t.Fatalf("Content-Type: %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("# service=git-receive-pack")) {
		t.Fatalf("missing service header: %q", body)
	}
	if !bytes.Contains(body, []byte("report-status")) {
		t.Fatalf("missing capability: %q", body)
	}
	if !bytes.Contains(body, []byte("refs/heads/main")) {
		t.Fatalf("missing ref: %q", body)
	}
}

func TestInfoRefs_V0UploadPackFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// No Git-Protocol: version=2 header → v0 fallback.
	resp, err := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if bytes.Contains(body, []byte("version 2")) {
		t.Fatalf("v0 fallback unexpectedly contained 'version 2': %q", body)
	}
	if !bytes.Contains(body, []byte("# service=git-upload-pack")) {
		t.Fatalf("missing service header in v0 fallback: %q", body)
	}
	if !bytes.Contains(body, []byte("multi_ack_detailed")) {
		t.Fatalf("missing v0 capability: %q", body)
	}
}

func TestInfoRefs_RejectsUnknownService(t *testing.T) {
	storeDir := t.TempDir()
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	resp, _ := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-evil-pack")
	if resp.StatusCode != 400 {
		t.Fatalf("unknown service: status %d, want 400", resp.StatusCode)
	}
}

func TestInfoRefs_RepoNotFound(t *testing.T) {
	storeDir := t.TempDir()
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	resp, _ := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-upload-pack")
	if resp.StatusCode != 404 {
		t.Fatalf("not-found: status %d, want 404", resp.StatusCode)
	}
}
