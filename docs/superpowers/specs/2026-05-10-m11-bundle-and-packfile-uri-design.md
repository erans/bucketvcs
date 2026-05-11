# M11 — Bundle URI + packfile URI acceleration (design)

Status: draft for implementation planning
Date: 2026-05-10
Spec sections: §3.1 (eventual protocol goals), §16.3, §16.4, §35
Decomposition row: M11 "Bundle URI + packfile URI + bundle freshness machinery (current/warm/stale/retired states)"
Predecessors: M9 (`docs/superpowers/specs/2026-05-10-m9-background-maintenance-design.md`), M10 (`docs/superpowers/specs/2026-05-10-m10-reachability-compaction-design.md`)

## 0. Goal and scope

M11 adds two related acceleration features to the cold-fetch path established by M3 (gateway), M9 (canonical pack), and M10 (pure-Go negotiation):

1. **Bundle URI** (§16.3, Git protocol v2 `bundle-uri` capability). `bucketvcs maintenance` produces and refreshes a single immutable bundle covering the default-branch tip per repo. The gateway advertises that bundle via the v2 `bundle-uri` command; clients download it directly from object storage (signed URL) or through a gateway-proxied endpoint, then negotiate only the residual.

2. **Packfile URI** (§16.4, Git protocol v2 `packfile-uris` capability). When the planned fetch is exactly "send canonical pack X in full," the gateway advertises pack X's URI in place of inline pack bytes. Same URL-delivery substrate as bundle-uri.

Both features are pure acceleration on top of the post-M10 fetch path: every M11 advertise outcome has a correct fall-through to the standard inline pack response. No change to receive-pack, no change to GC contract, no change to manifest reachability semantics.

The cold-clone SLO this milestone targets: for a freshly-maintained repo where the bundle is `current`, a v2 client supporting `bundle-uri` and `packfile-uris` should perform **zero** mirror materialization on the gateway side and receive **zero** inline pack bytes from the gateway — the client downloads the bundle (via direct signed URL on cloud, or gateway-proxied stream on localfs), gets every commit it asked for, and finishes negotiation with `nack`/`done` matched against the bundle's tip.

What M11 explicitly does **not** ship — designated successor milestones noted:

- Per-release-tag bundles. Belongs with hooks/policy (M14) since "which tags get bundles" is operator policy.
- Rolling-base + incremental bundles for large active repos (kernel-scale workloads). Designated successor; reuses M11's freshness state machine and advertise machinery.
- Mixed pack-uri delivery (URI + inline pack of leftovers in the same response). Designated successor; M11 ships narrow eligibility only.
- Bundle hit-ratio trigger (the third bullet in §16.3's regeneration-trigger list). Wired-but-inert in M11 because §32 does not yet emit a `bundle_hit_ratio` metric over a long-enough window to drive regen.
- "any-mode" bundle-uri (clients pre-configured with `clone.bundleURI` outside the protocol). M11 supports the in-protocol v2 mode only; the URLs we mint are short-lived and not intended for out-of-band publishing.
- Generated-pack URI handoff (`packs/generated/`). M9 confirmed no writer exists today; if/when dynamic packs land, the same Sign-or-proxy substrate generalizes trivially.

## 1. Brainstorming decisions

These choices were made during brainstorming and govern the rest of the spec.

| ID | Question | Choice |
|----|----------|--------|
| Q1 | Scope between bundle-uri and packfile-uri | **Both, full M11.** Ship together; they share the URL-delivery substrate and amortize the new storage-adapter capability. |
| Q2 | Bundle generation pattern | **Default-branch full bundle only.** One bundle per repo covering refs/heads/<default-branch> tip closure. Schema is forward-compatible with rolling-base / release-tag successors. |
| Q3 | Ownership of generation and freshness evaluation | **Maintenance generates; gateway evaluates freshness on-the-fly at advertise time.** Push never touches bundles. Mirrors M9/M10's "heavy work in maintenance, hot path stays clean" pattern. |
| Q4 | URI delivery mechanism | **`SignedGetURL` (existing M0 capability) + gateway-proxied fallback.** Cloud adapters already implement `SignedGetURL`; M11 extends `SignedURLOptions` with optional `ExpectedHash` for integrity binding. Localfs (and any non-signing adapter, reported via `Capabilities.SignedURLs == false`) gets a gateway-served `/_bundle/<hash>` and `/_pack/<hash>` path guarded by an HMAC token. Per-fetch decision: prefer direct signed URL; fall back to proxied. |
| Q5 | Pack-uri eligibility | **Narrow: only when the planned fetch is exactly "send canonical pack X in full."** Mixed delivery deferred. |

Three derived constraints carried forward without an explicit question:

- **Signed-URL TTL << M8 retention window.** M8's design explicitly relies on retention dominance over realistic clone/URL lifetimes. M11's defaults (1h pack TTL, 4h bundle TTL) are bounded well under M8's 168h default retention. The operator guide makes this an explicit invariant; CLI rejects TTL ≥ retention/24.
- **Freshness is computed, not stored.** §16.3's `current`/`warm`/`stale`/`retired` are pure functions of `(current_manifest_version, bundle.covers_manifest_version, age, configured thresholds)`. The manifest's `bundles[]` entries store only the inputs; freshness is decided at advertise time.
- **v2-only.** Bundle-uri and packfile-uris are protocol v2 capabilities. M3 already runs upload-pack on v2 and receive-pack on v0; M11 only advertises these capabilities on the v2 path. Clients negotiating v0 get classic upload-pack behavior unchanged.

## 2. Manifest schema additions

`internal/repo/manifest/body.go` extends the existing `Bundles` slot (currently undefined as a typed field — §7's example shows the JSON shape the spec reserves). M11 introduces:

```go
type Body struct {
    // ... existing fields ...
    Bundles []BundleEntry `json:"bundles,omitempty"`
}

type BundleEntry struct {
    ID                    string `json:"id"`                          // "bundle_<repo>_<version>_<short-hash>"
    Kind                  string `json:"kind"`                        // M11: only "full_default"
    BundleKey             string `json:"bundle_key"`                  // "bundles/<sha256>.bundle"
    SidecarKey            string `json:"sidecar_key"`                 // "bundles/<sha256>.json"
    BundleHash            string `json:"bundle_hash"`                 // sha256 of bundle body
    Ref                   string `json:"ref"`                         // "refs/heads/<default-branch>"
    TipOID                string `json:"tip_oid"`                     // 40-hex SHA-1
    CoversManifestVersion uint64 `json:"covers_manifest_version"`     // body.Version when bundle was generated
    ByteSize              int64  `json:"byte_size"`
    GeneratedAt           string `json:"generated_at"`                // RFC3339 UTC
}
```

`Bundles` is `omitempty`; legacy repos pre-M11 decode cleanly with `Bundles == nil`. M11 only ever writes one entry of `Kind == "full_default"`. The slice shape is preserved for forward-compat (rolling-base / release-tag successors append additional entries with new `Kind` values).

The spec's example `freshness` field is **not** stored. Freshness is computed by the gateway at advertise time; storing it would require eager updates on every threshold crossing, which adds CAS surface area for no protocol gain.

The bundle's content-addressed `BundleKey` lives at `tenants/<t>/repos/<r>/bundles/<sha256>.bundle` (matches §6's reserved layout). The sidecar is a small JSON document mirroring the `BundleEntry` fields plus a SHA-256 trailer of the bundle file; it exists so an out-of-band tool can reconstruct `BundleEntry` if the manifest is lost (M16 territory).

The `Indexes.Reachability.BaseManifest` field already records the manifest version that produced the current `(ObjectMap, CommitGraph)` pair (M10). `BundleEntry.CoversManifestVersion` plays the analogous role for bundles — used for freshness evaluation, never as a storage key.

## 3. Storage adapter SignedGetURL extension

`internal/storage` already exposes the URL-minting capability that M11 needs:

```go
// (existing M0 contract)
type ObjectStore interface {
    // ...
    SignedGetURL(ctx context.Context, key string, opts SignedURLOptions) (string, error)
    Capabilities() Capabilities  // includes .SignedURLs bool
}

type SignedURLOptions struct {
    Expires time.Duration
    Method  string  // typically "GET"
}
```

All four cloud adapters (`s3compat`, `gcs`, `azureblob`, plus R2 via `s3compat`) implement `SignedGetURL` against their SDK presigners. `localfs` returns `ErrNotSupported` and reports `Capabilities.SignedURLs == false`. The existing M0 conformance suite asserts the negative path ("if `Capabilities.SignedURLs == false`, `SignedGetURL` returns `ErrNotSupported`") for every adapter.

M11 extends `SignedURLOptions` with one optional integrity-binding field and adds positive-path conformance:

```go
type SignedURLOptions struct {
    Expires      time.Duration
    Method       string
    ExpectedHash string  // NEW: optional. If non-empty AND adapter supports
                         // server-side integrity headers, the URL binds the
                         // GET to objects whose content matches the hash.
                         // Adapters without integrity-header support ignore.
                         // Hash format: "sha256:<64-hex>".
}
```

Per-adapter `ExpectedHash` handling:

| Adapter | Mechanism | Behavior when `ExpectedHash` unset |
|---------|-----------|-----------------------------------|
| `s3compat` (AWS S3) | `x-amz-checksum-sha256` enforced via `If-Match`-style precondition or post-fetch verification (S3 supports `x-amz-checksum-mode: ENABLED` on GET — adapter sets it when `ExpectedHash` is provided so the response includes the checksum and a 4xx is returned by the SDK on mismatch). | Plain V4 presign, no integrity binding. Behavior unchanged from M0. |
| `s3compat` (Cloudflare R2) | Same as AWS S3 if R2 honors the AWS V4 checksum mode header. If not, the URL is still minted; integrity falls back to retention dominance. | Same as above. |
| `gcs` | `x-goog-hash` is a response header on GET; the adapter sets the SDK's `Content-Hash` precondition when supported, otherwise the URL is unbinder. | Plain V4 SignedURL. |
| `azureblob` | Account SAS supports content-MD5 binding via `rscc`/`rscm`/`rsce` headers; SHA-256 binding is not first-class, so M11's `azureblob` adapter ignores `ExpectedHash` for v1 and the URL relies on retention dominance. | Plain SAS. Behavior unchanged. |
| `localfs` | Returns `ErrNotSupported`. | Same. |

The TTL bound: each cloud adapter has a `Config.PresignDefaultTTL` (already in M0's adapter config) that bounds `Expires`. M11 callers pass per-call TTLs (1h pack, 4h bundle); adapters cap to their own ceiling and return the lower of the two.

The conformance suite gains a new optional factory `RunCapabilitySigning(t, factory)` that runs only when `Capabilities().SignedURLs == true`. It asserts:

- A freshly-minted URL fetches a byte-identical copy of an uploaded test object.
- An expired signature returns 4xx.
- A tampered query string (e.g., flipped one signature byte) returns 4xx.
- TTL clamping: requesting `Expires = config.PresignDefaultTTL * 100` returns a URL whose expiry is `≤ config.MaxSignTTL` (or `PresignDefaultTTL`, whichever the adapter exposes).
- `ExpectedHash` binding (where the adapter supports it): a URL minted with the *correct* expected hash fetches successfully; a URL minted with a *deliberately-wrong* expected hash returns 4xx.

The factory is wired into all four cloud adapters' conformance harnesses; `localfs` skips it (caps reports false).

## 4. Bundle generation in maintenance

`internal/maintenance` gains a `bundle-refresh` phase that runs alongside the existing `repack` and `compact-only` phases. Phase ordering: repack first (produces fresh canonical pack), then bundle-refresh (consumes default-branch tip from the post-repack manifest), then compact-only (already after repack today). This ordering means a single maintenance invocation that does all three phases reuses the materialized mirror once.

### 4.1 Triggers

`maintenance` Phase 0 grows three bundle threshold checks alongside the existing pack and reachability thresholds:

| Flag | Default | Spec | Notes |
|------|---------|------|-------|
| `--bundle-commits` | 100 | §16.3 | Default-branch tip moved by ≥N commits since `BundleEntry.TipOID`. Computed from the M10 `.bvcg` v2 commit graph by walking backward from the current default-branch tip looking for `BundleEntry.TipOID`; the count of commits walked is the delta. If `BundleEntry.TipOID` is not found within `--bundle-commits` of walk depth, treat the delta as infinite (force-push or rewind detected) and regenerate. Bounded walk in pure Go, no mirror needed. |
| `--bundle-age` | 24h | §16.3 | `now - BundleEntry.GeneratedAt`. |
| `--bundle-missing` | implicit | §16.3 | `Bundles == nil` or no entry with `Kind == "full_default"`. |
| `--bundle-hit-ratio` | omitted | §16.3 | Wired-but-inert; needs §32 metric not yet emitted. Documented in operator guide as future work. |

Threshold evaluation is cheap-first: missing → age → commits. The commit-delta walk is O(N) in the tip-to-bundle distance, but bounded by the trigger threshold itself (we abort the walk once N exceeds the threshold and declare "regenerate").

If any trigger fires, Phase `bundle-refresh` runs.

### 4.2 Generation pipeline

1. **Materialize the mirror.** If repack already ran in this maintenance invocation, reuse its bare-repo temp directory. Otherwise materialize via the same code path M9 uses (`internal/mirror.Manager.MaterializeBare(ctx, repoID, manifestSnapshot)`).
2. **Resolve default branch.** Read `HEAD` from the manifest's symbolic-ref table (M3 inline refs §19.1). Fall back to `refs/heads/main` then `refs/heads/master` if `HEAD` is unset (rare for OSS-imported repos).
3. **Generate bundle file.** Invoke `gitcli.BundleCreate(ctx, mirrorPath, bundleTmpPath, ref)` — wraps `git bundle create <bundleTmpPath> <ref>`. This writes a Git v3 bundle with prerequisite chain (none, since we're at refs/heads/<ref> tip closure) and the packfile.
4. **Hash and upload.** Stream the bundle file through SHA-256 to `tenants/<t>/repos/<r>/bundles/<sha256>.bundle` via `ObjectStore.Put` (uses M0's content-addressed write semantics). Build the sidecar JSON, upload to `bundles/<sha256>.json`.
5. **Build `BundleEntry`.** ID format: `bundle_<repo>_<version>_<sha256[:8]>`.
6. **CAS-merge into manifest.** Read current manifest; replace `Bundles` slice contents with the single new entry; CAS-write. On CAS conflict, re-read and re-merge (the new bundle file is content-addressed so the upload is idempotent under retry; only the manifest slot needs re-CAS).
7. **Old bundle file becomes M8-eligible.** No explicit unlink — the moment the previous `BundleEntry` is replaced in the manifest, the old `bundles/<old-sha256>.bundle` and its sidecar become orphan candidates. M8's mark/sweep with retention dominance handles cleanup.

### 4.3 Failure semantics

If `BundleCreate` fails (mirror materialization error, git invocation crash, upload failure), bundle-refresh logs the failure and **continues**. The rest of the maintenance run (repack, compact-only) proceeds. The old bundle entry stays in the manifest until the next successful regeneration; the gateway will continue to advertise it as long as freshness allows, then drop to "stale" and skip advertising.

This is intentionally lossier than M9/M10's failure semantics: bundles are pure acceleration, never required for correctness. A failed bundle-refresh is operationally similar to "we haven't run maintenance in a while" — clients fall through to standard upload-pack.

The maintenance run's exit code is `1` if bundle-refresh fails (consistent with M9/M10), but the failure is per-repo-isolated under `--all-repos`.

### 4.4 Concurrency posture

Two `bucketvcs maintenance` invocations against the same repo can both decide to regenerate. They generate separate bundle files at separate content-addressed keys (different bytes if mirror snapshots differ; identical key if not). One wins the manifest CAS; the other re-reads, finds its bundle either still "fresh enough" or already replaced, and either skips or re-runs Phase 0.

The losing run's bundle file becomes immediately orphan-eligible. Same M8 retention story as M9/M10.

## 5. Gateway: bundle-uri advertise path

`internal/gateway` (and its v2 implementation in `internal/v2proto`) gains a `bundle-uri` capability advertisement and command handler.

### 5.1 Capability advertisement

In the v2 capability advertisement string, append `bundle-uri` (no value parameters). This matches Git's [bundle-uri.txt protocol](https://git-scm.com/docs/bundle-uri) — clients that support bundle-uri see the capability and may issue the `command=bundle-uri` request after `command=ls-refs`.

The capability is advertised whenever:
- The repo's manifest has a `Bundles` entry with `Kind == "full_default"` AND
- That entry passes the freshness check below

If neither condition holds, the capability is omitted. Clients that planned to use bundle-uri will fall through to standard fetch.

### 5.2 Freshness evaluation

When a `command=bundle-uri` request arrives, the gateway:

1. Loads the manifest (cached from ls-refs in the same v2 session).
2. Picks the `Bundles` entry with `Kind == "full_default"` (the only kind M11 writes).
3. Computes freshness:

```text
age = now - bundle.GeneratedAt

if bundle is missing:                                      state = retired
else if NOT is_ancestor(bundle.TipOID, current_tip):       state = stale     # force-push / rewind
else if bundle.TipOID == current_tip:                      state = current
else:
    delta_commits = walk_back(current_tip, until=bundle.TipOID, max=warm_commits)
    if delta_commits <= warm_commits AND age < warm_age:   state = warm
    else:                                                  state = stale
```

`is_ancestor` and `walk_back` both use M10's `.bvcg` v2 commit graph in pure Go. The bounded walk costs at most `warm_commits` parent traversals (default 100), so the per-advertise overhead is negligible. If the walk hits the bound without finding `bundle.TipOID`, treat as `stale`.

Defaults: `warm_commits = 100`, `warm_age = 24h` (configurable via `bucketvcs serve --bundle-warm-commits=N --bundle-warm-age=D`). The "commits" unit matches §16.3 verbatim — a manifest-version count would be a poor proxy because a single push can land many commits.

States:
- `current` / `warm`: advertise the bundle URI.
- `stale` / `retired`: omit `bundle-uri` from the response (still advertise the capability if any bundle exists, but return zero URIs).

### 5.3 URI minting

The gateway picks the URI mode per the configured `--bundle-uri-mode={auto|direct|proxied|off}`:

- `direct`: call `store.SignedGetURL(ctx, BundleKey, SignedURLOptions{Expires: bundleTTL, Method: "GET", ExpectedHash: "sha256:" + BundleHash})`. On `ErrNotSupported`, error out the bundle-uri response (clients fall through to fetch). Operator chose `direct` knowing it requires a signing adapter.
- `proxied`: skip `SignedGetURL`; mint a gateway-proxied URL (see §7).
- `auto` (default): try `SignedGetURL` first; on `ErrNotSupported`, mint proxied URL.
- `off`: do not advertise `bundle-uri` capability at all.

Response body (per Git bundle-uri protocol):

```text
bundle.<bundleID>.uri=<URL>
bundle.<bundleID>.filter=blob:none      # not advertised; we ship full bundles
bundle.<bundleID>.creationToken=<unix-seconds-of-GeneratedAt>
```

Only `uri` and `creationToken` are advertised in M11. `filter` is reserved for partial-clone successors.

### 5.4 §16.3 consistency invariant

Spec: "The gateway MUST NOT advertise a bundle that is inconsistent with the advertised refs."

The freshness check above enforces this directly: any bundle whose `TipOID` is not an ancestor of the currently-advertised default-branch tip is short-circuited to `stale` and omitted from the response, regardless of age or commit-distance threshold. The `is_ancestor` step is the explicit guard; force-push and ref-rewind cases are caught here, not by the soft commit-distance threshold.

A unit test guards this: `TestBundleAdvertise_ForcePushDropsToStale` simulates a force-push to a divergent tip and asserts the gateway returns `state=stale` and omits the bundle from the next advertise.

## 6. Gateway: packfile-uris advertise path

### 6.1 Capability advertisement

In v2 capability advertisement, append `packfile-uris=https`. Clients opt in by including `packfile-uris` in their fetch arguments. M11 advertises only `https` scheme regardless of the URL we eventually mint (gateway-proxied URLs go to the same gateway HTTPS server).

### 6.2 Plan-shape detection

After M10's negotiation produces a fetch plan, the gateway inspects the plan:

```go
eligible := planResult.LooseObjectCount == 0 &&
            len(planResult.Packs) == 1 &&
            planResult.Packs[0].Kind == "canonical" &&
            planResult.Packs[0].FullPackRequested  // every object in the pack is in the want set
```

`FullPackRequested` is a new flag computed in the M10 plan: true when the want set equals the union of the canonical pack's contents. Cheap to compute from `.bvom` (which already enumerates `(oid, pack_id)` pairs).

If `eligible == false`, the gateway returns a normal inline pack (existing M10 path). No URI is advertised.

If `eligible == true` AND the client opted into `packfile-uris` AND `--pack-uri-mode != off`:

1. Mint a URL for `packs/canonical/<canonical-pack-hash>.pack` via the same Sign-or-proxy substrate (`--pack-uri-mode={auto|direct|proxied|off}`, default `auto`, packTTL default 1h).
2. Emit the `packfile-uris` packet stanza per Git's [packfile-uri.txt protocol](https://git-scm.com/docs/packfile-uri):
   ```text
   packfile-uri=<sha1-of-pack> https <URL>
   ```
   Where `<sha1-of-pack>` is the pack-trailer SHA-1 (Git's pack-checksum, distinct from our content-addressed SHA-256 storage hash). This value is cached in the manifest on a new `Pack.PackChecksum` field (§6.3).
3. Also send a zero-object inline pack (Git protocol requires a packfile section; the URI handoff means it carries no objects).

If the cached `PackChecksum` is missing (legacy pack from pre-M11), fall through to inline pack and log a one-time warning per repo. Maintenance backfills `PackChecksum` on the next repack.

### 6.3 Manifest schema addition for pack checksum

`internal/repo/manifest/body.go` extends `Pack` with one field:

```go
type Pack struct {
    // ... existing fields ...
    PackChecksum string `json:"pack_checksum,omitempty"` // 40-hex SHA-1, the pack trailer; needed for packfile-uri
}
```

`omitempty` for backward compat. M11 amends M9's repack code path to additionally write the field for new canonical packs (cheap — read the trailing 20 bytes after upload). For legacy packs from pre-M11 deployments, M11 maintenance backfills `PackChecksum` lazily on first encounter by reading the pack trailer once and CAS-merging the manifest. Until backfill, pack-uri advertise falls through to inline pack and logs a one-time warning per repo.

## 7. Gateway-proxied URL endpoints

`internal/gateway` adds two HTTP routes to its existing HTTPS server:

```text
GET /_bundle/<sha256>?token=<base64url-token>
GET /_pack/<sha1>?token=<base64url-token>
```

Both implement the same handler shape:

1. Parse the token. Token format: `base64url(json{ key, exp_unix, sig })` where `sig = HMAC-SHA256(signing_key, key || ":" || exp_unix)`. Constant-time comparison of `sig`.
2. Reject if `now > exp_unix`.
3. Reject if the URL path's hash does not match the token's `key` suffix.
4. `ObjectStore.Get(ctx, key)` and stream the body to the response with `Content-Type: application/octet-stream`, `Content-Length`, and (if the storage adapter exposes it) `ETag` headers.
5. Support `Range:` requests for large pack/bundle downloads — required for Git's resumable fetch behavior. Implementation streams from `ObjectStore.GetRange(ctx, key, start, end)` (capability already in M0's adapter contract).

The signing key (`--proxied-url-signing-key=<file>`) is a 32-byte random value loaded once at gateway startup. Operators rotate by stopping the gateway, replacing the file, and restarting (in-flight URLs become invalid; acceptable given short TTLs). A future enhancement could support overlapping key sets for zero-downtime rotation, but M11 ships single-key rotation.

The `_bundle` / `_pack` prefix is reserved (no tenant or repo path uses underscores in M3's URL routing).

The proxied-URL endpoints are NOT exposed over SSH. SSH bundle-uri responses contain HTTPS URLs; clients fetch them out-of-band. Operators running SSH-only deployments must either expose the gateway HTTPS port for proxied URLs OR run on cloud storage with `Sign()` support OR set `--bundle-uri-mode=off` and `--pack-uri-mode=off` (acceleration disabled, correctness intact).

## 8. CLI surface

### 8.1 `bucketvcs maintenance`

New flags:

| Flag | Default | Purpose |
|------|---------|---------|
| `--bundle-commits=<int>` | 100 | Regen trigger: default-branch tip moved by ≥N commits |
| `--bundle-age=<duration>` | 24h | Regen trigger: bundle older than D |
| `--bundle-only` | false | Skip repack and compact phases; only run bundle-refresh |
| `--no-bundle` | false | Skip bundle-refresh; only run repack and compact |
| `--bundle-default-branch=<ref>` | (auto) | Override default-branch detection |

Existing behavior unchanged; bundle-refresh is additive. JSON / text output formats grow a `bundle` object analogous to the existing `repack` and `compact` objects.

Exit codes mirror M9/M10: `0` success / no-op, `1` per-repo failure (in `--all-repos`, isolated failure), `2` invocation error.

### 8.2 `bucketvcs serve`

New flags:

| Flag | Default | Purpose |
|------|---------|---------|
| `--bundle-uri-mode={auto\|direct\|proxied\|off}` | `auto` | Bundle-uri delivery mode |
| `--pack-uri-mode={auto\|direct\|proxied\|off}` | `auto` | Pack-uri delivery mode |
| `--proxied-url-signing-key=<file>` | (required if any `auto`/`proxied`) | 32-byte HMAC key for gateway-proxied URLs |
| `--proxied-url-pack-ttl=<duration>` | 1h | TTL for proxied/signed pack URLs |
| `--proxied-url-bundle-ttl=<duration>` | 4h | TTL for proxied/signed bundle URLs |
| `--bundle-warm-commits=<int>` | 100 | Freshness threshold: warm if `bundle.TipOID` is reachable from current tip within ≤N commits |
| `--bundle-warm-age=<duration>` | 24h | Freshness threshold: warm if generated within D |

Validation: TTL must be < 1/24 of M8's retention window (sanity check; rejected at startup with clear error if violated). M8's retention defaults to 168h, so the M11 TTL ceiling is 7h — both defaults (1h / 4h) pass.

If any mode is `auto` or `proxied` and `--proxied-url-signing-key` is missing, gateway aborts at startup with a clear error.

If all modes are `off`, the gateway does not advertise `bundle-uri` or `packfile-uris` capabilities at all; behavior is identical to pre-M11.

### 8.3 `bucketvcs gc`

No flag changes. The mark-sweep contract from M8 handles bundle and old-pack cleanup naturally: bundles dropped from the manifest are orphan candidates after retention.

## 9. Cross-milestone touchpoints

### 9.1 M0 — storage conformance

Add `RunCapabilitySigning(t, factory)` to the conformance suite. Skipped on adapters where `Capabilities().SignedURLs == false`. Asserts (per §3):

- Signed URL fetches a byte-identical copy
- Expired signature returns 4xx
- Tampered query string returns 4xx
- TTL clamped to adapter's configured ceiling
- `ExpectedHash` binding (where the adapter supports it): correct hash succeeds, wrong hash returns 4xx

Wired into all four cloud adapters' conformance harnesses (`s3compat`, `gcs`, `azureblob`, R2 via `s3compat`). Skipped on `localfs`.

### 9.2 M2 — manifest schema

- `BundleEntry` struct added (§2 above).
- `Pack.PackChecksum` field added (§6.3 above).
- Both `omitempty`; legacy decode unchanged.

No body schema version bump (additive `omitempty` fields only). The diffharness gains a fixture for "M11 manifest with bundle entry round-trips through `manifest.Body` codec."

### 9.3 M3 / M6 — gateways

- v2 capability advertisement gains `bundle-uri` and `packfile-uris=https` (conditional per §5/§6).
- v2 command dispatcher gains `bundle-uri` handler.
- `internal/gateway` HTTPS server gains `/_bundle/<sha256>` and `/_pack/<sha1>` routes.
- SSH gateway: no new routes (SSH carries the v2 protocol unchanged; URL handoff is out-of-band over HTTPS).

### 9.4 M8 — GC

No contract change. Bundle files at `bundles/<sha256>.bundle` and sidecars at `bundles/<sha256>.json` become first-class GC scope: M8's mark phase walks the manifest's `Bundles[]` slice and marks the referenced keys; sweep reaps unreferenced files after retention.

`internal/gc` mark.go grows two lines: include `body.Bundles[i].BundleKey` and `body.Bundles[i].SidecarKey` in the live set.

The §43.6-style "in-flight signed URL outlives manifest reference" race is resolved by retention dominance, exactly as M8 documented for active sessions.

### 9.5 M9 — maintenance

- `internal/maintenance` package gains a `bundle-refresh` phase (§4 above).
- Phase ordering: repack → bundle-refresh → compact (within a single invocation).
- Mirror materialization from repack is reused for bundle-refresh when both run together.
- The package's `maintenance.Run(ctx, opts) (Result, error)` API gains a `BundleResult` field; existing `RepackResult` and `CompactResult` unchanged.

### 9.6 M10 — reachability

- `internal/reachability` planner gains a `FullPackRequested` flag in its result struct (§6.2 above). Cheap addition: walk the want set against the canonical pack's `.bvom` enumeration.
- M10's "negotiation pre-step + lazy mirror" SLO is preserved; bundle-uri and pack-uri advertisement happen *before* mirror materialization. When both advertisements succeed and the client uses them, the gateway never materializes the mirror at all.

## 10. Observability + audit

### 10.1 Metrics

| Metric | Type | Labels |
|--------|------|--------|
| `bundle_generated_total` | counter | `repo_id`, `outcome={success\|failure}` |
| `bundle_generation_duration_seconds` | histogram | `repo_id` |
| `bundle_advertised_total` | counter | `repo_id`, `freshness={current\|warm\|stale\|retired}` |
| `bundle_uri_served_total` | counter | `repo_id`, `via={direct\|proxied}` |
| `bundle_uri_served_bytes` | counter | `repo_id`, `via={direct\|proxied}` |
| `bundle_hit_ratio` | gauge | `repo_id` (rolling window: served / advertised) |
| `pack_uri_advertised_total` | counter | `repo_id` |
| `pack_uri_served_total` | counter | `repo_id`, `via={direct\|proxied}` |
| `pack_uri_served_bytes` | counter | `repo_id`, `via={direct\|proxied}` |
| `proxied_url_token_invalid_total` | counter | `reason={expired\|sig_mismatch\|key_mismatch\|malformed}` |
| `signed_url_minted_total` | counter | `adapter`, `kind={bundle\|pack}` |
| `signed_url_mint_errors_total` | counter | `adapter`, `error_class` |

### 10.2 Audit events (§31)

| Event | Fields |
|-------|--------|
| `bundle.generated` | `repo_id`, `bundle_id`, `bundle_hash`, `tip_oid`, `covers_manifest_version`, `byte_size`, `duration_ms` |
| `bundle.retired` | `repo_id`, `bundle_id`, `reason={replaced\|stale\|gc_swept}` |
| `bundle.uri.advertised` | `repo_id`, `bundle_id`, `freshness`, `via`, `actor_id`, `session_id` |
| `pack.uri.advertised` | `repo_id`, `pack_hash`, `via`, `actor_id`, `session_id` |
| `proxied.url.served` | `repo_id`, `kind={bundle\|pack}`, `key`, `bytes`, `actor_id` (if request bears auth) |

Audit emission consistent with M8/M9/M10 conventions: structured logs to the configured sink; no audit on request *failure* (proxied-URL token mismatch counted as a metric only).

## 11. Testing plan

### 11.1 Unit

- `internal/storage/{s3compat,gcs,azureblob}` `SignedGetURL` extension tests — `ExpectedHash` binding (success and mismatch); TTL clamping; integrity-header propagation; error class mapping.
- `internal/maintenance/bundle` — Phase 0 trigger evaluation against synthetic manifests; bundle generation against a fixture mirror via `gitcli.BundleCreate`; CAS-merge race against a concurrent push.
- `internal/gateway/v2/bundleuri` — freshness state machine truth table; URI minting fallback (`SignedGetURL` succeeds, returns `ErrNotSupported`, errors otherwise); response stanza format.
- `internal/gateway/v2/packuri` — `FullPackRequested` plan-shape detection; eligibility gating; legacy-pack fallback (missing `PackChecksum`).
- `internal/gateway/proxiedurl` — token mint/verify roundtrip; expired token rejection; tampered-path rejection; range-request streaming.

### 11.2 Differential vs upstream Git

- `git -c protocol.version=2 -c fetch.bundleURI=true clone <url>` against a fresh-bundle repo; assert clone succeeds, asserts upstream Git logs "downloaded bundle" / "applied bundle."
- Same with a `stale` bundle (gateway should omit advertise) — clone succeeds via classic fetch path, no bundle download attempted.
- `git -c protocol.version=2 -c uploadpack.allowFilter=false -c fetch.uriProtocols=https clone <url>` against a single-canonical-pack repo; assert pack URI is advertised and Git fetches it directly.
- Force-push regression: push, generate bundle, force-push to a divergent tip, advertise — assert bundle is *not* advertised.

### 11.3 Conformance

- `RunCapabilitySigning` (per §9.1) wired into all four cloud adapters.
- `RunPropertyBundleSafety` factory in the conformance suite: concurrent maintenance bundle-regen + push + advertise; assert that (a) every advertised bundle's `TipOID` is an ancestor of the advertised default-branch tip at the moment of advertise, and (b) M8 GC after retention reclaims orphaned bundle files. Three sub-tests; the cloud-emulator-required ones are scaffolded with `t.Skip` pending the same concurrent-test harness M10 deferred.

### 11.4 Integration

- End-to-end with localfs adapter: maintenance generates bundle, gateway serves bundle via `/_bundle/<hash>` proxied endpoint, real `git clone` against the gateway succeeds with bundle-uri.
- End-to-end with s3compat against a MinIO emulator: maintenance generates bundle, gateway advertises signed URL pointing to MinIO, real `git clone` succeeds.

## 12. Operator guide

`docs/m11-bundles-operator-guide.md` (to be authored alongside implementation):

- Bundle freshness model — when to expect current/warm/stale, tuning `--bundle-warm-commits` and `--bundle-warm-age`.
- Maintenance scheduling recipes (cron, Kubernetes CronJob, systemd timer), interaction with M9 repack and M10 compact phases. Recommend: run all three together at the same cadence (e.g., hourly) so the materialized mirror is amortized across phases.
- Signed-URL vs proxied-URL tradeoff:
  - Direct signed URL: bandwidth offload, no gateway egress, may bypass operator audit/perimeter controls.
  - Gateway-proxied: every byte traverses the gateway, full audit visibility, no public-bucket exposure.
  - Recommendation matrix: public-internet repos on cloud → direct; private-VPC or audit-strict deployments → proxied; localfs → proxied (only option).
- TTL guidance vs M8 retention:
  - Hard rule: TTL << retention. CLI enforces TTL ≤ retention/24.
  - Default 1h pack TTL / 4h bundle TTL covers any realistic clone duration without paging-in stale content.
- Bandwidth and request-pricing implications (§27 economic context):
  - Direct mode shifts egress from gateway compute to object storage. R2 and S3-compatible providers with low/zero egress dominate the gateway-proxied case here.
  - Proxied mode keeps gateway as the bandwidth bottleneck — operators should plan capacity accordingly.
- Disabling acceleration: set `--bundle-uri-mode=off --pack-uri-mode=off`. Gateway behavior reverts to pre-M11; M9/M10 paths unchanged.
- Forensics: how to inspect the manifest's `bundles[]`, the `bundle_*` and `pack_uri_*` metrics, and the `bundle.*` / `pack.uri.*` audit events.

## 13. Out-of-scope reminders (designated successors)

| Deferred | Successor |
|----------|-----------|
| Per-release-tag bundles | M14 (hooks/policy: "which tags get bundles") |
| Rolling-base + incremental bundles | Post-M14 large-repo milestone |
| Mixed packfile-uri delivery (URI + inline leftovers) | Same large-repo milestone |
| Bundle hit-ratio trigger | When §32 emits `bundle_hit_ratio` over a meaningful window |
| "any-mode" bundle-uri (out-of-band published URLs) | Hosted product surface, not OSS |
| Generated-pack URI handoff | Pairs with whoever first emits dynamic packs (§16.2) |
| CDN URL minting (vs raw signed URL) | Same Sign() substrate; trivially generalizes |
| Multi-key proxied-URL signing rotation | Operational hardening pass |

## 14. Implementation phasing (preview for plan-writing)

The plan-writing skill will turn this into ~10–14 phases. High-level slicing:

1. Manifest schema additions + JSON codec tests
2. Storage adapter `SignedURLOptions.ExpectedHash` extension + per-adapter implementation + positive-path conformance (`RunCapabilitySigning`)
3. `gitcli.BundleCreate` wrapper + maintenance Phase `bundle-refresh` (no gateway integration yet)
4. Maintenance CLI flags + JSON output
5. Gateway-proxied URL endpoints + HMAC token + range streaming
6. v2 `bundle-uri` capability advertisement + freshness state machine + handler
7. v2 `packfile-uris` capability + `FullPackRequested` plan-shape detection + handler
8. `Pack.PackChecksum` backfill in maintenance
9. M8 GC mark-phase extension for bundle keys
10. Differential tests vs upstream Git (bundle-uri + packfile-uri clone scenarios)
11. Conformance: `RunCapabilitySigning` + `RunPropertyBundleSafety`
12. Metrics + audit events end-to-end
13. Operator guide
14. End-to-end smoke against MinIO and localfs

Detailed task breakdown deferred to plan-writing.
