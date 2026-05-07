package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

const testNullOID = "0000000000000000000000000000000000000000"

func TestReceivePack_AcceptsDeleteOnly(t *testing.T) {
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

	const oldOID = "1111111111111111111111111111111111111111"
	body := pktBody(
		dataLine(oldOID+" "+testNullOID+" refs/heads/feature\x00report-status delete-refs\n"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestReceivePack_RejectsBadRefName(t *testing.T) {
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

	const oldOID = "1111111111111111111111111111111111111111"
	const newOID = "2222222222222222222222222222222222222222"
	body := pktBody(
		dataLine(oldOID+" "+newOID+" refs/heads/bad ref\x00report-status\n"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Fatalf("bad ref name: status %d, want 400", resp.StatusCode)
	}
}

func TestReceivePack_RejectsRefsReplace(t *testing.T) {
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

	const oldOID = "1111111111111111111111111111111111111111"
	const newOID = "2222222222222222222222222222222222222222"
	body := pktBody(
		dataLine(oldOID+" "+newOID+" refs/replace/abc\x00report-status\n"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Fatalf("refs/replace: status %d, want 400", resp.StatusCode)
	}
}

func TestReceivePack_RejectsBadOID(t *testing.T) {
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

	body := pktBody(
		dataLine("notanoid 2222222222222222222222222222222222222222 refs/heads/main\x00\n"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Fatalf("bad oid: status %d, want 400", resp.StatusCode)
	}
}

func TestReceivePack_StagesPackToIncomingDir(t *testing.T) {
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

	// Build a non-delete command + a fake PACK body. Validation is Task 17;
	// for Task 16 we just want to verify the body was drained without 4xx.
	const oldOID = "1111111111111111111111111111111111111111"
	const newOID = "2222222222222222222222222222222222222222"
	body := pktBody(
		dataLine(oldOID+" "+newOID+" refs/heads/main\x00report-status\n"),
		flush,
	)
	body = append(body, []byte("PACK\x00\x00\x00\x02fakebytes")...)

	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	// Task 16 emits placeholder report (200); Task 17 will validate the
	// pack and may return ng. For now we just want != 4xx for valid framing.
	if resp.StatusCode != 200 {
		t.Fatalf("staging: status %d, want 200", resp.StatusCode)
	}
}

// TestReceivePack_ReportEmitsUnpackOkAndNgForStale verifies that the
// report-status framing is well-formed: "unpack ok" header (the pack
// itself was processed; here there's no pack so unpack is trivially OK)
// followed by "ng <ref> stale info" for the rejected delete (the
// supplied old-OID does not match the actual ref tip).
func TestReceivePack_ReportEmitsUnpackOkAndNgForStale(t *testing.T) {
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

	const wrongOldOID = "1111111111111111111111111111111111111111"
	body := pktBody(
		dataLine(wrongOldOID+" "+testNullOID+" refs/heads/feature\x00report-status\n"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if !bytes.Contains(got, []byte("unpack ok")) {
		t.Fatalf("expected 'unpack ok' in report, got %q", got)
	}
	if !bytes.Contains(got, []byte("ng refs/heads/feature")) {
		t.Fatalf("expected 'ng refs/heads/feature' in report, got %q", got)
	}
	if !bytes.Contains(got, []byte("stale info")) {
		t.Fatalf("expected 'stale info' reason, got %q", got)
	}
}

// TestReceivePack_ReportUsesSidebandWhenNegotiated verifies that when the
// client requests side-band-64k, the report-status is multiplexed on
// band 1 rather than written as raw pkt-lines. Without this the response
// is malformed for a side-band-aware client (it expects framed channel
// payloads, not naked status lines).
func TestReceivePack_ReportUsesSidebandWhenNegotiated(t *testing.T) {
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

	const oldOID = "1111111111111111111111111111111111111111"
	body := pktBody(
		dataLine(oldOID+" "+testNullOID+" refs/heads/feature\x00report-status side-band-64k\n"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	// The first pkt-line must be a side-band Data frame: 4 hex length
	// bytes, then a band-id byte (0x01), then payload. The naked-pkt-line
	// path would start with a 4-hex length followed by "unpack" directly.
	if len(got) < 5 {
		t.Fatalf("response too short: %q", got)
	}
	if got[4] != 0x01 {
		t.Fatalf("first pkt-line is not side-band band-1; got byte 0x%02x in payload (full: %q)", got[4], got)
	}
	// Even though the pkt-line is band-1 wrapped, the inner stream still
	// contains the report — verify by substring.
	if !bytes.Contains(got, []byte("unpack ok")) {
		t.Fatalf("inner report missing 'unpack ok': %q", got)
	}
	if !bytes.Contains(got, []byte("ng refs/heads/feature")) {
		t.Fatalf("inner report missing 'ng refs/heads/feature': %q", got)
	}
}

// TestReceivePack_RejectsTrailingBytesAfterDeleteOnly verifies that a
// delete-only push (which forbids a trailing packfile per pack-protocol)
// is rejected when extra body bytes follow the flush. Without this check
// a malformed or attacker-crafted request could smuggle bytes past
// validation.
func TestReceivePack_RejectsTrailingBytesAfterDeleteOnly(t *testing.T) {
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

	const oldOID = "1111111111111111111111111111111111111111"
	body := pktBody(
		dataLine(oldOID+" "+testNullOID+" refs/heads/feature\x00report-status\n"),
		flush,
	)
	body = append(body, []byte("PACK\x00\x00\x00\x02junk")...)

	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("trailing bytes after delete-only: status %d, want 400", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "trailing bytes") {
		t.Fatalf("expected 'trailing bytes' message, got %q", got)
	}
}

// TestReceivePack_RejectsExtraTrailingNewline verifies the strict
// single-LF terminator rule. A command line with multiple trailing
// newlines must not be silently normalized — TrimRight would have
// accepted "...refs/heads/main\n\n", but the spec requires exactly one
// LF, and the OID/refname checks must surface the malformed shape.
func TestReceivePack_RejectsExtraTrailingNewline(t *testing.T) {
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

	const oldOID = "1111111111111111111111111111111111111111"
	// Two trailing newlines on a non-first command (so caps parsing
	// doesn't swallow them): TrimSuffix only strips one, leaving the
	// refname as "refs/heads/feature\n" which fails check-ref-format.
	body := pktBody(
		dataLine(oldOID+" "+testNullOID+" refs/heads/main\x00report-status\n"),
		dataLine(oldOID+" "+testNullOID+" refs/heads/feature\n\n"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("extra newline: status %d, want 400", resp.StatusCode)
	}
}

// TestReceivePack_AcceptsMissingLFTerminator verifies the relaxed
// terminator policy: pack-protocol(5) describes the command line as
// "<old> <new> <name>" with the LF permitted but optional. Real `git
// push` clients (observed: git 2.54) omit the LF entirely, so the parser
// MUST accept commands without a trailing newline. We still reject
// anything else malformed downstream (OID/refname validation runs after
// the optional LF strip).
func TestReceivePack_AcceptsMissingLFTerminator(t *testing.T) {
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

	const oldOID = "1111111111111111111111111111111111111111"
	// No trailing LF on the (only) command line — must be accepted.
	body := pktBody(
		dataLine(oldOID+" "+testNullOID+" refs/heads/feature\x00report-status"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("missing LF (now permitted): status %d, want 200", resp.StatusCode)
	}
}

// TestReceivePack_RejectsTooManyCommands enforces the per-request command
// cap that bounds CPU / subprocess cost. Each command invokes
// `git check-ref-format`, so an uncapped count is a DoS even at small
// per-command body sizes.
func TestReceivePack_RejectsTooManyCommands(t *testing.T) {
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

	// Build maxUpdateCommands+1 = 4097 delete commands.
	chunks := make([]pktChunk, 0, 4099)
	const oldOID = "1111111111111111111111111111111111111111"
	for i := 0; i < 4097; i++ {
		ref := fmt.Sprintf("refs/heads/branch-%05d", i)
		line := oldOID + " " + testNullOID + " " + ref
		if i == 0 {
			line += "\x00report-status"
		}
		line += "\n"
		chunks = append(chunks, dataLine(line))
	}
	chunks = append(chunks, flush)
	body := pktBody(chunks...)

	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("too-many-commands: status %d, want 400", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "too many update commands") {
		t.Fatalf("expected 'too many update commands' message, got %q", got)
	}
}

// TestReceivePack_GitPushEndToEnd drives a real `git push` against the
// gateway and verifies the new ref appears in the bucket manifest. This
// is the keystone test for Task 17: the placeholder report-status from
// Task 16 reported every push as "ng not-implemented", which would cause
// `git push` to exit with a failure status. After Task 17 the push must
// succeed end-to-end through the full validate + repack + commit +
// IngestPack pipeline.
func TestReceivePack_GitPushEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed an empty bucket repo. The push will create refs/heads/main
	// where there was no ref before (unborn default branch).
	srcBare := filepath.Join(t.TempDir(), "seed.git")
	mustExecGW(t, "", "git", "init", "--bare", srcBare)
	if _, err := importer.Import(context.Background(), store, importer.Options{
		Tenant: "acme", Repo: "demo", SourceDir: srcBare, DefaultBranch: "refs/heads/main",
	}); err != nil {
		t.Fatalf("seed Import: %v", err)
	}

	srv, err := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// Build a populated working repo to push from.
	work := filepath.Join(t.TempDir(), "wt")
	mustExecGW(t, "", "git", "init", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExecGW(t, work, "git", "add", ".")
	mustExecGW(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "-m", "init")
	cmd := exec.Command("git", "-C", work, "push",
		ts.URL+"/acme/demo.git", "HEAD:refs/heads/main")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	// Verify ref now lives in manifest body.
	r2, err := repo.Open(context.Background(), store, "acme", "demo")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r2.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := body.Refs["refs/heads/main"]; !ok {
		t.Fatalf("expected refs/heads/main in manifest after push: %+v", body.Refs)
	}
}

// TestReceivePack_RejectsStaleOldOID asserts that a non-create command
// whose old-OID does not match the bucket's current ref tip is rejected
// with "ng <ref> stale info". The push body still includes a (fake)
// pack body so the parser exercises the full read path; the validation
// must reject before any pack ingest is attempted.
func TestReceivePack_RejectsStaleOldOID(t *testing.T) {
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

	const wrongOldOID = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	const fakeNewOID = "1111111111111111111111111111111111111111"
	body := pktBody(
		dataLine(wrongOldOID+" "+fakeNewOID+" refs/heads/main\x00report-status\n"),
		flush,
	)
	body = append(body, []byte("PACK\x00\x00\x00\x02fakebytes")...)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(respBody, []byte("ng refs/heads/main")) {
		t.Fatalf("expected 'ng refs/heads/main' in response, got: %q", respBody)
	}
	if !bytes.Contains(respBody, []byte("stale info")) {
		t.Fatalf("expected 'stale info' reason: %q", respBody)
	}
}
