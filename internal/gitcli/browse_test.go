package gitcli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeBrowseBare builds a bare repo with one commit (a.txt + sub/b.txt) on main
// and returns (bareDir, commitOID).
func makeBrowseBare(t *testing.T) (string, string) {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "r.git")
	mustRun := func(dir string, args ...string) {
		c := exec.Command("git", args...)
		if dir != "" {
			c.Dir = dir
		}
		c.Env = append(scrubGitRepoEnv(os.Environ()), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	mustRun("", "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "sub", "b.txt"), []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(work, "add", ".")
	mustRun(work, "commit", "-q", "-m", "init")
	mustRun("", "clone", "-q", "--bare", work, bare)
	out, err := exec.Command("git", "-C", bare, "rev-parse", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	return bare, strings.TrimSpace(string(out))
}

func TestLsTree_RootAndSub(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, oid := makeBrowseBare(t)
	root, err := LsTree(context.Background(), bare, oid)
	if err != nil {
		t.Fatalf("LsTree root: %v", err)
	}
	if !strings.Contains(string(root), "a.txt") || !strings.Contains(string(root), "sub") {
		t.Fatalf("root listing missing entries: %q", root)
	}
	sub, err := LsTree(context.Background(), bare, oid+":sub")
	if err != nil {
		t.Fatalf("LsTree sub: %v", err)
	}
	if !strings.Contains(string(sub), "b.txt") {
		t.Fatalf("sub listing missing b.txt: %q", sub)
	}
}

func TestCatBlob(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, oid := makeBrowseBare(t)
	b, err := CatBlob(context.Background(), bare, oid+":a.txt")
	if err != nil {
		t.Fatalf("CatBlob: %v", err)
	}
	if string(b) != "hello\n" {
		t.Fatalf("got %q", b)
	}
}

func TestLogRaw(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, oid := makeBrowseBare(t)
	lg, err := LogRaw(context.Background(), bare, oid, 0, 10)
	if err != nil || !strings.Contains(string(lg), "init") {
		t.Fatalf("LogRaw: %v %q", err, lg)
	}
}

func TestCatFileCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, oid := makeBrowseBare(t)
	co, err := CatFileCommit(context.Background(), bare, oid)
	if err != nil || !strings.Contains(string(co), "author") {
		t.Fatalf("CatFileCommit: %v %q", err, co)
	}
}

func TestDiffTreePatch(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, oid := makeBrowseBare(t)
	d, err := DiffTreePatch(context.Background(), bare, oid, "")
	if err != nil || !strings.Contains(string(d), "a.txt") {
		t.Fatalf("DiffTreePatch: %v %q", err, d)
	}
}

func TestBrowseHelpers_RejectFlagLikeArgs(t *testing.T) {
	_, err := LsTree(context.Background(), "/tmp", "--upload-pack=evil")
	if err == nil {
		t.Fatal("expected rejection of flag-like treeish")
	}
}

func TestValidRevPath(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, _ := makeBrowseBare(t)

	// Space in path: should be accepted by validRevPath and resolve correctly.
	// Write a file with a space in the name directly into the work tree via
	// RunForTest, then read it back through the bare clone.
	work := t.TempDir()
	mustRun := func(dir string, args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(scrubGitRepoEnv(os.Environ()), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	mustRun("", "clone", "-q", bare, work)
	if err := os.WriteFile(filepath.Join(work, "has space.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(work, "add", ".")
	mustRun(work, "commit", "-q", "-m", "add spaced file")

	newBare := filepath.Join(t.TempDir(), "new.git")
	mustRun("", "clone", "-q", "--bare", work, newBare)
	headOID := strings.TrimSpace(func() string {
		out, err := exec.Command("git", "-C", newBare, "rev-parse", "main").Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}())

	rev := headOID + ":has space.txt"

	// LsTree on a treeish that contains a file with a space in the name.
	out, err := LsTree(context.Background(), newBare, headOID)
	if err != nil {
		t.Fatalf("LsTree: %v", err)
	}
	if !strings.Contains(string(out), "has space.txt") {
		t.Errorf("LsTree output missing spaced filename: %q", out)
	}

	// CatBlob with the rev "<oid>:has space.txt" (space in path component).
	b, err := CatBlob(context.Background(), newBare, rev)
	if err != nil {
		t.Fatalf("CatBlob spaced name: %v", err)
	}
	if string(b) != "ok\n" {
		t.Fatalf("CatBlob got %q", b)
	}

	// Confirm bare "-" prefix is still rejected.
	if _, err := CatBlob(context.Background(), newBare, "-bad"); err == nil {
		t.Fatal("expected rejection of flag-like rev")
	}
}

// TestRunCapped_Overflow confirms that when git output exceeds the byte cap,
// runCapped returns (prefix, err) where errors.Is(err, ErrOutputCapped) and
// len(prefix) <= cap.
func TestRunCapped_Overflow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, _ := makeBrowseBare(t)
	ctx := context.Background()
	const cap = 4
	out, err := runCapped(ctx, bare, cap, "log", "main")
	if err == nil {
		t.Fatalf("runCapped: expected error on cap=4, got nil (output: %q)", out)
	}
	if !errors.Is(err, ErrOutputCapped) {
		t.Fatalf("runCapped: error %v is not ErrOutputCapped", err)
	}
	if len(out) > cap {
		t.Fatalf("runCapped: output prefix len=%d exceeds cap=%d", len(out), cap)
	}
}

// TestRunCapped_UnderCap confirms that when git output fits within the cap,
// runCapped succeeds and returns the full output.
func TestRunCapped_UnderCap(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, _ := makeBrowseBare(t)
	ctx := context.Background()
	out, err := runCapped(ctx, bare, 1<<20, "rev-parse", "main")
	if err != nil {
		t.Fatalf("runCapped: unexpected error: %v", err)
	}
	oid := strings.TrimSpace(string(out))
	if len(oid) != 40 {
		t.Fatalf("runCapped: expected 40-char OID, got %q", oid)
	}
}

func TestLogNameStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, oid := makeBrowseBare(t)
	out, err := LogNameStatus(context.Background(), bare, oid, 10, "")
	if err != nil {
		t.Fatalf("LogNameStatus: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "\x1e") || !strings.Contains(s, "\x1f") {
		t.Fatalf("missing record/field separators: %q", s)
	}
	if !strings.Contains(s, "A\ta.txt") {
		t.Fatalf("missing name-status entry for a.txt: %q", s)
	}
	scoped, err := LogNameStatus(context.Background(), bare, oid, 10, "sub")
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if !strings.Contains(string(scoped), "sub/b.txt") || strings.Contains(string(scoped), "A\ta.txt") {
		t.Fatalf("scoping wrong: %q", scoped)
	}
}

func TestLogNameStatus_RejectsBadArgs(t *testing.T) {
	if _, err := LogNameStatus(context.Background(), "/tmp", "--evil", 10, ""); err == nil {
		t.Fatal("flag-like oid accepted")
	}
	if _, err := LogNameStatus(context.Background(), "/tmp", "abc", 10, "-evil"); err == nil {
		t.Fatal("flag-like scope path accepted")
	}
}
