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
	"github.com/bucketvcs/bucketvcs/internal/repo"
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

func TestInfoRefs_V0UploadPack_AdvertisesHEADAndSymref(t *testing.T) {
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

	resp, err := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(" HEAD\x00")) {
		t.Fatalf("v0 upload-pack missing HEAD advertisement: %q", body)
	}
	if !bytes.Contains(body, []byte("symref=HEAD:refs/heads/main")) {
		t.Fatalf("v0 upload-pack missing symref capability: %q", body)
	}
	hi := bytes.Index(body, []byte(" HEAD\x00"))
	ri := bytes.Index(body, []byte("refs/heads/main\n"))
	if hi < 0 || ri < 0 || hi >= ri {
		t.Fatalf("HEAD must precede refs/heads/main: HEAD@%d ref@%d body=%q", hi, ri, body)
	}
}

func TestInfoRefs_V0ReceivePack_NoHEADLine(t *testing.T) {
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
	body, _ := io.ReadAll(resp.Body)
	if bytes.Contains(body, []byte(" HEAD\x00")) || bytes.Contains(body, []byte(" HEAD\n")) {
		t.Fatalf("v0 receive-pack should not advertise HEAD: %q", body)
	}
	if bytes.Contains(body, []byte("symref=")) {
		t.Fatalf("v0 receive-pack should not advertise symref: %q", body)
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

func TestWantsV2(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"version=1", false},
		{"version=2", true},
		{"version=2:other=foo", true},
		{"other=foo:version=2", true},
		{" version=2 ", true},
		{"version=20", false},
	}
	for _, c := range cases {
		if got := wantsV2(c.header); got != c.want {
			t.Errorf("wantsV2(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

func TestInfoRefs_V2WithExtraProtocolTokens(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("GET", ts.URL+"/acme/demo.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Git-Protocol", "version=2:object-format=sha1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("version 2")) {
		t.Fatalf("colon-list Git-Protocol downgraded to v0: %q", body)
	}
}

func TestNewServer_RejectsBadVersion(t *testing.T) {
	storeDir := t.TempDir()
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	for _, bad := range []string{"1.0\n", "1.0\r2", "with space", "x\x00y"} {
		if _, err := NewServer(store, Options{MirrorDir: t.TempDir(), Version: bad}); err == nil {
			t.Errorf("NewServer accepted bad Version %q", bad)
		}
	}
}

// TestInfoRefs_V0UploadPack_UnbornDefaultBranch_AdvertisesSymref ensures that
// when a repo has DefaultBranch set but the target ref does not yet exist
// (unborn), the v0 upload-pack advertisement still includes the
// symref=HEAD:<default> capability so v0 clients can discover the intended
// remote default branch on first clone/fetch.
func TestInfoRefs_V0UploadPack_UnbornDefaultBranch_AdvertisesSymref(t *testing.T) {
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := repo.Create(context.Background(), store, "acme", "demo", repo.CreateOptions{
		DefaultBranch: "refs/heads/trunk",
	}); err != nil {
		t.Fatalf("repo.Create: %v", err)
	}

	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("symref=HEAD:refs/heads/trunk")) {
		t.Fatalf("unborn default branch must still advertise symref capability: %q", body)
	}
	// No HEAD line because the ref is unborn — emit capabilities^{} stub.
	if !bytes.Contains(body, []byte("capabilities^{}")) {
		t.Fatalf("expected capabilities^{} stub for unborn-only repo: %q", body)
	}
	if bytes.Contains(body, []byte(" HEAD\x00")) {
		t.Fatalf("must not advertise HEAD line for unborn default: %q", body)
	}
}
