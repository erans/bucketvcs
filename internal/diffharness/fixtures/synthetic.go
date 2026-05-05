package fixtures

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// writeFile is a thin wrapper preserving error handling style.
func writeFile(t *testing.T, dir, name string, body []byte) error {
	t.Helper()
	return os.WriteFile(filepath.Join(dir, name), body, 0o644)
}

func buildEmpty(t *testing.T, dir string) Fixture {
	work := initWork(t)
	buildBareFromWork(t, work, dir)
	return finalize(t, "empty", dir)
}

func buildSingleCommit(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "a\n", "init")
	buildBareFromWork(t, work, dir)
	return finalize(t, "single_commit", dir)
}

func buildLinear3(t *testing.T, dir string) Fixture {
	work := initWork(t)
	for _, c := range []struct{ content, msg string }{
		{"a\n", "1"},
		{"b\n", "2"},
		{"c\n", "3"},
	} {
		commitFile(t, work, "f", c.content, c.msg)
	}
	buildBareFromWork(t, work, dir)
	return finalize(t, "linear_3_commits", dir)
}

func buildBranchAndMerge(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "x\n", "base")
	mustGit(t, work, "checkout", "-b", "feature")
	commitFile(t, work, "g", "y\n", "feature")
	mustGit(t, work, "checkout", "main")
	commitFile(t, work, "h", "z\n", "main-2")
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@e",
		"merge", "--no-ff", "-m", "merge feature", "feature")
	buildBareFromWork(t, work, dir)
	return finalize(t, "branch_and_merge", dir)
}

func buildLightweightTag(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "x\n", "init")
	mustGit(t, work, "tag", "v1")
	buildBareFromWork(t, work, dir)
	return finalize(t, "lightweight_tag", dir)
}

func buildAnnotatedTag(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "x\n", "init")
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@e",
		"tag", "-a", "v1", "-m", "release v1")
	buildBareFromWork(t, work, dir)
	return finalize(t, "annotated_tag", dir)
}

func buildSymrefHead(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "x\n", "init")
	mustGit(t, work, "checkout", "-b", "dev")
	commitFile(t, work, "g", "y\n", "dev")
	buildBareFromWork(t, work, dir)
	if err := gitcli.SymbolicRefSet(context.Background(), dir, "HEAD", "refs/heads/dev"); err != nil {
		t.Fatalf("SymbolicRefSet: %v", err)
	}
	return finalize(t, "symref_head", dir)
}

func buildTwoBranchesDivergent(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "x\n", "base")
	mustGit(t, work, "checkout", "-b", "left")
	commitFile(t, work, "g", "y\n", "left-1")
	mustGit(t, work, "checkout", "-b", "right", "main")
	commitFile(t, work, "h", "z\n", "right-1")
	buildBareFromWork(t, work, dir)
	return finalize(t, "two_branches", dir)
}

func buildBlobWithBinaryContent(t *testing.T, dir string) Fixture {
	work := initWork(t)
	// 1 MiB pseudo-random blob (deterministic from a fixed seed for hermeticity).
	buf := make([]byte, 1024*1024)
	state := uint32(0x12345678)
	for i := range buf {
		state = state*1103515245 + 12345
		buf[i] = byte(state >> 16)
	}
	if err := writeFile(t, work, "blob.bin", buf); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit(t, work, "add", "blob.bin")
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@e",
		"commit", "-m", "binary blob")
	buildBareFromWork(t, work, dir)
	return finalize(t, "binary_blob", dir)
}

func buildDeepNestedTrees(t *testing.T, dir string) Fixture {
	work := initWork(t)
	deep := filepath.Join(work, "a", "b", "c", "d", "e", "f")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deep, "leaf"), []byte("L\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit(t, work, "add", "a")
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@e",
		"commit", "-m", "deep tree")
	buildBareFromWork(t, work, dir)
	return finalize(t, "deep_tree", dir)
}
