package refstore

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/oidconst"
)

// InlineRefStore wraps Body.Refs directly. All operations are
// in-memory; the ObjectStore is never consulted.
type InlineRefStore struct {
	refs map[string]string
}

// newInlineRefStore takes a body snapshot. The defensive shallow copy
// of body.Refs is load-bearing: callers may mutate the input body
// after constructing the store (e.g., during a Commit retry that
// recomputes Stage), and we don't want those mutations to bleed
// through. This copy works regardless of whether body.Refs was
// non-nil — manifest.UnmarshalBody guarantees that for v1 bodies,
// but newInlineRefStore handles a nil map gracefully via the
// len-on-nil convention.
func newInlineRefStore(body *manifest.Body) *InlineRefStore {
	// Shallow-copy the map so callers can mutate the input body after
	// constructing the store without corrupting our snapshot.
	refs := make(map[string]string, len(body.Refs))
	for k, v := range body.Refs {
		refs[k] = v
	}
	return &InlineRefStore{refs: refs}
}

// Mode returns ModeInline.
func (s *InlineRefStore) Mode() Mode { return ModeInline }

// Lookup returns the OID and existence for refname.
func (s *InlineRefStore) Lookup(_ context.Context, refname string) (string, bool, error) {
	oid, ok := s.refs[refname]
	return oid, ok, nil
}

// List returns a fresh copy of the ref map. Callers may mutate the
// returned map without affecting the store.
func (s *InlineRefStore) List(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(s.refs))
	for k, v := range s.refs {
		out[k] = v
	}
	return out, nil
}

// Stage merges updates into the snapshot and returns a Stage with
// Mode=ModeInline. Delete convention: empty OID or 40-zero oidconst.NullOIDHex.
// The returned NewInlineRefs is a freshly allocated map; mutating it
// does not affect the store.
func (s *InlineRefStore) Stage(_ context.Context, updates map[string]string) (Stage, error) {
	out := make(map[string]string, len(s.refs)+len(updates))
	for k, v := range s.refs {
		out[k] = v
	}
	for ref, oid := range updates {
		if oid == "" || oid == oidconst.NullOIDHex {
			delete(out, ref)
			continue
		}
		out[ref] = oid
	}
	return Stage{
		Mode:          ModeInline,
		NewInlineRefs: out,
	}, nil
}
