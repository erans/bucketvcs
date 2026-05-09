package uploadpack

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
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
	"github.com/bucketvcs/bucketvcs/internal/v2proto"
)

func TestService_RejectsNonV2(t *testing.T) {
	req := &EngineRequest{ProtocolVersion: 0, Stdin: strings.NewReader("")}
	err := Service(req)
	if !errors.Is(err, ErrV2Required) {
		t.Fatalf("got %v, want ErrV2Required", err)
	}
}

func TestService_LsRefs_DelegatesToV2Proto(t *testing.T) {
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

	// Build a pkt-line input: command=ls-refs DELIM symrefs ref-prefix refs/heads/ FLUSH
	// Using the same encoding helpers as gateway tests (reimplemented locally).
	var body bytes.Buffer
	writePktLine := func(s string) {
		n := len(s) + 4
		body.WriteByte(hexNibbleUP(byte(n >> 12)))
		body.WriteByte(hexNibbleUP(byte(n >> 8 & 0xf)))
		body.WriteByte(hexNibbleUP(byte(n >> 4 & 0xf)))
		body.WriteByte(hexNibbleUP(byte(n & 0xf)))
		body.WriteString(s)
	}
	writePktLine("command=ls-refs\n")
	body.WriteString("0001") // delim
	writePktLine("symrefs\n")
	writePktLine("ref-prefix refs/heads/\n")
	body.WriteString("0000") // flush

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	var out bytes.Buffer
	req := &EngineRequest{
		Ctx:             context.Background(),
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           &body,
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}
	// The ls-refs response must contain the main branch ref.
	if !bytes.Contains(out.Bytes(), []byte("refs/heads/main")) {
		t.Fatalf("ls-refs response missing refs/heads/main: %q", out.Bytes())
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

func hexNibbleUP(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + (n - 10)
}

func TestAdvertise_V0_RefAdvertisementShape(t *testing.T) {
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

	// The V0 output must NOT begin with the Smart-HTTP service preamble.
	// That preamble is HTTP-specific framing emitted by the gateway adapter;
	// the transport-neutral engine starts directly with the ref advertisement.
	servicePreamble := []byte("001e# service=git-upload-pack\n0000")
	if bytes.HasPrefix(buf.Bytes(), servicePreamble) {
		t.Fatalf("Advertise must not emit service preamble; it is HTTP-only framing: %q", buf.Bytes())
	}

	// The output must begin with a valid pkt-line ref entry.  The repo has a
	// HEAD pointing at refs/heads/main, so expect: "<oid> HEAD\x00<caps>".
	// A pkt-line ref line starts with a 4-hex-digit length prefix followed by
	// a 40-hex-digit OID, a space, and the ref name.
	output := buf.Bytes()
	if len(output) < 4 {
		t.Fatalf("output too short: %q", output)
	}
	// After the 4-byte pkt-line length the next 40 bytes are the OID.
	// We just verify the first non-pktlen byte is a hex digit (OID start),
	// not '#' (which would indicate a service preamble).
	if output[4] == '#' {
		t.Fatalf("first pkt-line payload starts with '#'; service preamble must not be in engine output: %q", output)
	}
	// The ref advertisement must contain HEAD with capabilities.
	if !bytes.Contains(output, []byte(" HEAD\x00")) {
		t.Fatalf("V0 advertisement missing HEAD line: %q", output)
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
