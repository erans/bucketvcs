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

// logPathFixture builds a two-commit repo tailored to the LogPath test:
//   - c1: adds dir/a.txt + dir/b.txt
//   - c2: modifies dir/a.txt
//
// It mirrors compareFixture()/fixture() construction
// (git init/commit -> importer.Import -> mirror.NewManager -> NewService).
func logPathFixture(t *testing.T) (svc *Service, tenant, repo, headOID string) {
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
	write("dir/a.txt", "one\n")
	write("dir/b.txt", "bee\n")
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "c1")

	write("dir/a.txt", "two\n")
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "c2")
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

	return NewService(store, mgr, 0, nil), "acme", "demo", headOID
}

func TestLogPath_FileVsDir(t *testing.T) {
	svc, tenant, repo, headOID := logPathFixture(t)
	ctx := context.Background()

	fileCommits, more, err := svc.LogPath(ctx, tenant, repo, headOID, "dir/a.txt", 0, 50)
	if err != nil {
		t.Fatalf("LogPath file: %v", err)
	}
	if len(fileCommits) != 2 || more {
		t.Fatalf("want 2 commits for dir/a.txt, got %d more=%v", len(fileCommits), more)
	}

	dirCommits, _, err := svc.LogPath(ctx, tenant, repo, headOID, "dir", 0, 50)
	if err != nil {
		t.Fatalf("LogPath dir: %v", err)
	}
	if len(dirCommits) != 2 {
		t.Fatalf("want 2 commits for dir/, got %d", len(dirCommits))
	}

	none, _, err := svc.LogPath(ctx, tenant, repo, headOID, "nope.txt", 0, 50)
	if err != nil {
		t.Fatalf("LogPath missing: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("want 0 commits for missing path, got %d", len(none))
	}
}
