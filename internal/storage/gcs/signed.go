package gcs

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gstorage "cloud.google.com/go/storage"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// SignedGetURL returns a v4 signed URL for time-limited object access.
// opts.Expires is clamped to PresignDefaultTTL when zero. If the
// configured credentials cannot sign URLs (e.g., metadata server tokens
// against fake-gcs-server), returns ErrNotSupported and the conformance
// suite skips §29 #10.
//
// opts.Method selects the operation:
//   - "" or "GET" (case-insensitive): presigns a GET URL. GCS signed
//     URLs do not bind to a SHA-256 (x-goog-hash carries CRC32C and
//     optionally MD5, not SHA-256). When opts.ExpectedHash is set the
//     adapter takes no action — the caller is responsible for hashing
//     the downloaded body and comparing it against opts.ExpectedHash.
//     The field is accepted without error but not honored at the URL
//     layer; integrity is the caller's responsibility on GCS.
//   - "PUT" (case-insensitive): presigns a PUT URL for direct object
//     upload. ExpectedHash is ignored on PUT; end-to-end integrity is
//     enforced by a post-upload verify step (see internal/lfs in M13).
//   - any other value: returns ErrInvalidArgument.
//
// Emulator behavior: when cfg.Endpoint is set, the host + scheme of
// that endpoint are propagated into SignedURLOptions so the minted URL
// points at the emulator rather than storage.googleapis.com. The
// emulator (e.g. fake-gcs-server) ignores the cryptographic signature
// but the URL plumbing — host, query layout, expected-method — is
// validated end-to-end. Production (Endpoint == "") is unchanged.
//
// The returned header set is nil — GCS v4 signing does not require
// any client-side request headers beyond what the URL already binds
// (Content-Type and other headers can be added by the client but are
// not mandatory for the upload to succeed). This is the inverse of
// Azure Blob's PUT path, which requires `x-ms-blob-type` on the
// request.
func (g *GCS) SignedGetURL(ctx context.Context, key string, opts bvstorage.SignedURLOptions) (string, http.Header, error) {
	if err := validateKey(key); err != nil {
		return "", nil, err
	}
	ttl := opts.Expires
	if ttl <= 0 {
		ttl = g.cfg.PresignDefaultTTL
	}
	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "PUT" {
		return "", nil, fmt.Errorf("gcs: signed-URL method %q: %w", opts.Method, bvstorage.ErrInvalidArgument)
	}
	sopts := &gstorage.SignedURLOptions{
		Method:  method,
		Expires: time.Now().Add(ttl),
		Scheme:  gstorage.SigningSchemeV4,
	}
	if host, insecure, ok := emulatorHostAndScheme(g.cfg.Endpoint); ok {
		sopts.Hostname = host
		sopts.Insecure = insecure
	}
	// Pass cached service-account credentials when present. Required
	// against fake-gcs-server / STORAGE_EMULATOR_HOST setups: emulator
	// mode skips the SDK's credential chain, so SignedURL has no way
	// to auto-detect GoogleAccessID otherwise. On real GCS with ADC
	// (workload identity / metadata server), both fields are empty
	// and the SDK signs via the IAM credentials API as before.
	if g.signGoogleAccessID != "" && len(g.signPrivateKey) > 0 {
		sopts.GoogleAccessID = g.signGoogleAccessID
		sopts.PrivateKey = g.signPrivateKey
	}
	url, err := g.bucket.SignedURL(applyPrefix(g.cfg.Prefix, key), sopts)
	if err != nil {
		// Translate sign-failure into ErrNotSupported so the
		// conformance suite probes correctly. Network/auth failures
		// against real GCS will still propagate via the suite as a
		// hard error.
		return "", nil, wrap(bvstorage.ErrNotSupported, err)
	}
	return url, nil, nil
}

// emulatorHostAndScheme extracts a host + insecure flag from a
// configured endpoint URL (cfg.Endpoint). Returns ok=false when
// endpoint is empty or unparseable — both cases let the SDK fall back
// to the production default (storage.googleapis.com over HTTPS).
//
// The endpoint typically looks like "http://localhost:4443/storage/v1/"
// (fake-gcs-server). Only the host:port and scheme are taken; the
// path component is ignored — signed URLs are minted at the bucket
// root regardless of the API base path.
func emulatorHostAndScheme(endpoint string) (host string, insecure bool, ok bool) {
	if endpoint == "" {
		return "", false, false
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", false, false
	}
	return u.Host, u.Scheme == "http", true
}
