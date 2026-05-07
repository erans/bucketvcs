package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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
