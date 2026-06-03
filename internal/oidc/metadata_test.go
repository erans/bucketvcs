package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscover(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
		})
	}))
	defer srv.Close()

	md, err := Discover(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if md.Issuer != srv.URL || md.AuthorizationEndpoint != srv.URL+"/authorize" ||
		md.TokenEndpoint != srv.URL+"/token" || md.JWKSURI != srv.URL+"/jwks" {
		t.Fatalf("metadata = %+v", md)
	}
}

func TestDiscover_IssuerMismatch(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": "https://evil.example.com", "token_endpoint": srv.URL + "/token",
		})
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("want error on issuer mismatch, got nil")
	}
}
