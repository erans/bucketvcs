# M13.1 — LFS Verify-Token Mechanism Design

**Date:** 2026-05-18
**Predecessor:** M13 (commit `1e12b73`, tag `m13-complete`)

## 1. Scope

Close the only security gap M13 left open: the LFS verify endpoint authenticates by echoing the inbound `Authorization` header from the Batch request into the Batch response's verify action. Under HTTP Basic auth (the only inbound credential the M4 gateway parses), this lands `base64(user:token)` in the Batch response body — captured by any upstream response-body logger.

Replace with an HMAC-signed single-use token bound to `(tenant, repo, oid)` and a short expiry.

The operator guide already documents this as deferred work (§8.5) and the SECURITY block in `internal/lfs/handler.go` is the canonical wording.

## 2. Architecture

```
[Batch upload request]                              [Verify request]
 client ──(HTTPS Basic)──► gateway                  client ──(HTTPS Bearer bvtv_<HMAC>)──► gateway
                          │ /info/lfs/objects/batch                                       │ /_lfs/<t>/<r>/<oid>
                          │ RunAuth (ActionRead→Write)                                    │ mux-mounted, BYPASSES RunAuth
                          ▼                                                               ▼
                          handleBatch                                                     handleVerify (new POST branch)
                          │ mint lfs-put token   (existing)                              │ proxiedurl.Verify(token, kind=5)
                          │ mint lfs-verify tok  (NEW — replaces echo)                    │ assert hash == "<t>/<r>/<oid>"
                          │ embed both in response                                        │ call lfs.Verify(store, oid, size)
                                                                                          │ map 200/404/422/500
```

Token IS the authorization. Permission is implicitly enforced upstream: only an actor with `ActionWrite` on the repo can reach the Batch upload action that mints the token.

## 3. Components and interfaces

### 3.1 `internal/proxiedurl/token.go`

Add kind byte `5` with the string `"lfs-verify"`. The existing payload shape `[kind(1B)] [exp(8B BE)] [hash(rest)]` is unchanged — the new kind reuses it. `Mint()` accepts the new kind without code change beyond the encode/decode switches.

Hash payload for kind=5: `"<tenant>/<repo>/<oid>"` — same shape as `lfs-put` / `lfs-get`. The shared format keeps the cross-kind diff small.

### 3.2 `internal/lfs/store.go`

```go
func (s *Store) ProxiedVerifyURL(oid string, ttl time.Duration) (href string, hdr http.Header)
```

Mirrors `ProxiedPutURL` / `ProxiedGetURL` exactly: mints a kind=5 token, returns `("", nil)` when `WithProxied` was not called. The Href format is the SAME as the upload/download URLs (the same `/_lfs/<t>/<r>/<oid>?token=...`); the method differs (POST). Hdr carries `{"Authorization": "Bearer bvtv_<token>"}` so the client sends the token both as the URL query string AND in the Authorization header — matching the LFS protocol convention.

Actually NO — re-reading: the URL-query token is enough for the proxied PUT/GET path; verify follows that same model. The `Authorization` Header in the action is for the LFS client to replay opaquely; we can put the `Bearer bvtv_...` there but the gateway side reads the token from the URL `?token=` parameter. The header is decorative for the wire — what matters is the URL token. Document this explicitly.

### 3.3 `internal/lfs/proxied.go`

The existing handler at `/_lfs/<tenant>/<repo>/<oid>` switches on `r.Method`:
- `PUT` → existing upload path (token kind=3 `lfs-put`)
- `GET` → existing download path (token kind=4 `lfs-get`)
- **`POST` → NEW verify path (token kind=5 `lfs-verify`)**

Verify branch flow:
1. Validate the URL `?token=` against kind=5 with hash `<tenant>/<repo>/<oid>` (rejects expired, wrong kind, wrong hash).
2. Decode JSON body `{oid, size}` with a 64 KiB cap.
3. Assert `body.oid == url.oid` (defense-in-depth; the token already binds the OID).
4. Call `lfs.Verify(ctx, store, oid, size)` — unchanged from M13 P3.
5. Map result to 200/404/422/500 and emit the existing `lfs_verify_requests_total` + `lfs.verify` audit event. Result labels unchanged.

### 3.4 `internal/lfs/batch.go`

In `processObject`, the upload branch already mints an `lfs-put` token. Add a paired `ProxiedVerifyURL` call and embed its href + header in `actions["verify"]`. Drop the `bearerForVerify` parameter from `Build()` entirely.

For cloud backends with native `SignedPutURL`, the verify URL still needs to mint the kind=5 HMAC token (the verify endpoint always runs through the gateway, regardless of where the actual PUT goes). This means `Build()` needs the verify-URL minter even when the upload presign came from `Store.PresignPut`. The store's `ProxiedVerifyURL` already returns empty when `WithProxied` is not configured; cloud-backed deployments now REQUIRE `--proxied-url-base` + `--proxied-url-signing-key` (previously they could omit these and HTTPS LFS would still work via the inbound-host fallback). Operator guide §3.3 recipe (a) needs updating to reflect this.

### 3.5 Removals

- `internal/lfs/handler.go`: delete `handleVerify`, the `parseLFSPath` `lfsRouteVerify` case, the SECURITY block. Drop the `bearerForVerify := r.Header.Get(...)` line. The handler now serves only `OpLFSBatch`.
- `internal/gateway/routes.go`: remove `OpLFSVerify` constant + ParseRoute case for `.../info/lfs/objects/<oid>/verify`.
- `internal/gateway/server.go`: remove `OpLFSVerify` dispatch arm.
- Tests that exercise the verify route via the HTTP handler move to `proxied_test.go` (same coverage, new wire path).

## 4. Wire format

### 4.1 Token shape

Same `proxiedurl` format as bundle/pack/lfs-put/lfs-get:
- 1 byte: kind = `5`
- 8 bytes: exp (big-endian unix seconds)
- N bytes: hash = `"<tenant>/<repo>/<oid>"`
- 32 bytes: HMAC-SHA256(key, payload)

Token string is base64url(payload || hmac), prefixed with `bvtv_` on the wire for forensics distinguishability:
- `bvts_...` = M4 session token (Basic password)
- `bvtv_...` = M13.1 verify token (Bearer in verify action)

### 4.2 Batch response

`actions["verify"]` carries:
```json
{
  "href":   "https://gw.example/_lfs/<tenant>/<repo>/<oid>?token=<base64url>",
  "header": {"Authorization": "Bearer bvtv_<base64url>"}
}
```

The git-lfs client POSTs to `href` with `header` replayed opaquely + `{oid, size}` body. The gateway validates `?token=` from the URL (the Authorization header is decorative — git-lfs sends it; the gateway reads the URL).

### 4.3 Status codes

- 200 OK — verify succeeded
- 404 Not Found — object missing in storage (or invalid token: the gateway returns 404 to avoid leaking that the token decoded successfully but the OID wasn't found)
- 422 Unprocessable Entity — size mismatch, body OID mismatch, malformed JSON
- 401 Unauthorized — token missing / invalid HMAC / expired / wrong kind (distinct from 404 for missing-object so operators can distinguish token-side failures from upload-side failures in audit)
- 500 Internal Server Error — backend HEAD failure

The split between 401 (token bad) and 404 (object missing) gives operators a clean way to triage in audit. The existing `lfs_verify_requests_total{result}` gains a new `token_invalid` label alongside `ok|missing|size_mismatch|error`.

## 5. Error handling

| Failure | Status | Audit result | Operator action |
|---|---|---|---|
| Missing/expired/wrong-kind token | 401 | `token_invalid` | Re-run Batch; the old token's TTL window closed |
| Body decode error / oid mismatch | 422 | `error` | Client bug; check git-lfs version |
| Object missing in storage | 404 | `missing` | Upload PUT failed silently — check `event=lfs.object.served op=upload` or S3 access logs |
| Size mismatch | 422 | `size_mismatch` | Client claimed wrong size; corruption between client hash and PUT |
| Backend HEAD failure | 500 | `error` | Transient storage issue; retry |
| Verify succeeded | 200 | `ok` | None |

## 6. Observability

The existing metric and audit event names are preserved. The metric label set grows by one (`token_invalid`); the audit event attrs are unchanged.

New emission site: `internal/lfs/proxied.go` `handleVerify` (instead of `internal/lfs/handler.go` `handleVerify`). The metric/audit helpers `EmitLFSVerify` / `emitVerifyRequestMetric` are reused — only the call site moves.

## 7. CLI surface

No new flags. `--proxied-url-signing-key` and `--proxied-url-base` become **required** when `--lfs=true`, regardless of storage backend, because the verify token mint requires them. `bucketvcs serve` should fail-fast (not warn) when LFS is enabled without these flags.

Today's serve.go has the opposite policy: warns when `--lfs=true && --proxied-url-base==""`, lets startup proceed, HTTPS LFS works via inbound-host fallback. With the verify-token mechanism, that fallback is no longer sufficient — the inbound-host trick only works for routing, not for HMAC minting. Operators upgrading need to set both flags.

## 8. Testing

| Layer | Coverage |
|---|---|
| Unit | `proxiedurl.Verify` for kind=5: ok, wrong kind, expired, bad HMAC, hash mismatch |
| Unit | `lfs.Store.ProxiedVerifyURL`: WithProxied-stub returns empty, configured returns valid Bearer |
| Unit | `proxied.go` POST branch: 200/401/404/422/500 paths; body oid mismatch; size mismatch; missing object; backend error |
| Unit | `batch.go` upload-branch embeds verify token with `Bearer bvtv_` prefix; verify Href matches POST URL |
| Integration | Localfs end-to-end: Batch → upload PUT → verify POST → 200 (all on `/_lfs/`) |
| Smoke (localfs) | Existing smoke updates: `event=lfs.verify result=ok` still fires; URL changes are transparent to git-lfs |
| Smoke (MinIO) | Existing smoke updates: same negative-marker logic (no `event=lfs.object.served`); verify works on a kind=5 token even though upload was direct via S3 |

## 9. Phase decomposition

Single phase, single squash. Branch `m13-lfs-verify-token`.

- T0. Worktree
- T1. `proxiedurl` kind=5 + tests
- T2. `lfs.Store.ProxiedVerifyURL` + tests
- T3. `internal/lfs/proxied.go` POST=verify branch + tests
- T4. `internal/lfs/batch.go`: mint verify token, drop bearer echo + tests
- T5. Remove old verify route (handler.go, routes.go, gateway server.go) + delete migrated tests
- T6. Serve.go: require `--proxied-url-base` + `--proxied-url-signing-key` when `--lfs=true` (fail-fast)
- T7. Update operator guide §3.3 recipe (a), §5.4, §8.5, §6.1 (token_invalid label)
- T8. Update smoke scripts (no marker changes; just confirm they still pass)
- T9. Spec + code-quality review + roborev-refine
- T10. Squash + tag `m13.1-verify-token` + memory entry

## 10. Deferred work (still)

The remaining M13 deferred list from the operator guide §8 is unchanged:
- Locking API
- Multipart upload
- LFS-aware reachability GC
- Per-tenant byte quotas

The verify-token mechanism is the only §8 item closing in M13.1.

## 11. Risks

- **Cloud-backend operators upgrading without setting `--proxied-url-base`/`--proxied-url-signing-key`** — fail-fast at startup is the explicit mitigation. Operator guide §3.3 recipe (a) updates to make the flags mandatory.
- **In-flight Batch responses pre-rollout** — clients holding a Batch response from the old code path POST to the old verify URL. They get 404 (route removed). git-lfs retries Batch, gets the new URL. No data loss; one wasted round trip.
- **Token replay within TTL** — same property as kind=3/4 today. Documented as acceptable in §11 since the existing tokens have the same model.
