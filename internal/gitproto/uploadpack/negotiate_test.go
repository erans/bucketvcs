package uploadpack_test

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitproto/uploadpack"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/rtest"
)

func TestNegotiate_NoHaves_ShipsAllFromWant(t *testing.T) {
	ctx := context.Background()
	fx := rtest.NewBaseOnlyRepo(t) // A-B-C linear
	set, err := reachability.Load(ctx, fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := uploadpack.Negotiate(ctx, set, uploadpack.NegotiateInput{
		Wants: []pack.OID{fx.C},
		Done:  true,
	})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if !sameSet(plan.Commits, []pack.OID{fx.A, fx.B, fx.C}) {
		t.Fatalf("commits = %v, want {A,B,C}", plan.Commits)
	}
}

func TestNegotiate_HaveAncestor_ShipsOnlyDescendants(t *testing.T) {
	ctx := context.Background()
	fx := rtest.NewBaseOnlyRepo(t)
	set, _ := reachability.Load(ctx, fx.Store, fx.Keys, fx.Body)
	plan, err := uploadpack.Negotiate(ctx, set, uploadpack.NegotiateInput{
		Wants: []pack.OID{fx.C},
		Haves: []pack.OID{fx.B},
		Done:  true,
	})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if !sameSet(plan.Commits, []pack.OID{fx.C}) {
		t.Fatalf("commits = %v, want {C}", plan.Commits)
	}
}

func TestNegotiate_HaveIsTip_ShipsNothing(t *testing.T) {
	ctx := context.Background()
	fx := rtest.NewBaseOnlyRepo(t)
	set, _ := reachability.Load(ctx, fx.Store, fx.Keys, fx.Body)
	plan, err := uploadpack.Negotiate(ctx, set, uploadpack.NegotiateInput{
		Wants: []pack.OID{fx.C},
		Haves: []pack.OID{fx.C},
		Done:  true,
	})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if len(plan.Commits) != 0 {
		t.Fatalf("expected empty plan, got %v", plan.Commits)
	}
}

func TestNegotiate_UnknownWant_Error(t *testing.T) {
	ctx := context.Background()
	fx := rtest.NewBaseOnlyRepo(t)
	set, _ := reachability.Load(ctx, fx.Store, fx.Keys, fx.Body)
	var unknown pack.OID
	unknown[0] = 0xFF
	_, err := uploadpack.Negotiate(ctx, set, uploadpack.NegotiateInput{
		Wants: []pack.OID{unknown},
		Done:  true,
	})
	if !errors.Is(err, uploadpack.ErrUnknownWant) {
		t.Fatalf("err = %v, want ErrUnknownWant", err)
	}
}

func sameSet(a, b []pack.OID) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]pack.OID(nil), a...)
	bs := append([]pack.OID(nil), b...)
	sortOIDs(as)
	sortOIDs(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func sortOIDs(s []pack.OID) {
	sort.Slice(s, func(i, j int) bool {
		for k := 0; k < 20; k++ {
			if s[i][k] != s[j][k] {
				return s[i][k] < s[j][k]
			}
		}
		return false
	})
}
