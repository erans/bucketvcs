package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest/manifesttest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
}

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func makeAndImport(t *testing.T) (storage.ObjectStore, string) {
	t.Helper()
	skipIfNoGit(t)
	work := t.TempDir()
	mustGit := func(args ...string) {
		t.Helper()
		if out, err := gitcli.RunForTest(work, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustGit("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit("add", "f")
	mustGit("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "init")
	bare := filepath.Join(t.TempDir(), "bare")
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	store := newTestStore(t)
	if _, err := importer.Import(context.Background(), store, importer.Options{
		SourceDir: bare, Tenant: "acme", Repo: "x", Actor: "test",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	return store, bare
}

func TestExport_RoundTripFsckClean(t *testing.T) {
	store, _ := makeAndImport(t)
	dst := filepath.Join(t.TempDir(), "out")
	res, err := Export(context.Background(), store, Options{
		Tenant: "acme", Repo: "x", DestDir: dst,
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !res.FsckOK {
		t.Fatalf("expected FsckOK")
	}
	if _, err := os.Stat(filepath.Join(dst, "objects")); err != nil {
		t.Fatalf("expected objects/: %v", err)
	}
}

func TestExport_RejectsNonEmptyDest(t *testing.T) {
	store, _ := makeAndImport(t)
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "junk"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Export(context.Background(), store, Options{
		Tenant: "acme", Repo: "x", DestDir: dst,
	})
	if err == nil {
		t.Fatalf("expected error on non-empty DestDir")
	}
}

func TestExport_RejectsNonexistentRepo(t *testing.T) {
	store := newTestStore(t)
	dst := filepath.Join(t.TempDir(), "out")
	_, err := Export(context.Background(), store, Options{
		Tenant: "absent", Repo: "absent", DestDir: dst,
	})
	if err == nil {
		t.Fatalf("expected error on nonexistent repo")
	}
}

func TestExport_RefsMatchSource(t *testing.T) {
	store, srcBare := makeAndImport(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Export(context.Background(), store, Options{
		Tenant: "acme", Repo: "x", DestDir: dst,
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	srcRefs, err := gitcli.ShowRef(context.Background(), srcBare)
	if err != nil {
		t.Fatalf("src ShowRef: %v", err)
	}
	dstRefs, err := gitcli.ShowRef(context.Background(), dst)
	if err != nil {
		t.Fatalf("dst ShowRef: %v", err)
	}
	if len(srcRefs) != len(dstRefs) {
		t.Fatalf("ref count differs: src=%d dst=%d", len(srcRefs), len(dstRefs))
	}
	for k, v := range srcRefs {
		if dstRefs[k] != v {
			t.Fatalf("ref %s: src=%s dst=%s", k, v, dstRefs[k])
		}
	}
}

func TestExport_SkipFsck(t *testing.T) {
	store, _ := makeAndImport(t)
	dst := filepath.Join(t.TempDir(), "out")
	res, err := Export(context.Background(), store, Options{
		Tenant: "acme", Repo: "x", DestDir: dst, SkipFsck: true,
	})
	if err != nil {
		t.Fatalf("Export with SkipFsck: %v", err)
	}
	if res.FsckOK {
		t.Fatalf("FsckOK should be false when SkipFsck=true")
	}
}

func TestExport_DefaultRunsFsck(t *testing.T) {
	store, _ := makeAndImport(t)
	dst := filepath.Join(t.TempDir(), "out")
	res, err := Export(context.Background(), store, Options{
		Tenant: "acme", Repo: "x", DestDir: dst,
		// SkipFsck not set — default = run fsck
	})
	if err != nil {
		t.Fatalf("Export with default options: %v", err)
	}
	if !res.FsckOK {
		t.Fatalf("FsckOK should be true with default options")
	}
}

func TestExport_RejectsMalformedPackID(t *testing.T) {
	// Build a synthetic manifest body with a malformed PackID, write it
	// directly to storage, then attempt export.
	store := newTestStore(t)
	// Use repo.Create to set up a valid version-1 root, then Commit a
	// body with an evil PackID.
	r, err := repo.Create(context.Background(), store, "t", "r", repo.CreateOptions{
		DefaultBranch: "refs/heads/main",
		ObjectFormat:  "sha1",
		Actor:         "test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{},
		Packs: []manifest.PackEntry{{
			PackID:      "../../etc/passwd",
			PackKey:     "bogus",
			IdxKey:      "bogus",
			SizeBytes:   1,
			ObjectCount: 1,
		}},
		Indexes: manifest.Indexes{},
	}
	bodyBytes, err := manifest.MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if _, err := r.Commit(context.Background(), tx.Body{Type: "test", Actor: "test"},
		func(prev *repo.RootView) ([]byte, error) { return bodyBytes, nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	_, err = Export(context.Background(), store, Options{
		Tenant: "t", Repo: "r", DestDir: dst, SkipFsck: true,
	})
	if err == nil {
		t.Fatalf("expected rejection of malformed PackID")
	}
}

func TestExport_RejectsNullRefOID(t *testing.T) {
	store := newTestStore(t)
	r, err := repo.Create(context.Background(), store, "t", "r", repo.CreateOptions{
		DefaultBranch: "refs/heads/main",
		ObjectFormat:  "sha1",
		Actor:         "test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{"refs/heads/main": "0000000000000000000000000000000000000000"},
		Packs:         []manifest.PackEntry{},
		Indexes:       manifest.Indexes{},
	}
	bodyBytes, err := manifest.MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if _, err := r.Commit(context.Background(), tx.Body{Type: "test", Actor: "test"},
		func(prev *repo.RootView) ([]byte, error) { return bodyBytes, nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	_, err = Export(context.Background(), store, Options{
		Tenant: "t", Repo: "r", DestDir: dst, SkipFsck: true,
	})
	if err == nil {
		t.Fatalf("expected rejection of null OID ref")
	}
}

func TestExport_RejectsMalformedRefOID(t *testing.T) {
	store := newTestStore(t)
	r, err := repo.Create(context.Background(), store, "t", "r", repo.CreateOptions{
		DefaultBranch: "refs/heads/main",
		ObjectFormat:  "sha1",
		Actor:         "test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{"refs/heads/main": "not-a-hex-oid"},
		Packs:         []manifest.PackEntry{},
		Indexes:       manifest.Indexes{},
	}
	bodyBytes, err := manifest.MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if _, err := r.Commit(context.Background(), tx.Body{Type: "test", Actor: "test"},
		func(prev *repo.RootView) ([]byte, error) { return bodyBytes, nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	_, err = Export(context.Background(), store, Options{
		Tenant: "t", Repo: "r", DestDir: dst, SkipFsck: true,
	})
	if err == nil {
		t.Fatalf("expected rejection of malformed ref OID")
	}
}

func TestExport_AcceptsDashPrefixedDest(t *testing.T) {
	store, _ := makeAndImport(t)
	parent := t.TempDir()
	dst := filepath.Join(parent, "-out") // name starts with dash
	if _, err := Export(context.Background(), store, Options{
		Tenant: "acme", Repo: "x", DestDir: dst, SkipFsck: true,
	}); err != nil {
		t.Fatalf("Export to dash-prefixed dest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "objects")); err != nil {
		t.Fatalf("expected objects/ in dst: %v", err)
	}
}

func TestExport_RejectsRefNameNotInRefsNamespace(t *testing.T) {
	store := newTestStore(t)
	r, err := repo.Create(context.Background(), store, "t", "r", repo.CreateOptions{
		DefaultBranch: "refs/heads/main",
		ObjectFormat:  "sha1",
		Actor:         "test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{"HEAD": "0123456789abcdef0123456789abcdef01234567"},
		Packs:         []manifest.PackEntry{},
		Indexes:       manifest.Indexes{},
	}
	bodyBytes, err := manifest.MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if _, err := r.Commit(context.Background(), tx.Body{Type: "test", Actor: "test"},
		func(prev *repo.RootView) ([]byte, error) { return bodyBytes, nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	_, err = Export(context.Background(), store, Options{
		Tenant: "t", Repo: "r", DestDir: dst, SkipFsck: true,
	})
	if err == nil {
		t.Fatalf("expected rejection of HEAD as ref name")
	}
}

func TestExport_RejectsInvalidDefaultBranch(t *testing.T) {
	store := newTestStore(t)
	r, err := repo.Create(context.Background(), store, "t", "r", repo.CreateOptions{
		DefaultBranch: "refs/heads/main",
		ObjectFormat:  "sha1",
		Actor:         "test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := manifest.Body{
		DefaultBranch: "garbage", // missing refs/ prefix
		Refs:          map[string]string{},
		Packs:         []manifest.PackEntry{},
		Indexes:       manifest.Indexes{},
	}
	bodyBytes, err := manifest.MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if _, err := r.Commit(context.Background(), tx.Body{Type: "test", Actor: "test"},
		func(prev *repo.RootView) ([]byte, error) { return bodyBytes, nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	_, err = Export(context.Background(), store, Options{
		Tenant: "t", Repo: "r", DestDir: dst, SkipFsck: true,
	})
	if err == nil {
		t.Fatalf("expected rejection of invalid default_branch")
	}
}

func TestDownloadBitmapSidecar_LandsAtPackBitmapPath(t *testing.T) {
	// Focused unit test for the M9.5 bitmap-download path: when
	// PackEntry.BitmapKey is non-empty, downloadBitmapSidecar fetches
	// the blob and writes it as pack-<id>.bitmap alongside .pack/.idx.
	s := newTestStore(t)
	const bitmapKey = "tenants/acme/repos/x/packs/canonical/abcd.bitmap"
	const packID = "0123456789012345678901234567890123456789"
	bitmapBytes := []byte("fake-bitmap-bytes\n")
	if _, err := s.PutIfAbsent(context.Background(), bitmapKey, bytes.NewReader(bitmapBytes), nil); err != nil {
		t.Fatalf("PutIfAbsent bitmap: %v", err)
	}
	packDir := t.TempDir()
	if err := downloadBitmapSidecar(context.Background(), s, bitmapKey, packDir, packID); err != nil {
		t.Fatalf("downloadBitmapSidecar: %v", err)
	}
	dst := filepath.Join(packDir, "pack-"+packID+".bitmap")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read bitmap: %v", err)
	}
	if string(got) != string(bitmapBytes) {
		t.Errorf("bitmap bytes mismatch:\ngot=%q\nwant=%q", got, bitmapBytes)
	}
}

func TestDownloadBitmapSidecar_NotFoundIsReportedToCaller(t *testing.T) {
	// downloadBitmapSidecar surfaces the error; the exporter caller
	// is the one that treats ErrNotFound as benign. Verify the error
	// is the right type so that gating works.
	s := newTestStore(t)
	packDir := t.TempDir()
	err := downloadBitmapSidecar(context.Background(), s, "missing/key", packDir, "0123456789012345678901234567890123456789")
	if err == nil {
		t.Fatal("expected error on missing key")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected wrapped ErrNotFound, got %v", err)
	}
}

// TestExport_ShardedBody_RejectsMalformedRefOID mirrors TestExport_RejectsMalformedRefOID
// but uses a v2 sharded manifest body (via manifesttest.MakeShardedBody) to verify
// that the exporter reads refs through refstore.List rather than body.Refs directly.
func TestExport_ShardedBody_RejectsMalformedRefOID(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	r, err := repo.Create(ctx, store, "t", "shard-export", repo.CreateOptions{
		DefaultBranch: "refs/heads/main",
		ObjectFormat:  "sha1",
		Actor:         "test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	k, err := keys.NewRepo("t", "shard-export")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	// Build a sharded body with a malformed OID — export must reject it
	// the same way it does for an inline body, confirming refs flow through refstore.
	shardRefs := map[string]string{
		"refs/heads/main": "not-a-hex-oid",
	}
	shardedBody, err := manifesttest.MakeShardedBody(ctx, store, k, "refs/heads/main", shardRefs)
	if err != nil {
		t.Fatalf("MakeShardedBody: %v", err)
	}
	bodyBytes, err := manifest.MarshalBody(shardedBody)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if _, err := r.Commit(ctx, tx.Body{Type: "test", Actor: "test"},
		func(prev *repo.RootView) ([]byte, error) { return bodyBytes, nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "out")
	_, err = Export(ctx, store, Options{
		Tenant: "t", Repo: "shard-export", DestDir: dst, SkipFsck: true,
	})
	if err == nil {
		t.Fatal("expected rejection of malformed OID in sharded body")
	}
}

// TestExport_ShardedBody_DefaultBranchMustExistInRefs mirrors the inline-body
// default_branch consistency check but drives it through a sharded (v2) body,
// confirming the refstore path handles this validation correctly.
func TestExport_ShardedBody_DefaultBranchMustExistInRefs(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	r, err := repo.Create(ctx, store, "t", "shard-export2", repo.CreateOptions{
		DefaultBranch: "refs/heads/main",
		ObjectFormat:  "sha1",
		Actor:         "test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	k, err := keys.NewRepo("t", "shard-export2")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	// Sharded body has refs/heads/dev but DefaultBranch says refs/heads/main.
	shardRefs := map[string]string{
		"refs/heads/dev": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	shardedBody, err := manifesttest.MakeShardedBody(ctx, store, k, "refs/heads/main", shardRefs)
	if err != nil {
		t.Fatalf("MakeShardedBody: %v", err)
	}
	bodyBytes, err := manifest.MarshalBody(shardedBody)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if _, err := r.Commit(ctx, tx.Body{Type: "test", Actor: "test"},
		func(prev *repo.RootView) ([]byte, error) { return bodyBytes, nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "out")
	_, err = Export(ctx, store, Options{
		Tenant: "t", Repo: "shard-export2", DestDir: dst, SkipFsck: true,
	})
	if err == nil {
		t.Fatal("expected error when default_branch not present in sharded refs")
	}
}

func TestDownloadAndIndexPack_MissingBitmapIsBenign(t *testing.T) {
	// Pins the contract that the exporter must treat a missing .bitmap
	// blob as a non-fatal skip: the pack download + index-pack succeed,
	// and the bitmap-download ErrNotFound is swallowed. A future
	// refactor that loses the errors.Is gate would break this test.
	store, _ := makeAndImport(t)
	ctx := context.Background()

	// Read the imported manifest to get a real PackEntry, then mutate
	// it to point BitmapKey at a key that does not exist in storage.
	r, err := repo.Open(ctx, store, "acme", "x")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Packs) == 0 {
		t.Fatal("no packs in imported manifest")
	}
	p := body.Packs[0]
	p.BitmapKey = "tenants/acme/repos/x/packs/canonical/never-existed.bitmap"

	dst := t.TempDir()
	// Bare layout: <dst>/objects/pack/ is what downloadAndIndexPack
	// writes into; we need the parent to exist.
	if err := os.MkdirAll(filepath.Join(dst, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}

	objCount, err := downloadAndIndexPack(ctx, store, p, dst)
	if err != nil {
		t.Fatalf("downloadAndIndexPack should treat missing bitmap as benign; got %v", err)
	}
	if objCount != p.ObjectCount {
		t.Errorf("ObjectCount: got %d, want %d", objCount, p.ObjectCount)
	}
	// .pack and .idx must have landed; .bitmap must NOT have landed.
	for _, ext := range []string{".pack", ".idx"} {
		if _, err := os.Stat(filepath.Join(dst, "objects", "pack", "pack-"+p.PackID+ext)); err != nil {
			t.Errorf("missing %s: %v", ext, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "objects", "pack", "pack-"+p.PackID+".bitmap")); err == nil {
		t.Error("unexpected .bitmap landed despite missing storage key")
	}
}
