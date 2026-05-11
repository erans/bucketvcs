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

func TestSet_WalkBackOID_Found(t *testing.T) {
	fx := rtest.NewBaseWithDeltaRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n, err := set.WalkBackOID(fx.D.String(), fx.A.String(), 10)
	if err != nil || n != 3 {
		t.Fatalf("WalkBackOID(D, A, 10) = (%d, %v); want (3, nil)", n, err)
	}
}

func TestSet_WalkBackOID_NotFoundWithinBound(t *testing.T) {
	fx := rtest.NewBaseWithDeltaRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n, err := set.WalkBackOID(fx.D.String(), fx.A.String(), 2)
	if err != nil || n != -1 {
		t.Fatalf("WalkBackOID(D, A, 2) = (%d, %v); want (-1, nil)", n, err)
	}
}

func TestSet_WalkBackOID_FromEqualsTarget(t *testing.T) {
	fx := rtest.NewBaseOnlyRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n, err := set.WalkBackOID(fx.C.String(), fx.C.String(), 10)
	if err != nil || n != 0 {
		t.Fatalf("WalkBackOID(C, C, 10) = (%d, %v); want (0, nil)", n, err)
	}
}

func TestSet_IsAncestor(t *testing.T) {
	fx := rtest.NewBaseWithDeltaRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !set.IsAncestor(fx.A.String(), fx.D.String(), 10) {
		t.Errorf("A should be ancestor of D")
	}
	if set.IsAncestor(fx.D.String(), fx.A.String(), 10) {
		t.Errorf("D should NOT be ancestor of A")
	}
}

func TestSet_IsAncestor_SelfAncestry(t *testing.T) {
	fx := rtest.NewBaseOnlyRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !set.IsAncestor(fx.C.String(), fx.C.String(), 10) {
		t.Errorf("C should be its own ancestor (depth 0)")
	}
}

func TestSet_IsAncestor_DescendantMissingFromSet(t *testing.T) {
	fx := rtest.NewBaseOnlyRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const unknown = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if set.IsAncestor(fx.A.String(), unknown, 10) {
		t.Errorf("unknown descendant should yield false")
	}
}

func TestSet_IsAncestor_MalformedOID(t *testing.T) {
	fx := rtest.NewBaseOnlyRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if set.IsAncestor("not-an-oid", fx.C.String(), 10) {
		t.Errorf("malformed ancestor should yield false")
	}
	if set.IsAncestor(fx.A.String(), "also-bad", 10) {
		t.Errorf("malformed descendant should yield false")
	}
}

func TestSet_WalkBackOID_MalformedFromEqualsTarget(t *testing.T) {
	fx := rtest.NewBaseOnlyRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Even when from == target, a malformed OID must surface as an error;
	// the equality short-circuit cannot bypass input validation.
	if _, err := set.WalkBackOID("", "", 5); err == nil {
		t.Errorf("empty OIDs should error, not return (0, nil)")
	}
}

func TestSet_WalkBackOID_FromNotInSet(t *testing.T) {
	fx := rtest.NewBaseOnlyRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const unknown = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if _, err := set.WalkBackOID(unknown, fx.A.String(), 5); err == nil {
		t.Errorf("expected error when from is not in set")
	}
}

func TestSet_WalkBackOID_MalformedTarget(t *testing.T) {
	fx := rtest.NewBaseOnlyRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := set.WalkBackOID(fx.C.String(), "not-hex", 5); err == nil {
		t.Errorf("expected error on malformed target with valid from")
	}
}

func TestSet_WalkBackOID_UnreachableTarget(t *testing.T) {
	// Walk from C looking for an OID that parses fine but is not on
	// C's ancestor chain. Walk should exhaust normally and return -1.
	fx := rtest.NewBaseOnlyRepo(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const unreachable = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	n, err := set.WalkBackOID(fx.C.String(), unreachable, 10)
	if err != nil {
		t.Fatalf("WalkBackOID: %v", err)
	}
	if n != -1 {
		t.Errorf("got n=%d, want -1 (target unreachable)", n)
	}
}
