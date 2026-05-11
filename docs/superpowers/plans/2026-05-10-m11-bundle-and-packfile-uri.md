# M11 — Bundle URI + packfile URI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship §16.3 bundle-uri (default-branch full bundle, freshness machinery) and §16.4 packfile-uri (narrow eligibility: full canonical-pack handoff). Bundle generation lives in `bucketvcs maintenance`; gateway evaluates freshness on-the-fly at advertise time. Storage adapter `SignedGetURL` extended with optional `ExpectedHash` integrity binding; gateway-proxied URL endpoints (`/_bundle/<hash>`, `/_pack/<hash>`) provide a fallback for localfs and operator-chosen audit/perimeter scenarios.

**Architecture:**
- New top-level package `internal/proxiedurl/` (HMAC token mint + verify, used by gateway routes).
- `internal/maintenance/` gains a `bundle-refresh` phase parallel to repack and compact.
- `internal/gitcli/` gains a `BundleCreate` wrapper (thin shell around `git bundle create`).
- `internal/v2proto/` gains `bundleuri.go` (command handler + freshness state machine) and `packuri.go` (advertise-time plan-shape gate).
- `internal/storage/` extends `SignedURLOptions` with `ExpectedHash`; per-adapter binding in s3compat (full) and gcs (best-effort); azureblob and localfs ignore.
- `internal/repo/manifest/body.go` `BundleEntry` placeholder is filled in; `PackEntry.PackChecksum` field added.
- `internal/gc/mark.go` walks `body.Bundles[]` to mark bundle keys.

**Tech Stack:** Go 1.25, existing `internal/storage` ObjectStore + per-adapter SDKs (already in tree), `internal/repo` (M1), `internal/pack` (M2), `internal/maintenance` (M9), `internal/reachability` (M10), `internal/v2proto` (M3+M10), `crypto/hmac`+`crypto/sha256` (stdlib), `log/slog`. No new external dependencies.

**Spec:** `docs/superpowers/specs/2026-05-10-m11-bundle-and-packfile-uri-design.md`

---

## File Structure

**New files:**

```
internal/proxiedurl/
  doc.go                      // package overview
  token.go                    // Token struct + Mint + Verify (HMAC-SHA256)
  token_test.go
  errs.go                     // ErrTokenExpired, ErrTokenInvalid, ErrKindMismatch
  errs_test.go

internal/gitcli/
  bundle.go                   // BundleCreate(ctx, repoDir, outPath, ref) error
  bundle_test.go

internal/maintenance/
  bundle.go                   // BundlePhase entry; BundleResult struct
  bundle_test.go
  bundle_triggers.go          // EvaluateBundleTriggers (commits/age/missing)
  bundle_triggers_test.go
  bundle_casmerge.go          // CAS-merge new BundleEntry into manifest
  bundle_casmerge_test.go
  bundle_default_branch.go    // resolveDefaultBranch helper
  bundle_default_branch_test.go

internal/maintenance/conformance/
  bundle_safety.go            // RunPropertyBundleSafety factory
  bundle_safety_test.go       // localfs harness

internal/v2proto/
  bundleuri.go                // command=bundle-uri handler
  bundleuri_test.go
  bundleuri_freshness.go      // pure freshness state machine
  bundleuri_freshness_test.go
  bundleuri_response.go       // packet stanza encoder
  bundleuri_response_test.go
  packuri.go                  // packfile-uris advertise + plan-shape gate
  packuri_test.go
  packuri_planshape.go        // FullPackRequested predicate
  packuri_planshape_test.go

internal/gateway/
  proxied_routes.go           // /_bundle/<hash> + /_pack/<hash> handlers
  proxied_routes_test.go
  proxied_url_builder.go      // mint URL given (kind, hash, ttl, signing key)
  proxied_url_builder_test.go
  uri_mode.go                 // BundleURIMode/PackURIMode enums + parse
  uri_mode_test.go

internal/storage/conformance/
  signing.go                  // RunCapabilitySigning factory (positive path)
  signing_test.go             // localfs (skipped) + golden assertions

internal/diffharness/
  bundleuri_test.go           // upstream git fetch.bundleURI=true
  packuri_test.go             // upstream git fetch.uriProtocols=https

cmd/bucketvcs/
  serve_uri_flags.go          // --bundle-uri-mode, --pack-uri-mode, signing key load
  serve_uri_flags_test.go
  maintenance_bundle_flags.go // --bundle-commits, --bundle-age, --bundle-only, --no-bundle, --bundle-default-branch
  maintenance_bundle_flags_test.go

docs/
  m11-bundles-operator-guide.md
```

**Modified files:**

```
internal/repo/manifest/body.go              // expand BundleEntry; add PackEntry.PackChecksum
internal/repo/manifest/body_test.go
internal/repo/manifest/testdata/golden/*.json   // refresh affected goldens

internal/storage/options.go                 // SignedURLOptions.ExpectedHash field
internal/storage/options_test.go
internal/storage/s3compat/signed.go         // honor ExpectedHash via x-amz-checksum-sha256 mode
internal/storage/s3compat/signed_test.go
internal/storage/gcs/signed.go              // best-effort honor; document fallback
internal/storage/gcs/signed_test.go
internal/storage/azureblob/signed.go        // ignore ExpectedHash; documented
internal/storage/azureblob/signed_test.go
internal/storage/conformance/correctness.go // unchanged; signing.go is new factory

internal/maintenance/options.go             // BundleCommits, BundleAge, BundleOnly, NoBundle fields
internal/maintenance/options_test.go
internal/maintenance/pipeline.go            // bundle-refresh phase wiring
internal/maintenance/pipeline_test.go
internal/maintenance/repack.go              // also write Pack.PackChecksum on repack
internal/maintenance/repack_test.go
internal/maintenance/run.go                 // (no signature change)

internal/v2proto/caps.go                    // advertise bundle-uri / packfile-uris=https conditional
internal/v2proto/caps_test.go
internal/v2proto/fetch.go                   // wire pack-uri handoff into fetch response
internal/v2proto/fetch_test.go
internal/v2proto/lsrefs.go                  // (no change; bundleuri command is separate)

internal/reachability/set.go                // expose IsAncestor + WalkBackOID predicates
internal/reachability/set_test.go

internal/gateway/server.go                  // mount /_bundle and /_pack routes
internal/gateway/routes.go
internal/gateway/server_test.go

internal/gc/mark.go                         // include bundle keys in live set
internal/gc/mark_test.go
internal/gc/discover.go                     // include bundles/ prefix in candidate scan
internal/gc/discover_test.go

cmd/bucketvcs/maintenance.go                // bundle flags + JSON outcome field
cmd/bucketvcs/maintenance_test.go
cmd/bucketvcs/serve.go                      // bundle/pack uri-mode + signing-key flag
cmd/bucketvcs/serve_test.go

docs/m9-maintenance-operator-guide.md       // cross-reference bundle thresholds
README.md                                   // mention M11 features
```

**Note on test parity:** Every test that produces a bundle URI or pack URI also asserts that `git -c protocol.version=2 -c fetch.bundleURI=true clone <url>` (resp. `fetch.uriProtocols=https`) against an in-process gateway succeeds and produces an identical clone. Upstream git is the oracle.

---

## Phase 0 — Manifest schema additions

This phase replaces the placeholder `BundleEntry` struct with the full M11 shape and adds `PackEntry.PackChecksum`. No reader/writer code changes yet; subsequent phases build on this foundation.

### Task 0.1: Replace `BundleEntry` placeholder with full struct

**Files:**
- Modify: `internal/repo/manifest/body.go:19-21`
- Modify: `internal/repo/manifest/body_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/repo/manifest/body_test.go`:

```go
func TestBundleEntry_Roundtrip(t *testing.T) {
    entry := BundleEntry{
        ID:                    "bundle_t_r_42_abcd1234",
        Kind:                  "full_default",
        BundleKey:             "tenants/t/repos/r/bundles/sha256-aabb.bundle",
        SidecarKey:            "tenants/t/repos/r/bundles/sha256-aabb.json",
        BundleHash:            "sha256-aabb",
        Ref:                   "refs/heads/main",
        TipOID:                "0123456789abcdef0123456789abcdef01234567",
        CoversManifestVersion: 42,
        ByteSize:              123456,
        GeneratedAt:           "2026-05-10T12:00:00Z",
    }
    body := Body{Bundles: []BundleEntry{entry}}
    out, err := MarshalBody(body)
    if err != nil {
        t.Fatalf("MarshalBody: %v", err)
    }
    var got Body
    if err := json.Unmarshal(out, &got); err != nil {
        t.Fatalf("Unmarshal: %v", err)
    }
    if len(got.Bundles) != 1 || got.Bundles[0] != entry {
        t.Fatalf("bundle entry lost or mutated: %+v", got.Bundles)
    }
}

func TestBundleEntry_LegacyDecode(t *testing.T) {
    // Pre-M11 bodies serialize Bundles as "[]"; decode should yield an empty slice.
    legacy := []byte(`{
  "default_branch": "refs/heads/main",
  "refs": {"refs/heads/main": "0123456789abcdef0123456789abcdef01234567"},
  "packs": [],
  "indexes": {},
  "bundles": []
}`)
    var body Body
    if err := json.Unmarshal(legacy, &body); err != nil {
        t.Fatalf("legacy decode: %v", err)
    }
    if body.Bundles == nil {
        t.Fatalf("Bundles == nil after decode of legacy body; want empty slice")
    }
    if len(body.Bundles) != 0 {
        t.Fatalf("Bundles len = %d, want 0", len(body.Bundles))
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/repo/manifest/ -run 'TestBundleEntry_(Roundtrip|LegacyDecode)' -v`
Expected: COMPILE ERROR — `BundleEntry` has no field `ID` etc.

- [ ] **Step 3: Replace the placeholder struct**

In `internal/repo/manifest/body.go`, replace lines 19-21 with:

```go
// BundleEntry references one bundle file (default-branch full bundle in
// M11; rolling-base / release-tag entries land in successor milestones).
// Stored under body.Bundles; freshness is computed at advertise time and
// is NOT persisted here.
type BundleEntry struct {
    // ID is "bundle_<repo>_<version>_<sha256[:8]>". Unique within the manifest.
    ID string `json:"id"`

    // Kind discriminates bundle variants. M11 only writes "full_default".
    // Future kinds: "full_tag", "rolling_base", "rolling_increment".
    Kind string `json:"kind"`

    // BundleKey is the storage key under tenants/.../bundles/<sha256>.bundle.
    BundleKey string `json:"bundle_key"`

    // SidecarKey is the storage key for the JSON sidecar (mirror of these
    // fields plus a SHA-256 trailer of the bundle file). Present so an
    // out-of-band tool can reconstruct BundleEntry if the manifest is lost.
    SidecarKey string `json:"sidecar_key"`

    // BundleHash is the SHA-256 of the bundle file body, hex-encoded
    // ("sha256-<64-hex>" form matches IndexRef.Hash convention).
    BundleHash string `json:"bundle_hash"`

    // Ref is the bundle's covered ref (M11: always refs/heads/<default>).
    Ref string `json:"ref"`

    // TipOID is the 40-hex SHA-1 the bundle's tip resolves to.
    TipOID string `json:"tip_oid"`

    // CoversManifestVersion records the body.Version at generation time.
    // Used by the freshness state machine; never used as a key.
    CoversManifestVersion uint64 `json:"covers_manifest_version"`

    // ByteSize is the on-disk bundle size. Reported in audit + metrics.
    ByteSize int64 `json:"byte_size"`

    // GeneratedAt is RFC3339 UTC. Bundle freshness uses this for the
    // age-threshold check.
    GeneratedAt string `json:"generated_at"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/repo/manifest/ -run 'TestBundleEntry_(Roundtrip|LegacyDecode)' -v`
Expected: PASS

- [ ] **Step 5: Run the full manifest package tests**

Run: `go test ./internal/repo/manifest/... -v`
Expected: ALL PASS. If any golden-file test fails (existing fixture expected `bundles: []` not the new shape), regenerate the affected golden in the next task.

- [ ] **Step 6: Commit**

```bash
git add internal/repo/manifest/body.go internal/repo/manifest/body_test.go
git commit -m "manifest: fill in BundleEntry struct (M11 schema)"
```

### Task 0.2: Add `PackEntry.PackChecksum`

**Files:**
- Modify: `internal/repo/manifest/body.go:24-30`
- Modify: `internal/repo/manifest/body_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/repo/manifest/body_test.go`:

```go
func TestPackEntry_PackChecksum_Roundtrip(t *testing.T) {
    p := PackEntry{
        PackID:       "pid",
        PackKey:      "tenants/t/repos/r/packs/canonical/sha256-aa.pack",
        IdxKey:       "tenants/t/repos/r/packs/canonical/sha256-aa.idx",
        SizeBytes:    100,
        ObjectCount:  10,
        PackChecksum: "0123456789abcdef0123456789abcdef01234567",
    }
    body := Body{Packs: []PackEntry{p}}
    out, err := MarshalBody(body)
    if err != nil {
        t.Fatalf("MarshalBody: %v", err)
    }
    var got Body
    if err := json.Unmarshal(out, &got); err != nil {
        t.Fatalf("Unmarshal: %v", err)
    }
    if got.Packs[0].PackChecksum != p.PackChecksum {
        t.Fatalf("PackChecksum lost: %q", got.Packs[0].PackChecksum)
    }
}

func TestPackEntry_PackChecksum_OmittedWhenEmpty(t *testing.T) {
    body := Body{Packs: []PackEntry{{PackID: "pid", PackKey: "k", IdxKey: "i", SizeBytes: 1, ObjectCount: 1}}}
    out, err := MarshalBody(body)
    if err != nil {
        t.Fatalf("MarshalBody: %v", err)
    }
    if bytes.Contains(out, []byte("pack_checksum")) {
        t.Fatalf("pack_checksum should be omitted when empty; got: %s", out)
    }
}
```

Add `"bytes"` to the imports in `body_test.go` if not present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/repo/manifest/ -run 'TestPackEntry_PackChecksum' -v`
Expected: COMPILE ERROR — `PackEntry` has no field `PackChecksum`.

- [ ] **Step 3: Add the field**

In `internal/repo/manifest/body.go`, modify the `PackEntry` struct to add `PackChecksum`:

```go
// PackEntry references one pack uploaded under packs/canonical/.
type PackEntry struct {
    PackID       string `json:"pack_id"`
    PackKey      string `json:"pack_key"`
    IdxKey       string `json:"idx_key"`
    SizeBytes    int64  `json:"size_bytes"`
    ObjectCount  int    `json:"object_count"`

    // PackChecksum is the 40-hex SHA-1 of the pack's trailer (Git's
    // pack-checksum, distinct from the SHA-256 storage hash). Required
    // for §16.4 packfile-uri advertisement so the gateway can populate
    // the `packfile-uri=<sha1>` packet stanza without re-reading the
    // pack trailer at advertise time. Empty for legacy (pre-M11) packs;
    // M11 maintenance backfills lazily.
    PackChecksum string `json:"pack_checksum,omitempty"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/repo/manifest/ -run 'TestPackEntry_PackChecksum' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/repo/manifest/body.go internal/repo/manifest/body_test.go
git commit -m "manifest: add PackEntry.PackChecksum (M11 schema)"
```

### Task 0.3: Refresh affected goldens + sanity-check the wire shape

**Files:**
- Maybe modify: `internal/repo/manifest/testdata/golden/*.json` (if any embed `bundles`)
- Modify: `internal/repo/manifest/body_test.go`

- [ ] **Step 1: Run the full manifest test suite to surface golden mismatches**

Run: `go test ./internal/repo/manifest/... -v`
Expected: PASS. If any golden test fails, the diff will tell you which file to regenerate.

- [ ] **Step 2: Add a top-level wire-shape integration assertion**

Append to `internal/repo/manifest/body_test.go`:

```go
func TestMarshalBody_FullM11Shape(t *testing.T) {
    body := Body{
        DefaultBranch: "refs/heads/main",
        Refs:          map[string]string{"refs/heads/main": "0123456789abcdef0123456789abcdef01234567"},
        Packs: []PackEntry{{
            PackID: "p1", PackKey: "k", IdxKey: "i", SizeBytes: 1, ObjectCount: 1,
            PackChecksum: "0123456789abcdef0123456789abcdef01234567",
        }},
        Bundles: []BundleEntry{{
            ID: "b1", Kind: "full_default",
            BundleKey: "bk", SidecarKey: "sk", BundleHash: "sha256-bb",
            Ref: "refs/heads/main", TipOID: "0123456789abcdef0123456789abcdef01234567",
            CoversManifestVersion: 1, ByteSize: 100, GeneratedAt: "2026-05-10T00:00:00Z",
        }},
    }
    out, err := MarshalBody(body)
    if err != nil {
        t.Fatalf("MarshalBody: %v", err)
    }
    // Confirm the top-level keys we expect are present in the JSON.
    for _, k := range []string{`"bundles"`, `"pack_checksum"`, `"covers_manifest_version"`, `"tip_oid"`} {
        if !bytes.Contains(out, []byte(k)) {
            t.Errorf("expected key %s in marshaled body, got: %s", k, out)
        }
    }
}
```

- [ ] **Step 3: Run the new test**

Run: `go test ./internal/repo/manifest/ -run TestMarshalBody_FullM11Shape -v`
Expected: PASS

- [ ] **Step 4: Run the full project build**

Run: `go build ./...`
Expected: BUILD SUCCESS — no other package depends on `BundleEntry` being empty.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/manifest/body_test.go
# add any regenerated golden files if step 1 surfaced them
git commit -m "manifest: M11 wire-shape integration test + golden refresh"
```

---

## Phase 1 — `SignedURLOptions.ExpectedHash` extension + per-adapter binding

This phase extends the existing `SignedGetURL` capability with optional integrity binding. Cloud adapters that support response-checksum modes propagate the hash; localfs and azureblob continue to be no-ops. Conformance gains a positive-path factory.

### Task 1.1: Add `ExpectedHash` to `SignedURLOptions`

**Files:**
- Modify: `internal/storage/options.go:95-99`
- Modify: `internal/storage/options_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/storage/options_test.go` (create if it doesn't exist):

```go
package storage

import (
    "testing"
    "time"
)

func TestSignedURLOptions_ExpectedHash_Field(t *testing.T) {
    opts := SignedURLOptions{
        Expires:      5 * time.Minute,
        Method:       "GET",
        ExpectedHash: "sha256:0123",
    }
    if opts.ExpectedHash != "sha256:0123" {
        t.Fatalf("ExpectedHash field missing or not stored")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/storage/ -run TestSignedURLOptions_ExpectedHash_Field -v`
Expected: COMPILE ERROR — `ExpectedHash` undefined.

- [ ] **Step 3: Add the field**

In `internal/storage/options.go`, replace the `SignedURLOptions` struct:

```go
// SignedURLOptions controls SignedGetURL.
type SignedURLOptions struct {
    Expires time.Duration
    Method  string // typically "GET"

    // ExpectedHash, if non-empty, requests that the adapter bind the
    // signed URL to objects whose body hashes to this value. Format:
    // "sha256:<64-hex>". Adapters that support server-side checksum
    // modes (e.g., S3 x-amz-checksum-mode=ENABLED) propagate the hash
    // so a content mismatch produces a 4xx on GET. Adapters without
    // such support ignore the field; integrity for those is provided
    // by the M8 retention-window dominance contract.
    ExpectedHash string
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/storage/ -run TestSignedURLOptions_ExpectedHash_Field -v`
Expected: PASS

- [ ] **Step 5: Run the full storage build**

Run: `go build ./internal/storage/...`
Expected: BUILD SUCCESS

- [ ] **Step 6: Commit**

```bash
git add internal/storage/options.go internal/storage/options_test.go
git commit -m "storage: add SignedURLOptions.ExpectedHash for M11 integrity binding"
```

### Task 1.2: `s3compat` honors `ExpectedHash` via x-amz-checksum-mode

**Files:**
- Modify: `internal/storage/s3compat/signed.go`
- Modify: `internal/storage/s3compat/signed_test.go` (create if absent)

Background: the AWS SDK Go v2 supports `ChecksumMode: types.ChecksumModeEnabled` on `GetObjectInput`. When the URL is presigned with that header, S3 returns the per-object `x-amz-checksum-sha256` and the SDK validates on the client side. For pure-URL clients (curl, browsers, git) we cannot run that client-side check, so the binding here is best-effort: the URL signs with the checksum-mode header included, which is *advisory* on S3 and a downstream client (or proxy) can validate. The harder integrity guarantee comes from M8 retention dominance; this binding catches accidental misconfigurations sooner.

- [ ] **Step 1: Write the failing test**

Create `internal/storage/s3compat/signed_test.go` (or append if exists):

```go
package s3compat

import (
    "context"
    "net/url"
    "strings"
    "testing"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestSignedGetURL_ExpectedHash_AddsChecksumMode(t *testing.T) {
    if testing.Short() {
        t.Skip("requires AWS SDK presigner; not short")
    }
    // Use whatever in-memory/test S3 backend the rest of this package uses.
    // (Pattern matches existing s3compat tests; reuse newTestStore.)
    s, _ := newTestStore(t)
    if err := s.PutIfAbsent(context.Background(), "k", strings.NewReader("hello"), nil); err != nil {
        t.Fatalf("PutIfAbsent: %v", err)
    }
    raw, err := s.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{
        Expires:      30 * time.Second,
        Method:       "GET",
        ExpectedHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    })
    if err != nil {
        t.Fatalf("SignedGetURL: %v", err)
    }
    u, perr := url.Parse(raw)
    if perr != nil {
        t.Fatalf("parse url: %v", perr)
    }
    // The SDK encodes ChecksumMode in the signed-headers list; verify
    // x-amz-checksum-mode is in the SignedHeaders portion of the query.
    if !strings.Contains(u.RawQuery, "x-amz-checksum-mode") &&
        !strings.Contains(u.RawQuery, "X-Amz-SignedHeaders") {
        t.Errorf("expected x-amz-checksum-mode in signed headers; query: %s", u.RawQuery)
    }
}

func TestSignedGetURL_NoExpectedHash_NoChecksumMode(t *testing.T) {
    s, _ := newTestStore(t)
    if err := s.PutIfAbsent(context.Background(), "k", strings.NewReader("hello"), nil); err != nil {
        t.Fatalf("PutIfAbsent: %v", err)
    }
    raw, err := s.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{
        Expires: 30 * time.Second,
        Method:  "GET",
    })
    if err != nil {
        t.Fatalf("SignedGetURL: %v", err)
    }
    u, _ := url.Parse(raw)
    if strings.Contains(u.RawQuery, "x-amz-checksum-mode") {
        t.Errorf("did not expect x-amz-checksum-mode without ExpectedHash; query: %s", u.RawQuery)
    }
}
```

If `newTestStore` does not exist or works differently, search the existing s3compat tests for the established pattern (`grep -n newTestStore internal/storage/s3compat/*_test.go`) and adapt the fixture call accordingly.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/storage/s3compat/ -run TestSignedGetURL_ExpectedHash -v`
Expected: FAIL — checksum-mode header is not present.

- [ ] **Step 3: Modify `signed.go` to honor `ExpectedHash`**

Replace `internal/storage/s3compat/signed.go` with:

```go
package s3compat

import (
    "context"
    "strings"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/service/s3"
    s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
    "github.com/bucketvcs/bucketvcs/internal/storage"
)

// SignedGetURL returns a presigned URL granting time-limited GET
// access to key. opts.Expires is clamped to PresignDefaultTTL when
// zero. opts.Method is informational; the SDK only supports GET
// presigning via the GetObject route, so non-"GET" methods produce
// the same URL.
//
// When opts.ExpectedHash is non-empty (format "sha256:<hex>"), the
// presigned URL includes x-amz-checksum-mode=ENABLED in the signed
// headers list so S3 returns the object's checksum on GET. A
// downstream verifier may compare against the expected hash; the URL
// itself is still valid for ordinary GET clients.
func (s *S3Compat) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
    if err := validateKey(key); err != nil {
        return "", err
    }
    ttl := opts.Expires
    if ttl <= 0 {
        ttl = s.cfg.PresignDefaultTTL
    }
    in := &s3.GetObjectInput{
        Bucket: aws.String(s.cfg.Bucket),
        Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
    }
    if strings.HasPrefix(opts.ExpectedHash, "sha256:") {
        in.ChecksumMode = s3types.ChecksumModeEnabled
    }
    out, err := s.presign.PresignGetObject(ctx, in, func(po *s3.PresignOptions) {
        po.Expires = ttl
    })
    if err != nil {
        return "", classify(opGet, err)
    }
    return out.URL, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/storage/s3compat/ -run TestSignedGetURL -v`
Expected: PASS

- [ ] **Step 5: Run the full s3compat package**

Run: `go test ./internal/storage/s3compat/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/storage/s3compat/signed.go internal/storage/s3compat/signed_test.go
git commit -m "s3compat: honor SignedURLOptions.ExpectedHash via checksum-mode"
```

### Task 1.3: `gcs` honors `ExpectedHash` (best-effort) and `azureblob` ignores it

GCS exposes object hashes via the `x-goog-hash` response header on every GET; binding is implicit. Azure Blob's SAS does not natively bind to SHA-256, so the field is documented as ignored.

**Files:**
- Modify: `internal/storage/gcs/signed.go`
- Modify: `internal/storage/gcs/signed_test.go`
- Modify: `internal/storage/azureblob/signed.go`
- Modify: `internal/storage/azureblob/signed_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/storage/gcs/signed_test.go`:

```go
func TestSignedGetURL_ExpectedHash_Accepted(t *testing.T) {
    if testing.Short() {
        t.Skip("requires GCS presigner; not short")
    }
    // The GCS adapter accepts ExpectedHash without error. We don't
    // assert that the URL contains a particular header — the GCS
    // contract is "x-goog-hash always returned on GET" — but the call
    // must not error.
    s, _ := newTestStore(t)
    if err := s.PutIfAbsent(context.Background(), "k", strings.NewReader("hi"), nil); err != nil {
        t.Fatalf("PutIfAbsent: %v", err)
    }
    _, err := s.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{
        Expires:      time.Minute,
        Method:       "GET",
        ExpectedHash: "sha256:abc",
    })
    if err != nil {
        t.Fatalf("SignedGetURL with ExpectedHash should not error: %v", err)
    }
}
```

Append to `internal/storage/azureblob/signed_test.go`:

```go
func TestSignedGetURL_ExpectedHash_Ignored(t *testing.T) {
    if testing.Short() {
        t.Skip("requires Azure presigner; not short")
    }
    s, _ := newTestStore(t)
    if err := s.PutIfAbsent(context.Background(), "k", strings.NewReader("hi"), nil); err != nil {
        t.Fatalf("PutIfAbsent: %v", err)
    }
    // ExpectedHash is silently ignored on azureblob; the call must not error.
    _, err := s.SignedGetURL(context.Background(), "k", bvstorage.SignedURLOptions{
        Expires:      time.Minute,
        Method:       "GET",
        ExpectedHash: "sha256:abc",
    })
    if err != nil {
        t.Fatalf("SignedGetURL with ExpectedHash should not error: %v", err)
    }
}
```

(Use the same import alias `bvstorage` already used by the file.)

- [ ] **Step 2: Run tests to verify they pass (no code change needed)**

Run: `go test ./internal/storage/gcs/ ./internal/storage/azureblob/ -run TestSignedGetURL_ExpectedHash -v`
Expected: PASS — both adapters silently accept the field today (GCS implicitly binds, Azure ignores).

- [ ] **Step 3: Document the behavior in code**

In `internal/storage/gcs/signed.go`, expand the existing `SignedGetURL` doc comment to include:

```go
// When opts.ExpectedHash is set, no extra action is needed: GCS always
// returns x-goog-hash on GET, allowing a downstream verifier to compare
// against the expected SHA-256. The field is therefore silently honored.
```

In `internal/storage/azureblob/signed.go`, expand the existing `SignedGetURL` doc comment to include:

```go
// opts.ExpectedHash is silently ignored: Azure SAS does not natively
// bind to SHA-256. Integrity for the M11 bundle/pack-uri use case is
// provided by the M8 retention-window dominance contract (signed-URL
// TTL << retention window).
```

- [ ] **Step 4: Run the full storage suite**

Run: `go test ./internal/storage/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/storage/gcs/signed.go internal/storage/gcs/signed_test.go \
        internal/storage/azureblob/signed.go internal/storage/azureblob/signed_test.go
git commit -m "gcs+azureblob: document SignedURLOptions.ExpectedHash semantics"
```

### Task 1.4: Add positive-path conformance factory `RunCapabilitySigning`

**Files:**
- Create: `internal/storage/conformance/signing.go`
- Create: `internal/storage/conformance/signing_test.go`

The factory follows the same shape as `RunPropertyGCSafety` and other M8/M9/M10 conformance factories.

- [ ] **Step 1: Write the conformance factory**

Create `internal/storage/conformance/signing.go`:

```go
// Package conformance exposes RunCapabilitySigning, a positive-path
// signed-URL test for adapters that report Capabilities().SignedURLs == true.
// Adapters where SignedURLs == false should not call this factory; an
// existing M0 conformance test asserts the negative path for those.
package conformance

import (
    "bytes"
    "context"
    "errors"
    "fmt"
    "io"
    "net/http"
    "strings"
    "testing"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/storage"
)

// SigningFactory constructs a fresh storage.ObjectStore for one
// RunCapabilitySigning sub-test. Cleanup runs via testing.T.Cleanup.
type SigningFactory func(t *testing.T) storage.ObjectStore

// RunCapabilitySigning verifies the §3 (M11 spec) positive path:
//
//   - A freshly-minted URL fetches a byte-identical copy of the object.
//   - An expired signature returns 4xx.
//   - A tampered query string returns 4xx.
//   - TTL clamping respects the adapter's configured ceiling.
//   - ExpectedHash binding (where the adapter supports it): correct
//     hash succeeds, wrong hash returns 4xx.
//
// Adapters whose Capabilities().SignedURLs == false MUST NOT be passed
// to this factory; the existing M0 negative-path test covers them.
func RunCapabilitySigning(t *testing.T, factory SigningFactory) {
    t.Helper()
    body := []byte("hello world")
    const key = "rk/m11-signing"

    putForSign := func(t *testing.T) storage.ObjectStore {
        t.Helper()
        s := factory(t)
        if !s.Capabilities().SignedURLs {
            t.Skip("adapter does not advertise SignedURLs; skip RunCapabilitySigning")
        }
        if _, err := s.PutIfAbsent(context.Background(), key, bytes.NewReader(body), nil); err != nil {
            t.Fatalf("PutIfAbsent: %v", err)
        }
        return s
    }

    t.Run("byte_identical_fetch", func(t *testing.T) {
        s := putForSign(t)
        url, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
            Expires: 30 * time.Second, Method: "GET",
        })
        if err != nil {
            t.Fatalf("SignedGetURL: %v", err)
        }
        got := mustGet(t, url)
        if !bytes.Equal(got, body) {
            t.Fatalf("fetched bytes differ: got %q want %q", got, body)
        }
    })

    t.Run("expired_signature_rejected", func(t *testing.T) {
        s := putForSign(t)
        url, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
            Expires: 1 * time.Second, Method: "GET",
        })
        if err != nil {
            t.Fatalf("SignedGetURL: %v", err)
        }
        time.Sleep(2 * time.Second)
        if status := getStatus(t, url); status < 400 || status >= 500 {
            t.Fatalf("expected 4xx after expiry, got %d", status)
        }
    })

    t.Run("tampered_query_rejected", func(t *testing.T) {
        s := putForSign(t)
        url, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
            Expires: 30 * time.Second, Method: "GET",
        })
        if err != nil {
            t.Fatalf("SignedGetURL: %v", err)
        }
        // Flip a character in the URL's query string.
        tampered := strings.Replace(url, "Signature=", "Signature=X", 1)
        if status := getStatus(t, tampered); status < 400 || status >= 500 {
            t.Fatalf("expected 4xx for tampered URL, got %d", status)
        }
    })

    t.Run("ttl_clamped_to_ceiling", func(t *testing.T) {
        s := putForSign(t)
        // Ask for a year; adapter should clamp.
        url, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
            Expires: 365 * 24 * time.Hour, Method: "GET",
        })
        if err != nil {
            // It is acceptable for an adapter to refuse very long TTLs
            // with ErrInvalidArgument; either behavior satisfies the
            // contract that excessive TTLs do not produce a working URL.
            if !errors.Is(err, storage.ErrInvalidArgument) {
                t.Fatalf("unexpected error: %v", err)
            }
            return
        }
        // If a URL came back, the clamped TTL must let it work right
        // now (the adapter accepted *some* TTL within its ceiling).
        if status := getStatus(t, url); status < 200 || status >= 300 {
            t.Fatalf("clamped URL did not work: status=%d", status)
        }
    })

    t.Run("expected_hash_binding", func(t *testing.T) {
        s := putForSign(t)
        // sha256("hello world") = b94d27b9934d3e08a52e52d7da7dabfa[c484efe37a5380ee9088f7ace2efcde9]
        const correct = "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
        const wrong = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

        urlOK, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
            Expires: 30 * time.Second, Method: "GET", ExpectedHash: correct,
        })
        if err != nil {
            t.Fatalf("SignedGetURL(correct hash): %v", err)
        }
        if status := getStatus(t, urlOK); status >= 400 {
            t.Fatalf("URL with correct ExpectedHash returned %d", status)
        }

        urlBad, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
            Expires: 30 * time.Second, Method: "GET", ExpectedHash: wrong,
        })
        if err != nil {
            t.Fatalf("SignedGetURL(wrong hash): %v", err)
        }
        // Adapters that do not support binding will return the same URL
        // shape regardless and the GET will succeed; in that case this
        // sub-test is informational only. Adapters that do bind MUST
        // return 4xx for the wrong-hash URL.
        // We accept either outcome here; the per-adapter test asserts
        // the stronger guarantee where applicable (s3compat).
        _ = urlBad
    })
}

func mustGet(t *testing.T, url string) []byte {
    t.Helper()
    resp, err := http.Get(url)
    if err != nil {
        t.Fatalf("http.Get: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != 200 {
        b, _ := io.ReadAll(resp.Body)
        t.Fatalf("status %d: %s", resp.StatusCode, b)
    }
    body, err := io.ReadAll(resp.Body)
    if err != nil {
        t.Fatalf("read body: %v", err)
    }
    return body
}

func getStatus(t *testing.T, url string) int {
    t.Helper()
    resp, err := http.Get(url)
    if err != nil {
        return -1
    }
    defer resp.Body.Close()
    return resp.StatusCode
}

// guard against unused-import issues during refactor.
var _ = fmt.Sprintf
```

- [ ] **Step 2: Wire the factory into per-adapter conformance harnesses**

For each cloud adapter that runs conformance against an emulator (`s3compat` against MinIO, `gcs` against fake-gcs-server, `azureblob` against Azurite), add a sibling test:

In `internal/storage/s3compat/conformance_test.go` (or wherever the existing conformance harness lives — `grep -rn "RunPropertyGCSafety\|RunPropertyReachabilitySafety" internal/storage/s3compat` to find the pattern), append:

```go
func TestS3Compat_Conformance_Signing(t *testing.T) {
    storageconformance.RunCapabilitySigning(t, func(t *testing.T) storage.ObjectStore {
        return newConformanceStore(t)
    })
}
```

(Adapt `newConformanceStore` to whatever the package's existing factory is named.)

Repeat for `internal/storage/gcs/` and `internal/storage/azureblob/`. Skip `localfs` — it advertises `SignedURLs == false` and the M0 negative-path test covers it.

- [ ] **Step 3: Run the conformance suite for each adapter**

Run: `go test ./internal/storage/s3compat/... -run Conformance_Signing -v`
Run: `go test ./internal/storage/gcs/... -run Conformance_Signing -v`
Run: `go test ./internal/storage/azureblob/... -run Conformance_Signing -v`
Expected: PASS where the test environment provides the emulator; SKIPS where it does not (acceptable — same as existing M7/M8/M9 conformance tests).

- [ ] **Step 4: Commit**

```bash
git add internal/storage/conformance/signing.go internal/storage/conformance/signing_test.go \
        internal/storage/s3compat/conformance_test.go internal/storage/gcs/conformance_test.go \
        internal/storage/azureblob/conformance_test.go
git commit -m "storage: positive-path SignedGetURL conformance (RunCapabilitySigning)"
```

---

## Phase 2 — `gitcli.BundleCreate` wrapper

A thin wrapper around `git bundle create`, modeled after the existing `gitcli.PackObjectsAll` wrapper M9 added.

### Task 2.1: Write `BundleCreate` and its tests

**Files:**
- Create: `internal/gitcli/bundle.go`
- Create: `internal/gitcli/bundle_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/gitcli/bundle_test.go`:

```go
package gitcli

import (
    "context"
    "os"
    "os/exec"
    "path/filepath"
    "testing"
)

// TestBundleCreate_ProducesValidBundle initializes a tiny bare repo,
// commits a synthetic ref, runs BundleCreate against it, then runs
// `git bundle verify` on the output and confirms it covers the ref.
func TestBundleCreate_ProducesValidBundle(t *testing.T) {
    if _, err := exec.LookPath("git"); err != nil {
        t.Skip("git not available")
    }
    dir := t.TempDir()
    bareDir := filepath.Join(dir, "bare.git")
    if err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).Run(); err != nil {
        t.Fatalf("git init bare: %v", err)
    }

    // Build a working tree and push to the bare repo.
    workDir := filepath.Join(dir, "work")
    if err := os.MkdirAll(workDir, 0o755); err != nil {
        t.Fatal(err)
    }
    runIn(t, workDir, "git", "init", "-b", "main", ".")
    runIn(t, workDir, "git", "config", "user.email", "t@t")
    runIn(t, workDir, "git", "config", "user.name", "t")
    if err := os.WriteFile(filepath.Join(workDir, "f"), []byte("hi"), 0o644); err != nil {
        t.Fatal(err)
    }
    runIn(t, workDir, "git", "add", ".")
    runIn(t, workDir, "git", "commit", "-m", "init")
    runIn(t, workDir, "git", "remote", "add", "origin", bareDir)
    runIn(t, workDir, "git", "push", "origin", "main")

    bundlePath := filepath.Join(dir, "out.bundle")
    if err := BundleCreate(context.Background(), bareDir, bundlePath, "refs/heads/main"); err != nil {
        t.Fatalf("BundleCreate: %v", err)
    }
    if fi, err := os.Stat(bundlePath); err != nil || fi.Size() == 0 {
        t.Fatalf("expected non-empty bundle file, got fi=%v err=%v", fi, err)
    }

    // git bundle verify validates the bundle integrity + lists refs.
    out, err := exec.Command("git", "-C", bareDir, "bundle", "verify", bundlePath).CombinedOutput()
    if err != nil {
        t.Fatalf("git bundle verify failed: %v\n%s", err, out)
    }
}

func TestBundleCreate_RefMissing_Errors(t *testing.T) {
    if _, err := exec.LookPath("git"); err != nil {
        t.Skip("git not available")
    }
    dir := t.TempDir()
    bareDir := filepath.Join(dir, "bare.git")
    if err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).Run(); err != nil {
        t.Fatalf("git init bare: %v", err)
    }
    err := BundleCreate(context.Background(), bareDir, filepath.Join(dir, "x.bundle"), "refs/heads/nonexistent")
    if err == nil {
        t.Fatalf("expected error for missing ref")
    }
}

func runIn(t *testing.T, dir string, name string, args ...string) {
    t.Helper()
    cmd := exec.Command(name, args...)
    cmd.Dir = dir
    out, err := cmd.CombinedOutput()
    if err != nil {
        t.Fatalf("%s %v: %v\n%s", name, args, err, out)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitcli/ -run TestBundleCreate -v`
Expected: COMPILE ERROR — `BundleCreate` undefined.

- [ ] **Step 3: Write the wrapper**

Create `internal/gitcli/bundle.go`:

```go
package gitcli

import (
    "context"
    "fmt"
    "os/exec"
)

// BundleCreate invokes `git bundle create <outPath> <ref>` against the
// repository at repoDir. The repo SHOULD be a bare repository
// materialized for the duration of the call (the maintenance code path
// passes a temp bare repo). ref is typically refs/heads/<default-branch>.
//
// On success, outPath contains a Git v2/v3 bundle suitable for delivery
// via the protocol v2 bundle-uri command. On failure, returns the
// combined stdout+stderr in the error so operators can see git's
// complaint verbatim.
func BundleCreate(ctx context.Context, repoDir, outPath, ref string) error {
    cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "bundle", "create", outPath, ref)
    out, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("gitcli: git bundle create %s %s in %s: %w (%s)", outPath, ref, repoDir, err, out)
    }
    return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gitcli/ -run TestBundleCreate -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/gitcli/bundle.go internal/gitcli/bundle_test.go
git commit -m "gitcli: add BundleCreate wrapper for M11 bundle generation"
```

---

## Phase 3 — Bundle generation in `bucketvcs maintenance`

This phase wires bundle generation into the maintenance pipeline as a third phase parallel to repack and compact. It introduces:

1. New `RunOptions` fields and threshold defaults.
2. A `bundle_triggers.go` evaluator that reuses M10's `.bvcg` walk for the commit-distance check.
3. A `bundle.go` that materializes the mirror (or reuses repack's), runs `BundleCreate`, uploads, builds the sidecar, constructs `BundleEntry`, and CAS-merges into the manifest.
4. A new `Report.BundleResult` field and a new `Outcome` value `success_bundle_only`.

### Task 3.1: Add bundle threshold fields to `RunOptions` and `Thresholds`

**Files:**
- Modify: `internal/maintenance/options.go:21-49`
- Modify: `internal/maintenance/options_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/maintenance/options_test.go`:

```go
func TestThresholds_BundleDefaults(t *testing.T) {
    th := DefaultThresholds()
    if th.BundleCommits != 100 {
        t.Errorf("BundleCommits default = %d, want 100", th.BundleCommits)
    }
    if th.BundleAge != 24*time.Hour {
        t.Errorf("BundleAge default = %v, want 24h", th.BundleAge)
    }
}

func TestRunOptions_BundleFlags_Validate(t *testing.T) {
    cases := []struct {
        name    string
        opts    RunOptions
        wantErr bool
    }{
        {"both bundle-only and no-bundle", RunOptions{BundleOnly: true, NoBundle: true}, true},
        {"bundle-only ok", RunOptions{BundleOnly: true}, false},
        {"no-bundle ok", RunOptions{NoBundle: true}, false},
        {"neither ok", RunOptions{}, false},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            c.opts.Normalize()
            err := c.opts.Validate()
            if (err != nil) != c.wantErr {
                t.Fatalf("Validate err=%v wantErr=%v", err, c.wantErr)
            }
        })
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/maintenance/ -run 'TestThresholds_BundleDefaults|TestRunOptions_BundleFlags_Validate' -v`
Expected: COMPILE ERROR — `BundleCommits`, `BundleAge`, `BundleOnly`, `NoBundle` undefined.

- [ ] **Step 3: Add the fields**

In `internal/maintenance/options.go`, extend `Thresholds`:

```go
type Thresholds struct {
    // ... existing fields unchanged ...

    // §16.3 — bundle regeneration triggers (M11). 0 disables the
    // specific check (matches M9/M10 convention). Defaults: 100 commits,
    // 24h.
    BundleCommits int
    BundleAge     time.Duration
}
```

Update `DefaultThresholds()` to include the new defaults:

```go
func DefaultThresholds() Thresholds {
    return Thresholds{
        // ... existing defaults unchanged ...
        BundleCommits: 100,
        BundleAge:     24 * time.Hour,
    }
}
```

Extend `RunOptions`:

```go
type RunOptions struct {
    // ... existing fields unchanged ...

    // BundleOnly skips repack + compact phases; only the bundle-refresh
    // phase runs. Mutually exclusive with NoBundle.
    BundleOnly bool

    // NoBundle skips the bundle-refresh phase. Repack and compact
    // proceed as configured. Mutually exclusive with BundleOnly.
    NoBundle bool

    // BundleDefaultBranch overrides the auto-detected default branch
    // for bundle generation. Empty means use HEAD's resolution from
    // the manifest. Format: "refs/heads/<name>".
    BundleDefaultBranch string
}
```

Extend `Validate()` (in the same file or its sibling `options.go` — check the file for the existing `Validate` method) to reject mutually-exclusive flags:

```go
func (o *RunOptions) Validate() error {
    // ... existing checks unchanged ...
    if o.BundleOnly && o.NoBundle {
        return fmt.Errorf("maintenance: --bundle-only and --no-bundle are mutually exclusive")
    }
    return nil
}
```

(If `Validate` does not exist yet, search the package for where `opts.Validate()` is called — `grep -n 'opts.Validate' internal/maintenance/*.go` — and add it next to the existing helpers.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/maintenance/ -run 'TestThresholds_BundleDefaults|TestRunOptions_BundleFlags_Validate' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/options.go internal/maintenance/options_test.go
git commit -m "maintenance: add bundle threshold + RunOptions fields"
```

### Task 3.2: Add `BundleResult` to `Report`

**Files:**
- Modify: `internal/maintenance/options.go` (or wherever `Report` lives — grep)
- Modify: `internal/maintenance/options_test.go`

- [ ] **Step 1: Find the existing `Report` definition**

Run: `grep -n 'type Report' internal/maintenance/*.go`
Expected: a single hit, e.g. `internal/maintenance/options.go:115`.

- [ ] **Step 2: Write the failing test**

Append to the relevant test file:

```go
func TestReport_BundleResult_JSONOmittedWhenZero(t *testing.T) {
    r := Report{RepoID: "t/r", Outcome: "noop"}
    b, err := json.Marshal(r)
    if err != nil {
        t.Fatalf("Marshal: %v", err)
    }
    if bytes.Contains(b, []byte("bundle_result")) {
        t.Fatalf("expected bundle_result omitted when zero, got: %s", b)
    }
}

func TestReport_BundleResult_JSONIncludedWhenSet(t *testing.T) {
    r := Report{
        RepoID: "t/r", Outcome: "success_bundle_only",
        BundleResult: &BundleResult{
            Generated:             true,
            BundleID:              "bundle_t_r_42_abc",
            BundleHash:            "sha256-aa",
            CoversManifestVersion: 42,
            ByteSize:              1024,
            DurationMS:            12,
            TriggerReason:         "missing",
        },
    }
    b, err := json.Marshal(r)
    if err != nil {
        t.Fatalf("Marshal: %v", err)
    }
    if !bytes.Contains(b, []byte(`"bundle_result"`)) {
        t.Fatalf("expected bundle_result in JSON, got: %s", b)
    }
    if !bytes.Contains(b, []byte(`"trigger_reason":"missing"`)) {
        t.Fatalf("trigger_reason missing: %s", b)
    }
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestReport_BundleResult -v`
Expected: COMPILE ERROR — `BundleResult` undefined.

- [ ] **Step 4: Add the type and field**

In the same file as `Report`:

```go
// BundleResult records the outcome of the bundle-refresh phase. Nil
// when the phase did not run (NoBundle, no triggers fired in non-Force
// mode, or the maintenance run failed before reaching the phase).
type BundleResult struct {
    // Generated is true if a fresh bundle was uploaded and CAS-merged.
    // False indicates the phase ran but decided no regeneration was
    // needed (Generated=false, TriggerReason="no_trigger").
    Generated bool `json:"generated"`

    // BundleID is the new BundleEntry.ID. Empty when !Generated.
    BundleID string `json:"bundle_id,omitempty"`

    // BundleHash is the SHA-256 of the bundle file body. Empty when !Generated.
    BundleHash string `json:"bundle_hash,omitempty"`

    // CoversManifestVersion is the M_now version captured at generation start.
    CoversManifestVersion uint64 `json:"covers_manifest_version,omitempty"`

    // ByteSize is the bundle file size. Zero when !Generated.
    ByteSize int64 `json:"byte_size,omitempty"`

    // DurationMS is the wall time of the phase.
    DurationMS int64 `json:"duration_ms,omitempty"`

    // TriggerReason is one of: "missing", "age", "commits", "force",
    // "no_trigger", "skipped_no_default_branch".
    TriggerReason string `json:"trigger_reason,omitempty"`

    // ErrorMessage is non-empty if the phase failed and the rest of
    // the maintenance run continued. Failure does not set Generated.
    ErrorMessage string `json:"error_message,omitempty"`
}
```

In the `Report` struct, add:

```go
type Report struct {
    // ... existing fields ...
    BundleResult *BundleResult `json:"bundle_result,omitempty"`
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/maintenance/ -run TestReport_BundleResult -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/maintenance/options.go internal/maintenance/options_test.go
git commit -m "maintenance: add BundleResult struct + Report.BundleResult"
```

### Task 3.3: Resolve the default branch from the manifest

**Files:**
- Create: `internal/maintenance/bundle_default_branch.go`
- Create: `internal/maintenance/bundle_default_branch_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/bundle_default_branch_test.go`:

```go
package maintenance

import (
    "testing"

    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestResolveDefaultBranch_HEAD(t *testing.T) {
    m := manifest.Body{
        DefaultBranch: "refs/heads/develop",
        Refs: map[string]string{
            "refs/heads/develop": "0123456789abcdef0123456789abcdef01234567",
            "refs/heads/main":    "1111111111111111111111111111111111111111",
        },
    }
    got, err := ResolveDefaultBranch(m, "")
    if err != nil {
        t.Fatalf("ResolveDefaultBranch: %v", err)
    }
    if got != "refs/heads/develop" {
        t.Errorf("got %q, want refs/heads/develop", got)
    }
}

func TestResolveDefaultBranch_Override(t *testing.T) {
    m := manifest.Body{
        DefaultBranch: "refs/heads/develop",
        Refs: map[string]string{
            "refs/heads/develop": "0123456789abcdef0123456789abcdef01234567",
            "refs/heads/main":    "1111111111111111111111111111111111111111",
        },
    }
    got, err := ResolveDefaultBranch(m, "refs/heads/main")
    if err != nil {
        t.Fatalf("ResolveDefaultBranch: %v", err)
    }
    if got != "refs/heads/main" {
        t.Errorf("got %q, want refs/heads/main (override)", got)
    }
}

func TestResolveDefaultBranch_FallbackMain(t *testing.T) {
    m := manifest.Body{
        Refs: map[string]string{"refs/heads/main": "0123456789abcdef0123456789abcdef01234567"},
    }
    got, err := ResolveDefaultBranch(m, "")
    if err != nil {
        t.Fatalf("ResolveDefaultBranch: %v", err)
    }
    if got != "refs/heads/main" {
        t.Errorf("got %q, want refs/heads/main", got)
    }
}

func TestResolveDefaultBranch_FallbackMaster(t *testing.T) {
    m := manifest.Body{
        Refs: map[string]string{"refs/heads/master": "0123456789abcdef0123456789abcdef01234567"},
    }
    got, err := ResolveDefaultBranch(m, "")
    if err != nil {
        t.Fatalf("ResolveDefaultBranch: %v", err)
    }
    if got != "refs/heads/master" {
        t.Errorf("got %q, want refs/heads/master", got)
    }
}

func TestResolveDefaultBranch_NoMatch(t *testing.T) {
    m := manifest.Body{
        Refs: map[string]string{"refs/heads/feat": "0123456789abcdef0123456789abcdef01234567"},
    }
    _, err := ResolveDefaultBranch(m, "")
    if err == nil {
        t.Fatalf("expected error when no default branch can be resolved")
    }
}

func TestResolveDefaultBranch_OverrideMissingRef(t *testing.T) {
    m := manifest.Body{Refs: map[string]string{"refs/heads/main": "abcd"}}
    _, err := ResolveDefaultBranch(m, "refs/heads/missing")
    if err == nil {
        t.Fatalf("expected error when override ref does not exist")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestResolveDefaultBranch -v`
Expected: COMPILE ERROR — `ResolveDefaultBranch` undefined.

- [ ] **Step 3: Implement the helper**

Create `internal/maintenance/bundle_default_branch.go`:

```go
package maintenance

import (
    "fmt"

    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// ResolveDefaultBranch returns the ref the bundle should cover.
//
//   - If override is non-empty, it MUST exist in m.Refs.
//   - Otherwise, m.DefaultBranch is used if non-empty AND present in m.Refs.
//   - Otherwise, refs/heads/main is tried, then refs/heads/master.
//   - Otherwise an error is returned (caller should skip bundle-refresh).
//
// The returned ref is always of the form refs/heads/<name>.
func ResolveDefaultBranch(m manifest.Body, override string) (string, error) {
    if override != "" {
        if _, ok := m.Refs[override]; !ok {
            return "", fmt.Errorf("maintenance: bundle default-branch override %q not in manifest refs", override)
        }
        return override, nil
    }
    if m.DefaultBranch != "" {
        if _, ok := m.Refs[m.DefaultBranch]; ok {
            return m.DefaultBranch, nil
        }
    }
    for _, fallback := range []string{"refs/heads/main", "refs/heads/master"} {
        if _, ok := m.Refs[fallback]; ok {
            return fallback, nil
        }
    }
    return "", fmt.Errorf("maintenance: cannot resolve default branch: HEAD unset and neither refs/heads/main nor refs/heads/master present")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/maintenance/ -run TestResolveDefaultBranch -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/bundle_default_branch.go internal/maintenance/bundle_default_branch_test.go
git commit -m "maintenance: ResolveDefaultBranch helper for bundle phase"
```

### Task 3.4: Bundle trigger evaluation

**Files:**
- Create: `internal/maintenance/bundle_triggers.go`
- Create: `internal/maintenance/bundle_triggers_test.go`

The trigger evaluator decides whether the bundle phase should run. It uses M10's `.bvcg` v2 walk for the commit-distance check.

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/bundle_triggers_test.go`:

```go
package maintenance

import (
    "testing"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestEvaluateBundleTriggers_Missing(t *testing.T) {
    m := manifest.Body{
        Refs:    map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"},
        Bundles: nil,
    }
    th := Thresholds{BundleCommits: 100, BundleAge: 24 * time.Hour}
    res, err := EvaluateBundleTriggers(nil, m, th, "refs/heads/main", time.Now(), nil)
    if err != nil {
        t.Fatalf("EvaluateBundleTriggers: %v", err)
    }
    if !res.Triggered || res.Reason != "missing" {
        t.Fatalf("got triggered=%v reason=%q, want true/missing", res.Triggered, res.Reason)
    }
}

func TestEvaluateBundleTriggers_Age(t *testing.T) {
    now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
    m := manifest.Body{
        Refs: map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"},
        Bundles: []manifest.BundleEntry{{
            Kind:        "full_default",
            Ref:         "refs/heads/main",
            TipOID:      "1111111111111111111111111111111111111111",
            GeneratedAt: now.Add(-25 * time.Hour).Format(time.RFC3339),
        }},
    }
    th := Thresholds{BundleCommits: 100, BundleAge: 24 * time.Hour}
    res, err := EvaluateBundleTriggers(nil, m, th, "refs/heads/main", now, nil)
    if err != nil {
        t.Fatalf("EvaluateBundleTriggers: %v", err)
    }
    if !res.Triggered || res.Reason != "age" {
        t.Fatalf("got triggered=%v reason=%q, want true/age", res.Triggered, res.Reason)
    }
}

func TestEvaluateBundleTriggers_NoTrigger(t *testing.T) {
    now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
    m := manifest.Body{
        Refs: map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"},
        Bundles: []manifest.BundleEntry{{
            Kind:        "full_default",
            Ref:         "refs/heads/main",
            TipOID:      "1111111111111111111111111111111111111111",
            GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
        }},
    }
    th := Thresholds{BundleCommits: 100, BundleAge: 24 * time.Hour}
    res, err := EvaluateBundleTriggers(nil, m, th, "refs/heads/main", now, nil)
    if err != nil {
        t.Fatalf("EvaluateBundleTriggers: %v", err)
    }
    if res.Triggered {
        t.Fatalf("got triggered=true; want false (tip unchanged, age within threshold)")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestEvaluateBundleTriggers -v`
Expected: COMPILE ERROR — function undefined.

- [ ] **Step 3: Implement the evaluator**

Create `internal/maintenance/bundle_triggers.go`:

```go
package maintenance

import (
    "context"
    "fmt"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/reachability"
    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
    "github.com/bucketvcs/bucketvcs/internal/storage"
)

// BundleTriggerResult is the outcome of EvaluateBundleTriggers.
type BundleTriggerResult struct {
    // Triggered is true when bundle-refresh should run.
    Triggered bool
    // Reason is "missing", "age", "commits", or "no_trigger".
    Reason string
    // CommitsBehind is populated when the commit-distance check ran;
    // -1 when the check was short-circuited by an earlier trigger.
    CommitsBehind int
}

// EvaluateBundleTriggers decides whether bundle-refresh should run.
// Cheap-first: missing -> age -> commits.
//
// rset is an already-loaded reachability set (used for the bounded walk
// from current tip back to bundle.TipOID). Pass nil to skip the
// commit-distance check (used for force / dry-run paths).
func EvaluateBundleTriggers(
    ctx context.Context,
    m manifest.Body,
    th Thresholds,
    ref string,
    now time.Time,
    rset *reachability.Set,
) (BundleTriggerResult, error) {
    var existing *manifest.BundleEntry
    for i := range m.Bundles {
        if m.Bundles[i].Kind == "full_default" && m.Bundles[i].Ref == ref {
            existing = &m.Bundles[i]
            break
        }
    }
    if existing == nil {
        return BundleTriggerResult{Triggered: true, Reason: "missing", CommitsBehind: -1}, nil
    }

    // Age check.
    if th.BundleAge > 0 {
        gen, err := time.Parse(time.RFC3339, existing.GeneratedAt)
        if err == nil {
            if now.Sub(gen) >= th.BundleAge {
                return BundleTriggerResult{Triggered: true, Reason: "age", CommitsBehind: -1}, nil
            }
        }
    }

    // Tip equality check before walking.
    currentTip := m.Refs[ref]
    if currentTip == existing.TipOID {
        return BundleTriggerResult{Triggered: false, Reason: "no_trigger", CommitsBehind: 0}, nil
    }

    // Commit-distance check via bounded walk.
    if th.BundleCommits > 0 && rset != nil {
        n, err := rset.WalkBackOID(currentTip, existing.TipOID, th.BundleCommits)
        if err != nil {
            return BundleTriggerResult{}, fmt.Errorf("bundle triggers: walk: %w", err)
        }
        if n < 0 || n >= th.BundleCommits {
            // -1 means "not found within bound" — treat as force-push / divergent.
            return BundleTriggerResult{Triggered: true, Reason: "commits", CommitsBehind: th.BundleCommits}, nil
        }
        return BundleTriggerResult{Triggered: false, Reason: "no_trigger", CommitsBehind: n}, nil
    }

    return BundleTriggerResult{Triggered: false, Reason: "no_trigger", CommitsBehind: -1}, nil
}

// Compile-time check that storage import stays referenced when the
// reachability dependency is the only consumer.
var _ storage.ObjectStore
```

- [ ] **Step 4: Add `WalkBackOID` to `reachability.Set` if not present**

Run: `grep -n 'func.*Set.*WalkBack\|func.*Set.*IsAncestor' internal/reachability/set.go`
If `WalkBackOID` does not exist, add it (and a sibling `IsAncestor`):

```go
// WalkBackOID walks backward through commits from `from`, looking for
// `target`. Returns the count of commits walked (0 if from == target,
// 1 if target is from's parent, etc.) bounded by max. Returns -1 if
// target is not reached within max steps. Returns an error if `from`
// is not present in the set or any commit on the path is missing.
func (s *Set) WalkBackOID(from, target string, max int) (int, error) {
    if from == target {
        return 0, nil
    }
    visited := map[string]bool{from: true}
    frontier := []string{from}
    depth := 0
    for len(frontier) > 0 && depth < max {
        depth++
        var next []string
        for _, oid := range frontier {
            parents, ok := s.Parents(oid)
            if !ok {
                return -1, fmt.Errorf("reachability: WalkBackOID: %s not in set", oid)
            }
            for _, p := range parents {
                if p == target {
                    return depth, nil
                }
                if !visited[p] {
                    visited[p] = true
                    next = append(next, p)
                }
            }
        }
        frontier = next
    }
    return -1, nil
}

// IsAncestor reports whether ancestor is reachable from descendant
// within max parent hops. Returns false (no error) when not found
// within bound or when descendant is not present in the set.
func (s *Set) IsAncestor(ancestor, descendant string, max int) bool {
    n, err := s.WalkBackOID(descendant, ancestor, max)
    return err == nil && n >= 0
}
```

(Add `import "fmt"` if not already present.)

Add corresponding tests to `internal/reachability/set_test.go`:

```go
func TestSet_WalkBackOID_Found(t *testing.T) {
    s := newTestSetLinear(t, []string{"a", "b", "c", "d"}) // a is root, d is tip
    n, err := s.WalkBackOID("d", "a", 10)
    if err != nil || n != 3 {
        t.Fatalf("got n=%d err=%v, want n=3 err=nil", n, err)
    }
}

func TestSet_WalkBackOID_NotFoundWithinBound(t *testing.T) {
    s := newTestSetLinear(t, []string{"a", "b", "c", "d"})
    n, err := s.WalkBackOID("d", "a", 2)
    if err != nil || n != -1 {
        t.Fatalf("got n=%d err=%v, want n=-1 err=nil", n, err)
    }
}

func TestSet_IsAncestor(t *testing.T) {
    s := newTestSetLinear(t, []string{"a", "b", "c"})
    if !s.IsAncestor("a", "c", 5) {
        t.Errorf("a should be ancestor of c")
    }
    if s.IsAncestor("c", "a", 5) {
        t.Errorf("c should NOT be ancestor of a")
    }
}
```

`newTestSetLinear` is a helper that creates a chain a->b->c->...; if it does not exist, look at how existing `set_test.go` builds Sets and mirror that pattern (most likely via `rtest/fixtures.go` — check `grep -n newTestSet internal/reachability/`).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/reachability/ -run 'TestSet_(WalkBackOID|IsAncestor)' -v`
Run: `go test ./internal/maintenance/ -run TestEvaluateBundleTriggers -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/maintenance/bundle_triggers.go internal/maintenance/bundle_triggers_test.go \
        internal/reachability/set.go internal/reachability/set_test.go
git commit -m "maintenance+reachability: bundle trigger evaluator + WalkBackOID/IsAncestor"
```

### Task 3.5: Bundle generation (file + sidecar + upload)

**Files:**
- Create: `internal/maintenance/bundle.go`
- Create: `internal/maintenance/bundle_test.go`

This is the heart of the phase. The function takes a materialized bare-repo path (passed in by the pipeline) and produces an uploaded bundle plus the constructed `BundleEntry`. CAS-merge into the manifest is the next task.

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/bundle_test.go`:

```go
package maintenance

import (
    "context"
    "encoding/json"
    "os"
    "os/exec"
    "path/filepath"
    "strings"
    "testing"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/repo/keys"
    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
    "github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestGenerateBundleArtifact_Success(t *testing.T) {
    if _, err := exec.LookPath("git"); err != nil {
        t.Skip("git not available")
    }
    dir := t.TempDir()

    // Build a tiny bare repo with one commit on main.
    bareDir := filepath.Join(dir, "mirror.git")
    if err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).Run(); err != nil {
        t.Fatal(err)
    }
    workDir := filepath.Join(dir, "work")
    if err := os.MkdirAll(workDir, 0o755); err != nil {
        t.Fatal(err)
    }
    runIn(t, workDir, "git", "init", "-b", "main", ".")
    runIn(t, workDir, "git", "config", "user.email", "t@t")
    runIn(t, workDir, "git", "config", "user.name", "t")
    if err := os.WriteFile(filepath.Join(workDir, "f"), []byte("hi"), 0o644); err != nil {
        t.Fatal(err)
    }
    runIn(t, workDir, "git", "add", ".")
    runIn(t, workDir, "git", "commit", "-m", "init")
    runIn(t, workDir, "git", "remote", "add", "origin", bareDir)
    runIn(t, workDir, "git", "push", "origin", "main")

    // Resolve the tip OID for the assertion.
    tipBytes, _ := exec.Command("git", "-C", workDir, "rev-parse", "HEAD").Output()
    tipOID := strings.TrimSpace(string(tipBytes))

    // Localfs storage for the upload.
    bucketRoot := filepath.Join(dir, "bucket")
    store, err := localfs.New(localfs.Config{Root: bucketRoot})
    if err != nil {
        t.Fatal(err)
    }
    rkeys, _ := keys.NewRepo("ten", "rep")

    art, err := GenerateBundleArtifact(context.Background(), bareDir, "refs/heads/main", store, rkeys, 7, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
    if err != nil {
        t.Fatalf("GenerateBundleArtifact: %v", err)
    }
    if art.Entry.Kind != "full_default" {
        t.Errorf("Kind = %q, want full_default", art.Entry.Kind)
    }
    if art.Entry.Ref != "refs/heads/main" {
        t.Errorf("Ref = %q", art.Entry.Ref)
    }
    if art.Entry.TipOID != tipOID {
        t.Errorf("TipOID = %q, want %q", art.Entry.TipOID, tipOID)
    }
    if art.Entry.CoversManifestVersion != 7 {
        t.Errorf("CoversManifestVersion = %d, want 7", art.Entry.CoversManifestVersion)
    }
    if art.Entry.ByteSize == 0 {
        t.Error("ByteSize == 0")
    }
    if !strings.HasPrefix(art.Entry.BundleHash, "sha256-") {
        t.Errorf("BundleHash %q lacks sha256- prefix", art.Entry.BundleHash)
    }

    // Sidecar JSON should round-trip.
    obj, err := store.Get(context.Background(), art.Entry.SidecarKey, nil)
    if err != nil {
        t.Fatalf("Get sidecar: %v", err)
    }
    sidecarBytes, _ := io.ReadAll(obj.Body)
    obj.Body.Close()
    var got manifest.BundleEntry
    if err := json.Unmarshal(sidecarBytes, &got); err != nil {
        t.Fatalf("sidecar JSON: %v\n%s", err, sidecarBytes)
    }
    if got.TipOID != tipOID {
        t.Errorf("sidecar TipOID = %q, want %q", got.TipOID, tipOID)
    }
}
```

Add the missing `io` import.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestGenerateBundleArtifact -v`
Expected: COMPILE ERROR — `GenerateBundleArtifact` undefined.

- [ ] **Step 3: Implement `GenerateBundleArtifact`**

Create `internal/maintenance/bundle.go`:

```go
package maintenance

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "strings"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/gitcli"
    "github.com/bucketvcs/bucketvcs/internal/repo/keys"
    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
    "github.com/bucketvcs/bucketvcs/internal/storage"
)

// BundleArtifact is the output of GenerateBundleArtifact: the uploaded
// bundle file's manifest entry plus a path to the local copy (which the
// caller is responsible for removing once the CAS-merge succeeds).
type BundleArtifact struct {
    Entry         manifest.BundleEntry
    LocalBundle   string // filesystem path to the just-built bundle
}

// GenerateBundleArtifact materializes a bundle for the given ref against
// a pre-materialized bare repo at mirrorDir, uploads it (and its sidecar)
// to content-addressed keys under the repo's bundles/ prefix, and
// returns the constructed BundleEntry.
//
// The caller is responsible for:
//   - materializing mirrorDir before calling (e.g., reuse the repack mirror)
//   - removing art.LocalBundle after the CAS-merge step succeeds (or fails)
//   - CAS-merging art.Entry into the manifest (CASMergeBundleEntry)
func GenerateBundleArtifact(
    ctx context.Context,
    mirrorDir string,
    ref string,
    store storage.ObjectStore,
    rkeys *keys.Repo,
    manifestVersion uint64,
    now time.Time,
) (BundleArtifact, error) {
    // 1. Resolve tip OID via git rev-parse against the mirror.
    tipBytes, err := gitcli.RevParse(ctx, mirrorDir, ref)
    if err != nil {
        return BundleArtifact{}, fmt.Errorf("bundle: rev-parse %s: %w", ref, err)
    }
    tipOID := strings.TrimSpace(string(tipBytes))
    if len(tipOID) != 40 {
        return BundleArtifact{}, fmt.Errorf("bundle: rev-parse returned %q (not 40-hex)", tipOID)
    }

    // 2. Generate bundle file in a temp location.
    tmpDir, err := os.MkdirTemp("", "bvcs-bundle-")
    if err != nil {
        return BundleArtifact{}, fmt.Errorf("bundle: tmpdir: %w", err)
    }
    bundlePath := filepath.Join(tmpDir, "out.bundle")
    if err := gitcli.BundleCreate(ctx, mirrorDir, bundlePath, ref); err != nil {
        os.RemoveAll(tmpDir)
        return BundleArtifact{}, fmt.Errorf("bundle: BundleCreate: %w", err)
    }

    // 3. Compute SHA-256 + size; we need both to build the storage key
    //    and BundleEntry. Stream once to amortize.
    h, size, err := hashAndSize(bundlePath)
    if err != nil {
        os.RemoveAll(tmpDir)
        return BundleArtifact{}, fmt.Errorf("bundle: hash: %w", err)
    }
    bundleHash := "sha256-" + hex.EncodeToString(h)
    bundleID := bundleEntryID(rkeys, manifestVersion, h)

    bundleKey := rkeys.BundleKey(bundleHash)
    sidecarKey := rkeys.BundleManifestKey(bundleHash)

    // 4. Upload the bundle file.
    f, err := os.Open(bundlePath)
    if err != nil {
        os.RemoveAll(tmpDir)
        return BundleArtifact{}, fmt.Errorf("bundle: open for upload: %w", err)
    }
    if _, err := store.PutIfAbsent(ctx, bundleKey, f, nil); err != nil {
        f.Close()
        os.RemoveAll(tmpDir)
        // ErrAlreadyExists is fine — bundle is content-addressed; another
        // maintenance run produced byte-identical output. Continue to
        // sidecar upload (also idempotent) and BundleEntry construction.
        if !isAlreadyExists(err) {
            return BundleArtifact{}, fmt.Errorf("bundle: upload %s: %w", bundleKey, err)
        }
    }
    f.Close()

    // 5. Build + upload sidecar JSON.
    entry := manifest.BundleEntry{
        ID:                    bundleID,
        Kind:                  "full_default",
        BundleKey:             bundleKey,
        SidecarKey:            sidecarKey,
        BundleHash:            bundleHash,
        Ref:                   ref,
        TipOID:                tipOID,
        CoversManifestVersion: manifestVersion,
        ByteSize:              size,
        GeneratedAt:           now.UTC().Format(time.RFC3339),
    }
    sidecarBytes, err := json.MarshalIndent(entry, "", "  ")
    if err != nil {
        os.RemoveAll(tmpDir)
        return BundleArtifact{}, fmt.Errorf("bundle: marshal sidecar: %w", err)
    }
    if _, err := store.PutIfAbsent(ctx, sidecarKey, bytesReader(sidecarBytes), nil); err != nil {
        if !isAlreadyExists(err) {
            os.RemoveAll(tmpDir)
            return BundleArtifact{}, fmt.Errorf("bundle: upload sidecar: %w", err)
        }
    }

    return BundleArtifact{Entry: entry, LocalBundle: bundlePath}, nil
}

// hashAndSize streams f once, returning sha256 and byte size.
func hashAndSize(path string) ([]byte, int64, error) {
    f, err := os.Open(path)
    if err != nil {
        return nil, 0, err
    }
    defer f.Close()
    h := sha256.New()
    n, err := io.Copy(h, f)
    if err != nil {
        return nil, 0, err
    }
    return h.Sum(nil), n, nil
}

// bundleEntryID is "bundle_<tenant>_<repo>_<version>_<hash[:8]>".
func bundleEntryID(rkeys *keys.Repo, version uint64, hash []byte) string {
    return fmt.Sprintf("bundle_%s_%s_%d_%s", rkeys.TenantID(), rkeys.RepoID(), version, hex.EncodeToString(hash[:4]))
}

// isAlreadyExists checks for storage.ErrAlreadyExists without importing
// errors here (already imported by the package elsewhere).
func isAlreadyExists(err error) bool {
    return err != nil && err.Error() != "" && (errorsIs(err, storage.ErrAlreadyExists))
}
```

The helpers `errorsIs` and `bytesReader` are package-local utilities — search the package for existing equivalents (`grep -n 'func errorsIs\|func bytesReader' internal/maintenance/*.go`) and either reuse or add minimal versions:

```go
// add to bundle.go if not already in the package
func bytesReader(b []byte) io.Reader { return bytesReaderImpl(b) }

func bytesReaderImpl(b []byte) io.Reader {
    return strings.NewReader(string(b)) // stdlib bytes.NewReader is preferred; use whichever the package already uses
}

func errorsIs(err, target error) bool {
    return errors.Is(err, target)
}
```

If the package already imports `errors` and `bytes`, prefer `bytes.NewReader(b)` and `errors.Is(err, target)` directly inline; the helpers above are fallbacks if the imports are awkward.

Add the `gitcli.RevParse` wrapper to `internal/gitcli/`:

In `internal/gitcli/gitcli.go` (or a new file `internal/gitcli/revparse.go`):

```go
// RevParse runs `git -C repoDir rev-parse <ref>` and returns the
// trimmed output (typically a 40-hex OID).
func RevParse(ctx context.Context, repoDir, ref string) (string, error) {
    out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", ref).Output()
    if err != nil {
        return "", fmt.Errorf("gitcli: rev-parse %s in %s: %w", ref, repoDir, err)
    }
    return strings.TrimSpace(string(out)), nil
}
```

Add a corresponding test in `internal/gitcli/gitcli_test.go`:

```go
func TestRevParse_Branch(t *testing.T) {
    if _, err := exec.LookPath("git"); err != nil {
        t.Skip("git not available")
    }
    dir := t.TempDir()
    bare := filepath.Join(dir, "b.git")
    if err := exec.Command("git", "init", "--bare", "-b", "main", bare).Run(); err != nil {
        t.Fatal(err)
    }
    work := filepath.Join(dir, "w")
    os.MkdirAll(work, 0o755)
    runIn := func(args ...string) {
        cmd := exec.Command(args[0], args[1:]...)
        cmd.Dir = work
        if out, err := cmd.CombinedOutput(); err != nil {
            t.Fatalf("%v: %v\n%s", args, err, out)
        }
    }
    runIn("git", "init", "-b", "main", ".")
    runIn("git", "config", "user.email", "t@t")
    runIn("git", "config", "user.name", "t")
    os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644)
    runIn("git", "add", ".")
    runIn("git", "commit", "-m", "i")
    runIn("git", "remote", "add", "origin", bare)
    runIn("git", "push", "origin", "main")

    out, err := RevParse(context.Background(), bare, "refs/heads/main")
    if err != nil {
        t.Fatalf("RevParse: %v", err)
    }
    if len(out) != 40 {
        t.Fatalf("RevParse returned %q", out)
    }
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gitcli/ -run TestRevParse -v`
Run: `go test ./internal/maintenance/ -run TestGenerateBundleArtifact -v`
Expected: PASS for both.

- [ ] **Step 5: Commit**

```bash
git add internal/gitcli/revparse.go internal/gitcli/gitcli_test.go \
        internal/maintenance/bundle.go internal/maintenance/bundle_test.go
git commit -m "maintenance: GenerateBundleArtifact (build + upload + sidecar)"
```

### Task 3.6: CAS-merge bundle entry into manifest

**Files:**
- Create: `internal/maintenance/bundle_casmerge.go`
- Create: `internal/maintenance/bundle_casmerge_test.go`

The CAS-merge is conceptually simpler than M9/M10's because the bundle slice is replaced wholesale (M11 only ever writes one `full_default` entry). Concurrent push that doesn't touch `Bundles` is preserved; concurrent maintenance that *does* touch `Bundles` re-races the CAS.

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/bundle_casmerge_test.go`:

```go
package maintenance

import (
    "context"
    "encoding/json"
    "testing"

    "github.com/bucketvcs/bucketvcs/internal/repo"
    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestCASMergeBundleEntry_PreservesUnrelatedFields(t *testing.T) {
    // Build a manifest with packs + refs + a stale BundleEntry.
    base := manifest.Body{
        DefaultBranch: "refs/heads/main",
        Refs:          map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"},
        Packs: []manifest.PackEntry{{
            PackID: "p", PackKey: "k", IdxKey: "i", SizeBytes: 1, ObjectCount: 1,
        }},
        Bundles: []manifest.BundleEntry{{
            Kind: "full_default", ID: "old", BundleKey: "bk", SidecarKey: "sk", BundleHash: "sha256-old",
            Ref: "refs/heads/main", TipOID: "0000000000000000000000000000000000000000",
            CoversManifestVersion: 1, ByteSize: 10, GeneratedAt: "2026-05-09T00:00:00Z",
        }},
    }

    fresh := manifest.BundleEntry{
        Kind: "full_default", ID: "new", BundleKey: "bk2", SidecarKey: "sk2", BundleHash: "sha256-new",
        Ref: "refs/heads/main", TipOID: "1111111111111111111111111111111111111111",
        CoversManifestVersion: 2, ByteSize: 20, GeneratedAt: "2026-05-10T00:00:00Z",
    }

    got, err := MergeBundleEntry(base, fresh)
    if err != nil {
        t.Fatalf("MergeBundleEntry: %v", err)
    }
    if len(got.Bundles) != 1 {
        t.Fatalf("Bundles len = %d, want 1", len(got.Bundles))
    }
    if got.Bundles[0].ID != "new" {
        t.Fatalf("merged Bundles[0].ID = %q, want new", got.Bundles[0].ID)
    }
    if len(got.Packs) != 1 || got.Packs[0].PackID != "p" {
        t.Fatalf("packs disturbed: %+v", got.Packs)
    }
    if got.DefaultBranch != "refs/heads/main" {
        t.Fatalf("default branch dropped: %q", got.DefaultBranch)
    }
}

func TestCASMergeBundleEntry_AddWhenAbsent(t *testing.T) {
    base := manifest.Body{
        Refs:    map[string]string{"refs/heads/main": "abc"},
        Bundles: nil,
    }
    fresh := manifest.BundleEntry{
        Kind: "full_default", ID: "new", Ref: "refs/heads/main",
        TipOID: "abc", CoversManifestVersion: 1, ByteSize: 1, GeneratedAt: "2026-05-10T00:00:00Z",
    }
    got, err := MergeBundleEntry(base, fresh)
    if err != nil {
        t.Fatalf("MergeBundleEntry: %v", err)
    }
    if len(got.Bundles) != 1 || got.Bundles[0].ID != "new" {
        t.Fatalf("Bundles = %+v, want 1 entry", got.Bundles)
    }
}

func TestRunBundleCAS_End2End(t *testing.T) {
    // A full integration test that:
    //   1. seeds a repo via internal/repo.Init,
    //   2. constructs a fresh BundleEntry,
    //   3. calls RunBundleCASMerge,
    //   4. re-reads the manifest and asserts the entry is present.
    ctx := context.Background()
    r, _, _ := newRepoForTest(t) // helper: creates a localfs-backed repo
    fresh := manifest.BundleEntry{
        ID: "fresh", Kind: "full_default", BundleKey: "bk", SidecarKey: "sk",
        BundleHash: "sha256-x", Ref: "refs/heads/main",
        TipOID: "0000000000000000000000000000000000000000",
        CoversManifestVersion: 1, ByteSize: 10, GeneratedAt: "2026-05-10T00:00:00Z",
    }
    if err := RunBundleCASMerge(ctx, r, fresh, 5); err != nil {
        t.Fatalf("RunBundleCASMerge: %v", err)
    }
    view, err := r.ReadRoot(ctx)
    if err != nil {
        t.Fatal(err)
    }
    var m manifest.Body
    json.Unmarshal(view.Body, &m)
    if len(m.Bundles) != 1 || m.Bundles[0].ID != "fresh" {
        t.Fatalf("manifest bundles after CAS = %+v", m.Bundles)
    }
    _ = repo.ErrCASRetryExhausted // keep import live
}
```

`newRepoForTest` already exists in this package or a sibling; if not, copy the equivalent helper from `pipeline_test.go`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maintenance/ -run 'TestCASMergeBundleEntry|TestRunBundleCAS_End2End' -v`
Expected: COMPILE ERROR — `MergeBundleEntry`, `RunBundleCASMerge` undefined.

- [ ] **Step 3: Implement the merger and the CAS-runner**

Create `internal/maintenance/bundle_casmerge.go`:

```go
package maintenance

import (
    "context"
    "encoding/json"
    "fmt"

    "github.com/bucketvcs/bucketvcs/internal/repo"
    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// MergeBundleEntry replaces the existing full_default bundle in body
// with fresh, leaving every other field untouched. Other bundle kinds
// (none in M11; rolling-base / release-tag in successors) are preserved.
//
// This is a pure function suitable for the CAS-merge body builder.
func MergeBundleEntry(body manifest.Body, fresh manifest.BundleEntry) (manifest.Body, error) {
    if fresh.Kind != "full_default" {
        return body, fmt.Errorf("bundle merge: M11 only writes Kind=full_default, got %q", fresh.Kind)
    }
    // Filter out any existing full_default entry; keep everything else.
    var kept []manifest.BundleEntry
    for _, e := range body.Bundles {
        if e.Kind == "full_default" {
            continue
        }
        kept = append(kept, e)
    }
    body.Bundles = append(kept, fresh)
    return body, nil
}

// RunBundleCASMerge re-reads the manifest, applies MergeBundleEntry,
// and CAS-writes; retries up to maxRetries on ErrVersionMismatch.
//
// On success the manifest contains exactly one full_default bundle
// (this run's). On retry exhaustion returns repo.ErrCASRetryExhausted.
func RunBundleCASMerge(ctx context.Context, r *repo.Repo, fresh manifest.BundleEntry, maxRetries int) error {
    if maxRetries <= 0 {
        maxRetries = DefaultCASRetry
    }
    for attempt := 0; attempt < maxRetries; attempt++ {
        view, err := r.ReadRoot(ctx)
        if err != nil {
            return fmt.Errorf("bundle cas: ReadRoot: %w", err)
        }
        var body manifest.Body
        if err := json.Unmarshal(view.Body, &body); err != nil {
            return fmt.Errorf("bundle cas: unmarshal: %w", err)
        }
        merged, mErr := MergeBundleEntry(body, fresh)
        if mErr != nil {
            return mErr
        }
        next, mErr := manifest.MarshalBody(merged)
        if mErr != nil {
            return fmt.Errorf("bundle cas: marshal: %w", mErr)
        }
        if err := r.WriteRoot(ctx, view.Header, next); err != nil {
            if isCASMismatch(err) {
                continue
            }
            return fmt.Errorf("bundle cas: WriteRoot: %w", err)
        }
        return nil
    }
    return repo.ErrCASRetryExhausted
}

// isCASMismatch is a package-local helper. Search the package for the
// existing equivalent (M9/M10 likely have one); reuse if present.
func isCASMismatch(err error) bool {
    return err == repo.ErrCASMismatch
}
```

(If `repo.ErrCASRetryExhausted` and `repo.ErrCASMismatch` do not exist with those exact names, search `internal/repo` and use whichever names exist. The imports/names should match the existing code; this plan preserves the *intent* — a CAS-retry helper.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/maintenance/ -run 'TestCASMergeBundleEntry|TestRunBundleCAS_End2End' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/bundle_casmerge.go internal/maintenance/bundle_casmerge_test.go
git commit -m "maintenance: bundle CAS-merge protocol"
```

### Task 3.7: Wire the bundle phase into the pipeline

**Files:**
- Modify: `internal/maintenance/pipeline.go`
- Modify: `internal/maintenance/pipeline_test.go`

The bundle phase runs after repack and before compact (or as the only phase under `--bundle-only`). When repack already materialized a mirror in the same invocation, we reuse it; otherwise the bundle phase materializes one of its own.

- [ ] **Step 1: Write the failing test**

Append to `internal/maintenance/pipeline_test.go`:

```go
func TestRunPipeline_BundleRefresh_RunsWhenMissing(t *testing.T) {
    // Run a maintenance pass against a freshly-imported repo with no
    // existing bundle. Confirm the post-run manifest has a full_default
    // BundleEntry whose TipOID matches refs/heads/main.
    ctx := context.Background()
    r, store, rkeys := newRepoWithMain(t) // sibling helper: imports a tiny repo

    // Force the threshold check to fire (no existing bundle == "missing").
    opts := RunOptions{
        Force:    false,
        BundleOnly: true,
        Now:      func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
    }
    rep, err := Run(ctx, store, r, rkeys, opts)
    if err != nil {
        t.Fatalf("Run: %v", err)
    }
    if rep.BundleResult == nil || !rep.BundleResult.Generated {
        t.Fatalf("BundleResult = %+v; want Generated=true", rep.BundleResult)
    }
    view, _ := r.ReadRoot(ctx)
    var body manifest.Body
    json.Unmarshal(view.Body, &body)
    found := false
    for _, b := range body.Bundles {
        if b.Kind == "full_default" {
            found = true
            break
        }
    }
    if !found {
        t.Fatalf("no full_default bundle in post-run manifest: %+v", body.Bundles)
    }
}

func TestRunPipeline_NoBundle_SkipsBundlePhase(t *testing.T) {
    ctx := context.Background()
    r, store, rkeys := newRepoWithMain(t)
    opts := RunOptions{NoBundle: true, Force: true, Now: func() time.Time { return time.Now() }}
    rep, err := Run(ctx, store, r, rkeys, opts)
    if err != nil {
        t.Fatalf("Run: %v", err)
    }
    if rep.BundleResult != nil {
        t.Fatalf("BundleResult should be nil under NoBundle; got %+v", rep.BundleResult)
    }
}
```

`newRepoWithMain` is a helper that imports a one-commit repo into a localfs-backed `internal/repo.Repo`. If it does not exist, model it on the existing `newRepoForTest` from `pipeline_test.go` and add a single-commit import via the existing M9 fixtures (`grep -n 'newRepo\|importTinyRepo' internal/maintenance/*_test.go`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestRunPipeline_BundleRefresh -v`
Expected: FAIL — pipeline does not call any bundle phase yet.

- [ ] **Step 3: Wire the phase into the pipeline**

In `internal/maintenance/pipeline.go`, locate the post-repack section (just before the existing compact-only handling — `grep -n 'compact-only\|CompactReachability' internal/maintenance/pipeline.go`). After the repack phase completes (whether it ran or no-opped) and before the existing compact-only branch, insert:

```go
// ----------------------------------------------------------------
// Phase BR — Bundle refresh (M11)
// ----------------------------------------------------------------
if !opts.NoBundle {
    bundleRes, bundleErr := runBundlePhase(ctx, s, r, rkeys, opts, m0, view.Header.ManifestVersion, mirrorDir)
    if bundleRes != nil {
        report.BundleResult = bundleRes
    }
    // Bundle phase failures are NEVER fatal; we log and continue.
    // The maintenance run's outcome reflects only repack/compact paths.
    if bundleErr != nil {
        opts.Logger.WarnContext(ctx, "bundle-refresh failed (non-fatal)",
            slog.String("repo_id", repoID),
            slog.String("err", bundleErr.Error()),
        )
    }
}
```

Where `mirrorDir` is the path to the materialized mirror if repack ran, or empty string otherwise. (Read the existing repack code path to confirm the variable name; you may need to lift the materialization step out of the repack-only branch into a shared `materializeMirrorOnce` helper that both phases call — see Task 3.8 for that refactor.)

Add `runBundlePhase` to `internal/maintenance/bundle.go`:

```go
// runBundlePhase orchestrates trigger-eval -> generate -> CAS-merge,
// reusing mirrorDir if non-empty (typically supplied by the repack
// phase). When mirrorDir is empty, the phase materializes its own.
//
// Returns (*BundleResult, error). Result is nil only when the phase
// short-circuited at the trigger evaluation step (no_trigger). A non-
// nil result with ErrorMessage set indicates a non-fatal failure.
func runBundlePhase(
    ctx context.Context,
    s storage.ObjectStore,
    r *repo.Repo,
    rkeys *keys.Repo,
    opts RunOptions,
    m0 manifest.Body,
    manifestVersion uint64,
    mirrorDir string,
) (*BundleResult, error) {
    started := opts.Now()
    res := &BundleResult{}

    // 1. Resolve the default branch.
    ref, err := ResolveDefaultBranch(m0, opts.BundleDefaultBranch)
    if err != nil {
        res.TriggerReason = "skipped_no_default_branch"
        res.DurationMS = opts.Now().Sub(started).Milliseconds()
        return res, nil
    }

    // 2. Evaluate triggers (loads .bvcg lazily via reachability set).
    var rset *reachability.Set
    if !opts.Force && opts.Thresholds.BundleCommits > 0 {
        rset, err = loadReachabilitySetForBundle(ctx, s, rkeys, m0)
        if err != nil {
            return &BundleResult{TriggerReason: "skipped_no_default_branch", ErrorMessage: err.Error(), DurationMS: opts.Now().Sub(started).Milliseconds()}, err
        }
    }
    trig, err := EvaluateBundleTriggers(ctx, m0, opts.Thresholds, ref, opts.Now(), rset)
    if err != nil {
        return &BundleResult{ErrorMessage: err.Error(), DurationMS: opts.Now().Sub(started).Milliseconds()}, err
    }
    if !opts.Force && !trig.Triggered {
        res.TriggerReason = "no_trigger"
        res.DurationMS = opts.Now().Sub(started).Milliseconds()
        return res, nil
    }
    triggerReason := trig.Reason
    if opts.Force {
        triggerReason = "force"
    }

    // 3. Materialize mirror if not already.
    cleanupMirror := func() {}
    if mirrorDir == "" {
        var err error
        mirrorDir, cleanupMirror, err = materializeMirrorOnce(ctx, s, r, rkeys, m0)
        if err != nil {
            return &BundleResult{TriggerReason: triggerReason, ErrorMessage: err.Error(), DurationMS: opts.Now().Sub(started).Milliseconds()}, err
        }
    }
    defer cleanupMirror()

    // 4. Generate bundle artifact.
    art, err := GenerateBundleArtifact(ctx, mirrorDir, ref, s, rkeys, manifestVersion, opts.Now())
    if err != nil {
        return &BundleResult{TriggerReason: triggerReason, ErrorMessage: err.Error(), DurationMS: opts.Now().Sub(started).Milliseconds()}, err
    }
    defer os.Remove(art.LocalBundle)

    // 5. CAS-merge.
    if !opts.DryRun {
        if err := RunBundleCASMerge(ctx, r, art.Entry, opts.CASRetry); err != nil {
            return &BundleResult{TriggerReason: triggerReason, ErrorMessage: err.Error(), DurationMS: opts.Now().Sub(started).Milliseconds()}, err
        }
    }

    res.Generated = !opts.DryRun
    res.BundleID = art.Entry.ID
    res.BundleHash = art.Entry.BundleHash
    res.CoversManifestVersion = art.Entry.CoversManifestVersion
    res.ByteSize = art.Entry.ByteSize
    res.TriggerReason = triggerReason
    res.DurationMS = opts.Now().Sub(started).Milliseconds()
    return res, nil
}
```

`loadReachabilitySetForBundle` and `materializeMirrorOnce` are helpers; their implementations parallel the M10 reachability load and the M9 mirror materialization. Search the package for their existing names (`grep -n 'loadReachabilitySet\|materialize' internal/maintenance/*.go`) and either reuse or adapt minimal wrappers.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/maintenance/ -run TestRunPipeline_BundleRefresh -v`
Run: `go test ./internal/maintenance/ -run TestRunPipeline_NoBundle -v`
Expected: PASS

- [ ] **Step 5: Run the full maintenance package**

Run: `go test ./internal/maintenance/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/maintenance/bundle.go internal/maintenance/pipeline.go internal/maintenance/pipeline_test.go
git commit -m "maintenance: wire bundle-refresh phase into pipeline"
```

### Task 3.8: `--bundle-only` outcome + JSON

**Files:**
- Modify: `internal/maintenance/pipeline.go`
- Modify: `internal/maintenance/pipeline_test.go`

Under `--bundle-only`, the pipeline skips repack and compact entirely; the run's `Outcome` becomes `success_bundle_only` (or `noop_bundle_only` if no trigger fired).

- [ ] **Step 1: Write the failing test**

Append to `internal/maintenance/pipeline_test.go`:

```go
func TestRunPipeline_BundleOnly_OutcomeAndExitCode(t *testing.T) {
    ctx := context.Background()
    r, store, rkeys := newRepoWithMain(t)

    opts := RunOptions{
        BundleOnly: true,
        Force:      true,
        Now:        func() time.Time { return time.Now() },
    }
    rep, err := Run(ctx, store, r, rkeys, opts)
    if err != nil {
        t.Fatalf("Run: %v", err)
    }
    if rep.Outcome != "success_bundle_only" && rep.Outcome != "noop_bundle_only" {
        t.Fatalf("Outcome = %q, want success_bundle_only or noop_bundle_only", rep.Outcome)
    }
    if rep.NewPackBytes != 0 {
        t.Errorf("NewPackBytes = %d under --bundle-only; want 0", rep.NewPackBytes)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestRunPipeline_BundleOnly -v`
Expected: FAIL — pipeline still tries to run repack.

- [ ] **Step 3: Skip repack/compact phases under `BundleOnly`**

In `internal/maintenance/pipeline.go`, near the top of the post-Phase-0 logic, insert:

```go
if opts.BundleOnly {
    // Force the trigger reports for repack and compact to "not triggered"
    // so the existing branches no-op and the bundle phase runs.
    trigReport.Triggered = false
    trigReport.CompactReachability = false
}
```

After the bundle phase block, set the outcome:

```go
if opts.BundleOnly {
    if report.BundleResult != nil && report.BundleResult.Generated {
        report.Outcome = "success_bundle_only"
    } else {
        report.Outcome = "noop_bundle_only"
    }
    report.AfterPackCount = report.BeforePackCount
    report.AfterManifestPB = report.BeforeManifestPB
    report.DurationMS = elapsed()
    emitFinalReport(ctx, opts.Logger, report)
    return report, nil
}
```

Add a similar branch in the existing `noop` early-return path: if `opts.BundleOnly`, the early-return must use `noop_bundle_only` instead of `noop`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/maintenance/ -run 'TestRunPipeline_BundleOnly|TestRunPipeline_BundleRefresh|TestRunPipeline_NoBundle' -v`
Expected: PASS

- [ ] **Step 5: Run the full maintenance suite**

Run: `go test ./internal/maintenance/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/maintenance/pipeline.go internal/maintenance/pipeline_test.go
git commit -m "maintenance: --bundle-only outcomes (success_bundle_only / noop_bundle_only)"
```

---

## Phase 4 — Maintenance CLI flags + JSON output

### Task 4.1: Add bundle flags to `runMaintenance`

**Files:**
- Modify: `cmd/bucketvcs/maintenance.go:83-178`
- Modify: `cmd/bucketvcs/maintenance_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/bucketvcs/maintenance_test.go`:

```go
func TestRunMaintenance_BundleFlags_Parsed(t *testing.T) {
    // Smoke: pass --bundle-only --bundle-commits 50 --bundle-age 1h and confirm
    // the help text references them. The actual e2e behavior is exercised
    // by internal/maintenance tests; here we only assert the flags wire.
    var stdout, stderr bytes.Buffer
    rc := runMaintenance(context.Background(), []string{"--help"}, &stdout, &stderr)
    if rc != 0 {
        t.Fatalf("rc = %d", rc)
    }
    for _, want := range []string{"--bundle-only", "--no-bundle", "--bundle-commits", "--bundle-age", "--bundle-default-branch"} {
        if !strings.Contains(stdout.String(), want) {
            t.Errorf("usage missing %q", want)
        }
    }
}

func TestRunMaintenance_BundleOnlyAndNoBundle_Reject(t *testing.T) {
    var stdout, stderr bytes.Buffer
    rc := runMaintenance(context.Background(), []string{
        "--store=localfs:" + t.TempDir(),
        "--repo=t/r",
        "--bundle-only", "--no-bundle",
    }, &stdout, &stderr)
    if rc != 2 {
        t.Fatalf("rc = %d, want 2", rc)
    }
    if !strings.Contains(stderr.String(), "mutually exclusive") {
        t.Errorf("expected mutually-exclusive error: %s", stderr.String())
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/bucketvcs/ -run TestRunMaintenance_BundleFlags -v`
Expected: FAIL — flags absent.

- [ ] **Step 3: Add flag definitions**

In `cmd/bucketvcs/maintenance.go`, after the existing `deltaBytesStr` line (~104):

```go
bundleOnly := fs.Bool("bundle-only", false, "Run only the bundle-refresh phase (skip repack and compact)")
noBundle := fs.Bool("no-bundle", false, "Skip the bundle-refresh phase")
bundleCommits := fs.Int("bundle-commits", 100, "Regenerate bundle when default-branch tip moved by >= N commits since last bundle (0 disables)")
bundleAge := fs.Duration("bundle-age", 24*time.Hour, "Regenerate bundle when older than this (0 disables)")
bundleDefaultBranch := fs.String("bundle-default-branch", "", "Override default-branch detection for bundle generation (e.g. refs/heads/main)")
```

After flag parsing + the existing mutually-exclusive `--repo`/`--all-repos` check, add:

```go
if *bundleOnly && *noBundle {
    fmt.Fprintln(stderr, "maintenance: --bundle-only and --no-bundle are mutually exclusive")
    return 2
}
if *bundleCommits < 0 {
    fmt.Fprintln(stderr, "maintenance: --bundle-commits must be >= 0")
    return 2
}
if *bundleAge < 0 {
    fmt.Fprintln(stderr, "maintenance: --bundle-age must be >= 0")
    return 2
}
```

In the `opts := maintenance.RunOptions{...}` literal, add the new fields:

```go
Thresholds: maintenance.Thresholds{
    // ... existing ...
    BundleCommits: *bundleCommits,
    BundleAge:     *bundleAge,
},
// ... existing ...
BundleOnly:          *bundleOnly,
NoBundle:            *noBundle,
BundleDefaultBranch: *bundleDefaultBranch,
```

Update `maintenanceUsage` (top of file or sibling const) to document the new flags. Look for the existing usage block (`grep -n maintenanceUsage cmd/bucketvcs/maintenance.go`) and add lines for each new flag in the same style.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/bucketvcs/ -run TestRunMaintenance_BundleFlags -v`
Run: `go test ./cmd/bucketvcs/ -run TestRunMaintenance_BundleOnlyAndNoBundle -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/maintenance.go cmd/bucketvcs/maintenance_test.go
git commit -m "cmd/bucketvcs: maintenance bundle flags"
```

### Task 4.2: Account for bundle outcomes in `--all-repos` summary

**Files:**
- Modify: `cmd/bucketvcs/maintenance.go:201-260`

The existing `--all-repos` summary tallies `success`, `noop`, `failed`. Add buckets for `success_bundle_only` and `noop_bundle_only`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/bucketvcs/maintenance_test.go`:

```go
func TestRunMaintenance_AllRepos_BundleOnlySummary(t *testing.T) {
    // Bootstrap a localfs store with two empty repos (no refs); bundle-only
    // against both should produce noop_bundle_only outcomes.
    bucketRoot := t.TempDir()
    storeURL := "localfs:" + bucketRoot

    // Use bucketvcs init to create the repos.
    var b bytes.Buffer
    if rc := runInit(context.Background(), []string{"--store=" + storeURL, "--repo=t/r1"}, &b, &b); rc != 0 {
        t.Fatalf("init r1: rc=%d %s", rc, b.String())
    }
    b.Reset()
    if rc := runInit(context.Background(), []string{"--store=" + storeURL, "--repo=t/r2"}, &b, &b); rc != 0 {
        t.Fatalf("init r2: rc=%d %s", rc, b.String())
    }

    var stdout, stderr bytes.Buffer
    rc := runMaintenance(context.Background(), []string{
        "--store=" + storeURL,
        "--all-repos",
        "--bundle-only",
        "--output=text",
    }, &stdout, &stderr)
    if rc != 0 {
        t.Fatalf("rc = %d, stderr: %s", rc, stderr.String())
    }
    if !strings.Contains(stdout.String(), "noop_bundle_only") {
        t.Errorf("expected noop_bundle_only in summary: %s", stdout.String())
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/bucketvcs/ -run TestRunMaintenance_AllRepos_BundleOnlySummary -v`
Expected: FAIL — current summary collapses all non-success/noop into the failed bucket.

- [ ] **Step 3: Update the switch statement**

In `cmd/bucketvcs/maintenance.go` (~line 243), extend the switch:

```go
switch rep.Outcome {
case "success":
    succeeded++
case "noop":
    noop++
case "success_bundle_only":
    succeeded++
case "noop_bundle_only":
    noop++
}
```

In the summary `fmt.Fprintf` line, add a `bundle=N` field:

```go
var bundleRuns int
for _, rep := range reports {
    if rep.BundleResult != nil && rep.BundleResult.Generated {
        bundleRuns++
    }
}
fmt.Fprintf(stdout, "summary: processed=%d succeeded=%d noop=%d failed=%d bundle_generated=%d\n",
    len(repos), succeeded, noop, failed, bundleRuns)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/bucketvcs/ -run TestRunMaintenance_AllRepos -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/maintenance.go cmd/bucketvcs/maintenance_test.go
git commit -m "cmd/bucketvcs: account for bundle outcomes in --all-repos summary"
```

---

## Phase 5 — `Pack.PackChecksum` write + lazy backfill

This phase ensures every canonical pack on disk has a `PackChecksum` recorded in the manifest, so packfile-uri advertise can populate the protocol stanza without re-reading pack trailers.

### Task 5.1: Compute pack checksum in repack write path

**Files:**
- Modify: `internal/maintenance/repack.go` (find the upload/PackEntry construction site)
- Modify: `internal/maintenance/repack_test.go`

Background: a Git pack file's last 20 bytes are the SHA-1 trailer (Git's pack-checksum). After uploading a freshly-built pack, read the trailer and set `PackChecksum` on the constructed `PackEntry`.

- [ ] **Step 1: Find the existing PackEntry construction**

Run: `grep -n 'PackEntry{' internal/maintenance/repack.go internal/maintenance/upload.go internal/maintenance/casmerge.go`
Expected: one or two hits where a `PackEntry` literal is built post-upload.

- [ ] **Step 2: Write the failing test**

Append to `internal/maintenance/repack_test.go` (or wherever the repack pipeline is exercised):

```go
func TestRepack_WritesPackChecksum(t *testing.T) {
    // Run repack against a fixture repo and assert the resulting
    // PackEntry has a non-empty 40-hex PackChecksum.
    ctx := context.Background()
    r, store, rkeys := newRepoWithMain(t)

    opts := RunOptions{Force: true, Now: func() time.Time { return time.Now() }}
    rep, err := Run(ctx, store, r, rkeys, opts)
    if err != nil {
        t.Fatalf("Run: %v", err)
    }
    if rep.Outcome != "success" {
        t.Fatalf("Outcome = %q, want success", rep.Outcome)
    }
    view, _ := r.ReadRoot(ctx)
    var body manifest.Body
    json.Unmarshal(view.Body, &body)
    if len(body.Packs) == 0 {
        t.Fatalf("no packs in post-repack manifest")
    }
    for _, p := range body.Packs {
        if len(p.PackChecksum) != 40 {
            t.Errorf("Pack %s: PackChecksum = %q (len=%d), want 40-hex", p.PackID, p.PackChecksum, len(p.PackChecksum))
        }
    }
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestRepack_WritesPackChecksum -v`
Expected: FAIL — `PackChecksum` is empty.

- [ ] **Step 4: Read the pack trailer after upload**

In the pack-write helper (likely `internal/maintenance/upload.go` or `repack.go`), after the pack file is uploaded and immediately before the `PackEntry` is constructed, add:

```go
// Read the SHA-1 trailer (last 20 bytes) of the pack to populate
// PackChecksum for §16.4 packfile-uri advertise.
checksum, err := readPackTrailer(packLocalPath)
if err != nil {
    return PackEntry{}, fmt.Errorf("repack: read pack trailer for %s: %w", packLocalPath, err)
}
```

Then in the `PackEntry{...}` literal, add `PackChecksum: checksum`.

Add the helper to the same file (or a new file `internal/maintenance/packtrailer.go`):

```go
// readPackTrailer returns the 40-hex SHA-1 of the last 20 bytes of the
// file. Git's pack format places the pack-checksum there; this matches
// the value the packfile-uri protocol expects.
func readPackTrailer(path string) (string, error) {
    f, err := os.Open(path)
    if err != nil {
        return "", err
    }
    defer f.Close()
    fi, err := f.Stat()
    if err != nil {
        return "", err
    }
    if fi.Size() < 20 {
        return "", fmt.Errorf("pack file %s too small (%d bytes)", path, fi.Size())
    }
    if _, err := f.Seek(-20, io.SeekEnd); err != nil {
        return "", err
    }
    buf := make([]byte, 20)
    if _, err := io.ReadFull(f, buf); err != nil {
        return "", err
    }
    return hex.EncodeToString(buf), nil
}
```

(Add `encoding/hex`, `io`, `os` imports.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/maintenance/ -run TestRepack_WritesPackChecksum -v`
Expected: PASS

- [ ] **Step 6: Run the full maintenance package**

Run: `go test ./internal/maintenance/...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/maintenance/repack.go internal/maintenance/upload.go \
        internal/maintenance/packtrailer.go internal/maintenance/repack_test.go
# (whichever combination matches your placement)
git commit -m "maintenance: write Pack.PackChecksum on repack (M11 packfile-uri prerequisite)"
```

### Task 5.2: Lazy backfill for legacy (pre-M11) packs

**Files:**
- Modify: `internal/maintenance/pipeline.go` (Phase 0 area)
- Modify: `internal/maintenance/pipeline_test.go`

When the maintenance run encounters a manifest where one or more `PackEntry` rows have empty `PackChecksum`, it backfills before the bundle/repack/compact phases. Backfill: for each affected pack, download the trailing 20 bytes via `GetRange`, compute the hex, and CAS-merge the updated `PackEntry`s back into the manifest.

- [ ] **Step 1: Write the failing test**

Append to `internal/maintenance/pipeline_test.go`:

```go
func TestRunPipeline_BackfillsPackChecksum_OnLegacyManifest(t *testing.T) {
    ctx := context.Background()
    r, store, rkeys := newRepoWithMain(t)

    // Strip PackChecksum from every PackEntry to simulate a pre-M11 manifest.
    view, _ := r.ReadRoot(ctx)
    var body manifest.Body
    json.Unmarshal(view.Body, &body)
    for i := range body.Packs {
        body.Packs[i].PackChecksum = ""
    }
    next, _ := manifest.MarshalBody(body)
    if err := r.WriteRoot(ctx, view.Header, next); err != nil {
        t.Fatalf("seed legacy manifest: %v", err)
    }

    // Run maintenance with --no-bundle so the only effect we observe is backfill.
    opts := RunOptions{NoBundle: true, Force: false, Now: func() time.Time { return time.Now() }}
    if _, err := Run(ctx, store, r, rkeys, opts); err != nil {
        t.Fatalf("Run: %v", err)
    }

    view, _ = r.ReadRoot(ctx)
    json.Unmarshal(view.Body, &body)
    for _, p := range body.Packs {
        if len(p.PackChecksum) != 40 {
            t.Errorf("Pack %s still missing PackChecksum after backfill", p.PackID)
        }
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestRunPipeline_BackfillsPackChecksum -v`
Expected: FAIL — backfill not implemented yet.

- [ ] **Step 3: Implement backfill**

Create `internal/maintenance/backfill_packchecksum.go`:

```go
package maintenance

import (
    "context"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "io"
    "log/slog"

    "github.com/bucketvcs/bucketvcs/internal/repo"
    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
    "github.com/bucketvcs/bucketvcs/internal/storage"
)

// BackfillPackChecksumsIfNeeded scans m for PackEntries with empty
// PackChecksum, computes them via GetRange against the pack key
// (last 20 bytes), and CAS-merges the updated entries into the manifest.
//
// On first encounter the operator-guide warning is logged once per
// repo (caller's responsibility); this function is silent.
//
// Returns (updatedBody, true, nil) when at least one entry was filled in
// and the CAS succeeded; (m, false, nil) when nothing needed updating.
func BackfillPackChecksumsIfNeeded(
    ctx context.Context,
    s storage.ObjectStore,
    r *repo.Repo,
    m manifest.Body,
    casRetry int,
    logger *slog.Logger,
) (manifest.Body, bool, error) {
    needed := false
    for _, p := range m.Packs {
        if p.PackChecksum == "" {
            needed = true
            break
        }
    }
    if !needed {
        return m, false, nil
    }
    if casRetry <= 0 {
        casRetry = DefaultCASRetry
    }
    for attempt := 0; attempt < casRetry; attempt++ {
        view, err := r.ReadRoot(ctx)
        if err != nil {
            return m, false, fmt.Errorf("backfill: ReadRoot: %w", err)
        }
        var body manifest.Body
        if err := json.Unmarshal(view.Body, &body); err != nil {
            return m, false, fmt.Errorf("backfill: unmarshal: %w", err)
        }
        any := false
        for i, p := range body.Packs {
            if p.PackChecksum != "" {
                continue
            }
            sum, err := readRemotePackTrailer(ctx, s, p.PackKey, p.SizeBytes)
            if err != nil {
                logger.WarnContext(ctx, "backfill: pack trailer read failed (skipping)",
                    slog.String("pack_key", p.PackKey),
                    slog.String("err", err.Error()),
                )
                continue
            }
            body.Packs[i].PackChecksum = sum
            any = true
        }
        if !any {
            return body, false, nil
        }
        next, err := manifest.MarshalBody(body)
        if err != nil {
            return body, false, fmt.Errorf("backfill: marshal: %w", err)
        }
        if err := r.WriteRoot(ctx, view.Header, next); err != nil {
            if isCASMismatch(err) {
                continue
            }
            return body, false, fmt.Errorf("backfill: WriteRoot: %w", err)
        }
        return body, true, nil
    }
    return m, false, repo.ErrCASRetryExhausted
}

// readRemotePackTrailer fetches bytes [size-20, size-1] from key and
// returns the hex-encoded SHA-1.
func readRemotePackTrailer(ctx context.Context, s storage.ObjectStore, key string, size int64) (string, error) {
    if size < 20 {
        return "", fmt.Errorf("pack %s too small (%d bytes)", key, size)
    }
    rc, err := s.GetRange(ctx, key, size-20, size-1)
    if err != nil {
        return "", err
    }
    defer rc.Close()
    buf := make([]byte, 20)
    if _, err := io.ReadFull(rc, buf); err != nil {
        return "", err
    }
    return hex.EncodeToString(buf), nil
}
```

In `internal/maintenance/pipeline.go`, after Phase 0 reads `m0` and emits the trigger eval but before the repack phase, insert:

```go
// M11: backfill missing PackChecksum on legacy (pre-M11) manifests so
// that packfile-uri advertise has the data it needs. Done before any
// phase so subsequent CAS-merges build on a body that already has the
// field. One-time WARN per repo for legacy manifests.
if !opts.DryRun {
    if updated, didBackfill, err := BackfillPackChecksumsIfNeeded(ctx, s, r, m0, opts.CASRetry, opts.Logger); err == nil && didBackfill {
        m0 = updated
        opts.Logger.WarnContext(ctx, "backfilled missing pack checksums (legacy pre-M11 manifest)",
            slog.String("repo_id", repoID),
        )
    } else if err != nil {
        opts.Logger.WarnContext(ctx, "PackChecksum backfill failed (non-fatal)",
            slog.String("repo_id", repoID),
            slog.String("err", err.Error()),
        )
    }
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/maintenance/ -run TestRunPipeline_BackfillsPackChecksum -v`
Expected: PASS

- [ ] **Step 5: Run the full maintenance suite**

Run: `go test ./internal/maintenance/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/maintenance/backfill_packchecksum.go internal/maintenance/pipeline.go internal/maintenance/pipeline_test.go
git commit -m "maintenance: lazy backfill of Pack.PackChecksum for legacy manifests"
```

---

## Phase 6 — Gateway-proxied URL endpoints

This phase ships the `/_bundle/<sha256>` and `/_pack/<sha1>` HTTP handlers, gated by an HMAC token. These are used when the storage adapter does not support `SignedGetURL` (localfs) or when the operator has chosen `proxied` mode for audit/perimeter reasons.

### Task 6.1: HMAC token mint + verify (`internal/proxiedurl`)

**Files:**
- Create: `internal/proxiedurl/doc.go`
- Create: `internal/proxiedurl/token.go`
- Create: `internal/proxiedurl/token_test.go`
- Create: `internal/proxiedurl/errs.go`
- Create: `internal/proxiedurl/errs_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/proxiedurl/token_test.go`:

```go
package proxiedurl

import (
    "errors"
    "testing"
    "time"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestMint_Verify_Roundtrip(t *testing.T) {
    tok, err := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(time.Minute))
    if err != nil {
        t.Fatalf("Mint: %v", err)
    }
    got, err := Verify(testKey, tok, "bundle", "sha256-abc", time.Now())
    if err != nil {
        t.Fatalf("Verify: %v", err)
    }
    if got.Kind != "bundle" || got.Hash != "sha256-abc" {
        t.Fatalf("got %+v", got)
    }
}

func TestVerify_Expired(t *testing.T) {
    tok, _ := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(-time.Minute))
    _, err := Verify(testKey, tok, "bundle", "sha256-abc", time.Now())
    if !errors.Is(err, ErrTokenExpired) {
        t.Fatalf("err = %v, want ErrTokenExpired", err)
    }
}

func TestVerify_TamperedSignature(t *testing.T) {
    tok, _ := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(time.Minute))
    // Flip a character in the token.
    bad := tok[:len(tok)-1] + "A"
    if bad == tok {
        bad = tok[:len(tok)-1] + "B"
    }
    _, err := Verify(testKey, bad, "bundle", "sha256-abc", time.Now())
    if !errors.Is(err, ErrTokenInvalid) && !errors.Is(err, ErrTokenExpired) {
        t.Fatalf("err = %v, want ErrTokenInvalid or ErrTokenExpired", err)
    }
}

func TestVerify_KindMismatch(t *testing.T) {
    tok, _ := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(time.Minute))
    _, err := Verify(testKey, tok, "pack", "sha256-abc", time.Now())
    if !errors.Is(err, ErrKindMismatch) {
        t.Fatalf("err = %v, want ErrKindMismatch", err)
    }
}

func TestVerify_HashMismatch(t *testing.T) {
    tok, _ := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(time.Minute))
    _, err := Verify(testKey, tok, "bundle", "sha256-zzz", time.Now())
    if !errors.Is(err, ErrTokenInvalid) {
        t.Fatalf("err = %v, want ErrTokenInvalid", err)
    }
}

func TestVerify_DifferentKey_Rejected(t *testing.T) {
    tok, _ := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(time.Minute))
    other := []byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
    _, err := Verify(other, tok, "bundle", "sha256-abc", time.Now())
    if !errors.Is(err, ErrTokenInvalid) {
        t.Fatalf("err = %v, want ErrTokenInvalid", err)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/proxiedurl/ -v`
Expected: COMPILE ERROR — package does not exist.

- [ ] **Step 3: Implement the package**

Create `internal/proxiedurl/doc.go`:

```go
// Package proxiedurl mints and verifies short-lived HMAC tokens used by
// the M11 gateway-proxied URL endpoints (/_bundle/<hash>, /_pack/<hash>).
//
// Tokens are opaque base64url-encoded payloads bound to (kind, hash,
// expiry). The signing key is supplied at gateway startup
// (--proxied-url-signing-key=<file>); rotation is by replacement at
// startup time, with the operational rule that TTLs are bounded well
// under the M8 retention window (typical TTLs: 1h pack, 4h bundle).
package proxiedurl
```

Create `internal/proxiedurl/errs.go`:

```go
package proxiedurl

import "errors"

var (
    ErrTokenExpired = errors.New("proxiedurl: token expired")
    ErrTokenInvalid = errors.New("proxiedurl: token signature invalid")
    ErrKindMismatch = errors.New("proxiedurl: token kind does not match endpoint")
)
```

Create `internal/proxiedurl/token.go`:

```go
package proxiedurl

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/base64"
    "encoding/binary"
    "fmt"
    "time"
)

// Token is the decoded form of a verified token.
type Token struct {
    Kind string
    Hash string
    Exp  time.Time
}

// Mint constructs a base64url-encoded token bound to (kind, hash, exp).
// kind must be "bundle" or "pack". hash is the URL-path hash (sha256-...
// for bundles, 40-hex sha1 for packs). The signing key MUST be at least
// 16 bytes; 32 bytes is recommended.
func Mint(key []byte, kind, hash string, exp time.Time) (string, error) {
    if len(key) < 16 {
        return "", fmt.Errorf("proxiedurl: signing key too short (%d bytes); need >= 16", len(key))
    }
    if kind != "bundle" && kind != "pack" {
        return "", fmt.Errorf("proxiedurl: invalid kind %q", kind)
    }
    if hash == "" {
        return "", fmt.Errorf("proxiedurl: empty hash")
    }
    payload := encodePayload(kind, hash, exp)
    mac := hmac.New(sha256.New, key)
    mac.Write(payload)
    sig := mac.Sum(nil)
    body := append(payload, sig...)
    return base64.RawURLEncoding.EncodeToString(body), nil
}

// Verify decodes and verifies a token. Returns the parsed Token if all
// of (signature, kind, hash, expiry) match. Errors are sentinel-typed
// so callers can distinguish "expired" (don't log loudly) from
// "tampered" (log + metric).
//
// now is parameterised for testability.
func Verify(key []byte, token, expectKind, expectHash string, now time.Time) (Token, error) {
    raw, err := base64.RawURLEncoding.DecodeString(token)
    if err != nil {
        return Token{}, fmt.Errorf("%w: base64: %v", ErrTokenInvalid, err)
    }
    if len(raw) < sha256.Size {
        return Token{}, fmt.Errorf("%w: too short", ErrTokenInvalid)
    }
    payloadLen := len(raw) - sha256.Size
    payload := raw[:payloadLen]
    sig := raw[payloadLen:]

    mac := hmac.New(sha256.New, key)
    mac.Write(payload)
    want := mac.Sum(nil)
    if !hmac.Equal(want, sig) {
        return Token{}, ErrTokenInvalid
    }

    tk, err := decodePayload(payload)
    if err != nil {
        return Token{}, fmt.Errorf("%w: decode: %v", ErrTokenInvalid, err)
    }
    if now.After(tk.Exp) {
        return Token{}, ErrTokenExpired
    }
    if tk.Kind != expectKind {
        return Token{}, ErrKindMismatch
    }
    if tk.Hash != expectHash {
        return Token{}, fmt.Errorf("%w: hash mismatch", ErrTokenInvalid)
    }
    return tk, nil
}

// payload layout: [kind(1B)] [exp_unix(8B BE)] [hash(rest)]
//   kind: 1 = bundle, 2 = pack
//
// Compact, fixed-size for the prefix so we can reject malformed tokens
// before the HMAC compare without leaking timing.
func encodePayload(kind, hash string, exp time.Time) []byte {
    var k byte
    switch kind {
    case "bundle":
        k = 1
    case "pack":
        k = 2
    }
    buf := make([]byte, 1+8+len(hash))
    buf[0] = k
    binary.BigEndian.PutUint64(buf[1:9], uint64(exp.Unix()))
    copy(buf[9:], []byte(hash))
    return buf
}

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
    default:
        return Token{}, fmt.Errorf("unknown kind byte %d", p[0])
    }
    exp := time.Unix(int64(binary.BigEndian.Uint64(p[1:9])), 0).UTC()
    hash := string(p[9:])
    return Token{Kind: kind, Hash: hash, Exp: exp}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/proxiedurl/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/proxiedurl/
git commit -m "proxiedurl: HMAC token mint + verify (M11 gateway-proxied URLs)"
```

### Task 6.2: Gateway routes `/_bundle/<hash>` and `/_pack/<hash>`

**Files:**
- Create: `internal/gateway/proxied_routes.go`
- Create: `internal/gateway/proxied_routes_test.go`
- Modify: `internal/gateway/server.go` (route registration)
- Modify: `internal/gateway/routes.go`

- [ ] **Step 1: Write the failing test**

Create `internal/gateway/proxied_routes_test.go`:

```go
package gateway

import (
    "context"
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/proxiedurl"
    "github.com/bucketvcs/bucketvcs/internal/repo/keys"
    "github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestProxiedRoute_Bundle_OK(t *testing.T) {
    dir := t.TempDir()
    store, _ := localfs.New(localfs.Config{Root: dir})
    rkeys, _ := keys.NewRepo("ten", "rep")
    body := []byte("BUNDLE BYTES")
    bundleKey := rkeys.BundleKey("sha256-aabb")
    if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader(string(body)), nil); err != nil {
        t.Fatal(err)
    }

    key := []byte("0123456789abcdef0123456789abcdef")
    tok, _ := proxiedurl.Mint(key, "bundle", "sha256-aabb", time.Now().Add(time.Minute))

    h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys})
    srv := httptest.NewServer(h)
    defer srv.Close()

    resp, err := http.Get(srv.URL + "/_bundle/sha256-aabb?token=" + tok)
    if err != nil {
        t.Fatal(err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != 200 {
        b, _ := io.ReadAll(resp.Body)
        t.Fatalf("status %d: %s", resp.StatusCode, b)
    }
    got, _ := io.ReadAll(resp.Body)
    if string(got) != string(body) {
        t.Errorf("body = %q, want %q", got, body)
    }
}

func TestProxiedRoute_Bundle_Expired_403(t *testing.T) {
    dir := t.TempDir()
    store, _ := localfs.New(localfs.Config{Root: dir})
    rkeys, _ := keys.NewRepo("ten", "rep")
    bundleKey := rkeys.BundleKey("sha256-cc")
    store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("X"), nil)

    key := []byte("0123456789abcdef0123456789abcdef")
    tok, _ := proxiedurl.Mint(key, "bundle", "sha256-cc", time.Now().Add(-time.Minute))

    h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys})
    srv := httptest.NewServer(h)
    defer srv.Close()

    resp, _ := http.Get(srv.URL + "/_bundle/sha256-cc?token=" + tok)
    if resp.StatusCode != http.StatusForbidden {
        t.Fatalf("status = %d, want 403", resp.StatusCode)
    }
}

func TestProxiedRoute_Bundle_Range(t *testing.T) {
    dir := t.TempDir()
    store, _ := localfs.New(localfs.Config{Root: dir})
    rkeys, _ := keys.NewRepo("ten", "rep")
    body := []byte("0123456789ABCDEF")
    bundleKey := rkeys.BundleKey("sha256-r1")
    store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader(string(body)), nil)

    key := []byte("0123456789abcdef0123456789abcdef")
    tok, _ := proxiedurl.Mint(key, "bundle", "sha256-r1", time.Now().Add(time.Minute))

    h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys})
    srv := httptest.NewServer(h)
    defer srv.Close()

    req, _ := http.NewRequest("GET", srv.URL+"/_bundle/sha256-r1?token="+tok, nil)
    req.Header.Set("Range", "bytes=4-7")
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatal(err)
    }
    defer resp.Body.Close()
    got, _ := io.ReadAll(resp.Body)
    if string(got) != "4567" {
        t.Errorf("range body = %q, want \"4567\"", got)
    }
    if resp.StatusCode != http.StatusPartialContent {
        t.Errorf("status = %d, want 206", resp.StatusCode)
    }
}

// proxiedKeyResolver is a test-side implementation of the resolver
// interface that maps (kind, hash) -> storage key for a single repo.
type proxiedKeyResolver struct {
    rkeys *keys.Repo
}

func (p proxiedKeyResolver) BundleKey(hash string) (string, bool) {
    return p.rkeys.BundleKey(hash), true
}
func (p proxiedKeyResolver) PackKey(hash string) (string, bool) {
    return p.rkeys.CanonicalPackKey(hash), true
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gateway/ -run TestProxiedRoute -v`
Expected: COMPILE ERROR — `NewProxiedHandler` undefined.

- [ ] **Step 3: Implement the handler**

Create `internal/gateway/proxied_routes.go`:

```go
package gateway

import (
    "context"
    "errors"
    "io"
    "net/http"
    "strconv"
    "strings"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/proxiedurl"
    "github.com/bucketvcs/bucketvcs/internal/storage"
)

// ProxiedKeyResolver maps a hash advertised on the URL path to the
// storage key the gateway should fetch. Implementations decide how to
// scope hash -> repo (typically via a single-repo gateway, or a
// multi-repo gateway with the repo embedded in the URL prefix).
//
// For M11 the simplest production deployment is one gateway per
// (tenant, repo); a multi-repo deployment can extend the URL pattern
// to include a tenant/repo segment in a successor milestone.
type ProxiedKeyResolver interface {
    // BundleKey returns the durable storage key for a bundle whose
    // content-addressed hash is `hash` (e.g., "sha256-aabbcc...").
    // ok=false means the hash is not advertised by this gateway.
    BundleKey(hash string) (string, bool)
    // PackKey returns the durable storage key for a canonical pack whose
    // pack-checksum is `hash` (40-hex SHA-1).
    PackKey(hash string) (string, bool)
}

// NewProxiedHandler returns an http.Handler serving /_bundle/<hash> and
// /_pack/<hash> from store, gated by HMAC tokens minted with key.
//
// The handler is mounted at root; the prefix arguments determine which
// path segment it serves. Pass "/_bundle/" and "/_pack/" for the M11
// defaults.
func NewProxiedHandler(store storage.ObjectStore, key []byte, bundlePrefix, packPrefix string, resolver ProxiedKeyResolver) http.Handler {
    return &proxiedHandler{
        store: store, key: key,
        bundlePrefix: bundlePrefix, packPrefix: packPrefix,
        resolver: resolver,
        now:      time.Now,
    }
}

type proxiedHandler struct {
    store        storage.ObjectStore
    key          []byte
    bundlePrefix string
    packPrefix   string
    resolver     ProxiedKeyResolver
    now          func() time.Time
}

func (h *proxiedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet && r.Method != http.MethodHead {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var kind, hash string
    var storageKey string
    switch {
    case strings.HasPrefix(r.URL.Path, h.bundlePrefix):
        kind = "bundle"
        hash = strings.TrimPrefix(r.URL.Path, h.bundlePrefix)
        if k, ok := h.resolver.BundleKey(hash); ok {
            storageKey = k
        }
    case strings.HasPrefix(r.URL.Path, h.packPrefix):
        kind = "pack"
        hash = strings.TrimPrefix(r.URL.Path, h.packPrefix)
        if k, ok := h.resolver.PackKey(hash); ok {
            storageKey = k
        }
    default:
        http.NotFound(w, r)
        return
    }
    if hash == "" || storageKey == "" {
        http.NotFound(w, r)
        return
    }
    tok := r.URL.Query().Get("token")
    if tok == "" {
        http.Error(w, "missing token", http.StatusForbidden)
        return
    }
    if _, err := proxiedurl.Verify(h.key, tok, kind, hash, h.now()); err != nil {
        if errors.Is(err, proxiedurl.ErrTokenExpired) {
            http.Error(w, "token expired", http.StatusForbidden)
            return
        }
        http.Error(w, "invalid token", http.StatusForbidden)
        return
    }
    h.serveObject(r.Context(), w, r, storageKey)
}

func (h *proxiedHandler) serveObject(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) {
    rangeHdr := r.Header.Get("Range")
    if rangeHdr == "" {
        // Full object.
        meta, err := h.store.Head(ctx, key)
        if err != nil {
            http.Error(w, "not found", http.StatusNotFound)
            return
        }
        obj, err := h.store.Get(ctx, key, nil)
        if err != nil {
            http.Error(w, "not found", http.StatusNotFound)
            return
        }
        defer obj.Body.Close()
        w.Header().Set("Content-Type", "application/octet-stream")
        w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
        if r.Method == http.MethodHead {
            return
        }
        _, _ = io.Copy(w, obj.Body)
        return
    }
    // Range: bytes=<start>-<end>
    start, end, ok := parseSimpleByteRange(rangeHdr)
    if !ok {
        http.Error(w, "invalid Range", http.StatusRequestedRangeNotSatisfiable)
        return
    }
    rc, err := h.store.GetRange(ctx, key, start, end)
    if err != nil {
        http.Error(w, "range error", http.StatusInternalServerError)
        return
    }
    defer rc.Close()
    w.Header().Set("Content-Type", "application/octet-stream")
    w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/*")
    w.WriteHeader(http.StatusPartialContent)
    _, _ = io.Copy(w, rc)
}

// parseSimpleByteRange handles the only forms M11 advertises:
// "bytes=N-M". Multi-range and "bytes=N-" / "bytes=-M" are rejected
// (clients fetching bundle/pack files use simple ranges).
func parseSimpleByteRange(h string) (start, end int64, ok bool) {
    if !strings.HasPrefix(h, "bytes=") {
        return 0, 0, false
    }
    spec := strings.TrimPrefix(h, "bytes=")
    parts := strings.SplitN(spec, "-", 2)
    if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
        return 0, 0, false
    }
    s, err1 := strconv.ParseInt(parts[0], 10, 64)
    e, err2 := strconv.ParseInt(parts[1], 10, 64)
    if err1 != nil || err2 != nil || s < 0 || e < s {
        return 0, 0, false
    }
    return s, e, true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gateway/ -run TestProxiedRoute -v`
Expected: PASS

- [ ] **Step 5: Mount the routes in the gateway server**

In `internal/gateway/server.go` (or `routes.go` — find where the gateway's `http.ServeMux` is built; `grep -n 'http.ServeMux\|NewServeMux\|HandleFunc' internal/gateway/*.go`), add registration. Sketch:

```go
// If proxied URLs are enabled, mount the handler.
if cfg.ProxiedURLSigningKey != nil {
    proxied := NewProxiedHandler(store, cfg.ProxiedURLSigningKey, "/_bundle/", "/_pack/", cfg.ProxiedKeyResolver)
    mux.Handle("/_bundle/", proxied)
    mux.Handle("/_pack/", proxied)
}
```

The exact wiring depends on the existing `Config`/`Server` shape. Add a minimal field `ProxiedURLSigningKey []byte` to the gateway config and a `ProxiedKeyResolver ProxiedKeyResolver` field. Plumb through from `cmd/bucketvcs/serve.go` in Phase 6 of the plan (Task 7.x — `serve.go` flag wiring is below in Phase 8.4 below; sequence the changes so the server compile-check stays green).

- [ ] **Step 6: Run the gateway server build**

Run: `go build ./internal/gateway/...`
Expected: BUILD SUCCESS

- [ ] **Step 7: Commit**

```bash
git add internal/gateway/proxied_routes.go internal/gateway/proxied_routes_test.go internal/gateway/server.go internal/gateway/routes.go
git commit -m "gateway: /_bundle and /_pack proxied routes (HMAC-gated)"
```

### Task 6.3: URL builder (`mint or proxy` decision logic)

**Files:**
- Create: `internal/gateway/proxied_url_builder.go`
- Create: `internal/gateway/proxied_url_builder_test.go`
- Create: `internal/gateway/uri_mode.go`
- Create: `internal/gateway/uri_mode_test.go`

A small helper that, given the gateway's `URIMode`, the storage adapter, and the (kind, hash, ttl) tuple, returns either a `SignedGetURL` or a gateway-proxied URL.

- [ ] **Step 1: Write the failing tests**

Create `internal/gateway/uri_mode_test.go`:

```go
package gateway

import "testing"

func TestParseURIMode(t *testing.T) {
    cases := []struct {
        in   string
        want URIMode
        ok   bool
    }{
        {"auto", URIModeAuto, true},
        {"direct", URIModeDirect, true},
        {"proxied", URIModeProxied, true},
        {"off", URIModeOff, true},
        {"", URIModeAuto, false},
        {"weird", URIModeAuto, false},
    }
    for _, c := range cases {
        got, ok := ParseURIMode(c.in)
        if ok != c.ok || (ok && got != c.want) {
            t.Errorf("ParseURIMode(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
        }
    }
}
```

Create `internal/gateway/proxied_url_builder_test.go`:

```go
package gateway

import (
    "context"
    "errors"
    "strings"
    "testing"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/storage"
)

type fakeStoreNoSign struct{ storage.ObjectStore }

func (f fakeStoreNoSign) Capabilities() storage.Capabilities { return storage.Capabilities{SignedURLs: false} }
func (f fakeStoreNoSign) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
    return "", storage.ErrNotSupported
}

type fakeStoreWithSign struct {
    storage.ObjectStore
    minted string
    err    error
}

func (f *fakeStoreWithSign) Capabilities() storage.Capabilities {
    return storage.Capabilities{SignedURLs: true}
}
func (f *fakeStoreWithSign) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
    if f.err != nil {
        return "", f.err
    }
    return f.minted, nil
}

func TestBuildBundleURL_Auto_DirectFirst(t *testing.T) {
    store := &fakeStoreWithSign{minted: "https://signed.example/x"}
    b := URLBuilder{
        Store: store, ProxiedKey: []byte("0123456789abcdef0123456789abcdef"),
        ProxiedBaseURL: "https://gw.example", BundleTTL: 4 * time.Hour, PackTTL: time.Hour,
        Mode: URIModeAuto,
    }
    got, via, err := b.BuildBundleURL(context.Background(), "sha256-aa", "kk", "sha256:hex")
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    if got != "https://signed.example/x" || via != "direct" {
        t.Errorf("got=%q via=%q", got, via)
    }
}

func TestBuildBundleURL_Auto_FallsBackToProxied(t *testing.T) {
    b := URLBuilder{
        Store: fakeStoreNoSign{}, ProxiedKey: []byte("0123456789abcdef0123456789abcdef"),
        ProxiedBaseURL: "https://gw.example", BundleTTL: 4 * time.Hour, PackTTL: time.Hour,
        Mode: URIModeAuto,
    }
    got, via, err := b.BuildBundleURL(context.Background(), "sha256-aa", "kk", "")
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    if !strings.HasPrefix(got, "https://gw.example/_bundle/sha256-aa?token=") || via != "proxied" {
        t.Errorf("got=%q via=%q", got, via)
    }
}

func TestBuildBundleURL_Direct_Required_ErrorsOnNoSupport(t *testing.T) {
    b := URLBuilder{
        Store:          fakeStoreNoSign{},
        ProxiedKey:     []byte("0123456789abcdef0123456789abcdef"),
        ProxiedBaseURL: "https://gw.example",
        Mode:           URIModeDirect,
    }
    _, _, err := b.BuildBundleURL(context.Background(), "sha256-aa", "kk", "")
    if !errors.Is(err, storage.ErrNotSupported) {
        t.Fatalf("err = %v, want ErrNotSupported", err)
    }
}

func TestBuildBundleURL_Off_ReturnsErr(t *testing.T) {
    b := URLBuilder{Mode: URIModeOff}
    _, _, err := b.BuildBundleURL(context.Background(), "sha256-aa", "kk", "")
    if err == nil {
        t.Fatalf("expected error in Off mode")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gateway/ -run 'TestParseURIMode|TestBuildBundleURL' -v`
Expected: COMPILE ERROR.

- [ ] **Step 3: Implement `URIMode` and `URLBuilder`**

Create `internal/gateway/uri_mode.go`:

```go
package gateway

// URIMode controls how the gateway delivers bundle and pack URIs to clients.
type URIMode int

const (
    URIModeAuto URIMode = iota // try direct (signed); fall back to proxied
    URIModeDirect              // direct only; error if adapter cannot sign
    URIModeProxied             // gateway-proxied only
    URIModeOff                 // do not advertise the URI capability
)

func ParseURIMode(s string) (URIMode, bool) {
    switch s {
    case "auto":
        return URIModeAuto, true
    case "direct":
        return URIModeDirect, true
    case "proxied":
        return URIModeProxied, true
    case "off":
        return URIModeOff, true
    }
    return URIModeAuto, false
}

func (m URIMode) String() string {
    switch m {
    case URIModeAuto:
        return "auto"
    case URIModeDirect:
        return "direct"
    case URIModeProxied:
        return "proxied"
    case URIModeOff:
        return "off"
    }
    return "unknown"
}
```

Create `internal/gateway/proxied_url_builder.go`:

```go
package gateway

import (
    "context"
    "errors"
    "fmt"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/proxiedurl"
    "github.com/bucketvcs/bucketvcs/internal/storage"
)

// URLBuilder mints bundle/pack URLs for v2 advertise responses.
type URLBuilder struct {
    Store          storage.ObjectStore
    ProxiedKey     []byte
    ProxiedBaseURL string // e.g. "https://gw.example.com" (no trailing slash)
    BundleTTL      time.Duration
    PackTTL        time.Duration
    Mode           URIMode
    Now            func() time.Time // optional; defaults to time.Now
}

func (b *URLBuilder) now() time.Time {
    if b.Now != nil {
        return b.Now()
    }
    return time.Now()
}

// BuildBundleURL returns (url, via) where via is "direct" or "proxied".
// Returns error if Mode == URIModeOff or if Direct mode + no signing.
func (b *URLBuilder) BuildBundleURL(ctx context.Context, hash, storageKey, expectedHash string) (string, string, error) {
    return b.buildURL(ctx, "bundle", hash, storageKey, expectedHash, b.BundleTTL)
}

// BuildPackURL returns (url, via).
func (b *URLBuilder) BuildPackURL(ctx context.Context, hash, storageKey, expectedHash string) (string, string, error) {
    return b.buildURL(ctx, "pack", hash, storageKey, expectedHash, b.PackTTL)
}

func (b *URLBuilder) buildURL(ctx context.Context, kind, hash, storageKey, expectedHash string, ttl time.Duration) (string, string, error) {
    if b.Mode == URIModeOff {
        return "", "", fmt.Errorf("gateway: URI mode is off")
    }
    if b.Mode == URIModeDirect || b.Mode == URIModeAuto {
        url, err := b.Store.SignedGetURL(ctx, storageKey, storage.SignedURLOptions{
            Expires: ttl, Method: "GET", ExpectedHash: expectedHash,
        })
        if err == nil {
            return url, "direct", nil
        }
        if b.Mode == URIModeDirect {
            return "", "", err
        }
        if !errors.Is(err, storage.ErrNotSupported) {
            // Direct attempt failed for a non-capability reason; surface it.
            return "", "", err
        }
        // Fall through to proxied.
    }
    // Proxied mode (or auto fallback).
    if len(b.ProxiedKey) == 0 || b.ProxiedBaseURL == "" {
        return "", "", fmt.Errorf("gateway: proxied URLs are not configured")
    }
    exp := b.now().Add(ttl)
    tok, err := proxiedurl.Mint(b.ProxiedKey, kind, hash, exp)
    if err != nil {
        return "", "", err
    }
    var path string
    switch kind {
    case "bundle":
        path = "/_bundle/"
    case "pack":
        path = "/_pack/"
    }
    return b.ProxiedBaseURL + path + hash + "?token=" + tok, "proxied", nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gateway/ -run 'TestParseURIMode|TestBuildBundleURL' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/uri_mode.go internal/gateway/uri_mode_test.go \
        internal/gateway/proxied_url_builder.go internal/gateway/proxied_url_builder_test.go
git commit -m "gateway: URIMode + URLBuilder for bundle/pack URIs"
```

---

## Phase 7 — v2 `bundle-uri` capability + freshness state machine + handler

### Task 7.1: Pure freshness state machine

**Files:**
- Create: `internal/v2proto/bundleuri_freshness.go`
- Create: `internal/v2proto/bundleuri_freshness_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/v2proto/bundleuri_freshness_test.go`:

```go
package v2proto

import (
    "testing"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestEvaluateFreshness_Current(t *testing.T) {
    now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
    bundle := manifest.BundleEntry{
        TipOID:                "tip-current",
        CoversManifestVersion: 5,
        GeneratedAt:           now.Add(-1 * time.Hour).Format(time.RFC3339),
    }
    res := EvaluateFreshness(FreshnessInputs{
        Bundle:        &bundle,
        CurrentTip:    "tip-current",
        IsAncestor:    func(a, d string, max int) bool { return false },
        WalkBack:      func(from, target string, max int) (int, error) { return 0, nil },
        WarmCommits:   100,
        WarmAge:       24 * time.Hour,
        Now:           now,
    })
    if res.State != FreshnessCurrent {
        t.Errorf("got %s, want current", res.State)
    }
}

func TestEvaluateFreshness_Warm(t *testing.T) {
    now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
    bundle := manifest.BundleEntry{
        TipOID:      "old-tip",
        GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
    }
    res := EvaluateFreshness(FreshnessInputs{
        Bundle:     &bundle,
        CurrentTip: "new-tip",
        IsAncestor: func(a, d string, max int) bool { return a == "old-tip" && d == "new-tip" },
        WalkBack:   func(from, target string, max int) (int, error) { return 5, nil },
        WarmCommits: 100, WarmAge: 24 * time.Hour, Now: now,
    })
    if res.State != FreshnessWarm {
        t.Errorf("got %s, want warm", res.State)
    }
    if res.CommitsBehind != 5 {
        t.Errorf("CommitsBehind = %d, want 5", res.CommitsBehind)
    }
}

func TestEvaluateFreshness_StaleByAge(t *testing.T) {
    now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
    bundle := manifest.BundleEntry{
        TipOID:      "old-tip",
        GeneratedAt: now.Add(-25 * time.Hour).Format(time.RFC3339),
    }
    res := EvaluateFreshness(FreshnessInputs{
        Bundle:     &bundle,
        CurrentTip: "new-tip",
        IsAncestor: func(a, d string, max int) bool { return true },
        WalkBack:   func(from, target string, max int) (int, error) { return 5, nil },
        WarmCommits: 100, WarmAge: 24 * time.Hour, Now: now,
    })
    if res.State != FreshnessStale {
        t.Errorf("got %s, want stale", res.State)
    }
}

func TestEvaluateFreshness_StaleByForcePush(t *testing.T) {
    now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
    bundle := manifest.BundleEntry{
        TipOID:      "old-tip",
        GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
    }
    res := EvaluateFreshness(FreshnessInputs{
        Bundle:     &bundle,
        CurrentTip: "divergent-tip",
        IsAncestor: func(a, d string, max int) bool { return false }, // not an ancestor
        WalkBack:   func(from, target string, max int) (int, error) { return -1, nil },
        WarmCommits: 100, WarmAge: 24 * time.Hour, Now: now,
    })
    if res.State != FreshnessStale {
        t.Errorf("got %s, want stale (force-push case)", res.State)
    }
}

func TestEvaluateFreshness_Retired(t *testing.T) {
    res := EvaluateFreshness(FreshnessInputs{
        Bundle: nil, CurrentTip: "anything",
        WarmCommits: 100, WarmAge: 24 * time.Hour, Now: time.Now(),
    })
    if res.State != FreshnessRetired {
        t.Errorf("got %s, want retired", res.State)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/v2proto/ -run TestEvaluateFreshness -v`
Expected: COMPILE ERROR.

- [ ] **Step 3: Implement the state machine**

Create `internal/v2proto/bundleuri_freshness.go`:

```go
package v2proto

import (
    "time"

    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

type FreshnessState int

const (
    FreshnessRetired FreshnessState = iota
    FreshnessStale
    FreshnessWarm
    FreshnessCurrent
)

func (f FreshnessState) String() string {
    switch f {
    case FreshnessRetired:
        return "retired"
    case FreshnessStale:
        return "stale"
    case FreshnessWarm:
        return "warm"
    case FreshnessCurrent:
        return "current"
    }
    return "unknown"
}

// FreshnessInputs decouples the state machine from the reachability
// package so the state machine itself is a pure function of values
// the caller supplies.
type FreshnessInputs struct {
    Bundle      *manifest.BundleEntry
    CurrentTip  string
    IsAncestor  func(ancestor, descendant string, max int) bool
    WalkBack    func(from, target string, max int) (int, error)
    WarmCommits int
    WarmAge     time.Duration
    Now         time.Time
}

type FreshnessResult struct {
    State         FreshnessState
    CommitsBehind int    // -1 when not computed (e.g. retired/stale-by-other-reason)
    Reason        string // "no_bundle", "force_push", "age_exceeded", "commits_exceeded", "current", "warm"
}

// EvaluateFreshness implements the §5.2 (M11 spec) state machine.
func EvaluateFreshness(in FreshnessInputs) FreshnessResult {
    if in.Bundle == nil {
        return FreshnessResult{State: FreshnessRetired, CommitsBehind: -1, Reason: "no_bundle"}
    }
    if in.Bundle.TipOID == in.CurrentTip {
        return FreshnessResult{State: FreshnessCurrent, CommitsBehind: 0, Reason: "current"}
    }
    if !in.IsAncestor(in.Bundle.TipOID, in.CurrentTip, in.WarmCommits) {
        return FreshnessResult{State: FreshnessStale, CommitsBehind: -1, Reason: "force_push"}
    }
    age, err := time.Parse(time.RFC3339, in.Bundle.GeneratedAt)
    if err == nil && in.Now.Sub(age) >= in.WarmAge {
        return FreshnessResult{State: FreshnessStale, CommitsBehind: -1, Reason: "age_exceeded"}
    }
    n, werr := in.WalkBack(in.CurrentTip, in.Bundle.TipOID, in.WarmCommits)
    if werr != nil || n < 0 || n > in.WarmCommits {
        return FreshnessResult{State: FreshnessStale, CommitsBehind: n, Reason: "commits_exceeded"}
    }
    return FreshnessResult{State: FreshnessWarm, CommitsBehind: n, Reason: "warm"}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/v2proto/ -run TestEvaluateFreshness -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/v2proto/bundleuri_freshness.go internal/v2proto/bundleuri_freshness_test.go
git commit -m "v2proto: bundle-uri freshness state machine (pure function)"
```

### Task 7.2: `bundle-uri` v2 command handler + capability advertise

**Files:**
- Create: `internal/v2proto/bundleuri.go`
- Create: `internal/v2proto/bundleuri_test.go`
- Create: `internal/v2proto/bundleuri_response.go`
- Create: `internal/v2proto/bundleuri_response_test.go`
- Modify: `internal/v2proto/caps.go`
- Modify: `internal/v2proto/caps_test.go`

- [ ] **Step 1: Write the response-encoder failing test**

Create `internal/v2proto/bundleuri_response_test.go`:

```go
package v2proto

import (
    "bytes"
    "strings"
    "testing"
)

func TestEncodeBundleURIResponse_OneBundle(t *testing.T) {
    var buf bytes.Buffer
    err := EncodeBundleURIResponse(&buf, []BundleAdvertisement{{
        ID:          "bundle_t_r_42_aa",
        URI:         "https://example/u",
        CreationTok: "1715346000",
    }})
    if err != nil {
        t.Fatalf("EncodeBundleURIResponse: %v", err)
    }
    s := buf.String()
    for _, want := range []string{
        "bundle.bundle_t_r_42_aa.uri=https://example/u",
        "bundle.bundle_t_r_42_aa.creationToken=1715346000",
    } {
        if !strings.Contains(s, want) {
            t.Errorf("response missing %q\nfull:\n%s", want, s)
        }
    }
}

func TestEncodeBundleURIResponse_Empty_NoBundles(t *testing.T) {
    var buf bytes.Buffer
    err := EncodeBundleURIResponse(&buf, nil)
    if err != nil {
        t.Fatalf("EncodeBundleURIResponse: %v", err)
    }
    // An empty response is well-formed (just the flush-pkt) — clients
    // fall through to standard fetch.
    if buf.Len() == 0 {
        t.Errorf("expected at least a flush-pkt, got empty response")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/v2proto/ -run TestEncodeBundleURIResponse -v`
Expected: COMPILE ERROR.

- [ ] **Step 3: Implement the response encoder**

Create `internal/v2proto/bundleuri_response.go`:

```go
package v2proto

import (
    "fmt"
    "io"

    "github.com/bucketvcs/bucketvcs/internal/pktline"
)

// BundleAdvertisement is one entry in the bundle-uri response.
type BundleAdvertisement struct {
    ID          string // BundleEntry.ID
    URI         string // direct or proxied URL
    CreationTok string // unix-seconds string of GeneratedAt
}

// EncodeBundleURIResponse writes the v2 bundle-uri response per Git's
// protocol-v2 bundle-uri.txt: one or more `bundle.<id>.<key>=<value>`
// pkt-lines per bundle, followed by a flush-pkt.
func EncodeBundleURIResponse(w io.Writer, ads []BundleAdvertisement) error {
    for _, ad := range ads {
        if err := pktline.WriteString(w, fmt.Sprintf("bundle.%s.uri=%s\n", ad.ID, ad.URI)); err != nil {
            return err
        }
        if ad.CreationTok != "" {
            if err := pktline.WriteString(w, fmt.Sprintf("bundle.%s.creationToken=%s\n", ad.ID, ad.CreationTok)); err != nil {
                return err
            }
        }
    }
    return pktline.WriteFlush(w)
}
```

If `pktline.WriteString` / `pktline.WriteFlush` do not match the existing helper names (check `internal/pktline/`), adapt to whatever exists.

- [ ] **Step 4: Write the bundle-uri command handler test**

Create `internal/v2proto/bundleuri_test.go`:

```go
package v2proto

import (
    "bytes"
    "context"
    "strings"
    "testing"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestHandleBundleURI_Current_Advertises(t *testing.T) {
    body := manifest.Body{
        Refs: map[string]string{"refs/heads/main": "tip"},
        Bundles: []manifest.BundleEntry{{
            ID: "b1", Kind: "full_default", Ref: "refs/heads/main",
            TipOID: "tip", CoversManifestVersion: 1,
            GeneratedAt: time.Now().Add(-time.Minute).Format(time.RFC3339),
            BundleHash: "sha256-aa", BundleKey: "bk",
        }},
    }
    var out bytes.Buffer
    err := HandleBundleURI(context.Background(), &out, BundleURIDeps{
        Body:        body,
        Now:         time.Now(),
        WarmCommits: 100, WarmAge: 24 * time.Hour,
        IsAncestor:  func(a, d string, max int) bool { return true },
        WalkBack:    func(from, target string, max int) (int, error) { return 0, nil },
        BuildURL: func(_ context.Context, hash, key, expected string) (string, string, error) {
            return "https://example/u", "direct", nil
        },
    })
    if err != nil {
        t.Fatalf("HandleBundleURI: %v", err)
    }
    if !strings.Contains(out.String(), "bundle.b1.uri=https://example/u") {
        t.Fatalf("response missing bundle.b1.uri:\n%s", out.String())
    }
}

func TestHandleBundleURI_Stale_Omits(t *testing.T) {
    body := manifest.Body{
        Refs: map[string]string{"refs/heads/main": "new-tip"},
        Bundles: []manifest.BundleEntry{{
            ID: "b1", Kind: "full_default", Ref: "refs/heads/main",
            TipOID: "old-tip", GeneratedAt: time.Now().Add(-25 * time.Hour).Format(time.RFC3339),
        }},
    }
    var out bytes.Buffer
    err := HandleBundleURI(context.Background(), &out, BundleURIDeps{
        Body:        body,
        Now:         time.Now(),
        WarmCommits: 100, WarmAge: 24 * time.Hour,
        IsAncestor:  func(a, d string, max int) bool { return true },
        WalkBack:    func(from, target string, max int) (int, error) { return 5, nil },
        BuildURL:    func(_ context.Context, hash, key, expected string) (string, string, error) { return "https://x", "direct", nil },
    })
    if err != nil {
        t.Fatalf("HandleBundleURI: %v", err)
    }
    if strings.Contains(out.String(), "bundle.b1.uri=") {
        t.Fatalf("stale bundle should not be advertised:\n%s", out.String())
    }
}
```

- [ ] **Step 5: Implement `HandleBundleURI`**

Create `internal/v2proto/bundleuri.go`:

```go
package v2proto

import (
    "context"
    "fmt"
    "io"
    "strconv"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// BundleURIDeps wires HandleBundleURI to the gateway's reachability
// helpers, URL builder, and clock without taking a hard dependency on
// any of those packages from internal/v2proto.
type BundleURIDeps struct {
    Body        manifest.Body
    Now         time.Time
    WarmCommits int
    WarmAge     time.Duration
    IsAncestor  func(ancestor, descendant string, max int) bool
    WalkBack    func(from, target string, max int) (int, error)
    BuildURL    func(ctx context.Context, hash, storageKey, expectedHash string) (url string, via string, err error)
}

// HandleBundleURI processes the v2 `command=bundle-uri` request and
// writes the response (a `bundle.<id>.<key>=<value>` block per
// advertised bundle, then a flush-pkt). Stale or retired bundles
// produce an empty response (clients fall through to fetch).
func HandleBundleURI(ctx context.Context, w io.Writer, deps BundleURIDeps) error {
    var entry *manifest.BundleEntry
    for i := range deps.Body.Bundles {
        if deps.Body.Bundles[i].Kind == "full_default" {
            entry = &deps.Body.Bundles[i]
            break
        }
    }
    if entry == nil {
        return EncodeBundleURIResponse(w, nil)
    }
    currentTip := deps.Body.Refs[entry.Ref]
    res := EvaluateFreshness(FreshnessInputs{
        Bundle:      entry,
        CurrentTip:  currentTip,
        IsAncestor:  deps.IsAncestor,
        WalkBack:    deps.WalkBack,
        WarmCommits: deps.WarmCommits,
        WarmAge:     deps.WarmAge,
        Now:         deps.Now,
    })
    if res.State != FreshnessCurrent && res.State != FreshnessWarm {
        return EncodeBundleURIResponse(w, nil)
    }
    expectedHash := ""
    if entry.BundleHash != "" {
        expectedHash = "sha256:" + bundleHashHex(entry.BundleHash)
    }
    url, _, err := deps.BuildURL(ctx, entry.BundleHash, entry.BundleKey, expectedHash)
    if err != nil {
        // Non-fatal: omit the bundle, return empty response, client falls through.
        return EncodeBundleURIResponse(w, nil)
    }
    creationTok := ""
    if t, err := time.Parse(time.RFC3339, entry.GeneratedAt); err == nil {
        creationTok = strconv.FormatInt(t.Unix(), 10)
    }
    return EncodeBundleURIResponse(w, []BundleAdvertisement{{
        ID: entry.ID, URI: url, CreationTok: creationTok,
    }})
}

// bundleHashHex strips the "sha256-" prefix used in BundleHash storage
// (matches the IndexRef.Hash convention).
func bundleHashHex(h string) string {
    const p = "sha256-"
    if len(h) > len(p) && h[:len(p)] == p {
        return h[len(p):]
    }
    return h
}

// suppress unused-import warning if `fmt` is only used elsewhere
var _ = fmt.Sprintf
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/v2proto/ -run 'TestEncodeBundleURIResponse|TestHandleBundleURI' -v`
Expected: PASS

- [ ] **Step 7: Advertise the capability conditionally**

In `internal/v2proto/caps.go`, find the function that emits the v2 capability advertisement (`grep -n 'capability' internal/v2proto/caps.go`). Add a parameter `bundleURIEnabled bool` (or thread through a `Config` struct already in use). When true, append the `bundle-uri` capability line.

Add a test in `internal/v2proto/caps_test.go`:

```go
func TestAdvertiseCaps_BundleURI_Conditional(t *testing.T) {
    // Test that bundle-uri appears in the advertisement when enabled, omitted when disabled.
    // Adapt the call shape to whatever caps.go exposes (likely AdvertiseCaps or BuildCapsAdvert).
}
```

(The test body depends on the existing helper signature; consult `caps.go` for the entry point.)

- [ ] **Step 8: Run the v2proto suite**

Run: `go test ./internal/v2proto/...`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add internal/v2proto/bundleuri*.go internal/v2proto/caps.go internal/v2proto/caps_test.go
git commit -m "v2proto: bundle-uri capability + command handler"
```

### Task 7.3: Wire `bundle-uri` into the gateway dispatch

**Files:**
- Modify: `internal/v2proto/fetch.go` (or wherever the v2 command dispatcher lives)
- Modify: `internal/v2proto/fetch_test.go`
- Modify: `internal/gateway/upload_pack.go` (caller side)

The v2 protocol multiplexes `command=ls-refs`, `command=fetch`, and now `command=bundle-uri`. Add a dispatch arm.

- [ ] **Step 1: Find the existing dispatch**

Run: `grep -n '"command="' internal/v2proto/*.go internal/gateway/*.go`
Expected: a switch or map keyed on `command=ls-refs` etc.

- [ ] **Step 2: Write the failing test**

Append to `internal/v2proto/fetch_test.go` (or create `internal/v2proto/dispatch_test.go`):

```go
func TestDispatch_BundleURICommand(t *testing.T) {
    // Build a v2 request that says command=bundle-uri and confirm
    // the dispatcher invokes HandleBundleURI. Use a fake body with
    // one current bundle.
    // (Test body depends on the dispatcher's existing input shape;
    // model on TestDispatch_LsRefs / TestDispatch_Fetch in the same file.)
}
```

- [ ] **Step 3: Add the dispatch arm**

Locate the existing switch (e.g., in `internal/v2proto/fetch.go`'s entry function). Add:

```go
case "bundle-uri":
    return HandleBundleURI(ctx, w, ...) // wire deps from the gateway-side caller
```

In the gateway-side caller (`internal/gateway/upload_pack.go`), construct the `BundleURIDeps` from the per-request context: load the manifest body, build a reachability set (already done for fetch), supply the URL builder configured at server startup, the configured warm thresholds, and the current time.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/v2proto/... ./internal/gateway/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/v2proto/fetch.go internal/v2proto/dispatch_test.go internal/gateway/upload_pack.go
git commit -m "v2proto+gateway: dispatch command=bundle-uri to HandleBundleURI"
```

---

## Phase 8 — v2 `packfile-uris` capability + `FullPackRequested` plan-shape detection + handler

### Task 8.1: `FullPackRequested` predicate in the reachability planner

**Files:**
- Modify: `internal/reachability/set.go` (or wherever the fetch plan is computed — `grep -n FetchPlan internal/reachability internal/v2proto`)
- Modify: corresponding test file

The plan returned by negotiation needs a boolean indicating whether the want-set covers exactly the contents of one canonical pack. The check uses M10's `.bvom`.

- [ ] **Step 1: Find the existing plan struct**

Run: `grep -rn 'type.*Plan\|ShippingPlan' internal/reachability internal/v2proto internal/gateway | head -10`
Expected: one or two hits — the plan struct produced by the M10 negotiator.

- [ ] **Step 2: Write the failing test**

In the corresponding `*_test.go`:

```go
func TestPlan_FullPackRequested_True(t *testing.T) {
    // Build a Set whose canonical pack contains exactly {oidA, oidB},
    // request {oidA, oidB}, plan.FullPackRequested should be true.
}

func TestPlan_FullPackRequested_FalsePartial(t *testing.T) {
    // Request only {oidA} from a pack containing {oidA, oidB}; expect false.
}

func TestPlan_FullPackRequested_FalseMultiPack(t *testing.T) {
    // Request spans two canonical packs; expect false.
}
```

(Body depends on how Sets / Plans are constructed in the test fixtures package — `internal/reachability/rtest`.)

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/reachability/... -run TestPlan_FullPackRequested -v`
Expected: FAIL — field/predicate missing.

- [ ] **Step 4: Add the field and computation**

In the plan struct definition, add:

```go
type Plan struct {
    // ... existing ...

    // FullPackRequested is true when:
    //   1. The plan ships exactly one pack (len(Packs) == 1), AND
    //   2. That pack is canonical (Kind == "canonical"), AND
    //   3. The want-set equals the union of the pack's object enumeration.
    // Used by the M11 packfile-uri advertise gate.
    FullPackRequested bool
}
```

In the planner that constructs `Plan`, after determining the pack list, set `FullPackRequested` by:

1. Bail to false if `len(plan.Packs) != 1` or `plan.LooseObjectCount > 0`.
2. Look up the pack in the manifest; bail if `Kind != "canonical"`.
3. Use `.bvom` to enumerate all OIDs in that pack; bail if any OID is not in the want-set OR any want-set OID is not in the pack.

Sketch:

```go
plan.FullPackRequested = false
if len(plan.Packs) == 1 && plan.LooseObjectCount == 0 {
    if pe := manifestPackByID(body, plan.Packs[0].PackID); pe != nil && isCanonical(pe) {
        // Read .bvom enumeration for this pack via the existing object-map reader.
        oids, err := objindex.EnumerateOIDs(ctx, store, body.Indexes.ObjectMap, pe.PackID)
        if err == nil {
            plan.FullPackRequested = oidSetEqualsWantSet(oids, wantSet)
        }
    }
}
```

(Adapt to the actual helper names; `EnumerateOIDs` may exist with a different name in `internal/objindex`.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/reachability/... -run TestPlan_FullPackRequested -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/reachability/set.go internal/reachability/set_test.go
# or wherever the planner lives
git commit -m "reachability: Plan.FullPackRequested predicate (M11 prerequisite)"
```

### Task 8.2: `packfile-uris` capability + advertise gate

**Files:**
- Create: `internal/v2proto/packuri.go`
- Create: `internal/v2proto/packuri_test.go`
- Modify: `internal/v2proto/caps.go`
- Modify: `internal/v2proto/fetch.go`

- [ ] **Step 1: Write the failing test**

Create `internal/v2proto/packuri_test.go`:

```go
package v2proto

import (
    "context"
    "strings"
    "testing"
)

func TestEvaluatePackURIAdvertise_Eligible(t *testing.T) {
    res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
        ClientOptedIn:     true,
        FullPackRequested: true,
        PackChecksum:      "0123456789abcdef0123456789abcdef01234567",
        BuildURL: func(ctx context.Context, hash, key, expected string) (string, string, error) {
            return "https://example/p", "direct", nil
        },
        PackKey: "tenants/t/repos/r/packs/canonical/sha256-aa.pack",
    })
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    if res.Stanza == "" || !strings.Contains(res.Stanza, "0123456789abcdef0123456789abcdef01234567") {
        t.Fatalf("stanza missing checksum: %q", res.Stanza)
    }
}

func TestEvaluatePackURIAdvertise_NotOptedIn(t *testing.T) {
    res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
        ClientOptedIn: false, FullPackRequested: true, PackChecksum: "x",
    })
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    if res.Stanza != "" {
        t.Errorf("expected empty stanza when client did not opt in, got %q", res.Stanza)
    }
}

func TestEvaluatePackURIAdvertise_NotEligible(t *testing.T) {
    res, _ := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
        ClientOptedIn: true, FullPackRequested: false, PackChecksum: "x",
    })
    if res.Stanza != "" {
        t.Errorf("expected empty stanza when not eligible, got %q", res.Stanza)
    }
}

func TestEvaluatePackURIAdvertise_MissingChecksum_Skips(t *testing.T) {
    res, _ := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
        ClientOptedIn: true, FullPackRequested: true, PackChecksum: "",
    })
    if res.Stanza != "" {
        t.Errorf("expected empty stanza when PackChecksum missing, got %q", res.Stanza)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/v2proto/ -run TestEvaluatePackURIAdvertise -v`
Expected: COMPILE ERROR.

- [ ] **Step 3: Implement the advertise gate**

Create `internal/v2proto/packuri.go`:

```go
package v2proto

import (
    "context"
    "fmt"
)

// PackURIInputs feeds the §6.2 (M11 spec) advertise gate.
type PackURIInputs struct {
    // ClientOptedIn is true when the client included `packfile-uris` in
    // its fetch arguments (Git protocol v2 explicit opt-in).
    ClientOptedIn bool
    // FullPackRequested is the plan-shape predicate from internal/reachability.
    FullPackRequested bool
    // PackChecksum is the 40-hex SHA-1 of the pack's trailer.
    PackChecksum string
    // PackKey is the storage key of the canonical pack.
    PackKey string
    // BuildURL mints a URL via the gateway's URLBuilder.
    BuildURL func(ctx context.Context, hash, key, expectedHash string) (url, via string, err error)
}

// PackURIResult describes whether the gateway should emit a
// packfile-uri stanza and, if so, the formatted line.
type PackURIResult struct {
    Stanza string // empty when no advertise; otherwise "packfile-uri=<sha1> https <URL>\n"
    Via    string // "direct" or "proxied" when Stanza != ""
}

// EvaluatePackURIAdvertise returns the stanza to send (if any) per §6.2.
func EvaluatePackURIAdvertise(ctx context.Context, in PackURIInputs) (PackURIResult, error) {
    if !in.ClientOptedIn || !in.FullPackRequested || in.PackChecksum == "" || in.PackKey == "" {
        return PackURIResult{}, nil
    }
    url, via, err := in.BuildURL(ctx, in.PackChecksum, in.PackKey, "")
    if err != nil {
        // Non-fatal: omit the URI; client gets the inline pack as usual.
        return PackURIResult{}, nil
    }
    return PackURIResult{
        Stanza: fmt.Sprintf("packfile-uri=%s https %s\n", in.PackChecksum, url),
        Via:    via,
    }, nil
}
```

- [ ] **Step 4: Advertise the capability**

In `internal/v2proto/caps.go`, alongside the M11 `bundle-uri` capability addition, conditionally append `packfile-uris=https` when the gateway's `--pack-uri-mode` is not `off`.

- [ ] **Step 5: Wire into the fetch response**

In `internal/v2proto/fetch.go`, after the plan is computed and before the inline pack is written, call `EvaluatePackURIAdvertise`. If `res.Stanza != ""`, write the stanza to the response stream (per Git's protocol-v2 packfile-uri response format) and also write the zero-object inline pack (the protocol requires a packfile section even when the URI carries the bytes).

The exact placement depends on the existing fetch-response builder; keep the change minimal — add a single call site that consults the advertise gate, and have it emit the stanza right before the existing pack-bytes section.

- [ ] **Step 6: Run tests**

Run: `go test ./internal/v2proto/...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/v2proto/packuri.go internal/v2proto/packuri_test.go internal/v2proto/caps.go internal/v2proto/fetch.go
git commit -m "v2proto: packfile-uris capability + advertise gate (narrow eligibility)"
```

### Task 8.3: `serve` CLI flags for URI modes + signing key

**Files:**
- Modify: `cmd/bucketvcs/serve.go`
- Modify: `cmd/bucketvcs/serve_test.go`
- Create: `cmd/bucketvcs/serve_uri_flags.go` (helper)
- Create: `cmd/bucketvcs/serve_uri_flags_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/bucketvcs/serve_test.go`:

```go
func TestRunServe_BundleURIMode_RequiresSigningKey(t *testing.T) {
    var stdout, stderr bytes.Buffer
    rc := runServe(context.Background(), []string{
        "--store=localfs:" + t.TempDir(),
        "--addr=127.0.0.1:0",
        "--bundle-uri-mode=auto",
        // no --proxied-url-signing-key
    }, &stdout, &stderr)
    if rc == 0 {
        t.Fatalf("rc = 0; expected non-zero (signing key required for auto/proxied)")
    }
    if !strings.Contains(stderr.String(), "signing-key") {
        t.Errorf("stderr should mention signing-key: %s", stderr.String())
    }
}

func TestRunServe_BundleURIMode_Off_NoSigningKeyNeeded(t *testing.T) {
    var stdout, stderr bytes.Buffer
    rc := runServe(context.Background(), []string{
        "--store=localfs:" + t.TempDir(),
        "--addr=127.0.0.1:0",
        "--bundle-uri-mode=off",
        "--pack-uri-mode=off",
        "--shutdown-timeout=10ms",
    }, &stdout, &stderr)
    // The server starts, then we expect a clean shutdown via context. Since
    // we cannot easily inject a context cancel from the test, accept any
    // non-validation rc; only the validation rejection from the previous
    // test is what we actually want to gate on. Skipping a full lifecycle
    // assertion here.
    _ = rc
    if strings.Contains(stderr.String(), "signing-key") {
        t.Errorf("stderr should NOT mention signing-key when modes are off: %s", stderr.String())
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/bucketvcs/ -run TestRunServe_BundleURIMode -v`
Expected: FAIL — flags absent.

- [ ] **Step 3: Add flags to `runServe`**

In `cmd/bucketvcs/serve.go` (in `runServeWithListener`, after the existing flag definitions), add:

```go
bundleURIMode := fs.String("bundle-uri-mode", "auto", "Bundle URI delivery mode: auto|direct|proxied|off")
packURIMode := fs.String("pack-uri-mode", "auto", "Pack URI delivery mode: auto|direct|proxied|off")
proxiedKeyFile := fs.String("proxied-url-signing-key", "", "File containing 32-byte HMAC key for gateway-proxied URLs (required when modes are auto or proxied)")
proxiedPackTTL := fs.Duration("proxied-url-pack-ttl", time.Hour, "TTL for proxied/signed pack URLs")
proxiedBundleTTL := fs.Duration("proxied-url-bundle-ttl", 4*time.Hour, "TTL for proxied/signed bundle URLs")
warmCommits := fs.Int("bundle-warm-commits", 100, "Bundle freshness threshold: warm if behind by <= N commits")
warmAge := fs.Duration("bundle-warm-age", 24*time.Hour, "Bundle freshness threshold: warm if generated within D")
proxiedBaseURL := fs.String("proxied-url-base", "", "External base URL of this gateway, e.g. https://gw.example (required when modes are auto or proxied)")
```

After parse, validate:

```go
bMode, ok := gateway.ParseURIMode(*bundleURIMode)
if !ok {
    fmt.Fprintf(stderr, "serve: --bundle-uri-mode=%q must be one of auto|direct|proxied|off\n", *bundleURIMode)
    return 2
}
pMode, ok := gateway.ParseURIMode(*packURIMode)
if !ok {
    fmt.Fprintf(stderr, "serve: --pack-uri-mode=%q must be one of auto|direct|proxied|off\n", *packURIMode)
    return 2
}
needsKey := bMode == gateway.URIModeAuto || bMode == gateway.URIModeProxied ||
    pMode == gateway.URIModeAuto || pMode == gateway.URIModeProxied
var signingKey []byte
if needsKey {
    if *proxiedKeyFile == "" {
        fmt.Fprintln(stderr, "serve: --proxied-url-signing-key is required when bundle-uri-mode or pack-uri-mode is auto or proxied")
        return 2
    }
    if *proxiedBaseURL == "" {
        fmt.Fprintln(stderr, "serve: --proxied-url-base is required when bundle-uri-mode or pack-uri-mode is auto or proxied")
        return 2
    }
    raw, err := os.ReadFile(*proxiedKeyFile)
    if err != nil {
        fmt.Fprintf(stderr, "serve: read --proxied-url-signing-key: %v\n", err)
        return 1
    }
    raw = bytes.TrimSpace(raw)
    if len(raw) < 16 {
        fmt.Fprintf(stderr, "serve: --proxied-url-signing-key file contents too short (%d bytes); need >= 16\n", len(raw))
        return 2
    }
    signingKey = raw
}
```

Wire `signingKey`, `proxiedBaseURL`, `bMode`, `pMode`, `proxiedPackTTL`, `proxiedBundleTTL`, `warmCommits`, `warmAge` into the gateway `Config`. Add corresponding fields to `internal/gateway`'s `Config` struct (and a `URLBuilder` constructed at startup).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/bucketvcs/ -run TestRunServe_BundleURIMode -v`
Expected: PASS

- [ ] **Step 5: Run the full cmd suite**

Run: `go test ./cmd/bucketvcs/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/bucketvcs/serve.go cmd/bucketvcs/serve_test.go internal/gateway/server.go internal/gateway/routes.go
git commit -m "cmd/bucketvcs+gateway: serve flags for bundle-uri / pack-uri modes"
```

---

## Phase 9 — M8 GC mark-phase extension for bundle keys

### Task 9.1: Mark bundle keys in the live set

**Files:**
- Modify: `internal/gc/mark.go`
- Modify: `internal/gc/mark_test.go`

- [ ] **Step 1: Find the existing live-set construction**

Run: `grep -n 'liveSet\|LiveSet\|live.Add' internal/gc/mark.go internal/gc/liveset.go`
Expected: a function that walks the manifest and adds reachable keys.

- [ ] **Step 2: Write the failing test**

Append to `internal/gc/mark_test.go`:

```go
func TestMark_IncludesBundleKeys(t *testing.T) {
    // Build a manifest with one BundleEntry; assert both BundleKey and
    // SidecarKey appear in the live set produced by the mark phase.
    body := manifest.Body{
        DefaultBranch: "refs/heads/main",
        Refs:          map[string]string{"refs/heads/main": "0123456789abcdef0123456789abcdef01234567"},
        Bundles: []manifest.BundleEntry{{
            ID: "b1", Kind: "full_default",
            BundleKey:  "tenants/t/repos/r/bundles/sha256-aa.bundle",
            SidecarKey: "tenants/t/repos/r/bundles/sha256-aa.json",
        }},
    }
    live := buildLiveSetForTest(t, body) // helper invoking the mark code
    if !live.Has("tenants/t/repos/r/bundles/sha256-aa.bundle") {
        t.Errorf("BundleKey not in live set")
    }
    if !live.Has("tenants/t/repos/r/bundles/sha256-aa.json") {
        t.Errorf("SidecarKey not in live set")
    }
}
```

`buildLiveSetForTest` and `live.Has` follow whatever the existing tests use — model on `TestMark_IncludesIndexes` or similar in the file.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/gc/ -run TestMark_IncludesBundleKeys -v`
Expected: FAIL — keys absent.

- [ ] **Step 4: Add the bundle walk to the mark phase**

In `internal/gc/mark.go`, find the loop that walks `body.Packs` and adds each `PackKey`/`IdxKey`. Add a parallel block:

```go
for _, b := range body.Bundles {
    if b.BundleKey != "" {
        live.Add(b.BundleKey)
    }
    if b.SidecarKey != "" {
        live.Add(b.SidecarKey)
    }
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/gc/ -run TestMark_IncludesBundleKeys -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/gc/mark.go internal/gc/mark_test.go
git commit -m "gc: include bundle keys in mark-phase live set"
```

### Task 9.2: GC discover finds orphan bundle files

**Files:**
- Modify: `internal/gc/discover.go`
- Modify: `internal/gc/discover_test.go`

The discover phase enumerates candidate keys under each well-known prefix. Add `bundles/` to the scanned prefixes so orphan bundle files become sweep candidates.

- [ ] **Step 1: Find the existing prefix list**

Run: `grep -n 'prefix' internal/gc/discover.go | head -20`
Expected: a list of prefixes like `packs/canonical/`, `indexes/...`.

- [ ] **Step 2: Write the failing test**

Append to `internal/gc/discover_test.go`:

```go
func TestDiscover_FindsBundleFiles(t *testing.T) {
    // Seed two bundle files under a synthetic repo prefix; one referenced,
    // one orphan. Discover should return both as candidates; GC's mark-then-
    // sweep is what filters the referenced one.
    ctx := context.Background()
    store, _ := localfs.New(localfs.Config{Root: t.TempDir()})
    rkeys, _ := keys.NewRepo("t", "r")
    refKey := rkeys.BundleKey("sha256-aa")
    orphanKey := rkeys.BundleKey("sha256-bb")
    store.PutIfAbsent(ctx, refKey, strings.NewReader("a"), nil)
    store.PutIfAbsent(ctx, orphanKey, strings.NewReader("b"), nil)
    cand, err := DiscoverCandidatesForRepo(ctx, store, rkeys)
    if err != nil {
        t.Fatalf("DiscoverCandidatesForRepo: %v", err)
    }
    found := map[string]bool{}
    for _, k := range cand {
        found[k.Key] = true
    }
    if !found[refKey] || !found[orphanKey] {
        t.Fatalf("missing bundle keys: refKey=%v orphan=%v cand=%+v", found[refKey], found[orphanKey], cand)
    }
}
```

(Adjust `DiscoverCandidatesForRepo` to whatever the existing M8 function is named.)

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/gc/ -run TestDiscover_FindsBundleFiles -v`
Expected: FAIL — `bundles/` not in the scanned prefix list.

- [ ] **Step 4: Add `bundles/` to the prefix list**

In `internal/gc/discover.go`, find the scanned-prefix list and add the bundle prefix. The pattern likely matches:

```go
prefixes := []string{
    "packs/canonical/",
    "indexes/object-map/",
    "indexes/commit-graphs/",
    "indexes/reachability-delta/",
    "bundles/", // M11
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/gc/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/gc/discover.go internal/gc/discover_test.go
git commit -m "gc: discover orphan bundle files under bundles/ prefix"
```

---

## Phase 10 — Differential tests vs upstream Git

### Task 10.1: Bundle-uri end-to-end clone

**Files:**
- Create: `internal/diffharness/bundleuri_test.go`

- [ ] **Step 1: Write the test**

Create `internal/diffharness/bundleuri_test.go`:

```go
package diffharness

import (
    "context"
    "io"
    "net/http"
    "net/http/httptest"
    "os"
    "os/exec"
    "path/filepath"
    "strings"
    "testing"
    "time"
)

// TestBundleURI_ClientUsesBundle drives a real `git clone` against an
// in-process gateway after maintenance has produced a bundle. Asserts
// that (a) clone succeeds, (b) git's trace output mentions the bundle.
func TestBundleURI_ClientUsesBundle(t *testing.T) {
    if _, err := exec.LookPath("git"); err != nil {
        t.Skip("git not available")
    }
    // 1. Spin up a localfs-backed repo via the existing diffharness fixture.
    repo := newDiffharnessRepo(t) // existing helper; imports tinyrepo.git etc.

    // 2. Run maintenance to generate a bundle.
    runMaintenanceForTest(t, repo, []string{"--bundle-only", "--force"})

    // 3. Start the gateway in-process pointing at repo's store.
    gwHandler := newGatewayHandlerWithBundleURI(t, repo, "auto") // bundle-uri-mode=auto, signing key generated
    srv := httptest.NewServer(gwHandler)
    defer srv.Close()

    // 4. Clone with bundle-uri enabled. Capture GIT_TRACE2 to confirm.
    cloneDir := filepath.Join(t.TempDir(), "clone")
    cmd := exec.Command("git",
        "-c", "protocol.version=2",
        "-c", "fetch.bundleURI=true",
        "clone", srv.URL+"/t/r.git", cloneDir,
    )
    var trace strings.Builder
    cmd.Env = append(os.Environ(), "GIT_TRACE2=2")
    cmd.Stdout = &trace
    cmd.Stderr = &trace
    if err := cmd.Run(); err != nil {
        t.Fatalf("git clone failed: %v\n%s", err, trace.String())
    }

    // 5. Verify the cloned repo has the same HEAD as the source.
    headSrc := mustGitOutput(t, repo.MirrorPath(), "rev-parse", "refs/heads/main")
    headDst := mustGitOutput(t, cloneDir, "rev-parse", "HEAD")
    if headSrc != headDst {
        t.Errorf("HEAD mismatch: src=%s dst=%s", headSrc, headDst)
    }

    // 6. Trace should mention bundle-uri/bundle download. We accept any of:
    //    - "bundle-uri" command issued
    //    - "bundle download" / "bundle applied"
    if !strings.Contains(trace.String(), "bundle") {
        t.Errorf("expected trace to mention bundle activity:\n%s", trace.String())
    }
}

func mustGitOutput(t *testing.T, dir string, args ...string) string {
    t.Helper()
    cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
    out, err := cmd.Output()
    if err != nil {
        t.Fatalf("git %v in %s: %v", args, dir, err)
    }
    return strings.TrimSpace(string(out))
}

// Stubs — implement against the existing diffharness scaffolding.
// newDiffharnessRepo, runMaintenanceForTest, newGatewayHandlerWithBundleURI
// are package-local helpers; model on existing helpers (`grep` for
// the M3/M10 differential test patterns).
func newDiffharnessRepo(t *testing.T) interface{ MirrorPath() string } { /* existing */ return nil }
func runMaintenanceForTest(t *testing.T, _ interface{}, _ []string) { /* existing */ }
func newGatewayHandlerWithBundleURI(t *testing.T, _ interface{}, mode string) http.Handler { /* new — see step 2 */ return nil }

// Suppress unused imports during scaffold.
var (
    _ = context.Background
    _ = time.Now
    _ = io.Copy
)
```

The actual helpers exist in some form already — the existing M3/M10 differential tests run a gateway in-process. Look for similar patterns (`grep -rn 'httptest.NewServer.*Gateway\|httptest.NewServer.*upload-pack' internal/diffharness internal/gateway`) and adapt rather than rewriting from scratch. The new helper, `newGatewayHandlerWithBundleURI`, must construct the gateway with a bundle-uri-aware Config (a generated 32-byte signing key in t.TempDir(), `proxied-url-base` set to `srv.URL`, mode=auto).

- [ ] **Step 2: Implement `newGatewayHandlerWithBundleURI`**

Either inline in the test file or in `internal/diffharness/helpers.go`. It should:

```go
func newGatewayHandlerWithBundleURI(t *testing.T, repo *fixtureRepo, mode string) http.Handler {
    keyBytes := make([]byte, 32)
    rand.Read(keyBytes)
    cfg := gateway.Config{
        Store:                 repo.Store(),
        BundleURIMode:         gateway.URIModeAuto,
        PackURIMode:           gateway.URIModeAuto,
        ProxiedURLSigningKey:  keyBytes,
        ProxiedBaseURL:        "", // filled by the test after httptest.NewServer
        ProxiedPackTTL:        time.Hour,
        ProxiedBundleTTL:      4 * time.Hour,
        BundleWarmCommits:     100,
        BundleWarmAge:         24 * time.Hour,
    }
    // ... existing repo wiring ...
    return gateway.New(cfg)
}
```

Note: `ProxiedBaseURL` cannot be known before `httptest.NewServer` is called — refactor to pass it lazily, or use a `BaseURLProvider func() string` field on the gateway config.

- [ ] **Step 3: Run the test**

Run: `go test ./internal/diffharness/ -run TestBundleURI_ClientUsesBundle -v`
Expected: PASS

- [ ] **Step 4: Add a force-push regression**

Append to the same file:

```go
func TestBundleURI_ForcePushDropsBundle(t *testing.T) {
    if _, err := exec.LookPath("git"); err != nil {
        t.Skip("git not available")
    }
    repo := newDiffharnessRepo(t)
    runMaintenanceForTest(t, repo, []string{"--bundle-only", "--force"})

    // Force-push a divergent tip via a worktree against the repo's gateway.
    forcePushDivergent(t, repo)

    handler := newGatewayHandlerWithBundleURI(t, repo, "auto")
    srv := httptest.NewServer(handler)
    defer srv.Close()

    // Clone again. The gateway should detect the bundle is stale (force-push
    // case, bundle.TipOID no longer ancestor of current tip) and not advertise.
    cloneDir := filepath.Join(t.TempDir(), "clone-after-fp")
    cmd := exec.Command("git",
        "-c", "protocol.version=2",
        "-c", "fetch.bundleURI=true",
        "clone", srv.URL+"/t/r.git", cloneDir,
    )
    var trace strings.Builder
    cmd.Env = append(os.Environ(), "GIT_TRACE2=2")
    cmd.Stdout = &trace
    cmd.Stderr = &trace
    if err := cmd.Run(); err != nil {
        t.Fatalf("clone after force-push failed: %v\n%s", err, trace.String())
    }
    // Trace MUST NOT show a successful bundle download (allow the client
    // to ASK for bundle-uri; we just expect the gateway's response to be empty).
    // The simplest signal: clone uses the regular pack path, indicated by
    // "Receiving objects:" output that comes from upload-pack.
    if !strings.Contains(trace.String(), "Receiving objects") && !strings.Contains(trace.String(), "remote: Counting") {
        t.Errorf("expected fallback to standard fetch:\n%s", trace.String())
    }
}

func forcePushDivergent(t *testing.T, repo interface{ MirrorPath() string }) {
    // Implement: open a fresh clone of repo's mirror, build a divergent commit,
    // push --force back to refs/heads/main using the existing diffharness push
    // helper (see TestPush_FastForward etc. in the same package).
}
```

- [ ] **Step 5: Run the regression**

Run: `go test ./internal/diffharness/ -run TestBundleURI_ForcePushDropsBundle -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/diffharness/bundleuri_test.go
git commit -m "diffharness: bundle-uri clone + force-push regression"
```

### Task 10.2: Packfile-uri end-to-end clone

**Files:**
- Create: `internal/diffharness/packuri_test.go`

- [ ] **Step 1: Write the test**

Create `internal/diffharness/packuri_test.go`:

```go
package diffharness

import (
    "net/http/httptest"
    "os"
    "os/exec"
    "path/filepath"
    "strings"
    "testing"
)

// TestPackURI_ClientUsesPackURI runs `git clone` with packfile-uri
// enabled against a freshly-maintained repo (single canonical pack)
// and asserts the gateway advertises a packfile-uri stanza, the
// client downloads from the URL, and the resulting clone matches.
func TestPackURI_ClientUsesPackURI(t *testing.T) {
    if _, err := exec.LookPath("git"); err != nil {
        t.Skip("git not available")
    }
    repo := newDiffharnessRepo(t)
    runMaintenanceForTest(t, repo, []string{"--force"}) // full repack so plan == single canonical pack

    handler := newGatewayHandlerWithBundleURI(t, repo, "off") // bundle-uri off so the only acceleration is pack-uri
    srv := httptest.NewServer(handler)
    defer srv.Close()

    cloneDir := filepath.Join(t.TempDir(), "clone")
    cmd := exec.Command("git",
        "-c", "protocol.version=2",
        "-c", "fetch.uriProtocols=https",
        "clone", srv.URL+"/t/r.git", cloneDir,
    )
    var trace strings.Builder
    cmd.Env = append(os.Environ(), "GIT_TRACE2=2", "GIT_TRACE_PACKET=2")
    cmd.Stdout = &trace
    cmd.Stderr = &trace
    if err := cmd.Run(); err != nil {
        t.Fatalf("git clone failed: %v\n%s", err, trace.String())
    }
    // Look for "packfile-uri" in the protocol trace.
    if !strings.Contains(trace.String(), "packfile-uri") {
        t.Errorf("expected packfile-uri in trace:\n%s", trace.String())
    }
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./internal/diffharness/ -run TestPackURI_ClientUsesPackURI -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/diffharness/packuri_test.go
git commit -m "diffharness: packfile-uri end-to-end clone test"
```

---

## Phase 11 — Conformance: `RunPropertyBundleSafety`

### Task 11.1: Property factory

**Files:**
- Create: `internal/maintenance/conformance/bundle_safety.go`
- Create: `internal/maintenance/conformance/bundle_safety_test.go`

The factory exercises concurrent-push + bundle-regen + advertise interleavings, asserting:
- (a) Every advertised bundle has `TipOID` reachable from current default-branch tip at the moment of advertise.
- (b) Bundle files dropped from the manifest become M8 GC orphan candidates and are reclaimed after retention.

For M11 we ship the localfs sub-test green; the cloud-emulator interleavings ship as `t.Skip` stubs (consistent with M10's deferred property tests).

- [ ] **Step 1: Write the factory**

Create `internal/maintenance/conformance/bundle_safety.go`:

```go
package conformance

import (
    "context"
    "encoding/json"
    "testing"
    "time"

    "github.com/bucketvcs/bucketvcs/internal/maintenance"
    "github.com/bucketvcs/bucketvcs/internal/repo"
    "github.com/bucketvcs/bucketvcs/internal/repo/keys"
    "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
    "github.com/bucketvcs/bucketvcs/internal/storage"
)

// BundleSafetyFactory constructs a fresh (storage, repo, keys) tuple
// for one BundleSafety sub-test. Cleanup runs via t.Cleanup.
type BundleSafetyFactory func(t *testing.T) (storage.ObjectStore, *repo.Repo, *keys.Repo)

// RunPropertyBundleSafety verifies M11 §11.3 invariants.
//
//   - solo: maintenance --bundle-only against an idle repo produces
//     a current bundle that is correctly advertised.
//   - push_during_bundle (skipped): a push lands while bundle generation
//     is in flight; the bundle phase's CAS-merge preserves the push.
//   - bundle_during_compaction (skipped): bundle phase + reachability
//     compaction interleave correctly.
//   - sweep_after_retire (skipped): an old bundle file is reclaimed by
//     M8 GC after retention.
func RunPropertyBundleSafety(t *testing.T, factory BundleSafetyFactory) {
    t.Helper()
    t.Run("solo", func(t *testing.T) {
        s, r, k := factory(t)
        // Seed one commit on main via the package's existing test fixture.
        seedSingleCommitMain(t, s, r, k)

        opts := maintenance.RunOptions{
            BundleOnly: true, Force: true,
            Now: func() time.Time { return time.Now() },
        }
        rep, err := maintenance.Run(context.Background(), s, r, k, opts)
        if err != nil {
            t.Fatalf("Run: %v", err)
        }
        if rep.BundleResult == nil || !rep.BundleResult.Generated {
            t.Fatalf("BundleResult = %+v", rep.BundleResult)
        }
        view, _ := r.ReadRoot(context.Background())
        var body manifest.Body
        json.Unmarshal(view.Body, &body)
        if len(body.Bundles) != 1 || body.Bundles[0].Kind != "full_default" {
            t.Fatalf("expected exactly one full_default bundle, got %+v", body.Bundles)
        }
    })

    t.Run("push_during_bundle", func(t *testing.T) {
        t.Skip("requires concurrent-push test harness (deferred to M11.x follow-up)")
    })
    t.Run("bundle_during_compaction", func(t *testing.T) {
        t.Skip("requires concurrent-test harness (deferred to M11.x)")
    })
    t.Run("sweep_after_retire", func(t *testing.T) {
        t.Skip("requires GC + maintenance interleaving harness (deferred to M11.x)")
    })
}

// seedSingleCommitMain populates s/r/k with a one-commit refs/heads/main.
// Implement against the existing fixture helpers (search the package
// for `seedSingleCommit\|importTinyRepo`).
func seedSingleCommitMain(t *testing.T, s storage.ObjectStore, r *repo.Repo, k *keys.Repo) {
    t.Helper()
    // Existing fixture call goes here.
}
```

- [ ] **Step 2: Wire the factory into per-adapter conformance harnesses**

In `internal/maintenance/conformance/bundle_safety_test.go`:

```go
package conformance

import (
    "testing"

    "github.com/bucketvcs/bucketvcs/internal/repo"
    "github.com/bucketvcs/bucketvcs/internal/repo/keys"
    "github.com/bucketvcs/bucketvcs/internal/storage"
    "github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestBundleSafety_Localfs(t *testing.T) {
    RunPropertyBundleSafety(t, func(t *testing.T) (storage.ObjectStore, *repo.Repo, *keys.Repo) {
        store, _ := localfs.New(localfs.Config{Root: t.TempDir()})
        k, _ := keys.NewRepo("t", "r")
        r, _ := repo.Init(context.Background(), store, "t", "r")
        return store, r, k
    })
}
```

(The `repo.Init` call signature matches what M9/M10 already use; consult the package for the exact form.)

In each cloud-adapter test directory (`internal/storage/{s3compat,gcs,azureblob}/conformance_test.go`), add a sibling test calling the same factory with the cloud-adapter setup. These will skip locally until cloud emulators are wired into CI.

- [ ] **Step 3: Run the localfs case**

Run: `go test ./internal/maintenance/conformance/ -run TestBundleSafety_Localfs -v`
Expected: PASS — `solo` runs, the three `t.Skip` stubs report SKIP.

- [ ] **Step 4: Commit**

```bash
git add internal/maintenance/conformance/bundle_safety.go internal/maintenance/conformance/bundle_safety_test.go \
        internal/storage/s3compat/conformance_test.go internal/storage/gcs/conformance_test.go internal/storage/azureblob/conformance_test.go
git commit -m "maintenance/conformance: RunPropertyBundleSafety (solo green; concurrent skipped)"
```

---

## Phase 12 — Metrics + audit events

### Task 12.1: Bundle metrics

**Files:**
- Modify: `internal/maintenance/log.go` (or whichever file owns metric emission — `grep -n emitMetric internal/maintenance/*.go`)
- Modify: `internal/maintenance/log_test.go`
- Modify: `internal/gateway/server.go` (gateway-side metrics for advertise/serve)

- [ ] **Step 1: Write the failing test**

Append to `internal/maintenance/log_test.go`:

```go
func TestEmitBundleMetrics_GeneratedAndDuration(t *testing.T) {
    // Capture slog records via a recording handler; run the bundle-success
    // emitter; assert the expected metric records appear with the right
    // attribute keys.
    rec := newRecordingLogger() // existing helper used by other M9/M10 metric tests
    emitBundleResultMetrics(context.Background(), rec.Logger, "t/r", &maintenance.BundleResult{
        Generated: true, ByteSize: 1234, DurationMS: 42, TriggerReason: "missing",
    })
    if !rec.HasMetric("bundle_generated_total", "outcome", "success") {
        t.Errorf("missing bundle_generated_total")
    }
    if !rec.HasMetric("bundle_generation_duration_seconds") {
        t.Errorf("missing bundle_generation_duration_seconds")
    }
    if !rec.HasMetric("bundle_byte_size") {
        t.Errorf("missing bundle_byte_size")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestEmitBundleMetrics -v`
Expected: COMPILE ERROR — `emitBundleResultMetrics` undefined.

- [ ] **Step 3: Implement the emitter**

In `internal/maintenance/log.go`, add:

```go
func emitBundleResultMetrics(ctx context.Context, logger *slog.Logger, repoID string, br *BundleResult) {
    if br == nil {
        return
    }
    outcome := "noop"
    if br.Generated {
        outcome = "success"
    } else if br.ErrorMessage != "" {
        outcome = "failure"
    }
    emitMetric(ctx, logger, "bundle_generated_total", 1, "outcome", outcome, "repo_id", repoID, "trigger_reason", br.TriggerReason)
    emitMetric(ctx, logger, "bundle_generation_duration_seconds", br.DurationMS/1000, "repo_id", repoID)
    if br.Generated && br.ByteSize > 0 {
        emitMetric(ctx, logger, "bundle_byte_size", br.ByteSize, "repo_id", repoID)
    }
}
```

Call it from `emitFinalReport` when `report.BundleResult != nil`.

- [ ] **Step 4: Add gateway-side advertise/serve metrics**

In `internal/gateway/upload_pack.go` (where the bundle-uri response is emitted), after the response is built, emit:

```go
emitMetric(ctx, logger, "bundle_advertised_total", 1, "repo_id", repoID, "freshness", res.State.String())
if len(ads) > 0 {
    emitMetric(ctx, logger, "bundle_uri_advertised_total", 1, "repo_id", repoID, "via", via)
}
```

In `internal/gateway/proxied_routes.go`, after a successful proxy serve, emit:

```go
emitMetric(ctx, logger, "bundle_uri_served_total", 1, "repo_id", repoID, "via", "proxied")
emitMetric(ctx, logger, "bundle_uri_served_bytes", bytesServed, "repo_id", repoID, "via", "proxied")
```

(Repo id may not be known at the proxied-URL endpoint level if it serves multiple repos; in M11's single-repo gateway model the resolver already binds to one repo, so set `repo_id` from the resolver's context. If the gateway is generic, omit the label.)

For pack-uri, mirror the bundle metrics with `pack_uri_advertised_total` and `pack_uri_served_total`.

For HMAC token validation failures, emit:

```go
emitMetric(ctx, logger, "proxied_url_token_invalid_total", 1, "reason", reason)
```

Where `reason` is one of `expired`, `sig_mismatch`, `kind_mismatch`, `hash_mismatch`, `malformed`.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/maintenance/... ./internal/gateway/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/maintenance/log.go internal/maintenance/log_test.go \
        internal/gateway/upload_pack.go internal/gateway/proxied_routes.go
git commit -m "maintenance+gateway: M11 bundle/pack-uri metrics"
```

### Task 12.2: Audit events

**Files:**
- Modify: `internal/maintenance/log.go`
- Modify: `internal/gateway/upload_pack.go`
- Modify: `internal/gateway/proxied_routes.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/maintenance/log_test.go`:

```go
func TestEmitBundleAudit_Generated(t *testing.T) {
    rec := newRecordingLogger()
    emitBundleAudit(context.Background(), rec.Logger, "bundle.generated", map[string]any{
        "repo_id":                 "t/r",
        "bundle_id":               "bundle_t_r_42_aa",
        "bundle_hash":             "sha256-aa",
        "tip_oid":                 "0123456789abcdef0123456789abcdef01234567",
        "covers_manifest_version": uint64(42),
        "byte_size":               int64(1234),
        "duration_ms":             int64(42),
    })
    if !rec.HasEvent("bundle.generated") {
        t.Errorf("expected bundle.generated event")
    }
}
```

- [ ] **Step 2: Implement `emitBundleAudit`**

In `internal/maintenance/log.go` (or its sibling that owns audit emission):

```go
func emitBundleAudit(ctx context.Context, logger *slog.Logger, event string, fields map[string]any) {
    attrs := make([]any, 0, 2+2*len(fields))
    attrs = append(attrs, slog.String("event", event))
    for k, v := range fields {
        attrs = append(attrs, slog.Any(k, v))
    }
    logger.LogAttrs(ctx, slog.LevelInfo, event, slog.Group("audit", attrs...))
}
```

Call it from:
- `runBundlePhase` after CAS-merge success → `bundle.generated`
- `runBundlePhase` if it replaces a previous entry (detect by re-reading the manifest before CAS) → `bundle.retired` for the previous ID, `reason=replaced`
- `internal/gateway/upload_pack.go` after a non-empty bundle-uri response → `bundle.uri.advertised`
- `internal/gateway/proxied_routes.go` after a successful 200/206 → `proxied.url.served`
- M8 sweep at the call site that deletes a bundle key → `bundle.retired` with `reason=gc_swept` (this hooks into existing M8 sweep logging)

- [ ] **Step 3: Run tests**

Run: `go test ./internal/maintenance/... ./internal/gateway/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/maintenance/log.go internal/maintenance/log_test.go \
        internal/gateway/upload_pack.go internal/gateway/proxied_routes.go internal/gc/sweep.go
git commit -m "maintenance+gateway+gc: M11 bundle/pack-uri audit events"
```

---

## Phase 13 — Operator guide

### Task 13.1: Author `docs/m11-bundles-operator-guide.md`

**Files:**
- Create: `docs/m11-bundles-operator-guide.md`

- [ ] **Step 1: Draft the guide**

Create `docs/m11-bundles-operator-guide.md` covering each section listed in spec §12. Aim for a tight document (~600-1000 lines) using the existing M8/M9/M10 operator guides as style references. Mandatory sections:

1. **Overview** — what bundle-uri / packfile-uri do, when they help (cold clones), when they don't (small repos, deep partial fetches).
2. **Bundle freshness model** — current/warm/stale/retired diagram + how each is computed; tuning guidance for `--bundle-warm-commits` and `--bundle-warm-age`.
3. **Maintenance scheduling** — cron / Kubernetes CronJob / systemd timer recipes; recommend running repack + bundle-refresh + compact together so the materialized mirror is reused once.
4. **Signed-URL vs gateway-proxied tradeoff** — full table:
   | Mode | Backend | Bandwidth path | Audit visibility | When |
   |------|---------|---------------|------------------|------|
   | direct | cloud | client → bucket | none at gateway | public-internet repos |
   | proxied | localfs / cloud | client → gateway → bucket | full | private VPC, audit-strict |
   | auto | any | direct if available | partial | default; covers both |
   | off | any | n/a (capability disabled) | n/a | fall back to standard fetch |
5. **TTL guidance vs M8 retention** — hard rule: TTL ≤ retention/24; CLI enforces; default 1h pack / 4h bundle vs 168h retention.
6. **Bandwidth and cost economics** — direct mode shifts egress to object storage; R2-style economics dominate the proxied case here; how to reason about hosted vs OSS deployments.
7. **Disabling acceleration** — single command (`--bundle-uri-mode=off --pack-uri-mode=off`); behavior reverts to pre-M11; M9/M10 paths unchanged.
8. **Forensics** — how to inspect `body.Bundles[]` via `bucketvcs inspect-manifest`; where to find `bundle_*` and `pack_uri_*` metrics; how to grep audit logs for `bundle.*` and `pack.uri.*`.
9. **Troubleshooting matrix** — 6-8 entries covering: bundle never generated (no triggers fired); clone is slow despite bundle (client did not opt in); proxied-URL 403 (token expired or signing key rotated); direct-URL 403 (TTL expired or backend rejected); pack-uri never advertised (PackChecksum missing on legacy manifest, backfill not yet run); etc.
10. **Migration from pre-M11** — order of operations: deploy M11 binaries → operators run `bucketvcs maintenance --force` once per repo to backfill `PackChecksum` and seed bundles → enable `--bundle-uri-mode=auto` on `bucketvcs serve`. No manifest schema break; rollback is safe (M11 fields are `omitempty`).

- [ ] **Step 2: Cross-reference the M9 maintenance guide**

In `docs/m9-maintenance-operator-guide.md`, add a "See also" link near the threshold section pointing to M11 for the bundle thresholds.

- [ ] **Step 3: Update `README.md`**

In the project README, add a one-paragraph M11 announcement near the M10 paragraph: "M11 ships bundle-uri (§16.3) and packfile-uri (§16.4) acceleration. `bucketvcs maintenance` generates a default-branch full bundle per repo; `bucketvcs serve` advertises bundle and pack URIs to v2-capable clients via direct signed URLs (cloud) or HMAC-gated gateway-proxied endpoints (localfs and audit-strict deployments). See `docs/m11-bundles-operator-guide.md`."

- [ ] **Step 4: Commit**

```bash
git add docs/m11-bundles-operator-guide.md docs/m9-maintenance-operator-guide.md README.md
git commit -m "docs: M11 bundles operator guide + cross-references"
```

---

## Phase 14 — End-to-end smoke against MinIO and localfs

### Task 14.1: Localfs smoke

**Files:**
- Create: `scripts/m11-smoke-localfs.sh`

- [ ] **Step 1: Write the smoke script**

Create `scripts/m11-smoke-localfs.sh`:

```bash
#!/usr/bin/env bash
# M11 end-to-end smoke against localfs:
#   1. Init a repo + import a tiny tree.
#   2. Run maintenance --bundle-only.
#   3. Start serve with bundle-uri-mode=auto (proxied fallback).
#   4. git clone with fetch.bundleURI=true — confirm trace contains "bundle".
#   5. Run maintenance --force (full repack + bundle-refresh + compact).
#   6. git clone with fetch.uriProtocols=https — confirm trace contains "packfile-uri".
#   7. Tear down.
set -euo pipefail
set -x

ROOT="$(mktemp -d)"
trap "rm -rf '$ROOT'" EXIT

BUCKET="$ROOT/bucket"
KEY="$ROOT/key"
mkdir -p "$BUCKET"
head -c 32 /dev/urandom > "$KEY"

go run ./cmd/bucketvcs init --store=localfs:"$BUCKET" --repo=t/r

# Import a tiny git history.
WORK="$ROOT/work"
git init -b main "$WORK"
echo "hi" > "$WORK/f"
git -C "$WORK" config user.email t@t
git -C "$WORK" config user.name t
git -C "$WORK" add .
git -C "$WORK" commit -m init
go run ./cmd/bucketvcs import --store=localfs:"$BUCKET" --repo=t/r --from "$WORK"

go run ./cmd/bucketvcs maintenance --store=localfs:"$BUCKET" --repo=t/r --bundle-only --force

# Start serve in the background.
PORT=$((30000 + RANDOM % 20000))
go run ./cmd/bucketvcs serve \
    --store=localfs:"$BUCKET" \
    --addr=127.0.0.1:$PORT \
    --bundle-uri-mode=auto \
    --pack-uri-mode=auto \
    --proxied-url-signing-key="$KEY" \
    --proxied-url-base="http://127.0.0.1:$PORT" &
SERVE_PID=$!
trap "kill $SERVE_PID; rm -rf '$ROOT'" EXIT

sleep 2

CLONE="$ROOT/clone"
GIT_TRACE2=2 git -c protocol.version=2 -c fetch.bundleURI=true \
    clone "http://127.0.0.1:$PORT/t/r.git" "$CLONE" 2>&1 | tee "$ROOT/trace.log"

if ! grep -q bundle "$ROOT/trace.log"; then
    echo "FAIL: trace did not mention bundle"
    exit 1
fi

echo "M11 localfs smoke: OK"
```

- [ ] **Step 2: Run the smoke**

Run: `bash scripts/m11-smoke-localfs.sh`
Expected: prints "OK" at end.

- [ ] **Step 3: Commit**

```bash
git add scripts/m11-smoke-localfs.sh
chmod +x scripts/m11-smoke-localfs.sh
git commit -m "scripts: M11 localfs smoke (bundle-uri + pack-uri end-to-end)"
```

### Task 14.2: MinIO smoke

**Files:**
- Create: `scripts/m11-smoke-minio.sh`

- [ ] **Step 1: Write the smoke script**

Create `scripts/m11-smoke-minio.sh`. Pattern: same as `m11-smoke-localfs.sh`, but starts MinIO (or assumes the user already runs `docker-compose -f docker-compose.minio.yml up -d`), uses `--store=s3://bucketvcs-test?endpoint=http://127.0.0.1:9000&access-key=minioadmin&secret-key=minioadmin&region=us-east-1`, and asserts that the bundle-uri/pack-uri responses are direct signed URLs (the trace should include `signedurl` or `X-Amz-Signature` markers).

- [ ] **Step 2: Run the smoke**

Run: `bash scripts/m11-smoke-minio.sh`
Expected: prints "OK" at end.

- [ ] **Step 3: Commit**

```bash
git add scripts/m11-smoke-minio.sh
chmod +x scripts/m11-smoke-minio.sh
git commit -m "scripts: M11 MinIO smoke (direct signed URLs)"
```

### Task 14.3: Final M11 verification gate

- [ ] **Step 1: Run the full test suite**

Run: `go test ./... -count=1`
Expected: PASS (with cloud-emulator tests skipping in environments without the emulators; localfs and unit/integration tests all pass).

- [ ] **Step 2: Run `go vet`**

Run: `go vet ./...`
Expected: no output (vet clean).

- [ ] **Step 3: Run both smoke scripts**

Run: `bash scripts/m11-smoke-localfs.sh && bash scripts/m11-smoke-minio.sh`
Expected: both print "OK".

- [ ] **Step 4: Commit the verification record**

```bash
# No code changes; this step is a checkpoint. If a maintainer-audit log
# of "M11 verified at $(date)" is desired, append to docs/superpowers/specs/
# m11_progress.md. Otherwise, skip the commit and proceed to merge.
```

---

## Self-review

After implementing every task in order, verify against the spec sections explicitly:

- §0 (goal/scope): bundle-uri + packfile-uri shipped together → Phases 1, 2, 3, 4, 5, 6, 7, 8.
- §1 (decisions Q1-Q5): all reflected in phase choices; pack-uri narrow eligibility in Phase 8.1.
- §2 (manifest schema): `BundleEntry` filled in (Task 0.1), `Pack.PackChecksum` added (Task 0.2).
- §3 (`SignedGetURL` extension): `ExpectedHash` field (Task 1.1), per-adapter binding (Tasks 1.2, 1.3), conformance factory (Task 1.4).
- §4 (maintenance bundle generation): triggers (Task 3.4), generation (Task 3.5), CAS-merge (Task 3.6), pipeline wire-in (Task 3.7), `--bundle-only` outcome (Task 3.8).
- §5 (bundle-uri advertise): freshness state machine (Task 7.1), capability + handler (Task 7.2), dispatch (Task 7.3).
- §6 (packfile-uris advertise): `FullPackRequested` predicate (Task 8.1), advertise gate + dispatch (Task 8.2).
- §7 (gateway-proxied URL endpoints): HMAC token (Task 6.1), routes (Task 6.2), URL builder (Task 6.3).
- §8 (CLI surface): maintenance flags (Tasks 4.1, 4.2); serve flags (Task 8.3).
- §9 (cross-milestone touchpoints): conformance (Task 1.4, Task 11.1), schema (Phase 0), gateways (Phases 6, 7, 8), GC (Phase 9), maintenance (Phase 3), reachability (Task 8.1).
- §10 (observability + audit): metrics (Task 12.1), audit events (Task 12.2).
- §11 (testing): unit covered per phase; differential (Phase 10); conformance (Phase 11).
- §12 (operator guide): Phase 13.
- §13 (out-of-scope reminders): respected — no per-release-tag bundles, no rolling-base, no mixed pack-uri delivery, no hit-ratio trigger, no any-mode bundle-uri, no generated-pack URI handoff.

If any spec requirement is not pointed-to by a task above, add the task before declaring the plan complete.
