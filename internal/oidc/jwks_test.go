package oidc

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newIssuerServer serves a discovery doc + JWKS. jwksHits counts JWKS fetches.
func newIssuerServer(t *testing.T, keys map[string]*rsa.PrivateKey, jwksHits *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   srv.URL,
			"jwks_uri": srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(jwksHits, 1)
		w.Write(publicJWKS(t, keys))
	})
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifierVerify(t *testing.T) {
	key := newRSAKey(t)
	var hits int32
	srv := newIssuerServer(t, map[string]*rsa.PrivateKey{"k1": key}, &hits)

	v := NewVerifier()
	v.HTTPClient = srv.Client()
	now := time.Now().Unix()
	tok := signToken(t, key, "k1", map[string]any{
		"iss": srv.URL,
		"aud": "aud1",
		"sub": "s",
		"exp": float64(now + 300),
		"iat": float64(now - 10),
	})

	t.Run("valid token verifies and caches jwks", func(t *testing.T) {
		c, err := v.Verify(context.Background(), tok, srv.URL)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if c.String("aud") != "aud1" {
			t.Fatalf("aud = %q", c.String("aud"))
		}
		// Second verify must hit cache (no extra JWKS fetch).
		if _, err := v.Verify(context.Background(), tok, srv.URL); err != nil {
			t.Fatalf("verify 2: %v", err)
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Fatalf("jwks fetched %d times, want 1 (cached)", got)
		}
	})

	t.Run("unknown kid triggers exactly one refresh then fails", func(t *testing.T) {
		before := atomic.LoadInt32(&hits)
		bad := signToken(t, key, "missing", map[string]any{
			"iss": srv.URL, "sub": "s", "exp": float64(now + 300),
		})
		if _, err := v.Verify(context.Background(), bad, srv.URL); err == nil {
			t.Fatal("want failure for unknown kid")
		}
		if got := atomic.LoadInt32(&hits) - before; got != 1 {
			t.Fatalf("unknown-kid caused %d refreshes, want exactly 1", got)
		}
	})
}

func TestVerifierRejectsInsecureIssuer(t *testing.T) {
	v := NewVerifier()
	// A non-loopback http issuer must be rejected before any network call.
	_, err := v.Verify(context.Background(), "x.y.z", "http://example.com")
	if err == nil {
		t.Fatal("want rejection of non-loopback http issuer")
	}
	if !errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("want ErrIssuerUnavailable, got %v", err)
	}
}
