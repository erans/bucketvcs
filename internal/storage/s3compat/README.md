# s3compat — S3-compatible storage adapter

Implements `internal/storage.ObjectStore` against any S3-compatible
object store via `aws-sdk-go-v2`. M5 ships this adapter as the
canonical Cloudflare R2 backend; AWS S3 is exercised in conformance
testing and is formally promoted to canonical at M7.

## CLI usage

```text
--store=s3://<bucket>[/<prefix>]
--store=r2://<bucket>[/<prefix>]
```

Required env vars (R2):

```text
BUCKETVCS_R2_ENDPOINT          https://<account>.r2.cloudflarestorage.com
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
```

R2 defaults Region to "auto" and ForcePathStyle to true. The endpoint
is required.

Required env vars (S3):

```text
AWS_REGION                     e.g. us-east-1 (or BUCKETVCS_S3_REGION)
AWS_ACCESS_KEY_ID              or set AWS_PROFILE
AWS_SECRET_ACCESS_KEY
```

If you use a shared-config profile (`AWS_PROFILE`), the profile's
region is honored automatically.

## Provider quirks

- Cloudflare R2 occasionally returns generic `InternalError` where AWS
  S3 returns specific codes. The adapter classifies these as
  `ErrTransient` so callers retry.
- `CompleteMultipartUpload` with `If-None-Match: *` is the only way to
  guarantee no silent overwrite during multipart completion. The
  adapter does not abort on conflict; orphan parts are reclaimed by
  M8 GC.
- `DeleteObject` with `If-Match` is not reliably enforced by S3 on
  absent keys (S3 treats DELETE as idempotent). The adapter Heads the
  object first, then deletes with If-Match for race safety.
- Single PUT vs multipart: objects ≤ 8 MiB go via single PUT;
  multipart is invoked explicitly via `CreateMultipart`.

## Conformance

Run `scripts/conformance-cloud.sh` from the repo root. The script
skips providers whose env is unset.

## Ship-gate (M5)

1. `go test ./internal/storage/s3compat/...` (in-tree, no creds).
2. `scripts/conformance-cloud.sh` (R2 + S3 conformance).
3. `BUCKETVCS_DIFFHARNESS_STORE=r2://...` `go test ./internal/diffharness/...`.

## Architecture

- `s3compat.go` — type S3Compat, Capabilities(), interface assertion.
- `config.go` / `url.go` — Config struct, Validate, applyDefaults, ParseURL.
- `open.go` — SDK client construction with profile-aware region resolution.
- `keys.go` — validateKey, validateListPrefix.
- `prefix.go` — applyPrefix / stripPrefix / normalizePrefix.
- `errs.go` — classify(op, err) → storage sentinel.
- `retry.go` — SDK standard retryer (412 not retried).
- `get.go` — Get / Head / GetRange.
- `put.go` — PutIfAbsent / PutIfVersionMatches / materializeForRetry / matchesAdapterShape.
- `delete.go` — DeleteIfVersionMatches (Head-then-Delete).
- `list.go` — List with prefix translation, delimiter, pagination.
- `multipart.go` — Create / UploadPart / Abort / CompleteMultipartIfAbsent (mutex-serialized terminal ops).
- `signed.go` — SignedGetURL via SDK presigner.
- `cleanup.go` — AbortMultipartsUnderPrefix (test/repair helper).
