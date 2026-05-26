package oidc

import (
	"crypto/rsa"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
)

func TestValidateStandardClaims(t *testing.T) {
	const iss = "https://issuer.example"
	now := int64(1_000_000)
	skew := int64(60)

	base := func() Claims {
		return Claims{
			"iss": iss,
			"aud": "https://bucketvcs.example",
			"sub": "repo:org/app:ref:refs/heads/main",
			"exp": float64(now + 300),
			"iat": float64(now - 10),
			"nbf": float64(now - 10),
		}
	}

	t.Run("valid", func(t *testing.T) {
		if err := validateStandardClaims(base(), iss, now, skew); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	})
	t.Run("wrong issuer", func(t *testing.T) {
		c := base()
		c["iss"] = "https://evil.example"
		if err := validateStandardClaims(c, iss, now, skew); err == nil {
			t.Fatal("want error for wrong iss")
		}
	})
	t.Run("expired", func(t *testing.T) {
		c := base()
		c["exp"] = float64(now - 120)
		if err := validateStandardClaims(c, iss, now, skew); err == nil {
			t.Fatal("want error for expired")
		}
	})
	t.Run("missing exp", func(t *testing.T) {
		c := base()
		delete(c, "exp")
		if err := validateStandardClaims(c, iss, now, skew); err == nil {
			t.Fatal("want error for missing exp")
		}
	})
	t.Run("not yet valid", func(t *testing.T) {
		c := base()
		c["nbf"] = float64(now + 120)
		if err := validateStandardClaims(c, iss, now, skew); err == nil {
			t.Fatal("want error for future nbf")
		}
	})
	t.Run("exp within skew tolerated", func(t *testing.T) {
		c := base()
		c["exp"] = float64(now - 30) // expired 30s ago, within 60s skew
		if err := validateStandardClaims(c, iss, now, skew); err != nil {
			t.Fatalf("want nil within skew, got %v", err)
		}
	})
}

func TestVerifySignature(t *testing.T) {
	key := newRSAKey(t)
	jwksJSON := publicJWKS(t, map[string]*rsa.PrivateKey{"k1": key})
	ks := parseJWKSForTest(t, jwksJSON)

	tok := signToken(t, key, "k1", map[string]any{"iss": "https://i.example", "sub": "s"})

	t.Run("valid signature returns claims", func(t *testing.T) {
		c, err := verifySignature(tok, ks)
		if err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if c.String("sub") != "s" {
			t.Fatalf("sub = %q", c.String("sub"))
		}
	})

	t.Run("wrong key rejected", func(t *testing.T) {
		other := newRSAKey(t)
		badKS := parseJWKSForTest(t, publicJWKS(t, map[string]*rsa.PrivateKey{"k1": other}))
		if _, err := verifySignature(tok, badKS); err == nil {
			t.Fatal("want signature failure")
		}
	})

	t.Run("alg none rejected", func(t *testing.T) {
		// header {"alg":"none"} . {"sub":"x"} . (empty sig)
		none := "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0."
		if _, err := verifySignature(none, ks); err == nil {
			t.Fatal("want rejection of alg:none")
		}
	})
}

// parseJWKSForTest wraps the production keyset parser for tests.
func parseJWKSForTest(t *testing.T, b []byte) *jose.JSONWebKeySet {
	t.Helper()
	ks, err := parseJWKS(b)
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	return ks
}
