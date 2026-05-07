package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestUploadPack_GitCloneEndToEnd(t *testing.T) {
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

	dst := t.TempDir() + "/clone.git"
	cmd := exec.Command("git", "clone", "--bare", "-c", "protocol.version=2", ts.URL+"/acme/demo.git", dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "HEAD")); err != nil {
		t.Fatalf("expected HEAD in clone: %v", err)
	}
}

func TestUploadPack_LsRefsOverV2(t *testing.T) {
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

	// Pkt-line body: command=ls-refs DELIM symrefs ref-prefix refs/heads/ FLUSH.
	body := pktBody(
		dataLine("command=ls-refs\n"),
		delim,
		dataLine("symrefs\n"),
		dataLine("ref-prefix refs/heads/\n"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-upload-pack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(got, []byte("refs/heads/main")) {
		t.Fatalf("ls-refs response missing main: %q", got)
	}
}

func TestUploadPack_RejectsMissingV2Header(t *testing.T) {
	storeDir := t.TempDir()
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, _ := http.Post(ts.URL+"/acme/demo.git/git-upload-pack", "application/x-git-upload-pack-request", bytes.NewReader([]byte("0000")))
	if resp.StatusCode != 400 {
		t.Fatalf("missing v2 header: status %d, want 400", resp.StatusCode)
	}
}

// TestUploadPack_RejectsUnreachableWant exercises the want-reachability check
// added to address roborev's high-severity finding: a client that names an
// OID NOT reachable from any advertised ref must be refused, even if the
// object happens to exist in the mirror's pack files.
func TestUploadPack_RejectsUnreachableWant(t *testing.T) {
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

	// A syntactically valid but non-existent OID — the reachability check
	// rejects it via the cat-file kind probe (object missing). The plumbing
	// for "exists but unreachable" requires bypassing the manifest layer;
	// the response shape we care about here is "fetch: not our ref ...".
	bogus := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	body := pktBody(
		dataLine("command=fetch\n"),
		delim,
		dataLine("want "+bogus+"\n"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-upload-pack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("unreachable want: status %d, want 400", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(got, []byte("not our ref")) {
		t.Fatalf("expected 'not our ref' message, got %q", got)
	}
}

// helpers
type pktChunk []byte

func dataLine(s string) pktChunk {
	n := len(s) + 4
	return pktChunk(append([]byte{
		hexNibbleTest(byte(n >> 12)),
		hexNibbleTest(byte(n >> 8 & 0xf)),
		hexNibbleTest(byte(n >> 4 & 0xf)),
		hexNibbleTest(byte(n & 0xf)),
	}, s...))
}

var (
	flush pktChunk = []byte("0000")
	delim pktChunk = []byte("0001")
)

func pktBody(chunks ...pktChunk) []byte {
	var buf bytes.Buffer
	for _, c := range chunks {
		buf.Write(c)
	}
	return buf.Bytes()
}

func hexNibbleTest(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + (n - 10)
}
