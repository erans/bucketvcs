package v2proto

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// BundleURIDeps wires HandleBundleURI to the gateway's reachability
// helpers, URL builder, and clock without taking a hard dependency on
// any of those packages from internal/v2proto.
//
// BuildURL returns only the URL string; callers that want to record
// transport selection (direct vs proxied) should do so inside the
// closure rather than punching the value back up through the v2
// dispatch.
type BundleURIDeps struct {
	Body        manifest.Body
	Now         time.Time
	WarmCommits int
	WarmAge     time.Duration
	IsAncestor  func(ancestor, descendant string, max int) bool
	WalkBack    func(from, target string, max int) (int, error)
	BuildURL    func(ctx context.Context, hash, storageKey, expectedHash string) (url string, err error)
}

// BundleURIOutcome reports what HandleBundleURI did, so callers can emit
// metrics and audit events without re-running freshness evaluation.
//
// State is always populated. URI is non-empty only when an advertisement
// was actually emitted (State is Current or Warm and BuildURL returned a
// non-empty URL). Callers classify proxied-vs-direct transport from URI.
//
// Special State values:
//   - FreshnessRetired: manifest had no full_default entry, the covered
//     ref was absent or empty (modeled as retired since the bundle
//     effectively cannot advertise), or freshness evaluation produced
//     Retired.
//   - FreshnessStale: EvaluateFreshness returned Stale.
//   - FreshnessCurrent / FreshnessWarm with URI == "": evaluated but
//     BuildURL failed or returned empty; not advertised.
//   - FreshnessCurrent / FreshnessWarm with URI != "": advertisement
//     successfully emitted.
type BundleURIOutcome struct {
	// State is the FreshnessState the request resolved to.
	State FreshnessState

	// URI is the advertised bundle URL when State is Current or Warm and
	// BuildURL returned a non-empty URL. Empty otherwise.
	URI string

	// Reason carries the operator-facing freshness label, used by callers
	// as the `freshness` metric label. Distinct from State.String() because
	// it disambiguates several distinct conditions that all resolve to
	// FreshnessRetired in State:
	//   - "no_bundle": manifest has no full_default entry.
	//   - "no_ref": entry exists but covered ref is missing or empty.
	//   - "retired", "stale", "warm", "current": pass-through from
	//     EvaluateFreshness's State.String() when the state machine ran.
	//   - "stale" sub-reasons (age_exceeded, walkback_error, etc.) are
	//     intentionally collapsed under "stale" to keep the metric
	//     cardinality bounded. M11 does NOT preserve the sub-reason
	//     anywhere reachable from the outcome — operators needing to
	//     differentiate stale-by-age from stale-by-walkback today must
	//     scrape the FreshnessResult logging directly. Successor
	//     milestones may extend this struct with a Detail field.
	//
	// The dispatch in serveBundleURI (which sees feature-off short-circuits
	// before HandleBundleURI runs) sets Reason="disabled" for those paths.
	Reason string

	// FirstTipOID is the TipOID of the BundleEntry that was advertised, or
	// empty when no advertisement was emitted. Surfaced so the gateway's
	// bundle.uri.advertised audit event uses the exact entry HandleBundleURI
	// selected (eliminating a re-scan + future-drift risk if the entry
	// selection rule changes).
	FirstTipOID string
}

// HandleBundleURI processes the v2 `command=bundle-uri` request and
// writes the response (the required bundle.version + bundle.mode
// header keys, a `bundle.<id>.<key>=<value>` block per advertised
// bundle, then a flush-pkt). Stale or retired bundles, missing refs,
// and URL-build failures all produce an empty response (just the
// flush-pkt) — clients then fall through to standard fetch, which is
// always correct.
//
// The returned BundleURIOutcome lets callers emit metrics and audit
// events without re-running the freshness state machine.
//
// Reason-vocabulary sync: the Reason strings set below (no_bundle, no_ref,
// and the State.String() pass-throughs) are duplicated by
// internal/gitproto/uploadpack/service.go::doServeBundleURI which short-
// circuits the same conditions as an optimization (avoiding the
// reachability.Load storage read for known-empty cases). If either set
// diverges, the gateway's freshness label will desync from a direct
// HandleBundleURI caller's. Keep them in sync.
func HandleBundleURI(ctx context.Context, w io.Writer, deps BundleURIDeps) (BundleURIOutcome, error) {
	var entry *manifest.BundleEntry
	for i := range deps.Body.Bundles {
		if deps.Body.Bundles[i].Kind == "full_default" {
			entry = &deps.Body.Bundles[i]
			break
		}
	}
	if entry == nil {
		return BundleURIOutcome{State: FreshnessRetired, Reason: "no_bundle"}, EncodeBundleURIResponse(w, nil)
	}
	currentTip, refPresent := deps.Body.Refs[entry.Ref]
	if !refPresent || currentTip == "" {
		// The bundle's covered ref has been deleted (or never existed).
		// EvaluateFreshness would still produce a stale verdict via the
		// not_ancestor_within_window branch, but routing it through here
		// makes the case explicit and avoids relying on accidental
		// IsAncestor semantics for an empty descendant OID.
		// Model as Retired since the bundle effectively cannot advertise.
		return BundleURIOutcome{State: FreshnessRetired, Reason: "no_ref"}, EncodeBundleURIResponse(w, nil)
	}
	res := EvaluateFreshness(FreshnessInputs{
		Bundle:      entry,
		CurrentTip:  currentTip,
		IsAncestor:  deps.IsAncestor,
		WalkBack:    deps.WalkBack,
		WarmCommits: deps.WarmCommits,
		WarmAge:     deps.WarmAge,
		Now:         deps.Now,
	})
	if res.State != FreshnessCurrent && res.State != FreshnessWarm {
		return BundleURIOutcome{State: res.State, Reason: res.State.String()}, EncodeBundleURIResponse(w, nil)
	}
	expectedHash := ""
	if hex := bundleHashHex(entry.BundleHash); hex != "" {
		expectedHash = "sha256:" + hex
	}
	url, err := deps.BuildURL(ctx, entry.BundleHash, entry.BundleKey, expectedHash)
	if err != nil || url == "" {
		// Non-fatal: omit the bundle, return empty response, client falls
		// through. An empty URL with nil error indicates a misconfigured
		// backend; treat it identically to an error to avoid emitting
		// `bundle.<id>.uri=` (an empty value would be rejected by Git as
		// a malformed advertisement, which is worse than no advertisement
		// at all).
		// Preserve the evaluated state even though we couldn't advertise.
		return BundleURIOutcome{State: res.State, Reason: res.State.String()}, EncodeBundleURIResponse(w, nil)
	}
	creationTok := ""
	if t, err := time.Parse(time.RFC3339, entry.GeneratedAt); err == nil {
		creationTok = strconv.FormatInt(t.Unix(), 10)
	}
	if err := EncodeBundleURIResponse(w, []BundleAdvertisement{{
		ID: entry.ID, URI: url, CreationTok: creationTok,
	}}); err != nil {
		// Validation rejected the advertisement (control chars in URI, or
		// a future ID convention that violates the [A-Za-z0-9_-] charset).
		// Don't tear down the response stream mid-flight — the upstream
		// HTTP handler has already committed success headers, and a
		// partial v2 frame would confuse the client more than the
		// advertised "empty response → client falls through to fetch"
		// contract. Emit the empty (flush-only) response instead.
		return BundleURIOutcome{State: res.State, Reason: res.State.String(), FirstTipOID: entry.TipOID}, EncodeBundleURIResponse(w, nil)
	}
	return BundleURIOutcome{State: res.State, Reason: res.State.String(), URI: url, FirstTipOID: entry.TipOID}, nil
}

// bundleHashHex extracts the hex body of a BundleHash of the form
// "sha256-<64-hex>" (the IndexRef.Hash convention used in the
// manifest). Returns "" for any value that doesn't carry an exact
// 64-char lowercase-hex body — the caller must then omit ExpectedHash
// from the URL request rather than produce a malformed value like
// "sha256:" or "sha256:not-hex".
func bundleHashHex(h string) string {
	const p = "sha256-"
	if !strings.HasPrefix(h, p) || len(h) != len(p)+64 {
		return ""
	}
	rest := h[len(p):]
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	return rest
}
