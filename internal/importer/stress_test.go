//go:build stress

package importer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// TestStress_1000CommitsRoundTrip imports a 1000-commit synthetic repo
// and asserts the resulting .bvom + .bvcg sizes are bounded.
//
// Run with: go test -tags stress -run TestStress_1000CommitsRoundTrip ./internal/importer/
//
// Not a ship gate; a smoke test catching ~10x format-size regressions.
func TestStress_1000CommitsRoundTrip(t *testing.T) {
	skipIfNoGit(t)
	work := t.TempDir()
	mustRun := func(args ...string) {
		t.Helper()
		if out, err := gitcli.RunForTest(work, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustRun("init", "--initial-branch=main")
	for i := 0; i < 1000; i++ {
		path := filepath.Join(work, fmt.Sprintf("f%d", i%50))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("rev=%d\n", i)), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		mustRun("add", "-A")
		mustRun("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "-m", fmt.Sprintf("c%d", i))
	}
	bare := filepath.Join(t.TempDir(), "bare")
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}

	store := newTestStore(t)
	res, err := Import(context.Background(), store, Options{
		SourceDir: bare, Tenant: "stress", Repo: "r", Actor: "stress",
		DefaultBranch: "refs/heads/main",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	t.Logf("imported %d objects, pack=%s, manifest_version=%d",
		res.ObjectCount, res.PackID, res.ManifestVersion)

	// Read .bvom + .bvcg sizes back via storage interface.
	k, err := keys.NewRepo("stress", "r")
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	bvomBody, err := getAll(context.Background(), store, k.ObjectMapKey(res.ObjectMapHash))
	if err != nil {
		t.Fatalf("get bvom: %v", err)
	}
	bvcgBody, err := getAll(context.Background(), store, k.CommitGraphKey(res.CommitGraphHash))
	if err != nil {
		t.Fatalf("get bvcg: %v", err)
	}
	combined := int64(len(bvomBody) + len(bvcgBody))
	const cap = int64(128 * 1024 * 1024)
	if combined > cap {
		t.Fatalf(".bvom + .bvcg = %d bytes, cap %d (smoke regression)", combined, cap)
	}
	t.Logf(".bvom=%d .bvcg=%d combined=%d (cap=%d)",
		len(bvomBody), len(bvcgBody), combined, cap)

	// Round-trip exports: confirm pack/idx/refs all materialize.
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := exporter.Export(context.Background(), store, exporter.Options{
		Tenant: "stress", Repo: "r", DestDir: dst,
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
}

// getAll reads an entire object's bytes from store via storage.ObjectStore.
func getAll(ctx context.Context, store storage.ObjectStore, key string) ([]byte, error) {
	obj, err := store.Get(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	defer obj.Body.Close()
	return io.ReadAll(obj.Body)
}
