package maintenance

import (
	"context"
	"fmt"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// BundleTriggerResult is the outcome of EvaluateBundleTriggers.
type BundleTriggerResult struct {
	// Triggered is true when bundle-refresh should run.
	Triggered bool
	// Reason is "missing", "age", "commits", or "no_trigger".
	Reason string
	// CommitsBehind is the depth of the bounded walk from current tip
	// back to the existing bundle's tip:
	//   - 0 when the tips are equal (no_trigger short-circuit)
	//   - >=1 when the walk located the existing tip within the bound
	//   - -1 when the walk bound was exhausted (commits trigger fires
	//     with the true distance unknown) OR when the commit-distance
	//     check did not run (no rset, BundleCommits=0, or an earlier
	//     trigger short-circuited).
	CommitsBehind int
}

// EvaluateBundleTriggers decides whether bundle-refresh should run.
// Cheap-first ordering: missing -> age -> tip-equality -> commits.
//
// rset is an already-loaded reachability set (used for the bounded walk
// from current tip back to bundle.TipOID). Pass nil to skip the
// commit-distance check (force / dry-run paths).
func EvaluateBundleTriggers(
	ctx context.Context,
	m manifest.Body,
	th Thresholds,
	ref string,
	now time.Time,
	rset *reachability.Set,
) (BundleTriggerResult, error) {
	var existing *manifest.BundleEntry
	for i := range m.Bundles {
		if m.Bundles[i].Kind == "full_default" && m.Bundles[i].Ref == ref {
			existing = &m.Bundles[i]
			break
		}
	}
	if existing == nil {
		return BundleTriggerResult{Triggered: true, Reason: "missing", CommitsBehind: -1}, nil
	}

	// Age check. An unparseable timestamp forces a refresh under the
	// "age" reason — it's the safe default (a corrupted or future-format
	// GeneratedAt should not silently disable the age policy).
	if th.BundleAge > 0 {
		gen, err := time.Parse(time.RFC3339, existing.GeneratedAt)
		if err != nil || now.Sub(gen) >= th.BundleAge {
			return BundleTriggerResult{Triggered: true, Reason: "age", CommitsBehind: -1}, nil
		}
	}

	// Tip equality short-circuit before any walk. Empty currentTip
	// means the caller passed a ref that's not in m.Refs (a precondition
	// violation — ResolveDefaultBranch upstream guarantees the ref
	// exists). Surface this rather than silently falling through.
	currentTip := m.Refs[ref]
	if currentTip == "" {
		return BundleTriggerResult{}, fmt.Errorf("bundle triggers: ref %q not in manifest refs", ref)
	}
	if currentTip == existing.TipOID {
		return BundleTriggerResult{Triggered: false, Reason: "no_trigger", CommitsBehind: 0}, nil
	}

	// Commit-distance check via bounded walk. WalkBackOID's loop guard
	// is `depth < max` evaluated at the top of each iteration, so the
	// walk can legitimately return n == max (target found at the exact
	// boundary). Both n == max and n == -1 indicate "distance >= max",
	// which the user has declared as the trigger threshold.
	if th.BundleCommits > 0 && rset != nil {
		n, err := rset.WalkBackOID(currentTip, existing.TipOID, th.BundleCommits)
		if err != nil {
			return BundleTriggerResult{}, fmt.Errorf("bundle triggers: walk: %w", err)
		}
		if n < 0 {
			// Walk exhausted its bound; true distance unknown.
			return BundleTriggerResult{Triggered: true, Reason: "commits", CommitsBehind: -1}, nil
		}
		if n >= th.BundleCommits {
			// Exact-boundary case: target found at depth == max.
			return BundleTriggerResult{Triggered: true, Reason: "commits", CommitsBehind: n}, nil
		}
		return BundleTriggerResult{Triggered: false, Reason: "no_trigger", CommitsBehind: n}, nil
	}

	return BundleTriggerResult{Triggered: false, Reason: "no_trigger", CommitsBehind: -1}, nil
}
