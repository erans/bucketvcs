# M22: OIDC Token Exchange — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a CI/CD workload exchange its IdP-issued OIDC `id_token` at `POST /_oidc/token` for a short-lived, repo-scoped `bvts` token (RFC 8693), which then flows through the existing auth stack unchanged.

**Architecture:** A new pure `internal/oidc` package does discovery + JWKS + JWT verification (using the already-vendored `go-jose/v4`). A trust-rules table in the authdb maps `(issuer, audience, exact claims) → (tenant, repo, scopes, ttl)`. The gateway exchange handler verifies the JWT, matches the first rule, and mints a `bvts` token bound to one repo via new `tokens.scope_*` columns — reusing the deploy-key `auth.Scope` short-circuit so the existing middleware and `CheckScope` sites need no changes. A periodic sweep reaps expired minted tokens.

**Tech Stack:** Go 1.25, `github.com/go-jose/go-jose/v4` (already in go.sum, promoted to direct), `modernc.org/sqlite`, stdlib `net/http` + `crypto`.

**Spec:** `docs/superpowers/specs/2026-05-25-m22-oidc-token-exchange-design.md`

---

## Conventions used in this plan

- Run all tests with `go test ./... ` unless a tighter path is given.
- The authdb is the M4 sqlite store at `internal/auth/sqlitestore`; migrations are numbered SQL files in `internal/auth/sqlitestore/migrations/` applied in lexical order. Last existing: `0009_hooks.sql`.
- Metrics are structured slog records, NOT OTel — mirror `internal/lfs/metrics.go` (`logger.LogAttrs(ctx, slog.LevelInfo, "metric", slog.String("metric_name", …), slog.Int64("value", …), …)`).
- Audit events mirror `internal/auth/audit.go` (`logger.LogAttrs(ctx, level, "event.name", attrs…)`, nil logger → `slog.Default()`).
- CLI subcommands mirror `cmd/bucketvcs/policy.go` (flag.FlagSet, exit codes: 0 ok / 1 operational / 2 usage).

---

## File Structure

**New files:**
- `internal/oidc/doc.go` — package doc
- `internal/oidc/verify.go` — `Claims`, standard-claim validation, JWS signature verification, `Verifier.Verify`
- `internal/oidc/discovery.go` — discovery doc fetch + cache
- `internal/oidc/jwks.go` — JWKS cache (lazy fetch, refresh-on-unknown-kid, min-refresh guard)
- `internal/oidc/verify_test.go`, `internal/oidc/discovery_test.go`, `internal/oidc/jwks_test.go`, `internal/oidc/testkeys_test.go`
- `internal/auth/oidctypes.go` — `OIDCIssuer`, `OIDCTrustRule`, pure `MatchRule`
- `internal/auth/oidctypes_test.go`
- `internal/auth/sqlitestore/migrations/0010_oidc.sql`
- `internal/auth/sqlitestore/oidc.go` — issuer/rule CRUD + `FindOIDCIssuerByURL` + `ListOIDCRulesForIssuer` + `MintOIDCToken` + `SweepExpiredOIDCTokens`
- `internal/auth/sqlitestore/oidc_test.go`
- `internal/gateway/oidc_exchange.go` — the exchange HTTP handler
- `internal/gateway/oidc_exchange_test.go`
- `cmd/bucketvcs/oidc.go` — `bucketvcs oidc issuer|rule …` CLI
- `cmd/bucketvcs/oidc_test.go`
- `scripts/smoke-oidc.sh` — localfs end-to-end smoke
- `docs/m22-oidc-operator-guide.md`

**Modified files:**
- `go.mod` — promote `github.com/go-jose/go-jose/v4` to the direct require block
- `internal/auth/sqlitestore/store.go` — `Token` struct + `GetTokenByID` select gain `scope_*`; `CreateToken` gains binding params; `verifyBasicPassword` returns `*auth.Scope` + synthetic actor for repo-bound tokens
- `internal/gateway/server.go` — `Options` gains `OIDCEnabled`, `OIDCStore`, `OIDCVerifier`; mount `/_oidc/`
- `cmd/bucketvcs/serve.go` — flags, verifier construction, OIDC store wiring, sweep goroutine
- `cmd/bucketvcs/main.go` — register `oidc` subcommand + usage line

---

## Task 0: Promote go-jose to a direct dependency + package skeleton

**Files:**
- Modify: `go.mod`
- Create: `internal/oidc/doc.go`

- [ ] **Step 1: Add the package doc file**

Create `internal/oidc/doc.go`:

```go
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
package oidc
```

- [ ] **Step 2: Confirm go-jose is available (promotion is automatic at Task 2)**

`github.com/go-jose/go-jose/v4` is already in `go.sum` (pulled in transitively). It becomes a *direct* dependency automatically when `go mod tidy` runs after Task 2 adds the first direct import — a manual move into the direct `require` block now would just be reverted by tidy, because nothing imports it yet. So do NOT hand-edit go.mod here; only confirm the build is green:

Run: `go build ./...` (from the repo/worktree root)
Expected: builds clean. (go.mod is unchanged by this task.)

- [ ] **Step 3: Commit**

```bash
git add internal/oidc/doc.go
git commit -m "M22: add internal/oidc package skeleton"
```

---

## Task 1: Standard-claim validation (pure, no network)

**Files:**
- Create: `internal/oidc/verify.go`
- Test: `internal/oidc/verify_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/oidc/verify_test.go`:

```go
package oidc

import "testing"

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
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/oidc/ -run TestValidateStandardClaims`
Expected: FAIL — `Claims`, `validateStandardClaims` undefined.

- [ ] **Step 3: Implement**

Add to `internal/oidc/verify.go`:

```go
package oidc

import (
	"errors"
	"fmt"
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
	case int64:
		return v, true
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/oidc/ -run TestValidateStandardClaims`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/oidc/verify.go internal/oidc/verify_test.go
git commit -m "M22: oidc standard-claim validation (iss/exp/nbf/iat + skew)"
```

---

## Task 2: JWS signature verification against a static JWKS

**Files:**
- Modify: `internal/oidc/verify.go`
- Test: `internal/oidc/testkeys_test.go`, `internal/oidc/verify_test.go`

- [ ] **Step 1: Write a test-key helper**

Create `internal/oidc/testkeys_test.go`:

```go
package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// newRSAKey returns a fresh 2048-bit RSA key with the given kid.
func newRSAKey(t *testing.T, kid string) *rsa.PrivateKey {
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
```

- [ ] **Step 2: Write the failing test**

Append to `internal/oidc/verify_test.go`:

```go
func TestVerifySignature(t *testing.T) {
	key := newRSAKey(t, "k1")
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
		other := newRSAKey(t, "k1")
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
```

(Add `jose "github.com/go-jose/go-jose/v4"` to the test imports.)

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/oidc/ -run TestVerifySignature`
Expected: FAIL — `verifySignature`, `parseJWKS` undefined.

- [ ] **Step 4: Implement**

Append to `internal/oidc/verify.go`:

```go
import (
	"encoding/json"

	jose "github.com/go-jose/go-jose/v4"
)

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
		return nil, fmt.Errorf("%w: signature: %v", ErrInvalidToken, err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("%w: claims decode", ErrInvalidToken)
	}
	return c, nil
}
```

Note: `(*jose.JSONWebKeySet).Verify` accepts a `*JSONWebKeySet` and tries the key matching the JWS header `kid`. If the header carries a `kid` absent from the set, Verify fails — Task 3 handles refresh-on-unknown-kid above this layer.

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/oidc/ -run TestVerifySignature`
Expected: PASS.

- [ ] **Step 6: Tidy modules (promotes go-jose to direct) + commit**

Now that `verify.go` imports `go-jose/v4` directly, run `go mod tidy` — it drops the `// indirect` marker on `github.com/go-jose/go-jose/v4`, making it a direct dependency (no new module is downloaded; it was already in go.sum).

Run: `go mod tidy && go build ./...`
Expected: builds clean; `go-jose/go-jose/v4` now appears without `// indirect` in go.mod.

```bash
git add internal/oidc/verify.go internal/oidc/verify_test.go internal/oidc/testkeys_test.go go.mod go.sum
git commit -m "M22: oidc JWS signature verification with asymmetric alg allowlist"
```

---

## Task 3: Discovery + JWKS cache + Verifier.Verify

**Files:**
- Create: `internal/oidc/discovery.go`, `internal/oidc/jwks.go`
- Modify: `internal/oidc/verify.go` (add `Verifier`)
- Test: `internal/oidc/jwks_test.go`

- [ ] **Step 1: Write the failing integration test**

Create `internal/oidc/jwks_test.go`:

```go
package oidc

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
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
	key := newRSAKey(t, "k1")
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

var _ = fmt.Sprintf
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/oidc/ -run TestVerifierVerify`
Expected: FAIL — `NewVerifier`, `Verifier`, `.Verify` undefined.

- [ ] **Step 3: Implement discovery**

Create `internal/oidc/discovery.go`:

```go
package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type discoveryDoc struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// fetchDiscovery retrieves <issuer>/.well-known/openid-configuration and
// returns the parsed doc. The issuer URL must be https (or http for the
// loopback test server). It verifies the returned "issuer" matches.
func fetchDiscovery(ctx context.Context, hc *http.Client, issuer string) (discoveryDoc, error) {
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return discoveryDoc{}, err
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
```

- [ ] **Step 4: Implement the JWKS cache + Verifier**

Create `internal/oidc/jwks.go`:

```go
package oidc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseJWKS(body)
}
```

Append the `Verifier` to `internal/oidc/verify.go`:

```go
import (
	"context"
	"net/http"
	"sync"
	"time"
)

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
	if err != nil {
		// Possible key rotation: refresh once (throttled) and retry.
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
	ks = &keyset{set: set, jwksURI: disc.JWKSURI, fetchedAt: now, lastRefresh: now}
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
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/oidc/`
Expected: PASS (all oidc tests). The unknown-kid test asserts exactly one refresh because `MinRefreshInterval` defaults to 1m and the test issues one bad token.

- [ ] **Step 6: Commit**

```bash
git add internal/oidc/discovery.go internal/oidc/jwks.go internal/oidc/verify.go internal/oidc/jwks_test.go
git commit -m "M22: oidc discovery + JWKS cache + Verifier.Verify (refresh-on-unknown-kid)"
```

---

## Task 4: Migration 0010 — OIDC tables + token scope columns + _oidc user

**Files:**
- Create: `internal/auth/sqlitestore/migrations/0010_oidc.sql`
- Test: `internal/auth/sqlitestore/oidc_test.go` (migration round-trip)

- [ ] **Step 1: Write the migration**

Create `internal/auth/sqlitestore/migrations/0010_oidc.sql`:

```sql
CREATE TABLE oidc_issuers (
    alias       TEXT PRIMARY KEY,
    issuer_url  TEXT NOT NULL UNIQUE,
    created_at  INTEGER NOT NULL
);

CREATE TABLE oidc_trust_rules (
    id            TEXT PRIMARY KEY,
    issuer_alias  TEXT NOT NULL REFERENCES oidc_issuers(alias) ON DELETE CASCADE,
    audience      TEXT NOT NULL,
    tenant        TEXT NOT NULL,
    repo          TEXT NOT NULL,
    scopes        INTEGER NOT NULL,
    ttl_seconds   INTEGER NOT NULL CHECK (ttl_seconds > 0),
    created_at    INTEGER NOT NULL,
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);
CREATE INDEX oidc_rules_issuer_idx ON oidc_trust_rules(issuer_alias);
CREATE INDEX oidc_rules_repo_idx   ON oidc_trust_rules(tenant, repo);

CREATE TABLE oidc_rule_claims (
    rule_id     TEXT NOT NULL REFERENCES oidc_trust_rules(id) ON DELETE CASCADE,
    claim_name  TEXT NOT NULL,
    claim_value TEXT NOT NULL,
    PRIMARY KEY (rule_id, claim_name)
);

-- Reserved system user so OIDC-minted tokens satisfy tokens.user_id NOT NULL.
INSERT INTO users (id, name, is_admin, created_at)
VALUES ('_oidc', '_oidc', 0, strftime('%s','now'));

-- Repo-binding columns on tokens (NULL for ordinary user tokens).
ALTER TABLE tokens ADD COLUMN scope_tenant TEXT;
ALTER TABLE tokens ADD COLUMN scope_repo   TEXT;
ALTER TABLE tokens ADD COLUMN scope_perm   TEXT;

INSERT INTO schema_version (version, applied_at) VALUES (10, strftime('%s','now'));
```

- [ ] **Step 2: Write the failing migration test**

Create `internal/auth/sqlitestore/oidc_test.go`:

```go
package sqlitestore

import (
	"context"
	"testing"
)

func TestMigration0010_OIDCTablesAndSystemUser(t *testing.T) {
	s := mustOpen(t) // package test helper in store_test.go (temp-dir store, migrations applied)
	ctx := context.Background()

	// _oidc system user exists and is enabled.
	u, err := s.GetUserByName(ctx, "_oidc")
	if err != nil {
		t.Fatalf("get _oidc user: %v", err)
	}
	if u.DisabledAt != nil {
		t.Fatal("_oidc user must not be disabled")
	}

	// New oidc tables accept inserts (sanity: raw exec).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO oidc_issuers (alias, issuer_url, created_at) VALUES ('gh','https://i',1)`); err != nil {
		t.Fatalf("insert issuer: %v", err)
	}
}
```

Note: `mustOpen(t)` is the existing package test helper (`store_test.go`). Tests are in-package so `s.db` is reachable. The migration file is picked up automatically (embed.FS globs `migrations/*.sql`).

- [ ] **Step 3: Run to verify it fails, then passes**

Run: `go test ./internal/auth/sqlitestore/ -run TestMigration0010`
Expected: PASS once the migration file is picked up (embed.FS globs `migrations/*.sql`). If the helper name differs, fix the test to match the existing helper, then PASS.

- [ ] **Step 4: Verify full suite still migrates**

Run: `go test ./internal/auth/...`
Expected: PASS — existing token tests must still pass (new columns are nullable; `CreateToken`'s INSERT lists explicit columns so it is unaffected).

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/migrations/0010_oidc.sql internal/auth/sqlitestore/oidc_test.go
git commit -m "M22: authdb migration 0010 — oidc tables, token scope_* columns, _oidc user"
```

---

## Task 5: Auth domain types + pure rule matcher

**Files:**
- Create: `internal/auth/oidctypes.go`, `internal/auth/oidctypes_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/auth/oidctypes_test.go`:

```go
package auth

import "testing"

func TestMatchRule(t *testing.T) {
	rules := []OIDCTrustRule{
		{ID: "r2", Audience: "aud", Tenant: "org", Repo: "app",
			Claims: map[string]string{"repository": "org/app", "ref": "refs/heads/main"}},
		{ID: "r1", Audience: "aud", Tenant: "org", Repo: "app",
			Claims: map[string]string{"repository": "org/app"}},
		{ID: "r3", Audience: "other", Tenant: "org", Repo: "app",
			Claims: map[string]string{"repository": "org/app"}},
	}

	t.Run("all claims equal matches; first by (tenant,repo,id)", func(t *testing.T) {
		claims := map[string]any{"aud": "aud", "repository": "org/app", "ref": "refs/heads/main"}
		got := MatchRule(rules, claims)
		if got == nil || got.ID != "r1" {
			t.Fatalf("want r1 (lowest id among matches), got %+v", got)
		}
	})
	t.Run("missing required claim does not match", func(t *testing.T) {
		claims := map[string]any{"aud": "aud", "repository": "org/other"}
		if got := MatchRule(rules, claims); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
	t.Run("audience must match", func(t *testing.T) {
		claims := map[string]any{"aud": "nope", "repository": "org/app"}
		if got := MatchRule(rules, claims); got != nil {
			t.Fatalf("want nil for wrong aud, got %+v", got)
		}
	})
	t.Run("zero-claim rule is wildcard", func(t *testing.T) {
		wc := []OIDCTrustRule{{ID: "w", Audience: "aud", Tenant: "org", Repo: "app", Claims: map[string]string{}}}
		claims := map[string]any{"aud": "aud", "anything": "x"}
		if got := MatchRule(wc, claims); got == nil {
			t.Fatal("zero-claim rule should match any token from issuer")
		}
	})
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/ -run TestMatchRule`
Expected: FAIL — `OIDCTrustRule`, `MatchRule` undefined.

- [ ] **Step 3: Implement**

Create `internal/auth/oidctypes.go`:

```go
package auth

import "sort"

// OIDCIssuer is a registered trusted OIDC issuer.
type OIDCIssuer struct {
	Alias     string
	IssuerURL string
	CreatedAt int64
}

// OIDCTrustRule maps validated token claims to a repo-scoped grant. A token
// matches when its `aud` equals Audience and every entry in Claims is present
// and string-equal in the token. An empty Claims map matches any token from
// the issuer.
type OIDCTrustRule struct {
	ID          string
	IssuerAlias string
	Audience    string
	Tenant      string
	Repo        string
	Scopes      TokenScope
	TTLSeconds  int64
	Claims      map[string]string
	CreatedAt   int64
}

// MatchRule returns the first rule (ordered by Tenant, Repo, ID) whose
// audience and claim constraints all match the token claims, or nil. The
// caller is responsible for passing only rules belonging to the token's
// issuer.
func MatchRule(rules []OIDCTrustRule, claims map[string]any) *OIDCTrustRule {
	sorted := make([]OIDCTrustRule, len(rules))
	copy(sorted, rules)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Tenant != sorted[j].Tenant {
			return sorted[i].Tenant < sorted[j].Tenant
		}
		if sorted[i].Repo != sorted[j].Repo {
			return sorted[i].Repo < sorted[j].Repo
		}
		return sorted[i].ID < sorted[j].ID
	})
	aud, _ := claims["aud"].(string)
	for i := range sorted {
		r := &sorted[i]
		if r.Audience != aud {
			continue
		}
		if claimsSatisfy(r.Claims, claims) {
			out := *r
			return &out
		}
	}
	return nil
}

func claimsSatisfy(required map[string]string, claims map[string]any) bool {
	for name, want := range required {
		got, ok := claims[name].(string)
		if !ok || got != want {
			return false
		}
	}
	return true
}
```

Note on `aud`: OIDC permits `aud` to be a string or array. For v1 we match the string form only (CI providers issue single-string `aud`). Array `aud` will not match and is documented as unsupported in the operator guide.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/auth/ -run TestMatchRule`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/oidctypes.go internal/auth/oidctypes_test.go
git commit -m "M22: auth OIDCIssuer/OIDCTrustRule types + pure first-match rule matcher"
```

---

## Task 6: sqlitestore OIDC CRUD + lookup

**Files:**
- Modify: `internal/auth/sqlitestore/oidc.go` (created in Task 4 test file is separate; create the impl file)
- Test: `internal/auth/sqlitestore/oidc_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `internal/auth/sqlitestore/oidc_test.go`:

```go
import (
	"errors"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestOIDCIssuerAndRuleCRUD(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "org", "app"); err != nil {
		t.Fatal(err)
	}

	if err := s.AddOIDCIssuer(ctx, "gh", "https://token.actions.githubusercontent.com"); err != nil {
		t.Fatalf("add issuer: %v", err)
	}
	// duplicate alias -> ErrConflict
	if err := s.AddOIDCIssuer(ctx, "gh", "https://other"); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("want ErrConflict on dup alias, got %v", err)
	}

	rule := auth.OIDCTrustRule{
		IssuerAlias: "gh", Audience: "https://bvcs.example",
		Tenant: "org", Repo: "app", Scopes: auth.ScopeRepoWrite, TTLSeconds: 900,
		Claims: map[string]string{"repository": "org/app", "ref": "refs/heads/main"},
	}
	id, err := s.AddOIDCRule(ctx, rule)
	if err != nil {
		t.Fatalf("add rule: %v", err)
	}
	if id == "" {
		t.Fatal("want non-empty rule id")
	}

	// empty audience rejected
	bad := rule
	bad.Audience = ""
	if _, err := s.AddOIDCRule(ctx, bad); err == nil {
		t.Fatal("want error for empty audience")
	}

	// FindOIDCIssuerByURL
	iss, err := s.FindOIDCIssuerByURL(ctx, "https://token.actions.githubusercontent.com")
	if err != nil {
		t.Fatalf("find issuer: %v", err)
	}
	if iss.Alias != "gh" {
		t.Fatalf("alias = %q", iss.Alias)
	}

	// ListOIDCRulesForIssuer returns the rule with its claims.
	rules, err := s.ListOIDCRulesForIssuer(ctx, "gh")
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(rules) != 1 || rules[0].Claims["ref"] != "refs/heads/main" {
		t.Fatalf("rules = %+v", rules)
	}

	// Remove rule, then issuer.
	if err := s.RemoveOIDCRule(ctx, id); err != nil {
		t.Fatalf("remove rule: %v", err)
	}
	if err := s.RemoveOIDCIssuer(ctx, "gh"); err != nil {
		t.Fatalf("remove issuer: %v", err)
	}
}
```

Confirm the rule's FK is satisfiable: `RegisterRepo(ctx, "org", "app")` inserts the `repos` row (method on `*Store`, see store.go:514).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run TestOIDCIssuerAndRuleCRUD`
Expected: FAIL — methods undefined.

- [ ] **Step 3: Implement**

Create `internal/auth/sqlitestore/oidc.go`:

```go
package sqlitestore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// AddOIDCIssuer registers a trusted issuer. Returns auth.ErrConflict if the
// alias or issuer_url already exists.
func (s *Store) AddOIDCIssuer(ctx context.Context, alias, issuerURL string) error {
	if alias == "" || issuerURL == "" {
		return fmt.Errorf("oidc: alias and issuer_url required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO oidc_issuers (alias, issuer_url, created_at) VALUES (?, ?, strftime('%s','now'))`,
		alias, issuerURL)
	if err != nil {
		if isUniqueViolation(err) {
			return auth.ErrConflict
		}
		return fmt.Errorf("add oidc issuer: %w", err)
	}
	return nil
}

// ListOIDCIssuers returns all registered issuers ordered by alias.
func (s *Store) ListOIDCIssuers(ctx context.Context) ([]auth.OIDCIssuer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT alias, issuer_url, created_at FROM oidc_issuers ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.OIDCIssuer
	for rows.Next() {
		var i auth.OIDCIssuer
		if err := rows.Scan(&i.Alias, &i.IssuerURL, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// RemoveOIDCIssuer deletes an issuer, cascading to its rules. Returns
// ErrNoSuchOIDCIssuer if the alias does not exist.
func (s *Store) RemoveOIDCIssuer(ctx context.Context, alias string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oidc_issuers WHERE alias = ?`, alias)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchOIDCIssuer
	}
	return nil
}

// FindOIDCIssuerByURL resolves an issuer by its exact URL.
func (s *Store) FindOIDCIssuerByURL(ctx context.Context, issuerURL string) (auth.OIDCIssuer, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT alias, issuer_url, created_at FROM oidc_issuers WHERE issuer_url = ?`, issuerURL)
	var i auth.OIDCIssuer
	if err := row.Scan(&i.Alias, &i.IssuerURL, &i.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.OIDCIssuer{}, ErrNoSuchOIDCIssuer
		}
		return auth.OIDCIssuer{}, err
	}
	return i, nil
}

// AddOIDCRule inserts a trust rule and its claim constraints in one tx.
// Returns the generated rule id. Validates audience non-empty and ttl > 0.
func (s *Store) AddOIDCRule(ctx context.Context, r auth.OIDCTrustRule) (string, error) {
	if r.Audience == "" {
		return "", fmt.Errorf("oidc: audience required")
	}
	if r.TTLSeconds <= 0 {
		return "", fmt.Errorf("oidc: ttl must be > 0")
	}
	id := "bvor_" + randomHex(12)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO oidc_trust_rules
		   (id, issuer_alias, audience, tenant, repo, scopes, ttl_seconds, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, strftime('%s','now'))`,
		id, r.IssuerAlias, r.Audience, r.Tenant, r.Repo, int64(r.Scopes), r.TTLSeconds)
	if err != nil {
		return "", fmt.Errorf("insert rule: %w", err)
	}
	for name, val := range r.Claims {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO oidc_rule_claims (rule_id, claim_name, claim_value) VALUES (?, ?, ?)`,
			id, name, val); err != nil {
			return "", fmt.Errorf("insert claim: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ListOIDCRulesForIssuer returns rules (with claims loaded) for one issuer.
func (s *Store) ListOIDCRulesForIssuer(ctx context.Context, alias string) ([]auth.OIDCTrustRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, issuer_alias, audience, tenant, repo, scopes, ttl_seconds, created_at
		   FROM oidc_trust_rules WHERE issuer_alias = ?`, alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.OIDCTrustRule
	for rows.Next() {
		var r auth.OIDCTrustRule
		var scopes int64
		if err := rows.Scan(&r.ID, &r.IssuerAlias, &r.Audience, &r.Tenant, &r.Repo,
			&scopes, &r.TTLSeconds, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Scopes = auth.TokenScope(scopes)
		r.Claims = map[string]string{}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		cl, err := s.loadRuleClaims(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Claims = cl
	}
	return out, nil
}

// ListOIDCRulesForRepo returns rules scoped to (tenant, repo) for CLI listing.
func (s *Store) ListOIDCRulesForRepo(ctx context.Context, tenant, repo string) ([]auth.OIDCTrustRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, issuer_alias, audience, tenant, repo, scopes, ttl_seconds, created_at
		   FROM oidc_trust_rules WHERE tenant = ? AND repo = ? ORDER BY id`, tenant, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.OIDCTrustRule
	for rows.Next() {
		var r auth.OIDCTrustRule
		var scopes int64
		if err := rows.Scan(&r.ID, &r.IssuerAlias, &r.Audience, &r.Tenant, &r.Repo,
			&scopes, &r.TTLSeconds, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Scopes = auth.TokenScope(scopes)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		cl, err := s.loadRuleClaims(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Claims = cl
	}
	return out, nil
}

func (s *Store) loadRuleClaims(ctx context.Context, ruleID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT claim_name, claim_value FROM oidc_rule_claims WHERE rule_id = ?`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// RemoveOIDCRule deletes a rule (claims cascade). Returns ErrNoSuchOIDCRule
// if the id does not exist.
func (s *Store) RemoveOIDCRule(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oidc_trust_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchOIDCRule
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ErrNoSuchOIDCIssuer / ErrNoSuchOIDCRule are not-found sentinels.
var (
	ErrNoSuchOIDCIssuer = errors.New("sqlitestore: no such oidc issuer")
	ErrNoSuchOIDCRule   = errors.New("sqlitestore: no such oidc rule")
)
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/auth/sqlitestore/ -run TestOIDCIssuerAndRuleCRUD`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/oidc.go internal/auth/sqlitestore/oidc_test.go
git commit -m "M22: sqlitestore OIDC issuer/rule CRUD + FindOIDCIssuerByURL + ListOIDCRulesForIssuer"
```

---

## Task 7: Token scope binding — Token struct, CreateToken, VerifyCredential

**Files:**
- Modify: `internal/auth/sqlitestore/store.go`
- Test: `internal/auth/sqlitestore/oidc_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `internal/auth/sqlitestore/oidc_test.go`:

```go
func TestRepoBoundTokenVerifies(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "org", "app"); err != nil {
		t.Fatal(err)
	}

	token, id, secret, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	exp := timeNowPlus(900)
	if err := s.CreateToken(ctx, id, "_oidc", hash, "oidc:gh:sub", &exp,
		auth.ScopeRepoWrite, "org", "app", "write"); err != nil {
		t.Fatalf("create scoped token: %v", err)
	}

	// Any username works for a repo-bound token (the binding is the credential).
	actor, tokID, scope, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "x-access-token", Password: token})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if scope == nil || scope.Tenant != "org" || scope.Repo != "app" || scope.Perm != auth.PermWrite {
		t.Fatalf("scope = %+v", scope)
	}
	if tokID != id {
		t.Fatalf("tokID = %q want %q", tokID, id)
	}
	if actor.Scopes != auth.ScopeRepoWrite {
		t.Fatalf("actor scopes = %v", actor.Scopes)
	}
	if actor.Name != "oidc:gh:sub" {
		t.Fatalf("actor name = %q (want label-derived)", actor.Name)
	}
}
```

`timeNowPlus` helper: add to the test file if not present — `func timeNowPlus(sec int64) int64 { return time.Now().Unix() + sec }` (import `time`).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run TestRepoBoundTokenVerifies`
Expected: FAIL — `CreateToken` signature mismatch (extra args), scope not returned.

- [ ] **Step 3: Extend the Token struct + GetTokenByID**

In `internal/auth/sqlitestore/store.go`, add fields to `Token`:

```go
type Token struct {
	ID         string
	UserID     string
	SecretHash string
	Label      string
	CreatedAt  int64
	ExpiresAt  *int64
	LastUsedAt *int64
	RevokedAt  *int64
	Scopes     auth.TokenScope
	// Repo binding (M22). All three set together for OIDC-minted tokens; all
	// empty for ordinary user tokens.
	ScopeTenant string
	ScopeRepo   string
	ScopePerm   string // "read" | "write" | ""
}
```

Update `GetTokenByID`'s SELECT and Scan to include the new columns:

```go
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, secret_hash, COALESCE(label,''), created_at,
		        expires_at, last_used_at, revoked_at, scopes,
		        COALESCE(scope_tenant,''), COALESCE(scope_repo,''), COALESCE(scope_perm,'')
		   FROM tokens WHERE id = ?`, id,
	)
	t := &Token{}
	var exp, last, rev sql.NullInt64
	var scopesRaw int64
	if err := row.Scan(&t.ID, &t.UserID, &t.SecretHash, &t.Label, &t.CreatedAt,
		&exp, &last, &rev, &scopesRaw,
		&t.ScopeTenant, &t.ScopeRepo, &t.ScopePerm); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrNoSuchToken
		}
		return nil, err
	}
```

(Leave the rest of `GetTokenByID` unchanged.)

- [ ] **Step 4: Extend CreateToken with binding params**

Change `CreateToken`'s signature and INSERT:

```go
func (s *Store) CreateToken(ctx context.Context, id, userID, secretHash, label string,
	expiresAt *int64, scopes auth.TokenScope, scopeTenant, scopeRepo, scopePerm string) error {
	now := time.Now().Unix()
	var exp sql.NullInt64
	if expiresAt != nil {
		exp = sql.NullInt64{Int64: *expiresAt, Valid: true}
	}
	var lbl sql.NullString
	if label != "" {
		lbl = sql.NullString{String: label, Valid: true}
	}
	nz := func(s string) sql.NullString {
		if s == "" {
			return sql.NullString{}
		}
		return sql.NullString{String: s, Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (id, user_id, secret_hash, label, created_at, expires_at, scopes,
		                     scope_tenant, scope_repo, scope_perm)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, secretHash, lbl, now, exp, int64(scopes),
		nz(scopeTenant), nz(scopeRepo), nz(scopePerm),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return auth.ErrConflict
		}
		return fmt.Errorf("create token: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Update existing CreateToken callers**

Find all callers and pass `"", "", ""` for the new binding params (they create unbound user tokens):

Run: `grep -rn '\.CreateToken(' --include='*.go' | grep -v _test`
Expected callers include `cmd/bucketvcs/token.go:116` and any LFS/SSH token issuers. Update each call to append `, "", "", ""`. Also update any test callers found by `grep -rn '\.CreateToken(' --include='*_test.go'`.

- [ ] **Step 6: Make verifyBasicPassword return the scope**

Replace the tail of `verifyBasicPassword` (the user-lookup + return block) with:

```go
	// Repo-bound (OIDC-minted) tokens: the (tenant, repo, perm) binding IS the
	// credential, like a deploy key. The actor identity comes from the label,
	// and the URL username is not checked (CI plugs the token into any user
	// slot, e.g. x-access-token).
	if tok.ScopeTenant != "" && tok.ScopeRepo != "" {
		// The backing user (_oidc) must still be enabled.
		row := s.db.QueryRowContext(ctx,
			`SELECT disabled_at FROM users WHERE id = ?`, tok.UserID)
		var disabled sql.NullInt64
		if err := row.Scan(&disabled); err != nil {
			return nil, "", nil, auth.ErrInvalidCredential
		}
		if disabled.Valid {
			return nil, "", nil, auth.ErrUserDisabled
		}
		perm := auth.PermRead
		if tok.ScopePerm == "write" {
			perm = auth.PermWrite
		}
		name := tok.Label
		if name == "" {
			name = "oidc"
		}
		return &auth.Actor{
				UserID:  tok.UserID,
				Name:    name,
				IsAdmin: false,
				Scopes:  tok.Scopes,
			}, tokenID,
			&auth.Scope{Tenant: tok.ScopeTenant, Repo: tok.ScopeRepo, Perm: perm},
			nil
	}

	// Ordinary user token: name match + disabled check (unchanged).
	row := s.db.QueryRowContext(ctx,
		`SELECT name, is_admin, disabled_at FROM users WHERE id = ?`, tok.UserID,
	)
	var name string
	var adminInt int
	var disabled sql.NullInt64
	if err := row.Scan(&name, &adminInt, &disabled); err != nil {
		return nil, "", nil, auth.ErrInvalidCredential
	}
	if disabled.Valid {
		return nil, "", nil, auth.ErrUserDisabled
	}
	if bp.Username != name {
		return nil, "", nil, auth.ErrInvalidCredential
	}
	return &auth.Actor{
		UserID:  tok.UserID,
		Name:    name,
		IsAdmin: adminInt != 0,
		Scopes:  tok.Scopes,
	}, tokenID, nil, nil
```

- [ ] **Step 7: Run to verify it passes**

Run: `go test ./internal/auth/...`
Expected: PASS — the new test plus all existing token tests (now updated for the signature change).

- [ ] **Step 8: Commit**

```bash
git add internal/auth/sqlitestore/store.go internal/auth/sqlitestore/oidc_test.go cmd/bucketvcs/token.go
# plus any other modified callers of CreateToken
git commit -m "M22: tokens scope binding — CreateToken params, GetTokenByID, repo-bound verify path"
```

---

## Task 8: MintOIDCToken + expiry sweep

**Files:**
- Modify: `internal/auth/sqlitestore/oidc.go`
- Test: `internal/auth/sqlitestore/oidc_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `internal/auth/sqlitestore/oidc_test.go`:

```go
func TestMintAndSweepOIDCToken(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "org", "app"); err != nil {
		t.Fatal(err)
	}

	tok, err := s.MintOIDCToken(ctx, MintOIDCParams{
		Tenant: "org", Repo: "app", Perm: auth.PermWrite,
		Scopes: auth.ScopeRepoWrite, TTLSeconds: 900, Label: "oidc:gh:sub",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// The minted token authenticates and is repo-bound.
	_, _, scope, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "x", Password: tok})
	if err != nil || scope == nil || scope.Repo != "app" {
		t.Fatalf("verify minted: scope=%+v err=%v", scope, err)
	}

	// Mint an already-expired token and sweep it.
	_, err = s.MintOIDCToken(ctx, MintOIDCParams{
		Tenant: "org", Repo: "app", Perm: auth.PermRead,
		Scopes: auth.ScopeRepoRead, TTLSeconds: -1, Label: "oidc:gh:old",
	})
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	n, err := s.SweepExpiredOIDCTokens(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n < 1 {
		t.Fatalf("swept %d, want >= 1", n)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run TestMintAndSweepOIDCToken`
Expected: FAIL — `MintOIDCParams`, `MintOIDCToken`, `SweepExpiredOIDCTokens` undefined.

- [ ] **Step 3: Implement**

Append to `internal/auth/sqlitestore/oidc.go`:

```go
import "time"

// oidcSystemUserID is the reserved user inserted by migration 0010.
const oidcSystemUserID = "_oidc"

// MintOIDCParams describes a token to mint from a matched trust rule.
type MintOIDCParams struct {
	Tenant     string
	Repo       string
	Perm       auth.Perm // PermRead or PermWrite
	Scopes     auth.TokenScope
	TTLSeconds int64
	Label      string // "oidc:<alias>:<sub>"
}

// MintOIDCToken creates a short-lived repo-bound bvts token under the _oidc
// system user and returns the wire-format token string. The secret is shown
// only here (it is stored hashed).
func (s *Store) MintOIDCToken(ctx context.Context, p MintOIDCParams) (string, error) {
	token, id, secret, err := auth.GenerateToken()
	if err != nil {
		return "", err
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		return "", err
	}
	exp := time.Now().Unix() + p.TTLSeconds
	permStr := "read"
	if p.Perm == auth.PermWrite {
		permStr = "write"
	}
	if err := s.CreateToken(ctx, id, oidcSystemUserID, hash, p.Label, &exp,
		p.Scopes, p.Tenant, p.Repo, permStr); err != nil {
		return "", err
	}
	return token, nil
}

// SweepExpiredOIDCTokens deletes expired tokens owned by the _oidc system
// user and returns the number removed. Scoped to _oidc so it never touches
// operator-managed user tokens.
func (s *Store) SweepExpiredOIDCTokens(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tokens WHERE user_id = ? AND expires_at IS NOT NULL AND expires_at < ?`,
		oidcSystemUserID, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/auth/sqlitestore/ -run TestMintAndSweepOIDCToken`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/oidc.go internal/auth/sqlitestore/oidc_test.go
git commit -m "M22: MintOIDCToken + SweepExpiredOIDCTokens"
```

---

## Task 9: Gateway exchange handler

**Files:**
- Create: `internal/gateway/oidc_exchange.go`, `internal/gateway/oidc_exchange_test.go`
- Modify: `internal/gateway/server.go` (Options + mount)

- [ ] **Step 1: Define the handler interfaces + Options fields**

Add to `internal/gateway/server.go` `Options` struct:

```go
	// OIDCEnabled mounts POST /_oidc/token (M22). Requires OIDCStore and
	// OIDCVerifier when true.
	OIDCEnabled bool
	// OIDCStore resolves issuers/rules and mints repo-bound tokens.
	OIDCStore OIDCExchangeStore
	// OIDCVerifier verifies id_token signatures + standard claims.
	OIDCVerifier OIDCVerifier
```

Create `internal/gateway/oidc_exchange.go`:

```go
package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
	"github.com/bucketvcs/bucketvcs/internal/oidc"
)

// OIDCVerifier is the subset of *oidc.Verifier the gateway depends on.
type OIDCVerifier interface {
	Verify(ctx context.Context, rawToken, issuer string) (map[string]any, error)
}

// OIDCExchangeStore is the store surface the exchange handler needs.
type OIDCExchangeStore interface {
	FindOIDCIssuerByURL(ctx context.Context, issuerURL string) (auth.OIDCIssuer, error)
	ListOIDCRulesForIssuer(ctx context.Context, alias string) ([]auth.OIDCTrustRule, error)
	MintOIDCToken(ctx context.Context, tenant, repo string, perm auth.Perm,
		scopes auth.TokenScope, ttlSeconds int64, label string) (string, error)
}

const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	subjectTokenJWT    = "urn:ietf:params:oauth:token-type:jwt"
	issuedTokenAccess  = "urn:ietf:params:oauth:token-type:access-token"
)

// handleOIDCExchange implements POST /_oidc/token (RFC 8693).
func (s *Server) handleOIDCExchange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := ClientIP(r, s.opts.TrustProxyHeaders)

	if r.Method != http.MethodPost {
		writeOIDCError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	// Rate-limit gate (failures only count below; this just blocks floods).
	if allowed, _, _ := s.opts.Limiter.CheckDetailed(ip, ""); !allowed {
		writeOIDCError(w, http.StatusTooManyRequests, "slow_down", "rate limited")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOIDCError(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	if r.PostForm.Get("grant_type") != grantTokenExchange {
		writeOIDCError(w, http.StatusBadRequest, "unsupported_grant_type", "")
		return
	}
	if st := r.PostForm.Get("subject_token_type"); st != "" && st != subjectTokenJWT {
		writeOIDCError(w, http.StatusBadRequest, "invalid_request", "unsupported subject_token_type")
		return
	}
	raw := r.PostForm.Get("subject_token")
	if raw == "" {
		writeOIDCError(w, http.StatusBadRequest, "invalid_request", "missing subject_token")
		return
	}

	// Peek iss from the unverified payload to find the registered issuer.
	iss, ok := unverifiedIssuer(raw)
	if !ok {
		writeOIDCError(w, http.StatusBadRequest, "invalid_request", "malformed token")
		return
	}
	issuer, err := s.opts.OIDCStore.FindOIDCIssuerByURL(ctx, iss)
	if err != nil {
		// Unknown issuer: uniform 400, NO JWKS fetch.
		auth.EmitOIDCRejected(ctx, s.logger, "unknown", ip, "unknown_issuer")
		emitOIDCMetric(ctx, s.logger, "bad_request")
		writeOIDCError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}

	claims, err := s.opts.OIDCVerifier.Verify(ctx, raw, issuer.IssuerURL)
	if err != nil {
		if errors.Is(err, oidc.ErrIssuerUnavailable) {
			// Issuer unreachable: retryable, NOT a credential failure (don't
			// punish the client for the IdP being down).
			auth.EmitOIDCRejected(ctx, s.logger, issuer.Alias, ip, "issuer_unavailable")
			emitOIDCMetric(ctx, s.logger, "issuer_unavailable")
			writeOIDCError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "issuer discovery/JWKS unreachable")
			return
		}
		s.opts.Limiter.MarkFailure(ip, "")
		ratelimit.EmitRateLimitMetric(ctx, s.logger, "failure_counted")
		auth.EmitOIDCRejected(ctx, s.logger, issuer.Alias, ip, "invalid_token")
		emitOIDCMetric(ctx, s.logger, "invalid_token")
		writeOIDCError(w, http.StatusUnauthorized, "invalid_token", "")
		return
	}

	rules, err := s.opts.OIDCStore.ListOIDCRulesForIssuer(ctx, issuer.Alias)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rule := auth.MatchRule(rules, claims)
	if rule == nil {
		auth.EmitOIDCRejected(ctx, s.logger, issuer.Alias, ip, "no_rule")
		emitOIDCMetric(ctx, s.logger, "no_rule")
		writeOIDCError(w, http.StatusForbidden, "access_denied", "")
		return
	}

	// Effective scopes: rule.Scopes, optionally narrowed by requested scope.
	effective := rule.Scopes
	if req := strings.TrimSpace(r.PostForm.Get("scope")); req != "" {
		want, perr := auth.ParseScopes(strings.ReplaceAll(req, " ", ","))
		if perr != nil {
			writeOIDCError(w, http.StatusBadRequest, "invalid_scope", "")
			return
		}
		effective = rule.Scopes & want
		if effective == 0 {
			emitOIDCMetric(ctx, s.logger, "invalid_scope")
			writeOIDCError(w, http.StatusBadRequest, "invalid_scope", "requested scope exceeds grant")
			return
		}
	}

	perm := auth.PermRead
	if effective.Has(auth.ScopeRepoWrite) {
		perm = auth.PermWrite
	}
	sub, _ := claims["sub"].(string)
	label := "oidc:" + issuer.Alias + ":" + sub
	token, err := s.opts.OIDCStore.MintOIDCToken(ctx, rule.Tenant, rule.Repo, perm,
		effective, rule.TTLSeconds, label)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	auth.EmitOIDCExchanged(ctx, s.logger, issuer.Alias, sub, rule.Tenant, rule.Repo, effective, rule.TTLSeconds)
	emitOIDCMetric(ctx, s.logger, "minted")
	s.opts.Limiter.MarkSuccess(ip, "")

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":      token,
		"issued_token_type": issuedTokenAccess,
		"token_type":        "Bearer",
		"expires_in":        rule.TTLSeconds,
		"scope":             strings.ReplaceAll(auth.FormatScopes(effective), ",", " "),
	})
}

// unverifiedIssuer extracts the "iss" claim from an unverified compact JWS.
// This is used ONLY to select the registered issuer before signature
// verification; nothing here is trusted.
func unverifiedIssuer(raw string) (string, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &c); err != nil || c.Iss == "" {
		return "", false
	}
	return c.Iss, true
}

func writeOIDCError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	_ = json.NewEncoder(w).Encode(body)
}
```

- [ ] **Step 2: Add the audit + metric emitters**

Append to `internal/auth/audit.go`:

```go
// EmitOIDCExchanged records a successful token exchange (M22).
func EmitOIDCExchanged(ctx context.Context, logger *slog.Logger,
	issuerAlias, sub, tenant, repo string, scopes TokenScope, ttlSec int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.oidc.exchanged",
		slog.String("issuer", issuerAlias),
		slog.String("sub", sub),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("scopes", FormatScopes(scopes)),
		slog.Int64("ttl_sec", ttlSec),
	)
}

// EmitOIDCRejected records a rejected exchange (M22). reason is an enum:
// unknown_issuer | invalid_token | no_rule.
func EmitOIDCRejected(ctx context.Context, logger *slog.Logger,
	issuerAlias, ip, reason string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "auth.oidc.rejected",
		slog.String("issuer", issuerAlias),
		slog.String("ip", ip),
		slog.String("reason", reason),
	)
}
```

Add a metric emitter to `internal/gateway/oidc_exchange.go`:

```go
// emitOIDCMetric logs an oidc_exchange_total{result} counter increment in the
// gateway's structured-metric shape (mirrors internal/lfs/metrics.go).
func emitOIDCMetric(ctx context.Context, logger *slog.Logger, result string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "oidc_exchange_total"),
		slog.Int64("value", 1),
		slog.String("result", result),
	)
}
```

- [ ] **Step 3: Mount the endpoint**

In `internal/gateway/server.go` `NewServer`, before `s.mux.HandleFunc("/", s.routeRoot)`:

```go
	if opts.OIDCEnabled {
		if opts.OIDCStore == nil || opts.OIDCVerifier == nil {
			return nil, fmt.Errorf("gateway: OIDCEnabled requires OIDCStore and OIDCVerifier")
		}
		s.mux.HandleFunc("/_oidc/token", s.handleOIDCExchange)
	}
```

- [ ] **Step 4: Write the handler test**

Create `internal/gateway/oidc_exchange_test.go`:

```go
package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// fakeVerifier returns preset claims for a matching token, else an error.
type fakeVerifier struct {
	claims map[string]any
	jwks   int // counts Verify calls (proxy for "fetch happened")
}

func (f *fakeVerifier) Verify(ctx context.Context, raw, issuer string) (map[string]any, error) {
	f.jwks++
	if raw == "bad" {
		return nil, auth.ErrInvalidCredential
	}
	return f.claims, nil
}

// fakeOIDCStore implements OIDCExchangeStore.
type fakeOIDCStore struct {
	issuers   map[string]auth.OIDCIssuer // keyed by url
	rules     []auth.OIDCTrustRule
	minted    int
	lastMint  MintRecord
}
type MintRecord struct {
	Tenant, Repo string
	Perm         auth.Perm
	Scopes       auth.TokenScope
}

func (s *fakeOIDCStore) FindOIDCIssuerByURL(ctx context.Context, u string) (auth.OIDCIssuer, error) {
	i, ok := s.issuers[u]
	if !ok {
		return auth.OIDCIssuer{}, auth.ErrNoSuchRepo // any not-found sentinel
	}
	return i, nil
}
func (s *fakeOIDCStore) ListOIDCRulesForIssuer(ctx context.Context, alias string) ([]auth.OIDCTrustRule, error) {
	return s.rules, nil
}
func (s *fakeOIDCStore) MintOIDCToken(ctx context.Context, tenant, repo string, perm auth.Perm,
	scopes auth.TokenScope, ttl int64, label string) (string, error) {
	s.minted++
	s.lastMint = MintRecord{tenant, repo, perm, scopes}
	return "bvts_minted", nil
}

// fakeJWT builds a compact token whose payload carries iss (signature is not
// checked by fakeVerifier).
func fakeJWT(iss string) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	pl := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + iss + `"}`))
	return hdr + "." + pl + ".sig"
}

func newOIDCTestServer(t *testing.T, store *fakeOIDCStore, ver *fakeVerifier) *Server {
	t.Helper()
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.opts = Options{OIDCEnabled: true, OIDCStore: store, OIDCVerifier: ver}
	return s
}

func TestOIDCExchange_HappyPath(t *testing.T) {
	iss := "https://i.example"
	store := &fakeOIDCStore{
		issuers: map[string]auth.OIDCIssuer{iss: {Alias: "gh", IssuerURL: iss}},
		rules: []auth.OIDCTrustRule{{
			ID: "r1", IssuerAlias: "gh", Audience: "aud", Tenant: "org", Repo: "app",
			Scopes: auth.ScopeRepoWrite, TTLSeconds: 900, Claims: map[string]string{"repository": "org/app"},
		}},
	}
	ver := &fakeVerifier{claims: map[string]any{"aud": "aud", "sub": "s", "repository": "org/app"}}
	s := newOIDCTestServer(t, store, ver)

	form := url.Values{
		"grant_type":    {grantTokenExchange},
		"subject_token": {fakeJWT(iss)},
	}
	r := httptest.NewRequest(http.MethodPost, "/_oidc/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleOIDCExchange(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["access_token"] != "bvts_minted" {
		t.Fatalf("token = %v", resp["access_token"])
	}
	if store.lastMint.Perm != auth.PermWrite || store.lastMint.Repo != "app" {
		t.Fatalf("mint = %+v", store.lastMint)
	}
}

func TestOIDCExchange_UnknownIssuer_NoVerify(t *testing.T) {
	store := &fakeOIDCStore{issuers: map[string]auth.OIDCIssuer{}}
	ver := &fakeVerifier{}
	s := newOIDCTestServer(t, store, ver)
	form := url.Values{"grant_type": {grantTokenExchange}, "subject_token": {fakeJWT("https://nope")}}
	r := httptest.NewRequest(http.MethodPost, "/_oidc/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleOIDCExchange(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
	if ver.jwks != 0 {
		t.Fatalf("verifier called %d times for unregistered issuer, want 0", ver.jwks)
	}
}

func TestOIDCExchange_NoRule_403(t *testing.T) {
	iss := "https://i.example"
	store := &fakeOIDCStore{
		issuers: map[string]auth.OIDCIssuer{iss: {Alias: "gh", IssuerURL: iss}},
		rules:   []auth.OIDCTrustRule{{ID: "r1", IssuerAlias: "gh", Audience: "other", Tenant: "org", Repo: "app"}},
	}
	ver := &fakeVerifier{claims: map[string]any{"aud": "aud", "sub": "s"}}
	s := newOIDCTestServer(t, store, ver)
	form := url.Values{"grant_type": {grantTokenExchange}, "subject_token": {fakeJWT(iss)}}
	r := httptest.NewRequest(http.MethodPost, "/_oidc/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleOIDCExchange(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d", w.Code)
	}
}
```

Note: confirm a `testLogger(t)` helper exists in the gateway tests (`grep -n "func testLogger" internal/gateway/*_test.go`). If not, inline `slog.New(slog.NewTextHandler(io.Discard, nil))` (as shown above). The `Server.opts`/`logger` fields are package-private and reachable from in-package tests.

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/gateway/ -run TestOIDCExchange`
Expected: PASS (3 tests). `ClientIP` and the nil-`Limiter` no-op behavior are already in the package.

- [ ] **Step 6: Run the full gateway suite**

Run: `go test ./internal/gateway/`
Expected: PASS (no regressions; the new mount is gated behind `OIDCEnabled`).

- [ ] **Step 7: Commit**

```bash
git add internal/gateway/oidc_exchange.go internal/gateway/oidc_exchange_test.go internal/gateway/server.go internal/auth/audit.go
git commit -m "M22: gateway /_oidc/token exchange handler + audit + metric"
```

---

## Task 10: serve.go wiring — flags, verifier, OIDC store, sweep goroutine

**Files:**
- Modify: `cmd/bucketvcs/serve.go`

- [ ] **Step 1: Add flags**

In `runServe`'s flag block (near the other feature flags), add:

```go
	oidcEnabled := fs.Bool("oidc", false,
		"Enable the OIDC token-exchange endpoint POST /_oidc/token (M22)")
	oidcSweepInterval := fs.Duration("oidc-sweep-interval", 5*time.Minute,
		"Interval for sweeping expired OIDC-minted tokens")
```

- [ ] **Step 2: Construct the verifier + wire Options**

After the authdb store (`authS`) is opened and before/around the `gateway.NewServer` call, add the verifier and Options fields. The OIDC store is the concrete `*sqlitestore.Store` (`authS`), which already satisfies `gateway.OIDCExchangeStore` via the methods from Tasks 6/8 — except the signatures must line up. Add a thin adapter if needed:

```go
	var oidcVerifier gateway.OIDCVerifier
	var oidcStore gateway.OIDCExchangeStore
	if *oidcEnabled {
		oidcVerifier = oidc.NewVerifier()
		oidcStore = oidcStoreAdapter{authS}
	}
```

Add the adapter type at the bottom of `serve.go` (translates the gateway's mint signature to the store's `MintOIDCParams`, and `Verify`'s `Claims` to `map[string]any`):

```go
// oidcStoreAdapter adapts *sqlitestore.Store to gateway.OIDCExchangeStore.
type oidcStoreAdapter struct{ s *sqlitestore.Store }

func (a oidcStoreAdapter) FindOIDCIssuerByURL(ctx context.Context, u string) (auth.OIDCIssuer, error) {
	return a.s.FindOIDCIssuerByURL(ctx, u)
}
func (a oidcStoreAdapter) ListOIDCRulesForIssuer(ctx context.Context, alias string) ([]auth.OIDCTrustRule, error) {
	return a.s.ListOIDCRulesForIssuer(ctx, alias)
}
func (a oidcStoreAdapter) MintOIDCToken(ctx context.Context, tenant, repo string, perm auth.Perm,
	scopes auth.TokenScope, ttl int64, label string) (string, error) {
	return a.s.MintOIDCToken(ctx, sqlitestore.MintOIDCParams{
		Tenant: tenant, Repo: repo, Perm: perm, Scopes: scopes, TTLSeconds: ttl, Label: label,
	})
}
```

Note: `oidc.NewVerifier()` returns `*oidc.Verifier` whose `Verify` returns `(oidc.Claims, error)`. Since `oidc.Claims` is `map[string]any`, it satisfies the `OIDCVerifier` interface method `Verify(...) (map[string]any, error)` ONLY if Go treats the named type as the interface's `map[string]any` — it does NOT (named type ≠ interface signature). So add a tiny verifier adapter too:

```go
type oidcVerifierAdapter struct{ v *oidc.Verifier }

func (a oidcVerifierAdapter) Verify(ctx context.Context, raw, issuer string) (map[string]any, error) {
	c, err := a.v.Verify(ctx, raw, issuer)
	return map[string]any(c), err
}
```

and set `oidcVerifier = oidcVerifierAdapter{oidc.NewVerifier()}`.

Add to the `gateway.Options{...}` literal:

```go
			OIDCEnabled:  *oidcEnabled,
			OIDCStore:    oidcStore,
			OIDCVerifier: oidcVerifier,
```

Add imports: `"github.com/bucketvcs/bucketvcs/internal/oidc"`, and ensure `auth`, `sqlitestore` are imported (sqlitestore likely already is).

- [ ] **Step 3: Start the sweep goroutine**

After the webhook worker `go webhooks.StartWorker(...)`:

```go
	if *oidcEnabled {
		go func() {
			t := time.NewTicker(*oidcSweepInterval)
			defer t.Stop()
			for {
				select {
				case <-serveCtx.Done():
					return
				case <-t.C:
					n, err := authS.SweepExpiredOIDCTokens(serveCtx)
					if err != nil {
						buildLogger.LogAttrs(serveCtx, slog.LevelWarn, "oidc sweep error", slog.String("err", err.Error()))
						continue
					}
					if n > 0 {
						buildLogger.LogAttrs(serveCtx, slog.LevelInfo, "metric",
							slog.String("metric_name", "oidc_tokens_swept_total"), slog.Int64("value", n))
					}
				}
			}
		}()
	}
```

Use whatever logger variable serve.go already has in scope (grep for the logger passed to `gateway.Options.Logger` / `webhooks.StartWorker`; substitute its name for `buildLogger`).

- [ ] **Step 4: Build + run serve tests**

Run: `go build ./... && go test ./cmd/bucketvcs/ -run TestServe`
Expected: builds; existing serve tests pass. (If serve_test has a smoke that starts the server, `--oidc` defaults false so behavior is unchanged.)

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/serve.go
git commit -m "M22: serve --oidc wiring — verifier, store adapter, expiry sweep goroutine"
```

---

## Task 11: CLI — `bucketvcs oidc issuer|rule`

**Files:**
- Create: `cmd/bucketvcs/oidc.go`, `cmd/bucketvcs/oidc_test.go`
- Modify: `cmd/bucketvcs/main.go`

- [ ] **Step 1: Register the subcommand**

In `cmd/bucketvcs/main.go` `run`'s switch, add:

```go
	case "oidc":
		return runOIDC(ctx, rest, stdout, stderr)
```

And add to the `usage` text:

```
  oidc               Manage OIDC issuers + trust rules (issuer/rule add/list/remove)
```

- [ ] **Step 2: Implement the CLI**

Create `cmd/bucketvcs/oidc.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

const oidcUsage = `Usage: bucketvcs oidc <object> <action> [flags]

Objects + actions:
  issuer add    --auth-db=<path> --alias=<name> --url=<issuer-url>
  issuer list   --auth-db=<path> [--format=text|json]
  issuer remove --auth-db=<path> --alias=<name>
  rule add      --auth-db=<path> --issuer=<alias> --audience=<aud>
                --tenant=<t> --repo=<r> --scopes=<csv> --ttl=<dur>
                [--claim name=value ...]
  rule list     --auth-db=<path> [--issuer=<alias> | --repo=<t>/<r>] [--format=text|json]
  rule remove   --auth-db=<path> --id=<bvor_...>

Exit codes: 0 ok | 1 operational | 2 usage.
TTL is a Go duration (e.g. 15m). Maximum 1h. --audience is required.
Claim matching is exact string equality; repeat --claim for multiple
constraints; omit --claim entirely for an issuer-wide (wildcard) rule.`

const oidcTTLCeiling = time.Hour

func runOIDC(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, oidcUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "issuer":
		return runOIDCIssuer(ctx, args[1:], stdout, stderr)
	case "rule":
		return runOIDCRule(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "oidc: unknown object %q\n%s", args[0], oidcUsage)
		return 2
	}
}

func openOIDCStore(authDB string) (*sqlitestore.Store, error) {
	if authDB == "" {
		return nil, fmt.Errorf("--auth-db required")
	}
	return sqlitestore.Open(authDB)
}

func runOIDCIssuer(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "oidc issuer: action required (add|list|remove)")
		return 2
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("oidc issuer add", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		alias := fs.String("alias", "", "")
		urlF := fs.String("url", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *authDB == "" || *alias == "" || *urlF == "" {
			fmt.Fprintln(stderr, "oidc issuer add: --auth-db, --alias, --url required")
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc issuer add: %v\n", err)
			return 1
		}
		defer st.Close()
		if err := st.AddOIDCIssuer(ctx, *alias, *urlF); err != nil {
			fmt.Fprintf(stderr, "oidc issuer add: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "alias=%s  url=%s\n", *alias, *urlF)
		return 0
	case "list":
		fs := flag.NewFlagSet("oidc issuer list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		format := fs.String("format", "text", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc issuer list: %v\n", err)
			return 1
		}
		defer st.Close()
		issuers, err := st.ListOIDCIssuers(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "oidc issuer list: %v\n", err)
			return 1
		}
		for _, i := range issuers {
			if *format == "json" {
				b, _ := json.Marshal(map[string]any{"alias": i.Alias, "url": i.IssuerURL, "created_at": i.CreatedAt})
				fmt.Fprintln(stdout, string(b))
			} else {
				fmt.Fprintf(stdout, "alias=%s  url=%s\n", i.Alias, i.IssuerURL)
			}
		}
		return 0
	case "remove":
		fs := flag.NewFlagSet("oidc issuer remove", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		alias := fs.String("alias", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *authDB == "" || *alias == "" {
			fmt.Fprintln(stderr, "oidc issuer remove: --auth-db, --alias required")
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc issuer remove: %v\n", err)
			return 1
		}
		defer st.Close()
		if err := st.RemoveOIDCIssuer(ctx, *alias); err != nil {
			fmt.Fprintf(stderr, "oidc issuer remove: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed alias=%s\n", *alias)
		return 0
	default:
		fmt.Fprintf(stderr, "oidc issuer: unknown action %q\n", args[0])
		return 2
	}
}

// claimFlags collects repeated --claim name=value pairs.
type claimFlags map[string]string

func (c claimFlags) String() string { return "" }
func (c claimFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("claim must be name=value")
	}
	c[k] = val
	return nil
}

func runOIDCRule(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "oidc rule: action required (add|list|remove)")
		return 2
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("oidc rule add", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		issuer := fs.String("issuer", "", "")
		audience := fs.String("audience", "", "")
		tenant := fs.String("tenant", "", "")
		repo := fs.String("repo", "", "")
		scopesF := fs.String("scopes", "", "")
		ttl := fs.Duration("ttl", 15*time.Minute, "")
		claims := claimFlags{}
		fs.Var(claims, "claim", "repeatable name=value claim constraint")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *authDB == "" || *issuer == "" || *audience == "" || *tenant == "" || *repo == "" || *scopesF == "" {
			fmt.Fprintln(stderr, "oidc rule add: --auth-db, --issuer, --audience, --tenant, --repo, --scopes required")
			return 2
		}
		if *ttl <= 0 || *ttl > oidcTTLCeiling {
			fmt.Fprintf(stderr, "oidc rule add: --ttl must be > 0 and <= %s\n", oidcTTLCeiling)
			return 2
		}
		scopes, err := auth.ParseScopes(*scopesF)
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule add: %v\n", err)
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule add: %v\n", err)
			return 1
		}
		defer st.Close()
		id, err := st.AddOIDCRule(ctx, auth.OIDCTrustRule{
			IssuerAlias: *issuer, Audience: *audience, Tenant: *tenant, Repo: *repo,
			Scopes: scopes, TTLSeconds: int64(ttl.Seconds()), Claims: map[string]string(claims),
		})
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule add: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "id=%s  issuer=%s  tenant=%s  repo=%s  scopes=%s  ttl=%s  claims=%d\n",
			id, *issuer, *tenant, *repo, auth.FormatScopes(scopes), *ttl, len(claims))
		return 0
	case "list":
		fs := flag.NewFlagSet("oidc rule list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		issuer := fs.String("issuer", "", "")
		repoF := fs.String("repo", "", "tenant/repo")
		format := fs.String("format", "text", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule list: %v\n", err)
			return 1
		}
		defer st.Close()
		var rules []auth.OIDCTrustRule
		switch {
		case *repoF != "":
			tn, rp, ok := strings.Cut(*repoF, "/")
			if !ok {
				fmt.Fprintln(stderr, "oidc rule list: --repo must be tenant/repo")
				return 2
			}
			rules, err = st.ListOIDCRulesForRepo(ctx, tn, rp)
		case *issuer != "":
			rules, err = st.ListOIDCRulesForIssuer(ctx, *issuer)
		default:
			fmt.Fprintln(stderr, "oidc rule list: --issuer or --repo required")
			return 2
		}
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule list: %v\n", err)
			return 1
		}
		for _, r := range rules {
			if *format == "json" {
				b, _ := json.Marshal(map[string]any{
					"id": r.ID, "issuer": r.IssuerAlias, "audience": r.Audience,
					"tenant": r.Tenant, "repo": r.Repo, "scopes": auth.FormatScopes(r.Scopes),
					"ttl_seconds": r.TTLSeconds, "claims": r.Claims, "wildcard": len(r.Claims) == 0,
				})
				fmt.Fprintln(stdout, string(b))
			} else {
				wc := ""
				if len(r.Claims) == 0 {
					wc = "  [WILDCARD: matches any token from issuer]"
				}
				fmt.Fprintf(stdout, "id=%s  issuer=%s  aud=%s  %s/%s  scopes=%s  ttl=%ds  claims=%d%s\n",
					r.ID, r.IssuerAlias, r.Audience, r.Tenant, r.Repo,
					auth.FormatScopes(r.Scopes), r.TTLSeconds, len(r.Claims), wc)
			}
		}
		return 0
	case "remove":
		fs := flag.NewFlagSet("oidc rule remove", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		id := fs.String("id", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *authDB == "" || *id == "" {
			fmt.Fprintln(stderr, "oidc rule remove: --auth-db, --id required")
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule remove: %v\n", err)
			return 1
		}
		defer st.Close()
		if err := st.RemoveOIDCRule(ctx, *id); err != nil {
			fmt.Fprintf(stderr, "oidc rule remove: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed id=%s\n", *id)
		return 0
	default:
		fmt.Fprintf(stderr, "oidc rule: unknown action %q\n", args[0])
		return 2
	}
}
```

Note: confirm `sqlitestore.Open(path)` is the constructor used elsewhere (`grep -n "sqlitestore.Open" cmd/bucketvcs/*.go`); match its exact signature (it may take options/ctx).

- [ ] **Step 3: Write a CLI test**

Create `cmd/bucketvcs/oidc_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOIDCCLI_IssuerAndRuleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "auth.db")
	ctx := context.Background()

	// Seed a repo via the existing repo CLI so the rule FK is satisfiable.
	var rout, rerr bytes.Buffer
	if rc := runRepo(ctx, []string{"register", "org/app", "--no-init", "--auth-db", db}, &rout, &rerr); rc != 0 {
		t.Fatalf("repo register: rc=%d err=%s", rc, rerr.String())
	}

	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runOIDC(ctx, args, &out, &errb)
		return code, out.String(), errb.String()
	}

	if code, _, e := run("issuer", "add", "--auth-db", db, "--alias", "gh", "--url", "https://i.example"); code != 0 {
		t.Fatalf("issuer add: code=%d err=%s", code, e)
	}
	code, out, _ := run("issuer", "list", "--auth-db", db, "--format", "json")
	if code != 0 || !strings.Contains(out, `"alias":"gh"`) {
		t.Fatalf("issuer list: code=%d out=%s", code, out)
	}
	if code, _, e := run("rule", "add", "--auth-db", db, "--issuer", "gh",
		"--audience", "aud", "--tenant", "org", "--repo", "app",
		"--scopes", "repo:write", "--ttl", "15m", "--claim", "repository=org/app"); code != 0 {
		t.Fatalf("rule add: code=%d err=%s", code, e)
	}
	code, out, _ = run("rule", "list", "--auth-db", db, "--issuer", "gh")
	if code != 0 || !strings.Contains(out, "repo=app") {
		t.Fatalf("rule list: code=%d out=%s", code, out)
	}
	// bad ttl rejected
	if code, _, _ := run("rule", "add", "--auth-db", db, "--issuer", "gh",
		"--audience", "aud", "--tenant", "org", "--repo", "app",
		"--scopes", "repo:write", "--ttl", "5h"); code != 2 {
		t.Fatalf("over-ceiling ttl: want exit 2, got %d", code)
	}
}
```

Confirm `runRepo`'s `register` accepts `tenant/repo --no-init --auth-db <path>` (it does today, per `cmd/bucketvcs/repocmd_test.go`).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/bucketvcs/ -run TestOIDCCLI`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/oidc.go cmd/bucketvcs/oidc_test.go cmd/bucketvcs/main.go
git commit -m "M22: bucketvcs oidc issuer|rule CLI (NDJSON, ttl ceiling, wildcard flagging)"
```

---

## Task 12: End-to-end smoke (localfs)

**Files:**
- Create: `scripts/smoke-oidc.sh`

- [ ] **Step 1: Write the smoke script**

Create `scripts/smoke-oidc.sh`. It (1) builds the binary, (2) starts a tiny local issuer HTTP server (a small Go helper compiled inline or a Python script) that serves discovery + JWKS and can sign a JWT, (3) registers issuer + rule, (4) starts `bucketvcs serve --oidc`, (5) exchanges a signed JWT for a token, (6) pushes to the bound repo (success), (7) attempts a push to a different repo (expect failure), (8) waits for an expired token to be rejected.

Model the structure on an existing smoke (`grep -l 'bucketvcs serve' scripts/*.sh`, e.g. the LFS smoke `scripts/smoke-lfs*.sh`). Reuse its repo-init, user/token seeding, and `serve` bring-up/teardown scaffolding. The OIDC-specific additions:

```bash
#!/usr/bin/env bash
# scripts/smoke-oidc.sh — M22 OIDC token-exchange end-to-end (localfs).
set -euo pipefail

# --- helper: a local IdP that serves discovery + JWKS and signs id_tokens ---
# Write a small Go program (in $TMP) that: (a) generates an RSA key at startup,
# (b) serves /.well-known/openid-configuration + /jwks from that key's public
# half, (c) has a sign mode printing a signed RS256 JWT for given iss/aud/sub/
# claims. The same key signs and is published, so the gateway's JWKS fetch
# validates it. Use internal/oidc/testkeys_test.go's go-jose signing as the
# reference for the signer (publicJWKS + signToken).

# 1. build
go build -o "$TMP/bucketvcs" ./cmd/bucketvcs

# 2. start local IdP stub on $IDP_ADDR, capture $ISS=http://$IDP_ADDR
# 3. init repo + register repo in authdb
"$TMP/bucketvcs" oidc issuer add --auth-db "$DB" --alias gh --url "$ISS"
"$TMP/bucketvcs" oidc rule add --auth-db "$DB" --issuer gh --audience "$AUD" \
  --tenant org --repo app --scopes repo:write --ttl 15m --claim repository=org/app

# 4. start serve --oidc on $GW_ADDR
"$TMP/bucketvcs" serve --store "localfs:$STORE" --auth-db "$DB" \
  --addr "$GW_ADDR" --oidc --oidc-sweep-interval 2s &
GW_PID=$!; trap 'kill $GW_PID $IDP_PID 2>/dev/null || true' EXIT

# 5. mint id_token via the stub, exchange it
JWT="$(idp_sign iss="$ISS" aud="$AUD" sub="repo:org/app" repository=org/app)"
RESP="$(curl -sS -X POST "http://$GW_ADDR/_oidc/token" \
  -d grant_type=urn:ietf:params:oauth:grant-type:token-exchange \
  -d subject_token="$JWT")"
TOKEN="$(echo "$RESP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')"
[ -n "$TOKEN" ] || { echo "FAIL: no access_token"; exit 1; }

# 6. push to bound repo succeeds
git -C "$WORK" push "http://_oidc:$TOKEN@$GW_ADDR/org/app" HEAD:refs/heads/main
echo "PASS: push to bound repo succeeded"

# 7. push to a different repo fails (403)
if git -C "$WORK" push "http://_oidc:$TOKEN@$GW_ADDR/org/other" HEAD:refs/heads/main 2>/dev/null; then
  echo "FAIL: cross-repo push should have been denied"; exit 1
fi
echo "PASS: cross-repo push denied"

echo "SMOKE OK"
```

(Flesh out `$TMP/$DB/$STORE/$WORK/$AUD/$GW_ADDR/$IDP_ADDR` setup and the `idp_sign`/stub exactly as the LFS smoke sets up its environment. The stub's signing key must be the one published at the JWKS endpoint.)

- [ ] **Step 2: Run the smoke**

Run: `bash scripts/smoke-oidc.sh`
Expected: prints `PASS: push to bound repo succeeded`, `PASS: cross-repo push denied`, `SMOKE OK`; exit 0.

- [ ] **Step 3: Commit**

```bash
git add scripts/smoke-oidc.sh
git commit -m "M22: localfs end-to-end OIDC exchange smoke (bound push ok, cross-repo denied)"
```

---

## Task 13: Operator guide

**Files:**
- Create: `docs/m22-oidc-operator-guide.md`

- [ ] **Step 1: Write the guide**

Create `docs/m22-oidc-operator-guide.md` covering, in the style of `docs/m15-webhooks-operator-guide.md`:
- **What it is:** RFC 8693 exchange of a CI id_token for a short-lived repo-scoped token; no UI.
- **Enabling:** `--oidc`, `--oidc-sweep-interval`. Note the endpoint is `POST /_oidc/token`, unauthenticated at the Basic layer (the JWT is the credential).
- **Registering trust:** `oidc issuer add`, `oidc rule add` worked examples for GitHub Actions (`--url=https://token.actions.githubusercontent.com`, claim `repository`, `ref`) and GitLab.
- **Using from CI:** GitHub Actions snippet fetching `$ACTIONS_ID_TOKEN_REQUEST_*`, curling `/_oidc/token`, plugging the result into `git clone https://_oidc:$TOKEN@...`. Note the username slot is ignored for OIDC tokens.
- **Security guidance (prominent):** the operator trusts the IdP to assert claims; **always set `--audience`**; a rule with no `--claim` is a wildcard for that issuer — flagged in `rule list`; keep `--ttl` short.
- **Limits / unsupported (v1):** array-valued `aud` not matched; no `jti` replay cache; exact claim matching only (no globs); human SSO out of scope.
- **Observability:** `auth.oidc.exchanged` / `auth.oidc.rejected` audit events; `oidc_exchange_total{result}`, `oidc_tokens_swept_total` metrics.
- **Troubleshooting:** `401 invalid_token` (clock skew / wrong key / expired), `403 access_denied` (no matching rule — check `aud` and claims), `400 invalid_request` (unregistered issuer or malformed token), `503` (IdP discovery/JWKS unreachable).

- [ ] **Step 2: Commit**

```bash
git add docs/m22-oidc-operator-guide.md
git commit -m "M22: OIDC token-exchange operator guide"
```

---

## Final verification

- [ ] **Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: all PASS.

- [ ] **Run the smoke once more**

Run: `bash scripts/smoke-oidc.sh`
Expected: `SMOKE OK`.

- [ ] **Update memory index**

Add a one-line `MEMORY.md` pointer and an `m22_progress.md` topic file summarizing what shipped, mirroring prior milestone entries. (MEMORY.md is already near its size cap — keep the index line short, put detail in the topic file.)

---

## Self-review notes (for the implementer)

- **Spec coverage:** discovery/JWKS/verify (Tasks 1–3) ↔ spec §2.1; schema (Task 4) ↔ §2.2; rule matching (Task 5) ↔ §2.2; CRUD (Task 6) ↔ §7; token binding + verify (Task 7) ↔ §2.3/§3.1; mint+sweep (Task 8) ↔ §2.3/§2.5; exchange flow + security gates (Task 9) ↔ §3/§4; metrics+audit (Task 9) ↔ §5; serve flags (Task 10) ↔ §3/§2.5; CLI (Task 11) ↔ §7; smoke (Task 12) ↔ §8.5; guide (Task 13) ↔ §4.6/§1.2.
- **The `aud`-is-validated-by-caller split:** `oidc.Verify` checks iss/exp/nbf/iat but NOT aud; `MatchRule` enforces aud exact-equality. This is intentional (a token may legitimately carry an aud for a specific rule). Both halves are tested.
- **Down-scope semantics:** `effective = rule.Scopes & requested`. The spec says intersect; `&` is correct. Empty intersection → 400.
- **The deploy-key short-circuit reuse:** `verifyBasicPassword` returning `*auth.Scope` makes the existing `auth.go` middleware do the cross-repo 403 and `perm = scope.Perm` with no middleware change; `CheckScope` at the 9 M17 sites reads `actor.Scopes` which is populated from the token row. Verified against `internal/gateway/auth.go` as it stands today.
- **Known follow-on (documented, not built):** array `aud`, `jti` replay cache, glob claim matching, provider presets, per-issuer key pinning.
