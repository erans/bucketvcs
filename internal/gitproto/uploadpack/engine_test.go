package uploadpack

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
	"github.com/bucketvcs/bucketvcs/internal/v2proto"
)

func TestStubsCompile(t *testing.T) {
	if err := Service(&EngineRequest{}); err == nil {
		t.Fatal("expected ErrNotImplemented from Service stub")
	}
}

// makeUploadPackStore creates a synthetic store with one repo for use in
// uploadpack package tests. It mirrors the pattern from
// internal/gateway/inforefs_test.go::makeRepoInStore without importing
// gateway test code.
func makeUploadPackStore(t *testing.T, storeDir, tenant, repoID string) {
	t.Helper()
	srcBare := filepath.Join(t.TempDir(), "src.git")
	work := filepath.Join(t.TempDir(), "wt")

	mustExecUP(t, "", "git", "init", "--bare", srcBare)
	mustExecUP(t, "", "git", "clone", srcBare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExecUP(t, work, "git", "add", ".")
	mustExecUP(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustExecUP(t, work, "git", "push", "origin", "HEAD:refs/heads/main")

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

func mustExecUP(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func TestAdvertise_V0_EmitsServicePreamble(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeUploadPackStore(t, storeDir, "acme", "demo")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	var buf bytes.Buffer
	req := &EngineRequest{
		Ctx:             context.Background(),
		Tenant:          "acme",
		Repo:            "demo",
		Stdout:          &buf,
		ProtocolVersion: 0,
		Store:           store,
		AgentVersion:    "test",
	}
	if err := Advertise(req); err != nil {
		t.Fatalf("Advertise: %v", err)
	}

	// The pkt-line for "# service=git-upload-pack\n" must appear first.
	// pkt-line format: 4-hex-digit length (includes itself) + payload.
	// len("# service=git-upload-pack\n") = 26, + 4 = 30 = 0x1e → "001e"
	want := []byte("001e# service=git-upload-pack\n0000")
	if !bytes.Contains(buf.Bytes(), want) {
		t.Fatalf("output missing service preamble %q; got:\n%q", want, buf.Bytes())
	}
	if !bytes.HasPrefix(buf.Bytes(), want) {
		t.Fatalf("service preamble not at start of output; got:\n%q", buf.Bytes())
	}
}

func TestAdvertise_V2_DelegatesToV2Proto(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeUploadPackStore(t, storeDir, "acme", "demo")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	var buf bytes.Buffer
	req := &EngineRequest{
		Ctx:             context.Background(),
		Tenant:          "acme",
		Repo:            "demo",
		Stdout:          &buf,
		ProtocolVersion: 2,
		Store:           store,
		AgentVersion:    "0.0.0",
	}
	if err := Advertise(req); err != nil {
		t.Fatalf("Advertise: %v", err)
	}

	// Compare against the reference output from v2proto directly.
	var ref bytes.Buffer
	if err := v2proto.WriteV2Advertisement(&ref, "git-upload-pack", "0.0.0"); err != nil {
		t.Fatalf("WriteV2Advertisement: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), ref.Bytes()) {
		t.Fatalf("V2 output does not match v2proto reference.\ngot: %q\nwant: %q", buf.Bytes(), ref.Bytes())
	}
}
