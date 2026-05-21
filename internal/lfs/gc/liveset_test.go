package gc_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/lfs/gc"
)

// seedBareRepoWithPointers creates a tmp bare repo and commits one
// regular blob + two LFS pointer blobs. Returns the bare dir and the
// expected LFS OIDs the live set should contain.
func seedBareRepoWithPointers(t *testing.T) (bareDir string, wantOIDs []string) {
	t.Helper()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	mustRun := func(dir string, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v in %s failed: %v\n%s", name, args, dir, err, b)
		}
	}
	mustRun(tmp, "git", "init", "--bare", bare)
	mustRun(tmp, "git", "init", "-q", "-b", "main", work)
	mustRun(work, "git", "config", "user.email", "smoke@example.com")
	mustRun(work, "git", "config", "user.name", "smoke")

	// Regular blob.
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	// LFS pointer 1 — references oid aa...aa.
	const oid1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pointer1 := "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid1 + "\nsize 1234\n"
	if err := os.WriteFile(filepath.Join(work, "model.bin"), []byte(pointer1), 0o644); err != nil {
		t.Fatalf("write model.bin: %v", err)
	}
	// LFS pointer 2 — references oid bb...bb.
	const oid2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pointer2 := "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid2 + "\nsize 5678\n"
	if err := os.WriteFile(filepath.Join(work, "weights.bin"), []byte(pointer2), 0o644); err != nil {
		t.Fatalf("write weights.bin: %v", err)
	}

	mustRun(work, "git", "add", "README", "model.bin", "weights.bin")
	mustRun(work, "git", "commit", "-qm", "initial")
	mustRun(work, "git", "push", "-q", bare, "main:refs/heads/main")

	return bare, []string{oid1, oid2}
}

func TestBuildLiveSet_FindsBothPointers(t *testing.T) {
	bare, want := seedBareRepoWithPointers(t)
	got, err := gc.BuildLiveSet(context.Background(), bare)
	if err != nil {
		t.Fatalf("BuildLiveSet: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d want %d (got=%v)", len(got), len(want), got)
	}
	for _, oid := range want {
		if _, ok := got[oid]; !ok {
			t.Errorf("missing oid %q from live set %v", oid, got)
		}
	}
}

func TestBuildLiveSet_EmptyRepoReturnsEmptySet(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	cmd := exec.Command("git", "init", "--bare", bare)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, b)
	}
	got, err := gc.BuildLiveSet(context.Background(), bare)
	if err != nil {
		t.Fatalf("BuildLiveSet on empty repo: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty repo live set len=%d want 0", len(got))
	}
}

func TestBuildLiveSet_IgnoresLargeBlobs(t *testing.T) {
	// A blob larger than our 1024-byte size filter (we'd cat-file it
	// and ParsePointer would reject) is never inspected — but a
	// regression that drops the size filter would silently slow GC
	// on large repos. Use a 2 KiB blob to exercise the boundary.
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	work := filepath.Join(tmp, "work")
	mustRun := func(dir string, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, b)
		}
	}
	mustRun(tmp, "git", "init", "--bare", bare)
	mustRun(tmp, "git", "init", "-q", "-b", "main", work)
	mustRun(work, "git", "config", "user.email", "smoke@example.com")
	mustRun(work, "git", "config", "user.name", "smoke")
	big := make([]byte, 2048)
	for i := range big {
		big[i] = byte('x')
	}
	if err := os.WriteFile(filepath.Join(work, "big.bin"), big, 0o644); err != nil {
		t.Fatalf("write big: %v", err)
	}
	mustRun(work, "git", "add", "big.bin")
	mustRun(work, "git", "commit", "-qm", "big")
	mustRun(work, "git", "push", "-q", bare, "main:refs/heads/main")
	got, err := gc.BuildLiveSet(context.Background(), bare)
	if err != nil {
		t.Fatalf("BuildLiveSet: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("large-blob repo live set=%v want empty", got)
	}
}
