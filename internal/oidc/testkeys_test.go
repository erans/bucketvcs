package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// newRSAKey returns a fresh 2048-bit RSA key.
func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	return k
}

// publicJWKS returns a JWKS JSON document containing the public halves of the
// given keys, each tagged with its kid and RS256.
func publicJWKS(t *testing.T, keys map[string]*rsa.PrivateKey) []byte {
	t.Helper()
	var set jose.JSONWebKeySet
	for kid, k := range keys {
		set.Keys = append(set.Keys, jose.JSONWebKey{
			Key:       k.Public(),
			KeyID:     kid,
			Algorithm: "RS256",
			Use:       "sig",
		})
	}
	b, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return b
}

// signToken signs claims with key under kid using RS256 and returns the
// compact serialization.
func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	sk := jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: kid, Algorithm: "RS256"}}
	signer, err := jose.NewSigner(sk, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	s, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}
