package diffharness

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/importer"
)

// cloneOracleSkip lists fixtures that legitimately cannot pass the clone-
// equivalence oracle as currently formulated. Each entry must explain why.
//
// The clone uses --mirror (symmetric to push --mirror) so all refs and HEAD
// are advertised + copied; no fixtures currently need to be skipped, but the
// map is kept as a place to document any future exceptions discovered while
// the registry grows.
var cloneOracleSkip = map[string]string{}

// TestOracle_CloneEquivalence verifies that for each fixture, a `git clone`
// against the gateway produces a bare clone whose reachable objects match
// the original fixture exactly.
func TestOracle_CloneEquivalence(t *testing.T) {
	skipIfNoGit(t)
	for name, build := range fixtures.Registry {
		name, build := name, build
		t.Run(name, func(t *testing.T) {
			if reason, skip := cloneOracleSkip[name]; skip {
				t.Skip(reason)
			}
			t.Parallel()
			workDir := t.TempDir()
			srcDir := filepath.Join(workDir, "src")
			fx := build(t, srcDir)
			gitFsck(t, srcDir)

			store := newTestStore(t)
			tenant := "fx"
			repoID := name
			defaultBranch := ""
			if len(fx.Refs) == 0 {
				defaultBranch = "refs/heads/main"
			}
			if _, err := importer.Import(context.Background(), store, importer.Options{
				SourceDir:     srcDir,
				Tenant:        tenant,
				Repo:          repoID,
				Actor:         "harness",
				DefaultBranch: defaultBranch,
			}); err != nil {
				t.Fatalf("Import: %v", err)
			}

			srv, err := gateway.NewServer(store, gateway.Options{
				MirrorDir: t.TempDir(),
				Version:   "test",
				AuthStore: newDiffharnessAuthStore(t, tenant, repoID),
			})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			t.Cleanup(func() { _ = srv.Close() })
			ts := httptest.NewServer(srv)
			t.Cleanup(ts.Close)

			dstDir := filepath.Join(workDir, "clone.git")
			// --mirror clones every ref (refs/heads/*, refs/tags/*,
			// refs/replace/*, etc.) symmetric to push --mirror, so the
			// equivalence comparison covers the full advertisement.
			cmd := exec.Command("git", "clone", "--mirror",
				"-c", "protocol.version=2",
				ts.URL+"/"+tenant+"/"+repoID+".git", dstDir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git clone --mirror: %v\n%s", err, out)
			}

			// Empty fixture: clone produces a bare repo with HEAD pointing at
			// refs/heads/main but no actual ref. The reachable-object set is
			// trivially empty on both sides; M2 round-trip already covers it.
			if len(fx.Refs) == 0 {
				return
			}

			gitFsck(t, dstDir)

			srcRefs := gitShowRef(t, srcDir)
			dstRefs := gitShowRef(t, dstDir)
			if !equalRefs(srcRefs, dstRefs) {
				t.Fatalf("refs differ.\nsrc=%v\ndst=%v", srcRefs, dstRefs)
			}
			srcOIDs := gitRevListAllObjects(t, srcDir)
			dstOIDs := gitRevListAllObjects(t, dstDir)
			if !equalOIDLists(srcOIDs, dstOIDs) {
				t.Fatalf("reachable OIDs differ.\nsrc=%v\ndst=%v", srcOIDs, dstOIDs)
			}
			for _, oid := range srcOIDs {
				got := gitCatFilePretty(t, dstDir, oid)
				want := gitCatFilePretty(t, srcDir, oid)
				ensureBytesEqual(t, "cat-file -p "+oid, got, want)
			}
		})
	}
}
