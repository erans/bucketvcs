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

// HandleBundleURI processes the v2 `command=bundle-uri` request and
// writes the response (the required bundle.version + bundle.mode
// header keys, a `bundle.<id>.<key>=<value>` block per advertised
// bundle, then a flush-pkt). Stale or retired bundles, missing refs,
// and URL-build failures all produce an empty response (just the
// flush-pkt) — clients then fall through to standard fetch, which is
// always correct.
func HandleBundleURI(ctx context.Context, w io.Writer, deps BundleURIDeps) error {
	var entry *manifest.BundleEntry
	for i := range deps.Body.Bundles {
		if deps.Body.Bundles[i].Kind == "full_default" {
			entry = &deps.Body.Bundles[i]
			break
		}
	}
	if entry == nil {
		return EncodeBundleURIResponse(w, nil)
	}
	currentTip, refPresent := deps.Body.Refs[entry.Ref]
	if !refPresent || currentTip == "" {
		// The bundle's covered ref has been deleted (or never existed).
		// EvaluateFreshness would still produce a stale verdict via the
		// not_ancestor_within_window branch, but routing it through here
		// makes the case explicit and avoids relying on accidental
		// IsAncestor semantics for an empty descendant OID.
		return EncodeBundleURIResponse(w, nil)
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
		return EncodeBundleURIResponse(w, nil)
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
		return EncodeBundleURIResponse(w, nil)
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
		return EncodeBundleURIResponse(w, nil)
	}
	return nil
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
