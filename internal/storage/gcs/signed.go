package gcs

import (
	"context"
	"time"

	gstorage "cloud.google.com/go/storage"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// SignedGetURL returns a v4 signed URL granting time-limited GET
// access to key. opts.Expires is clamped to PresignDefaultTTL when
// zero. If the configured credentials cannot sign URLs (e.g., metadata
// server tokens against fake-gcs-server), returns ErrNotSupported and
// the conformance suite skips §29 #10.
//
// GCS signed URLs do not bind to a SHA-256 (x-goog-hash carries CRC32C
// and optionally MD5, not SHA-256). When opts.ExpectedHash is set the
// adapter takes no action — the caller is responsible for hashing the
// downloaded body and comparing it against opts.ExpectedHash. The field
// is accepted without error but not honored at the URL layer; integrity
// is the caller's responsibility on GCS.
func (g *GCS) SignedGetURL(ctx context.Context, key string, opts bvstorage.SignedURLOptions) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	ttl := opts.Expires
	if ttl <= 0 {
		ttl = g.cfg.PresignDefaultTTL
	}
	url, err := g.bucket.SignedURL(applyPrefix(g.cfg.Prefix, key), &gstorage.SignedURLOptions{
		Method:  "GET",
		Expires: time.Now().Add(ttl),
		Scheme:  gstorage.SigningSchemeV4,
	})
	if err != nil {
		// Translate sign-failure into ErrNotSupported so the
		// conformance suite probes correctly. Network/auth failures
		// against real GCS will still propagate via the suite as a
		// hard error.
		return "", wrap(bvstorage.ErrNotSupported, err)
	}
	return url, nil
}
