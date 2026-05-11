package reachability_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/rtest"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestGenLookup_FromBaseOnly(t *testing.T) {
	fx := rtest.NewBaseOnlyRepo(t)
	gl, err := reachability.LoadGenLookup(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("LoadGenLookup: %v", err)
	}
	// C is the tip commit (A→B→C), so generation should be 3.
	if g, ok := gl.Lookup(fx.C); !ok || g != 3 {
		t.Errorf("C = (%d, %v), want (3, true)", g, ok)
	}
	// A is the root commit, generation 1.
	if g, ok := gl.Lookup(fx.A); !ok || g != 1 {
		t.Errorf("A = (%d, %v), want (1, true)", g, ok)
	}
}

func TestGenLookup_DeltaShadowsBase(t *testing.T) {
	fx := rtest.NewBaseWithDeltaRepo(t)
	gl, err := reachability.LoadGenLookup(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("LoadGenLookup: %v", err)
	}
	// D is commit 4 (A=1, B=2, C=3, D=4).
	if g, ok := gl.Lookup(fx.D); !ok || g != 4 {
		t.Errorf("D = (%d, %v), want (4, true)", g, ok)
	}
}

func TestGenLookup_LegacyErrNoIndex(t *testing.T) {
	// A body with no CommitGraph/ObjectMap should return ErrNoIndex.
	body := manifest.Body{} // empty indexes
	_, err := reachability.LoadGenLookup(context.Background(), nil, nil, body)
	if !errors.Is(err, reachability.ErrNoIndex) {
		t.Fatalf("want ErrNoIndex, got %v", err)
	}
}

func TestGenLookup_NewGenLookup(t *testing.T) {
	// Test the map-constructor path.
	var zeroOID pack.OID
	gl := reachability.NewGenLookup(map[pack.OID]uint32{zeroOID: 5})
	if g, ok := gl.Lookup(zeroOID); !ok || g != 5 {
		t.Errorf("Lookup zero OID = (%d, %v), want (5, true)", g, ok)
	}
	var unknown pack.OID
	unknown[0] = 0xFF
	if _, ok := gl.Lookup(unknown); ok {
		t.Error("Lookup of absent OID should return false")
	}
}
