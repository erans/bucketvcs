//go:build stress

package gateway

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestStress_Push1000Commits(t *testing.T) {
	const N = 1000

	work := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustExecStress(t, work, "git", "init", "--initial-branch=main")
	mustExecStress(t, work, "git", "config", "user.email", "stress@stress")
	mustExecStress(t, work, "git", "config", "user.name", "stress")
	for i := 0; i < N; i++ {
		path := filepath.Join(work, "f.txt")
		_ = os.WriteFile(path, []byte("v"+strconv.Itoa(i)+"\n"), 0o644)
		mustExecStress(t, work, "git", "add", "f.txt")
		mustExecStress(t, work, "git", "commit", "-m", "c"+strconv.Itoa(i))
	}

	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed an empty repo (M3 push requires the repo to exist).
	emptyBare := filepath.Join(t.TempDir(), "seed.git")
	mustExecStress(t, "", "git", "init", "--bare", emptyBare)
	if _, err := importer.Import(context.Background(), store, importer.Options{
		SourceDir: emptyBare, Tenant: "fx", Repo: "stress", Actor: "stress",
		DefaultBranch: "refs/heads/main",
	}); err != nil {
		t.Fatalf("seed Import: %v", err)
	}

	srv, err := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "stress", AuthStore: newPermissiveAuthStore(t, "fx", "stress")})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	const limit = 120 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, "git", "-C", work, "push", ts.URL+"/fx/stress.git", "HEAD:refs/heads/main")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("git push timed out after %v (limit=%v)\n%s", time.Since(start), limit, out)
		}
		t.Fatalf("git push: %v\n%s", err, out)
	}
	elapsed := time.Since(start)
	t.Logf("stress push of %d commits completed in %v", N, elapsed)
}

func mustExecStress(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}
