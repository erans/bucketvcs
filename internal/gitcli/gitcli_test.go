package gitcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := Version(context.Background()); err != nil {
		t.Skip("git not available on PATH:", err)
	}
}

func TestVersion_Reports(t *testing.T) {
	skipIfNoGit(t)
	v, err := Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !strings.HasPrefix(v, "git version ") {
		t.Fatalf("Version output unexpected: %q", v)
	}
}

func TestInitBare_CreatesObjectsDir(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "objects")); err != nil {
		t.Fatalf("expected objects/ dir after InitBare: %v", err)
	}
}

func TestFsck_OK(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	if err := Fsck(context.Background(), dir, true); err != nil {
		t.Fatalf("Fsck on empty bare repo: %v", err)
	}
}

func TestFsck_DetectsCorruption(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	// Drop a clearly-bogus loose object.
	bogus := filepath.Join(dir, "objects", "ab")
	if err := os.MkdirAll(bogus, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bogus, "cdef0123456789012345678901234567890123"), []byte("not-a-git-object"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := Fsck(context.Background(), dir, true); err == nil {
		t.Fatalf("expected Fsck to fail on corrupt loose object")
	}
}

func TestSetBinaryForTest_Override(t *testing.T) {
	old := SetBinaryForTest("/nonexistent-git-binary")
	t.Cleanup(func() { SetBinaryForTest(old) })
	if _, err := Version(context.Background()); err == nil {
		t.Fatalf("expected error when binary path is bogus")
	}
}
