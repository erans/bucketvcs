package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
}

// makeSrcRepo authors a tiny bare repo and returns its path. Used by
// importer tests across Tasks 16-18.
func makeSrcRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	mustGit := func(args ...string) {
		t.Helper()
		out, err := gitcli.RunForTest(work, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustGit("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit("add", "f")
	mustGit("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "init")
	bare := filepath.Join(t.TempDir(), "bare")
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	return bare
}

func TestPrepareLocalPack_ProducesPackAndRefs(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	prep, err := prepareLocalPack(context.Background(), src)
	if err != nil {
		t.Fatalf("prepareLocalPack: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prep.WorkDir) })

	if prep.PackID == "" || len(prep.PackID) != 40 {
		t.Fatalf("PackID: %q", prep.PackID)
	}
	if _, err := os.Stat(prep.PackPath); err != nil {
		t.Fatalf("pack stat: %v", err)
	}
	if _, err := os.Stat(prep.IdxPath); err != nil {
		t.Fatalf("idx stat: %v", err)
	}
	if len(prep.Refs) == 0 {
		t.Fatalf("expected refs")
	}
	if !strings.HasPrefix(prep.DefaultBranch, "refs/heads/") {
		t.Fatalf("DefaultBranch: %q", prep.DefaultBranch)
	}
	if _, err := os.Stat(prep.WorkDir); err != nil {
		t.Fatalf("workdir stat: %v", err)
	}
}

func TestPrepareLocalPack_FsckRejectsCorruptSource(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	// Drop a corrupt loose object into the source repo to make fsck fail.
	bogusDir := filepath.Join(src, "objects", "ab")
	if err := os.MkdirAll(bogusDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bogusDir, "cdef0123456789012345678901234567890123"), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := prepareLocalPack(context.Background(), src); err == nil {
		t.Fatalf("expected prepareLocalPack to fail on corrupt source")
	}
}

func TestPrepareLocalPack_RejectsNonexistentSource(t *testing.T) {
	skipIfNoGit(t)
	if _, err := prepareLocalPack(context.Background(), "/nonexistent/path"); err == nil {
		t.Fatalf("expected prepareLocalPack to fail on nonexistent source")
	}
}
