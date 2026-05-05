package exporter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/importer"
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
		Tenant: "acme", Repo: "x", DestDir: dst, RunFsck: true,
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
		Tenant: "acme", Repo: "x", DestDir: dst, RunFsck: true,
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
		Tenant: "acme", Repo: "x", DestDir: dst, RunFsck: true,
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
