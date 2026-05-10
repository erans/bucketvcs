package maintenance

import "github.com/bucketvcs/bucketvcs/internal/repo/manifest"

// mergeInput is the per-run state that buildMergedBody needs.
type mergeInput struct {
	P0Keys         []string           // PackKey set we repacked at run start
	NewPack        manifest.PackEntry // the consolidated repack output
	NewObjectMap   manifest.IndexRef
	NewCommitGraph manifest.IndexRef
}

// buildMergedBody constructs the manifest body that maintenance wants
// to commit, given prev (the just-read manifest) and our run state.
//
//	Packs         = [NewPack] ++ (prev.Packs filtered by PackKey ∉ P0Keys)
//	Indexes       = { ObjectMap: NewObjectMap, CommitGraph: NewCommitGraph }
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
		ObjectMap:   &bvom,
		CommitGraph: &bvcg,
	}
	return out
}
