package v2proto

import (
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

type FreshnessState int

const (
	FreshnessRetired FreshnessState = iota
	FreshnessStale
	FreshnessWarm
	FreshnessCurrent
)

func (f FreshnessState) String() string {
	switch f {
	case FreshnessRetired:
		return "retired"
	case FreshnessStale:
		return "stale"
	case FreshnessWarm:
		return "warm"
	case FreshnessCurrent:
		return "current"
	}
	return "unknown"
}

// FreshnessInputs decouples the state machine from the reachability
// package so the state machine itself is a pure function of values
// the caller supplies.
//
// WarmCommits must be positive: most reachability implementations treat
// max == 0 as "walk zero steps" and IsAncestor will return false, which
// would silently classify every non-current bundle as stale. Callers
// (gateway.NewServer, sshd.NewServer) default this to 5000 when
// BundleURIEnabled is true; a direct caller of EvaluateFreshness must
// supply a sane value too.
type FreshnessInputs struct {
	Bundle      *manifest.BundleEntry
	CurrentTip  string
	IsAncestor  func(ancestor, descendant string, max int) bool
	WalkBack    func(from, target string, max int) (int, error)
	WarmCommits int
	WarmAge     time.Duration
	Now         time.Time
}

type FreshnessResult struct {
	State         FreshnessState
	CommitsBehind int    // -1 when not computed (e.g. retired/stale-by-other-reason)
	Reason        string // "no_bundle", "warm_thresholds_misconfigured", "not_ancestor_within_window", "age_unparseable", "age_exceeded", "walkback_error", "walkback_not_reachable", "commits_exceeded", "current", "warm"
}

// EvaluateFreshness implements the §5.2 (M11 spec) state machine.
func EvaluateFreshness(in FreshnessInputs) FreshnessResult {
	if in.Bundle == nil {
		return FreshnessResult{State: FreshnessRetired, CommitsBehind: -1, Reason: "no_bundle"}
	}
	if in.Bundle.TipOID == in.CurrentTip {
		// "current" is reachable without consulting IsAncestor/WalkBack
		// or the WarmCommits/WarmAge thresholds, so we DON'T require them
		// to be configured for the hot path. Misconfiguration only
		// matters when we need to evaluate a non-current bundle.
		return FreshnessResult{State: FreshnessCurrent, CommitsBehind: 0, Reason: "current"}
	}
	if in.WarmCommits <= 0 || in.WarmAge <= 0 {
		// Loud failure for a caller who supplied a non-current bundle
		// with unset thresholds. The "stale" verdict matches what would
		// happen silently via IsAncestor(_, _, 0) returning false, but
		// the dedicated reason makes the misconfiguration visible to
		// operator log triage.
		return FreshnessResult{State: FreshnessStale, CommitsBehind: -1, Reason: "warm_thresholds_misconfigured"}
	}
	if !in.IsAncestor(in.Bundle.TipOID, in.CurrentTip, in.WarmCommits) {
		// IsAncestor returning false is ambiguous: a real force-push
		// (tip rewritten, no ancestor relationship) AND a legitimately
		// stale bundle that's more than WarmCommits behind both produce
		// this verdict. We don't differentiate here because the resulting
		// action is the same (omit advertisement) and the cheap
		// IsAncestor probe is bounded specifically to avoid the more
		// expensive WalkBack call when we'd reject either way.
		return FreshnessResult{State: FreshnessStale, CommitsBehind: -1, Reason: "not_ancestor_within_window"}
	}
	age, err := time.Parse(time.RFC3339, in.Bundle.GeneratedAt)
	if err != nil {
		// An unparseable GeneratedAt means we can't bound the bundle's age,
		// so we can't safely classify it as warm. Treat as stale and let
		// the client fall through to fetch — same outcome as age_exceeded
		// but with a distinct reason for operator diagnostics.
		return FreshnessResult{State: FreshnessStale, CommitsBehind: -1, Reason: "age_unparseable"}
	}
	if in.Now.Sub(age) > in.WarmAge {
		return FreshnessResult{State: FreshnessStale, CommitsBehind: -1, Reason: "age_exceeded"}
	}
	n, werr := in.WalkBack(in.CurrentTip, in.Bundle.TipOID, in.WarmCommits)
	if werr != nil {
		// WalkBack failed (index decode error, storage read error, etc.).
		// The returned n is undefined; report CommitsBehind: -1 and a
		// distinct reason so operator logs can tell a real index error
		// apart from a legitimate "too many commits" exceedance.
		return FreshnessResult{State: FreshnessStale, CommitsBehind: -1, Reason: "walkback_error"}
	}
	if n < 0 {
		// Some reachability implementations return (n=-1, nil) as a
		// sentinel for "target not reachable from `from`". Report it as
		// stale with a distinct reason from commits_exceeded so log
		// triage can tell unreachable-target apart from real exceedance.
		return FreshnessResult{State: FreshnessStale, CommitsBehind: -1, Reason: "walkback_not_reachable"}
	}
	if n > in.WarmCommits {
		// Defensive: IsAncestor(_, _, WarmCommits) already returned true
		// above, so a well-behaved reachability implementation should
		// produce n ≤ WarmCommits here. We keep this check because the
		// two helpers may have subtly different walk semantics (e.g.,
		// inclusive vs exclusive bound) in future implementations, and
		// emitting "commits_exceeded" is safer than silently warm-
		// classifying a bundle whose WalkBack disagrees with IsAncestor.
		return FreshnessResult{State: FreshnessStale, CommitsBehind: n, Reason: "commits_exceeded"}
	}
	return FreshnessResult{State: FreshnessWarm, CommitsBehind: n, Reason: "warm"}
}
