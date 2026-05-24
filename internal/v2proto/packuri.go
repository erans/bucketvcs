package v2proto

import (
	"context"
	"strings"
)

// PackURIInputs feeds the §16.4 (M11 spec) advertise gate that decides
// whether to advertise a packfile-uris response on an in-flight fetch.
//
// The gate is intentionally a pure function of inputs — gateway-side
// observability (URL minting outcomes, CDN selection, etc.) lives in
// the BuildURL closure rather than being threaded back through this
// result.
type PackURIInputs struct {
	// Tenant and Repo are the request-scope identifiers threaded through
	// BuildURL (M19). The closure embeds them into the URL path + signature
	// so the same gateway can serve many (tenant, repo) repos from one mount.
	Tenant string
	Repo   string
	// ClientOptedIn is true when fetchReq.PackfileURIs is non-empty
	// (the client advertised at least one accepted protocol scheme).
	ClientOptedIn bool
	// FullPackRequested is the predicate from EvaluateFullPackRequested:
	// the request can be served by a single canonical pack.
	FullPackRequested bool
	// PackChecksum is the 40-hex SHA-1 trailer of the canonical pack
	// the server intends to advertise. Required (gate skips on empty).
	PackChecksum string
	// PackKey is the storage key of the canonical pack. Required
	// (gate skips on empty); passed to BuildURL as the storageKey arg.
	PackKey string
	// PackID is the canonical pack's content-addressed identifier. The
	// bare-mirror filename is `pack-<PackID>.pack`, which the upload-pack
	// service forwards to `git pack-objects --keep-pack=` to elide
	// URI-covered objects from the inline pack. Required (gate skips on
	// empty) and must be 40 lowercase hex — same shape as PackChecksum,
	// enforced here so the advertise decision and the downstream
	// keep-pack basename validation (gitcli.validPackBasename) cannot
	// disagree mid-response.
	PackID string
	// BuildURL mints a URL via the gateway's URLBuilder. The closure
	// receives the manifest pack hash, storage key, and an expected
	// hash hint (currently empty). On error or empty URL the gate
	// treats the result as "skip advertisement" (soft failure) so the
	// client falls through to the inline packfile section.
	BuildURL func(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string) (string, error)
}

// PackURIResult is the gate's verdict.
type PackURIResult struct {
	// Stanza is the wire bytes to emit inside a "packfile-uris\n"
	// section. Format per Git protocol-v2 packfile-uris:
	//
	//	"<40-hex-sha1> <uri>\n"
	//
	// (a bare line, NO "packfile-uri=" prefix). Empty when no URI
	// should be advertised — the caller must then skip emitting the
	// section entirely.
	Stanza string

	// URL is the resolved bundle URL the Stanza encodes, exposed
	// separately so callers that need to classify proxied-vs-direct
	// transport (for observability) can avoid re-parsing the Stanza.
	// Mirrors the Task 2 BundleURIOutcome shape where observability
	// needs are surfaced through structured return types rather than
	// scraped from wire bytes. Empty when Stanza is empty.
	URL string
}

// EvaluatePackURIAdvertise returns the wire stanza to emit inside a
// packfile-uris response section, or empty when no URI should be
// advertised. The function never returns a non-nil error from any
// path documented as "skip" — soft failures (BuildURL error, empty
// URL, control chars in URI, malformed checksum) all produce an
// empty stanza with nil error so the caller falls through to the
// inline packfile section without disturbing the response stream.
func EvaluatePackURIAdvertise(ctx context.Context, in PackURIInputs) (PackURIResult, error) {
	// Gate 1: client must have opted in AND the request must be servable
	// by a single canonical pack AND we must know which pack to advertise.
	if !in.ClientOptedIn || !in.FullPackRequested || in.PackChecksum == "" || in.PackKey == "" || in.PackID == "" {
		return PackURIResult{}, nil
	}

	// Gate 2: strict 40-hex lowercase validation of the SHA-1 trailer
	// AND of the PackID. Validating PackID here (not at the keep-pack
	// call site downstream) ensures advertise emission and inline-pack
	// elision stay in lockstep: if either value is malformed we skip
	// the whole URI advertise rather than emit a stanza whose paired
	// `--keep-pack=pack-<PackID>.pack` would be rejected mid-response.
	if !validPackSHA1(in.PackChecksum) || !validPackSHA1(in.PackID) {
		return PackURIResult{}, nil
	}

	if in.BuildURL == nil {
		return PackURIResult{}, nil
	}

	// Gate 3: mint the URL. Errors and empty URLs are both soft skips.
	url, err := in.BuildURL(ctx, in.Tenant, in.Repo, in.PackChecksum, in.PackKey, "")
	if err != nil || url == "" {
		return PackURIResult{}, nil
	}

	// Gate 4: reject control characters in the URI to prevent pkt-line
	// frame injection. CR/LF would terminate the line early; NUL is
	// disallowed in pkt-line payloads. Tab is permitted (URI grammar
	// allows it, and HTTPS URIs don't contain tabs in practice).
	if strings.ContainsAny(url, "\r\n\x00") {
		return PackURIResult{}, nil
	}

	return PackURIResult{Stanza: in.PackChecksum + " " + url + "\n", URL: url}, nil
}

// validPackSHA1 returns true iff s is exactly 40 lowercase hex chars.
// We require lowercase to match Git's canonical representation; an
// uppercase or mixed-case checksum is treated as malformed input from
// an upstream stage that produced a non-canonical value.
func validPackSHA1(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
