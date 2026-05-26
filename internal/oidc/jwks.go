package oidc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// keyset is a cached JWKS for one issuer, with the timestamp of the last
// successful fetch (used to throttle refresh-on-unknown-kid).
type keyset struct {
	set         *jose.JSONWebKeySet
	jwksURI     string
	fetchedAt   time.Time
	lastRefresh time.Time
}

func fetchJWKS(ctx context.Context, hc *http.Client, jwksURI string) (*jose.JSONWebKeySet, error) {
	if err := requireSecureURL(jwksURI); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 1<<20 {
		return nil, fmt.Errorf("jwks response exceeds 1MiB")
	}
	return parseJWKS(body)
}
