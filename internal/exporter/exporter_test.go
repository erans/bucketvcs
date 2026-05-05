package exporter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
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
