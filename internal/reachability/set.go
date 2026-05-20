package reachability

import (
	"context"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/commitgraph"
	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/oidconst"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Set is the unified view (base + delta chain) consumed by negotiation.
type Set struct {
	cg         *commitgraph.Reader
	omap       *objindex.Map
	deltas     []*deltaindex.Delta                   // base-first ordering
	refs       map[string]pack.OID                   // effective ref tips after deltas applied
	deltaIndex map[pack.OID]*deltaindex.CommitRecord // O(1) lookups; latest delta wins
}

// Load constructs a Set from the manifest body. ErrNoIndex if the
// base index pair is missing.
func Load(ctx context.Context, store storage.ObjectStore, k *keys.Repo, body manifest.Body) (*Set, error) {
	if body.Indexes.CommitGraph == nil || body.Indexes.ObjectMap == nil {
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
	omap, err := objindex.OpenWithExpectedHash(ctx, store, body.Indexes.ObjectMap.Key, body.Indexes.ObjectMap.Hash)
	if err != nil {
		return nil, fmt.Errorf("reachability: open .bvom: %w", err)
	}

	var deltas []*deltaindex.Delta
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
			deltas = append(deltas, d)
		}
	}

	rs, err := refstore.New(ctx, store, k, &body)
	if err != nil {
		return nil, fmt.Errorf("reachability: open refstore: %w", err)
	}
	rawRefs, err := rs.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("reachability: list refs: %w", err)
	}
	refs := make(map[string]pack.OID, len(rawRefs))
	for name, hex := range rawRefs {
		if hex == "" || hex == oidconst.NullOIDHex {
			continue
		}
		o, err := pack.ParseOID(hex)
		if err != nil {
			return nil, fmt.Errorf("reachability: parse ref %q: %w", name, err)
		}
		refs[name] = o
	}
	for _, d := range deltas {
		for _, tip := range d.RefTips {
			if tip.NewOID == (pack.OID{}) {
				// Zero OID = deletion: remove the ref from the effective map.
				delete(refs, tip.RefName)
			} else {
				refs[tip.RefName] = tip.NewOID
			}
		}
	}

	// Build O(1) lookup index: iterate deltas in order so that the latest
	// delta's record for a given OID wins (overwrites earlier entries).
	deltaIndex := make(map[pack.OID]*deltaindex.CommitRecord, 64)
	for _, d := range deltas {
		for i := range d.Commits {
			deltaIndex[d.Commits[i].OID] = &d.Commits[i]
		}
	}

	return &Set{cg: cg, omap: omap, deltas: deltas, refs: refs, deltaIndex: deltaIndex}, nil
}

func readObject(ctx context.Context, store storage.ObjectStore, key string) ([]byte, error) {
	obj, err := store.Get(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	defer obj.Body.Close()
	return io.ReadAll(obj.Body)
}

func short(hash string) string {
	if len(hash) < 8 {
		return hash
	}
	return hash[:8]
}

// Has reports whether oid is a commit known to the base or any delta.
func (s *Set) Has(oid pack.OID) bool {
	if _, ok := s.deltaIndex[oid]; ok {
		return true
	}
	_, ok := s.cg.GenerationOf(oid)
	return ok
}

// Parents returns oid's parents, looking deltas-first then base.
func (s *Set) Parents(oid pack.OID) []pack.OID {
	if rec, ok := s.deltaIndex[oid]; ok {
		return rec.Parents
	}
	if rec, ok := s.cg.RecordOf(oid); ok {
		return rec.Parents
	}
	return nil
}

// Generation returns the commit-graph generation number for oid, or (0, false).
func (s *Set) Generation(oid pack.OID) (uint32, bool) {
	if rec, ok := s.deltaIndex[oid]; ok {
		return rec.Generation, true
	}
	return s.cg.GenerationOf(oid)
}

// WalkAncestors visits roots and their ancestors transitively in
// generation-descending order. visit returns error to stop early.
func (s *Set) WalkAncestors(roots []pack.OID, visit func(oid pack.OID, gen uint32) error) error {
	seen := make(map[pack.OID]bool, 64)
	h := newGenHeap()
	for _, r := range roots {
		if !s.Has(r) {
			// Skip roots not in the Set — seeding an unknown OID would call
			// visit with gen=0 and no parents, which is misleading and wastes
			// a heap slot.
			continue
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		gen, _ := s.Generation(r)
		h.push(genItem{oid: r, gen: gen})
	}
	for h.len() > 0 {
		it := h.pop()
		if err := visit(it.oid, it.gen); err != nil {
			return err
		}
		for _, p := range s.Parents(it.oid) {
			if seen[p] {
				continue
			}
			if !s.Has(p) {
				continue // skip unknown parents (corrupt chain protection)
			}
			seen[p] = true
			pgen, _ := s.Generation(p)
			h.push(genItem{oid: p, gen: pgen})
		}
	}
	return nil
}

type genItem struct {
	oid pack.OID
	gen uint32
}

type genHeap struct{ items []genItem }

func newGenHeap() *genHeap  { return &genHeap{} }
func (h *genHeap) len() int { return len(h.items) }
func (h *genHeap) push(it genItem) {
	h.items = append(h.items, it)
	for i := len(h.items) - 1; i > 0; {
		parent := (i - 1) / 2
		if h.items[parent].gen >= h.items[i].gen {
			break
		}
		h.items[parent], h.items[i] = h.items[i], h.items[parent]
		i = parent
	}
}
func (h *genHeap) pop() genItem {
	top := h.items[0]
	n := len(h.items) - 1
	h.items[0] = h.items[n]
	h.items = h.items[:n]
	for i := 0; ; {
		l, r := 2*i+1, 2*i+2
		best := i
		if l < n && h.items[l].gen > h.items[best].gen {
			best = l
		}
		if r < n && h.items[r].gen > h.items[best].gen {
			best = r
		}
		if best == i {
			break
		}
		h.items[i], h.items[best] = h.items[best], h.items[i]
		i = best
	}
	return top
}

// RefTips returns the effective ref tip map (base + deltas applied in
// order). The returned map is a copy.
func (s *Set) RefTips() map[string]pack.OID {
	out := make(map[string]pack.OID, len(s.refs))
	for k, v := range s.refs {
		out[k] = v
	}
	return out
}

// ObjectPack delegates to the underlying .bvom view. Returns (packID, ok).
// packID is the pack hash hex string as stored in the .bvom pack table.
func (s *Set) ObjectPack(oid pack.OID) (string, bool) {
	packID, _, ok := s.omap.Lookup(oid)
	return packID, ok
}

// WalkBackOID walks backward through commits from `from`, looking for
// `target`. Returns the count of commits walked (0 if from == target,
// 1 if target is from's parent, etc.) bounded by max. Returns -1 if
// target is not reached within max steps. Returns an error if either
// OID fails to parse or `from` is not present in the set.
func (s *Set) WalkBackOID(from, target string, max int) (int, error) {
	fromOID, err := pack.ParseOID(from)
	if err != nil {
		return -1, fmt.Errorf("reachability: WalkBackOID: parse from: %w", err)
	}
	targetOID, err := pack.ParseOID(target)
	if err != nil {
		return -1, fmt.Errorf("reachability: WalkBackOID: parse target: %w", err)
	}
	if !s.Has(fromOID) {
		return -1, fmt.Errorf("reachability: WalkBackOID: %s not in set", from)
	}
	if fromOID == targetOID {
		return 0, nil
	}
	visited := map[pack.OID]bool{fromOID: true}
	frontier := []pack.OID{fromOID}
	depth := 0
	for len(frontier) > 0 && depth < max {
		depth++
		var next []pack.OID
		for _, oid := range frontier {
			parents := s.Parents(oid)
			for _, p := range parents {
				if p == targetOID {
					return depth, nil
				}
				if !visited[p] {
					visited[p] = true
					next = append(next, p)
				}
			}
		}
		frontier = next
	}
	return -1, nil
}

// IsAncestor reports whether ancestor is reachable from descendant
// within max parent hops. Returns false (no error) when not found
// within bound or when descendant is not present in the set.
func (s *Set) IsAncestor(ancestor, descendant string, max int) bool {
	n, err := s.WalkBackOID(descendant, ancestor, max)
	return err == nil && n >= 0
}
