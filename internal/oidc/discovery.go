package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type discoveryDoc struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// requireSecureURL rejects non-https URLs, except http on loopback hosts
// (localhost / 127.0.0.1 / ::1) for local development and tests. JWKS and
// discovery key material must not travel in cleartext over a real network.
func requireSecureURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("insecure url scheme %q (https required for %s)", u.Scheme, raw)
}

// fetchDiscovery retrieves <issuer>/.well-known/openid-configuration and
// returns the parsed doc. It verifies the returned "issuer" matches.
func fetchDiscovery(ctx context.Context, hc *http.Client, issuer string) (discoveryDoc, error) {
	if err := requireSecureURL(issuer); err != nil {
		return discoveryDoc{}, err
	}
	u := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return discoveryDoc{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return discoveryDoc{}, fmt.Errorf("discovery fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return discoveryDoc{}, fmt.Errorf("discovery status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return discoveryDoc{}, err
	}
	if len(body) > 1<<20 {
		return discoveryDoc{}, fmt.Errorf("discovery response exceeds 1MiB")
	}
	var d discoveryDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return discoveryDoc{}, fmt.Errorf("discovery decode: %w", err)
	}
	if d.Issuer != issuer {
		return discoveryDoc{}, fmt.Errorf("discovery issuer mismatch: got %q want %q", d.Issuer, issuer)
	}
	if d.JWKSURI == "" {
		return discoveryDoc{}, fmt.Errorf("discovery missing jwks_uri")
	}
	return d, nil
}
