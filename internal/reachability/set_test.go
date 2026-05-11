package reachability_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os/exec"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/reachability/rtest"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestLoad_LegacyManifest_ReturnsErrNoIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()
	k, _ := keys.NewRepo("t", "r")
	body := manifest.Body{DefaultBranch: "main", Refs: map[string]string{}}
	_, err = reachability.Load(ctx, store, k, body)
	if !errors.Is(err, reachability.ErrNoIndex) {
		t.Fatalf("err = %v, want ErrNoIndex", err)
	}
}

func TestLoad_BaseOnly_HasCommits(t *testing.T) {
	fx := rtest.NewBaseOnlyRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !set.Has(fx.C) {
		t.Error("Set.Has(C) = false, want true")
	}
	if !set.Has(fx.A) {
		t.Error("Set.Has(A) = false, want true")
	}
	if !set.Has(fx.B) {
		t.Error("Set.Has(B) = false, want true")
	}
}

func TestSet_Parents_FromDelta(t *testing.T) {
	fx := rtest.NewBaseWithDeltaRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	parents := set.Parents(fx.D)
	if len(parents) != 1 || parents[0] != fx.C {
		t.Fatalf("Parents(D) = %v, want [C]", parents)
	}
}

func TestSet_Generation(t *testing.T) {
	fx := rtest.NewBaseWithDeltaRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if g, ok := set.Generation(fx.A); !ok || g != 1 {
		t.Errorf("gen(A) = (%d, %v), want (1, true)", g, ok)
	}
	if g, ok := set.Generation(fx.D); !ok || g != 4 {
		t.Errorf("gen(D) = (%d, %v), want (4, true)", g, ok)
	}
}

func TestSet_WalkAncestors_VisitsAll(t *testing.T) {
	fx := rtest.NewBaseWithDeltaRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	visited := map[pack.OID]bool{}
	if err := set.WalkAncestors([]pack.OID{fx.D}, func(o pack.OID, _ uint32) error {
		visited[o] = true
		return nil
	}); err != nil {
		t.Fatalf("WalkAncestors: %v", err)
	}
	for name, want := range map[string]pack.OID{"A": fx.A, "B": fx.B, "C": fx.C, "D": fx.D} {
		if !visited[want] {
			t.Errorf("missing %s (%x)", name, want[:4])
		}
	}
}

func TestSet_RefTips_AppliesDeltas(t *testing.T) {
	fx := rtest.NewBaseWithDeltaRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if set.RefTips()["refs/heads/main"] != fx.D {
		t.Fatalf("main tip mismatch: got %v, want %v", set.RefTips()["refs/heads/main"], fx.D)
	}
}

// TestSet_DeleteOnlyPush_ChainReplay_RemovesPhantomRef verifies that a
// delete-tip RefTipDiff (NewOID==zero) in a later delta removes a ref that
// was created by an earlier delta, even though body.Refs no longer contains
// the ref. This guards against the round-8 "skip delete-only push" regression
// where the chain replay would resurrect a deleted ref as a phantom.
func TestSet_DeleteOnlyPush_ChainReplay_RemovesPhantomRef(t *testing.T) {
	gitAvailableSkip(t)
	ctx := context.Background()

	// Build a base repo (A→B→C); body.Refs has NO "refs/heads/feature"
	// (it was already deleted before this snapshot was taken).
	fx := rtest.NewBaseOnlyRepo(t)

	// delta_1 creates refs/heads/feature pointing at A.
	delta1 := deltaindex.Delta{
		RefTips: []deltaindex.RefTipDiff{
			{RefName: "refs/heads/feature", OldOID: pack.OID{}, NewOID: fx.A},
		},
	}
	delta1Bytes, err := deltaindex.Encode(delta1)
	if err != nil {
		t.Fatalf("Encode delta1: %v", err)
	}
	delta1Sum := sha256Sum(delta1Bytes)
	delta1Key := fx.Keys.ReachabilityDeltaKey(delta1Sum)
	uploadBytesStore(t, ctx, fx.Store, delta1Bytes, delta1Key)

	// delta_2 deletes refs/heads/feature (NewOID==zero).
	delta2 := deltaindex.Delta{
		RefTips: []deltaindex.RefTipDiff{
			{RefName: "refs/heads/feature", OldOID: fx.A, NewOID: pack.OID{}},
		},
	}
	delta2Bytes, err := deltaindex.Encode(delta2)
	if err != nil {
		t.Fatalf("Encode delta2: %v", err)
	}
	delta2Sum := sha256Sum(delta2Bytes)
	delta2Key := fx.Keys.ReachabilityDeltaKey(delta2Sum)
	uploadBytesStore(t, ctx, fx.Store, delta2Bytes, delta2Key)

	// Construct a body where Refs does NOT contain refs/heads/feature
	// (it was deleted before this snapshot). The chain contains both deltas.
	body := fx.Body
	body.Indexes.Reachability = &manifest.ReachabilityRef{
		BaseManifest: "v00000001",
		Deltas: []manifest.IndexRef{
			{Key: delta1Key, Hash: delta1Sum},
			{Key: delta2Key, Hash: delta2Sum},
		},
	}

	set, err := reachability.Load(ctx, fx.Store, fx.Keys, body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tips := set.RefTips()
	if _, exists := tips["refs/heads/feature"]; exists {
		t.Errorf("phantom ref: refs/heads/feature present in RefTips after chain replay, want absent; tips=%v", tips)
	}
}

// gitAvailableSkip skips t if git is not on PATH.
func gitAvailableSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// sha256Sum returns the hex-encoded SHA-256 of b.
func sha256Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// uploadBytesStore stores b under key in store; fatal on error.
func uploadBytesStore(t *testing.T, ctx context.Context, store storage.ObjectStore, b []byte, key string) {
	t.Helper()
	if _, err := store.PutIfAbsent(ctx, key, bytes.NewReader(b), nil); err != nil {
		t.Fatalf("PutIfAbsent(%s): %v", key, err)
	}
}
