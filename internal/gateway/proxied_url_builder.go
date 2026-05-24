package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// URLBuilder mints bundle/pack URLs for v2 advertise responses.
type URLBuilder struct {
	Store          storage.ObjectStore
	ProxiedKey     []byte
	ProxiedBaseURL string // e.g. "https://gw.example.com" (no trailing slash)
	BundleTTL      time.Duration
	PackTTL        time.Duration
	Mode           URIMode
	Now            func() time.Time // optional; defaults to time.Now
}

func (b *URLBuilder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// BuildBundleURL returns (url, via, error). url is the URL git will fetch;
// via is "direct" (signed object-store URL) or "proxied" (URL through this
// gateway's /_bundle/<t>/<r>/<h> handler). M19: tenant and repo are
// embedded in the URL path and bound into the proxied token's hash field
// so any path-segment tamper fails HMAC verify.
func (b *URLBuilder) BuildBundleURL(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string) (string, string, error) {
	return b.buildURL(ctx, "bundle", tenant, repo, hash, storageKey, expectedHash, b.BundleTTL)
}

// BuildPackURL is the pack-uri analogue of BuildBundleURL.
func (b *URLBuilder) BuildPackURL(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string) (string, string, error) {
	return b.buildURL(ctx, "pack", tenant, repo, hash, storageKey, expectedHash, b.PackTTL)
}

func (b *URLBuilder) buildURL(ctx context.Context, kind, tenant, repo, hash, storageKey, expectedHash string, ttl time.Duration) (string, string, error) {
	if b.Mode == URIModeOff {
		return "", "", fmt.Errorf("gateway: URI mode is off")
	}
	if b.Mode == URIModeDirect || b.Mode == URIModeAuto {
		signedURL, hdr, err := b.Store.SignedGetURL(ctx, storageKey, storage.SignedURLOptions{
			Expires: ttl, Method: "GET", ExpectedHash: expectedHash,
		})
		// Bundle/pack URLs are advertised to git via the v2 bundle-uri and
		// packfile-uri caps. Git does not let the server pin extra request
		// headers on those fetches, so any backend-required headers (hdr)
		// would be unenforceable here. Today no adapter returns a non-nil
		// hdr on GET (S3/GCS/Azure/localfs all return nil — Azure only
		// uses the header channel for PUT). If a future backend starts
		// returning GET headers, the direct path becomes unsafe: a fetch
		// from git would 400 silently. Treat a non-empty hdr as
		// "backend incompatible with bundle-uri direct mode" — in Auto
		// mode we fall through to proxied; in Direct mode we surface a
		// clear error rather than emitting an unusable URL.
		if err == nil && len(hdr) > 0 {
			// Wrap as ErrNotSupported so URIModeAuto falls through to
			// proxied (the safe path) while URIModeDirect surfaces the
			// reason via the same channel as a hard capability failure.
			err = fmt.Errorf("gateway: backend requires request headers on GET (%d) which v2 bundle-uri/packfile-uri cannot pin; backend not usable for direct-mode advertisement: %w", len(hdr), storage.ErrNotSupported)
		}
		if err == nil {
			return signedURL, "direct", nil
		}
		if b.Mode == URIModeDirect {
			return "", "", err
		}
		if !errors.Is(err, storage.ErrNotSupported) {
			// Direct attempt failed for a non-capability reason; surface it.
			return "", "", err
		}
		// Fall through to proxied.
	}
	// Proxied mode (or auto fallback).
	if len(b.ProxiedKey) == 0 || b.ProxiedBaseURL == "" {
		return "", "", fmt.Errorf("gateway: proxied URLs are not configured")
	}
	// M19: defensive — empty tenant/repo would mint a structurally-broken
	// URL like /_bundle//site/<hash> that the handler silently 404s on, so
	// the bundle-uri advertisement would vanish without error. Fail loudly
	// instead so misconfiguration surfaces at mint time.
	if tenant == "" || repo == "" {
		return "", "", fmt.Errorf("gateway: empty tenant or repo in proxied URL request (tenant=%q, repo=%q)", tenant, repo)
	}
	exp := b.now().Add(ttl)
	// M19: token hash field is the composite "<tenant>/<repo>/<hash>".
	// Any tamper of the URL path segments (tenant, repo, or hash) produces
	// a different composite on the verify side and fails HMAC.
	composite := tenant + "/" + repo + "/" + hash
	tok, err := proxiedurl.Mint(b.ProxiedKey, kind, composite, exp)
	if err != nil {
		return "", "", err
	}
	var path string
	switch kind {
	case "bundle":
		path = "/_bundle/"
	case "pack":
		path = "/_pack/"
	default:
		// Defense-in-depth: proxiedurl.Mint already rejected non-bundle/pack
		// kinds above, so reaching here means buildURL and Mint have drifted
		// out of sync. Fail loudly rather than emit a malformed URL.
		return "", "", fmt.Errorf("gateway: unsupported kind %q", kind)
	}
	// Canonicalize: trim a trailing slash from the operator-supplied base
	// URL so the join doesn't double it. Each path segment is escaped
	// independently — tenant/repo already pass name validators with a
	// safe charset, but PathEscape is belt-and-suspenders for any future
	// validator widening. tok contains base64url chars; QueryEscape is
	// likewise defensive.
	base := strings.TrimRight(b.ProxiedBaseURL, "/")
	return base + path +
		url.PathEscape(tenant) + "/" +
		url.PathEscape(repo) + "/" +
		url.PathEscape(hash) +
		"?token=" + url.QueryEscape(tok), "proxied", nil
}
