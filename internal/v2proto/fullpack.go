package v2proto

import (
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// FullPackRequestedInputs is the data needed to decide whether a fetch
// request can be served by exactly one canonical pack.
type FullPackRequestedInputs struct {
	// Wants from the parsed fetch request (40-hex strings, lowercase, deduped).
	Wants []string
	// Haves from the parsed fetch request (same format).
	Haves []string
	// Packs from the manifest body (PackEntry slice).
	Packs []manifest.PackEntry
	// RefTips: the tip OIDs of all advertised refs (40-hex strings, lowercase).
	// Caller is responsible for excluding hidden refs.
	RefTips []string
}

// EvaluateFullPackRequested returns true iff:
//   - Haves is empty (full clone), AND
//   - len(Packs) == 1 (single canonical pack), AND
//   - Packs[0].PackChecksum != "" (M11 backfilled), AND
//   - The Wants set equals the RefTips set (full clone of all refs).
//
// When all four hold, the .bvom invariant guarantees the single pack
// contains every object reachable from the wants — therefore the pack
// alone satisfies the fetch.
func EvaluateFullPackRequested(in FullPackRequestedInputs) bool {
	// Condition 1: Haves must be empty (full clone, not incremental fetch).
	if len(in.Haves) > 0 {
		return false
	}

	// Condition 2: Exactly one canonical pack.
	if len(in.Packs) != 1 {
		return false
	}

	// Condition 3: Pack must have a checksum (M11 backfilled; legacy packs don't).
	if in.Packs[0].PackChecksum == "" {
		return false
	}

	// Condition 4: Wants set must be non-empty and equal to the RefTips set.
	if len(in.Wants) == 0 {
		return false
	}

	// Build normalized (lowercase) sets and compare.
	wantSet := make(map[string]struct{}, len(in.Wants))
	for _, w := range in.Wants {
		wantSet[strings.ToLower(w)] = struct{}{}
	}

	tipSet := make(map[string]struct{}, len(in.RefTips))
	for _, t := range in.RefTips {
		tipSet[strings.ToLower(t)] = struct{}{}
	}

	if len(wantSet) != len(tipSet) {
		return false
	}
	for tip := range tipSet {
		if _, ok := wantSet[tip]; !ok {
			return false
		}
	}

	return true
}
