package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// TestUploadPack_RejectsShallowFetch verifies that depth-bounded fetches are
// refused with a clear error rather than silently served as full packs (which
// would corrupt the client's history view). M3 advertises fetch=shallow at
// the capability level but the gateway has no shallow-info plumbing yet.
func TestUploadPack_RejectsShallowFetch(t *testing.T) {
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

	// Need a valid (reachable) want OID for this test; resolve refs/heads/main
	// in the mirror by issuing an info/refs first.
	resp, err := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("info/refs: %v", err)
	}
	advert, _ := io.ReadAll(resp.Body)
	idx := bytes.Index(advert, []byte(" refs/heads/main"))
	if idx < 0 {
		t.Fatalf("info/refs missing main: %q", advert)
	}
	mainOID := string(advert[idx-40 : idx])

	body := pktBody(
		dataLine("command=fetch\n"),
		delim,
		dataLine("want "+mainOID+"\n"),
		dataLine("deepen 1\n"),
		dataLine("done\n"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-upload-pack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", "version=2")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("shallow fetch: status %d, want 400", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(got, []byte("shallow/deepen")) {
		t.Fatalf("expected shallow rejection message, got %q", got)
	}
}

// TestUploadPack_RejectsExistingButUnreachableObject covers the security
// case behind the want-reachability check: an OID that physically exists in
// the mirror's object store but is NOT reachable from any advertised ref
// (e.g. left over from a deleted branch, or smuggled in via a hash-object
// write) must NOT be servable. We trigger a clone first to materialize the
// mirror, then write a loose blob directly into bare/ via git hash-object,
// then attempt to fetch its OID and verify the gateway refuses with 400.
func TestUploadPack_RejectsExistingButUnreachableObject(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	mirrorDir := t.TempDir()
	srv, _ := NewServer(store, Options{MirrorDir: mirrorDir, Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// Materialize the mirror by performing a real clone. After this returns
	// the bare dir at mirrorDir/acme/demo/bare exists.
	dst := t.TempDir() + "/clone.git"
	cmd := exec.Command("git", "clone", "--bare", "-c", "protocol.version=2", ts.URL+"/acme/demo.git", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v\n%s", err, out)
	}
	bareDir := filepath.Join(mirrorDir, "acme", "demo", "bare")
	if _, err := os.Stat(bareDir); err != nil {
		t.Fatalf("bare dir not materialized at %s: %v", bareDir, err)
	}

	// Write an unreachable loose blob into the mirror. The OID exists but
	// no ref points to it, so it must NOT be packable from the want list.
	hashCmd := exec.Command("git", "hash-object", "-w", "--stdin")
	hashCmd.Dir = bareDir
	hashCmd.Stdin = bytes.NewReader([]byte("secret unreachable contents\n"))
	hashOut, err := hashCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hash-object: %v\n%s", err, hashOut)
	}
	hiddenOID := strings.TrimSpace(string(hashOut))
	if len(hiddenOID) != 40 {
		t.Fatalf("unexpected hash-object output: %q", hashOut)
	}

	body := pktBody(
		dataLine("command=fetch\n"),
		delim,
		dataLine("want "+hiddenOID+"\n"),
		dataLine("done\n"),
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
		t.Fatalf("hidden-object fetch: status %d, want 400", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	// The blob is rejected by the kind-must-be-commit-or-tag check before
	// the rev-list reachability probe; the security guarantee is the same
	// (the OID is never serviced) and the response is a clean 400.
	if !bytes.Contains(got, []byte("want must be a commit or tag")) && !bytes.Contains(got, []byte("not our ref")) {
		t.Fatalf("unexpected reject reason: %q", got)
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
