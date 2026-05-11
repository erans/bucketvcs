package reachability_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/rtest"
)

// TestM10_EndToEnd_LocalfsSmoke verifies the M10 components plug together
// end-to-end against a real localfs store. It builds a fixture with a
// 3-commit base (A→B→C) and a 1-commit delta (D, gen=4), then exercises
// Load, Has, Generation, and WalkAncestors.
func TestM10_EndToEnd_LocalfsSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("M10 smoke test — skipped in -short mode")
	}
	ctx := context.Background()
	fx := rtest.NewBaseWithDeltaRepo(t)

	// Load the Set from the fixture store.
	set, err := reachability.Load(ctx, fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// D is in the delta; Has(D) must be true.
	if !set.Has(fx.D) {
		t.Fatalf("Has(D) = false, want true")
	}

	// A, B, C are in the base index.
	for name, oid := range map[string]pack.OID{"A": fx.A, "B": fx.B, "C": fx.C} {
		if !set.Has(oid) {
			t.Errorf("Has(%s) = false, want true", name)
		}
	}

	// Generation(D) must be 4.
	if g, ok := set.Generation(fx.D); !ok || g != 4 {
		t.Fatalf("Generation(D) = (%d, %v), want (4, true)", g, ok)
	}

	// Walk all ancestors from D; should visit {A, B, C, D} — at least 4 commits.
	var visited []pack.OID
	if err := set.WalkAncestors([]pack.OID{fx.D}, func(oid pack.OID, gen uint32) error {
		visited = append(visited, oid)
		return nil
	}); err != nil {
		t.Fatalf("WalkAncestors: %v", err)
	}
	if len(visited) < 4 {
		t.Fatalf("WalkAncestors visited %d commits, want >= 4", len(visited))
	}

	// D must appear in the visited list (it's the root of the walk).
	foundD := false
	for _, oid := range visited {
		if oid == fx.D {
			foundD = true
			break
		}
	}
	if !foundD {
		t.Errorf("WalkAncestors did not visit D; visited: %v", visited)
	}

	// RefTips should include refs/heads/main pointing at D after delta.
	tips := set.RefTips()
	mainTip, ok := tips["refs/heads/main"]
	if !ok {
		t.Errorf("RefTips missing refs/heads/main")
	} else if mainTip != fx.D {
		t.Errorf("RefTips[refs/heads/main] = %v, want D=%v", mainTip, fx.D)
	}

	_ = fmt.Sprintf("smoke passed: visited %d commits", len(visited))
}

// TestM10_EndToEnd_BaseOnlySmoke verifies that Load works for a repo with
// a base index but no delta chain (the pre-delta steady state).
func TestM10_EndToEnd_BaseOnlySmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("M10 smoke test — skipped in -short mode")
	}
	ctx := context.Background()
	fx := rtest.NewBaseOnlyRepo(t)

	set, err := reachability.Load(ctx, fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load (base-only): %v", err)
	}

	// A, B, C should all be reachable.
	for name, oid := range map[string]pack.OID{"A": fx.A, "B": fx.B, "C": fx.C} {
		if !set.Has(oid) {
			t.Errorf("Has(%s) = false, want true (base-only fixture)", name)
		}
	}

	// Walk from C; should visit at least A, B, C.
	var n int
	if err := set.WalkAncestors([]pack.OID{fx.C}, func(oid pack.OID, gen uint32) error {
		n++
		return nil
	}); err != nil {
		t.Fatalf("WalkAncestors (base-only): %v", err)
	}
	if n < 3 {
		t.Fatalf("WalkAncestors visited %d commits, want >= 3", n)
	}
}
