package reachability_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/rtest"
)

func TestSet_ShadowSemantics_LatestDeltaWins(t *testing.T) {
	fx := rtest.NewShadowedFixture(t)
	set, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g, ok := set.Generation(fx.A)
	if !ok || g != 99 {
		t.Fatalf("shadowed gen = (%d, %v), want (99, true)", g, ok)
	}
}
