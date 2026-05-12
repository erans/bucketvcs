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
	// BuildURL mints a URL via the gateway's URLBuilder. The closure
	// receives the manifest pack hash, storage key, and an expected
	// hash hint (currently empty). On error or empty URL the gate
	// treats the result as "skip advertisement" (soft failure) so the
	// client falls through to the inline packfile section.
	BuildURL func(ctx context.Context, hash, storageKey, expectedHash string) (string, error)
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
	if !in.ClientOptedIn || !in.FullPackRequested || in.PackChecksum == "" || in.PackKey == "" {
		return PackURIResult{}, nil
	}

	// Gate 2: strict 40-hex lowercase validation of the SHA-1 trailer.
	// A malformed checksum would produce an invalid `<sha1> <uri>\n`
	// line that some clients reject as a malformed advertisement.
	if !validPackSHA1(in.PackChecksum) {
		return PackURIResult{}, nil
	}

	if in.BuildURL == nil {
		return PackURIResult{}, nil
	}

	// Gate 3: mint the URL. Errors and empty URLs are both soft skips.
	url, err := in.BuildURL(ctx, in.PackChecksum, in.PackKey, "")
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

	return PackURIResult{Stanza: in.PackChecksum + " " + url + "\n"}, nil
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
