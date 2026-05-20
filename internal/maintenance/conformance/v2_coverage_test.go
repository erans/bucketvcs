// v2_coverage_test.go exercises assertReachable (and transitively
// runBundleSolo's ref-lookup path) against a v2 sharded manifest body.
// The test seeds a v1 repo via SeedRepoFromImport, then rewrites the
// manifest with a sharded body (same packs, same refs) to confirm that
// assertReachable no longer t.Skipf's on v2 and instead reads refs
// through refstore.List.
package conformance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest/manifesttest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// TestAssertReachable_V2ShardedBody verifies that assertReachable works
// against a v2 sharded body (no t.Skipf guard fires).
func TestAssertReachable_V2ShardedBody(t *testing.T) {
	mtest.GitAvailable(t)

	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}

	// Seed a v1 repo.
	mtest.SeedRepoFromImport(t, s, "acme", "site")
	ctx := context.Background()

	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}

	// Read the v1 body to capture packs and refs.
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var v1Body manifest.Body
	if err := json.Unmarshal(view.Body, &v1Body); err != nil {
		t.Fatalf("Unmarshal v1 body: %v", err)
	}

	// Build a v2 body: same packs + default branch, but refs sharded.
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}
	v2Body, err := manifesttest.MakeShardedBody(ctx, s, k, v1Body.DefaultBranch, v1Body.Refs)
	if err != nil {
		t.Fatalf("MakeShardedBody: %v", err)
	}
	// Carry over the pack entries from the v1 body so assertReachable
	// can download them into the bare repo.
	v2Body.Packs = v1Body.Packs

	bodyBytes, err := manifest.MarshalBody(v2Body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if _, err := r.Commit(ctx, tx.Body{Type: "test_v2_reshard", Actor: "u_test"},
		func(_ *repo.RootView) ([]byte, error) { return bodyBytes, nil },
	); err != nil {
		t.Fatalf("Commit v2 body: %v", err)
	}

	// assertReachable must succeed without calling t.Skipf.
	assertReachable(t, s, r)
}
