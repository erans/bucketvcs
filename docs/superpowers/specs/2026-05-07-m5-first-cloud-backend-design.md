# M5 — First Cloud Backend (S3-Compatible, R2 First)

Date: 2026-05-07
Status: design draft
Scope: bucketvcs OSS-core milestone M5 only
Source decomposition: `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md`
Source spec sections: §9, §10, §11.1, §12.1, §12.3, §27, §29, §40.4

## What this milestone delivers

A single S3-compatible storage adapter (`internal/storage/s3compat`) that satisfies `storage.ObjectStore` and passes the M0 conformance suite (15 correctness + stress) against:

- **Cloudflare R2** — the canonical M5 backend, matching the §27 hosted-default thesis.
- **AWS S3** — exercised in M5 CI to prove the adapter generalizes; formally promoted to "canonical" at M7.

After M5, `bucketvcs init`, `inspect-manifest`, `import`, `export`, and `serve` all work end-to-end against `--store=s3://<bucket>[/<prefix>]` and `--store=r2://<bucket>[/<prefix>]`. Native Git clients can clone, fetch, and push (with M4 token auth) against a real cloud bucket.

## What this milestone does not deliver

- **GCS** and **Azure Blob** adapters — deferred to M7.
- **Worker bindings** for R2 (alternate access path) — deferred until measured need.
- **localfs → cloud migration tool** — repo bytes are bit-identical across adapters; an operator can `aws s3 sync` manually. Tooling lives at M16 if it's still needed.
- **GC of orphaned multipart uploads** — M8 handles object GC; M16 covers repair.
- **Per-repo cost dashboards**, **multi-region replicas**, **CDN wiring**, **BYOB UX** — hosted/post-MVP concerns.
- **Backend selection per-repo at runtime** — one `--store` per deployment.

## Architecture

### Package layout

```
internal/storage/s3compat/
  s3compat.go          // type S3Compat; var _ storage.ObjectStore = (*S3Compat)(nil)
  config.go            // Config struct, validation
  open.go              // Open(ctx, Config) (*S3Compat, error)
  get.go head.go range.go
  put.go               // PutIfAbsent, PutIfVersionMatches
  delete.go list.go
  multipart.go         // CreateMultipart, MultipartUpload value, CompleteMultipartIfAbsent
  signed.go            // SignedGetURL via presign client
  errs.go              // classify(err) -> storage sentinel
  retry.go             // SDK retryer configuration
  prefix.go            // applyPrefix / stripPrefix helpers
  s3compat_test.go                 // unit tests (no credentials)
  s3compat_conformance_test.go     // live conformance (skip when env unset)
```

R2 is **not** a separate package. `r2://` is a CLI-layer alias for `s3://` with R2-flavored defaults (`Region=auto`, `ForcePathStyle=true`, mandatory endpoint).

### Boundary

The rest of the codebase (M1 `internal/repo`, M2 pack writer, M3 gateway, M4 auth) sees only `storage.ObjectStore`. No file outside `internal/storage/s3compat/` imports from `aws-sdk-go-v2`.

### CLI integration

`cmd/bucketvcs/store.go` grows two scheme cases (`s3`, `r2`), removes them from the "reserved" error path, and keeps the `gcs`/`azureblob` reservation pointing at M7. `parseStoreURL` returns `(scheme, bucket, prefix)` for cloud schemes; `openStore` builds a `Config` from URL + env and calls `s3compat.Open`.

## Configuration

`Config` is the only constructor input. Tests build it directly without env.

```go
package s3compat

type Config struct {
    Bucket          string        // required
    Prefix          string        // optional; trailing "/" normalized
    Region          string        // required; "auto" for R2
    Endpoint        string        // optional for AWS S3; required for R2/MinIO
    ForcePathStyle  bool          // true for R2/MinIO; false (vhost) for AWS S3
    AccessKeyID     string        // optional; falls back to default chain
    SecretAccessKey string
    SessionToken    string
    Profile         string        // optional shared-config profile name

    UploadPartSize    int64         // default 8 MiB
    MaxRetries        int           // default 5
    RequestTimeout    time.Duration // default 60 s
    PresignDefaultTTL time.Duration // default 15 min
}
```

### Env mapping (consumed by `cmd/bucketvcs`, not by the adapter)

```
BUCKETVCS_S3_REGION              (also AWS_REGION as fallback)
BUCKETVCS_S3_ENDPOINT            (R2: required; S3: optional)
BUCKETVCS_S3_FORCE_PATH_STYLE    (default: true for r2://, false for s3://)
BUCKETVCS_S3_PROFILE             (also AWS_PROFILE as fallback)
AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN
                                 (standard SDK credential chain still applies)
```

Secrets are never URL-embedded and never appear in the process arg vector.

### Validation at `Open`

- `Bucket` non-empty.
- `Region` non-empty.
- If scheme is `r2://`, `Endpoint` must be non-empty.
- `Prefix` sanitized: no leading `/`, single trailing `/`, no `..` path components.

## ObjectStore method mapping

Errors from the SDK are normalized via `classify(err)` to the sentinels in `internal/storage/errors.go`.

| Method | SDK call | Notes |
|---|---|---|
| `Get(key)` | `GetObject` | 404 → `ErrNotFound`. `Object.Body` is the SDK response body (caller closes). `Version` = ETag (kind=`etag`). |
| `Head(key)` | `HeadObject` | 404 → `ErrNotFound`. |
| `GetRange(key, start, endIncl)` | `GetObject` with `Range: bytes=start-endIncl` | Negative indices → `ErrInvalidArgument`. Read past EOF returns the partial bytes. |
| `PutIfAbsent(key, body)` | `PutObject` + `If-None-Match: *` | 412 → `ErrAlreadyExists`. R2 + S3 both support this header. |
| `PutIfVersionMatches(key, expected, body)` | `PutObject` + `If-Match: <expected.token>` | 412 → `ErrVersionMismatch` (also when key absent). |
| `DeleteIfVersionMatches(key, expected)` | `DeleteObject` + `If-Match: <expected.token>` | 412 → `ErrVersionMismatch`. 404 → `ErrNotFound`. |
| `List(prefix, opts)` | `ListObjectsV2` | Configured key prefix prepended on the way in, stripped on the way out. `MaxKeys`, `ContinuationToken`, `Delimiter` map directly. |
| `CreateMultipart(key, opts)` | `CreateMultipartUpload` | Returns a `MultipartUpload` value with `UploadID` + resolved key. |
| `MultipartUpload.UploadPart(...)` | `UploadPart` | Returns the part ETag as `MultipartPart.Token`. |
| `CompleteMultipartIfAbsent(upload, parts)` | `CompleteMultipartUpload` + `If-None-Match: *` | 412 → `ErrAlreadyExists`. Adapter does **not** abort on conflict; orphan cleanup is M8 GC / M16 repair territory. |
| `SignedGetURL(key, opts)` | presign client `GetObject` | TTL clamped to `PresignDefaultTTL` if zero. |

Cross-cutting: every operation takes `ctx`; `RequestTimeout` is layered with `context.WithTimeout`. `ContentType` from `PutOptions` / `MultipartOptions` is passed through; otherwise omitted.

### Capabilities

```go
Capabilities{
    SignedURLs:           true,
    StrongList:           true,        // R2 + S3 are strongly consistent
    MultipartMinPartSize: 5 << 20,     // 5 MiB; S3-mandated
    MultipartMaxParts:    10000,
    MaxObjectSize:        5 << 40,     // 5 TiB
}
```

### Single-PUT vs multipart threshold

Objects ≤ `UploadPartSize` (8 MiB default) go via single `PutObject`. Multipart is exposed only via the explicit `CreateMultipart` API; the adapter never silently switches a `PutIfAbsent` call to multipart. M2's pack writer is the only caller that produces objects large enough to use multipart.

## Error mapping & retry

### Classification (`errs.go`)

A single `classify(err) error` walks the error chain (smithy `APIError` codes + HTTP status from `ResponseError`) and returns the wrapped sentinel. Order matters — match more specific cases first.

| Trigger | Returned sentinel |
|---|---|
| `NoSuchKey`, `NotFound`, HTTP 404 on `GetObject`/`HeadObject` | `ErrNotFound` |
| HTTP 412 on `PutObject` / `Complete*` with `If-None-Match: *` | `ErrAlreadyExists` |
| HTTP 412 on `PutObject` / `DeleteObject` with `If-Match: <etag>` | `ErrVersionMismatch` |
| `SlowDown`, `ThrottlingException`, HTTP 429, throttle-coded HTTP 503 | `ErrThrottled` |
| HTTP 5xx not above; dial/EOF/`io.ErrUnexpectedEOF`; transport-level deadline | `ErrTransient` |
| `AccessDenied`, `InvalidAccessKeyId`, `SignatureDoesNotMatch`, HTTP 401/403 | `ErrAccessDenied` |
| `InvalidArgument`, `MalformedXML`, `EntityTooSmall`, client-side validation failures | `ErrInvalidArgument` |
| Anything unrecognized | `errors.Join(ErrTransient, err)` |

Wrapped errors keep the original SDK error reachable via `errors.Unwrap` / `errors.As` so operators see the provider code. Conformance tests assert sentinel via `errors.Is` only.

### Retry policy (`retry.go`)

The SDK retryer is configured, not replaced.

- `retry.NewStandard` with `MaxAttempts = Config.MaxRetries` (default 5), exponential backoff with full jitter.
- The standard retryer already retries 5xx, `RequestTimeout`, `SlowDown`, `ThrottlingException`, `RequestLimitExceeded`, and connection errors. We do not extend the retryable set.
- HTTP 412 (`PreconditionFailed`) is **not** retried. The SDK's default behavior matches; a regression test asserts it.
- Conditional `PutObject` / `CompleteMultipartUpload` calls remain idempotent under retry: a successful write whose response was dropped will, on retry, return 412 and surface as `ErrAlreadyExists` / `ErrVersionMismatch`. Conformance #14 covers this.
- Non-seekable `io.Reader` bodies are buffered into memory before the first attempt when size is below 8 MiB; larger non-seekable bodies cause a clear up-front error. Manifest writes (M1) are tiny; pack writes (M2) use multipart with seekable per-part buffers.

### Throttling escalation

`ErrThrottled` returns to the caller after the SDK retry budget is exhausted. M5 does not add adapter-level rate limiting beyond what the SDK provides; observed throttle behavior informs M9/M10 tuning.

## Conformance & testing

### In-tree tests (no credentials needed)

`internal/storage/s3compat/s3compat_test.go` covers:

- URL parser (`s3://...`, `r2://...`, malformed inputs).
- Config validation.
- Prefix application/stripping (round-trip + edge cases).
- Error classification (table-driven against fabricated smithy/HTTP errors).
- Retry-config wiring (asserts 412 not retryable, asserts `MaxAttempts` honored).

`cmd/bucketvcs/store_test.go` grows cases for `s3://` and `r2://` URL parsing.

### Live conformance — `s3compat_conformance_test.go`

Mirrors `localfs/localfs_conformance_test.go`. Each provider is its own top-level test that skips when its env is unset.

```go
func TestConformance_R2(t *testing.T) {
    if os.Getenv("BUCKETVCS_R2_BUCKET") == "" {
        t.Skip("R2 conformance: set BUCKETVCS_R2_BUCKET, BUCKETVCS_R2_ENDPOINT, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY")
    }
    conformance.Run(t, r2Factory(t))
}

func TestConformance_S3(t *testing.T) {
    if os.Getenv("BUCKETVCS_S3_BUCKET") == "" {
        t.Skip("S3 conformance: set BUCKETVCS_S3_BUCKET, BUCKETVCS_S3_REGION, AWS credentials")
    }
    conformance.Run(t, s3Factory(t))
}
```

**Per-test isolation**: each `Factory(t)` call generates a unique prefix (`conformance/<ulid>/`). Cleanup lists under the prefix and deletes all keys + aborts any open multipart uploads.

### CI

A new `conformance-cloud` job runs:

- On every merge to `main`.
- On PRs that touch `internal/storage/s3compat/**`, `internal/storage/conformance/**`, `internal/storage/*.go`, or `cmd/bucketvcs/store*.go`.
- Skipped on `pull_request` events from forks (no secret exposure).

Repository secrets: `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET`; `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `AWS_S3_BUCKET`. Set as job env.

Stress sub-suite skipped under `-short`; the cloud job runs without `-short`. Runtime budget: < 5 minutes per provider. Cost: a few cents per run (R2 has free egress; S3 stress reads are minimal).

### Stress-test sanity

`100 concurrent manifest CAS` and `10000 small object creates` are the most likely to surface provider quirks. Tests retry on `ErrThrottled` with backoff but **not** on `ErrVersionMismatch` (CAS contention is the point of the test).

### Diff-harness integration

M2's diff harness gains a parameterized run pointed at an `s3compat` store backed by R2 (CI-only). It runs in the same `conformance-cloud` job after the conformance suite passes. This is the M5 thesis test: does a real cloud bucket actually serve git correctly under the existing harness?

## Risks

| # | Risk | Mitigation |
|---|---|---|
| R1 | `If-None-Match: *` semantics for `CompleteMultipartUpload` are subtler than for `PutObject`. | Conformance #8 tests this directly; `classify` treats 200-with-different-ETag-than-expected as success and lets §43.6 GC handle orphan parts. Documented in `s3compat/README.md`. |
| R2 | R2 occasionally returns generic `InternalError` where S3 returns specific codes. | Run conformance against R2 specifically before promoting; `ErrTransient` is the default fallthrough so misclassification fails caller-visible-but-retryable. |
| R3 | `aws-sdk-go-v2` adds ~40 transitive deps. | Pinned via go.sum; dependabot/renovate alerts; monthly `go list -m -u all` review. Acceptable cost vs. hand-rolling SigV4. |
| R4 | Per-test-prefix cleanup can leak storage if cleanup itself fails. | ULID-prefixed test runs + a small `bucketvcs storage gc-conformance --older-than=24h` helper script run weekly via cron. |
| R5 | The 5–10 MiB band sits between single-PUT and 2-part multipart and can spike object-op cost if mishandled. | Adapter chooses single-PUT for objects ≤ `UploadPartSize` (8 MiB); callers wanting multipart explicitly use the multipart API. |
| R6 | Retry on a `PutObject` whose response was lost can succeed twice with different ETags. | Retries only happen before any caller-visible state exists. Conformance #14 explicitly tests this; SDK's idempotent retry mode is enabled. |

## Decisions captured

- First canonical cloud backend = **Cloudflare R2** (per §27); AWS S3 is exercised in M5 CI but formally promoted at M7.
- One package, two schemes: `internal/storage/s3compat` serves both `s3://` and `r2://`.
- Credentials: env + standard AWS chain; never URL-embedded.
- Adapter applies optional key prefix; no other code path knows about it.
- Capabilities advertise real provider limits (5 MiB / 10 000 parts / 5 TiB / strong list / signed URLs).
- Conformance gates on real R2 + real S3 in PR CI; fork PRs skip.
- No localfs→cloud migration tool in M5.

## Ship gate

M5 is done when:

1. In-tree tests pass.
2. Conformance suite passes against R2 in CI.
3. Conformance suite passes against S3 in CI.
4. M2 diff-harness passes against R2.
5. `bucketvcs init`, `inspect-manifest`, `import`, `export`, `serve` all work end-to-end against `s3://` and `r2://` URLs with real Git clients (using M4 token auth).

## Memory updates after merge

After M5 merges, update `MEMORY.md` with an `m5_progress.md` entry following the M0–M4 pattern (commit, tag `m5-complete`, one-line summary).

## Open questions resolved by this spec

- §44.6 (first hosted backend choice) → **Cloudflare R2**, with AWS S3 as M7's separately-shipped canonical backend.

## Open questions deferred

- §44.4 (how much serving path is pure Go in v1) — continues to be answered iteratively via the diff harness; M5 does not change the answer.
- §44.13 (default GC retention window) — answered at M8.

## Next step

After user approval of this spec, invoke the `superpowers:writing-plans` skill to produce an implementation plan. No code is written until the plan is approved.
