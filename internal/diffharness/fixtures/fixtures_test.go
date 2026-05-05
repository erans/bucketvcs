package fixtures

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
}

func TestRegistry_AllFixturesProduceFsckCleanRepos(t *testing.T) {
	skipIfNoGit(t)
	for name, build := range Registry {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bare")
			fx := build(t, dir)
			if fx.Name != name {
				t.Fatalf("fixture name mismatch: %q vs %q", fx.Name, name)
			}
			if err := gitcli.Fsck(context.Background(), dir, true); err != nil {
				t.Fatalf("fsck: %v", err)
			}
			if name != "empty" && len(fx.Refs) == 0 {
				t.Fatalf("%s: expected ≥1 ref", name)
			}
		})
	}
}

func TestRegistry_HasExpectedFixtures(t *testing.T) {
	want := []string{
		"empty", "single_commit", "linear_3_commits", "branch_and_merge",
		"lightweight_tag", "annotated_tag", "symref_head",
		"two_branches", "binary_blob", "deep_tree",
	}
	for _, n := range want {
		if _, ok := Registry[n]; !ok {
			t.Errorf("Registry missing %q", n)
		}
	}
}
