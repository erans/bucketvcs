package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Claims is the decoded JWT claim set. Values are whatever encoding/json
// produces: strings, float64 numbers, bools, []any, map[string]any.
type Claims map[string]any

// ErrInvalidToken is the sentinel for any verification failure. Callers map
// it to a uniform 401 so the wire never reveals which gate failed.
var ErrInvalidToken = errors.New("oidc: invalid token")

// String returns the string-typed claim, or "" if absent or not a string.
func (c Claims) String(name string) string {
	s, _ := c[name].(string)
	return s
}

// numericDate reads a JSON-numeric claim (float64) as a unix timestamp.
func (c Claims) numericDate(name string) (int64, bool) {
	switch v := c[name].(type) {
	case float64:
		return int64(v), true
	default:
		return 0, false
	}
}

// validateStandardClaims checks iss (exact), exp (required), nbf and iat,
// each with +/- skew seconds of tolerance. aud is NOT checked here.
func validateStandardClaims(c Claims, expectedIss string, now, skew int64) error {
	if c.String("iss") != expectedIss {
		return fmt.Errorf("%w: issuer mismatch", ErrInvalidToken)
	}
	exp, ok := c.numericDate("exp")
	if !ok {
		return fmt.Errorf("%w: missing exp", ErrInvalidToken)
	}
	if now-skew >= exp {
		return fmt.Errorf("%w: token expired", ErrInvalidToken)
	}
	if nbf, ok := c.numericDate("nbf"); ok && now+skew < nbf {
		return fmt.Errorf("%w: token not yet valid", ErrInvalidToken)
	}
	if iat, ok := c.numericDate("iat"); ok && now+skew < iat {
		return fmt.Errorf("%w: issued in the future", ErrInvalidToken)
	}
	return nil
}

// allowedAlgs is the asymmetric allowlist. HMAC algorithms and "none" are
// deliberately excluded — go-jose rejects any token whose header alg is not
// in this list, closing the alg-confusion family of attacks.
var allowedAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384,
}

// parseJWKS decodes a JWKS document.
func parseJWKS(b []byte) (*jose.JSONWebKeySet, error) {
	var ks jose.JSONWebKeySet
	if err := json.Unmarshal(b, &ks); err != nil {
		return nil, fmt.Errorf("%w: malformed jwks", ErrInvalidToken)
	}
	return &ks, nil
}

// verifySignature parses the compact JWS, enforces the alg allowlist, and
// verifies the signature against keyset. It returns the decoded claims on
// success. It does NOT validate any claim values.
func verifySignature(raw string, keyset *jose.JSONWebKeySet) (Claims, error) {
	sig, err := jose.ParseSigned(raw, allowedAlgs)
	if err != nil {
		return nil, fmt.Errorf("%w: parse: %v", ErrInvalidToken, err)
	}
	payload, err := sig.Verify(keyset)
	if err != nil {
		return nil, fmt.Errorf("%w: signature: %w", ErrInvalidToken, err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("%w: claims decode", ErrInvalidToken)
	}
	return c, nil
}

// ErrIssuerUnavailable indicates discovery or JWKS retrieval failed (network
// error or non-200). Callers map it to 503 — distinct from ErrInvalidToken's
// 401 — and must NOT count it as a credential failure.
var ErrIssuerUnavailable = errors.New("oidc: issuer discovery or JWKS unavailable")

// Verifier performs discovery + JWKS-cached signature verification. It is
// safe for concurrent use. Construct with NewVerifier.
type Verifier struct {
	// HTTPClient is used for discovery + JWKS. Defaults to a 10s-timeout client.
	HTTPClient *http.Client
	// Skew is the allowed clock skew for exp/nbf/iat. Default 60s.
	Skew time.Duration
	// DiscoveryTTL bounds how long a discovery doc is cached. Default 1h.
	DiscoveryTTL time.Duration
	// MinRefreshInterval throttles refresh-on-unknown-kid. Default 1m.
	MinRefreshInterval time.Duration

	mu        sync.Mutex
	keysets   map[string]*keyset // keyed by issuer URL
	discovery map[string]discoveryDoc
	discAt    map[string]time.Time
}

// NewVerifier returns a Verifier with default timeouts.
func NewVerifier() *Verifier {
	return &Verifier{
		HTTPClient:         &http.Client{Timeout: 10 * time.Second},
		Skew:               60 * time.Second,
		DiscoveryTTL:       time.Hour,
		MinRefreshInterval: time.Minute,
		keysets:            map[string]*keyset{},
		discovery:          map[string]discoveryDoc{},
		discAt:             map[string]time.Time{},
	}
}

// Verify fetches/uses the cached JWKS for issuer, verifies the token's
// signature, and validates standard claims (iss/exp/nbf/iat). On an unknown
// kid it refreshes the JWKS at most once per MinRefreshInterval. It returns
// the decoded claims; the caller validates aud against a trust rule.
func (v *Verifier) Verify(ctx context.Context, raw, issuer string) (Claims, error) {
	ks, err := v.getKeyset(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIssuerUnavailable, err)
	}
	claims, err := verifySignature(raw, ks.set)
	if err != nil && errors.Is(err, jose.ErrJWKSKidNotFound) {
		// Unknown kid: the issuer may have rotated keys. Refresh once
		// (throttled by MinRefreshInterval) and retry. Genuine signature
		// failures over a known kid do NOT trigger a refetch.
		if ks2, rerr := v.refreshKeyset(ctx, issuer, ks); rerr == nil && ks2 != nil {
			claims, err = verifySignature(raw, ks2.set)
		}
	}
	if err != nil {
		return nil, err
	}
	if verr := validateStandardClaims(claims, issuer, time.Now().Unix(), int64(v.Skew.Seconds())); verr != nil {
		return nil, verr
	}
	return claims, nil
}

func (v *Verifier) getKeyset(ctx context.Context, issuer string) (*keyset, error) {
	v.mu.Lock()
	ks := v.keysets[issuer]
	v.mu.Unlock()
	if ks != nil {
		return ks, nil
	}
	disc, err := v.getDiscovery(ctx, issuer)
	if err != nil {
		return nil, err
	}
	set, err := fetchJWKS(ctx, v.HTTPClient, disc.JWKSURI)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	// lastRefresh is zero on initial fetch so the first rotation-triggered
	// refresh (refresh-on-unknown-kid) is never throttled.
	ks = &keyset{set: set, jwksURI: disc.JWKSURI, fetchedAt: now}
	v.mu.Lock()
	v.keysets[issuer] = ks
	v.mu.Unlock()
	return ks, nil
}

func (v *Verifier) refreshKeyset(ctx context.Context, issuer string, old *keyset) (*keyset, error) {
	v.mu.Lock()
	cur := v.keysets[issuer]
	if cur != nil && time.Since(cur.lastRefresh) < v.MinRefreshInterval {
		v.mu.Unlock()
		return nil, fmt.Errorf("refresh throttled")
	}
	v.mu.Unlock()
	set, err := fetchJWKS(ctx, v.HTTPClient, old.jwksURI)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	ks := &keyset{set: set, jwksURI: old.jwksURI, fetchedAt: now, lastRefresh: now}
	v.mu.Lock()
	v.keysets[issuer] = ks
	v.mu.Unlock()
	return ks, nil
}

func (v *Verifier) getDiscovery(ctx context.Context, issuer string) (discoveryDoc, error) {
	v.mu.Lock()
	d, ok := v.discovery[issuer]
	at := v.discAt[issuer]
	v.mu.Unlock()
	if ok && time.Since(at) < v.DiscoveryTTL {
		return d, nil
	}
	d, err := fetchDiscovery(ctx, v.HTTPClient, issuer)
	if err != nil {
		return discoveryDoc{}, err
	}
	v.mu.Lock()
	v.discovery[issuer] = d
	v.discAt[issuer] = time.Now()
	v.mu.Unlock()
	return d, nil
}
