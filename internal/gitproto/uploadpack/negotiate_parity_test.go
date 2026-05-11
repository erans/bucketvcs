package uploadpack_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitproto/uploadpack"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/rtest"
)

// referenceWalk is a hand-written BFS implementation that computes the
// expected shipping plan independently from Negotiate. It uses the same
// Set primitives but implements its own traversal loop, giving us a
// meaningful algorithm-vs-algorithm comparison (distinct code paths with
// equivalent semantics).
//
// TODO: replace with a full git-upload-pack oracle (shell out to `git
// upload-pack --stateless-rpc` against a materialised bare repo) once
// the mirror/fixture infrastructure makes it straightforward to do so.
// The test is left as synthetic for now to avoid pulling in hundreds of
// lines of pack-streaming and pkt-line encoding for the oracle alone.
func referenceWalk(s *reachability.Set, wants, haves []pack.OID) []pack.OID {
	// Compute have-set via BFS.
	haveSet := make(map[pack.OID]bool)
	queue := append([]pack.OID(nil), haves...)
	visited := make(map[pack.OID]bool)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		if !s.Has(cur) {
			continue
		}
		haveSet[cur] = true
		for _, p := range s.Parents(cur) {
			if !visited[p] {
				queue = append(queue, p)
			}
		}
	}

	// Walk wants; collect commits not in have-set.
	var result []pack.OID
	seen := make(map[pack.OID]bool)
	queue = append([]pack.OID(nil), wants...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if !s.Has(cur) {
			continue
		}
		if !haveSet[cur] {
			result = append(result, cur)
		}
		for _, p := range s.Parents(cur) {
			if !seen[p] {
				queue = append(queue, p)
			}
		}
	}
	return result
}

func TestNegotiate_ParityAgainstSyntheticOracle_NoHaves(t *testing.T) {
	ctx := context.Background()
	fx := rtest.NewBaseOnlyRepo(t)
	set, err := reachability.Load(ctx, fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	wants := []pack.OID{fx.C}
	haves := []pack.OID(nil)

	plan, err := uploadpack.Negotiate(ctx, set, uploadpack.NegotiateInput{
		Wants: wants,
		Haves: haves,
		Done:  true,
	})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}

	oracle := referenceWalk(set, wants, haves)
	if !sameSet(plan.Commits, oracle) {
		t.Fatalf("Negotiate = %v, oracle = %v: mismatch", plan.Commits, oracle)
	}
}

func TestNegotiate_ParityAgainstSyntheticOracle_WithHave(t *testing.T) {
	ctx := context.Background()
	fx := rtest.NewBaseOnlyRepo(t)
	set, err := reachability.Load(ctx, fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	wants := []pack.OID{fx.C}
	haves := []pack.OID{fx.A}

	plan, err := uploadpack.Negotiate(ctx, set, uploadpack.NegotiateInput{
		Wants: wants,
		Haves: haves,
		Done:  true,
	})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}

	oracle := referenceWalk(set, wants, haves)
	if !sameSet(plan.Commits, oracle) {
		t.Fatalf("Negotiate = %v, oracle = %v: mismatch", plan.Commits, oracle)
	}
}

func TestNegotiate_ParityAgainstSyntheticOracle_WithDelta(t *testing.T) {
	ctx := context.Background()
	fx := rtest.NewBaseWithDeltaRepo(t)
	set, err := reachability.Load(ctx, fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Want D (tip), have B — expect C and D shipped.
	wants := []pack.OID{fx.D}
	haves := []pack.OID{fx.B}

	plan, err := uploadpack.Negotiate(ctx, set, uploadpack.NegotiateInput{
		Wants: wants,
		Haves: haves,
		Done:  true,
	})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}

	oracle := referenceWalk(set, wants, haves)
	if !sameSet(plan.Commits, oracle) {
		t.Fatalf("Negotiate = %v, oracle = %v: mismatch", plan.Commits, oracle)
	}
}
