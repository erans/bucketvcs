package uploadpack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
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
	if err := v2proto.WriteV2Advertisement(&ref, "git-upload-pack", "0.0.0", v2proto.CapsOptions{}); err != nil {
		t.Fatalf("WriteV2Advertisement: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), ref.Bytes()) {
		t.Fatalf("V2 output does not match v2proto reference.\ngot: %q\nwant: %q", buf.Bytes(), ref.Bytes())
	}
}

// TestAdvertise_V2_BundleURI_Cap verifies that setting BundleURIEnabled=true
// causes the bundle-uri capability to appear in the v2 advertisement.
func TestAdvertise_V2_BundleURI_Cap(t *testing.T) {
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
		Ctx:              context.Background(),
		Tenant:           "acme",
		Repo:             "demo",
		Stdout:           &buf,
		ProtocolVersion:  2,
		Store:            store,
		AgentVersion:     "0.0.0",
		BundleURIEnabled: true,
	}
	if err := Advertise(req); err != nil {
		t.Fatalf("Advertise: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("bundle-uri")) {
		t.Fatalf("expected bundle-uri cap in advertisement, got: %q", buf.Bytes())
	}

	// A second request with BundleURIEnabled=false must NOT carry the cap.
	var buf2 bytes.Buffer
	req2 := &EngineRequest{
		Ctx:              context.Background(),
		Tenant:           "acme",
		Repo:             "demo",
		Stdout:           &buf2,
		ProtocolVersion:  2,
		Store:            store,
		AgentVersion:     "0.0.0",
		BundleURIEnabled: false,
	}
	if err := Advertise(req2); err != nil {
		t.Fatalf("Advertise (disabled): %v", err)
	}
	if bytes.Contains(buf2.Bytes(), []byte("bundle-uri")) {
		t.Fatalf("bundle-uri cap must be absent when BundleURIEnabled=false, got: %q", buf2.Bytes())
	}
}

// buildBundleURIPktLine builds the pkt-line encoding of a single-command request.
// format: command=bundle-uri\n DELIM FLUSH
func buildBundleURIRequest() []byte {
	var body bytes.Buffer
	writePkt := func(s string) {
		n := len(s) + 4
		body.WriteByte(hexNibbleUP(byte(n >> 12)))
		body.WriteByte(hexNibbleUP(byte(n >> 8 & 0xf)))
		body.WriteByte(hexNibbleUP(byte(n >> 4 & 0xf)))
		body.WriteByte(hexNibbleUP(byte(n & 0xf)))
		body.WriteString(s)
	}
	writePkt("command=bundle-uri\n")
	body.WriteString("0001") // delim
	body.WriteString("0000") // flush
	return body.Bytes()
}

// TestService_BundleURI_Disabled writes command=bundle-uri when BundleURIEnabled
// is false. The dispatch arm is always present; when disabled, serveBundleURI
// produces an empty response (flush-pkt only) rather than a 400 bad-request.
func TestService_BundleURI_Disabled(t *testing.T) {
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

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	var out bytes.Buffer
	req := &EngineRequest{
		Ctx:              context.Background(),
		Tenant:           "acme",
		Repo:             "demo",
		Stdin:            bytes.NewReader(buildBundleURIRequest()),
		Stdout:           &out,
		Stderr:           &bytes.Buffer{},
		ProtocolVersion:  2,
		Store:            store,
		Mirror:           mgr,
		AgentVersion:     "test",
		BundleURIEnabled: false, // disabled: no BuildURL configured
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service returned error (expected empty response): %v", err)
	}
	// Empty bundle-uri response is just a flush-pkt ("0000").
	if out.String() != "0000" {
		t.Fatalf("expected flush-only response, got: %q", out.String())
	}
}

// TestService_BundleURI_NoBundles verifies that with BundleURIEnabled=true but
// no bundles in the manifest, the response is an empty flush.
func TestService_BundleURI_NoBundles(t *testing.T) {
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

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	buildURLCalled := false
	var out bytes.Buffer
	req := &EngineRequest{
		Ctx:             context.Background(),
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           bytes.NewReader(buildBundleURIRequest()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		BundleURIEnabled: true,
		BundleWarmCommits: 5000,
		BundleWarmAge:     24 * time.Hour,
		BundleURIBuildURL: func(_ context.Context, _, _, _ string) (string, error) {
			buildURLCalled = true
			return "https://example.com/bundle", nil
		},
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}
	// No bundles in the manifest → HandleBundleURI returns empty response.
	if out.String() != "0000" {
		t.Fatalf("expected flush-only response when no bundles exist, got: %q", out.String())
	}
	if buildURLCalled {
		t.Fatalf("BuildURL should not be called when no bundles exist")
	}
}

// TestService_BundleURI_Advertises verifies that with BundleURIEnabled=true,
// a current bundle in the manifest, and a working BuildURL, the response
// contains bundle.<id>.uri=.
func TestService_BundleURI_Advertises(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	// makeUploadPackStore sets up a repo with refs/heads/main pointing at a real commit.
	makeUploadPackStore(t, storeDir, "acme", "demo")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	// Look up the current tip OID of refs/heads/main so we can set
	// the bundle TipOID to match (making it "current").
	ctx := context.Background()
	r, openErr := repo.Open(ctx, store, "acme", "demo")
	if openErr != nil {
		t.Fatalf("repo.Open: %v", openErr)
	}
	view, viewErr := r.ReadRoot(ctx)
	if viewErr != nil {
		t.Fatalf("r.ReadRoot: %v", viewErr)
	}
	var body manifest.Body
	if jsonErr := json.Unmarshal(view.Body, &body); jsonErr != nil {
		t.Fatalf("unmarshal: %v", jsonErr)
	}
	tipOID, ok := body.Refs["refs/heads/main"]
	if !ok {
		t.Fatalf("refs/heads/main not found in manifest")
	}

	// Write a current bundle entry into the manifest. BundleHash uses a
	// well-formed 64-char lowercase hex body so the bundleHashHex path
	// is exercised end-to-end (the BuildURL closure below asserts the
	// expected hash threads through correctly).
	const bundleHashHex = "deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	entry := manifest.BundleEntry{
		ID: "b1", Kind: "full_default", Ref: "refs/heads/main",
		TipOID:      tipOID,
		GeneratedAt: time.Now().Add(-time.Minute).Format(time.RFC3339),
		BundleHash:  "sha256-" + bundleHashHex,
		BundleKey:   "bundles/test.bundle",
	}
	if casErr := maintenance.RunBundleCASMerge(ctx, r, entry, "test", 1); casErr != nil {
		t.Fatalf("RunBundleCASMerge: %v", casErr)
	}

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
		Stdin:           bytes.NewReader(buildBundleURIRequest()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		BundleURIEnabled:  true,
		BundleWarmCommits: 5000,
		BundleWarmAge:     24 * time.Hour,
		BundleURIBuildURL: func(_ context.Context, hash, key, expected string) (string, error) {
			if want := "sha256:" + bundleHashHex; expected != want {
				t.Errorf("BuildURL got expectedHash=%q, want %q", expected, want)
			}
			return "https://cdn.example.com/bundle.git", nil
		},
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}
	// The response must contain bundle.b1.uri=.
	if !bytes.Contains(out.Bytes(), []byte("bundle.b1.uri=")) {
		t.Fatalf("expected bundle.b1.uri= in response, got: %q", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("https://cdn.example.com/bundle.git")) {
		t.Fatalf("expected bundle URL in response, got: %q", out.String())
	}
	// Per Git's protocol-v2 bundle-uri spec, bundle.version=1 and
	// bundle.mode=all MUST precede the per-bundle keys. Without them a
	// compliant client treats the entire list as invalid.
	if !bytes.Contains(out.Bytes(), []byte("bundle.version=1")) {
		t.Fatalf("expected bundle.version=1 header, got: %q", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("bundle.mode=all")) {
		t.Fatalf("expected bundle.mode=all header, got: %q", out.String())
	}
	if modeIdx := bytes.Index(out.Bytes(), []byte("bundle.mode=all")); modeIdx < 0 || modeIdx > bytes.Index(out.Bytes(), []byte("bundle.b1.uri=")) {
		t.Fatalf("bundle.mode=all must precede per-bundle keys; got: %q", out.String())
	}
}

// TestAdvertise_V2_PackURI_Cap verifies that setting PackURIEnabled=true
// causes the "fetch=packfile-uris" capability to appear in the v2
// advertisement (packfile-uris is a sub-feature of the fetch command,
// not a top-level cap; the client's server_supports_feature() helper
// requires this exact shape), and is absent when disabled.
func TestAdvertise_V2_PackURI_Cap(t *testing.T) {
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

	// Enabled: cap must appear.
	var bufOn bytes.Buffer
	reqOn := &EngineRequest{
		Ctx:             context.Background(),
		Tenant:          "acme",
		Repo:            "demo",
		Stdout:          &bufOn,
		ProtocolVersion: 2,
		Store:           store,
		AgentVersion:    "0.0.0",
		PackURIEnabled:  true,
	}
	if err := Advertise(reqOn); err != nil {
		t.Fatalf("Advertise (enabled): %v", err)
	}
	if !bytes.Contains(bufOn.Bytes(), []byte("fetch=packfile-uris")) {
		t.Fatalf("expected fetch=packfile-uris cap in advertisement, got: %q", bufOn.Bytes())
	}

	// Disabled: cap must be absent.
	var bufOff bytes.Buffer
	reqOff := &EngineRequest{
		Ctx:             context.Background(),
		Tenant:          "acme",
		Repo:            "demo",
		Stdout:          &bufOff,
		ProtocolVersion: 2,
		Store:           store,
		AgentVersion:    "0.0.0",
		PackURIEnabled:  false,
	}
	if err := Advertise(reqOff); err != nil {
		t.Fatalf("Advertise (disabled): %v", err)
	}
	if bytes.Contains(bufOff.Bytes(), []byte("fetch=packfile-uris")) {
		t.Fatalf("fetch=packfile-uris cap must be absent when PackURIEnabled=false, got: %q", bufOff.Bytes())
	}
}

// TestService_PackURI_AdvertisedInFetchResponse exercises the full fetch
// path with a FullPackRequested-shaped manifest and asserts that the
// response carries a "packfile-uris\n" section header followed by the
// "<sha1> <uri>\n" stanza, AND still contains the inline "packfile\n"
// section (per protocol-v2: packfile-uris does not elide the packfile
// section in Phase 8.2).
func TestService_PackURI_AdvertisedInFetchResponse(t *testing.T) {
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

	ctx := context.Background()
	r, err := repo.Open(ctx, store, "acme", "demo")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("r.ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tipOID, ok := body.Refs["refs/heads/main"]
	if !ok {
		t.Fatalf("refs/heads/main not in manifest")
	}
	if len(body.Packs) != 1 {
		t.Fatalf("expected single pack in manifest, got %d", len(body.Packs))
	}

	// Backfill PackChecksum so EvaluateFullPackRequested gate passes. The
	// gate only validates 40-hex format and does not cross-check the value
	// against the actual pack contents — using a synthetic checksum here
	// keeps the test focused on the wire-up shape.
	const fakeSha1 = "0123456789abcdef0123456789abcdef01234567"
	if _, err := r.Commit(ctx, txBody("test_packchecksum_backfill"), func(prev *repo.RootView) ([]byte, error) {
		var pb manifest.Body
		if uerr := json.Unmarshal(prev.Body, &pb); uerr != nil {
			return nil, uerr
		}
		if len(pb.Packs) != 1 {
			t.Fatalf("expected single pack inside commit cb, got %d", len(pb.Packs))
		}
		pb.Packs[0].PackChecksum = fakeSha1
		return manifest.MarshalBody(pb)
	}); err != nil {
		t.Fatalf("Commit (backfill PackChecksum): %v", err)
	}

	// Build the fetch request: command=fetch + DELIM + want <tip> +
	// done + packfile-uris=https + FLUSH. Done=true short-circuits
	// negotiation so the server emits the final response (acks if any +
	// packfile-uris if applicable + packfile + flush).
	var body2 bytes.Buffer
	writePkt := func(s string) {
		n := len(s) + 4
		body2.WriteByte(hexNibbleUP(byte(n >> 12)))
		body2.WriteByte(hexNibbleUP(byte(n >> 8 & 0xf)))
		body2.WriteByte(hexNibbleUP(byte(n >> 4 & 0xf)))
		body2.WriteByte(hexNibbleUP(byte(n & 0xf)))
		body2.WriteString(s)
	}
	writePkt("command=fetch\n")
	body2.WriteString("0001") // DELIM
	writePkt("want " + tipOID + "\n")
	writePkt("done\n")
	writePkt("packfile-uris=https\n")
	body2.WriteString("0000") // FLUSH

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	const wantURL = "https://cdn.example.com/pack-uri.pack"
	buildCalled := false
	var out bytes.Buffer
	req := &EngineRequest{
		Ctx:             ctx,
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           bytes.NewReader(body2.Bytes()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		PackURIEnabled:  true,
		PackURIBuildURL: func(_ context.Context, hash, key, expected string) (string, error) {
			buildCalled = true
			if hash != fakeSha1 {
				t.Errorf("BuildURL hash arg: got %q, want %q", hash, fakeSha1)
			}
			if key == "" {
				t.Errorf("BuildURL key arg unexpectedly empty")
			}
			return wantURL, nil
		},
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}
	if !buildCalled {
		t.Fatalf("BuildURL was not called; gate likely short-circuited unexpectedly")
	}
	// Response must contain the packfile-uris section header.
	if !bytes.Contains(out.Bytes(), []byte("packfile-uris\n")) {
		t.Fatalf("expected packfile-uris section header in response, got: %q", out.Bytes())
	}
	// And the URL stanza.
	if !bytes.Contains(out.Bytes(), []byte(wantURL)) {
		t.Fatalf("expected URL %q in response, got: %q", wantURL, out.Bytes())
	}
	// And the SHA-1 prefix on the same line.
	if !bytes.Contains(out.Bytes(), []byte(fakeSha1+" "+wantURL)) {
		t.Fatalf("expected stanza %q in response, got: %q", fakeSha1+" "+wantURL, out.Bytes())
	}
	// Per protocol-v2, the packfile section MUST still follow.
	if !bytes.Contains(out.Bytes(), []byte("packfile\n")) {
		t.Fatalf("expected packfile section to follow packfile-uris, got: %q", out.Bytes())
	}
	// Section ordering: packfile-uris must precede packfile.
	puriIdx := bytes.Index(out.Bytes(), []byte("packfile-uris\n"))
	pfIdx := bytes.Index(out.Bytes(), []byte("packfile\n"))
	if puriIdx < 0 || pfIdx < 0 || puriIdx >= pfIdx {
		t.Fatalf("packfile-uris must precede packfile; puriIdx=%d pfIdx=%d", puriIdx, pfIdx)
	}
}

// TestService_PackURI_NotAdvertisedWhenClientSilent verifies that the
// gate does not advertise (and never invokes BuildURL) when the client
// did not opt in via packfile-uris=, even with PackURIEnabled=true.
func TestService_PackURI_NotAdvertisedWhenClientSilent(t *testing.T) {
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

	ctx := context.Background()
	r, err := repo.Open(ctx, store, "acme", "demo")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("r.ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tipOID := body.Refs["refs/heads/main"]

	// Backfill PackChecksum so the only thing keeping the gate from
	// advertising is the missing client opt-in.
	if _, err := r.Commit(ctx, txBody("test_packchecksum_backfill"), func(prev *repo.RootView) ([]byte, error) {
		var pb manifest.Body
		if uerr := json.Unmarshal(prev.Body, &pb); uerr != nil {
			return nil, uerr
		}
		pb.Packs[0].PackChecksum = "0123456789abcdef0123456789abcdef01234567"
		return manifest.MarshalBody(pb)
	}); err != nil {
		t.Fatalf("Commit (backfill PackChecksum): %v", err)
	}

	var body2 bytes.Buffer
	writePkt := func(s string) {
		n := len(s) + 4
		body2.WriteByte(hexNibbleUP(byte(n >> 12)))
		body2.WriteByte(hexNibbleUP(byte(n >> 8 & 0xf)))
		body2.WriteByte(hexNibbleUP(byte(n >> 4 & 0xf)))
		body2.WriteByte(hexNibbleUP(byte(n & 0xf)))
		body2.WriteString(s)
	}
	writePkt("command=fetch\n")
	body2.WriteString("0001")
	writePkt("want " + tipOID + "\n")
	writePkt("done\n")
	// NB: no packfile-uris= line — client did not opt in.
	body2.WriteString("0000")

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	buildCalled := false
	var out bytes.Buffer
	req := &EngineRequest{
		Ctx:             ctx,
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           bytes.NewReader(body2.Bytes()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		PackURIEnabled:  true,
		PackURIBuildURL: func(_ context.Context, _, _, _ string) (string, error) {
			buildCalled = true
			return "https://cdn.example.com/pack.pack", nil
		},
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}
	if buildCalled {
		t.Fatalf("BuildURL must not be called when client did not opt in")
	}
	if bytes.Contains(out.Bytes(), []byte("packfile-uris\n")) {
		t.Fatalf("packfile-uris section must be absent when client did not opt in, got: %q", out.Bytes())
	}
	// The packfile section must still be present (this is a normal fetch).
	if !bytes.Contains(out.Bytes(), []byte("packfile\n")) {
		t.Fatalf("expected packfile section in normal fetch, got: %q", out.Bytes())
	}
}

// txBody is a tiny helper that wraps the maintenance test boilerplate of
// constructing a tx.Body{Type, Actor} pair for the Commit retry callback.
func txBody(typ string) tx.Body {
	return tx.Body{Type: typ, Actor: "u_test"}
}

// captureLogger returns a slog.Logger that writes JSON to a bytes.Buffer.
// The buffer is returned so callers can inspect emitted log lines.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// logContains returns true if any JSON-encoded log line in buf has the given key==value pair.
func logContains(buf *bytes.Buffer, key, value string) bool {
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if v, ok := m[key]; ok {
			switch vt := v.(type) {
			case string:
				if vt == value {
					return true
				}
			}
		}
	}
	return false
}

// logContainsMetric returns true if buf contains a "metric" log line with
// metric_name==name AND the given label key==value.
func logContainsMetric(buf *bytes.Buffer, name, labelKey, labelValue string) bool {
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if m["msg"] != "metric" {
			continue
		}
		if m["metric_name"] != name {
			continue
		}
		if lv, ok := m[labelKey]; ok {
			if s, ok := lv.(string); ok && s == labelValue {
				return true
			}
		}
	}
	return false
}

// TestServeBundleURI_EmitsAdvertisedMetric_NoEntry verifies that when the
// manifest has no full_default bundle entry, bundle_advertised_total with
// freshness=retired is emitted but bundle_uri_advertised_total and the
// bundle.uri.advertised audit event are NOT emitted.
func TestServeBundleURI_EmitsAdvertisedMetric_NoEntry(t *testing.T) {
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

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	var out bytes.Buffer
	var logBuf bytes.Buffer
	req := &EngineRequest{
		Ctx:             context.Background(),
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           bytes.NewReader(buildBundleURIRequest()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		Logger:          captureLogger(&logBuf),
		BundleURIEnabled: true,
		BundleWarmCommits: 5000,
		BundleWarmAge:     24 * time.Hour,
		BundleURIBuildURL: func(_ context.Context, _, _, _ string) (string, error) {
			t.Fatalf("BuildURL must not be called when no bundles exist")
			return "", nil
		},
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}

	// bundle_advertised_total{freshness=no_bundle} must be emitted.
	if !logContainsMetric(&logBuf, "bundle_advertised_total", "freshness", "no_bundle") {
		t.Errorf("expected bundle_advertised_total{freshness=no_bundle} in logs:\n%s", logBuf.String())
	}
	// bundle_uri_advertised_total must NOT be emitted.
	if logContainsMetric(&logBuf, "bundle_uri_advertised_total", "via", "direct") ||
		logContainsMetric(&logBuf, "bundle_uri_advertised_total", "via", "proxied") {
		t.Errorf("bundle_uri_advertised_total must not be emitted when no bundle advertised:\n%s", logBuf.String())
	}
	// bundle.uri.advertised audit must NOT be emitted.
	if logContains(&logBuf, "event", "bundle.uri.advertised") {
		t.Errorf("bundle.uri.advertised audit must not fire when no URI advertised:\n%s", logBuf.String())
	}
}

// TestServeBundleURI_EmitsAdvertisedMetricAndAudit_Current verifies that
// when a current full_default bundle is advertised via a proxied URL, all
// three observability outputs are emitted: bundle_advertised_total{freshness=current},
// bundle_uri_advertised_total{via=proxied}, and bundle.uri.advertised audit.
func TestServeBundleURI_EmitsAdvertisedMetricAndAudit_Current(t *testing.T) {
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

	ctx := context.Background()
	r, openErr := repo.Open(ctx, store, "acme", "demo")
	if openErr != nil {
		t.Fatalf("repo.Open: %v", openErr)
	}
	view, viewErr := r.ReadRoot(ctx)
	if viewErr != nil {
		t.Fatalf("r.ReadRoot: %v", viewErr)
	}
	var body manifest.Body
	if jsonErr := json.Unmarshal(view.Body, &body); jsonErr != nil {
		t.Fatalf("unmarshal: %v", jsonErr)
	}
	tipOID, ok := body.Refs["refs/heads/main"]
	if !ok {
		t.Fatalf("refs/heads/main not found in manifest")
	}

	const bundleHashHex = "deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	entry := manifest.BundleEntry{
		ID: "b1", Kind: "full_default", Ref: "refs/heads/main",
		TipOID:      tipOID,
		GeneratedAt: time.Now().Add(-time.Minute).Format(time.RFC3339),
		BundleHash:  "sha256-" + bundleHashHex,
		BundleKey:   "bundles/test.bundle",
	}
	if casErr := maintenance.RunBundleCASMerge(ctx, r, entry, "test", 1); casErr != nil {
		t.Fatalf("RunBundleCASMerge: %v", casErr)
	}

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	var out bytes.Buffer
	var logBuf bytes.Buffer
	req := &EngineRequest{
		Ctx:             ctx,
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           bytes.NewReader(buildBundleURIRequest()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		Logger:          captureLogger(&logBuf),
		BundleURIEnabled:  true,
		BundleWarmCommits: 5000,
		BundleWarmAge:     24 * time.Hour,
		BundleURIBuildURL: func(_ context.Context, _, _, _ string) (string, error) {
			// Proxied URL shape: contains /_bundle/
			return "https://gw.example.com/_bundle/b1?token=abc", nil
		},
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}

	// bundle must be advertised in the response.
	if !bytes.Contains(out.Bytes(), []byte("bundle.b1.uri=")) {
		t.Fatalf("expected bundle.b1.uri= in response, got: %q", out.String())
	}
	// bundle_advertised_total{freshness=current} must be emitted.
	if !logContainsMetric(&logBuf, "bundle_advertised_total", "freshness", "current") {
		t.Errorf("expected bundle_advertised_total{freshness=current}:\n%s", logBuf.String())
	}
	// bundle_uri_advertised_total{via=proxied} must be emitted.
	if !logContainsMetric(&logBuf, "bundle_uri_advertised_total", "via", "proxied") {
		t.Errorf("expected bundle_uri_advertised_total{via=proxied}:\n%s", logBuf.String())
	}
	// bundle.uri.advertised audit must be emitted.
	if !logContains(&logBuf, "event", "bundle.uri.advertised") {
		t.Errorf("expected bundle.uri.advertised audit event:\n%s", logBuf.String())
	}
}

// TestServeBundleURI_EmitsAdvertisedMetric_OnlyWhen_BuildURLEmpty verifies
// that when the freshness state is current but BuildURL returns empty string,
// bundle_advertised_total{freshness=current} is emitted but
// bundle_uri_advertised_total and the bundle.uri.advertised audit are NOT.
func TestServeBundleURI_EmitsAdvertisedMetric_OnlyWhen_BuildURLEmpty(t *testing.T) {
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

	ctx := context.Background()
	r, openErr := repo.Open(ctx, store, "acme", "demo")
	if openErr != nil {
		t.Fatalf("repo.Open: %v", openErr)
	}
	view, viewErr := r.ReadRoot(ctx)
	if viewErr != nil {
		t.Fatalf("r.ReadRoot: %v", viewErr)
	}
	var body manifest.Body
	if jsonErr := json.Unmarshal(view.Body, &body); jsonErr != nil {
		t.Fatalf("unmarshal: %v", jsonErr)
	}
	tipOID, ok := body.Refs["refs/heads/main"]
	if !ok {
		t.Fatalf("refs/heads/main not found in manifest")
	}

	entry := manifest.BundleEntry{
		ID: "b1", Kind: "full_default", Ref: "refs/heads/main",
		TipOID:      tipOID,
		GeneratedAt: time.Now().Add(-time.Minute).Format(time.RFC3339),
		BundleKey:   "bundles/test.bundle",
	}
	if casErr := maintenance.RunBundleCASMerge(ctx, r, entry, "test", 1); casErr != nil {
		t.Fatalf("RunBundleCASMerge: %v", casErr)
	}

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	var out bytes.Buffer
	var logBuf bytes.Buffer
	req := &EngineRequest{
		Ctx:             ctx,
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           bytes.NewReader(buildBundleURIRequest()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		Logger:          captureLogger(&logBuf),
		BundleURIEnabled:  true,
		BundleWarmCommits: 5000,
		BundleWarmAge:     24 * time.Hour,
		BundleURIBuildURL: func(_ context.Context, _, _, _ string) (string, error) {
			return "", nil // misconfigured backend: empty URL, nil error
		},
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}

	// Response must be empty (flush only).
	if out.String() != "0000" {
		t.Fatalf("expected flush-only response when BuildURL returns empty, got: %q", out.String())
	}
	// bundle_advertised_total{freshness=current} must still be emitted
	// (state was evaluated before BuildURL was called).
	if !logContainsMetric(&logBuf, "bundle_advertised_total", "freshness", "current") {
		t.Errorf("expected bundle_advertised_total{freshness=current} even when BuildURL returns empty:\n%s", logBuf.String())
	}
	// bundle_uri_advertised_total must NOT be emitted.
	if logContainsMetric(&logBuf, "bundle_uri_advertised_total", "via", "direct") ||
		logContainsMetric(&logBuf, "bundle_uri_advertised_total", "via", "proxied") {
		t.Errorf("bundle_uri_advertised_total must not be emitted when outcome.URI is empty:\n%s", logBuf.String())
	}
	// bundle.uri.advertised audit must NOT be emitted.
	if logContains(&logBuf, "event", "bundle.uri.advertised") {
		t.Errorf("bundle.uri.advertised audit must not fire when outcome.URI is empty:\n%s", logBuf.String())
	}
}

// TestServeFetch_EmitsPackURIAdvertisedMetric_WhenStanzaFires verifies that
// pack_uri_advertised_total{via=proxied} is emitted when the packfile-uris
// gate produces a non-empty stanza (all preconditions hold and BuildURL
// returns a proxied URL).
func TestServeFetch_EmitsPackURIAdvertisedMetric_WhenStanzaFires(t *testing.T) {
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

	ctx := context.Background()
	r, err := repo.Open(ctx, store, "acme", "demo")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("r.ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tipOID, ok := body.Refs["refs/heads/main"]
	if !ok {
		t.Fatalf("refs/heads/main not in manifest")
	}

	// Backfill PackChecksum so EvaluateFullPackRequested gate passes.
	const fakeSha1 = "0123456789abcdef0123456789abcdef01234567"
	if _, err := r.Commit(ctx, txBody("test_packchecksum_backfill"), func(prev *repo.RootView) ([]byte, error) {
		var pb manifest.Body
		if uerr := json.Unmarshal(prev.Body, &pb); uerr != nil {
			return nil, uerr
		}
		pb.Packs[0].PackChecksum = fakeSha1
		return manifest.MarshalBody(pb)
	}); err != nil {
		t.Fatalf("Commit (backfill PackChecksum): %v", err)
	}

	var body2 bytes.Buffer
	writePkt := func(s string) {
		n := len(s) + 4
		body2.WriteByte(hexNibbleUP(byte(n >> 12)))
		body2.WriteByte(hexNibbleUP(byte(n >> 8 & 0xf)))
		body2.WriteByte(hexNibbleUP(byte(n >> 4 & 0xf)))
		body2.WriteByte(hexNibbleUP(byte(n & 0xf)))
		body2.WriteString(s)
	}
	writePkt("command=fetch\n")
	body2.WriteString("0001")
	writePkt("want " + tipOID + "\n")
	writePkt("done\n")
	writePkt("packfile-uris=https\n")
	body2.WriteString("0000")

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	var out bytes.Buffer
	var logBuf bytes.Buffer
	req := &EngineRequest{
		Ctx:             ctx,
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           bytes.NewReader(body2.Bytes()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		Logger:          captureLogger(&logBuf),
		PackURIEnabled:  true,
		PackURIBuildURL: func(_ context.Context, _, _, _ string) (string, error) {
			// Proxied URL shape: path contains /_pack/
			return "https://gw.example.com/_pack/" + fakeSha1 + "?token=abc", nil
		},
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}

	// packfile-uris section must be present in the response.
	if !bytes.Contains(out.Bytes(), []byte("packfile-uris\n")) {
		t.Fatalf("expected packfile-uris section in response, got: %q", out.Bytes())
	}
	// pack_uri_advertised_total{via=proxied} must be emitted.
	if !logContainsMetric(&logBuf, "pack_uri_advertised_total", "via", "proxied") {
		t.Errorf("expected pack_uri_advertised_total{via=proxied} in logs:\n%s", logBuf.String())
	}
	// pack_uri_advertised_total{repo_id=acme/demo} must be emitted.
	if !logContainsMetric(&logBuf, "pack_uri_advertised_total", "repo_id", "acme/demo") {
		t.Errorf("expected pack_uri_advertised_total{repo_id=acme/demo} in logs:\n%s", logBuf.String())
	}
}

// TestServeFetch_NoPackURIMetric_WhenGateClosed verifies that
// pack_uri_advertised_total is NOT emitted when the packfile-uris gate is
// closed (PackURIEnabled=false, regardless of BuildURL or client opt-in).
func TestServeFetch_NoPackURIMetric_WhenGateClosed(t *testing.T) {
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

	ctx := context.Background()
	r, err := repo.Open(ctx, store, "acme", "demo")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("r.ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tipOID := body.Refs["refs/heads/main"]

	// Backfill PackChecksum so the only thing preventing the metric is
	// PackURIEnabled=false.
	if _, err := r.Commit(ctx, txBody("test_packchecksum_backfill"), func(prev *repo.RootView) ([]byte, error) {
		var pb manifest.Body
		if uerr := json.Unmarshal(prev.Body, &pb); uerr != nil {
			return nil, uerr
		}
		pb.Packs[0].PackChecksum = "0123456789abcdef0123456789abcdef01234567"
		return manifest.MarshalBody(pb)
	}); err != nil {
		t.Fatalf("Commit (backfill PackChecksum): %v", err)
	}

	var body2 bytes.Buffer
	writePkt := func(s string) {
		n := len(s) + 4
		body2.WriteByte(hexNibbleUP(byte(n >> 12)))
		body2.WriteByte(hexNibbleUP(byte(n >> 8 & 0xf)))
		body2.WriteByte(hexNibbleUP(byte(n >> 4 & 0xf)))
		body2.WriteByte(hexNibbleUP(byte(n & 0xf)))
		body2.WriteString(s)
	}
	writePkt("command=fetch\n")
	body2.WriteString("0001")
	writePkt("want " + tipOID + "\n")
	writePkt("done\n")
	writePkt("packfile-uris=https\n")
	body2.WriteString("0000")

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	var out bytes.Buffer
	var logBuf bytes.Buffer
	req := &EngineRequest{
		Ctx:             ctx,
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           bytes.NewReader(body2.Bytes()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		Logger:          captureLogger(&logBuf),
		PackURIEnabled:  false, // gate closed
		PackURIBuildURL: func(_ context.Context, _, _, _ string) (string, error) {
			t.Fatalf("BuildURL must not be called when PackURIEnabled=false")
			return "", nil
		},
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}

	// packfile-uris section must be absent.
	if bytes.Contains(out.Bytes(), []byte("packfile-uris\n")) {
		t.Fatalf("packfile-uris section must be absent when gate closed, got: %q", out.Bytes())
	}
	// pack_uri_advertised_total must NOT be emitted.
	if logContainsMetric(&logBuf, "pack_uri_advertised_total", "via", "direct") ||
		logContainsMetric(&logBuf, "pack_uri_advertised_total", "via", "proxied") {
		t.Errorf("pack_uri_advertised_total must not be emitted when gate is closed:\n%s", logBuf.String())
	}
	// Normal fetch must still work (packfile section present).
	if !bytes.Contains(out.Bytes(), []byte("packfile\n")) {
		t.Fatalf("expected packfile section in normal fetch, got: %q", out.Bytes())
	}
}

// TestServeBundleURI_EmitsAdvertisedMetric_FeatureDisabled verifies that when
// BundleURIEnabled=false, bundle_advertised_total{freshness=disabled} is emitted
// but bundle_uri_advertised_total and the bundle.uri.advertised audit are NOT.
func TestServeBundleURI_EmitsAdvertisedMetric_FeatureDisabled(t *testing.T) {
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

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	var out bytes.Buffer
	var logBuf bytes.Buffer
	req := &EngineRequest{
		Ctx:             context.Background(),
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           bytes.NewReader(buildBundleURIRequest()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		Logger:          captureLogger(&logBuf),
		BundleURIEnabled: false, // feature disabled
		BundleWarmCommits: 5000,
		BundleWarmAge:     24 * time.Hour,
		// BundleURIBuildURL intentionally nil to match the disabled path.
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}

	// Response must be empty (flush only).
	if out.String() != "0000" {
		t.Fatalf("expected flush-only response when feature disabled, got: %q", out.String())
	}
	// bundle_advertised_total{freshness=disabled} must be emitted.
	if !logContainsMetric(&logBuf, "bundle_advertised_total", "freshness", "disabled") {
		t.Errorf("expected bundle_advertised_total{freshness=disabled} in logs:\n%s", logBuf.String())
	}
	// bundle_uri_advertised_total must NOT be emitted.
	if logContainsMetric(&logBuf, "bundle_uri_advertised_total", "via", "direct") ||
		logContainsMetric(&logBuf, "bundle_uri_advertised_total", "via", "proxied") {
		t.Errorf("bundle_uri_advertised_total must not be emitted when feature disabled:\n%s", logBuf.String())
	}
	// bundle.uri.advertised audit must NOT be emitted.
	if logContains(&logBuf, "event", "bundle.uri.advertised") {
		t.Errorf("bundle.uri.advertised audit must not fire when feature disabled:\n%s", logBuf.String())
	}
}

// TestServeBundleURI_EmitsAdvertisedMetricAndAudit_DirectURL verifies that
// when BuildURL returns a non-proxied (direct cloud-shaped) URL,
// bundle_uri_advertised_total{via=direct} and the audit event fire with via=direct.
func TestServeBundleURI_EmitsAdvertisedMetricAndAudit_DirectURL(t *testing.T) {
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

	ctx := context.Background()
	r, openErr := repo.Open(ctx, store, "acme", "demo")
	if openErr != nil {
		t.Fatalf("repo.Open: %v", openErr)
	}
	view, viewErr := r.ReadRoot(ctx)
	if viewErr != nil {
		t.Fatalf("r.ReadRoot: %v", viewErr)
	}
	var body manifest.Body
	if jsonErr := json.Unmarshal(view.Body, &body); jsonErr != nil {
		t.Fatalf("unmarshal: %v", jsonErr)
	}
	tipOID, ok := body.Refs["refs/heads/main"]
	if !ok {
		t.Fatalf("refs/heads/main not found in manifest")
	}

	entry := manifest.BundleEntry{
		ID: "b1", Kind: "full_default", Ref: "refs/heads/main",
		TipOID:      tipOID,
		GeneratedAt: time.Now().Add(-time.Minute).Format(time.RFC3339),
		BundleKey:   "bundles/test.bundle",
	}
	if casErr := maintenance.RunBundleCASMerge(ctx, r, entry, "test", 1); casErr != nil {
		t.Fatalf("RunBundleCASMerge: %v", casErr)
	}

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	// Direct URL: S3 pre-signed URL shape — no /_bundle/ or /_pack/ in path.
	const directURL = "https://s3.amazonaws.com/mybucket/bundles/abc.bundle?X-Amz-Signature=fakesig"

	var out bytes.Buffer
	var logBuf bytes.Buffer
	req := &EngineRequest{
		Ctx:             ctx,
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           bytes.NewReader(buildBundleURIRequest()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		Logger:          captureLogger(&logBuf),
		BundleURIEnabled:  true,
		BundleWarmCommits: 5000,
		BundleWarmAge:     24 * time.Hour,
		BundleURIBuildURL: func(_ context.Context, _, _, _ string) (string, error) {
			return directURL, nil
		},
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}

	// Bundle must be advertised in the response.
	if !bytes.Contains(out.Bytes(), []byte("bundle.b1.uri=")) {
		t.Fatalf("expected bundle.b1.uri= in response, got: %q", out.String())
	}
	// bundle_advertised_total{freshness=current} must be emitted.
	if !logContainsMetric(&logBuf, "bundle_advertised_total", "freshness", "current") {
		t.Errorf("expected bundle_advertised_total{freshness=current}:\n%s", logBuf.String())
	}
	// bundle_uri_advertised_total{via=direct} must be emitted.
	if !logContainsMetric(&logBuf, "bundle_uri_advertised_total", "via", "direct") {
		t.Errorf("expected bundle_uri_advertised_total{via=direct}:\n%s", logBuf.String())
	}
	// bundle.uri.advertised audit must be emitted with via=direct.
	if !logContains(&logBuf, "event", "bundle.uri.advertised") {
		t.Errorf("expected bundle.uri.advertised audit event:\n%s", logBuf.String())
	}
}

// TestServeFetch_EmitsPackURIAdvertisedMetric_DirectURL verifies that when
// BuildURL returns a non-proxied (direct cloud-shaped) URL for a pack,
// pack_uri_advertised_total{via=direct} is emitted.
func TestServeFetch_EmitsPackURIAdvertisedMetric_DirectURL(t *testing.T) {
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

	ctx := context.Background()
	r, err := repo.Open(ctx, store, "acme", "demo")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("r.ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tipOID, ok := body.Refs["refs/heads/main"]
	if !ok {
		t.Fatalf("refs/heads/main not in manifest")
	}

	// Backfill PackChecksum so EvaluateFullPackRequested gate passes.
	const fakeSha1 = "0123456789abcdef0123456789abcdef01234567"
	if _, err := r.Commit(ctx, txBody("test_packchecksum_backfill"), func(prev *repo.RootView) ([]byte, error) {
		var pb manifest.Body
		if uerr := json.Unmarshal(prev.Body, &pb); uerr != nil {
			return nil, uerr
		}
		pb.Packs[0].PackChecksum = fakeSha1
		return manifest.MarshalBody(pb)
	}); err != nil {
		t.Fatalf("Commit (backfill PackChecksum): %v", err)
	}

	var body2 bytes.Buffer
	writePkt := func(s string) {
		n := len(s) + 4
		body2.WriteByte(hexNibbleUP(byte(n >> 12)))
		body2.WriteByte(hexNibbleUP(byte(n >> 8 & 0xf)))
		body2.WriteByte(hexNibbleUP(byte(n >> 4 & 0xf)))
		body2.WriteByte(hexNibbleUP(byte(n & 0xf)))
		body2.WriteString(s)
	}
	writePkt("command=fetch\n")
	body2.WriteString("0001")
	writePkt("want " + tipOID + "\n")
	writePkt("done\n")
	writePkt("packfile-uris=https\n")
	body2.WriteString("0000")

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	defer mgr.Close()

	// Direct URL: Cloudflare R2 pre-signed URL shape.
	const directPackURL = "https://my-r2-bucket.r2.cloudflarestorage.com/packs/def.pack?X-Amz-Signature=fakesig"

	var out bytes.Buffer
	var logBuf bytes.Buffer
	req := &EngineRequest{
		Ctx:             ctx,
		Tenant:          "acme",
		Repo:            "demo",
		Stdin:           bytes.NewReader(body2.Bytes()),
		Stdout:          &out,
		Stderr:          &bytes.Buffer{},
		ProtocolVersion: 2,
		Store:           store,
		Mirror:          mgr,
		AgentVersion:    "test",
		Logger:          captureLogger(&logBuf),
		PackURIEnabled:  true,
		PackURIBuildURL: func(_ context.Context, hash, key, expected string) (string, error) {
			return directPackURL, nil
		},
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}

	// packfile-uris section must be present in the response.
	if !bytes.Contains(out.Bytes(), []byte("packfile-uris\n")) {
		t.Fatalf("expected packfile-uris section in response, got: %q", out.Bytes())
	}
	// pack_uri_advertised_total{via=direct} must be emitted.
	if !logContainsMetric(&logBuf, "pack_uri_advertised_total", "via", "direct") {
		t.Errorf("expected pack_uri_advertised_total{via=direct} in logs:\n%s", logBuf.String())
	}
	// pack_uri_advertised_total{repo_id=acme/demo} must be emitted.
	if !logContainsMetric(&logBuf, "pack_uri_advertised_total", "repo_id", "acme/demo") {
		t.Errorf("expected pack_uri_advertised_total{repo_id=acme/demo} in logs:\n%s", logBuf.String())
	}
}
