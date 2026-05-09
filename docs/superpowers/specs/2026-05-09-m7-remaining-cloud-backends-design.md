# M7 — Remaining Canonical Cloud Backends (GCS + Azure Blob + S3 Promotion)

Date: 2026-05-09
Status: design draft
Scope: bucketvcs OSS-core milestone M7 only
Source decomposition: `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md`
Source spec sections: §11.1, §12.1, §12.2, §12.4, §29

## What this milestone delivers

Two new native cloud-storage adapters and the formal promotion of the existing AWS S3 path:

- **`internal/storage/gcs`** — Google Cloud Storage adapter built on `cloud.google.com/go/storage`. Passes the M0 conformance suite against real GCS and against `fake-gcs-server`.
- **`internal/storage/azureblob`** — Azure Blob Storage adapter built on `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob`. Passes conformance against real Azure Blob and against Azurite.
- **AWS S3 promotion to canonical** — `internal/storage/s3compat` (M5) gains a real-AWS leg in the nightly conformance run and a required MinIO PR gate. Documentation drops the "M7 promotion in progress" qualifier. No s3compat code changes.

After M7, `bucketvcs init`, `inspect-manifest`, `import`, `export`, and `serve` all work end-to-end with `--store=gcs://<bucket>[/<prefix>]` and `--store=azureblob://<container>[/<prefix>]`. All four canonical backends listed in §11.1 (S3, GCS, R2, Azure) are conformance-gated in CI.

## What this milestone does not deliver

- **§11.2 deployment-tested candidates** (Tigris, MinIO AIStor) — out of scope.
- **§11.3 compatibility-tested S3-compatible backends** (Wasabi, B2, Ceph, etc.) — operators can try them; not gated by upstream CI.
- **Worker bindings for R2** — same M5 deferral.
- **Cross-backend migration tooling** — repo bytes are bit-identical across adapters; manual `gsutil cp` / `azcopy` / `aws s3 sync` works. Tooling lives at M16 if it is still needed.
- **Per-repo cost dashboards, multi-region replication, CDN wiring, BYOB UX** — hosted/post-MVP concerns.
- **Backend selection per-repo at runtime** — one `--store` per deployment, unchanged from M5.
- **Refactor extracting shared cloud-adapter scaffolding** — kept self-contained; the real overlap across the three SDK-backed adapters is too small to justify the coupling.
- **`bucketvcs store check` smoke-test subcommand** — nice-to-have, not blocking; out of scope.

## Architecture

### Package layout

```
internal/storage/
  s3compat/        (M5 — unchanged)
  gcs/             (NEW)
    gcs.go              // type GCS; var _ storage.ObjectStore = (*GCS)(nil)
    config.go           // Config struct, validation
    open.go             // Open(ctx, Config) (*GCS, error)
    get.go head.go range.go
    put.go              // PutIfAbsent (ifGenerationMatch=0), PutIfVersionMatches (ifGenerationMatch=N)
    delete.go list.go
    multipart.go        // resumable upload session per MultipartUpload
    signed.go           // SignedGetURL via storage.SignedURL
    errs.go             // classify(err) -> storage sentinel
    retry.go            // gax retry policy
    prefix.go keys.go
    gcs_test.go
    gcs_conformance_test.go    // skipped without env

  azureblob/       (NEW)
    azureblob.go        // type AzureBlob; var _ storage.ObjectStore = (*AzureBlob)(nil)
    config.go open.go
    get.go head.go range.go
    put.go              // PutIfAbsent (If-None-Match: "*"), PutIfVersionMatches (If-Match: <etag>)
    delete.go list.go
    multipart.go        // StageBlock per part; CommitBlockList on complete
    signed.go           // SignedGetURL via service SAS
    errs.go retry.go prefix.go keys.go
    azureblob_test.go
    azureblob_conformance_test.go

cmd/bucketvcs/store.go   (modified — adds gcs:// and azureblob:// cases)
docs/m7-cloud-quickstart.md   (NEW — mirrors m5-cloud-quickstart.md)
```

### Boundary

The rest of the codebase (M1 `internal/repo`, M2 pack writer, M3 gateway, M4 auth, M6 sshd) sees only `storage.ObjectStore`. No file outside `internal/storage/gcs/` imports `cloud.google.com/go/storage`; no file outside `internal/storage/azureblob/` imports `azblob`/`azidentity`. Mirrors the s3compat boundary established in M5.

### Code-sharing decision

Each adapter is self-contained. Prefix/key normalization, retry shape, and error classification are copied (and adapted) from `s3compat` rather than extracted into a shared package. The actual overlap is ~50 LoC of prefix handling per adapter; SDK-specific error and retry shapes diverge sharply enough that a shared base would either be a thin pass-through or a leaky abstraction. Revisit only if a fourth canonical adapter lands.

## GCS adapter

### Provider mapping (§12.2)

| `ObjectStore` method | GCS implementation |
|---|---|
| `PutIfAbsent` | `Writer` with `Conditions{DoesNotExist: true}` (`ifGenerationMatch=0`); 412 → `ErrAlreadyExists` |
| `PutIfVersionMatches(expected)` | `Writer` with `Conditions{GenerationMatch: parseGen(expected)}`; 412 → `ErrVersionMismatch` |
| `DeleteIfVersionMatches` | `ObjectHandle.If(Conditions{GenerationMatch}).Delete`; 412 → `ErrVersionMismatch`; 404 → `ErrNotFound` |
| `Get` / `Head` | `NewReader` / `Attrs` |
| `GetRange(start,end)` | `NewRangeReader(start, end-start+1)` |
| `List(prefix, opts)` | `Bucket.Objects(ctx, &Query{Prefix, Delimiter, StartOffset})`; pagination via pager token round-trip |
| `CreateMultipart` | Open a resumable upload session; `MultipartUpload` carries the session URI + target key |
| `MultipartUpload.UploadPart` | Stream-write to the resumable session; part numbers determine ordering |
| `CompleteMultipartIfAbsent` | Final `Writer.Close()` carries `Conditions{DoesNotExist: true}`; 412 → `ErrAlreadyExists` |
| Multipart abort | `CancelUpload` on the resumable session |
| `SignedGetURL` | `bucket.SignedURL(key, &SignedURLOptions{Method:"GET", Expires:…})` |

### Version token format

Decimal generation as a string (e.g. `"1709512345678901"`). `ObjectVersion` remains opaque to callers. `parseGen` rejects non-numeric tokens with `ErrInvalidArgument`.

### Multipart shape

GCS has no S3-style "stage parts then assemble" API. `MultipartUpload` is modeled as a single resumable-upload session per upload (one session URI, monotonically appended). Part numbers determine write order; the §29 #8 "multipart cannot overwrite" invariant holds because the finalize PUT carries `ifGenerationMatch=0`. Aborting calls `CancelUpload`. The conformance suite passes unchanged because it only relies on the `MultipartUpload` interface contract, not on a specific underlying mechanism.

### Credentials

Application Default Credentials (`google.FindDefaultCredentials`) by default — env vars (`GOOGLE_APPLICATION_CREDENTIALS`), workload identity, GCE/GKE metadata. `Config.CredentialsJSON` (raw bytes) and `Config.CredentialsFile` (path) override. `Config.Endpoint` overrides the default GCS endpoint and is the hook used by `fake-gcs-server` in CI.

### Config

```go
package gcs

type Config struct {
    Bucket          string        // required
    Prefix          string        // optional; trailing "/" normalized
    Endpoint        string        // optional; default GCS; CI sets fake-gcs URL
    CredentialsJSON []byte        // optional
    CredentialsFile string        // optional
    UserProject     string        // optional; for requester-pays buckets

    UploadChunkSize    int           // default 8 MiB
    MaxRetries         int           // default 5
    RequestTimeout     time.Duration // default 60s
    PresignDefaultTTL  time.Duration // default 15 min
}
```

## Azure Blob adapter

### Provider mapping (§12.4)

| `ObjectStore` method | Azure implementation |
|---|---|
| `PutIfAbsent` | `BlockBlobClient.Upload(body, …{IfNoneMatch: ETagAny})`; 409/412 → `ErrAlreadyExists` |
| `PutIfVersionMatches(expected)` | `Upload` with `IfMatch: parseETag(expected)`; 412 → `ErrVersionMismatch` |
| `DeleteIfVersionMatches` | `BlockBlobClient.Delete` with `IfMatch`; 412 → `ErrVersionMismatch`; 404 → `ErrNotFound` |
| `Get` / `Head` | `BlockBlobClient.DownloadStream` / `GetProperties` |
| `GetRange(start,end)` | `DownloadStream(ctx, &DownloadStreamOptions{Range: HTTPRange{Offset:start, Count:end-start+1}})` |
| `List(prefix, opts)` | `ContainerClient.NewListBlobsFlatPager(…{Prefix})` (or hierarchical for delimiter); continuation-token round-trip |
| `CreateMultipart` | Mint a `MultipartUpload` carrying target key + upload-scoped GUID for block IDs; no Azure call needed (blocks are staged lazily) |
| `MultipartUpload.UploadPart(n, body)` | `BlockBlobClient.StageBlock(blockID(n), body)` where `blockID(n) = base64(guid + ":" + zeroPad(n))` |
| `CompleteMultipartIfAbsent` | `BlockBlobClient.CommitBlockList(blockIDs, …{IfNoneMatch: ETagAny})`; 409/412 → `ErrAlreadyExists` |
| Multipart abort | No-op at upload-time; uncommitted blocks are GC'd by Azure after 7 days. Defensive `BlockBlobClient.Delete` with `If-Match` only when abort follows a partial commit. |
| `SignedGetURL` | `BlockBlobClient.GetSASURL(BlobSASPermissions{Read:true}, expiry, nil)` — requires Shared Key credential; without one, returns `ErrNotSupported`. |

### Version token format

Raw ETag string with quotes stripped (e.g. `"0x8DBF1234ABCD"`). Round-tripped opaquely to/from the SDK.

### Block-blob choice

We use block blobs (not append blobs or page blobs). Block blobs are the blob type whose staged-block / commit-list flow maps onto our `MultipartUpload` shape: `StageBlock` per part, `CommitBlockList` as the all-or-nothing finalize, with `If-None-Match: *` on the commit giving us the §29 #8 invariant. Append blobs are append-only with no commit step; page blobs target random-access workloads (VHDs) and assume per-page updates — neither fits a content-addressed pack-file workload. Block ID layout is `base64(guid + ":" + zeroPad(partNumber))` — Azure requires fixed-length block IDs within a single commit, and the per-upload GUID prevents collisions across concurrent uploads to the same target key.

### Credentials

`azidentity.NewDefaultAzureCredential` by default (env, workload identity, managed identity, az CLI). `Config.AccountKey` activates Shared Key auth (necessary for SAS issuance). `Config.ConnectionString` is honored as a convenience for local Azurite and takes precedence when set.

### Config

```go
package azureblob

type Config struct {
    Account          string        // required (e.g. "myaccount") if no conn string
    Container        string        // required
    Prefix           string        // optional
    ServiceURL       string        // optional override; default "https://{Account}.blob.core.windows.net"; Azurite uses this
    AccountKey       string        // optional Shared Key (enables SAS)
    ConnectionString string        // optional; takes precedence; primarily for Azurite

    UploadBlockSize    int64         // default 8 MiB
    MaxRetries         int           // default 5
    RequestTimeout     time.Duration // default 60s
    PresignDefaultTTL  time.Duration // default 15 min
}
```

## CLI integration

`cmd/bucketvcs/store.go` adds two scheme cases and removes the existing M7-reservation error path.

```go
case "gcs":
    cfg := gcs.Config{
        Bucket:          bucket,
        Prefix:          prefix,
        Endpoint:        os.Getenv("BUCKETVCS_GCS_ENDPOINT"),
        CredentialsFile: os.Getenv("BUCKETVCS_GCS_CREDENTIALS_FILE"),
        UserProject:     os.Getenv("BUCKETVCS_GCS_USER_PROJECT"),
    }
    return gcs.Open(ctx, cfg)

case "azureblob":
    cfg := azureblob.Config{
        Account:          os.Getenv("BUCKETVCS_AZURE_ACCOUNT"),
        Container:        bucket,
        Prefix:           prefix,
        ServiceURL:       os.Getenv("BUCKETVCS_AZURE_SERVICE_URL"),
        AccountKey:       os.Getenv("BUCKETVCS_AZURE_ACCOUNT_KEY"),
        ConnectionString: os.Getenv("BUCKETVCS_AZURE_CONNECTION_STRING"),
    }
    return azureblob.Open(ctx, cfg)
```

### URL parsing

- `gcs://my-bucket` → `Bucket=my-bucket`, `Prefix=""`
- `gcs://my-bucket/repos/staging/` → `Bucket=my-bucket`, `Prefix=repos/staging/` (trailing slash normalized)
- `azureblob://my-container/repos` → `Container=my-container`, `Prefix=repos/`

### Secrets policy

Unchanged from M5: never URL-embedded, never in `os.Args`. `Open` fails if any secret-bearing field is parsed out of the URL.

### Removed code

The `case "gcs", "azureblob"` reservation block in `store.go` and its associated comment ("reserved; cloud adapter for this provider lands at M7") are removed.

## Conformance and CI

### Test wiring

Each adapter ships `_conformance_test.go` mirroring `s3compat_conformance_test.go`:

```go
func TestGCSConformance(t *testing.T) {
    cfg, ok := loadGCSConfigFromEnv(t)
    if !ok { t.Skip("BUCKETVCS_GCS_TEST_BUCKET unset") }
    conformance.Run(t, conformance.Factory{
        New: func(t testing.TB) storage.ObjectStore {
            s, err := Open(context.Background(), cfg)
            // ...unique per-test prefix, register cleanup
        },
    })
}
```

Same shape for `azureblob_conformance_test.go`. Live tests are gated by env vars and skipped otherwise — same M0 contract.

### Local-emulator profile (PR-blocking)

A new `docker-compose.cloud.yml` (alongside `docker-compose.minio.yml`) brings up MinIO, `fake-gcs-server`, and Azurite. A new `scripts/conformance-emulators.sh` boots the stack, exports the matching env vars, creates buckets/containers, and runs `go test ./internal/storage/...`.

```yaml
services:
  minio:
    image: minio/minio
    ...
  fake-gcs:
    image: fsouza/fake-gcs-server
    command: ["-scheme", "http", "-public-host", "fake-gcs:4443"]
    ports: ["4443:4443"]
  azurite:
    image: mcr.microsoft.com/azure-storage/azurite
    command: ["azurite-blob", "--blobHost", "0.0.0.0"]
    ports: ["10000:10000"]
```

### CI matrix (`.github/workflows/conformance.yml`, new)

| Job | Trigger | Backends | Env source |
|---|---|---|---|
| `emulators` | every PR + push | localfs, MinIO (s3), fake-gcs, Azurite | docker compose |
| `real-cloud` | nightly cron + manual `workflow_dispatch` + `release` tag | AWS S3, R2, real GCS, real Azure Blob | repo secrets |

The `emulators` job is a **required** check on PRs. The `real-cloud` job runs on a schedule and posts results to a tracked issue; failures block tagging `m7-complete` (and any future release tag).

### Documented conformance gaps (skips, not bypasses)

- `fake-gcs-server` does not implement signed URLs. The conformance suite probes `SignedGetURL` once during setup; if the call returns `ErrNotSupported` (or `Capabilities().SignedURLs == false`), `§29 #10 SignedURL` is skipped with a `t.Logf`. The skip is therefore data-driven — real GCS exercises the full case, fake-gcs is automatically skipped.
- Azurite supports SAS, but only when initialized with an account key (the default Azurite container ships with a well-known dev key). If the live config has no `AccountKey` and no `ConnectionString` carrying one, the same skip path triggers.
- Both skips emit a single `t.Logf` line so they are visible in CI output and cannot be silently lost.

## AWS S3 promotion to canonical

No new code in `s3compat`. The promotion is operational + documentation:

1. **CI:** the MinIO leg of `s3compat` conformance, optional in M5, becomes a required PR check via the `emulators` job. A real-AWS leg joins the nightly `real-cloud` job using a dedicated test bucket + IAM user with conformance-prefix-only permissions.
2. **Docs:**
   - `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md` — §11.1 row marks `s3://` canonical.
   - `docs/m5-cloud-quickstart.md` — S3 examples lose the "M7 promotion in progress" qualifier and move into their own section.
   - `README.md` — lists `s3://` alongside `r2://`, `gcs://`, `azureblob://` under canonical backends.
   - `internal/storage/s3compat/README.md` — drops "promoted to canonical at M7" wording.
3. **Memory:** the line "AWS S3 (M7 promotion in progress)" in `m5_progress.md` is updated once M7 ships.

If a divergence surfaces during the AWS nightly run, it is filed as a bug, not bundled into M7.

## Documentation deliverables

- `docs/m7-cloud-quickstart.md` — mirrors `m5-cloud-quickstart.md`. One section per provider with `--store=` URL, env vars, IAM/role example, smoke test (`bucketvcs init` → `import` → native `git clone`/`push`).
- `docs/superpowers/specs/2026-05-09-m7-remaining-cloud-backends-design.md` — this design.
- `docs/superpowers/specs/m7_progress.md` — written at merge, like prior milestones.
- `internal/storage/gcs/README.md` and `internal/storage/azureblob/README.md` — short package-level docs (capability summary, env vars, "how to run live conformance").

## Branching and merge

Single `worktree-m7-cloud` branch. Tasks: GCS adapter, Azure adapter, AWS S3 promotion (CI + docs), CLI wiring, quickstart doc, CI workflow, emulator compose file. Single merge into `main` with tag `m7-complete`. Matches the M5/M6 pattern.

## Acceptance criteria

For M7 to be done:

1. `internal/storage/gcs` passes the full conformance suite against real GCS and against `fake-gcs-server` (with documented signed-URL skip).
2. `internal/storage/azureblob` passes the full conformance suite against real Azure Blob and against Azurite (with documented SAS skip when no key).
3. `internal/storage/s3compat` passes the full conformance suite against real AWS S3 in addition to existing R2 and MinIO runs.
4. `bucketvcs init --store=gcs://…`, `--store=azureblob://…`, and `--store=s3://…` all work end-to-end with `import` → `serve` → native `git clone`/`push`, including M4 token auth and M6 SSH key auth.
5. The `emulators` CI job is a required PR check; the `real-cloud` nightly is green for the seven days preceding the `m7-complete` tag.
6. No file outside `internal/storage/{gcs,azureblob,s3compat}/` imports a provider SDK.

## Risk register

- **Real-cloud secret rotation in CI** — credentials live as repo secrets; rotation is a manual ops task. Document in `m7-cloud-quickstart.md` how to rotate without a CI-secret leak.
- **fake-gcs-server / Azurite drift from real services** — emulators historically lag conditional-write semantics. Mitigated by the nightly real-cloud run; any divergence shows up within 24h.
- **Azure block-ID collision under concurrent multipart uploads to the same key** — addressed by the per-upload GUID prefix; covered by an explicit conformance test (`MultipartConcurrentComplete` already exercises the case).
- **GCS resumable session staleness** — sessions expire after one week; out of scope for M7 since uploads complete in seconds. If long-lived uploads ever become a thing (M9+ acceleration), revisit.
- **Adding three SDK dep trees inflates `go.sum`** — accepted cost; deps are isolated to leaf packages and do not leak into the core types.
