package maintenance

import "github.com/bucketvcs/bucketvcs/internal/repo/manifest"

// mergeInput is the per-run state that buildMergedBody needs.
type mergeInput struct {
	P0Keys         []string           // PackKey set we repacked at run start
	NewPack        manifest.PackEntry // the consolidated repack output
	NewObjectMap   manifest.IndexRef
	NewCommitGraph manifest.IndexRef

	// ConsumedHashes is the set of delta hash strings that this run consumed
	// (captured from the run-start snapshot). buildMergedBody trims exactly
	// these entries from prev.Indexes.Reachability.Deltas, preserving any
	// entry whose hash is NOT in the set (concurrent pushes appended after
	// the snapshot). ConsumedDeltaCount is retained for the report field.
	// BaseManifest is set inside the CAS callback from view.Header.ManifestVersion.
	ConsumedHashes     map[string]struct{}
	ConsumedDeltaCount int // kept for report; trim is driven by ConsumedHashes
	BaseManifest       string
}

// buildMergedBody constructs the manifest body that maintenance wants
// to commit, given prev (the just-read manifest) and our run state.
//
//	Packs         = [NewPack] ++ (prev.Packs filtered by PackKey ∉ P0Keys)
//	Indexes       = { ObjectMap: NewObjectMap, CommitGraph: NewCommitGraph,
//	                  Reachability: trimmed(prev.Reachability, ConsumedHashes) }
//	Refs          = prev.Refs        (preserved verbatim)
//	DefaultBranch = prev.DefaultBranch
//	Bundles       = prev.Bundles
//
// This is a pure function over its inputs — fully testable without an
// ObjectStore. The retry loop in repo.Repo.Commit re-runs this on each
// CAS attempt with a fresh prev.
//
// Aliasing note: out.Refs and out.Bundles share map/slice headers with
// prev.Refs / prev.Bundles. The current caller (repo.Repo.Commit's
// retry loop) re-unmarshals prev from JSON on every attempt, so prev
// is always a fresh value and the aliasing is harmless. Future callers
// that want to mutate the returned body should deep-copy these fields
// first, or the function should be hardened to copy them itself.
func buildMergedBody(prev manifest.Body, in mergeInput) manifest.Body {
	p0 := make(map[string]struct{}, len(in.P0Keys))
	for _, k := range in.P0Keys {
		p0[k] = struct{}{}
	}
	out := manifest.Body{
		DefaultBranch: prev.DefaultBranch,
		Refs:          prev.Refs,
		Bundles:       prev.Bundles,
	}
	out.Packs = append(out.Packs, in.NewPack)
	for _, p := range prev.Packs {
		if _, repacked := p0[p.PackKey]; repacked {
			continue
		}
		out.Packs = append(out.Packs, p)
	}
	bvom := in.NewObjectMap
	bvcg := in.NewCommitGraph
	out.Indexes = manifest.Indexes{
		ObjectMap:    &bvom,
		CommitGraph:  &bvcg,
		Reachability: trimConsumedByHash(prev.Indexes.Reachability, in.ConsumedHashes, in.BaseManifest, true),
	}
	return out
}

// compactOnlyInput is the per-run state for buildCompactOnlyBody.
type compactOnlyInput struct {
	// NewCommitGraph is the freshly-built commit-graph index.
	// NewObjectMap is intentionally absent: compact-only does not produce a
	// new pack, so the .bvom pack-id table would reference a locally-built
	// pack that is never uploaded. ObjectMap is preserved from prev verbatim.
	NewCommitGraph manifest.IndexRef
	// ConsumedHashes is the set of delta hash strings consumed by this run.
	// These are dropped from prev.Indexes.Reachability.Deltas; any delta
	// whose hash is NOT in the set (appended by concurrent pushes after the
	// snapshot) is preserved.
	ConsumedHashes map[string]struct{}
	// ConsumedDeltaCount is kept for the compaction report only; the trim
	// is driven entirely by ConsumedHashes.
	ConsumedDeltaCount int
	// BaseManifest is the manifest version string to record in the
	// ReachabilityRef after compaction (e.g. "v00000042").
	BaseManifest string
}

// buildCompactOnlyBody constructs the manifest body for a compact-only
// run (no new pack produced). Packs are preserved from prev; .bvom is
// preserved from prev (compact-only does not change the pack set, so the
// existing pack-id table remains valid); .bvcg is swapped to the
// freshly-built value; consumed deltas are trimmed by hash set so
// concurrent push deltas are never silently dropped.
//
// Invariant maintained: body.Indexes.ObjectMap pack-id table ⊆ manifest
// pack IDs. Compact-only must not swap in a .bvom built from a locally
// repacked pack that is never uploaded, because that would produce a
// dangling pack-id reference until the next full repack repairs it.
func buildCompactOnlyBody(prev manifest.Body, in compactOnlyInput) manifest.Body {
	bvcg := in.NewCommitGraph
	out := manifest.Body{
		DefaultBranch: prev.DefaultBranch,
		Refs:          prev.Refs,
		Packs:         prev.Packs,
		Bundles:       prev.Bundles,
		Indexes: manifest.Indexes{
			ObjectMap:   prev.Indexes.ObjectMap, // preserved — compact-only doesn't touch .bvom
			CommitGraph: &bvcg,
			Reachability: trimConsumedByHash(
				prev.Indexes.Reachability,
				in.ConsumedHashes,
				in.BaseManifest,
				false,
			),
		},
	}
	return out
}

// trimConsumedByHash removes entries from the delta chain whose hash is
// in consumedHashes, preserving any entry whose hash is NOT in the set
// (concurrent pushes that appended deltas after our snapshot). It is a
// pure function — safe to call from within a CAS retry loop.
//
// advanceBaseManifest distinguishes two call sites:
//   - true  (repack path): the base index was rebuilt this run, so
//     BaseManifest must be updated even when consumedHashes is empty
//     (e.g. m0 had no Reachability, but a concurrent push added one).
//   - false (compact-only path): no new base index was produced; if
//     nothing was consumed, preserve prev verbatim (round-11 invariant).
//
// This is the race-safe replacement for the old index-based trim: if two
// compactions race, the second one's prev.Deltas may no longer match the
// original snapshot order, but hash-set membership is order-independent.
func trimConsumedByHash(prev *manifest.ReachabilityRef, consumedHashes map[string]struct{}, baseVersion string, advanceBaseManifest bool) *manifest.ReachabilityRef {
	if prev == nil && len(consumedHashes) == 0 {
		// Nothing to record: prev is absent (legacy repo shape) and there are no
		// deltas to drop. Return nil so we don't inject an empty ReachabilityRef
		// into manifests that have never had one — keeps legacy-shaped manifests
		// legacy-shaped until the first real compaction establishes the base.
		return nil
	}
	if prev != nil && len(consumedHashes) == 0 {
		if !advanceBaseManifest {
			// Compact-only with nothing consumed: preserve prev verbatim.
			// Advancing BaseManifest when no actual compaction occurred would
			// record a "compaction happened" state that isn't true.
			cp := *prev
			cp.Deltas = append([]manifest.IndexRef(nil), prev.Deltas...)
			return &cp
		}
		// Repack path with nothing consumed (m0 had no Reachability, but a
		// concurrent push added one during the run): keep prev's Deltas but
		// advance BaseManifest since the base index was rebuilt this run.
		cp := *prev
		cp.BaseManifest = baseVersion
		cp.Deltas = append([]manifest.IndexRef(nil), prev.Deltas...)
		return &cp
	}
	out := &manifest.ReachabilityRef{BaseManifest: baseVersion}
	if prev == nil {
		return out
	}
	kept := make([]manifest.IndexRef, 0, len(prev.Deltas))
	for _, ref := range prev.Deltas {
		if _, consumed := consumedHashes[ref.Hash]; consumed {
			continue
		}
		kept = append(kept, ref)
	}
	out.Deltas = kept
	return out
}
