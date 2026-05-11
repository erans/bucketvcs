package uploadpack

import (
	"context"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
)

// ErrUnknownWant is returned when the client requests a commit not
// present in the reachability Set. Map to Git ERR pkt-line in the
// engine flow and abort the session.
var ErrUnknownWant = errors.New("uploadpack: unknown want")

// NegotiateInput is the parsed wants/haves/done state from the
// pkt-line layer.
type NegotiateInput struct {
	Wants []pack.OID
	Haves []pack.OID
	Done  bool
}

// Negotiate computes the ShippingPlan: commits reachable from Wants
// minus commits reachable from Haves. Pure-Go, reads only the Set.
func Negotiate(ctx context.Context, s *reachability.Set, in NegotiateInput) (ShippingPlan, error) {
	for _, w := range in.Wants {
		if !s.Has(w) {
			return ShippingPlan{}, fmt.Errorf("%w: %s", ErrUnknownWant, w)
		}
	}

	// Compute ancestors-of-haves.
	haveSet := make(map[pack.OID]bool, 64)
	knownHaves := make([]pack.OID, 0, len(in.Haves))
	for _, h := range in.Haves {
		if s.Has(h) {
			knownHaves = append(knownHaves, h)
		}
		// Unknown haves: silently ignore (Git protocol permits this).
	}
	if err := s.WalkAncestors(knownHaves, func(oid pack.OID, _ uint32) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		haveSet[oid] = true
		return nil
	}); err != nil {
		return ShippingPlan{}, err
	}

	// Walk wants, emit non-haveSet commits.
	shipping := make([]pack.OID, 0, 64)
	shippingSeen := make(map[pack.OID]bool, 64)
	if err := s.WalkAncestors(in.Wants, func(oid pack.OID, _ uint32) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if haveSet[oid] || shippingSeen[oid] {
			return nil
		}
		shippingSeen[oid] = true
		shipping = append(shipping, oid)
		return nil
	}); err != nil {
		return ShippingPlan{}, err
	}

	return ShippingPlan{Commits: shipping, Refs: s.RefTips()}, nil
}
