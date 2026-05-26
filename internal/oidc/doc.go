// Package oidc verifies OIDC id_tokens for the M22 token-exchange endpoint.
//
// It performs OpenID Connect discovery (fetching
// <issuer>/.well-known/openid-configuration), caches the issuer's JWKS,
// and verifies a signed JWT's signature and standard claims (iss, exp,
// nbf, iat). Audience (aud) is intentionally validated by the caller
// against a matched trust rule, not here.
//
// Signature verification uses github.com/go-jose/go-jose/v4. Only
// asymmetric algorithms are accepted (RS256/384/512, ES256/384); the
// "none" algorithm and all HMAC algorithms are rejected, which makes the
// RS256<->HS256 confusion attack unrepresentable.
//
// Discovery and JWKS endpoints are fetched over HTTPS only; plain HTTP is
// permitted solely for loopback hosts (localhost, 127.0.0.1, ::1) in
// development and tests.
//
// Known v1 limitation: issuers that publish keys without a "kid" field, or
// issue tokens without a "kid" header, are not supported because key
// selection is by kid. Multi-key fallback is not implemented.
package oidc
