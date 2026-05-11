package receivepack

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
)

// setup2CommitBare creates a bare repo with 2 commits: parent commit on main,
// then child commit. Returns bare dir, parent OID, child OID as hex strings.
func setup2CommitBare(t *testing.T) (bareDir, parentOID, childOID string) {
	t.Helper()

	bare := t.TempDir()
	wt := t.TempDir()

	mustExecDir(t, "", "git", "init", "--bare", bare)
	mustExecDir(t, "", "git", "clone", bare, wt)

	// Commit A (parent).
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustExecDir(t, wt, "git", "add", ".")
	mustExecDir(t, wt, "git", "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-m", "A")
	parentOID = mustExecDirOutput(t, wt, "git", "rev-parse", "HEAD")
	mustExecDir(t, wt, "git", "push", "origin", "HEAD:refs/heads/main")

	// Commit B (child).
	if err := os.WriteFile(filepath.Join(wt, "b.txt"), []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustExecDir(t, wt, "git", "add", ".")
	mustExecDir(t, wt, "git", "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-m", "B")
	childOID = mustExecDirOutput(t, wt, "git", "rev-parse", "HEAD")
	mustExecDir(t, wt, "git", "push", "origin", "HEAD:refs/heads/main")

	return bare, parentOID, childOID
}

func mustExecDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func mustExecDirOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestBuildDelta_LinearOneCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git")
	}
	bareDir, parentOIDStr, childOIDStr := setup2CommitBare(t)

	parentOID, err := pack.ParseOID(parentOIDStr)
	if err != nil {
		t.Fatalf("parse parent: %v", err)
	}
	childOID, err := pack.ParseOID(childOIDStr)
	if err != nil {
		t.Fatalf("parse child: %v", err)
	}

	// GenLookup: parent is gen 1 (as if it came from the base .bvcg).
	gl := reachability.NewGenLookup(map[pack.OID]uint32{
		parentOID: 1,
	})

	// revListNewOIDs: only the child commit is "new" (not in base).
	revListNewOIDs := []string{childOIDStr}

	updates := []updateCommand{
		{Refname: "refs/heads/main", OldOID: parentOIDStr, NewOID: childOIDStr},
	}
	packIDs := []pack.OID{} // not critical for this test

	ctx := context.Background()
	d, err := buildDelta(ctx, bareDir, revListNewOIDs, gl, updates, packIDs)
	if err != nil {
		t.Fatalf("buildDelta: %v", err)
	}
	if len(d.Commits) != 1 {
		t.Fatalf("commits = %d, want 1", len(d.Commits))
	}
	if d.Commits[0].OID != childOID {
		t.Errorf("commit OID = %s, want %s", d.Commits[0].OID, childOID)
	}
	// Child's generation = parent gen (1) + 1 = 2.
	if d.Commits[0].Generation != 2 {
		t.Errorf("gen = %d, want 2 (parent gen 1 + 1)", d.Commits[0].Generation)
	}
	if len(d.RefTips) != 1 || d.RefTips[0].RefName != "refs/heads/main" {
		t.Errorf("ref tips = %+v", d.RefTips)
	}
	if d.RefTips[0].NewOID != childOID {
		t.Errorf("tip new OID = %s, want %s", d.RefTips[0].NewOID, childOID)
	}
}

func TestParseCommitParents_Root(t *testing.T) {
	body := []byte("tree abc\nauthor foo\n\nmsg\n")
	parents, err := parseCommitParents(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 0 {
		t.Errorf("root commit: want 0 parents, got %d", len(parents))
	}
}

func TestParseCommitParents_OneParent(t *testing.T) {
	parentHex := "0000000000000000000000000000000000000002"
	body := []byte("tree 0000000000000000000000000000000000000001\nparent " + parentHex + "\nauthor foo\n\nmsg\n")
	parents, err := parseCommitParents(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 1 {
		t.Fatalf("want 1 parent, got %d", len(parents))
	}
	want, _ := pack.ParseOID(parentHex)
	if parents[0] != want {
		t.Errorf("parent = %s, want %s", parents[0], want)
	}
}

func TestParseCommitParents_TwoParents(t *testing.T) {
	p1 := "0000000000000000000000000000000000000001"
	p2 := "0000000000000000000000000000000000000002"
	body := []byte("tree 0000000000000000000000000000000000000000\nparent " + p1 + "\nparent " + p2 + "\nauthor foo\n\nmerge\n")
	parents, err := parseCommitParents(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 2 {
		t.Fatalf("want 2 parents, got %d", len(parents))
	}
}
