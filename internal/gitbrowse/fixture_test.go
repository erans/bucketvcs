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

// fixture imports a synthetic repo into a localfs store and returns a ready
// Service plus (tenant, repo) and a map of useful OIDs. The repo has:
//   - branch main: a.txt, README.md, sub/b.txt, bin.dat (binary)
//   - branch feature/foo: adds c.txt (tests slash-ref disambiguation)
//   - tag v1.0 on main's first commit
//   - two commits on main (so log/diff have content)
func fixture(t *testing.T) (svc *Service, tenant, repo string, oids map[string]string) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires git binary")
	}
	work := t.TempDir()
	srcBare := filepath.Join(t.TempDir(), "src.git")
	oids = map[string]string{}

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
	write("a.txt", "hello\n")
	write("README.md", "# Demo\n\nHello *world* & <b>safe</b>.\n")
	write("sub/b.txt", "world\n")
	if err := os.WriteFile(filepath.Join(work, "bin.dat"), []byte{0x00, 0x01, 0x02, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "init")
	oids["c1"] = git(work, "rev-parse", "HEAD")
	git(work, "tag", "v1.0")

	write("a.txt", "hello again\n")
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "update a")
	oids["c2"] = git(work, "rev-parse", "HEAD")

	git(work, "checkout", "-q", "-b", "feature/foo")
	write("c.txt", "branch file\n")
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "add c on branch")
	oids["feat"] = git(work, "rev-parse", "HEAD")
	git(work, "checkout", "-q", "main")

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

	return NewService(store, mgr, 0), "acme", "demo", oids
}
