package reachability

import (
	"context"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/commitgraph"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// GenLookup is a read-only oid -> generation-number map covering all
// commits known to the manifest's base + delta chain. Used by
// receive-pack's gen-number computation during push.
type GenLookup struct {
	m map[pack.OID]uint32
}

// LoadGenLookup constructs a GenLookup from a manifest body. Returns
// ErrNoIndex for legacy manifests (no .bvcg); receive-pack callers handle
// that by computing gens without a base lookup (resulting commits start
// gen=1 transitively).
//
// LoadGenLookup uses a leaner path than Load: it opens only the .bvcg
// (commit-graph) and any delta Commits sections, skipping the .bvom
// (object-map) entirely. ObjectMap is not needed for generation-number
// lookups, so this avoids a round-trip to storage on every push.
func LoadGenLookup(ctx context.Context, store storage.ObjectStore, k *keys.Repo, body manifest.Body) (*GenLookup, error) {
	if body.Indexes.CommitGraph == nil {
		return nil, ErrNoIndex
	}

	cgBytes, err := readObject(ctx, store, body.Indexes.CommitGraph.Key)
	if err != nil {
		return nil, fmt.Errorf("reachability: read .bvcg: %w", err)
	}
	cg, err := commitgraph.Open(cgBytes)
	if err != nil {
		return nil, fmt.Errorf("reachability: open .bvcg: %w", err)
	}

	m := make(map[pack.OID]uint32, 256)
	cg.IterRecords(func(oid pack.OID, gen uint32) {
		m[oid] = gen
	})

	// Overlay delta commits — later deltas overwrite earlier ones.
	if body.Indexes.Reachability != nil {
		for _, ref := range body.Indexes.Reachability.Deltas {
			bts, err := readObject(ctx, store, ref.Key)
			if err != nil {
				return nil, fmt.Errorf("reachability: read delta %s: %w", short(ref.Hash), err)
			}
			d, err := deltaindex.Decode(bts)
			if err != nil {
				return nil, fmt.Errorf("reachability: decode delta %s: %w", short(ref.Hash), err)
			}
			for _, c := range d.Commits {
				m[c.OID] = c.Generation
			}
		}
	}

	return &GenLookup{m: m}, nil
}

// NewGenLookup creates a GenLookup from a pre-built oid->gen map.
// Intended for tests and internal use where a Set is not desirable.
func NewGenLookup(m map[pack.OID]uint32) *GenLookup {
	cp := make(map[pack.OID]uint32, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return &GenLookup{m: cp}
}

// Lookup returns the generation number for oid.
func (g *GenLookup) Lookup(oid pack.OID) (uint32, bool) {
	v, ok := g.m[oid]
	return v, ok
}
