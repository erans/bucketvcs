# M13 Phase 2 — Transfer Wiring

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `bucketvcs serve --lfs --store=localfs:...` work end-to-end with a stock `git-lfs` client. Phase 1 returns per-object 503 on localfs because `Store.ProxiedPutURL`/`ProxiedGetURL` are stubs; Phase 2 wires them to mint HMAC URLs against a new `/_lfs/<tenant>/<repo>/<oid>` proxied route that handles PUT (upload) and GET (download) by streaming bytes to/from the configured `ObjectStore`.

**Architecture:** Extend `internal/proxiedurl/token.go` to support two new kinds (`"lfs-put"` and `"lfs-get"`). Extend `internal/lfs/Store` with a `WithProxied(key, baseURL, tenant, repo)` builder so the URL methods can mint complete URLs without altering the existing `NewStore` signature. Add `internal/lfs/proxied.go` — a new gateway handler mounted at `/_lfs/` (sibling to the existing `/_bundle/` and `/_pack/`) that verifies the token, parses tenant/repo/oid from the URL path, and serves PUT/GET against the underlying `ObjectStore`. Wire the route in `gateway.NewServer` when `LFSEnabled && ProxiedURLSigningKey` are both set.

**Tech Stack:** Go. Reuses `internal/proxiedurl` (HMAC), `internal/storage.ObjectStore` (Get/PutIfAbsent/Head), `internal/lfs.Store` (Phase 0/1). New package additions only inside `internal/lfs/` and one file in `internal/proxiedurl/`.

**Reference spec:** `docs/superpowers/specs/2026-05-18-m13-lfs-design.md` (§§ 2 architecture, 3 components, 5.1/5.2 wire flows).

**Master plan:** `docs/superpowers/plans/2026-05-18-m13-lfs.md` (Phase 2 outline).

---

## Phase 2 design decisions

These are committed up-front:

1. **Mount at `/_lfs/<tenant>/<repo>/<oid>` rather than `/<tenant>/<repo>.git/lfs/objects/<oid>`.** The underscore-prefixed path is a sibling of the existing `/_bundle/` and `/_pack/` proxied routes, mounted directly on the gateway mux. This bypasses `routeRoot`/`routeRepo`'s `RunAuth` (the token IS the authorization), avoids any path collision risk with Git smart-HTTP, and keeps the proxied infrastructure clearly separate from the actor-authed paths. The LFS Batch endpoint stays where Phase 1 put it (`/<tenant>/<repo>.git/info/lfs/objects/batch`).
2. **Token kinds:** Add `"lfs-put"` (kind byte 3) and `"lfs-get"` (kind byte 4) to `internal/proxiedurl/token.go`. The token's `hash` field is `<tenant>/<repo>/<oid>` — three path-separated components so a token minted for tenant=A,repo=X,oid=Y cannot drive PUT/GET against tenant=B.
3. **OID validation lives in the proxied handler.** Same regex (`^[0-9a-f]{64}$`) as Phase 1's `Build`. Tenant/repo go through `routenames.ValidateName`.
4. **PUT semantics:** the proxied handler calls `storage.PutIfAbsent(key, body, &PutOptions{})`. On `ErrAlreadyExists` we return 200 (idempotent — LFS clients can retry safely). On size mismatch or backend error we return the appropriate 4xx/5xx.
5. **`Content-Length` cap on PUT body:** 5 GiB (the S3 single-PUT limit per the spec §4). Enforced via `http.MaxBytesReader`. This is generous for localfs / Azure too.
6. **Auth on the proxied URL is the token, period.** No actor-based check. The token's expiry (`--lfs-presign-ttl`, default 15m) bounds replay risk.
7. **Phase 2 covers localfs; cloud backends still use direct signed URLs.** The proxied path is reachable only when `Capabilities().SignedURLs == false`. Cloud-backed deployments minting a proxied URL would be a misconfiguration (the gateway would burn bandwidth needlessly). For now we keep the simple rule: cloud → direct, localfs → proxied.
8. **Metrics:** Add `lfs_presign_seconds{backend}` (per-presign-call duration, milliseconds-as-int64) and `lfs_object_served_total{op,result}` (per proxied PUT/GET). Audit event `event=lfs.object.served` mirrors the M11 `proxied.url.served` shape.

---

## File structure (Phase 2)

| File | Purpose | New / Modified |
|---|---|---|
| `internal/proxiedurl/token.go` | Add `lfs-put` (kind=3) and `lfs-get` (kind=4) to Mint, encodePayload, decodePayload. | Modified |
| `internal/proxiedurl/token_test.go` | New mint+verify tests covering both new kinds. | Modified |
| `internal/lfs/store.go` | Add `WithProxied(key, baseURL, tenant, repo)` builder + real `ProxiedPutURL`/`ProxiedGetURL` minting tokens via `internal/proxiedurl`. | Modified |
| `internal/lfs/store_test.go` | Tests covering token-mint shape, URL format, and the "no proxied config" stub case. | Modified |
| `internal/lfs/proxied.go` | New handler: parses `/_lfs/<tenant>/<repo>/<oid>`, verifies token, serves PUT via `PutIfAbsent` / GET via `Get`. | New |
| `internal/lfs/proxied_test.go` | httptest-based PUT round-trip + GET round-trip + token-failure cases. | New |
| `internal/lfs/metrics.go` | Add `lfs_presign_seconds{backend}` + `lfs_object_served_total{op,result}` emitters. | Modified |
| `internal/lfs/metrics_test.go` | Tests for the new emitters. | Modified |
| `internal/lfs/audit.go` | Add `emitLFSObjectServed(ctx, logger, op, hash, bytes, status)` slog audit. | Modified |
| `internal/lfs/audit_test.go` | Test the new audit emit. | Modified |
| `internal/gateway/server.go` | Mount `/_lfs/` proxied handler on the mux when `LFSEnabled && len(ProxiedURLSigningKey) >= 16 && ProxiedBaseURL != ""`. Pass proxied config into `lfs.NewStore` via `WithProxied`. | Modified |
| `cmd/bucketvcs/serve.go` | Make `--lfs` require `--proxied-url-signing-key` + `--proxied-url-base` when the configured store has `Capabilities().SignedURLs == false` (i.e., localfs). Document this in the help text. | Modified |
| (End-to-end test) | New `internal/lfs/e2e_test.go` driving batch → proxied PUT → batch → proxied GET against an in-memory gateway+localfs. | New |

---

## Task decomposition

Phase 2 contains 7 implementation tasks (2.1–2.7) plus worktree setup (2.0) and review/squash (2.8).

---

### Task 2.0: Worktree + branch

- [ ] **Step 1: Create the worktree off main**

```bash
cd /home/eran/work/bucketvcs
git worktree add -b m13-lfs-p2 .claude/worktrees/m13-lfs-p2 main
cd .claude/worktrees/m13-lfs-p2
```

Expected: clean tree on branch `m13-lfs-p2`, head is the Phase 1 squash commit.

- [ ] **Step 2: Sanity-check baseline**

```bash
go test ./internal/lfs/ ./internal/gateway/ ./internal/proxiedurl/ ./cmd/bucketvcs/ 2>&1 | tail -8
```

Expected: ALL PASS.

---

### Task 2.1: Extend proxiedurl token to support lfs-put/lfs-get

**Files:**
- Modify: `internal/proxiedurl/token.go`
- Test: `internal/proxiedurl/token_test.go`

#### Step 1: Write the failing tests

Append to `internal/proxiedurl/token_test.go` (look for the existing test file structure first):

```go
func TestMintVerify_LFSPut(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    hash := "acme/foo/" + strings.Repeat("a", 64)
    tok, err := Mint(key, "lfs-put", hash, time.Now().Add(time.Minute))
    if err != nil {
        t.Fatalf("Mint(lfs-put): %v", err)
    }
    decoded, err := Verify(key, tok, "lfs-put", hash, time.Now())
    if err != nil {
        t.Fatalf("Verify(lfs-put): %v", err)
    }
    if decoded.Kind != "lfs-put" {
        t.Errorf("Kind=%q", decoded.Kind)
    }
}

func TestMintVerify_LFSGet(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    hash := "acme/foo/" + strings.Repeat("a", 64)
    tok, err := Mint(key, "lfs-get", hash, time.Now().Add(time.Minute))
    if err != nil {
        t.Fatalf("Mint(lfs-get): %v", err)
    }
    decoded, err := Verify(key, tok, "lfs-get", hash, time.Now())
    if err != nil {
        t.Fatalf("Verify(lfs-get): %v", err)
    }
    if decoded.Kind != "lfs-get" {
        t.Errorf("Kind=%q", decoded.Kind)
    }
}

func TestVerify_LFSKindMismatch(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    hash := "acme/foo/" + strings.Repeat("a", 64)
    tok, err := Mint(key, "lfs-put", hash, time.Now().Add(time.Minute))
    if err != nil {
        t.Fatalf("Mint: %v", err)
    }
    // Token minted as lfs-put cannot be used as lfs-get.
    if _, err := Verify(key, tok, "lfs-get", hash, time.Now()); !errors.Is(err, ErrKindMismatch) {
        t.Fatalf("expected ErrKindMismatch; got %v", err)
    }
    // Or as bundle.
    if _, err := Verify(key, tok, "bundle", hash, time.Now()); !errors.Is(err, ErrKindMismatch) {
        t.Fatalf("expected ErrKindMismatch; got %v", err)
    }
}

func TestMint_RejectsUnknownKind(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    _, err := Mint(key, "frobnicate", "hash", time.Now().Add(time.Minute))
    if err == nil {
        t.Fatal("expected error for unknown kind")
    }
}
```

(Add imports `strings` if not present.)

Run: `go test ./internal/proxiedurl/ -run TestMintVerify_LFS -v` — expect FAIL (kind validation in Mint rejects "lfs-put").

#### Step 2: Extend token.go

In `internal/proxiedurl/token.go`:

**Update `Mint` kind validation:**

```go
if kind != "bundle" && kind != "pack" && kind != "lfs-put" && kind != "lfs-get" {
    return "", fmt.Errorf("proxiedurl: invalid kind %q", kind)
}
```

**Update `encodePayload`:**

```go
func encodePayload(kind, hash string, exp time.Time) []byte {
    var k byte
    switch kind {
    case "bundle":
        k = 1
    case "pack":
        k = 2
    case "lfs-put":
        k = 3
    case "lfs-get":
        k = 4
    default:
        panic(fmt.Sprintf("proxiedurl.encodePayload: invalid kind %q (caller must validate)", kind))
    }
    // ... existing body ...
}
```

**Update `decodePayload`:**

```go
func decodePayload(p []byte) (Token, error) {
    if len(p) < 10 {
        return Token{}, fmt.Errorf("payload too short (%d)", len(p))
    }
    var kind string
    switch p[0] {
    case 1:
        kind = "bundle"
    case 2:
        kind = "pack"
    case 3:
        kind = "lfs-put"
    case 4:
        kind = "lfs-get"
    default:
        return Token{}, fmt.Errorf("unknown kind byte %d", p[0])
    }
    // ... existing body ...
}
```

**Update doc comments** on `Mint` and the package doc.go (if present) to mention the new kinds.

Run: `go test ./internal/proxiedurl/` — expect ALL PASS.

#### Step 3: Commit

```bash
git add internal/proxiedurl/token.go internal/proxiedurl/token_test.go
git commit -m "proxiedurl: add lfs-put and lfs-get token kinds"
```

---

### Task 2.2: Extend Store with proxied config + real URL minting

**Files:**
- Modify: `internal/lfs/store.go`
- Test: `internal/lfs/store_test.go`

#### Step 1: Write the failing tests

Append to `internal/lfs/store_test.go`:

```go
func TestStore_WithProxied_PUT_URL(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    s := NewStore(&fakeStore{}, "tenants/acme/repos/foo/lfs/objects/").
        WithProxied(key, "https://gw.example", "acme", "foo")
    oid := strings.Repeat("a", 64)
    url, hdr := s.ProxiedPutURL(oid, 100, time.Minute)
    if url == "" {
        t.Fatal("expected non-empty proxied URL")
    }
    if !strings.HasPrefix(url, "https://gw.example/_lfs/acme/foo/"+oid+"?token=") {
        t.Errorf("URL prefix wrong: %s", url)
    }
    if hdr == nil || hdr.Get("Content-Type") != "application/octet-stream" {
        t.Errorf("missing/wrong Content-Type header")
    }
}

func TestStore_WithProxied_GET_URL(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    s := NewStore(&fakeStore{}, "p/").
        WithProxied(key, "https://gw.example", "acme", "foo")
    oid := strings.Repeat("a", 64)
    url, hdr := s.ProxiedGetURL(oid, time.Minute)
    if !strings.HasPrefix(url, "https://gw.example/_lfs/acme/foo/"+oid+"?token=") {
        t.Errorf("URL prefix wrong: %s", url)
    }
    if hdr != nil {
        t.Errorf("expected nil header on GET; got %+v", hdr)
    }
}

func TestStore_NoProxiedConfig_ReturnsEmpty(t *testing.T) {
    // Without WithProxied, the methods are stubs (preserve P0/P1 behavior).
    s := NewStore(&fakeStore{}, "p/")
    oid := strings.Repeat("a", 64)
    if url, _ := s.ProxiedPutURL(oid, 100, time.Minute); url != "" {
        t.Errorf("expected empty URL without WithProxied; got %q", url)
    }
    if url, _ := s.ProxiedGetURL(oid, time.Minute); url != "" {
        t.Errorf("expected empty URL without WithProxied; got %q", url)
    }
}

func TestStore_WithProxied_TokenIsVerifiable(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    s := NewStore(&fakeStore{}, "p/").
        WithProxied(key, "https://gw.example", "acme", "foo")
    oid := strings.Repeat("a", 64)
    url, _ := s.ProxiedPutURL(oid, 100, time.Minute)
    u, err := neturl.Parse(url)
    if err != nil {
        t.Fatalf("parse: %v", err)
    }
    tok := u.Query().Get("token")
    if tok == "" {
        t.Fatal("token missing")
    }
    expectedHash := "acme/foo/" + oid
    decoded, err := proxiedurl.Verify(key, tok, "lfs-put", expectedHash, time.Now())
    if err != nil {
        t.Fatalf("Verify: %v", err)
    }
    if decoded.Kind != "lfs-put" || decoded.Hash != expectedHash {
        t.Errorf("decoded=%+v", decoded)
    }
}
```

Add imports: `"bytes"`, `neturl "net/url"`, `"strings"`, `"github.com/bucketvcs/bucketvcs/internal/proxiedurl"`.

Run: `go test ./internal/lfs/ -run TestStore -v` — expect FAIL (WithProxied undefined).

#### Step 2: Extend store.go

In `internal/lfs/store.go`, add four unexported fields to `Store`:

```go
type Store struct {
    backend storage.ObjectStore
    prefix  string

    // Proxied-URL config; zero values disable the ProxiedPutURL/
    // ProxiedGetURL minting (they fall back to returning "", nil — the
    // P1 stub behavior — which Build then surfaces as a per-object 503).
    proxiedKey     []byte
    proxiedBaseURL string
    proxiedTenant  string
    proxiedRepo    string
}
```

Add the builder method:

```go
// WithProxied configures the Store to mint proxied transfer URLs in
// ProxiedPutURL / ProxiedGetURL. Pass an HMAC signing key (>= 16
// bytes), the external base URL of the gateway, and the (tenant,
// repo) pair the Store is scoped to.
//
// Returns the same Store so the call can be chained:
//
//     lfs.NewStore(backend, prefix).WithProxied(key, baseURL, t, r)
//
// If signingKey is empty (zero-length), proxied methods continue to
// return empty URLs — useful for tests that exercise only the presign
// path.
func (s *Store) WithProxied(signingKey []byte, baseURL, tenant, repo string) *Store {
    s.proxiedKey = signingKey
    s.proxiedBaseURL = baseURL
    s.proxiedTenant = tenant
    s.proxiedRepo = repo
    return s
}
```

Implement `ProxiedPutURL`:

```go
// ProxiedPutURL mints a gateway-proxied URL the LFS client uses to PUT
// the object. The returned URL is HMAC-signed via internal/proxiedurl
// and expires after ttl. Returns ("", nil) if WithProxied was not
// called (preserving the P1 stub behavior).
//
// Size is currently informational — passed in so the proxied handler
// can enforce an upper bound at PUT time, but not encoded in the token
// today. The 5 GiB hard cap is applied by the proxied handler via
// http.MaxBytesReader regardless of the size argument.
func (s *Store) ProxiedPutURL(oid string, size int64, ttl time.Duration) (string, http.Header) {
    _ = size // reserved for future Content-Length-bound signing
    if len(s.proxiedKey) == 0 || s.proxiedBaseURL == "" {
        return "", nil
    }
    hash := s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid
    tok, err := proxiedurl.Mint(s.proxiedKey, "lfs-put", hash, time.Now().Add(ttl))
    if err != nil {
        return "", nil
    }
    u := s.proxiedBaseURL + "/_lfs/" + s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid + "?token=" + tok
    hdr := http.Header{}
    hdr.Set("Content-Type", "application/octet-stream")
    return u, hdr
}
```

Implement `ProxiedGetURL` (similar shape, "lfs-get" kind, no header):

```go
func (s *Store) ProxiedGetURL(oid string, ttl time.Duration) (string, http.Header) {
    if len(s.proxiedKey) == 0 || s.proxiedBaseURL == "" {
        return "", nil
    }
    hash := s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid
    tok, err := proxiedurl.Mint(s.proxiedKey, "lfs-get", hash, time.Now().Add(ttl))
    if err != nil {
        return "", nil
    }
    u := s.proxiedBaseURL + "/_lfs/" + s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid + "?token=" + tok
    return u, nil
}
```

Add `"github.com/bucketvcs/bucketvcs/internal/proxiedurl"` to imports.

Run: `go test ./internal/lfs/` — expect ALL PASS.

#### Step 3: Commit

```bash
git add internal/lfs/store.go internal/lfs/store_test.go
git commit -m "lfs: Store.WithProxied mints HMAC URLs for /_lfs/ proxied transfer"
```

---

### Task 2.3: New proxied object handler

**Files:**
- Create: `internal/lfs/proxied.go`
- Create: `internal/lfs/proxied_test.go`

#### Step 1: Write the failing tests

Create `internal/lfs/proxied_test.go`:

```go
package lfs

import (
    "bytes"
    "context"
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/proxiedurl"
    "github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

const goodOID = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

func newProxiedHandlerForTest(t *testing.T, key []byte) (*httptest.Server, *localfs.Localfs) {
    t.Helper()
    dir := t.TempDir()
    l, err := localfs.Open(dir)
    if err != nil {
        t.Fatalf("localfs.Open: %v", err)
    }
    t.Cleanup(func() { _ = l.Close() })
    h := NewProxiedObjectHandler(ProxiedDeps{
        Store:  l,
        Key:    key,
        Logger: nil,
    })
    return httptest.NewServer(h), l
}

func mintLFSToken(t *testing.T, key []byte, kind, tenant, repo, oid string) string {
    t.Helper()
    tok, err := proxiedurl.Mint(key, kind, tenant+"/"+repo+"/"+oid, time.Now().Add(time.Minute))
    if err != nil {
        t.Fatalf("Mint: %v", err)
    }
    return tok
}

func TestProxiedObjectHandler_PutThenGet(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    srv, l := newProxiedHandlerForTest(t, key)
    defer srv.Close()

    // PUT.
    putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", goodOID)
    putBody := []byte("hello LFS world")
    req, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+goodOID+"?token="+putTok, bytes.NewReader(putBody))
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("PUT: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("PUT status=%d", resp.StatusCode)
    }

    // HEAD via the localfs backend at the canonical LFS key.
    expectedKey := "tenants/acme/repos/foo/lfs/objects/" + goodOID
    meta, err := l.Head(context.Background(), expectedKey)
    if err != nil {
        t.Fatalf("Head: %v", err)
    }
    if meta.Size != int64(len(putBody)) {
        t.Fatalf("Head size=%d, want %d", meta.Size, len(putBody))
    }

    // GET via proxied handler.
    getTok := mintLFSToken(t, key, "lfs-get", "acme", "foo", goodOID)
    getResp, err := http.Get(srv.URL + "/_lfs/acme/foo/" + goodOID + "?token=" + getTok)
    if err != nil {
        t.Fatalf("GET: %v", err)
    }
    defer getResp.Body.Close()
    if getResp.StatusCode != http.StatusOK {
        t.Fatalf("GET status=%d", getResp.StatusCode)
    }
    got, _ := io.ReadAll(getResp.Body)
    if !bytes.Equal(got, putBody) {
        t.Errorf("body bytes differ: got %q want %q", got, putBody)
    }
}

func TestProxiedObjectHandler_RejectsMissingToken(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    srv, _ := newProxiedHandlerForTest(t, key)
    defer srv.Close()

    resp, err := http.Get(srv.URL + "/_lfs/acme/foo/" + goodOID)
    if err != nil {
        t.Fatalf("GET: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusForbidden {
        t.Fatalf("status=%d, want 403", resp.StatusCode)
    }
}

func TestProxiedObjectHandler_RejectsKindMismatch(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    srv, _ := newProxiedHandlerForTest(t, key)
    defer srv.Close()

    // GET with a PUT-kind token.
    putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", goodOID)
    resp, err := http.Get(srv.URL + "/_lfs/acme/foo/" + goodOID + "?token=" + putTok)
    if err != nil {
        t.Fatalf("GET: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusForbidden {
        t.Fatalf("status=%d, want 403", resp.StatusCode)
    }
}

func TestProxiedObjectHandler_RejectsBadOID(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    srv, _ := newProxiedHandlerForTest(t, key)
    defer srv.Close()

    badPaths := []string{
        "/_lfs/acme/foo/short",
        "/_lfs/acme/foo/" + strings.Repeat("Z", 64),
        "/_lfs/acme/foo/../etc/passwd",
        "/_lfs/acme/foo/" + strings.Repeat("a", 64) + "x",
    }
    for _, p := range badPaths {
        // Mint a token with the path's "oid" component anyway — the
        // OID-format reject should fire before token verification.
        resp, err := http.Get(srv.URL + p + "?token=fake")
        if err != nil {
            t.Fatalf("GET %q: %v", p, err)
        }
        resp.Body.Close()
        if resp.StatusCode == http.StatusOK {
            t.Errorf("%q: status=200 want 4xx", p)
        }
    }
}

func TestProxiedObjectHandler_RejectsBadTenantRepo(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    srv, _ := newProxiedHandlerForTest(t, key)
    defer srv.Close()

    badPaths := []string{
        "/_lfs//foo/" + goodOID,           // empty tenant
        "/_lfs/acme//" + goodOID,          // empty repo
        "/_lfs/../foo/" + goodOID,         // traversal in tenant
        "/_lfs/acme/.hidden/" + goodOID,   // leading dot in repo
    }
    for _, p := range badPaths {
        resp, err := http.Get(srv.URL + p + "?token=fake")
        if err != nil {
            t.Fatalf("GET %q: %v", p, err)
        }
        resp.Body.Close()
        if resp.StatusCode == http.StatusOK {
            t.Errorf("%q: status=200 want 4xx", p)
        }
    }
}

func TestProxiedObjectHandler_RejectsPostAndDelete(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    srv, _ := newProxiedHandlerForTest(t, key)
    defer srv.Close()

    for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
        req, _ := http.NewRequest(m, srv.URL+"/_lfs/acme/foo/"+goodOID+"?token=fake", nil)
        resp, err := http.DefaultClient.Do(req)
        if err != nil {
            t.Fatalf("%s: %v", m, err)
        }
        resp.Body.Close()
        if resp.StatusCode != http.StatusMethodNotAllowed {
            t.Errorf("%s: status=%d want 405", m, resp.StatusCode)
        }
    }
}

func TestProxiedObjectHandler_PutAlreadyExistsIsIdempotent(t *testing.T) {
    key := bytes.Repeat([]byte{0xab}, 32)
    srv, _ := newProxiedHandlerForTest(t, key)
    defer srv.Close()

    putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", goodOID)
    body := []byte("dup-test")
    // First PUT.
    req1, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+goodOID+"?token="+putTok, bytes.NewReader(body))
    resp1, _ := http.DefaultClient.Do(req1)
    resp1.Body.Close()
    if resp1.StatusCode != http.StatusOK {
        t.Fatalf("first PUT status=%d", resp1.StatusCode)
    }
    // Second PUT — same body, same key. Must be 200 (idempotent).
    req2, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+goodOID+"?token="+putTok, bytes.NewReader(body))
    resp2, _ := http.DefaultClient.Do(req2)
    resp2.Body.Close()
    if resp2.StatusCode != http.StatusOK {
        t.Fatalf("second PUT status=%d, want 200 (idempotent)", resp2.StatusCode)
    }
}
```

Run: `go test ./internal/lfs/ -run TestProxiedObjectHandler -v` — expect FAIL (NewProxiedObjectHandler undefined).

#### Step 2: Implement proxied.go

Create `internal/lfs/proxied.go`:

```go
package lfs

import (
    "context"
    "errors"
    "io"
    "log/slog"
    "net/http"
    "strings"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/proxiedurl"
    "github.com/bucketvcs/bucketvcs/internal/storage"
)

// maxLFSObjectSize bounds the per-PUT body for proxied uploads. 5 GiB
// matches the S3 single-PUT limit referenced in the M13 spec §4 and
// is generous for localfs / Azure / GCS deployments alike.
const maxLFSObjectSize = 5 << 30

// ProxiedDeps is the dependency surface NewProxiedObjectHandler needs.
type ProxiedDeps struct {
    // Store is the underlying object store; LFS object bytes are
    // written via PutIfAbsent and read via Get.
    Store storage.ObjectStore

    // Key is the HMAC signing key shared with Store.WithProxied.
    Key []byte

    // Logger is used for metric + audit emission. Nil falls back to
    // slog.Default().
    Logger *slog.Logger
}

// NewProxiedObjectHandler returns the http.Handler mounted at /_lfs/
// for gateway-proxied LFS object PUT and GET. The handler is the
// terminal owner of the request — no upstream auth runs; the token
// is the authorization.
func NewProxiedObjectHandler(deps ProxiedDeps) http.Handler {
    if deps.Store == nil {
        panic("lfs.NewProxiedObjectHandler: ProxiedDeps.Store is required")
    }
    if len(deps.Key) < 16 {
        panic("lfs.NewProxiedObjectHandler: ProxiedDeps.Key must be >= 16 bytes")
    }
    h := &proxiedObjectHandler{
        store:  deps.Store,
        key:    deps.Key,
        logger: deps.Logger,
        now:    time.Now,
    }
    if h.logger == nil {
        h.logger = slog.Default()
    }
    return h
}

type proxiedObjectHandler struct {
    store  storage.ObjectStore
    key    []byte
    logger *slog.Logger
    now    func() time.Time
}

func (h *proxiedObjectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()

    // Method gate first so unsupported methods take a uniform 405 path
    // regardless of path content.
    switch r.Method {
    case http.MethodPut, http.MethodGet, http.MethodHead:
    default:
        w.Header().Set("Allow", "GET, PUT, HEAD")
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }

    tenant, repo, oid, ok := splitProxiedLFSPath(r.URL.Path)
    if !ok {
        http.NotFound(w, r)
        return
    }

    // Token check BEFORE storage so unauthenticated probes can't
    // distinguish "OID exists" from "OID missing".
    tok := r.URL.Query().Get("token")
    if tok == "" {
        emitMetric(ctx, h.logger, "lfs_object_token_invalid_total", 1, "reason", "missing")
        http.Error(w, "missing token", http.StatusForbidden)
        return
    }
    expectedKind := "lfs-get"
    op := "download"
    if r.Method == http.MethodPut {
        expectedKind = "lfs-put"
        op = "upload"
    }
    expectedHash := tenant + "/" + repo + "/" + oid
    if _, err := proxiedurl.Verify(h.key, tok, expectedKind, expectedHash, h.now()); err != nil {
        reason := "invalid"
        msg := "invalid token"
        switch {
        case errors.Is(err, proxiedurl.ErrTokenExpired):
            reason, msg = "expired", "token expired"
        case errors.Is(err, proxiedurl.ErrKindMismatch):
            reason = "kind_mismatch"
        }
        emitMetric(ctx, h.logger, "lfs_object_token_invalid_total", 1, "reason", reason)
        http.Error(w, msg, http.StatusForbidden)
        return
    }

    storageKey := "tenants/" + tenant + "/repos/" + repo + "/lfs/objects/" + oid

    if r.Method == http.MethodPut {
        h.servePut(ctx, w, r, op, storageKey)
        return
    }
    h.serveGet(ctx, w, r, op, storageKey)
}

// splitProxiedLFSPath parses /_lfs/<tenant>/<repo>/<oid>. Validates
// tenant/repo against routenames.ValidateName and OID against
// validOID.
func splitProxiedLFSPath(p string) (tenant, repo, oid string, ok bool) {
    rest := strings.TrimPrefix(p, "/_lfs/")
    if rest == p { // prefix mismatch
        return "", "", "", false
    }
    parts := strings.Split(rest, "/")
    if len(parts) != 3 {
        return "", "", "", false
    }
    tenant, repo, oid = parts[0], parts[1], parts[2]
    if !validRouteName(tenant) || !validRouteName(repo) {
        return "", "", "", false
    }
    if !validOID.MatchString(oid) {
        return "", "", "", false
    }
    return tenant, repo, oid, true
}

func (h *proxiedObjectHandler) servePut(ctx context.Context, w http.ResponseWriter, r *http.Request, op, key string) {
    body := http.MaxBytesReader(w, r.Body, maxLFSObjectSize)
    defer body.Close()
    _, err := h.store.PutIfAbsent(ctx, key, body, nil)
    if errors.Is(err, storage.ErrAlreadyExists) {
        // Idempotent: same OID, content-addressed, already stored.
        w.WriteHeader(http.StatusOK)
        emitObjectServedMetric(ctx, h.logger, op, "exists")
        return
    }
    var maxErr *http.MaxBytesError
    if errors.As(err, &maxErr) {
        http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
        emitObjectServedMetric(ctx, h.logger, op, "too_large")
        return
    }
    if err != nil {
        http.Error(w, "storage error", http.StatusInternalServerError)
        emitObjectServedMetric(ctx, h.logger, op, "error")
        return
    }
    w.WriteHeader(http.StatusOK)
    emitObjectServedMetric(ctx, h.logger, op, "ok")
}

func (h *proxiedObjectHandler) serveGet(ctx context.Context, w http.ResponseWriter, r *http.Request, op, key string) {
    meta, err := h.store.Head(ctx, key)
    if errors.Is(err, storage.ErrNotFound) {
        http.Error(w, "not found", http.StatusNotFound)
        emitObjectServedMetric(ctx, h.logger, op, "missing")
        return
    }
    if err != nil {
        http.Error(w, "storage error", http.StatusInternalServerError)
        emitObjectServedMetric(ctx, h.logger, op, "error")
        return
    }
    w.Header().Set("Content-Type", "application/octet-stream")
    w.Header().Set("Content-Length", intToString(meta.Size))
    if r.Method == http.MethodHead {
        emitObjectServedMetric(ctx, h.logger, op, "ok")
        return
    }
    obj, err := h.store.Get(ctx, key, nil)
    if err != nil {
        http.Error(w, "storage error", http.StatusInternalServerError)
        emitObjectServedMetric(ctx, h.logger, op, "error")
        return
    }
    defer obj.Body.Close()
    _, _ = io.Copy(w, obj.Body)
    emitObjectServedMetric(ctx, h.logger, op, "ok")
}

func intToString(n int64) string {
    // Avoid importing strconv just for one call site — but actually
    // strconv is already imported transitively in the package; just
    // import it here.
    return strconv.FormatInt(n, 10)
}
```

Add `"strconv"` to imports and remove the `intToString` helper, using `strconv.FormatInt(meta.Size, 10)` directly. (The helper was a placeholder during plan authoring; clean it up.)

Run: `go test ./internal/lfs/ -run TestProxiedObjectHandler -v` — expect ALL PASS.

#### Step 3: Commit

```bash
git add internal/lfs/proxied.go internal/lfs/proxied_test.go
git commit -m "lfs: gateway-proxied LFS object PUT/GET handler at /_lfs/"
```

---

### Task 2.4: Wire /_lfs/ route into gateway + extend NewServer

**Files:**
- Modify: `internal/gateway/server.go`

#### Step 1: Mount the proxied LFS handler

In `internal/gateway/server.go::NewServer`, find the existing LFS handler-construction block (added in P1, around line 300):

```go
if opts.LFSEnabled {
    ttl := opts.LFSPresignTTL
    if ttl <= 0 {
        ttl = 15 * time.Minute
    }
    s.lfsHandler = lfs.NewHTTPHandler(lfs.Deps{
        AuthStore:        opts.AuthStore,
        ActorFromContext: ActorFromContext,
        NewStore: func(tenant, repo string) *lfs.Store {
            return lfs.NewStore(store, repoLFSPrefix(tenant, repo))
        },
        PresignTTL: ttl,
        Logger:     opts.Logger,
    })
}
```

Replace with:

```go
if opts.LFSEnabled {
    ttl := opts.LFSPresignTTL
    if ttl <= 0 {
        ttl = 15 * time.Minute
    }
    // Wire WithProxied so localfs (or any backend lacking native
    // SignedURLs) gets a real proxied URL minted by lfs.Store.
    // Cloud backends with SignedURLs return real signed URLs via
    // Store.PresignPut/PresignGet and never reach the proxied path.
    proxiedKey := opts.ProxiedURLSigningKey
    proxiedBase := opts.ProxiedBaseURL
    s.lfsHandler = lfs.NewHTTPHandler(lfs.Deps{
        AuthStore:        opts.AuthStore,
        ActorFromContext: ActorFromContext,
        NewStore: func(tenant, repo string) *lfs.Store {
            ls := lfs.NewStore(store, repoLFSPrefix(tenant, repo))
            if len(proxiedKey) >= 16 && proxiedBase != "" {
                ls = ls.WithProxied(proxiedKey, proxiedBase, tenant, repo)
            }
            return ls
        },
        PresignTTL: ttl,
        Logger:     opts.Logger,
    })

    // Mount the proxied object handler at /_lfs/ when proxied URL
    // signing is configured. Without a signing key we cannot verify
    // tokens, so the handler is omitted and ProxiedPutURL/GetURL above
    // returns empty URLs (which Build then surfaces as per-object 503).
    if len(proxiedKey) >= 16 {
        s.lfsObjectHandler = lfs.NewProxiedObjectHandler(lfs.ProxiedDeps{
            Store:  store,
            Key:    proxiedKey,
            Logger: opts.Logger,
        })
    }
}
```

Add the `lfsObjectHandler http.Handler` field on `Server`.

Mount the handler on the mux. Find where `/_bundle/` and `/_pack/` are mounted (around server.go:285):

```go
if proxied != nil {
    s.mux.Handle("/_bundle/", proxied)
    s.mux.Handle("/_pack/", proxied)
}
```

After that block, add:

```go
if s.lfsObjectHandler != nil {
    s.mux.Handle("/_lfs/", s.lfsObjectHandler)
}
```

#### Step 2: Build + run tests

```bash
go build ./...
go test ./internal/gateway/ ./internal/lfs/
```

Expected: ALL PASS.

#### Step 3: Commit

```bash
git add internal/gateway/server.go
git commit -m "gateway: mount /_lfs/ proxied handler + wire Store.WithProxied"
```

---

### Task 2.5: Metrics + audit additions

**Files:**
- Modify: `internal/lfs/metrics.go`
- Modify: `internal/lfs/metrics_test.go`
- Modify: `internal/lfs/audit.go`
- Modify: `internal/lfs/audit_test.go`

#### Step 1: Write the failing tests

Append to `metrics_test.go`:

```go
func TestEmitObjectServedMetric_OK(t *testing.T) {
    var buf bytes.Buffer
    emitObjectServedMetric(context.Background(), captureLogger(&buf), "upload", "ok")
    line := buf.String()
    for _, want := range []string{
        `"metric_name":"lfs_object_served_total"`,
        `"op":"upload"`,
        `"result":"ok"`,
    } {
        if !strings.Contains(line, want) {
            t.Errorf("missing %q in %s", want, line)
        }
    }
}

func TestEmitPresignSeconds(t *testing.T) {
    var buf bytes.Buffer
    emitPresignSeconds(context.Background(), captureLogger(&buf), "s3", 42)
    line := buf.String()
    for _, want := range []string{
        `"metric_name":"lfs_presign_seconds"`,
        `"backend":"s3"`,
        `"value":42`,
    } {
        if !strings.Contains(line, want) {
            t.Errorf("missing %q in %s", want, line)
        }
    }
}
```

Append to `audit_test.go`:

```go
func TestEmitLFSObjectServed_Shape(t *testing.T) {
    var buf bytes.Buffer
    emitLFSObjectServed(context.Background(), captureLogger(&buf), "upload", "acme/foo/oid", 1024, 200)
    line := buf.String()
    for _, want := range []string{
        `"msg":"lfs.object.served"`,
        `"event":"lfs.object.served"`,
        `"op":"upload"`,
        `"hash":"acme/foo/oid"`,
        `"bytes":1024`,
        `"status":200`,
    } {
        if !strings.Contains(line, want) {
            t.Errorf("missing %q in %s", want, line)
        }
    }
}
```

#### Step 2: Add the emitters

Append to `internal/lfs/metrics.go`:

```go
// emitObjectServedMetric increments lfs_object_served_total{op,result}.
// Emitted once per /_lfs/ PUT or GET request. op is "upload" or
// "download". result is one of: "ok", "exists", "missing", "too_large",
// "error".
func emitObjectServedMetric(ctx context.Context, logger *slog.Logger, op, result string) {
    emitMetric(ctx, logger, "lfs_object_served_total", 1,
        "op", op,
        "result", result,
    )
}

// emitPresignSeconds records the duration of a single signed-URL mint
// operation. Backend identifies the storage adapter ("s3", "gcs",
// "azureblob", "localfs_proxied"). value is in milliseconds (int64
// like other duration metrics in this project — fractional seconds
// would require a separate histogram pipeline we don't have).
func emitPresignSeconds(ctx context.Context, logger *slog.Logger, backend string, ms int64) {
    emitMetric(ctx, logger, "lfs_presign_seconds", ms,
        "backend", backend,
    )
}
```

Append to `internal/lfs/audit.go`:

```go
// emitLFSObjectServed records a "lfs.object.served" audit event after
// a /_lfs/ PUT or GET completes. Matches the M11 proxied.url.served
// audit shape.
//
// hash is the token's hash field (<tenant>/<repo>/<oid>); bytes is
// the response body byte count (PUT: input bytes; GET: output bytes);
// status is the HTTP status returned.
func emitLFSObjectServed(ctx context.Context, logger *slog.Logger, op, hash string, bytes int64, status int) {
    if logger == nil {
        logger = slog.Default()
    }
    logger.LogAttrs(ctx, slog.LevelInfo, "lfs.object.served",
        slog.String("event", "lfs.object.served"),
        slog.String("op", op),
        slog.String("hash", hash),
        slog.Int64("bytes", bytes),
        slog.Int("status", status),
    )
}
```

#### Step 3: Wire emitLFSObjectServed into proxied.go

In `internal/lfs/proxied.go`, replace each `emitObjectServedMetric` call site with also calling `emitLFSObjectServed`. The PUT/GET handlers should look like:

```go
// (Inside servePut, on the success path:)
w.WriteHeader(http.StatusOK)
emitObjectServedMetric(ctx, h.logger, op, "ok")
emitLFSObjectServed(ctx, h.logger, op, tenant+"/"+repo+"/"+oid, putBytes, http.StatusOK)
```

This requires plumbing `tenant`, `repo`, `oid` (and a byte counter) into servePut and serveGet. Simplest: change the function signatures to take a `hash string` parameter (built once in ServeHTTP), and use a `countingWriter` for GET / count `r.ContentLength` (or read body bytes) for PUT.

Actually, the simplest approach is to NOT thread bytes through and just emit the audit with bytes=0 on PUT (we don't know exactly without buffering the body, and ContentLength may be -1 for chunked encoding). For GET, wrap the writer with a counter and emit on completion.

For PUT, use `io.Copy`'s byte count from PutIfAbsent — but PutIfAbsent doesn't return bytes-written. Alternative: wrap the body in a counter reader before passing to PutIfAbsent.

Either approach is correct. The plan recommends:
- PUT: wrap body in a `countingReader`; emit `bytes=counter.n` on completion.
- GET: wrap response writer in a `countingWriter`; emit `bytes=counter.n` on completion.

Add a small `countingReader` helper to proxied.go (mirrors the existing `countingWriter` in `internal/gateway/proxied_routes.go`).

#### Step 4: Run tests

```bash
go test ./internal/lfs/
```

Expected: PASS.

#### Step 5: Commit

```bash
git add internal/lfs/metrics.go internal/lfs/metrics_test.go internal/lfs/audit.go internal/lfs/audit_test.go internal/lfs/proxied.go
git commit -m "lfs: object_served metric + lfs.object.served audit"
```

---

### Task 2.6: End-to-end integration test

**Files:**
- Create: `internal/lfs/e2e_test.go`

#### Step 1: Write the test

Create `internal/lfs/e2e_test.go`:

```go
package lfs_test

import (
    "bytes"
    "context"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "testing"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/auth"
    "github.com/bucketvcs/bucketvcs/internal/gateway"
    "github.com/bucketvcs/bucketvcs/internal/lfs"
    "github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// fakeAuth used only for the integration test — minimal: one user,
// PermWrite on one repo.
type fakeAuthE2E struct{}

func (f *fakeAuthE2E) GetRepoFlags(context.Context, string, string) (auth.RepoFlags, error) {
    return auth.RepoFlags{}, nil
}
func (f *fakeAuthE2E) LookupRepoPerm(context.Context, *auth.Actor, string, string) (auth.Perm, error) {
    return auth.PermWrite, nil
}
func (f *fakeAuthE2E) VerifyCredential(_ context.Context, c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
    return &auth.Actor{Name: "alice"}, "tok", nil, nil
}
func (f *fakeAuthE2E) TouchTokenUsage(context.Context, string) error                  { return nil }
func (f *fakeAuthE2E) AddSSHKey(context.Context, auth.SSHKey) error                   { return nil }
func (f *fakeAuthE2E) ListSSHKeysForUser(context.Context, string) ([]auth.SSHKey, error) {
    return nil, nil
}
func (f *fakeAuthE2E) ListSSHKeysForRepo(_ context.Context, _, _ string) ([]auth.SSHKey, error) {
    return nil, nil
}
func (f *fakeAuthE2E) RevokeSSHKey(context.Context, string) error                      { return nil }
func (f *fakeAuthE2E) TouchSSHKeyUsage(context.Context, string) error                  { return nil }
func (f *fakeAuthE2E) GetUserByName(context.Context, string) (*auth.User, error)        { return nil, nil }
func (f *fakeAuthE2E) Close() error                                                    { return nil }

func TestE2E_LFS_LocalfsProxiedTransfer(t *testing.T) {
    dir := t.TempDir()
    l, err := localfs.Open(dir)
    if err != nil {
        t.Fatalf("localfs.Open: %v", err)
    }
    defer l.Close()

    // 16-byte HMAC key.
    key := bytes.Repeat([]byte{0xab}, 32)

    srv := httptest.NewServer(nil) // placeholder so we know the URL
    defer srv.Close()
    baseURL := srv.URL

    gw, err := gateway.NewServer(l, gateway.Options{
        AuthStore:            &fakeAuthE2E{},
        LFSEnabled:           true,
        LFSPresignTTL:        time.Minute,
        ProxiedURLSigningKey: key,
        ProxiedBaseURL:       baseURL,
    })
    if err != nil {
        t.Fatalf("NewServer: %v", err)
    }
    defer gw.Close()
    srv.Config.Handler = gw

    // Compute a real LFS OID for the payload.
    payload := []byte("the bunny hops at dawn")
    sum := sha256.Sum256(payload)
    oid := hex.EncodeToString(sum[:])

    // 1. Batch upload — expect upload action with a /_lfs/ URL.
    batchReq, _ := json.Marshal(lfs.BatchRequest{
        Operation: "upload",
        Transfers: []string{"basic"},
        Objects:   []lfs.ObjectRef{{OID: oid, Size: int64(len(payload))}},
    })
    req, _ := http.NewRequest(http.MethodPost, baseURL+"/acme/foo.git/info/lfs/objects/batch", bytes.NewReader(batchReq))
    req.Header.Set("Content-Type", lfs.ContentType)
    req.SetBasicAuth("alice", "pw")
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("batch upload: %v", err)
    }
    if resp.StatusCode != 200 {
        t.Fatalf("batch status=%d", resp.StatusCode)
    }
    var batchResp lfs.BatchResponse
    _ = json.NewDecoder(resp.Body).Decode(&batchResp)
    resp.Body.Close()
    if len(batchResp.Objects) != 1 || batchResp.Objects[0].Error != nil {
        t.Fatalf("batchResp=%+v", batchResp)
    }
    uploadAction, ok := batchResp.Objects[0].Actions["upload"]
    if !ok || uploadAction.Href == "" {
        t.Fatalf("upload action missing or empty href: %+v", batchResp.Objects[0])
    }

    // 2. PUT the payload to the upload URL.
    putReq, _ := http.NewRequest(http.MethodPut, uploadAction.Href, bytes.NewReader(payload))
    if ct := uploadAction.Header["Content-Type"]; ct != "" {
        putReq.Header.Set("Content-Type", ct)
    }
    putResp, err := http.DefaultClient.Do(putReq)
    if err != nil {
        t.Fatalf("PUT: %v", err)
    }
    putResp.Body.Close()
    if putResp.StatusCode != 200 {
        t.Fatalf("PUT status=%d", putResp.StatusCode)
    }

    // 3. Batch download — expect a download action.
    batchReq2, _ := json.Marshal(lfs.BatchRequest{
        Operation: "download",
        Transfers: []string{"basic"},
        Objects:   []lfs.ObjectRef{{OID: oid, Size: int64(len(payload))}},
    })
    req2, _ := http.NewRequest(http.MethodPost, baseURL+"/acme/foo.git/info/lfs/objects/batch", bytes.NewReader(batchReq2))
    req2.Header.Set("Content-Type", lfs.ContentType)
    req2.SetBasicAuth("alice", "pw")
    resp2, err := http.DefaultClient.Do(req2)
    if err != nil {
        t.Fatalf("batch download: %v", err)
    }
    var batchResp2 lfs.BatchResponse
    _ = json.NewDecoder(resp2.Body).Decode(&batchResp2)
    resp2.Body.Close()
    downloadAction := batchResp2.Objects[0].Actions["download"]
    if downloadAction.Href == "" {
        t.Fatalf("download action missing href: %+v", batchResp2.Objects[0])
    }

    // 4. GET via download URL — body must equal payload.
    getResp, err := http.Get(downloadAction.Href)
    if err != nil {
        t.Fatalf("GET: %v", err)
    }
    got, _ := io.ReadAll(getResp.Body)
    getResp.Body.Close()
    if !bytes.Equal(got, payload) {
        t.Fatalf("downloaded bytes differ: got %q want %q", got, payload)
    }
}
```

Note: this test uses `package lfs_test` (external) since it imports gateway. The earlier in-package handler/store/proxied tests use `package lfs`.

#### Step 2: Run

```bash
go test ./internal/lfs/ -run TestE2E -v
```

Expected: PASS.

Also: `go test ./...` for full smoke.

#### Step 3: Commit

```bash
git add internal/lfs/e2e_test.go
git commit -m "lfs: end-to-end batch+proxied transfer integration test"
```

---

### Task 2.7: Update CLI requirements

**Files:**
- Modify: `cmd/bucketvcs/serve.go`

`--lfs=true` with localfs and no `--proxied-url-signing-key` will silently break (the lfsObjectHandler isn't mounted and Build returns 503 per object). Warn the operator at startup.

#### Step 1: Add validation after flag parse

In `cmd/bucketvcs/serve.go`, find the existing startup warning added in P1 (the one that prints under `if *lfsEnabled`). Add an additional check:

After the existing warning:

```go
if *lfsEnabled {
    // ... existing inbound-Authorization-echo warning ...

    // M13 P2: localfs (and any backend without native SignedURLs)
    // requires the gateway-proxied transfer route. The route only
    // mounts when --proxied-url-signing-key and --proxied-url-base
    // are configured. Without them, Batch responses on localfs
    // return per-object 503 errors.
    if *proxiedKeyFile == "" || *proxiedBaseURL == "" {
        fmt.Fprintln(stderr, "serve: --lfs is enabled but --proxied-url-signing-key or --proxied-url-base is not set. LFS will return per-object 503 errors on backends without native SignedURLs (e.g. localfs). Configure these flags or disable LFS with --lfs=false.")
    }
}
```

#### Step 2: Run

```bash
go test ./cmd/bucketvcs/
```

Expected: PASS.

#### Step 3: Commit

```bash
git add cmd/bucketvcs/serve.go
git commit -m "cmd/bucketvcs/serve: warn when --lfs needs proxied-url config"
```

---

### Task 2.8: Phase 2 review + squash

#### Step 1: Run full test suite

```bash
go test ./...
```

Expected: ALL PASS.

#### Step 2: Spec compliance + code quality review (combined subagent)

Dispatch one subagent verifying:
1. proxiedurl supports lfs-put and lfs-get; existing bundle/pack tokens still work.
2. Store.WithProxied mints verifiable tokens; tests assert URL shape AND verifiable round-trip.
3. ProxiedObjectHandler enforces method (PUT/GET/HEAD only), token validity, tenant/repo/oid format.
4. PutIfAbsent on duplicate OID returns 200 (idempotent).
5. Body cap 5 GiB enforced via MaxBytesReader.
6. e2e test exercises full batch → PUT → batch → GET round trip on localfs.
7. CLI startup warning fires when --lfs=true without proxied config.
8. No scope leakage: no verify route (P3), no SSH (P4), no real LFS spec features beyond Phase 1's surface plus Phase 2's localfs proxied transfer.

#### Step 3: roborev-refine

```bash
roborev review --branch --wait
```

Iterate fixes per the M1+ protocol until pass or diminishing returns.

#### Step 4: Squash to main

```bash
cd /home/eran/work/bucketvcs
git checkout main
git merge --squash m13-lfs-p2
git commit -m "$(cat <<'EOF'
M13 Phase 2: localfs proxied LFS transfer

Wires the gateway-proxied transfer path so localfs (and any backend
lacking native SignedURLs) supports stock git-lfs end to end.

- internal/proxiedurl: add lfs-put (kind=3) and lfs-get (kind=4) token
  kinds alongside the existing bundle/pack.
- internal/lfs/store.go: WithProxied builder + ProxiedPutURL/ProxiedGetURL
  mint HMAC-signed URLs at /_lfs/<tenant>/<repo>/<oid>.
- internal/lfs/proxied.go: new gateway handler at /_lfs/ verifying the
  token, parsing tenant/repo/oid, serving PUT (PutIfAbsent, idempotent)
  and GET against the underlying ObjectStore. Method-pinned to GET/PUT/
  HEAD; 5 GiB body cap; OID + tenant/repo validated.
- internal/lfs/{metrics,audit}.go: lfs_object_served_total{op,result},
  lfs_presign_seconds{backend}, event=lfs.object.served audit.
- internal/gateway/server.go: mount /_lfs/ when --proxied-url-signing-key
  + --proxied-url-base are configured; pass WithProxied(...) into
  lfs.NewStore so Build returns real proxied URLs.
- cmd/bucketvcs/serve.go: stderr warning when --lfs=true but proxied URL
  config is absent (LFS would otherwise 503 on localfs).
- e2e_test: full batch → PUT → batch → GET round trip on in-memory
  gateway + localfs.

Reviews: superpowers spec+quality APPROVE. roborev-refine N iterations
clean. M commits squashed.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

#### Step 5: Clean up

```bash
git worktree remove .claude/worktrees/m13-lfs-p2
git branch -D m13-lfs-p2
```

---

## Cross-task notes

- **Token kind enum extension** is a wire-format change to `internal/proxiedurl/token.go`. Existing bundle/pack tokens remain compatible (kind bytes 1 and 2 unchanged). The format is forwards-compatible with future kind additions.
- **Per-object 503 from Phase 1 disappears** for localfs once `WithProxied` is wired in. The TestBuild_ProxiedFallbackEmptyURLBecomesPerObject503 test should still pass (the underlying ProxiedPutURL stub-when-no-WithProxied behavior remains the trigger).
- **Cloud backends bypass the proxied path entirely** in Phase 2. Direct signed URLs from Phase 0 carry the transfer; the proxied handler only exists for localfs (and any future no-signed-URL backend). This matches the spec §5.1/§5.2 wire flows.
- **Verify route is still Phase 3.** The verify action in Batch responses points to a URL that 404s today; this is intentional. Phase 3 lands the handler.
