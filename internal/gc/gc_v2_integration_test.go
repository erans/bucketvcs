package gc_test

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest/manifesttest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// TestSweep_V2Body_ShardKeysNotDeleted is an end-to-end GC integration test
// for M12 Phase 7. It verifies that a full mark+sweep cycle against a repo
// with a v2 (sharded) manifest does NOT delete any ref-shard objects.
func TestSweep_V2Body_ShardKeysNotDeleted(t *testing.T) {
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	ctx := context.Background()
	r, err := repo.Create(ctx, store, "acme", "demo", repo.CreateOptions{Actor: "u_test"})
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	k, err := keys.NewRepo("acme", "demo")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	// Build a v2 body with two refs that land in distinct shards.
	// Ref-sharding uses hash_v1 on the REF NAME (not the OID), so verify
	// shard separation before committing.
	const (
		nameA = "refs/heads/main"
		nameB = "refs/heads/release"
	)
	if refstore.ShardKey(nameA) == refstore.ShardKey(nameB) {
		t.Fatalf("test fixture: %q and %q collide in shard %q; pick different names", nameA, nameB, refstore.ShardKey(nameA))
	}
	refs := map[string]string{
		nameA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		nameB: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	body, err := manifesttest.MakeShardedBody(ctx, store, k, nameA, refs)
	if err != nil {
		t.Fatalf("MakeShardedBody: %v", err)
	}
	if len(body.RefShards) < 2 {
		t.Fatalf("MakeShardedBody produced %d shard(s); need at least 2", len(body.RefShards))
	}

	// Commit the v2 body into the repo manifest so GC picks it up via ReadRoot.
	bodyJSON, err := manifest.MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if _, err := r.Commit(ctx,
		tx.Body{Type: "reshard", Actor: "u_test"},
		func(_ *repo.RootView) ([]byte, error) { return bodyJSON, nil },
	); err != nil {
		t.Fatalf("r.Commit: %v", err)
	}

	// Mark phase: zero-retention so all unreferenced objects are immediately eligible.
	mark, err := gc.RunMark(ctx, store, r, gc.MarkOptions{
		Now:              time.Now,
		RetentionSeconds: 0,
	})
	if err != nil {
		t.Fatalf("RunMark: %v", err)
	}
	if err := marks.Write(ctx, store, k, mark); err != nil {
		t.Fatalf("marks.Write: %v", err)
	}

	// Sweep phase with retention=0 so anything unreachable is deleted immediately.
	fixedNow := time.Now().Add(2 * time.Second)
	if _, err := gc.RunSweep(ctx, store, r, mark, gc.SweepOptions{
		Now: func() time.Time { return fixedNow },
	}); err != nil {
		t.Fatalf("RunSweep: %v", err)
	}

	// Assert every ref-shard object survived.
	for _, s := range body.RefShards {
		if _, err := store.Head(ctx, s.Key); err != nil {
			t.Errorf("ref-shard key %q was deleted by sweep: %v", s.Key, err)
		}
	}
}
