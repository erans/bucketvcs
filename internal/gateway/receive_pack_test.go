package gateway

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// TestReceivePack_ReportSignalsNotImplemented verifies the placeholder
// report-status emits "unpack ng" and per-ref "ng" so a real Git client
// surfaces the push as failed (commit logic lands in Task 17). HTTP is
// 200 because report-status framing requires a 2xx response.
func TestReceivePack_ReportSignalsNotImplemented(t *testing.T) {
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
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if !bytes.Contains(got, []byte("unpack not-implemented")) {
		t.Fatalf("expected 'unpack not-implemented' in report, got %q", got)
	}
	if !bytes.Contains(got, []byte("ng refs/heads/feature")) {
		t.Fatalf("expected 'ng refs/heads/feature' in report, got %q", got)
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
	if !bytes.Contains(got, []byte("unpack not-implemented")) {
		t.Fatalf("inner report missing 'unpack not-implemented': %q", got)
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

// TestReceivePack_RejectsMissingLFTerminator verifies the strict
// terminator rule from the other direction: pack-protocol(5) requires
// each command to end with exactly one LF, and a frame with no
// terminator at all must be rejected rather than silently accepted.
func TestReceivePack_RejectsMissingLFTerminator(t *testing.T) {
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
	// No trailing LF on the (only) command line.
	body := pktBody(
		dataLine(oldOID+" "+testNullOID+" refs/heads/feature\x00report-status"),
		flush,
	)
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("missing LF: status %d, want 400", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "missing LF") {
		t.Fatalf("expected 'missing LF' message, got %q", got)
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
