# M19: Multi-tenant proxied bundle/pack mode

**Status:** Design.
**Date:** 2026-05-24.
**Scope:** Close the M11 deferral: "multi-tenant proxied mode (single-repo only today)". Extend `/_bundle/<hash>` and `/_pack/<hash>` to embed `(tenant, repo)`, bind tokens to the composite, drop the unused `ProxiedKeyResolver` indirection, and **actually wire** the inbound mount in `cmd/bucketvcs/serve.go` (the central bug M19 closes — proxied mode today mints URLs the gateway returns 404 on).

## 1. Goals

### 1.1 In scope

- New URL shape for proxied bundle + pack: `/_bundle/<tenant>/<repo>/<hash>?token=...` and `/_pack/<tenant>/<repo>/<hash>?token=...`. Mirrors the M13 LFS pattern (`/_lfs/<tenant>/<repo>/<oid>`) byte-for-byte.
- Token binding: kinds 1 (bundle) and 2 (pack) are reused; the `hash` field of the token payload becomes the composite string `tenant + "/" + repo + "/" + hash`. Tampering with any of the three path segments fails HMAC verify.
- Drop the `ProxiedKeyResolver` interface. The handler computes storage keys directly via `keys.BundleKey(tenant, repo, hash)` and `keys.CanonicalPackKey(tenant, repo, hash)`. M13 LFS does this; M19 adopts the same pattern.
- Wire `gateway.Options.ProxiedURLSigningKey` in `cmd/bucketvcs/serve.go` (the M11 omission). The `/_bundle/`, `/_pack/` mux mount becomes reachable when the operator sets `--proxied-url-signing-key`.
- Tenant + repo name validation at the inbound handler via the existing `routenames.ValidateName` (same validator the normal git router uses).
- Observability refinement: existing `bundle_uri_served_total` / `pack_uri_served_total` metrics gain `tenant` + `repo` labels; existing `bundle.uri.served` / `pack.uri.served` audit events gain `tenant`/`repo` attrs. No new event names.
- Update `docs/m11-bundles-operator-guide.md` to retract the 5 single-repo caveats and document the new URL shape.
- Smoke script `scripts/m19-multitenant-proxied-smoke.sh` verifying two-tenant URL minting + isolation + cross-tenant tamper rejection.

### 1.2 Out of scope (deferred)

- **Per-tenant signing keys.** Single global key remains; rotation is a serve-wide event. M13 LFS uses the same model.
- **Storage-side key partitioning improvements** beyond what `keys.BundleKey`/`CanonicalPackKey` already do (these are tenant-scoped since M1).
- **Subdomain-based multi-tenancy** (`<tenant>.gw.example.com/...`). Requires wildcard TLS + DNS; diverges from M13 LFS pattern.
- **Cross-tenant deduplication** of identical bundle/pack content. Each tenant gets its own URL + storage key; backend-level dedupe is not exposed at the URL layer.
- **New token kinds** (kinds 6, 7) for the multi-tenant variants. Kinds 1+2 are reused since they are pure dead code in production today (proxied mode has never been wired).
- **CDN-friendly Cache-Control headers** on proxied responses. Content is content-addressed and could safely carry `Cache-Control: public, max-age=<ttl>, immutable`, but emitting them is a separate observability/perf concern.
- **Operator CLI for issuing one-off proxied URLs** outside the protocol-v2 flow.

## 2. Architecture overview

```
internal/proxiedurl/token.go               (no signature change; doc-only update describing the new
                                            "tenant/repo/hash" composite for kinds 1+2)

internal/gateway/proxied_url_builder.go    (modified) — buildURL takes (tenant, repo, hash);
                                            produces /_bundle/<t>/<r>/<h>; encodes "<t>/<r>/<h>"
                                            into token hash field

internal/gateway/proxied_routes.go         (modified) — drop ProxiedKeyResolver; ServeHTTP parses
                                            3-segment path; validates names via routenames; computes
                                            storage key via keys.BundleKey / CanonicalPackKey

internal/gateway/server.go                 (modified) — Options drops ProxiedKeyResolver field;
                                            mount gate becomes ProxiedURLSigningKey alone

internal/gateway/observability.go          (modified) — bundle_uri_served_*/pack_uri_served_* gain
                                            tenant+repo labels; audit attrs gain tenant+repo

internal/uploadpack/bundleuri.go           (modified) — URLBuilder.Bundle/Pack callers updated
                                            to pass (tenant, repo) through

cmd/bucketvcs/serve.go                     (modified) — populate gateway.Options.ProxiedURLSigningKey;
                                            delete the "not wired" NOTE comment block

docs/m11-bundles-operator-guide.md         (modified) — retract single-repo caveats at L20/L277/
                                            L408/L488/L909; document the new URL shape; example
                                            URLs use /_bundle/acme/site/<hash>

scripts/m19-multitenant-proxied-smoke.sh   (new) — two-tenant end-to-end smoke
```

**Sequence (clone with `--bundle-uri-mode=proxied` enabled):**
1. Client opens protocol-v2 negotiation, sends `bundle-uri` capability.
2. Server runs `bundleuri.GenerateAdvertisement(ctx, tenant, repo, ...)` which calls `URLBuilder.Bundle(tenant, repo, hash, expectedHash) string`.
3. `URLBuilder.Bundle` mints a token with `kind=1`, hash field `=tenant+"/"+repo+"/"+hash`, and returns `<baseURL>/_bundle/<tenant>/<repo>/<hash>?token=...`.
4. Client makes a GET request to that URL. The gateway's `/_bundle/` handler parses the 3 path segments, validates tenant/repo via `routenames.ValidateName`, verifies the token's HMAC + expiry + binding to the literal `tenant/repo/hash` composite, computes `key := keys.BundleKey(tenant, repo, hash)`, and streams the object via `ObjectStore.Get(ctx, key)`.
5. Range requests, audit emission, metrics increment unchanged from M11 except for the new `tenant`/`repo` labels and attrs.

**Wiring at startup (`cmd/bucketvcs/serve.go`):**

Today (M11–M18) `gateway.Options{}` is built without `ProxiedURLSigningKey`, so the `/_bundle/`, `/_pack/` mux mount at `server.go:348-354` is unconditionally skipped. M19 changes:

```go
gateway.Options{
    // ... existing fields ...
    ProxiedURLSigningKey: proxiedKey, // <-- new, M19
    // ProxiedKeyResolver field is deleted
}
```

The mount gate becomes `if opts.ProxiedURLSigningKey != nil`. The NOTE comment block at `cmd/bucketvcs/serve.go:227-261` is deleted.

## 3. URL + token contract

### 3.1 URL shape

```
Bundle: <baseURL>/_bundle/<tenant>/<repo>/<hash>?token=<base64url>
Pack:   <baseURL>/_pack/<tenant>/<repo>/<hash>?token=<base64url>
```

- `<baseURL>` = `--proxied-url-base` operator flag, e.g. `https://gw.example.com`
- `<tenant>`, `<repo>` validated by `routenames.ValidateName` at mint and verify time (rejected paths return 400 without store lookup)
- `<hash>` for bundle = sha256-prefixed pack hash (e.g. `sha256-aabbcc...`, 64 hex), for pack = 40-hex canonical pack hash
- `<token>` = base64url(payload + HMAC-SHA256(key, payload)), payload = `[kind:1B][exp_unix:8B BE][hash_string:rest]`

### 3.2 Token binding

| Kind | Description | Hash field |
| --- | --- | --- |
| 1 | Bundle | `<tenant>/<repo>/<hash>` |
| 2 | Pack | `<tenant>/<repo>/<hash>` |
| 3 | LFS GET (existing M13) | `<tenant>/<repo>/<oid>` |
| 4 | LFS PUT (existing M13) | `<tenant>/<repo>/<oid>` |
| 5 | LFS verify (existing M13.1) | `<tenant>/<repo>/<oid>` |

Kinds 1+2 reuse the existing wire-format slot. Pre-M19 builds never minted a URL a real client received because the proxied mount was never reachable; the in-code wire format is broken without operator visibility.

### 3.3 HMAC verify

The handler reconstructs the expected hash string from URL path segments:

```go
expected := tenant + "/" + repo + "/" + hash
ok := proxiedurl.Verify(key, tokenBytes, kind, expected, now)
```

Any tampering — swapping tenant, swapping repo, swapping hash — produces a different `expected` and the HMAC fails. There is no need for a separate "tenant-binding" check.

## 4. Failure modes

Status codes are deliberately uniform across "object missing" and "bad token"
so a probing attacker cannot distinguish "the (tenant, repo) exists and the
object is there but my token is wrong" from "the (tenant, repo) doesn't
exist." This is M11's anti-enumeration design preserved by M19.

| Failure | Response |
| --- | --- |
| URL has < 3 path segments after `/_bundle/` or `/_pack/` | 404 Not Found |
| Tenant or repo segment fails `routenames.ValidateName` | 404 Not Found, no store lookup |
| Hash segment fails format check (bundle: sha256-prefixed; pack: 40 hex) | 404 Not Found |
| Token missing | 403 Forbidden, metric `proxied_url_token_invalid_total{reason=missing}` |
| Token signature invalid (any path-segment tamper, wrong key) | 403 Forbidden, metric `reason=invalid` |
| Token expired | 403 Forbidden, metric `reason=expired` |
| Token kind mismatch (bundle token used on pack endpoint) | 403 Forbidden (body "invalid token" collapses the distinction), metric `reason=kind_mismatch` |
| HMAC verifies, but `ObjectStore.Get(keys.BundleKey/CanonicalPackKey)` returns ErrNotExist | 404 Not Found |
| ObjectStore returns transient error | 500 Internal Server Error |
| `ProxiedURLSigningKey` unset at startup, operator sets `--bundle-uri-mode=proxied` | `NewServer` returns an error and `bucketvcs serve` exits non-zero with "gateway: BundleURIMode=proxied requires ProxiedURLSigningKey and ProxiedBaseURL". (`URIModeAuto`/`URIModeDirect` without proxied fallback do NOT fail-fast — they emit a startup WARN and degrade to empty bundle-uri advertisement so clients fall back to a normal fetch.) |
| Cross-tenant URL swap (mint for `acme/r1`, request `other/r1`) | 403 Forbidden (HMAC binds all three segments) |
| Same hash exists in two tenants' storage | Each tenant gets its own URL + token; treated as independent objects at the URL layer |

## 5. Observability

### 5.1 Metrics (modified)

Existing counters from M11 gain two labels:

```
bundle_uri_served_total{outcome, tenant, repo}     // outcomes unchanged: ok|expired|forbidden|notfound|error
pack_uri_served_total{outcome, tenant, repo}
```

`outcome` enum is unchanged; the new labels enable per-tenant dashboards.

### 5.2 Audit (modified)

The two confirmed M11 events gain attrs:

| Event | New attrs |
| --- | --- |
| `bundle.uri.served` | `tenant`, `repo` |
| `pack.uri.served` | `tenant`, `repo` |

If the M11 implementation also emits separate `proxied.url.expired` / `proxied.url.bad_signature` events (the implementer should confirm during Task 0), they receive the same `tenant`/`repo` attrs on a best-effort basis (parsed from URL pre-HMAC, may be empty on garbage input). No new event names.

## 6. Testing

### 6.1 Unit

`internal/gateway/proxied_url_builder_test.go`:
- Bundle URL contains `/_bundle/<tenant>/<repo>/<hash>` exactly
- Pack URL contains `/_pack/<tenant>/<repo>/<hash>` exactly
- Token hash field equals `tenant + "/" + repo + "/" + hash`
- Round-trip: builder mint → verify with same key + composite succeeds; verify with wrong tenant/repo/hash fails

`internal/gateway/proxied_routes_test.go`:
- Happy path: well-formed bundle URL + token serves the object bytes
- Tampered tenant → 403
- Tampered repo → 403
- Tampered hash → 403
- Cross-tenant swap (mint for acme/r1, request other/r1, valid token from acme/r1) → 403
- Tenant fails `routenames.ValidateName` (`..`, empty, length cap) → 404
- Repo same → 404
- Hash fails format (bundle non-sha256-prefix, pack non-40-hex) → 404
- Missing path segments (`/_bundle/acme/r1` without hash) → 404
- Token expired → 403, metric `proxied_url_token_invalid_total{reason=expired}`
- Storage miss (verifies but object absent) → 404
- Pack equivalents for each of the above

`internal/proxiedurl/token_test.go`:
- Add an explicit test that the hash field with embedded slashes (`acme/r1/sha256-aabbcc...`) round-trips through mint+verify

### 6.2 Integration

End-to-end multi-tenant integration is covered by the smoke script in §6.3 (two real tenants, real gateway, real client). A separate `internal/gateway/proxied_integration_test.go` was considered but rejected as duplicating smoke coverage at higher cost; the unit tests in §6.1 (especially the cross-tenant tamper case) plus the smoke are sufficient.

### 6.3 Smoke

`scripts/m19-multitenant-proxied-smoke.sh`:
1. Spin up localfs-backed serve with `--bundle-uri-mode=proxied --pack-uri-mode=proxied --proxied-url-signing-key=<base64> --proxied-url-base=http://127.0.0.1:<PORT>`
2. Register tenants `acme` + `other`; create repo `r1` in each; push a small repo to each
3. Trigger maintenance to materialize a bundle for each repo
4. Curl protocol-v2 ls-remote with bundle-uri; assert the advertised URL contains `/_bundle/acme/r1/` for acme and `/_bundle/other/r1/` for other
5. Curl both URLs; assert 200 + non-empty body
6. Swap the tenant segment in acme's URL to `other` keeping the same token; assert 403
7. Swap the repo segment within tenant; assert 403
8. Assert `bundle.uri.served` audit lines mention both `tenant=acme` and `tenant=other` in serve.log
9. Echo `M19_MULTITENANT_PROXIED_SMOKE_OK`

## 7. Acceptance criteria

- Unit tests pass for all cases in §6.1
- Integration test in §6.2 passes
- Smoke in §6.3 passes
- All prior smokes (M11/M12/M12.1/M13/M13.3/M13.4/M13.5/M14/M15/M15.1/M16/M17/M18) pass unmodified
- `bucketvcs serve --bundle-uri-mode=proxied --proxied-url-signing-key=...` actually serves `/_bundle/<t>/<r>/<h>` (the central M11 bug closed)
- `docs/m11-bundles-operator-guide.md` no longer mentions "single-repo only" or "multi-tenant deferred"; example URLs use the new shape
- `gateway.Options.ProxiedKeyResolver` is removed (compile-time enforcement that no caller still threads it)

## 8. Open questions

None — all decisions captured above.
