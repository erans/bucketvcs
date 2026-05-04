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

// TestRun_ScrubsRepoScopingEnv verifies that GIT_DIR set in the environment
// does not redirect InitBare away from the real target directory.
func TestRun_ScrubsRepoScopingEnv(t *testing.T) {
	skipIfNoGit(t)
	t.Setenv("GIT_DIR", "/some/bogus/path")
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare with GIT_DIR set to bogus path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "objects")); err != nil {
		t.Fatalf("expected objects/ dir in real dir after InitBare: %v", err)
	}
}

// TestScrubGitRepoEnv_RemovesAllScopingVars verifies that the helper strips
// all 9 repo-scoping variables and leaves non-scoping variables intact.
func TestScrubGitRepoEnv_RemovesAllScopingVars(t *testing.T) {
	input := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/root",
		"GIT_AUTHOR_NAME=Alice",
		"GIT_DIR=/some/dir",
		"GIT_WORK_TREE=/work",
		"GIT_INDEX_FILE=/idx",
		"GIT_OBJECT_DIRECTORY=/obj",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/alt",
		"GIT_COMMON_DIR=/common",
		"GIT_NAMESPACE=ns",
		"GIT_CEILING_DIRECTORIES=/ceiling",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM=1",
	}
	got := scrubGitRepoEnv(input)

	// Non-scoping vars must survive.
	keep := []string{"PATH=/usr/bin:/bin", "HOME=/root", "GIT_AUTHOR_NAME=Alice"}
	for _, want := range keep {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("scrubGitRepoEnv removed %q but should have kept it", want)
		}
	}

	// Scoping vars must be absent.
	for _, scoping := range gitRepoScopingVars {
		for _, g := range got {
			key := g
			if idx := strings.Index(g, "="); idx >= 0 {
				key = g[:idx]
			}
			if key == scoping {
				t.Errorf("scrubGitRepoEnv kept scoping var %q", scoping)
			}
		}
	}
}
