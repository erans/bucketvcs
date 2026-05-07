// Package fixtures defines the synthetic-repo corpus used by the M2
// differential harness. Each builder authors a bare git repo at the
// given dir and returns a Fixture describing its refs and reachable
// object set, computed via internal/gitcli.
package fixtures

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// Fixture describes a built synthetic repo.
type Fixture struct {
	Name    string
	Refs    map[string]string // ref name -> hex OID
	AllOIDs []string          // reachable OIDs from rev-list --all
}

// Builder authors a bare git repo at dir and returns its Fixture.
type Builder func(t *testing.T, dir string) Fixture

// Registry maps fixture names to builders.
var Registry = map[string]Builder{
	"empty":                        buildEmpty,
	"single_commit":                buildSingleCommit,
	"linear_3_commits":             buildLinear3,
	"branch_and_merge":             buildBranchAndMerge,
	"lightweight_tag":              buildLightweightTag,
	"annotated_tag":                buildAnnotatedTag,
	"symref_head":                  buildSymrefHead,
	"two_branches":                 buildTwoBranchesDivergent,
	"binary_blob":                  buildBlobWithBinaryContent,
	"deep_tree":                    buildDeepNestedTrees,
	"replace_ref":                  buildReplaceRef,
	"force_push_overwrite":         buildForcePushOverwrite,
	"delete_branch":                buildDeleteBranch,
	"atomic_multi_ref_push":        buildAtomicMultiRefPush,
	"incremental_fetch_after_push": buildIncrementalFetchAfterPush,
	"shallow_clone_depth_1":        buildShallowCloneDepth1,
}

// buildBareFromWork clones a non-bare working repo into a bare repo at dir.
func buildBareFromWork(t *testing.T, work, dir string) {
	t.Helper()
	if err := gitcli.CloneBareMirror(context.Background(), work, dir); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
}

// finalize populates Refs and AllOIDs for the bare repo at dir.
func finalize(t *testing.T, name, dir string) Fixture {
	t.Helper()
	refs, err := gitcli.ShowRef(context.Background(), dir)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	oids, err := gitcli.RevListAllObjects(context.Background(), dir)
	if err != nil {
		t.Fatalf("RevListAllObjects: %v", err)
	}
	return Fixture{Name: name, Refs: refs, AllOIDs: oids}
}

// commitFile authors a file and commits it. Hermetic env.
func commitFile(t *testing.T, work, name, content, msg string) {
	t.Helper()
	if err := writeFile(t, work, name, []byte(content)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit(t, work, "add", name)
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", msg)
}

func mustGit(t *testing.T, work string, args ...string) {
	t.Helper()
	if out, err := gitcli.RunForTest(work, args...); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// initWork makes a non-bare working repo, returning its path.
func initWork(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	mustGit(t, work, "init", "--initial-branch=main")
	return work
}
