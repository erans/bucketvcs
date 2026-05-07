package fixtures

import (
	"bytes"
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

func buildReplaceRef(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "original\n", "original")
	mustGit(t, work, "checkout", "-b", "tmp")
	commitFile(t, work, "f", "replacement\n", "replacement")
	// Get the original commit's OID and the replacement's.
	origOIDBytes, err := gitcli.RunForTest(work, "rev-parse", "main")
	if err != nil {
		t.Fatalf("rev-parse main: %v: %s", err, origOIDBytes)
	}
	origOID := string(bytes.TrimSpace(origOIDBytes))
	replOIDBytes, err := gitcli.RunForTest(work, "rev-parse", "tmp")
	if err != nil {
		t.Fatalf("rev-parse tmp: %v: %s", err, replOIDBytes)
	}
	replOID := string(bytes.TrimSpace(replOIDBytes))
	mustGit(t, work, "checkout", "main")
	mustGit(t, work, "branch", "-D", "tmp")
	mustGit(t, work, "replace", origOID, replOID)
	buildBareFromWork(t, work, dir)
	return finalize(t, "replace_ref", dir)
}

func buildForcePushOverwrite(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "a.txt", "first\n", "first")
	mustGit(t, work, "branch", "before")
	mustGit(t, work, "checkout", "-B", "after")
	// Diverge from "before" by amending the commit (different commit OID,
	// same tree shape would also work but amend is simpler).
	mustGit(t, work, "commit", "--amend", "-m", "first-amended")
	mustGit(t, work, "checkout", "before")
	buildBareFromWork(t, work, dir)
	return finalize(t, "force_push_overwrite", dir)
}

func buildDeleteBranch(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "a.txt", "main\n", "init")
	mustGit(t, work, "branch", "to-delete")
	buildBareFromWork(t, work, dir)
	return finalize(t, "delete_branch", dir)
}

func buildAtomicMultiRefPush(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "a.txt", "main\n", "init")
	mustGit(t, work, "checkout", "-B", "topic-a")
	commitFile(t, work, "a.txt", "topic-a\n", "topic-a")
	mustGit(t, work, "checkout", "main")
	mustGit(t, work, "checkout", "-B", "topic-b")
	commitFile(t, work, "b.txt", "topic-b\n", "topic-b")
	mustGit(t, work, "checkout", "main")
	buildBareFromWork(t, work, dir)
	return finalize(t, "atomic_multi_ref_push", dir)
}

func buildIncrementalFetchAfterPush(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "a.txt", "first\n", "first")
	commitFile(t, work, "a.txt", "second\n", "second")
	buildBareFromWork(t, work, dir)
	return finalize(t, "incremental_fetch_after_push", dir)
}

func buildShallowCloneDepth1(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "a.txt", "v1\n", "v1")
	commitFile(t, work, "a.txt", "v2\n", "v2")
	commitFile(t, work, "a.txt", "v3\n", "v3")
	buildBareFromWork(t, work, dir)
	return finalize(t, "shallow_clone_depth_1", dir)
}
