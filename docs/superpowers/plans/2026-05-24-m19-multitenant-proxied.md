# M19 Multi-Tenant Proxied Bundle/Pack Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make bundle-uri/packfile-uri proxied mode multi-tenant-aware (URL embeds `<tenant>/<repo>/<hash>`, token binds to the composite), drop the unused `ProxiedKeyResolver` indirection, and actually wire the inbound `/_bundle/`,`/_pack/` mount in `cmd/bucketvcs/serve.go` (the central M11 bug — proxied mode mints URLs the gateway 404s on today).

**Architecture:** Mirror M13 LFS pattern. The URL builder produces `/_bundle/<t>/<r>/<h>` and `/_pack/<t>/<r>/<h>`; the token's `hash` field encodes `<t>+"/"+<r>+"/"+<h>` so any path-segment tamper fails HMAC verify. The proxied handler validates names with `routenames.ValidateName`, then computes the storage key directly via `keys.BundleKey(t, r, h)` / `keys.CanonicalPackKey(t, r, h)`. The `BundleURIBuildURL` / `PackURIBuildURL` closure type gains `tenant` and `repo` parameters; service.go passes `req.Tenant`, `req.Repo` through.

**Tech Stack:** Go 1.21+ stdlib (net/http, strings, log/slog, errors), existing `internal/proxiedurl`, existing `internal/repo/keys`, existing `internal/gateway/routenames`. No new dependencies. No DB schema changes.

---

## File structure

**Modified:**
- `internal/proxiedurl/token.go` — doc comment on Mint/Verify clarifies `hash` field can be a composite "<tenant>/<repo>/<hash>" string for kinds 1+2 (no signature change)
- `internal/proxiedurl/token_test.go` — add test asserting slash-bearing hash round-trips
- `internal/gateway/proxied_url_builder.go` — `BuildBundleURL`/`BuildPackURL`/`buildURL` signatures gain `tenant, repo`; URL path becomes `/_bundle/<t>/<r>/<h>`; token hash field is composite
- `internal/gateway/proxied_url_builder_test.go` — update existing tests; add cross-tenant tamper test
- `internal/gateway/proxied_routes.go` — `proxiedHandler.ServeHTTP` parses 3-segment path; `ProxiedKeyResolver` interface + field deleted; handler holds an `ObjectStore` and computes keys directly via `keys.BundleKey`/`keys.CanonicalPackKey`
- `internal/gateway/proxied_routes_test.go` — adjust existing tests; add tampering tests
- `internal/gateway/server.go` — `Options.ProxiedKeyResolver` field deleted; `NewProxiedHandler` call drops resolver arg
- `internal/gateway/observability.go` (and any sites that emit `bundle_uri_served_*` / `pack_uri_served_*` / `proxied.url.served`) — gain `tenant`, `repo` labels/attrs
- `internal/gitproto/uploadpack/engine.go` — `BundleURIBuildURL` / `PackURIBuildURL` closure type signatures gain `tenant, repo`
- `internal/gitproto/uploadpack/service.go` — call sites pass `req.Tenant`, `req.Repo` to the closure
- `internal/gitproto/uploadpack/engine_test.go` — update all closure literals (~12 occurrences) to new signature
- `internal/sshd/server.go` — same closure type update; `Options.BundleURIBuildURL` / `PackURIBuildURL` signatures gain `tenant, repo`
- `cmd/bucketvcs/serve.go` — closures pass tenant/repo through; `gateway.Options.ProxiedURLSigningKey` is populated; the NOTE comment at L227–L234 is deleted
- `docs/m11-bundles-operator-guide.md` — retract single-repo caveats at L20, L277, L408, L488, L909; document new URL shape

**Created:**
- `scripts/m19-multitenant-proxied-smoke.sh` — two-tenant end-to-end smoke

**Deleted:**
- `ProxiedKeyResolver` interface (defined in `internal/gateway/proxied_routes.go:25-33`) and any test mocks (in `proxied_routes_test.go`)

---

## Task 0: Survey and confirm assumptions

**Files:** read-only.

This task is a guard against assumptions in the plan drifting from current code. Read the four files below; verify the listed facts.

- [ ] **Step 1: Re-read spec**

```bash
cat /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/docs/superpowers/specs/2026-05-24-m19-multitenant-proxied-design.md
```

- [ ] **Step 2: Read key files and verify facts**

```bash
cat /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gateway/proxied_routes.go
cat /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gateway/proxied_url_builder.go
sed -n '210,260p' /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/cmd/bucketvcs/serve.go
grep -nE "BundleURIBuildURL|PackURIBuildURL" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gitproto/uploadpack/engine.go /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/sshd/server.go
```

Verify these facts (each should be true; if any is false, STOP and report — the plan needs revision):
1. `BundleURIBuildURL` closure type today is `func(ctx context.Context, hash, storageKey, expectedHash string) (string, error)` — no tenant/repo
2. `proxiedHandler.ServeHTTP` (proxied_routes.go around line 69) parses `r.URL.Path` by trimming `bundlePrefix`/`packPrefix`; what remains is the bare hash
3. `cmd/bucketvcs/serve.go` builds `gateway.Options{}` WITHOUT setting `ProxiedURLSigningKey` (the NOTE at line 227-234 documents this)
4. `internal/repo/keys` package exports `BundleKey(tenant, repo, hash) string` and `CanonicalPackKey(tenant, repo, hash) string` (or equivalents that produce tenant-scoped storage keys)
5. `internal/gateway/routenames` exports `ValidateName(s) error`
6. Existing audit events `bundle.uri.served` and `pack.uri.served` are emitted in `internal/gateway/proxied_routes.go` (via `emitProxiedURLServed`)

If keys package function names differ from `BundleKey`/`CanonicalPackKey`, note the actual names — Task 4 will use them.

- [ ] **Step 3: Confirm existing audit/metric emission sites**

```bash
grep -nE "bundle_uri_served|pack_uri_served|proxied.url" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gateway/*.go
```

List the file:line of each metric/audit call site. Task 6 modifies them.

- [ ] **Step 4: Confirm M19 has its own worktree branch**

```bash
cd /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied && git rev-parse --abbrev-ref HEAD
```

Expected: `worktree-m19-multitenant-proxied`. If not, STOP — the implementation must run inside the worktree.

- [ ] **Step 5: Commit nothing (survey-only task)**

Survey is read-only; no commit. Report findings in the agent response.

---

## Task 1: Token round-trip test for composite hash

**Files:**
- Modify: `internal/proxiedurl/token.go` (doc comment only)
- Test: `internal/proxiedurl/token_test.go`

The `proxiedurl.Mint` / `Verify` functions accept `hash` as an opaque string and embed it byte-for-byte in the payload. They already work for "tenant/repo/hash" strings — but no existing test pins that. Add one before changing the URL builder.

- [ ] **Step 1: Find the right test file and confirm Mint/Verify signatures**

```bash
ls /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/proxiedurl/
grep -nE "^func (Mint|Verify)" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/proxiedurl/token.go
```

Expected: `Mint(key []byte, kind string, hash string, exp time.Time) (string, error)` and `Verify(key []byte, tok, kind, hash string, now time.Time) (..., error)`. If the signature differs, adapt the test below to the actual one.

- [ ] **Step 2: Write the failing test**

Append to `internal/proxiedurl/token_test.go`:

```go
// TestMintVerify_CompositeHashWithSlashes pins that the hash field round-trips
// when it carries embedded slashes — the format M19 uses for kind=1/2
// bundle/pack tokens, where hash = "<tenant>/<repo>/<sha>". The mint+verify
// pair must be byte-exact on the composite string; any difference (extra
// slash, different tenant, swapped order) must fail Verify.
func TestMintVerify_CompositeHashWithSlashes(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	exp := time.Now().Add(time.Hour)
	for _, kind := range []string{"bundle", "pack"} {
		composite := "acme/site/sha256-" + strings.Repeat("ab", 32)
		tok, err := proxiedurl.Mint(key, kind, composite, exp)
		if err != nil {
			t.Fatalf("mint %s: %v", kind, err)
		}
		if _, err := proxiedurl.Verify(key, tok, kind, composite, time.Now()); err != nil {
			t.Errorf("verify %s with same composite: %v", kind, err)
		}
		// Tampered tenant
		if _, err := proxiedurl.Verify(key, tok, kind, "other/site/sha256-"+strings.Repeat("ab", 32), time.Now()); err == nil {
			t.Errorf("verify %s with swapped tenant: expected error, got nil", kind)
		}
		// Tampered repo
		if _, err := proxiedurl.Verify(key, tok, kind, "acme/elsewhere/sha256-"+strings.Repeat("ab", 32), time.Now()); err == nil {
			t.Errorf("verify %s with swapped repo: expected error, got nil", kind)
		}
		// Tampered hash
		if _, err := proxiedurl.Verify(key, tok, kind, "acme/site/sha256-"+strings.Repeat("cd", 32), time.Now()); err == nil {
			t.Errorf("verify %s with swapped hash: expected error, got nil", kind)
		}
	}
}
```

Ensure the imports include `"bytes"`, `"strings"`, `"testing"`, `"time"`, `proxiedurl` if not already.

- [ ] **Step 3: Run test to verify it passes**

```bash
go test ./internal/proxiedurl/... -run TestMintVerify_CompositeHashWithSlashes -count=1 -v
```

Expected: PASS. The test passes immediately because Mint/Verify already treat `hash` as opaque bytes; this test just pins that behavior so a future refactor can't break it.

- [ ] **Step 4: Update Mint doc comment**

Edit `internal/proxiedurl/token.go`. Find the Mint doc comment (above `func Mint`). Append after the existing description, BEFORE the function signature:

```
// For kinds 1 (bundle) and 2 (pack), M19 callers use a composite
// hash string "<tenant>/<repo>/<hash>"; the payload contains the
// composite byte-for-byte and Verify compares with constant-time
// equality, so any tenant/repo/hash swap fails verification.
```

Same addition for the `Verify` doc comment.

- [ ] **Step 5: Run all proxiedurl tests**

```bash
go test ./internal/proxiedurl/... -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/proxiedurl/
git commit -m "proxiedurl: pin composite hash round-trip + doc update (M19 Task 1)"
```

---

## Task 2: Extend URLBuilder.buildURL to embed (tenant, repo)

**Files:**
- Modify: `internal/gateway/proxied_url_builder.go`
- Test: `internal/gateway/proxied_url_builder_test.go`

Change the URLBuilder signatures from `(ctx, hash, storageKey, expectedHash)` to `(ctx, tenant, repo, hash, storageKey, expectedHash)`. The minted URL path becomes `/_bundle/<tenant>/<repo>/<hash>`; the proxied-token hash field becomes `tenant + "/" + repo + "/" + hash`.

- [ ] **Step 1: Read the current builder**

```bash
cat /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gateway/proxied_url_builder.go
```

- [ ] **Step 2: Write failing tests in proxied_url_builder_test.go**

The existing tests pass `BuildBundleURL(ctx, hash, key, expected)` (4 args). The new signature is `BuildBundleURL(ctx, tenant, repo, hash, key, expected)` (6 args). Append these tests at the end of `internal/gateway/proxied_url_builder_test.go`:

```go
// TestBuildBundleURL_MultiTenantURLShape pins that proxied URLs embed
// (tenant, repo) as the first two path segments after /_bundle/ — the
// shape introduced in M19 that mirrors /_lfs/<t>/<r>/<oid> from M13.
func TestBuildBundleURL_MultiTenantURLShape(t *testing.T) {
	st := &stubStoreProxiedOnly{}
	b := &URLBuilder{
		Store:          st,
		ProxiedKey:     bytes.Repeat([]byte{0x11}, 32),
		ProxiedBaseURL: "https://gw.example.com",
		BundleTTL:      time.Hour,
		Mode:           URIModeProxied,
	}
	hash := "sha256-" + strings.Repeat("a", 64)
	got, via, err := b.BuildBundleURL(context.Background(), "acme", "site", hash, "irrelevant-key", "")
	if err != nil {
		t.Fatalf("BuildBundleURL: %v", err)
	}
	if via != "proxied" {
		t.Errorf("via=%q, want proxied", via)
	}
	wantPrefix := "https://gw.example.com/_bundle/acme/site/" + hash + "?token="
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("URL=%q, want prefix %q", got, wantPrefix)
	}
}

// TestBuildPackURL_MultiTenantURLShape mirrors the above for packs.
func TestBuildPackURL_MultiTenantURLShape(t *testing.T) {
	st := &stubStoreProxiedOnly{}
	b := &URLBuilder{
		Store:          st,
		ProxiedKey:     bytes.Repeat([]byte{0x22}, 32),
		ProxiedBaseURL: "https://gw.example.com",
		PackTTL:        time.Hour,
		Mode:           URIModeProxied,
	}
	hash := strings.Repeat("c", 40)
	got, via, err := b.BuildPackURL(context.Background(), "acme", "site", hash, "irrelevant-key", "")
	if err != nil {
		t.Fatalf("BuildPackURL: %v", err)
	}
	if via != "proxied" {
		t.Errorf("via=%q, want proxied", via)
	}
	wantPrefix := "https://gw.example.com/_pack/acme/site/" + hash + "?token="
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("URL=%q, want prefix %q", got, wantPrefix)
	}
}

// TestBuildBundleURL_TokenBindsTenantRepoHash mints a URL, extracts the token,
// and verifies it ONLY round-trips against the same composite "<t>/<r>/<h>".
func TestBuildBundleURL_TokenBindsTenantRepoHash(t *testing.T) {
	st := &stubStoreProxiedOnly{}
	key := bytes.Repeat([]byte{0x33}, 32)
	b := &URLBuilder{
		Store:          st,
		ProxiedKey:     key,
		ProxiedBaseURL: "https://gw.example.com",
		BundleTTL:      time.Hour,
		Mode:           URIModeProxied,
	}
	hash := "sha256-" + strings.Repeat("e", 64)
	u, _, err := b.BuildBundleURL(context.Background(), "acme", "site", hash, "k", "")
	if err != nil {
		t.Fatalf("BuildBundleURL: %v", err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	tok := parsed.Query().Get("token")
	if tok == "" {
		t.Fatalf("no token in %q", u)
	}
	composite := "acme/site/" + hash
	if _, err := proxiedurl.Verify(key, tok, "bundle", composite, time.Now()); err != nil {
		t.Errorf("verify with correct composite: %v", err)
	}
	if _, err := proxiedurl.Verify(key, tok, "bundle", "other/site/"+hash, time.Now()); err == nil {
		t.Errorf("verify with swapped tenant: expected error, got nil")
	}
}
```

You will need to add imports if not already present: `"bytes"`, `"context"`, `"net/url"`, `"strings"`, `"testing"`, `"time"`, and `"github.com/bucketvcs/bucketvcs/internal/proxiedurl"`.

The existing `stubStoreProxiedOnly` is defined elsewhere in this test file or its siblings; if not, grep for it:

```bash
grep -n "stubStoreProxiedOnly" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gateway/*.go
```

If absent, define a minimal stub at the top of the test file:

```go
type stubStoreProxiedOnly struct{ storage.ObjectStore }

func (s *stubStoreProxiedOnly) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, http.Header, error) {
	return "", nil, storage.ErrNotSupported
}
```

(import `"net/http"` and `"github.com/bucketvcs/bucketvcs/internal/storage"` as needed.)

- [ ] **Step 3: Run failing tests**

```bash
go test ./internal/gateway/... -run "TestBuildBundleURL_MultiTenant|TestBuildPackURL_MultiTenant|TestBuildBundleURL_TokenBindsTenantRepoHash" -count=1 -v 2>&1 | tail -30
```

Expected: FAIL with compile error "too many arguments in call to BuildBundleURL".

- [ ] **Step 4: Update `buildURL` signature**

Edit `internal/gateway/proxied_url_builder.go`. Replace the function block. Full new body:

```go
// BuildBundleURL returns (url, via, error). url is the URL git will fetch;
// via is "direct" (signed object-store URL) or "proxied" (URL through this
// gateway's /_bundle/<t>/<r>/<h> handler). M19: tenant and repo are
// embedded in the URL path and bound into the proxied token's hash field
// so any path-segment tamper fails HMAC verify.
func (b *URLBuilder) BuildBundleURL(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string) (string, string, error) {
	return b.buildURL(ctx, "bundle", tenant, repo, hash, storageKey, expectedHash, b.BundleTTL)
}

// BuildPackURL is the pack-uri analogue of BuildBundleURL.
func (b *URLBuilder) BuildPackURL(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string) (string, string, error) {
	return b.buildURL(ctx, "pack", tenant, repo, hash, storageKey, expectedHash, b.PackTTL)
}

func (b *URLBuilder) buildURL(ctx context.Context, kind, tenant, repo, hash, storageKey, expectedHash string, ttl time.Duration) (string, string, error) {
	if b.Mode == URIModeOff {
		return "", "", fmt.Errorf("gateway: URI mode is off")
	}
	if b.Mode == URIModeDirect || b.Mode == URIModeAuto {
		signedURL, hdr, err := b.Store.SignedGetURL(ctx, storageKey, storage.SignedURLOptions{
			Expires: ttl, Method: "GET", ExpectedHash: expectedHash,
		})
		if err == nil && len(hdr) > 0 {
			err = fmt.Errorf("gateway: backend requires request headers on GET (%d) which v2 bundle-uri/packfile-uri cannot pin; backend not usable for direct-mode advertisement: %w", len(hdr), storage.ErrNotSupported)
		}
		if err == nil {
			return signedURL, "direct", nil
		}
		if b.Mode == URIModeDirect {
			return "", "", err
		}
		if !errors.Is(err, storage.ErrNotSupported) {
			return "", "", err
		}
		// Fall through to proxied.
	}
	// Proxied mode (or auto fallback).
	if len(b.ProxiedKey) == 0 || b.ProxiedBaseURL == "" {
		return "", "", fmt.Errorf("gateway: proxied URLs are not configured")
	}
	// M19: token hash field is the composite "<tenant>/<repo>/<hash>".
	// Any tamper of the URL path segments (tenant, repo, or hash) produces
	// a different composite on the verify side and fails HMAC.
	composite := tenant + "/" + repo + "/" + hash
	exp := b.now().Add(ttl)
	tok, err := proxiedurl.Mint(b.ProxiedKey, kind, composite, exp)
	if err != nil {
		return "", "", err
	}
	var prefix string
	switch kind {
	case "bundle":
		prefix = "/_bundle/"
	case "pack":
		prefix = "/_pack/"
	default:
		return "", "", fmt.Errorf("gateway: unsupported kind %q", kind)
	}
	base := strings.TrimRight(b.ProxiedBaseURL, "/")
	// Each path segment is escaped independently — routenames already
	// constrains tenant/repo to a safe charset, but PathEscape is
	// belt-and-suspenders in case a future name validator widens.
	return base + prefix +
		url.PathEscape(tenant) + "/" +
		url.PathEscape(repo) + "/" +
		url.PathEscape(hash) +
		"?token=" + url.QueryEscape(tok), "proxied", nil
}
```

- [ ] **Step 5: Update existing tests in proxied_url_builder_test.go for new signature**

Every existing call to `b.BuildBundleURL(ctx, hash, key, expected)` becomes `b.BuildBundleURL(ctx, "acme", "site", hash, key, expected)`. Same for `BuildPackURL`. Find them:

```bash
grep -n "BuildBundleURL\|BuildPackURL" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gateway/proxied_url_builder_test.go
```

For each line printed (excluding the doc-comment references), edit the call to insert `"acme", "site",` after `context.Background(),`. Test names and assertions otherwise unchanged.

- [ ] **Step 6: Run all builder tests**

```bash
go test ./internal/gateway/... -run "TestBuild(Bundle|Pack)URL" -count=1 -v 2>&1 | tail -40
```

Expected: PASS. If any non-M19 test fails, restore its arg list (you may have updated something that shouldn't have changed).

- [ ] **Step 7: Commit**

```bash
git add internal/gateway/proxied_url_builder.go internal/gateway/proxied_url_builder_test.go
git commit -m "gateway: URLBuilder embeds (tenant, repo) in proxied URLs (M19 Task 2)"
```

---

## Task 3: Propagate (tenant, repo) through the BuildURL closure type

**Files:**
- Modify: `internal/gitproto/uploadpack/engine.go` (closure type definition)
- Modify: `internal/gitproto/uploadpack/service.go` (call sites)
- Modify: `internal/gitproto/uploadpack/engine_test.go` (test closure literals — ~12 sites)
- Modify: `internal/sshd/server.go` (Options closure type)
- Modify: `internal/gateway/server.go` (closure wrappers — ~2 sites)
- Modify: `cmd/bucketvcs/serve.go` (closure wrappers — 2 sites)

The closure today is `func(ctx, hash, storageKey, expectedHash) (string, error)`. New: `func(ctx, tenant, repo, hash, storageKey, expectedHash) (string, error)`. service.go passes `req.Tenant`, `req.Repo` through; engine/sshd wrappers thread them; the cmd/bucketvcs and server.go inner closures forward.

- [ ] **Step 1: Find every occurrence of the closure type**

```bash
grep -rn "func(ctx context.Context, hash, storageKey, expectedHash string)" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/ /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/cmd/ 2>&1 | head -20
```

Make a list. Every match becomes `func(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string)`.

- [ ] **Step 2: Update closure type in `internal/gitproto/uploadpack/engine.go`**

Find lines 51-77 (or wherever `BundleURIBuildURL` and `PackURIBuildURL` fields are declared in `EngineRequest`). Change both signatures. The new declaration:

```go
	// BundleURIBuildURL mints the URL the bundle-uri response advertises.
	// (tenant, repo) are the request-scope identifiers; the URL embeds them
	// as the first two path segments so the same gateway can serve many
	// (tenant, repo) repos from one mount.
	BundleURIBuildURL func(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string) (string, error)
```

And for pack:

```go
	// PackURIBuildURL mints the URL the packfile-uris response advertises.
	// (tenant, repo) bind into the URL path + token; see BundleURIBuildURL.
	PackURIBuildURL func(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string) (string, error)
```

- [ ] **Step 3: Update the call sites in `internal/gitproto/uploadpack/service.go`**

Two call sites: the wrappedBuildURL for bundle (around line 665) and the BuildURL field passed to the pack-uri builder (around line 392). For each:

The bundle wrap at ~line 665 becomes:

```go
	logBuildURL := req.BundleURIBuildURL
	wrappedBuildURL := func(ctx context.Context, tenant, repo, hash, key, expected string) (string, error) {
		url, err := logBuildURL(ctx, tenant, repo, hash, key, expected)
		if err != nil {
			slog.WarnContext(ctx, "upload-pack: bundle-uri BuildURL error",
				"tenant", req.Tenant, "repo", req.Repo, "err", err)
		} else if url == "" {
			slog.WarnContext(ctx, "upload-pack: bundle-uri BuildURL returned empty URL with nil error (misconfigured backend?)",
				"tenant", req.Tenant, "repo", req.Repo)
		}
		return url, err
	}
```

The downstream call site that invokes `wrappedBuildURL` (or the BuildURL field on the sub-engine) — search for the actual invocation in service.go:

```bash
grep -nB2 -A2 "BuildURL(.*hash\|BuildURL(ctx" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gitproto/uploadpack/*.go
```

At each call site that invokes `BuildURL(ctx, hash, key, expected)`, change to `BuildURL(ctx, req.Tenant, req.Repo, hash, key, expected)`. Note: the downstream may be a sub-package (e.g. internal/uploadpack/bundleuri). If the sub-package has its own BuildURL field, propagate the signature change THERE TOO. Trace any field named `BuildURL` of the matching closure type and update the call site that invokes it.

- [ ] **Step 4: Update `internal/sshd/server.go` Options**

Find lines 47-60 (Options.BundleURIBuildURL and PackURIBuildURL fields). Replace both with the new 6-arg signature, matching the new engine.go style.

- [ ] **Step 5: Update `internal/gateway/server.go` closure wrappers**

Find lines around 302 and 342 (the inner closures that wrap `ub.BuildBundleURL` and `pub.BuildPackURL`). Each was:

```go
s.bundleURIBuildURL = func(ctx context.Context, hash, storageKey, expectedHash string) (string, error) {
	url, _, err := ub.BuildBundleURL(ctx, hash, storageKey, expectedHash)
	return url, err
}
```

Becomes:

```go
s.bundleURIBuildURL = func(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string) (string, error) {
	url, _, err := ub.BuildBundleURL(ctx, tenant, repo, hash, storageKey, expectedHash)
	return url, err
}
```

Same shape for the pack closure.

- [ ] **Step 6: Update `cmd/bucketvcs/serve.go` closure wrappers**

Find lines ~244 and ~257. Each was:

```go
bundleBuildURL = func(ctx context.Context, hash, key, expected string) (string, error) {
	u, _, err := bub.BuildBundleURL(ctx, hash, key, expected)
	return u, err
}
```

Becomes:

```go
bundleBuildURL = func(ctx context.Context, tenant, repo, hash, key, expected string) (string, error) {
	u, _, err := bub.BuildBundleURL(ctx, tenant, repo, hash, key, expected)
	return u, err
}
```

Same shape for `packBuildURL`.

- [ ] **Step 7: Update test closure literals in engine_test.go**

Every test that constructs a closure literal of the matching type needs the new signature. The pattern is `func(_ context.Context, _, _, _ string)` becomes `func(_ context.Context, _, _, _, _, _ string)`. Use sed:

```bash
cd /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied
# Patterns to replace (be careful — only apply to test files):
sed -i 's|func(_ context.Context, _, _, _ string) (string, error)|func(_ context.Context, _, _, _, _, _ string) (string, error)|g' internal/gitproto/uploadpack/engine_test.go
sed -i 's|func(_ context.Context, hash, key, expected string) (string, error)|func(_ context.Context, _, _, hash, key, expected string) (string, error)|g' internal/gitproto/uploadpack/engine_test.go
```

Verify nothing else snuck in:

```bash
grep -n "func(_ context.Context" internal/gitproto/uploadpack/engine_test.go | head -20
```

All matches must have 6 string params (5 if `_ context.Context` doesn't count itself). The shape should be `func(_ context.Context, _, _, _, _, _ string)` or `func(_ context.Context, _, _, hash, key, expected string)`.

- [ ] **Step 8: Build everything**

```bash
go build ./... 2>&1 | tail -20
```

Expected: clean. If you see compile errors, they point to closure literals or call sites the previous steps missed. Add `req.Tenant, req.Repo` or `"acme", "site"` (in tests) accordingly.

- [ ] **Step 9: Run all tests**

```bash
go test ./internal/gitproto/uploadpack/... ./internal/gateway/... ./internal/sshd/... ./cmd/bucketvcs/... -count=1 2>&1 | tail -15
```

Expected: PASS. If anything fails because of arg count mismatch, that's a missed call site.

- [ ] **Step 10: Commit**

```bash
git add internal/gitproto/uploadpack/ internal/sshd/server.go internal/gateway/server.go cmd/bucketvcs/serve.go
git commit -m "uploadpack/sshd/gateway/cmd: thread (tenant, repo) through BuildURL closure (M19 Task 3)"
```

---

## Task 4: Multi-tenant proxied handler (drop ProxiedKeyResolver, parse 3-segment path)

**Files:**
- Modify: `internal/gateway/proxied_routes.go`
- Modify: `internal/gateway/proxied_routes_test.go`
- Modify: `internal/gateway/server.go` (NewProxiedHandler call + Options field removal)

Replace the `hash`-only path parser with one that extracts `(tenant, repo, hash)` and verifies the token against the composite. Delete the `ProxiedKeyResolver` interface and field; the handler computes storage keys directly via `keys.BundleKey`/`keys.CanonicalPackKey`.

- [ ] **Step 1: Confirm the keys helper names**

```bash
grep -nE "^func (BundleKey|CanonicalPackKey|.*BundleKey|.*PackKey)" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/repo/keys/*.go | head
```

You're looking for two functions that take `(tenant, repo, hash string)` and return `string`. Names might be `BundleKey`, `BundleObjectKey`, `BundleStorageKey` etc. Use the actual exported names in the steps below.

If the keys package exports a `Repo` type with methods instead of free functions (e.g. `keys.NewRepo(tenant, repo).Bundle(hash)`), adapt: construct the `Repo` once per request inside the handler, then call its methods.

- [ ] **Step 2: Confirm routenames.ValidateName**

```bash
grep -nE "^func ValidateName" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gateway/routenames/*.go
```

Signature should be `ValidateName(s string) error`. If it's named differently, adapt.

- [ ] **Step 3: Write failing tests in proxied_routes_test.go**

The existing tests construct a `proxiedHandler` with a `ProxiedKeyResolver` mock. The new tests construct it with no resolver (deleted) and rely on direct keys.* calls. Append the following tests; they will fail until Step 4 lands:

```go
// TestProxiedHandler_BundleMultiTenantURL_OK pins that the M19 URL shape
// /_bundle/<tenant>/<repo>/<hash> with a token bound to the composite
// "<tenant>/<repo>/<hash>" serves the storage key keys.BundleKey(t,r,h).
func TestProxiedHandler_BundleMultiTenantURL_OK(t *testing.T) {
	tenant, repo := "acme", "site"
	hash := "sha256-" + strings.Repeat("ab", 32)
	composite := tenant + "/" + repo + "/" + hash

	key := bytes.Repeat([]byte{0x55}, 32)
	exp := time.Now().Add(time.Hour)
	tok, err := proxiedurl.Mint(key, "bundle", composite, exp)
	if err != nil {
		t.Fatal(err)
	}

	// Populate the stub store at keys.BundleKey(tenant, repo, hash). If
	// the actual keys helper differs, adjust this line per Step 1.
	storageKey := keys.BundleKey(tenant, repo, hash)
	bodyBytes := []byte("bundle-payload")
	store := newStubStore()
	store.put(storageKey, bodyBytes)

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_bundle/"+tenant+"/"+repo+"/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), bodyBytes) {
		t.Errorf("body mismatch")
	}
}

// TestProxiedHandler_TamperedTenant_Rejected pins that a token minted for
// (acme, site, hash) cannot be replayed against (other, site, hash) — the
// HMAC binds the composite.
func TestProxiedHandler_TamperedTenant_Rejected(t *testing.T) {
	hash := "sha256-" + strings.Repeat("ab", 32)
	composite := "acme/site/" + hash
	key := bytes.Repeat([]byte{0x66}, 32)
	tok, _ := proxiedurl.Mint(key, "bundle", composite, time.Now().Add(time.Hour))
	store := newStubStore()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_bundle/other/site/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("code=%d, want 403 (HMAC binds tenant)", w.Code)
	}
}

// TestProxiedHandler_TamperedRepo_Rejected — repo segment swap.
func TestProxiedHandler_TamperedRepo_Rejected(t *testing.T) {
	hash := "sha256-" + strings.Repeat("ab", 32)
	composite := "acme/site/" + hash
	key := bytes.Repeat([]byte{0x77}, 32)
	tok, _ := proxiedurl.Mint(key, "bundle", composite, time.Now().Add(time.Hour))
	store := newStubStore()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_bundle/acme/elsewhere/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("code=%d, want 403 (HMAC binds repo)", w.Code)
	}
}

// TestProxiedHandler_BadTenantName_Rejected — routenames.ValidateName
// must filter ".." etc. before any store lookup.
func TestProxiedHandler_BadTenantName_Rejected(t *testing.T) {
	hash := "sha256-" + strings.Repeat("ab", 32)
	key := bytes.Repeat([]byte{0x88}, 32)
	store := newStubStore()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	// Token for the bad tenant string would also pass HMAC if we constructed
	// it that way, so this test ensures the early-reject path fires BEFORE
	// HMAC verify. routenames must reject "..".
	tok, _ := proxiedurl.Mint(key, "bundle", "../site/"+hash, time.Now().Add(time.Hour))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_bundle/../site/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Errorf("bad tenant should be rejected, got 200")
	}
}

// TestProxiedHandler_MissingPathSegments — /_bundle/<just-tenant>/ has only
// 1 of 3 required segments. Reject.
func TestProxiedHandler_MissingPathSegments(t *testing.T) {
	key := bytes.Repeat([]byte{0x99}, 32)
	store := newStubStore()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_bundle/acme/site", nil) // no hash segment
	h.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Errorf("missing hash segment should reject; got 200")
	}
}
```

The test file imports must include: `"bytes"`, `"net/http"`, `"net/http/httptest"`, `"net/url"`, `"strings"`, `"testing"`, `"time"`, `"github.com/bucketvcs/bucketvcs/internal/proxiedurl"`, `"github.com/bucketvcs/bucketvcs/internal/repo/keys"`.

If `newStubStore()` doesn't exist yet in the test file, grep for it; if absent, define it as a small in-memory `storage.ObjectStore` (or extract from any existing test in the same package). The minimum surface needed: `Get(ctx, key, *GetOptions) (*storage.Object, error)` and `Head(ctx, key) (*storage.Meta, error)`. If a stub already exists in another _test.go in the package, just reuse it.

- [ ] **Step 4: Run failing tests**

```bash
go test ./internal/gateway/... -run "TestProxiedHandler_(BundleMultiTenant|TamperedTenant|TamperedRepo|BadTenantName|MissingPathSegments)" -count=1 -v 2>&1 | tail -25
```

Expected: FAIL — `NewProxiedHandler` still takes a `ProxiedKeyResolver` arg (4th non-logger arg), so the test calls with `nil` for that 4th position. The test may actually compile but every assertion fails because the handler still uses the resolver. Either way, you're not done until Step 5.

- [ ] **Step 5: Rewrite `internal/gateway/proxied_routes.go`**

Replace the entire `ProxiedKeyResolver` interface, the field on `proxiedHandler`, the constructor signature, and the `ServeHTTP` body that parses the path. Concretely:

(a) Delete the `ProxiedKeyResolver` interface (lines 17-33 in the file you read at Task 0).

(b) Update `NewProxiedHandler` signature and `proxiedHandler` struct:

```go
// NewProxiedHandler returns an http.Handler serving
// /_bundle/<tenant>/<repo>/<hash> and /_pack/<tenant>/<repo>/<hash>
// from store, gated by HMAC tokens minted with key. Storage keys are
// computed via internal/repo/keys; there is no resolver indirection.
//
// logger is used for served-* metrics and the proxied.url.served audit
// event. If nil, slog.Default() is used.
func NewProxiedHandler(store storage.ObjectStore, key []byte, bundlePrefix, packPrefix string, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &proxiedHandler{
		store:        store,
		key:          key,
		bundlePrefix: bundlePrefix,
		packPrefix:   packPrefix,
		now:          time.Now,
		logger:       logger,
	}
}

type proxiedHandler struct {
	store        storage.ObjectStore
	key          []byte
	bundlePrefix string
	packPrefix   string
	now          func() time.Time
	logger       *slog.Logger
}
```

(c) Replace `ServeHTTP`. Full new body:

```go
func (h *proxiedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cw := &countingWriter{ResponseWriter: w}
	var kind, rest string
	switch {
	case strings.HasPrefix(r.URL.Path, h.bundlePrefix):
		kind = "bundle"
		rest = strings.TrimPrefix(r.URL.Path, h.bundlePrefix)
	case strings.HasPrefix(r.URL.Path, h.packPrefix):
		kind = "pack"
		rest = strings.TrimPrefix(r.URL.Path, h.packPrefix)
	default:
		http.NotFound(cw, r)
		return
	}
	// M19: parse <tenant>/<repo>/<hash> — exactly 3 non-empty segments.
	segs := strings.Split(rest, "/")
	if len(segs) != 3 || segs[0] == "" || segs[1] == "" || segs[2] == "" {
		http.NotFound(cw, r)
		return
	}
	tenant, repo, hash := segs[0], segs[1], segs[2]
	// Validate tenant and repo against the same charset the normal git
	// router uses. This rejects ".." and other path-tricky values BEFORE
	// any store lookup.
	if err := routenames.ValidateName(tenant); err != nil {
		http.NotFound(cw, r)
		return
	}
	if err := routenames.ValidateName(repo); err != nil {
		http.NotFound(cw, r)
		return
	}
	if !validProxiedHash(kind, hash) {
		http.NotFound(cw, r)
		return
	}
	// Token verify against the composite <tenant>/<repo>/<hash>. Bind happens
	// here so a 401 leaks no information about whether the (tenant, repo)
	// exists or has the object.
	tok := r.URL.Query().Get("token")
	if tok == "" {
		emitMetric(r.Context(), h.logger, "proxied_url_token_invalid_total", 1,
			"reason", "missing", "tenant", tenant, "repo", repo)
		http.Error(cw, "missing token", http.StatusForbidden)
		return
	}
	composite := tenant + "/" + repo + "/" + hash
	if _, err := proxiedurl.Verify(h.key, tok, kind, composite, h.now()); err != nil {
		reason := "invalid"
		msg := "invalid token"
		switch {
		case errors.Is(err, proxiedurl.ErrTokenExpired):
			reason, msg = "expired", "token expired"
		case errors.Is(err, proxiedurl.ErrKindMismatch):
			reason = "kind_mismatch"
		}
		emitMetric(r.Context(), h.logger, "proxied_url_token_invalid_total", 1,
			"reason", reason, "tenant", tenant, "repo", repo)
		http.Error(cw, msg, http.StatusForbidden)
		return
	}
	// Compute the storage key directly. No resolver indirection.
	var storageKey string
	switch kind {
	case "bundle":
		storageKey = keys.BundleKey(tenant, repo, hash)
	case "pack":
		storageKey = keys.CanonicalPackKey(tenant, repo, hash)
	}
	h.serveObject(r.Context(), cw, r, kind, hash, tenant, repo, storageKey)
}
```

(If `keys.BundleKey`/`keys.CanonicalPackKey` are actually named differently per Task 0 Step 2, adapt; or use a `keys.NewRepo(tenant, repo)` and method calls if that's the shape.)

(d) Update `serveObject` to accept and forward `tenant`, `repo`:

```go
func (h *proxiedHandler) serveObject(ctx context.Context, w *countingWriter, r *http.Request, kind, hash, tenant, repo, key string) {
```

…and at every `h.emitServed(ctx, kind, hash, w.bytes, ...)` call, change to `h.emitServed(ctx, kind, hash, tenant, repo, w.bytes, ...)`. (The actual `emitServed` signature change comes in Task 6 — for now adjust the calls but leave `emitServed` itself alone. Compilation will fail; that's fine until Task 6 lands. **Alternative:** in this task, just keep `emitServed` calls unchanged and let Task 6 add the tenant/repo args. Pick whichever keeps each task green-on-green.)

Recommended: **Keep emitServed calls unchanged in Task 4; tenant/repo audit attrs land in Task 6.**

(e) Add imports: `"github.com/bucketvcs/bucketvcs/internal/gateway/routenames"` and `"github.com/bucketvcs/bucketvcs/internal/repo/keys"`.

- [ ] **Step 6: Remove `ProxiedKeyResolver` field from `gateway.Options`**

Edit `internal/gateway/server.go`. Find `Options.ProxiedKeyResolver` field declaration; delete it and its doc comment. Then find the `NewProxiedHandler(store, opts.ProxiedURLSigningKey, "/_bundle/", "/_pack/", opts.ProxiedKeyResolver, s.logger)` call (around line 351) and remove the `opts.ProxiedKeyResolver,` arg:

```go
proxied := NewProxiedHandler(store, opts.ProxiedURLSigningKey, "/_bundle/", "/_pack/", s.logger)
```

- [ ] **Step 7: Update existing proxied_routes_test.go callsites**

The existing tests use `NewProxiedHandler(store, key, "/_bundle/", "/_pack/", resolver, logger)`. Remove the resolver arg from each call. Find them:

```bash
grep -n "NewProxiedHandler(" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gateway/proxied_routes_test.go
```

For each line: delete the `resolver,` (or whatever local mock variable name) argument. Also delete the mock resolver type definitions and helper constructors at the top of the test file (they're now unreachable code).

The existing tests also use the old URL shape `/_bundle/<hash>`; those will now fail (3-segment parse rejects them). Either:
(a) Update them to use the new shape, OR
(b) Delete tests that overlap with the new tests from Step 3.

Pick whichever leaves the test file the cleanest. The new tests from Step 3 already cover the happy path + tampering; many of the OLD tests (`TestProxiedHandler_BundleHappyPath` etc.) become redundant. **Preferred:** keep the new tests; delete tests that test exact-URL-shape semantics; preserve tests that test orthogonal behaviors (Range serving, method-not-allowed, expired token, hash format validation).

- [ ] **Step 8: Build everything**

```bash
go build ./... 2>&1 | tail -20
```

Expected: clean. If anything outside the gateway package references `ProxiedKeyResolver`, you'll see compile errors — fix by removing those references too.

- [ ] **Step 9: Run gateway tests**

```bash
go test ./internal/gateway/... -count=1 2>&1 | tail -20
```

Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/gateway/proxied_routes.go internal/gateway/proxied_routes_test.go internal/gateway/server.go
git commit -m "gateway: proxied handler parses /_<kind>/<t>/<r>/<h>; drop ProxiedKeyResolver (M19 Task 4)"
```

---

## Task 5: Wire `gateway.Options.ProxiedURLSigningKey` in serve.go

**Files:**
- Modify: `cmd/bucketvcs/serve.go`

This is the central M11 bug. Today `gateway.Options{}` is built without `ProxiedURLSigningKey`, so the `/_bundle/`, `/_pack/` mount at `gateway/server.go:350` is skipped. M19 populates it.

- [ ] **Step 1: Find the gateway.Options literal**

```bash
grep -nB2 -A20 "gateway.Options{" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/cmd/bucketvcs/serve.go | head -50
```

You're looking for the `gateway.Options{...}` literal. It's the one with `AuthStore`, `MirrorDir`, etc. — likely around line 355-400.

- [ ] **Step 2: Add `ProxiedURLSigningKey` and `ProxiedBaseURL`**

Inside the `gateway.Options{...}` literal, add:

```go
		ProxiedURLSigningKey: signingKey,
		ProxiedBaseURL:       *proxiedBaseURL,
```

`signingKey` and `*proxiedBaseURL` are already in scope (they're used for the URLBuilder around line 239). If they're not in scope at the Options-literal site (e.g. behind a feature flag), hoist them earlier in the function.

If `gateway.Options` doesn't currently have a `ProxiedBaseURL` field, verify:

```bash
grep -n "ProxiedBaseURL\|ProxiedURLSigningKey" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gateway/server.go | head
```

If only `ProxiedURLSigningKey` exists on `Options`, drop the `ProxiedBaseURL` line — it's the URLBuilder's concern, not the inbound handler's. The handler doesn't need a base URL; it serves whatever path it's mounted at.

- [ ] **Step 3: Delete the NOTE comment**

Find lines 227-234 of cmd/bucketvcs/serve.go (the NOTE block starting "NOTE: This task wires URL minting only;"). Delete the entire NOTE comment block. Replace with a one-line summary:

```go
// Build URLBuilder-backed closures once and share between the HTTP
// gateway and the SSH listener; minting and serving use the same key.
```

- [ ] **Step 4: Build**

```bash
go build ./... 2>&1 | tail -10
```

Expected: clean.

- [ ] **Step 5: Smoke-build and confirm the mount fires**

```bash
go build -o /tmp/bucketvcs-m19 ./cmd/bucketvcs
/tmp/bucketvcs-m19 serve --help 2>&1 | grep -E "bundle-uri|pack-uri|proxied" | head -10
```

Expected: flags still documented; nothing missing.

- [ ] **Step 6: Run full test suite**

```bash
go test ./... -count=1 2>&1 | tail -15
```

Expected: PASS for all packages M19 touches. (The known pre-existing importer flake from M12 may still occur — re-run if so.)

- [ ] **Step 7: Commit**

```bash
git add cmd/bucketvcs/serve.go
git commit -m "cmd/serve: wire gateway.Options.ProxiedURLSigningKey; close M11 bug (M19 Task 5)"
```

---

## Task 6: Add tenant/repo to metrics + audit

**Files:**
- Modify: `internal/gateway/proxied_routes.go` (emitServed signature + callers)
- Modify: `internal/gateway/observability.go` (or wherever `emitProxiedURLServed` is defined)
- Possibly modify: `internal/gateway/observability_test.go` if tests pin metric label sets

Existing `bundle_uri_served_total`, `pack_uri_served_total`, `bundle.uri.served`, `pack.uri.served` gain `tenant` + `repo` labels/attrs. No new event names.

- [ ] **Step 1: Find emitProxiedURLServed**

```bash
grep -nB2 -A6 "func emitProxiedURLServed" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/internal/gateway/*.go
```

- [ ] **Step 2: Write failing test**

In `internal/gateway/observability_test.go` (create if absent), or in `proxied_routes_test.go`, add:

```go
// TestProxiedHandler_AuditIncludesTenantRepo asserts the served audit
// event carries tenant + repo attrs (M19 multi-tenant observability).
func TestProxiedHandler_AuditIncludesTenantRepo(t *testing.T) {
	tenant, repo := "acme", "site"
	hash := "sha256-" + strings.Repeat("ab", 32)
	composite := tenant + "/" + repo + "/" + hash
	key := bytes.Repeat([]byte{0xAA}, 32)
	tok, _ := proxiedurl.Mint(key, "bundle", composite, time.Now().Add(time.Hour))

	storageKey := keys.BundleKey(tenant, repo, hash)
	store := newStubStore()
	store.put(storageKey, []byte("data"))

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", logger)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_bundle/"+tenant+"/"+repo+"/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: code=%d", w.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, `"tenant":"acme"`) {
		t.Errorf("audit log missing tenant=acme: %s", logged)
	}
	if !strings.Contains(logged, `"repo":"site"`) {
		t.Errorf("audit log missing repo=site: %s", logged)
	}
}
```

Add the imports: `"log/slog"` if not present.

- [ ] **Step 3: Run failing test**

```bash
go test ./internal/gateway/... -run TestProxiedHandler_AuditIncludesTenantRepo -count=1 -v 2>&1 | tail -10
```

Expected: FAIL (tenant/repo attrs absent from the log).

- [ ] **Step 4: Update emitProxiedURLServed**

Edit the function definition to accept tenant + repo:

```go
func emitProxiedURLServed(ctx context.Context, logger *slog.Logger, kind, hash, tenant, repo string, bytesServed int64, statusCode int, rangeRequest bool) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, kind+".uri.served",
		slog.String("kind", kind),
		slog.String("hash", hash),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.Int64("bytes_served", bytesServed),
		slog.Int("status_code", statusCode),
		slog.Bool("range_request", rangeRequest),
	)
}
```

If the existing function has a different shape, adapt — preserve all existing attrs, just add `tenant` + `repo`.

- [ ] **Step 5: Update emitServed (the wrapper in proxied_routes.go)**

```go
func (h *proxiedHandler) emitServed(ctx context.Context, kind, hash, tenant, repo string, bytesServed int64, statusCode int, rangeRequest bool) {
	emitMetric(ctx, h.logger, kind+"_uri_served_total", 1, "via", "proxied", "tenant", tenant, "repo", repo)
	emitMetric(ctx, h.logger, kind+"_uri_served_bytes", bytesServed, "via", "proxied", "tenant", tenant, "repo", repo)
	emitProxiedURLServed(ctx, h.logger, kind, hash, tenant, repo, bytesServed, statusCode, rangeRequest)
}
```

- [ ] **Step 6: Update emitServed callers in serveObject**

In `serveObject`, every `h.emitServed(ctx, kind, hash, w.bytes, ...)` becomes `h.emitServed(ctx, kind, hash, tenant, repo, w.bytes, ...)`. There are 2 call sites (full-object 200 path and Range 206 path). Also ensure `serveObject` itself receives `tenant, repo` (Step 5 of Task 4 set this up).

- [ ] **Step 7: Run failing test**

```bash
go test ./internal/gateway/... -run TestProxiedHandler_AuditIncludesTenantRepo -count=1 -v 2>&1 | tail -10
```

Expected: PASS.

- [ ] **Step 8: Run all gateway tests**

```bash
go test ./internal/gateway/... -count=1 2>&1 | tail -10
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/gateway/
git commit -m "gateway: bundle_uri/pack_uri served metric+audit gain tenant+repo (M19 Task 6)"
```

---

## Task 7: End-to-end smoke + operator guide update

**Files:**
- Create: `scripts/m19-multitenant-proxied-smoke.sh`
- Modify: `docs/m11-bundles-operator-guide.md`

- [ ] **Step 1: Study a recent smoke script**

```bash
ls /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/scripts/m1[6-8]*.sh
cat /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/scripts/m18-rate-limit-smoke.sh | head -80
```

Match the shape: `set -euo pipefail`, tempdir, port-finder, cleanup trap, EXIT-time forensics.

- [ ] **Step 2: Write `scripts/m19-multitenant-proxied-smoke.sh`**

The smoke must:
1. Build bucketvcs and start serve with `--bundle-uri-mode=proxied --pack-uri-mode=proxied --proxied-url-signing-key=<base64> --proxied-url-base=http://127.0.0.1:<PORT>` against localfs storage
2. Create tenant `acme` + tenant `other`; create repo `r1` in each; push a small repo to each
3. Trigger maintenance to materialize a bundle for each
4. Use `git ls-remote --bundle-uri` (or curl the v2 advertise endpoint directly) to extract the advertised URL for each
5. Assert URL for tenant `acme` contains `/_bundle/acme/r1/`
6. Assert URL for tenant `other` contains `/_bundle/other/r1/`
7. Curl both URLs → 200 + non-empty body
8. Swap the tenant segment in acme's URL → 403
9. grep `serve.log` for `"tenant":"acme"` and `"tenant":"other"` (proves audit emission per tenant)
10. Echo `M19_MULTITENANT_PROXIED_SMOKE_OK`

Approximate skeleton (adapt to existing helpers):

```bash
#!/usr/bin/env bash
set -euo pipefail
# ... boilerplate copied from m18-rate-limit-smoke.sh ...

PORT=$(grep -m1 "^[0-9]" <<< "$(comm -23 <(seq 18000 18999 | sort) <(ss -ltn | awk 'NR>1 {sub(/.*:/, "", $4); print $4}' | sort -u))")
# Two tenants, two repos.
"$BIN" user add alice
TOKA=$("$BIN" token create --user alice | sed -n 's/^token=//p' | head -1)
"$BIN" repo create acme/r1 --owner alice
"$BIN" repo create other/r1 --owner alice
# push small repos to each (use git via http or direct fs init)
# ... seed each repo ...
# trigger maintenance
"$BIN" maintenance --tenant acme --repo r1 --build-bundles
"$BIN" maintenance --tenant other --repo r1 --build-bundles
# advertise
ACME_URL=$(curl -sS -u "alice:$TOKA" "http://127.0.0.1:$PORT/acme/r1.git/info/refs?service=git-upload-pack" \
  | tee /dev/null \
  | grep -oE 'https?://[^"]*/_bundle/[^"]*' | head -1)
OTHER_URL=$(curl -sS -u "alice:$TOKA" "http://127.0.0.1:$PORT/other/r1.git/info/refs?service=git-upload-pack" \
  | grep -oE 'https?://[^"]*/_bundle/[^"]*' | head -1)

[[ "$ACME_URL" == *"/_bundle/acme/r1/"* ]] || { echo "FAIL: acme URL=$ACME_URL"; exit 1; }
[[ "$OTHER_URL" == *"/_bundle/other/r1/"* ]] || { echo "FAIL: other URL=$OTHER_URL"; exit 1; }
echo "OK   acme URL: $ACME_URL"
echo "OK   other URL: $OTHER_URL"

curl -fsS "$ACME_URL" -o /tmp/acme-bundle.bin && [[ -s /tmp/acme-bundle.bin ]] || { echo "FAIL: GET acme"; exit 1; }
curl -fsS "$OTHER_URL" -o /tmp/other-bundle.bin && [[ -s /tmp/other-bundle.bin ]] || { echo "FAIL: GET other"; exit 1; }

# Swap tenant: replace acme with other in acme's URL, keeping acme's token.
SWAPPED=$(echo "$ACME_URL" | sed 's|/_bundle/acme/|/_bundle/other/|')
STATUS=$(curl -sS -o /dev/null -w '%{http_code}' "$SWAPPED")
[[ "$STATUS" == "403" ]] || { echo "FAIL: tenant swap returned $STATUS, want 403"; exit 1; }
echo "OK   tenant swap rejected: $STATUS"

grep -q '"tenant":"acme"' "$SERVE_LOG" || { echo "FAIL: serve log missing tenant=acme audit"; exit 1; }
grep -q '"tenant":"other"' "$SERVE_LOG" || { echo "FAIL: serve log missing tenant=other audit"; exit 1; }
echo "OK   audit log contains both tenants"

echo "M19_MULTITENANT_PROXIED_SMOKE_OK"
```

**Adapt the seeding step** (`push small repos`) to whatever the recent smokes do — likely a git init + git push over http://localhost:$PORT/<tenant>/<repo>.git using the token in Basic auth. M18's smoke or M17's smoke is a good reference for the exact incantation.

Make executable: `chmod +x scripts/m19-multitenant-proxied-smoke.sh`.

- [ ] **Step 3: Run smoke**

```bash
./scripts/m19-multitenant-proxied-smoke.sh 2>&1 | tail -25
```

Iterate until it ends with `M19_MULTITENANT_PROXIED_SMOKE_OK`. Common pitfalls:
- Port collision: increment PORT range
- Bundle materialization timing: add a small sleep after `maintenance --build-bundles`
- URL extraction regex: adjust if v2 advertise uses a different framing
- Missing `--build-bundles` flag: check `bucketvcs maintenance --help` for the actual flag

- [ ] **Step 4: Confirm prior smokes still pass**

```bash
./scripts/m11-bundles-smoke.sh 2>&1 | tail -3 || true
./scripts/m18-rate-limit-smoke.sh 2>&1 | tail -3
```

Expected: each ends with its OK marker.

- [ ] **Step 5: Update docs/m11-bundles-operator-guide.md**

```bash
grep -nE "single.repo|multi.tenant|deferred|m19|M19" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/docs/m11-bundles-operator-guide.md | head -20
```

At each line referencing "single-repo only" / "multi-tenant deferred" / "successor milestone": replace with a sentence describing the new URL shape `/_bundle/<tenant>/<repo>/<hash>` and that one gateway can now serve many tenants. The exact rewrites depend on the section context; aim for minimal diff that retracts the caveat without disturbing the surrounding prose.

Also: search for any example URLs in the guide that show `/_bundle/<hash>` (without tenant/repo); update to `/_bundle/<tenant>/<repo>/<hash>` to match the new shape.

- [ ] **Step 6: Commit smoke + guide**

```bash
git add scripts/m19-multitenant-proxied-smoke.sh docs/m11-bundles-operator-guide.md
git commit -m "scripts+docs: M19 multi-tenant proxied smoke + operator-guide retraction (M19 Task 7)"
```

---

## Acceptance verification (post-Task 7)

After all tasks land, run:

```bash
go vet ./...
go build ./...
go test ./... -count=1
./scripts/m19-multitenant-proxied-smoke.sh
./scripts/m18-rate-limit-smoke.sh
./scripts/m17-auth-scopes-smoke.sh
./scripts/m11-bundles-smoke.sh
```

All must pass. `ProxiedKeyResolver` must NOT appear in any production .go file:

```bash
grep -rn "ProxiedKeyResolver" --include="*.go" /home/eran/work/bucketvcs/.claude/worktrees/m19-multitenant-proxied/
```

Expected: zero matches (or only matches in deleted test mocks that may remain in `*_test.go` — also zero ideally).

`bucketvcs serve --bundle-uri-mode=proxied --proxied-url-signing-key=...` must actually serve `/_bundle/<t>/<r>/<h>` (the smoke verifies this; the central M11 bug is closed).

---

## Self-review notes

- **Spec coverage:** §1.1 in scope items each map to a task. URL+token shape → Task 2 + Task 4. ProxiedKeyResolver removal → Task 4. serve.go wiring → Task 5. Metrics + audit → Task 6. Smoke + guide → Task 7. Token round-trip pin → Task 1. (Task 3 — closure signature fan-out — is implicit in the spec under "code surface" §2.)
- **Placeholder scan:** none. Each step has the actual code or command.
- **Type consistency:** the closure signature `func(ctx, tenant, repo, hash, storageKey, expectedHash) (string, error)` is introduced in Task 3 and used identically in Task 2's URLBuilder return path and in the closure-wrapper updates in Tasks 3 + 5. `keys.BundleKey(tenant, repo, hash)` / `keys.CanonicalPackKey(tenant, repo, hash)` are used in Task 4's handler and Task 3's test setup (Task 0 confirms the real names before Task 4 commits).
- **Cross-task coupling:** Task 4 makes `serveObject` accept `tenant, repo` but leaves `emitServed` calls unchanged; Task 6 picks up `emitServed`. This is deliberate — each task's tests pass on commit.
