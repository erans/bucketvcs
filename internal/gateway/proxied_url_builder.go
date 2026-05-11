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

// BuildBundleURL returns (url, via) where via is "direct" or "proxied".
// Returns error if Mode == URIModeOff or if Direct mode + no signing.
func (b *URLBuilder) BuildBundleURL(ctx context.Context, hash, storageKey, expectedHash string) (string, string, error) {
	return b.buildURL(ctx, "bundle", hash, storageKey, expectedHash, b.BundleTTL)
}

// BuildPackURL returns (url, via).
func (b *URLBuilder) BuildPackURL(ctx context.Context, hash, storageKey, expectedHash string) (string, string, error) {
	return b.buildURL(ctx, "pack", hash, storageKey, expectedHash, b.PackTTL)
}

func (b *URLBuilder) buildURL(ctx context.Context, kind, hash, storageKey, expectedHash string, ttl time.Duration) (string, string, error) {
	if b.Mode == URIModeOff {
		return "", "", fmt.Errorf("gateway: URI mode is off")
	}
	if b.Mode == URIModeDirect || b.Mode == URIModeAuto {
		signedURL, err := b.Store.SignedGetURL(ctx, storageKey, storage.SignedURLOptions{
			Expires: ttl, Method: "GET", ExpectedHash: expectedHash,
		})
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
	exp := b.now().Add(ttl)
	tok, err := proxiedurl.Mint(b.ProxiedKey, kind, hash, exp)
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
	// URL so the join doesn't double it. PathEscape the hash and
	// QueryEscape the token even though both are charset-restricted by
	// construction (sha256-hex / 40-hex / base64url-without-padding),
	// because an unexpected character would silently produce a malformed
	// URL otherwise. tok in particular contains '-' and '_' which are
	// fine but illustrates the principle.
	base := strings.TrimRight(b.ProxiedBaseURL, "/")
	return base + path + url.PathEscape(hash) + "?token=" + url.QueryEscape(tok), "proxied", nil
}
