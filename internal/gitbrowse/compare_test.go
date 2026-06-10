package gitbrowse

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// compareFixture builds a two-commit repo tailored to the Compare test:
//   - base commit: a.txt="one\n"
//   - head commit: a.txt="two\n" (modified) AND c.txt="new\n" (added)
//
// It mirrors the construction pattern of fixture() in fixture_test.go
// (git init/commit -> importer.Import -> mirror.NewManager -> NewService).
func compareFixture(t *testing.T) (svc *Service, tenant, repo, baseOID, headOID string) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires git binary")
	}
	work := t.TempDir()
	srcBare := filepath.Join(t.TempDir(), "src.git")

	git := func(dir string, args ...string) string {
		c := exec.Command("git", args...)
		if dir != "" {
			c.Dir = dir
		}
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ann", "GIT_AUTHOR_EMAIL=ann@x",
			"GIT_COMMITTER_NAME=Ann", "GIT_COMMITTER_EMAIL=ann@x")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, content string) {
		p := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("", "init", "-q", "-b", "main", work)
	write("a.txt", "one\n")
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "base")
	baseOID = git(work, "rev-parse", "HEAD")

	write("a.txt", "two\n")
	write("c.txt", "new\n")
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "head")
	headOID = git(work, "rev-parse", "HEAD")

	git("", "clone", "-q", "--bare", work, srcBare)

	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := importer.Import(context.Background(), store, importer.Options{
		SourceDir:     srcBare,
		Tenant:        "acme",
		Repo:          "demo",
		Actor:         "test",
		DefaultBranch: "refs/heads/main",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	mgr, err := mirror.NewManager(t.TempDir(), store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	return NewService(store, mgr, 0, nil), "acme", "demo", baseOID, headOID
}

func TestCompare_ModifiedAndAdded(t *testing.T) {
	svc, tenant, repo, baseOID, headOID := compareFixture(t)
	cmp, err := svc.Compare(context.Background(), tenant, repo, baseOID, headOID)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(cmp.Files) != 2 {
		t.Fatalf("want 2 files, got %d: %+v", len(cmp.Files), cmp.Files)
	}
	var sawMod, sawAdd bool
	for _, f := range cmp.Files {
		if f.NewPath == "a.txt" && f.Status == "M" {
			sawMod = true
		}
		if f.NewPath == "c.txt" && f.Status == "A" {
			sawAdd = true
		}
	}
	if !sawMod || !sawAdd {
		t.Fatalf("missing M a.txt / A c.txt: %+v", cmp.Files)
	}
	if cmp.Additions == 0 {
		t.Fatalf("additions should be > 0")
	}
}
