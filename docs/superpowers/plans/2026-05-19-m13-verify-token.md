# M13.1 — LFS Verify-Token Mechanism Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Replace the `Authorization`-echo on the LFS verify action with an HMAC-signed single-use kind=5 token (`bvtv_`), close the M13 §5.4 security gap, and tag `m13.1-verify-token`.

**Architecture:** Verify lives at the existing `/_lfs/<tenant>/<repo>/<oid>?token=...` mount; method dispatch (PUT/GET/POST) selects upload/download/verify. The old `.../info/lfs/objects/<oid>/verify` route + `OpLFSVerify` gateway op are removed. Cloud-direct deployments now require `--proxied-url-signing-key` + `--proxied-url-base` because the verify URL needs HMAC minting regardless of where the PUT goes.

**Tech Stack:** Go, standard library + the existing `internal/proxiedurl` HMAC helper. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-05-18-m13-verify-token-design.md` (commit `04d1315`).

---

## File structure

| File | Change |
|---|---|
| `internal/proxiedurl/token.go` | Add kind byte 5 + `"lfs-verify"` case in `encodePayload` / `decodePayload` |
| `internal/proxiedurl/token_test.go` | New: round-trip kind=5 ok, unknown-kind reject |
| `internal/lfs/store.go` | New `ProxiedVerifyURL(oid, ttl) (string, http.Header)` mirroring ProxiedGetURL |
| `internal/lfs/store_test.go` | New tests covering empty-when-unconfigured + valid bvtv_ Bearer |
| `internal/lfs/proxied.go` | New POST-method branch: validate kind=5 token, decode body, call lfs.Verify, map status, emit metric+audit |
| `internal/lfs/proxied_test.go` | New verify tests (happy, token-invalid, missing, size-mismatch, body oid mismatch, backend error) |
| `internal/lfs/batch.go` | Mint verify token via ProxiedVerifyURL; embed in actions["verify"]; drop bearerForVerify param |
| `internal/lfs/batch_test.go` | Update verify-action assertions; drop bearer-echo tests |
| `internal/lfs/handler.go` | Remove handleVerify, lfsRouteVerify case, SECURITY block, bearerForVerify plumbing |
| `internal/lfs/handler_test.go` | Remove TestHandler_Verify_* (moved to proxied_test.go) |
| `internal/lfs/metrics.go` | Add `token_invalid` to documented result label set |
| `internal/gateway/routes.go` | Remove OpLFSVerify constant + ParseRoute case |
| `internal/gateway/routes_test.go` | Remove OpLFSVerify test cases |
| `internal/gateway/server.go` | Remove OpLFSVerify dispatch arm |
| `cmd/bucketvcs/serve.go` | Require --proxied-url-base + --proxied-url-signing-key when --lfs=true (fail-fast, not warn) |
| `docs/m13-lfs-operator-guide.md` | Rewrite §5.4 (gap closed), §8.5 (remove deferred item), §3.3 recipe (a) (flags now mandatory), §6.1 (token_invalid label), §6.2 (new audit emission site) |
| `scripts/m13-lfs-smoke-local.sh` | No grep changes (markers unchanged); confirm still passes |
| `scripts/m13-lfs-smoke-minio.sh` | Same — markers unchanged |
| `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m13_progress.md` | Update — verify-token gap closed |
| `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md` | Add M13.1 entry |

---

## Task 1: Worktree

**Files:** —

- [ ] **Step 1: Clean state on main**

```bash
cd /home/eran/work/bucketvcs
git status
git log --oneline -3
```

Expected: clean on `main`, tip is `04d1315` (the spec commit on top of `1e12b73` M13 P5).

- [ ] **Step 2: Create worktree**

```bash
git worktree add .claude/worktrees/m13-lfs-verify-token -b m13-lfs-verify-token main
cd .claude/worktrees/m13-lfs-verify-token
```

- [ ] **Step 3: Verify**

```bash
git branch --show-current     # m13-lfs-verify-token
go test ./...                 # all green
```

---

## Task 2: `proxiedurl` kind=5 "lfs-verify"

**Files:**
- Modify: `internal/proxiedurl/token.go`
- Modify: `internal/proxiedurl/token_test.go`

#### Step 1: Failing test

Append to `internal/proxiedurl/token_test.go`:

```go
func TestMintAndVerify_LFSVerify(t *testing.T) {
	tok, err := Mint(testKey, "lfs-verify", "acme/foo/abc123", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := Verify(testKey, tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Kind != "lfs-verify" || got.Hash != "acme/foo/abc123" {
		t.Errorf("got %+v", got)
	}
}
```

Run: `go test ./internal/proxiedurl/ -run TestMintAndVerify_LFSVerify -v`
Expected: FAIL (unknown kind).

#### Step 2: Add kind byte 5

Edit `internal/proxiedurl/token.go`:

In `encodePayload` (currently switches on `kind` string to set the byte), add the `"lfs-verify"` case mapping to byte `5`.

In `decodePayload` (switches on the byte), add:

```go
case 5:
	kind = "lfs-verify"
```

In `Mint`'s validation switch (the one that rejects unknown kinds before encoding), add `"lfs-verify"` to the accepted set.

#### Step 3: Run tests

```bash
go test ./internal/proxiedurl/ -v
```

Expected: all pass (existing 4 kinds + new kind=5).

#### Step 4: Commit

```bash
git add internal/proxiedurl/token.go internal/proxiedurl/token_test.go
git commit -m "proxiedurl: add kind=5 lfs-verify"
```

---

## Task 3: `lfs.Store.ProxiedVerifyURL`

**Files:**
- Modify: `internal/lfs/store.go`
- Modify: `internal/lfs/store_test.go`

#### Step 1: Failing test

Append to `internal/lfs/store_test.go`:

```go
func TestStore_WithProxied_VerifyURL(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	s := NewStore(&fakeBackend{}, "tenants/acme/repos/foo/lfs/objects/").
		WithProxied(key, "https://gw.example", "acme", "foo")
	oid := strings.Repeat("a", 64)
	url, hdr := s.ProxiedVerifyURL(oid, time.Minute)
	if !strings.HasPrefix(url, "https://gw.example/_lfs/acme/foo/"+oid+"?token=") {
		t.Errorf("url=%q", url)
	}
	authz := hdr.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer bvtv_") {
		t.Errorf("Authorization=%q", authz)
	}
}

func TestStore_ProxiedVerifyURL_Stub(t *testing.T) {
	s := NewStore(&fakeBackend{}, "p/")
	url, hdr := s.ProxiedVerifyURL("oid", time.Minute)
	if url != "" || hdr != nil {
		t.Errorf("expected empty when WithProxied not called; got url=%q hdr=%v", url, hdr)
	}
}
```

(Adapt `fakeBackend` and import shapes to match the existing `store_test.go` file; mirror `TestStore_WithProxied_GET_URL`.)

Run: `go test ./internal/lfs/ -run TestStore_.*Verify -v`
Expected: FAIL (method undefined).

#### Step 2: Implement `ProxiedVerifyURL`

Add to `internal/lfs/store.go` immediately after `ProxiedGetURL`:

```go
// ProxiedVerifyURL mints a gateway-proxied URL the LFS client POSTs
// to verify an uploaded object. The URL is the SAME as the proxied
// PUT/GET URL — the HTTP method (POST) selects the verify branch in
// the proxied handler. The returned header carries
// "Authorization: Bearer bvtv_<token>" with the same token encoded
// in the URL ?token= parameter, for forensic distinguishability from
// M4 session tokens (bvts_) and for LFS-protocol-convention parity
// with the upload/download actions.
//
// Returns ("", nil) if WithProxied was not called (preserving the
// stub behavior used by tests that exercise only the presign path).
func (s *Store) ProxiedVerifyURL(oid string, ttl time.Duration) (string, http.Header) {
	if len(s.proxiedKey) == 0 || s.proxiedBaseURL == "" {
		return "", nil
	}
	hash := s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid
	tok, err := proxiedurl.Mint(s.proxiedKey, "lfs-verify", hash, time.Now().Add(ttl))
	if err != nil {
		return "", nil
	}
	u := s.proxiedBaseURL + "/_lfs/" + s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid + "?token=" + tok
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer bvtv_"+tok)
	return u, hdr
}
```

Wait — the token returned by `Mint` is already a `bvtv_`-less base64 string. We want the wire token in the URL to NOT carry the `bvtv_` prefix (matching how the URL token is encoded for kind=3/4 — check `ProxiedPutURL` to confirm). The `bvtv_` prefix lives only in the Authorization header for the forensic-distinguishability use case.

Actually let me re-check: looking at the existing `ProxiedPutURL`, the URL has `?token=` plus the raw `Mint()` output, no prefix. The wire-format spec in M13.1 design §4.2 shows the Authorization header carries `Bearer bvtv_<base64url>`. So:
- URL `?token=` = raw token
- Header `Authorization` = `Bearer bvtv_` + raw token

That's what the code above produces. Good.

#### Step 3: Run tests

```bash
go test ./internal/lfs/ -run TestStore_.*Verify -v
```

Expected: 2 PASS.

#### Step 4: Commit

```bash
git add internal/lfs/store.go internal/lfs/store_test.go
git commit -m "lfs: Store.ProxiedVerifyURL mints kind=5 token + bvtv_ Bearer header"
```

---

## Task 4: `internal/lfs/proxied.go` POST=verify branch

**Files:**
- Modify: `internal/lfs/proxied.go`
- Modify: `internal/lfs/proxied_test.go`

#### Step 1: Inspect existing handler

```bash
grep -n "func\|r.Method\|switch.*Method\|ServeHTTP" internal/lfs/proxied.go | head -20
```

Locate where the handler currently dispatches on PUT vs GET. The new POST branch needs to slot in alongside.

#### Step 2: Failing tests

Append to `internal/lfs/proxied_test.go`:

```go
func TestProxied_Verify_OK(t *testing.T) {
	// Setup: storage with an object at known size; mint kind=5 token for
	// (tenant, repo, oid); POST {oid, size} → expect 200.
	// ...
}

func TestProxied_Verify_TokenInvalid(t *testing.T) {
	// POST with no ?token= param → 401 + result=token_invalid metric.
}

func TestProxied_Verify_TokenWrongKind(t *testing.T) {
	// POST with a kind=3 (lfs-put) token in ?token= → 401.
}

func TestProxied_Verify_TokenExpired(t *testing.T) {
	// POST with a kind=5 token whose exp is past → 401.
}

func TestProxied_Verify_TokenHashMismatch(t *testing.T) {
	// POST to /_lfs/acme/foo/<oidA>?token=<kind=5 for acme/foo/oidB> → 401.
}

func TestProxied_Verify_BodyOIDMismatch(t *testing.T) {
	// POST {oid:"other", size:N} to /_lfs/acme/foo/<oid> → 422.
}

func TestProxied_Verify_SizeMismatch(t *testing.T) {
	// Object exists at size 100; POST {oid, size:999} → 422 + result=size_mismatch.
}

func TestProxied_Verify_Missing(t *testing.T) {
	// Object does not exist; POST {oid, size:N} → 404 + result=missing.
}

func TestProxied_Verify_BackendError(t *testing.T) {
	// Fake store Head returns transient error; POST → 500 + result=error.
}
```

Each test mirrors the existing PUT/GET test setup in this file. Use the package-level `slog`-capture helper if one exists (check `proxied_test.go` and `metrics_test.go` for `captureLogger` or similar).

Run: `go test ./internal/lfs/ -run TestProxied_Verify -v`
Expected: ALL FAIL (POST branch unimplemented).

#### Step 3: Add POST branch

In `internal/lfs/proxied.go`, locate the method switch (likely `switch r.Method`) and add:

```go
case http.MethodPost:
	d.handleVerify(w, r, tenant, repo, oid)
```

Implement `handleVerify` as a method on the proxied handler's receiver. Pseudocode (translate to match the existing style):

```go
func (d *proxiedDeps) handleVerify(w http.ResponseWriter, r *http.Request, tenant, repo, oid string) {
	ctx := r.Context()
	logger := d.Logger
	repoFQN := tenant + "/" + repo
	user := "" // verify is token-authenticated; no actor in context

	// 1. Validate token from ?token=
	rawTok := r.URL.Query().Get("token")
	tok, err := proxiedurl.Verify(d.Key, rawTok)
	if err != nil || tok.Kind != "lfs-verify" || tok.Hash != repoFQN+"/"+oid {
		WriteError(w, http.StatusUnauthorized, "invalid or expired verify token")
		emitVerifyRequestMetric(ctx, logger, "token_invalid")
		emitLFSVerify(ctx, logger, repoFQN, user, oid, 0, "token_invalid")
		return
	}

	// 2. Decode body
	body := http.MaxBytesReader(w, r.Body, 64*1024)
	var vreq VerifyRequest
	if err := json.NewDecoder(body).Decode(&vreq); err != nil {
		WriteError(w, http.StatusUnprocessableEntity, "unprocessable: "+err.Error())
		emitVerifyRequestMetric(ctx, logger, "error")
		emitLFSVerify(ctx, logger, repoFQN, user, oid, 0, "error")
		return
	}
	if vreq.OID != oid {
		WriteError(w, http.StatusUnprocessableEntity, "body oid does not match URL oid")
		emitVerifyRequestMetric(ctx, logger, "error")
		emitLFSVerify(ctx, logger, repoFQN, user, oid, vreq.Size, "error")
		return
	}

	// 3. Verify via lfs.Store (the handler must have a Store-builder for the repo).
	store := NewStore(d.Store, RepoLFSPrefix(tenant, repo))
	err = Verify(ctx, store, oid, vreq.Size)
	switch {
	case err == nil:
		w.Header().Set("Content-Type", ContentType)
		w.WriteHeader(http.StatusOK)
		emitVerifyRequestMetric(ctx, logger, "ok")
		emitLFSVerify(ctx, logger, repoFQN, user, oid, vreq.Size, "ok")
	case errors.Is(err, ErrVerifyNotFound):
		WriteError(w, http.StatusNotFound, "object not uploaded")
		emitVerifyRequestMetric(ctx, logger, "missing")
		emitLFSVerify(ctx, logger, repoFQN, user, oid, vreq.Size, "missing")
	case errors.Is(err, ErrVerifySizeMismatch):
		WriteError(w, http.StatusUnprocessableEntity, err.Error())
		emitVerifyRequestMetric(ctx, logger, "size_mismatch")
		emitLFSVerify(ctx, logger, repoFQN, user, oid, vreq.Size, "size_mismatch")
	default:
		WriteError(w, http.StatusInternalServerError, "internal error")
		emitVerifyRequestMetric(ctx, logger, "error")
		emitLFSVerify(ctx, logger, repoFQN, user, oid, vreq.Size, "error")
	}
}
```

Adapt to the actual receiver / dependency-injection shape of `proxied.go`. The Store-builder might already be supplied via `ProxiedDeps.NewStore` from gateway/server.go (check) — reuse rather than re-construct.

#### Step 4: Run tests

```bash
go test ./internal/lfs/ -run TestProxied_Verify -v
go test ./...
```

Expected: 9 new verify tests pass; full suite still green (old HTTP handler-route verify tests are still passing because they haven't been removed yet — that's T6).

#### Step 5: Commit

```bash
git add internal/lfs/proxied.go internal/lfs/proxied_test.go
git commit -m "lfs: proxied handler POST=verify with kind=5 HMAC token"
```

---

## Task 5: `internal/lfs/batch.go` mint verify token; drop echo

**Files:**
- Modify: `internal/lfs/batch.go`
- Modify: `internal/lfs/batch_test.go`

#### Step 1: Drop `bearerForVerify` from `Build`

Change the signature:

```go
// Before
func Build(ctx context.Context, req BatchRequest, store *Store, verifyBaseURL, bearerForVerify string, presignTTL time.Duration) (BatchResponse, error)

// After
func Build(ctx context.Context, req BatchRequest, store *Store, presignTTL time.Duration) (BatchResponse, error)
```

(`verifyBaseURL` also goes away — the verify URL now comes from `store.ProxiedVerifyURL`, which encodes the base URL via the `WithProxied`-stored `proxiedBaseURL`.)

Propagate the signature change through `buildOne` and remove the `verifyHeader` map plumbing.

#### Step 2: Embed verify action via ProxiedVerifyURL

In the upload branch of `buildOne` (currently around `case "upload":`), AFTER the existing `ProxiedPutURL` or `PresignPut` for the upload action, add:

```go
verifyURL, verifyHdr := store.ProxiedVerifyURL(ref.OID, ttl)
if verifyURL == "" {
	out.Error = &ObjectError{
		Code:    503,
		Message: "verify URL unavailable; --proxied-url-signing-key and --proxied-url-base required when --lfs is enabled",
	}
	return out
}
out.Actions = map[string]Action{
	"upload": {Href: uploadURL, Header: headerMap(uploadHdr)},
	"verify": {Href: verifyURL, Header: headerMap(verifyHdr)},
}
```

Both upload and verify actions are now present. The verify action's Href and Authorization Header both carry the kind=5 token (URL token is what the gateway reads; the Bearer header is opaque to the client and decorative on the wire).

#### Step 3: Update tests

`internal/lfs/batch_test.go` currently asserts the verify action's Authorization is the echoed inbound bearer. Change every such assertion to:

```go
authz := obj.Actions["verify"].Header["Authorization"]
if !strings.HasPrefix(authz, "Bearer bvtv_") {
	t.Errorf("verify Authorization=%q, want Bearer bvtv_<...>", authz)
}
hrefURL, _ := url.Parse(obj.Actions["verify"].Href)
if !strings.HasPrefix(hrefURL.Path, "/_lfs/") {
	t.Errorf("verify Href path = %q, want /_lfs/...", hrefURL.Path)
}
if hrefURL.Query().Get("token") == "" {
	t.Errorf("verify Href missing ?token= query param")
}
```

Drop any test that exercises the OLD `bearerForVerify` path (e.g. `TestHandler_Batch_VerifyEchoesAuth` if it exists — search and migrate or delete).

Add a new test asserting the 503 path when the store has no proxied config:

```go
func TestBuild_VerifyURL_RequiresProxiedConfig(t *testing.T) {
	// store WITHOUT WithProxied. Upload Batch → object error 503 with
	// "verify URL unavailable" message.
}
```

#### Step 4: Run tests

```bash
go test ./internal/lfs/ -v
go test ./...
```

Expected: all green.

#### Step 5: Commit

```bash
git add internal/lfs/batch.go internal/lfs/batch_test.go
git commit -m "lfs: Batch embeds kind=5 verify token; drop Authorization-echo plumbing"
```

---

## Task 6: Remove old verify route + SECURITY block

**Files:**
- Modify: `internal/lfs/handler.go`
- Modify: `internal/lfs/handler_test.go`
- Modify: `internal/gateway/routes.go`
- Modify: `internal/gateway/routes_test.go`
- Modify: `internal/gateway/server.go`

#### Step 1: Remove `OpLFSVerify` from gateway routes

In `internal/gateway/routes.go`:
- Delete the `OpLFSVerify` constant (or move it to the end of the enum to avoid renumbering other consumers — find the safest path via `grep -rn "OpLFSVerify" internal/`).
- Delete the `case method == http.MethodPost && ...verify` branch in `ParseRoute`.

In `internal/gateway/routes_test.go`: remove `OpLFSVerify` test cases.

In `internal/gateway/server.go`: remove `OpLFSVerify` from the dispatch switch in routeRepo.

#### Step 2: Remove `handleVerify` + SECURITY block from `handler.go`

- Delete the entire `handleVerify` function body in `internal/lfs/handler.go`.
- Delete the `parseLFSPath` `lfsRouteVerify` case (and the `lfsRouteVerify` enum value if unused).
- Delete the SECURITY block above `bearerForVerify := r.Header.Get(...)` AND that line itself.
- Trim `handleBatch` to no longer pass `bearerForVerify` to `Build` (T5 already changed the signature; this just removes the caller-side reference).

Replace the SECURITY block with a one-line note:

```go
// Verify lives under /_lfs/ with HMAC-token authentication (M13.1).
// This handler serves only OpLFSBatch.
```

#### Step 3: Remove migrated tests from `handler_test.go`

Search `internal/lfs/handler_test.go` for `TestHandler_Verify_*` and delete each (they're covered by `proxied_test.go` after T4). Also remove `TestHandler_Verify_GET_Returns404`, `TestHandler_Verify_RejectsMismatchedContentType` (the proxied POST handler validates differently; coverage is in T4).

#### Step 4: Run tests

```bash
go test ./...
```

Expected: all green. The old verify route is gone; the new one in `/_lfs/` is fully covered by T4.

#### Step 5: Commit

```bash
git add internal/lfs/handler.go internal/lfs/handler_test.go internal/gateway/
git commit -m "lfs, gateway: remove OpLFSVerify route + Authorization-echo SECURITY block"
```

---

## Task 7: Serve.go fail-fast when --lfs without proxied flags

**Files:**
- Modify: `cmd/bucketvcs/serve.go`

#### Step 1: Locate the existing warning

```bash
grep -n "lfs is enabled" cmd/bucketvcs/serve.go
```

Today there's a `fmt.Fprintln(stderr, ...)` warning when `--lfs` is set without both proxied flags. Change it to a hard error.

#### Step 2: Replace warning with fail-fast

Find the existing block (something like):

```go
if *lfsEnabled && (*proxiedKeyFile == "" || *proxiedBaseURL == "") {
	fmt.Fprintln(stderr, "serve: --lfs is enabled but --proxied-url-... is not set...")
}
```

Replace with:

```go
if *lfsEnabled && (*proxiedKeyFile == "" || *proxiedBaseURL == "") {
	fmt.Fprintln(stderr, "serve: --lfs=true requires both --proxied-url-signing-key and --proxied-url-base — the LFS verify action mints HMAC tokens regardless of which backend serves the upload. Set both flags or pass --lfs=false.")
	return 2
}
```

Also delete the stale SECURITY-warning stderr line (the one about `Authorization` echo) — that warning is obsolete now that verify uses HMAC tokens. Keep startup quiet on the happy path.

#### Step 3: Adjust the needsKey conditional

The existing `needsKey` block conditionally loads the signing key when bundle/pack proxied modes OR LFS+proxied flags are set. With LFS now hard-requiring the flags, the `--lfs && proxied-set` branch is redundant but harmless — leave it.

#### Step 4: Tests

```bash
go test ./cmd/bucketvcs/... -v
```

Existing serve tests should pass. If any test exercises `--lfs=true` without the proxied flags expecting success, update it to either provide the flags or expect exit code 2.

#### Step 5: Commit

```bash
git add cmd/bucketvcs/serve.go
git commit -m "serve: fail-fast when --lfs=true without --proxied-url-* (HMAC verify token requires them)"
```

---

## Task 8: Operator guide rewrite

**Files:**
- Modify: `docs/m13-lfs-operator-guide.md`

#### Step 1: Production-readiness preamble (top of file)

Find the line `(6 metrics + 4 audit events)` — verify still 6 (the metric set is unchanged; we only ADD a label value). Audit events: still 4. No change to the count, but update if any wording referenced the verify-token deferral.

In the production-readiness table, change the verify-token row:

```
| Verify-token mechanism | ✅ shipped | M13.1; HMAC kind=5 token in verify action replaces Authorization-echo |
```

(Find the current row referencing verify-token as deferred and flip the status.)

#### Step 2: §3.3 recipe (a) — update flag mandatoriness

The S3/R2 production recipe currently shows `--proxied-url-base` + `--proxied-url-signing-key` as optional. Make them mandatory in the recipe and add a one-sentence note:

```
Note: --proxied-url-signing-key and --proxied-url-base are required
whenever --lfs=true, even on cloud backends, because the verify action
mints HMAC tokens regardless of which backend serves the upload.
```

#### Step 3: §5.4 — gap closed

The current §5.4 documents the Authorization-echo SECURITY caveat. Replace with:

```markdown
### 5.4 The verify-token mechanism (M13.1)

As of M13.1, the verify action carries an HMAC-signed single-use token
of kind=5 ("lfs-verify"), not an echo of the inbound Authorization
header. The token is bound to (tenant, repo, oid) and expires after
--lfs-presign-ttl (default 15m). The git-lfs client POSTs to
/_lfs/<tenant>/<repo>/<oid>?token=... with the token in both the URL
?token= query parameter and the Authorization header; the gateway
validates the URL token.

This closes the response-body credential leak the pre-M13.1 echo
mechanism exposed:
- Client-side persistence — git-lfs caches a kind=5 token in
  .git/lfs that expires in 15 minutes and is scoped to one OID, not
  a long-lived user credential.
- Response-body log exposure — the cached token is the only
  credential in the Batch response body; under capture it expires in
  minutes and grants only verify on a single object.

No operator action is required to enable this — the mechanism is
on whenever --lfs=true.
```

#### Step 4: §6.1 — add `token_invalid` to metric label set

In the row for `lfs_verify_requests_total`, update the result label set to include `token_invalid`. Add a one-line note: "`token_invalid` indicates the kind=5 token was missing, expired, or did not bind to the (tenant, repo, oid) of the request URL."

#### Step 5: §6.2 — update audit emission site

In the `event=lfs.verify` row, change the "Site" reference from `internal/lfs/handler.go handleVerify` to `internal/lfs/proxied.go handleVerify` (POST branch).

#### Step 6: §8.5 — remove deferred-verify-token item

Delete the §8.5 subsection. Renumber any subsequent subsections if needed (probably nothing follows §8.5 — it was the last deferred item). Add a one-line callout in §8 intro:

```markdown
The verify-token mechanism listed here in M13 shipped in M13.1. The
remaining deferred items (locks, multipart, GC, quotas) stand.
```

Wait, that's awkward. Instead: delete §8.5 entirely and add a note at the very top of §8:

```markdown
The verify-token mechanism originally tracked here as deferred shipped
in M13.1 — see §5.4.
```

#### Step 7: Lint

```bash
! grep -nE 'TBD|TODO|FIXME|\?\?\?' docs/m13-lfs-operator-guide.md
```

Expected: exit 0.

#### Step 8: Commit

```bash
git add docs/m13-lfs-operator-guide.md
git commit -m "docs: M13.1 operator guide — verify-token gap closed (§5.4, §3.3, §6, §8)"
```

---

## Task 9: Smoke regression check

**Files:** — (no edits expected)

#### Step 1: Confirm both smokes still pass

```bash
bash scripts/m13-lfs-smoke-local.sh
# expects M13_SMOKE_OK
bash scripts/m13-lfs-smoke-minio.sh
# expects M13_LFS_MINIO_SMOKE_OK
```

If either fails because the verify-event no longer fires on the OLD URL: the smokes grep for `event=lfs.verify` which is emission-site-agnostic, so this should NOT happen. If it does, the new emission site name differs from the old — verify the emitter in `internal/lfs/proxied.go` uses `slog.LogAttrs(... "lfs.verify")` exactly like the old one (via `emitLFSVerify`).

#### Step 2: If updates needed

Edit only the grep patterns or comments; do NOT rewrite the smoke logic. The marker contract is `event=lfs.batch` + `event=lfs.verify` positive, `event=lfs.object.served` negative — none of these change in M13.1.

#### Step 3: Commit (only if changes)

```bash
git add scripts/m13-lfs-*.sh
git commit -m "scripts: smoke regression-check updates for M13.1 verify-token mechanism"
```

---

## Task 10: Review + squash + tag

#### Step 1: Full test suite

```bash
go test ./...
```

Expected: ALL PASS.

#### Step 2: Spec + code-quality review

Dispatch a subagent verifying:

1. `proxiedurl` kind=5 round-trips with `lfs-verify` string label.
2. `Store.ProxiedVerifyURL` returns empty when `WithProxied` not called; valid Bearer + URL token otherwise.
3. Proxied handler POST branch returns 401 for token failures, 404 for missing object, 422 for body/oid/size errors, 500 for backend errors.
4. Batch upload action carries BOTH upload and verify actions. Verify Href is `<proxiedBase>/_lfs/<tenant>/<repo>/<oid>?token=...`. Verify Header has `Bearer bvtv_<token>`.
5. Old `OpLFSVerify` route is GONE from `internal/gateway/routes.go` (no constant, no ParseRoute case).
6. Old `handleVerify` body is GONE from `internal/lfs/handler.go`.
7. Old `bearerForVerify` param is GONE from `Build()` signature.
8. `cmd/bucketvcs/serve.go` exits 2 when `--lfs=true` without both proxied flags (was: warned and continued).
9. Operator guide §5.4 rewritten; §8.5 removed; §3.3 recipe (a) flags marked mandatory; §6.1 includes `token_invalid` label.
10. Both smokes pass end-to-end with no marker changes.

#### Step 3: roborev-refine

```bash
roborev review --branch --wait
```

Iterate per M1+ protocol (max 5 iterations).

#### Step 4: Squash + tag + worktree cleanup

```bash
cd /home/eran/work/bucketvcs
git checkout main
git merge --squash m13-lfs-verify-token
git commit -m "$(cat <<'EOF'
M13.1: LFS verify-token mechanism (closes M13 §5.4 security gap)

Replaces the Authorization-echo on the LFS verify action with an
HMAC-signed single-use kind=5 token bound to (tenant, repo, oid).

- internal/proxiedurl/token.go: add kind=5 lfs-verify.
- internal/lfs/store.go: ProxiedVerifyURL mints kind=5 token and
  Bearer bvtv_<token> header; URL is the same path as upload/download
  (/_lfs/<t>/<r>/<oid>?token=...) with method POST selecting verify.
- internal/lfs/proxied.go: POST=verify branch validates token,
  decodes body, calls Verify(), maps 200/401/404/422/500, emits
  existing metric+audit. New result label "token_invalid".
- internal/lfs/batch.go: upload action now carries both upload and
  verify presigned actions; bearerForVerify plumbing dropped from
  Build() signature.
- internal/lfs/handler.go: handleVerify body removed; SECURITY block
  and Authorization-echo logic gone. Handler serves only OpLFSBatch.
- internal/gateway/{routes.go, server.go}: OpLFSVerify removed.
- cmd/bucketvcs/serve.go: --lfs=true now hard-requires
  --proxied-url-signing-key and --proxied-url-base on ALL backends
  (verify token mint needs them regardless of upload presign path).
  Old warning replaced with exit 2.
- docs/m13-lfs-operator-guide.md: §5.4 rewritten as "gap closed";
  §8.5 deleted; §3.3 recipe (a) flags marked mandatory; §6.1 adds
  token_invalid label; §6.2 emission site updated.

Reviews: superpowers spec+quality APPROVE. roborev-refine N iterations
clean. Both smokes pass end-to-end with no marker changes.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
git tag -a m13.1-verify-token -m "M13.1 LFS verify-token mechanism — closes M13 §5.4 security gap"
git worktree remove .claude/worktrees/m13-lfs-verify-token
git branch -D m13-lfs-verify-token
```

#### Step 5: Memory finalize

Update `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m13_progress.md`:
- Add the M13.1 squash SHA + tag + date.
- Move the verify-token line from "Deferred" to a new "Closed in M13.1" section.

Update `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md`:

```markdown
- [M13.1 verify-token mechanism merged to main](m13_progress.md) — commit <SHA>, tag m13.1-verify-token (2026-05-19); HMAC kind=5 token replaces Authorization-echo; closes M13 §5.4 security gap; --lfs now hard-requires --proxied-url-* on all backends
```

#### Step 6: Final post-squash gate

```bash
go test ./...
bash scripts/m13-lfs-smoke-local.sh
```

Expected: green + `M13_SMOKE_OK`.

---

## Cross-task notes

- The token kind byte stays a single byte; adding more kinds beyond 5 is straightforward (the format reserves 256 distinct values). No format-version bump needed.
- The verify URL Path matches PUT/GET exactly — clients can compute it from the upload Href + change the method, if their LFS implementation does that optimization. Standard git-lfs does not; it uses the verify-action Href as given.
- Failure modes for the gateway not having `--proxied-url-base` set: the operator gets exit 2 at startup with a clear message. No silent degradation, no broken Batch responses, no half-shipped feature.
- The `event=lfs.verify` audit event continues to fire from the new emission site with the same attrs. Operator alerting rules remain valid.
- This phase does NOT touch SSH `git-lfs-authenticate` (M13 P4) — the SSH path mints session tokens for HTTPS auth, which is independent of the verify-token mechanism.
