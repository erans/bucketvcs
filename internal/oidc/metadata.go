package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Metadata is the subset of OIDC provider metadata a relying party needs.
type Metadata struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	JWKSURI               string
}

// Discover fetches <issuer>/.well-known/openid-configuration and returns the
// authorization/token/jwks endpoints. The issuer must be HTTPS (except
// loopback) and the document's "issuer" field must equal the requested issuer.
func Discover(ctx context.Context, hc *http.Client, issuer string) (Metadata, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if err := requireSecureURL(issuer); err != nil {
		return Metadata{}, fmt.Errorf("%w: %v", ErrIssuerUnavailable, err)
	}
	url := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Metadata{}, fmt.Errorf("%w: %v", ErrIssuerUnavailable, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Metadata{}, fmt.Errorf("%w: %v", ErrIssuerUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Metadata{}, fmt.Errorf("%w: discovery status %d", ErrIssuerUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return Metadata{}, fmt.Errorf("%w: %v", ErrIssuerUnavailable, err)
	}
	if len(body) > 1<<20 {
		return Metadata{}, fmt.Errorf("%w: discovery response exceeds 1MiB", ErrIssuerUnavailable)
	}
	var doc struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		JWKSURI               string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Metadata{}, fmt.Errorf("%w: discovery decode: %v", ErrIssuerUnavailable, err)
	}
	if doc.Issuer != issuer {
		return Metadata{}, fmt.Errorf("%w: discovery issuer mismatch (%q != %q)", ErrInvalidToken, doc.Issuer, issuer)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return Metadata{}, fmt.Errorf("%w: discovery missing endpoints", ErrIssuerUnavailable)
	}
	return Metadata{
		Issuer:                doc.Issuer,
		AuthorizationEndpoint: doc.AuthorizationEndpoint,
		TokenEndpoint:         doc.TokenEndpoint,
		JWKSURI:               doc.JWKSURI,
	}, nil
}
