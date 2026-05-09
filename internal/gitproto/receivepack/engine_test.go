package receivepack

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
	"github.com/bucketvcs/bucketvcs/internal/v2proto"
)

func TestService_FlushOnlyProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeReceivePackStore(t, storeDir, "acme", "demo")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	// Stdin = pktline flush packet "0000"
	pw := &bytes.Buffer{}
	wtr := pktline.NewWriter(pw)
	if err := wtr.WriteFlush(); err != nil {
		t.Fatalf("WriteFlush: %v", err)
	}

	var stdout bytes.Buffer
	req := &EngineRequest{
		Ctx:          context.Background(),
		Tenant:       "acme",
		Repo:         "demo",
		Stdin:        pw,
		Stdout:       &stdout,
		Stderr:       &bytes.Buffer{},
		Store:        store,
		Mirror:       mgr,
		AgentVersion: "test",
	}
	err = Service(req)
	if !errors.Is(err, ErrFlushOnlyProbe) {
		t.Fatalf("Service with flush-only body: want ErrFlushOnlyProbe, got %v", err)
	}
}

func TestStubsCompile(t *testing.T) {
	// Service is now implemented; Serve must succeed or return a non-nil error.
	// We just verify that calling Service with an empty request doesn't panic
	// and that Serve delegates to Advertise then Service.
	// (With no Store or Mirror, both will return non-nil errors — that's fine.)
	_ = Service(&EngineRequest{Ctx: context.Background()})
	_ = Serve(&EngineRequest{Ctx: context.Background()})
}

// makeReceivePackStore creates a synthetic store with one repo for use in
// receivepack package tests. It mirrors the pattern from
// internal/gitproto/uploadpack/engine_test.go::makeUploadPackStore.
func makeReceivePackStore(t *testing.T, storeDir, tenant, repoID string) {
	t.Helper()
	srcBare := filepath.Join(t.TempDir(), "src.git")
	work := filepath.Join(t.TempDir(), "wt")

	mustExecRP(t, "", "git", "init", "--bare", srcBare)
	mustExecRP(t, "", "git", "clone", srcBare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExecRP(t, work, "git", "add", ".")
	mustExecRP(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustExecRP(t, work, "git", "push", "origin", "HEAD:refs/heads/main")

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

func mustExecRP(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func TestAdvertise_V0_RefAdvertisementShape(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeReceivePackStore(t, storeDir, "acme", "demo")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	var buf bytes.Buffer
	req := &EngineRequest{
		Ctx:          context.Background(),
		Tenant:       "acme",
		Repo:         "demo",
		Stdout:       &buf,
		Store:        store,
		AgentVersion: "test",
	}
	if err := Advertise(req); err != nil {
		t.Fatalf("Advertise: %v", err)
	}

	output := buf.Bytes()

	// Output must NOT begin with the Smart-HTTP service preamble.
	// That preamble is HTTP-specific framing emitted by the gateway adapter.
	if bytes.Contains(output, []byte("# service=")) {
		t.Fatalf("Advertise must not emit service preamble; it is HTTP-only framing: %q", output)
	}

	// Output must contain the main branch ref.
	if !bytes.Contains(output, []byte("refs/heads/main")) {
		t.Fatalf("output missing refs/heads/main: %q", output)
	}

	// Output must end with a flush packet "0000".
	if !strings.HasSuffix(strings.TrimRight(string(output), ""), "0000") {
		// Check raw bytes: last 4 bytes should be '0','0','0','0'
		if len(output) < 4 || string(output[len(output)-4:]) != "0000" {
			t.Fatalf("output does not end with flush packet '0000': %q", output)
		}
	}

	// Capability list must contain "report-status", "delete-refs", and agent.
	if !bytes.Contains(output, []byte("report-status")) {
		t.Fatalf("output missing capability 'report-status': %q", output)
	}
	if !bytes.Contains(output, []byte("delete-refs")) {
		t.Fatalf("output missing capability 'delete-refs': %q", output)
	}
	agentCap := "agent=" + v2proto.AgentName + "/test"
	if !bytes.Contains(output, []byte(agentCap)) {
		t.Fatalf("output missing agent capability %q: %q", agentCap, output)
	}

	// Receive-pack must NOT advertise HEAD (push targets are real refs).
	if bytes.Contains(output, []byte(" HEAD\x00")) || bytes.Contains(output, []byte(" HEAD\n")) {
		t.Fatalf("receive-pack must not advertise HEAD: %q", output)
	}
}
