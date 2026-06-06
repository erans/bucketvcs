# Git LFS Operator Guide

This guide is for operators who deploy, tune, monitor, and roll back Git
LFS support in production. It covers the LFS production-readiness surface,
the per-repo storage layout, the LFS-relevant `bucketvcs serve` flags, the
per-backend transfer-mode matrix, three minimum operator setup recipes, the
signed-URL TTL rule against GC retention, the verify-failure forensic
procedure, the complete observability surface (10 metrics + 7 audit events),
operations runbooks (signing-key rotation, emergency disable, manual
cleanup), the deferred-work tracker, and an FAQ for the common stock
`git-lfs` operator questions. Stock `git-lfs ≥ 3.0` clients push and pull
unchanged against a bucketvcs gateway over both HTTPS and SSH.

---

## Production readiness

| Concern | Status | Notes |
|---|---|---|
| HTTPS LFS push / pull | ✅ shipped | direct signed URL on S3/R2/GCS/Azure, gateway-proxied URL on localfs |
| SSH `git-lfs-authenticate` | ✅ shipped | Basic-auth bearer in the response header |
| Locks API | ✅ implemented | Stock git-lfs lock/unlock/locks/locks --verify work against this server |
| Multipart upload | ❌ deferred | Single PUT only; proxied-path cap 5 GiB (5×2³⁰ bytes), direct-path cap is the backend's single-PUT limit (5 GB / 5×10⁹ bytes on S3/R2 — slightly under the proxied cap) |
| LFS GC | ✅ implemented | `bucketvcs gc --lfs` walks reachable Git blobs and sweeps unreferenced LFS objects past retention |
| Per-tenant byte quotas | ✅ implemented | `bucketvcs quota` CLI; LFS-only; per-tenant hard cap enforced at the Batch handler |
| LFS-aware bandwidth metering | ❌ deferred | Get byte usage from S3 access logs / GCS audit logs / Azure storage analytics |
| LFS-specific token scopes | ❌ deferred | Today every write token can push LFS; every read token can pull LFS |

The remaining deferred items are tracked in §8 with the trigger condition each
operator should watch for. The verify-token mechanism is described in §5.4; the
Locks API in §8.1.

---

## 1. Overview

Git LFS (Large File Storage) stores large blobs out-of-band from the Git
object graph. The client replaces a tracked file with a small pointer blob in
Git; the actual bytes live in an LFS object store keyed by the SHA-256 of the
content. The LFS server protocol is implemented so that stock `git-lfs`
clients push and pull large objects against a bucketvcs gateway with no
client-side configuration beyond the usual `git lfs track "*.bin"`.

Two transfer modes carry LFS object bytes through the system:

- **Direct mode** — used on every cloud backend (S3, R2, GCS, Azure Blob).
  The gateway mints a backend-native signed URL (S3-style presigned PUT/GET,
  GCS V4 signature, Azure SAS) and returns it in the Batch response. The
  client uploads or downloads bytes straight to the bucket. The gateway sees
  the Batch request and the post-upload verify, but never sees the bytes.
- **Proxied mode** — used on the localfs backend. Localfs has no native
  signed-URL primitive, so the gateway mints an HMAC-signed URL of the form
  `https://<gw>/_lfs/<tenant>/<repo>/<oid>?token=…` and proxies the PUT/GET
  through the gateway process to the local filesystem.

The selection is automatic: backend capability determines the path. There is
no `--lfs-mode` flag and no per-repo override. Cloud backends always present
direct URLs; localfs always presents proxied URLs. See §3.2 for the matrix.

LFS objects are written under the same per-repo storage prefix that holds
the repository's pack data. No separate bucket, no separate prefix root.
Objects content-addressed by their LFS pointer SHA-256 deduplicate within a
single repo; cross-repo dedupe remains deferred — see §8.4 "Deferred work" for details.

---

## 2. Storage layout

Each repo's LFS area lives at:

```
tenants/<tenant>/repos/<repo>/lfs/objects/<sha256>
```

`<sha256>` is the 64-character lowercase hex OID the LFS client computes from
the file contents. The OID appears verbatim in the storage key — there is no
`<aa>/<bb>/...` sharding, no size suffix, no metadata file alongside the
object. Listing the prefix `tenants/<tenant>/repos/<repo>/lfs/objects/`
returns one entry per LFS object in the repo.

The flat layout is intentional. LFS clients address objects by full OID; they
never list. Deep prefixing buys nothing for content-addressed key access and
complicates the listing path used by operator-side audits and the
LFS GC walk. The trade-off versus a `<aa>/<bb>/<rest>` 2/2 sharded layout —
the convention some filesystems use to bound directory entry counts — is that
on filesystems with hard caps on entries per directory (ext2/ext3 with the
default 32 000-entry limit, network filesystems with their own limits), a
flat layout can exhaust the directory at high object counts. In production,
LFS runs against cloud object stores (S3, R2, GCS, Azure Blob) where listing
is paginated and there is no directory-entry cap. Localfs is intended for
development; ext4 (the modern Linux default) has no per-directory limit, so
the flat layout is safe in practice.

See spec §4 for the rationale chain and the per-backend single-PUT size
limits that the flat layout interacts with.

---

## 3. Configuration

### 3.1 CLI flags

The five LFS-relevant `bucketvcs serve` flags:

| Flag | Default | Purpose |
|---|---|---|
| `--lfs` | `true` | Enable the LFS Batch API. LFS routes (`/info/lfs/objects/batch` for Batch; `/_lfs/<tenant>/<repo>/<oid>` for proxied PUT/GET/POST, where POST is the verify endpoint) are mounted only when this is true. Hard-requires `--proxied-url-signing-key` + `--proxied-url-base`; set to `false` to make the gateway return 404 on every LFS route — see §7.2. |
| `--lfs-presign-ttl` | `15m` | TTL for LFS upload/download presigned URLs (direct mode) and HMAC-signed proxied URLs (localfs). The Batch response's `expires_at` field is set from `now + this`. Clients refresh by re-running Batch. |
| `--lfs-ssh-token-ttl` | `15m` | TTL for the bearer token issued via SSH `git-lfs-authenticate`. The client uses that bearer to drive the HTTPS Batch API and signed-URL transfers — once it expires, the client re-runs the SSH authenticate command. |
| `--proxied-url-signing-key` | (empty) | Path to a file holding an HMAC key (≥ 16 bytes) used to sign proxied `/_lfs/` URLs. Required when the store is localfs and `--lfs=true`. Shared with bundle/pack proxied URLs. |
| `--proxied-url-base` | (empty) | External base URL of this gateway, e.g. `https://gw.example`. Required for SSH `git-lfs-authenticate` (no inbound HTTP request to derive the host from) and for proxied LFS URLs on localfs. HTTPS Batch on cloud backends works without it. |
| `--max-body-bytes` | `1073741824` (1 GiB) | Global HTTP body cap. Applies to every gateway HTTP path including the LFS proxied `/_lfs/` PUT — to allow LFS objects above 1 GiB on the proxied path, raise this to at least the largest expected single-object size (proxied path has a separate 5 GiB hard cap; see §8.2). Direct-path LFS PUTs do NOT pass through the gateway and ignore this flag. |

All defaults match `cmd/bucketvcs/serve.go`. The retention flag
referenced by §4 is `bucketvcs gc --retention` (default `168h` / 7 days);
the same flag governs `bucketvcs maintenance` indirectly through the GC it
schedules.

### 3.2 Per-backend support matrix

| Backend | Direct presign | Proxied URL | Mode used by default |
|---|---|---|---|
| S3 / R2 (`s3compat`) | ✅ via `SignedPutURL` / `SignedGetURL` (S3 presign V4) | (not used) | direct |
| GCS (`gcs`) | ✅ via `SignedPutURL` / `SignedGetURL` (V4 signature) | (not used) | direct |
| Azure Blob (`azureblob`) | ✅ via `SignedPutURL` / `SignedGetURL` (SAS) | (not used) | direct |
| localfs | ❌ `SignedPutURL` / `SignedGetURL` return `storage.ErrNotSupported` | ✅ HMAC URL of the form `/_lfs/<tenant>/<repo>/<oid>?token=…` | proxied |

`internal/lfs/batch.go` chooses the path: it calls `PresignPut` /
`PresignGet` first; on `storage.ErrNotSupported` it falls back to
`Store.ProxiedPutURL` / `Store.ProxiedGetURL`, which mint an HMAC URL signed
with the `--proxied-url-signing-key` keyed against the `--proxied-url-base`
host. The gateway's `/_lfs/<tenant>/<repo>/<oid>` route validates the token
on each request and streams bytes through to (or from) localfs.

The three cloud backends bypass the gateway entirely on the transfer hop —
client bytes flow client → bucket — so the gateway's CPU and bandwidth
budget do not scale with LFS traffic on cloud deployments.

### 3.3 Minimum operator setup recipes

#### (a) S3 / R2 production, HTTPS only

```bash
bucketvcs serve \
  --addr :443 \
  --store "s3://my-bucket?endpoint=https://account.r2.cloudflarestorage.com&region=auto" \
  --lfs \
  --lfs-presign-ttl 15m \
  --proxied-url-signing-key /etc/bucketvcs/proxied.key \
  --proxied-url-base "https://gw.example.com"
```

Both `--proxied-url-signing-key` and `--proxied-url-base` are
**required** whenever `--lfs=true`, even on cloud backends, because the
verify action mints an HMAC-signed kind=5 token regardless of which
backend serves the upload (see §5.4). Upload/download URLs remain
direct-presigned by S3/R2; only the verify URL is gateway-proxied.
Starting `bucketvcs serve` with `--lfs=true` and either flag missing
exits with code 2 and a diagnostic.

#### (b) S3 / R2 production + SSH

```bash
bucketvcs serve \
  --addr :443 \
  --ssh-addr :2222 \
  --ssh-host-key /etc/bucketvcs/ssh_host_ed25519_key \
  --store "s3://my-bucket?endpoint=https://account.r2.cloudflarestorage.com&region=auto" \
  --lfs \
  --lfs-presign-ttl 15m \
  --lfs-ssh-token-ttl 15m \
  --proxied-url-signing-key /etc/bucketvcs/proxied.key \
  --proxied-url-base "https://gw.example.com"
```

Both `--proxied-url-signing-key` and `--proxied-url-base` are required:
the signing key signs verify tokens (kind=5) and
bundle/pack-URI tokens (kind=1/2); the base URL is the external gateway
host used as the verify URL prefix and as the SSH `git-lfs-authenticate`
Href. LFS upload/download URLs remain S3-presigned (direct mode).

#### (c) Localfs development

```bash
bucketvcs serve \
  --addr 127.0.0.1:8080 \
  --store "localfs:/var/lib/bucketvcs" \
  --lfs \
  --proxied-url-signing-key /etc/bucketvcs/proxied.key \
  --proxied-url-base "http://127.0.0.1:8080"
```

Localfs means proxied URLs. `--proxied-url-signing-key` is required (the
gateway must HMAC-sign each `/_lfs/` URL); `--proxied-url-base` is required
so the URL has the right host. Generate the signing key with
`openssl rand -hex 16 > /etc/bucketvcs/proxied.key`. The key file is shared
with bundle / pack proxied URLs — if you already run proxied mode,
the existing file is reused.

---

## 4. Signed-URL TTL rule

### 4.1 The hard rule

The bundle/pack URL retention rule applies to LFS in the same form:

```
TTL ≤ retention / 24
```

Where `TTL` is `--lfs-presign-ttl` (and `--lfs-ssh-token-ttl`, on the SSH
authentication path) and `retention` is `bucketvcs gc --retention`. This is
an operational rule, not a CLI-enforced check — the binary will run if you
violate it, but a long-lived URL minted at the start of a TTL window may
reference an LFS object that gets swept by GC before the client downloads
it. The client then sees a 404 (direct mode) or 500 (proxied mode) when
finally pulling. Treat the rule as a hard pre-deploy lint.

The 24× safety factor (the same as the bundle/pack TTL rule) accommodates GC scheduling
jitter and the §43.6-style race window described in the GC operator guide.
A URL minted right before a GC mark — but downloaded right after the
following sweep — must remain valid; the 24× headroom is what makes that
hold against the worst-case GC timing.

LFS GC is active (§8.3). Reachability-based mark-and-sweep of
unreferenced LFS objects after retention elapses runs, so the
24× headroom rule is load-bearing for LFS too: a signed-URL TTL ≥
24× the GC cadence ensures URLs minted just before a mark remain
valid through the following sweep. Operators who configure TTLs in
respect of this rule do not need to revisit it.

### 4.2 Relevant flags

LFS TTL flags:

- `--lfs-presign-ttl` — default `15m`. Maximum lifetime of a minted LFS
  upload or download URL (direct on cloud, proxied on localfs).
- `--lfs-ssh-token-ttl` — default `15m`. Maximum lifetime of the bearer
  token issued via SSH `git-lfs-authenticate`. The client uses that bearer
  for the HTTPS Batch API; once it expires, `git-lfs` re-invokes the SSH
  authenticate command to mint a new one.

Retention flag:

- `bucketvcs gc --retention` — default `168h` (7 days).

### 4.3 Recommended values

```
lfs-presign-ttl     ≤ retention / 24    → 15m ≤ 168h/24=7h    ✓ OK at defaults
lfs-ssh-token-ttl   ≤ retention / 24    → 15m ≤ 168h/24=7h    ✓ OK at defaults
```

The default presign and SSH-token TTLs of 15 minutes sit 28× under the
7-hour threshold implied by `retention / 24` (7h ÷ 15m = 28), so the
defaults satisfy the hard rule with room to spare. They are deliberately
conservative — 15 minutes is comfortably long for a single
batch + upload sequence on typical LFS workloads (a few hundred MB) and
short enough that an interrupted client always re-runs Batch rather than
holding on to a near-expired URL.

If you decrease `--retention` from the 168h default, reduce both TTLs
proportionally so that `TTL ≤ retention/24`. For example, with
`--retention 48h`:

```
--lfs-presign-ttl 2h --lfs-ssh-token-ttl 2h
```

(Since `48h / 24 = 2h`.)

If you increase TTL — for example to 1 hour to give clients more headroom
on a slow network — check that retention is at least 24× the new value:
`1h × 24 = 24h`, well below the 168h default, so a TTL bump alone needs no
retention change. Tune retention upward, not TTL downward — TTL governs
client-side request windows and the worst-case bytes-in-flight; bumping it
up trades clock-skew tolerance for a smaller retention safety margin.

The bundle/pack proxied-URL TTL flags (`--proxied-url-bundle-ttl`,
`--proxied-url-pack-ttl`) are independent of the LFS TTLs and obey the
24× rule in their own right. LFS reuses the bundle/pack signing-key file for
`/_lfs/` URLs but uses a separate TTL knob — there is no shared TTL
constraint between LFS and bundle/pack.

---

## 5. Verify failure forensics

### 5.1 The verify endpoint shape

After every successful LFS upload PUT, the stock `git-lfs` client POSTs
to the verify URL returned in the Batch response's `verify` action:

```
POST /_lfs/<tenant>/<repo>/<oid>?token=<base64url-kind5-token>
Content-Type: application/vnd.git-lfs+json
Authorization: Bearer bvtv_<token>

{"oid": "<sha256>", "size": <bytes>}
```

The URL is identical to the upload PUT / download GET URL — the HTTP
method selects the verify branch on the proxied handler. The handler
validates the `?token=` query parameter (HMAC-signed kind=5
`lfs-verify`, bound to `<tenant>/<repo>/<oid>`, TTL =
`--lfs-presign-ttl`), decodes the body (cap: 64 KiB), confirms the
body's `oid` matches the URL's `<oid>` (422 on mismatch), and calls
`Verify(store, oid, size)`, which `Head`s the LFS object in storage:

| HTTP status | Trigger | Audit `result` | Metric `result` |
|---|---|---|---|
| `200 OK` | Object exists at the claimed size | `ok` | `ok` |
| `403 Forbidden` | Token missing / invalid / expired / wrong kind / hash mismatch | — (no `lfs.verify` event) | — (counted on `lfs_object_token_invalid_total{reason}`) |
| `404 Not Found` | Object is absent from storage (`ErrVerifyNotFound`) | `missing` | `missing` |
| `422 Unprocessable Entity` | Object present, size differs (`ErrVerifySizeMismatch`); or malformed body / oid mismatch | `size_mismatch` (size mismatch) or `error` (decode / oid mismatch) | same |
| `500 Internal Server Error` | Backend `Head` returned a non-not-found error | `error` | `error` |

Authentication is by the HMAC token alone — there is no inbound
`Authorization` check on the verify POST. The kind=5 token authorizes
exactly verify on exactly one (tenant, repo, oid).
Source: `internal/lfs/proxied.go` `serveVerify` and
`internal/lfs/verify.go` `Verify`.

### 5.2 What a verify failure means

The four `result` label values surface distinct operational conditions:

| `result` | Operational meaning | Typical cause | Action |
|---|---|---|---|
| `ok` | Normal success | Client uploaded fully; bytes are durable in storage | None |
| `missing` | Object never landed in storage | Client PUT 4xx'd / 5xx'd silently, network drop mid-PUT, client retried Batch without re-uploading | Investigate client logs; on direct cloud mode, check S3 / GCS access logs for the corresponding PUT |
| `size_mismatch` | Object present but bytes != claimed | Truncated upload that the backend still finalized (rare on S3 single-PUT but possible on multi-step paths), or client lied about size in the Batch request | Force re-upload from client; if persistent, suspect a client-side LFS clean filter bug |
| `error` | Backend transient failure or malformed request | Cloud backend 5xx during `Head`; oid-in-body / oid-in-URL mismatch (client bug) | Retry-able if transient; otherwise inspect serve.log for the underlying error line |

`missing` is the most operationally significant: it means the gateway told
the client "your bytes are here" via Batch, the client believed it, and now
nothing is there. The repo is now in a state where Git history references an
LFS pointer whose payload does not exist in the bucket. Subsequent
`git lfs pull` against that ref will fail per-object.

`size_mismatch` is rare in practice (cloud backends reject content-length
mismatches at PUT time). When it does fire it is almost always a client-side
clean-filter bug; treat the client install as suspect.

### 5.3 Forensic procedure

LFS observability is slog text-format log lines. The recipes below assume
`serve.log` captures the stdout/stderr of `bucketvcs serve`.

**Step 1: enumerate recent verify failures.**

```bash
grep 'event=lfs.verify' serve.log | grep -v 'result=ok'
```

Each line carries `repo=<tenant>/<repo> user=<actor> oid=<sha256>
size=<claimed> result=<label>`. Inventory the failing OIDs.

**Step 2: cross-reference the proxied PUT (localfs only).**

For each failing OID, check whether the proxied PUT reached the gateway:

```bash
grep 'event=lfs.object.served' serve.log | grep 'op=upload' | grep "<oid>"
```

This audit fires ONLY on the localfs / proxied path — the gateway handler
`internal/lfs/proxied.go` records it after the `/_lfs/` PUT completes. If
the line is missing, the client never even tried the PUT (Batch likely
returned an empty `actions` map because the OID was thought to be present
at the matching size — see FAQ §9.1). If the line is present with
`status=200`, the bytes landed in the storage adapter but the subsequent
`Head` did not see them — investigate the localfs path or any intervening
filesystem layer.

**Step 3: cross-reference the direct PUT (cloud backends).**

On cloud backends the PUT goes client → bucket and bypasses the gateway
entirely, so there is no `lfs.object.served` audit. Look at the backend's
own access logs:

- S3 / R2: enable S3 server access logging on the bucket; filter for
  `REST.PUT.OBJECT` on the key `tenants/<tenant>/repos/<repo>/lfs/objects/<oid>`.
- GCS: enable Cloud Storage audit logs (data access); filter for the same
  key.
- Azure Blob: enable storage analytics logging; filter for `PutBlob` on
  the corresponding blob path.

Absence of the PUT in the backend log means the client's transfer failed
silently — typically a TLS or DNS issue on the client side, or a presigned
URL that expired before the PUT completed.

**Step 4: decide retry vs escalate.**

- One-off `missing` on a known transient client (laptop on a flaky network):
  ask the client to `git lfs push --all` and watch for the same OID. The
  client repository already has the blob; re-running the push is cheap.
- Sustained `missing` from one client: suspect a client-side LFS clean
  filter or `.gitattributes` bug — the pointer was committed but the
  large-object byte stream was never produced.
- Sustained `missing` across clients: backend ingress problem (regional
  outage, CORS, expired bucket creds). Drop `--lfs=false` per §7.2 until
  the upstream is restored.

### 5.4 The verify-token mechanism

The verify action carries an HMAC-signed short-TTL token
of kind=5 (`lfs-verify`) — not an echo of the inbound Batch request's
`Authorization` header. The token is bound to (tenant, repo, oid) and
expires after `--lfs-presign-ttl` (default 15m). The token is not
consume-on-use: within its TTL a client may replay it against the same
OID (each replay re-runs the backend `Head` and re-emits the audit
event), but it cannot be repurposed for upload, download, Batch, or
verify on a different OID/repo/tenant. The Batch response's verify
action carries the token in both the URL `?token=` query parameter and
an `Authorization: Bearer bvtv_<token>` header; the `git-lfs` client
replays both on the verify POST, and the gateway validates the URL
token (the header is decorative — the `bvtv_` prefix distinguishes
verify tokens from session tokens `bvts_` in forensics).

This closes the response-body credential leak that an
`Authorization`-echo mechanism would expose:

- **Client-side persistence.** `git-lfs` caches the verify action's
  `Authorization` value on disk. The cached value is a
  15-minute kind=5 token scoped to one OID, not a long-lived user
  credential.
- **Response-body log exposure.** Any access log / reverse proxy / WAF
  that captures Batch response bodies now sees only a short-TTL
  per-OID verify token, not a replayable user credential. The kind=5
  token is single-purpose: it authorizes verify on exactly one (tenant,
  repo, oid) and cannot be used for upload, download, or Batch.

**Residual risk to be aware of.** A token recovered from a captured
response body remains replayable against the same OID for the rest of
its TTL (up to `--lfs-presign-ttl`). Each replay triggers one backend
`Head` and one `lfs.verify` audit event, but does NOT leak object
bytes — verify never reads object contents. Operators who cannot
disable response-body logging on the path between the gateway and the
public internet should still treat the Batch response as
moderately-sensitive (short-TTL, narrow-scope), even though the prior
long-lived-credential exposure is gone.

No operator action is required to enable this — the mechanism is on
whenever `--lfs=true`, which now also hard-requires
`--proxied-url-signing-key` and `--proxied-url-base` (see §3.3 recipe
(a)). Starting `bucketvcs serve --lfs=true` with either flag missing
exits with code 2.

---

## 6. Observability reference

### 6.1 Metrics

All LFS metrics are emitted as slog text-format `metric` records with
`metric_name=<name> value=<int>` plus label key/value pairs. Below are
the ten LFS metrics with valid label values and emission sites (six
core LFS metrics plus four for the Locks API):

#### `lfs_batch_requests_total{op,result}`

One record per Batch request.
- `op`: `upload`, `download`, or `unknown` (when the body failed to
  decode before `req.Operation` could be read).
- `result`:
  - `ok` — 200 returned, request processed.
  - `unauthorized` — 401 (anonymous upload).
  - `forbidden` — 403 (actor lacks write).
  - `notfound` — 404 (repo not found during the write recheck).
  - `too_large` — 413 (Batch body exceeded the 1 MiB cap).
  - `error` — any other 4xx / 5xx, including 422 on malformed body.

Site: `internal/lfs/handler.go` `handleBatch`.

#### `lfs_batch_objects_total{op,result}`

One record per object in a successful Batch response (not emitted on
request-level failures).
- `op`: `upload` or `download`.
- `result`:
  - `new` — upload that produced an upload action (object was missing).
  - `exists` — upload returned an empty `actions` map (already present at
    matching size) OR download returned a download action.
  - `missing` — download for an absent object (per-object 404).
  - `error` — per-object error (size mismatch on upload, presign failure,
    head error).

Site: `internal/lfs/handler.go` `handleBatch` calling `perObjectResult`
once per response object.

#### `lfs_object_served_total{op,result}`

One record per proxied `/_lfs/<tenant>/<repo>/<oid>` PUT or GET. Emitted
ONLY on the localfs / proxied path; cloud backends bypass the gateway and
do not produce this metric.
- `op`: `upload` (PUT) or `download` (GET).
- `result`:
  - `ok` — transfer completed, 200 returned.
  - `exists` — PUT short-circuit because the object was already present
    at the matching size (idempotent re-upload).
  - `missing` — GET for an object not present (404).
  - `too_large` — PUT body exceeded the operator's `--max-body-bytes`.
  - `hash_mismatch` — PUT body's computed SHA-256 differed from the
    URL's OID (client integrity bug or hostile client). **Operators
    should alert on this** — it is never expected in normal operation.
  - `error` — any other PUT / GET failure.

Site: `internal/lfs/proxied.go` `servePut` and `serveGet`.

#### `lfs_object_token_invalid_total{reason}`

One record per `/_lfs/` request rejected because the proxied URL token
was missing or invalid. Fires on PUT (upload), GET (download), and
POST (verify) — the token-validation prologue is shared.
- `reason`: `missing` (no `?token=…` query parameter), `invalid` (token
  decode / HMAC verify failed), `expired` (token past its expiry), or
  `kind_mismatch` (token minted for `lfs-get` used on a PUT, or a
  non-`lfs-verify` token used on POST, etc.).

Site: `internal/lfs/proxied.go` request prologue. Alert on sustained
non-zero `expired` or `kind_mismatch` — they indicate clients holding
stale URLs (set TTL too short) or active enumeration / replay attempts.

#### `lfs_verify_requests_total{result}`

One record per verify request. No `op` label — verify is operation-less.
- `result`: `ok`, `missing`, `size_mismatch`, `error`. See §5.2 for
  operational meaning of each label.

Token-validation failures (missing / expired / wrong kind / hash
mismatch) are NOT counted here — they are counted on
`lfs_object_token_invalid_total{reason}` together with the equivalent
PUT/GET failures, because the token-validation prologue is shared.
`lfs_object_token_invalid_total` carries a `reason` label but no `op`
label, so verify token failures cannot be isolated from upload/download
token failures at the metric level today. Operators should treat the
counter as an aggregate proxied-LFS token-health signal; a per-`op`
breakdown is tracked as future work.

Site: `internal/lfs/proxied.go` `serveVerify`.

#### `lfs_ssh_authenticate_total{op,result}`

One record per SSH `git-lfs-authenticate` exec command.
- `op`: `upload` or `download` (the LFS op the client claimed in the
  `git-lfs-authenticate <repo> <op>` argument).
- `result`:
  - `ok` — token minted, response written, client acknowledged.
  - `forbidden` — actor lacks the required permission (`download`
    requires read, `upload` requires write).
  - `disabled` — server started with `--lfs=false`.
  - `anon` — anonymous SSH session reached the dispatcher (deploy keys
    too, since deploy actors cannot mint LFS bearers — see the SSH
    `git-lfs-authenticate` scope decision).
  - `error` — token mint failed, IO failed, or other internal error.
  - `client_disconnected` — token was minted and written to the wire
    but the client dropped before the gateway saw the close — useful for
    detecting flaky SSH transports, see EmitSSHAuthenticateMetric godoc.

Site: `internal/sshd/session.go` `handleLFSAuthenticate`.

#### `lfs_locks_created_total{outcome}`

One record per `POST /info/lfs/locks` request.
- `outcome`:
  - `created` — 201 lock created successfully.
  - `conflict` — 409 path already locked by another owner.
  - `error` — any other failure (401 / 400 / 503 / 500).

Site: `internal/lfs/locks_handler.go` `handleLocksCreate`.

#### `lfs_locks_listed_total{outcome}`

One record per `GET /info/lfs/locks` request.
- `outcome`:
  - `success` — 200 list returned normally.
  - `error` — any failure (401 / 400 / 503 / 500).

Site: `internal/lfs/locks_handler.go` `handleLocksList`.

#### `lfs_locks_verified_total{outcome}`

One record per `POST /info/lfs/locks/verify` request.
- `outcome`:
  - `success` — 200 partitioned ours/theirs returned normally.
  - `error` — any failure (401 / 400 / 503 / 500).

Site: `internal/lfs/locks_handler.go` `handleLocksVerify`.

#### `lfs_locks_deleted_total{force,outcome}`

One record per `POST /info/lfs/locks/<id>/unlock` request.
- `force`: `true` or `false` — whether the caller set `force=true`.
- `outcome`:
  - `owner` — 200 caller is the lock owner.
  - `forced` — 200 non-owner caller used `force=true`.
  - `denied` — 403 non-owner caller did not pass `force=true`.
  - `not_found` — 404 lock ID does not exist.
  - `error` — any other failure (401 / 400 / 503 / 500).

Operators should alert on a sustained non-zero `forced` rate — that
indicates non-owner force-unlocks are happening in volume and may
warrant social escalation. Site: `internal/lfs/locks_handler.go`
`handleLocksUnlock`.

#### `lfs_gc_objects_marked_total{outcome}`

One record per `RunMark` call, emitted after the mark record is built.
- `outcome`:
  - `candidate` — count of orphan LFS objects recorded in the mark.

Site: `internal/lfs/gc/gc.go` `RunMark`.

#### `lfs_gc_objects_swept_total{outcome}`

Four records per `RunSweep` call (one per outcome bucket, including
zero counts so dashboards can graph deltas reliably).
- `outcome`:
  - `deleted` — object removed from storage (or counted as such in dry-run).
  - `skipped_retention` — candidate still inside the retention window.
  - `skipped_concurrent` — Head/Delete race; will be retried next sweep.
  - `error` — per-object delete failure (logged + counted in the report).

`skipped_concurrent` is broken out from `skipped_retention` on purpose:
the two have different operational implications. Concurrent races
resolve on the next sweep; retention skips wait for the wall clock.
Operators alerting on "why aren't reclaims happening?" can pivot on
the bucket.

**Note on the `deleted` bucket:** the count also includes objects
that disappeared from storage between mark and sweep (e.g., another
GC run on the same mark, an out-of-band cleanup, or a backend
lifecycle rule). When two sweeps targeting the same mark race, both
will increment `deleted` for any already-gone object, inflating
`lfs_gc_bytes_swept_total` over the true reclaimed-bytes figure for
that mark. Use the audit-event `deleted_bytes` value cross-referenced
across `sweep_id`s for accurate per-sweep accounting, or treat
`lfs_gc_bytes_swept_total` as an upper bound when concurrent sweeps
are possible.

Site: `internal/lfs/gc/gc.go` `RunSweep`.

#### `lfs_gc_bytes_swept_total`

One record per `RunSweep` call. Value is the total bytes the sweep
reclaimed (or would have reclaimed, in dry-run). Operators tracking
storage-cost recovery should chart this against
`lfs_object_served_total{op=upload}` byte deltas for trend analysis.

Site: `internal/lfs/gc/gc.go` `RunSweep`.

#### `lfs_quota_check_total{outcome}`

One record per Batch upload pre-check.
- `outcome`:
  - `ok` — batch fit the tenant's quota (or no quota row exists).
  - `exceeded` — batch was rejected; every Upload object returned a 507.

Site: `internal/lfs/handler.go::handleBatch`.

#### `lfs_quota_bytes_used{tenant}`

Gauge of the current `used_bytes` for the named tenant. Refreshed
on every Add (verify success), Subtract (GC sweep success), Set,
Clear, and Reconcile. Operators graph this directly; alert when
`value / limit > 0.9` (soft-cap proxy).

Sites: `internal/lfs/proxied.go::serveVerify`, `internal/lfs/gc/gc.go::RunSweep`,
`cmd/bucketvcs/quota.go::runQuota{Set,Clear,Reconcile}`.

### 6.2 Audit events

Eleven LFS audit events (four core LFS events, three
for the Locks API, two for LFS GC, two
for quotas). All use the
flat-attribute slog shape — each event has a
top-level `event=<name>` attr plus event-specific attrs. The audit
stream is the same stdout/stderr stream that carries metrics.

> **Durable shipping.** The `serve`-emitted events (`lfs.batch`,
> `lfs.object.served`, `lfs.verify`, the `lfs.lock.*` events, `lfs.quota.exceeded`)
> are **shipped** to `sys/logs/activity/` by default — see
> [log shipping](log-shipping.md) and the [observability overview](observability.md).
> The CLI-emitted events `lfs.gc.mark` / `lfs.gc.sweep` (`bucketvcs gc --lfs`)
> and `lfs.quota.reconcile` (`bucketvcs quota reconcile`) run outside `serve` and
> are **not** shipped — they reach stderr only ([log shipping §1.1](log-shipping.md#11-the-two-streams)).

#### `event=lfs.batch`

Emitted at the end of every Batch request that reached the write-check
stage (so: not emitted on malformed-body 422s, where the audit shape
would carry sentinel data).

Attrs: `repo=<tenant>/<repo>`, `user=<actor or empty>`, `op=upload|download`,
`n_objects=<int>` (count in the response), and `result=<label>` where
label is one of the post-parse `lfs_batch_requests_total` results
(`ok`, `unauthorized`, `forbidden`, `notfound`, or `error` after a
successful parse). NOTE: the pre-parse `error` and `too_large` results
on the metric are NOT mirrored here — when the request body is
malformed the audit shape would carry sentinel data, so emission is
skipped (the metric still fires). When correlating an alert on
`lfs_batch_requests_total` to the audit stream, expect a gap for those
two label values.

Site: `internal/lfs/audit.go` `emitLFSBatch`, called from `handleBatch`.

#### `event=lfs.object.served`

Emitted at the end of every `/_lfs/` PUT or GET. Localfs / proxied only.

Attrs: `op=upload|download`, `hash=<tenant>/<repo>/<oid>` (the token's
hash field), `bytes=<int64>` (input bytes on PUT, output bytes on GET),
`status=<HTTP status>`.

Site: `internal/lfs/audit.go` `emitLFSObjectServed`, called from
`internal/lfs/proxied.go`.

#### `event=lfs.verify`

Emitted at the end of every verify request, regardless of outcome.

Attrs: `repo=<tenant>/<repo>`, `user=""` (always empty —
verify is authenticated by the kind=5 HMAC token bound to (tenant,
repo, oid), not a session, so there is no actor to record), `oid=<sha256>`,
`size=<claimed size>`, `result=ok|missing|size_mismatch|error`.

Site: `internal/lfs/audit.go` `emitLFSVerify`, called from
`internal/lfs/proxied.go` `serveVerify` (POST branch).

#### `event=lfs.ssh_authenticate`

Emitted at the end of every SSH `git-lfs-authenticate` exec command.

Attrs: `repo=<tenant>/<repo>`, `user=<actor name or empty>`,
`op=upload|download`, `ttl_seconds=<int64>` (0 on disabled / forbidden /
anon paths; the configured TTL otherwise), `result=ok|forbidden|disabled|
anon|error|client_disconnected`.

Site: `internal/lfs/audit.go` `EmitLFSSSHAuthenticate`, called from
`internal/sshd/session.go`.

#### `event=lfs.lock.create`

Emitted after a `POST /info/lfs/locks` request creates a lock (201).
Not emitted on conflict / unauthorized / error.

Attrs: `repo=<tenant>/<repo>`, `user=<actor name>`,
`owner_user_id=<user ID of the creator>`, `lock_id=<lock_…>`,
`path=<locked path>`, `ref_name=<ref name or empty>` — `ref_name` is the
optional repo-ref the lock was scoped to (empty for repo-wide locks).
The explicit `owner_user_id` field lets audit consumers pivot on user
IDs without a name-join.

Site: `internal/lfs/audit.go` `emitLFSLockCreate`, called from
`internal/lfs/locks_handler.go` `handleLocksCreate`.

#### `event=lfs.lock.delete`

Emitted after a `POST /info/lfs/locks/<id>/unlock` request deletes a
lock (200). Not emitted on 403 / 404 / other-error paths.

Attrs: `repo=<tenant>/<repo>`, `user=<actor name>`, `lock_id=<lock_…>`,
`force=<true|false>` (whether the caller passed `force=true`),
`force_target_user_id=<owner user ID when force-deleting another user's
lock; empty when the caller is the owner>`. Operators looking for
audit traces of force-unlocks should grep for non-empty
`force_target_user_id`.

Site: `internal/lfs/audit.go` `emitLFSLockDelete`, called from
`internal/lfs/locks_handler.go` `handleLocksUnlock`.

#### `event=lfs.lock.verify`

Emitted after a `POST /info/lfs/locks/verify` request completes (200).
Not emitted on unauthorized / error.

Attrs: `repo=<tenant>/<repo>`, `user=<actor name>`,
`ours_count=<int>` (locks owned by the caller),
`theirs_count=<int>` (locks owned by others).

Site: `internal/lfs/audit.go` `emitLFSLockVerify`, called from
`internal/lfs/locks_handler.go` `handleLocksVerify`.

#### `event=lfs.gc.mark`

Emitted after `RunMark` finishes one mark pass. One event per repo
per CLI invocation.

Attrs: `repo=<tenant>/<repo>`, `mark_id=<lfs-...>`,
`candidates_count=<int>` (orphan LFS objects recorded),
`manifest_version=<uint64>` (manifest version observed at the start
of the mark phase), `dry_run=<bool>`.

The `dry_run=true` variant signals that the mark record was NOT
persisted to storage (e.g. `--mark-only --dry-run`). Audit-log
consumers should not conclude a mark exists on disk in that case;
running `--sweep-only` afterwards will fail with `ErrNoMarks`.

Site: `internal/lfs/audit.go` `EmitLFSGCMark`, called from
`internal/lfs/gc/gc.go` `RunMark`.

#### `event=lfs.gc.sweep`

Emitted after `RunSweep` finishes one sweep pass. One event per repo
per CLI invocation.

Attrs: `repo=<tenant>/<repo>`, `mark_id=<lfs-...>`,
`sweep_id=<lfs-sweep-...>`, `deleted_count=<int>`,
`deleted_bytes=<int64>`, `skipped_retention=<int>`,
`skipped_concurrent=<int>`, `error_count=<int>`, `dry_run=<bool>`.

The `dry_run=true` variant lets log consumers distinguish what a
real sweep would have done from what it actually did.

Site: `internal/lfs/audit.go` `EmitLFSGCSweep`, called from
`internal/lfs/gc/gc.go` `RunSweep`.

#### `event=lfs.quota.exceeded`

Emitted on every rejected Batch upload.

Attrs: `tenant=<t>`, `current_bytes=<int64>`, `limit_bytes=<int64>`,
`requested_bytes=<int64>` (sum of object sizes in the rejected batch),
`oids=<comma-separated>` (rejected OIDs).

Site: `internal/lfs/audit.go::EmitLFSQuotaExceeded`, called from
`internal/lfs/handler.go::handleBatch`.

#### `event=lfs.quota.reconcile`

Emitted once per `bucketvcs quota reconcile` invocation per tenant.

Attrs: `tenant=<t>`, `before_bytes=<int64>`, `after_bytes=<int64>`,
`drift_bytes=<int64-signed>` (positive: counter was under-reporting;
negative: over-reporting), `dry_run=<bool>`.

Site: `internal/lfs/audit.go::EmitLFSQuotaReconcile`, called from
`cmd/bucketvcs/quota.go::runQuotaReconcile`.

### 6.3 Recommended alerts

Three alerts capture the operationally significant LFS failure modes.
Phrase as `metric_name{label=value}` for clarity even though the
underlying stream is slog text-format, not Prometheus.

- **`lfs_verify_requests_total{result="missing"} > 0` within any rolling
  5-minute window.** Indicates client uploads dropping silently —
  clients believe their LFS bytes are durable, but the post-PUT verify
  cannot find them. Page the on-call. Run the §5.3 forensic procedure.
- **`lfs_batch_requests_total{result="error"}` sustained above background
  noise for 15+ minutes.** Indicates the backend presign primitive is
  failing (expired credentials, regional outage, IAM policy drift) and
  the gateway cannot mint upload / download URLs for clients. Drop
  `--lfs=false` per §7.2 while you fix the upstream, then restore.
- **`lfs_ssh_authenticate_total{result="client_disconnected"}` sustained
  at a non-zero rate.** Indicates SSH transport flakiness — tokens are
  being minted but never acknowledged by clients. Investigate the SSH
  layer (load balancer health, MTU issues, client OpenSSH version).
  Optional: alert on
  `lfs_object_served_total{result="hash_mismatch"} > 0` for localfs
  deployments — never expected in normal operation; indicates a
  misbehaving or hostile client.

The byte volumes pushed through LFS are intentionally NOT in the metric
stream — see spec §7. Use the backend's own access logs / billing
dashboards for byte-level visibility.

---

## 7. Operations runbook

### 7.1 Rotating the proxied-URL signing key

The proxied-URL signing key (file pointed at by `--proxied-url-signing-key`)
is HMAC material used to mint and verify the tokens on `/_lfs/`,
`/_bundle/`, and `/_pack/` URLs. Rotation is appropriate on operator
turnover, on a suspected key compromise, or as scheduled hygiene.

**Important: the current implementation supports ONE active key at a
time** — see `cmd/bucketvcs/serve.go` reading a single file into
`signingKey`, which is plumbed into `LFSProxiedURLSigningKey`,
`ProxiedURLSigningKey`, and the bundle/pack `URLBuilder.ProxiedKey` simultaneously.
There is no overlap window: a hard cutover means in-flight tokens minted
with the old key fail verification immediately after restart.

Procedure:

1. Generate the new key file alongside the current one:
   ```bash
   openssl rand -hex 16 > /etc/bucketvcs/proxied.key.new
   chmod 0400 /etc/bucketvcs/proxied.key.new
   ```
2. Replace atomically and restart:
   ```bash
   mv /etc/bucketvcs/proxied.key.new /etc/bucketvcs/proxied.key
   systemctl restart bucketvcs-serve   # or your supervisor's restart verb
   ```
3. Expect clients with in-flight Batch responses minted under the old
   key to see HTTP 403 with `reason=invalid` on their next `/_lfs/` PUT
   or GET. Stock `git-lfs` retries by re-running Batch with a fresh
   token; users see at most one transient transfer failure and an
   automatic recovery.

**Note on token expiry vs key rotation.** Tokens carry their own expiry
(`--lfs-presign-ttl`, `--proxied-url-bundle-ttl`, `--proxied-url-pack-ttl`).
A token signed under the old key fails the HMAC check first; the
expiry check never runs. There is no "in-flight tokens minted under the
old key remain valid until they expire" period — verification against a
single active key means rotation is an immediate cutover.

If you need a soft rotation (both old and new keys verifying for a
window), the workaround today is to schedule the rotation during a
maintenance window when active Batch responses are unlikely, and warn
users in advance. Multi-key verify is tracked as future work; until then,
plan rotations for low-traffic windows.

### 7.2 Disabling LFS in an emergency

Cases warranting an emergency disable:
- The backend presign primitive is broken and Batch is 5xx-ing across
  the board.
- A storage cost runaway from misbehaving clients.

Procedure:

1. Restart the gateway with `--lfs=false`:
   ```bash
   # Edit your systemd unit / k8s manifest / supervisor config to set
   # --lfs=false on the serve command, then:
   systemctl restart bucketvcs-serve
   ```
2. The gateway now returns 404 on every LFS route:
   `/info/lfs/objects/batch` and `/_lfs/<tenant>/<repo>/<oid>` (PUT
   upload, GET download, POST verify). The SSH `git-lfs-authenticate`
   exec command emits `result=disabled` on its audit event and returns
   a non-zero exit status; stock `git-lfs` clients surface this as a
   clear "LFS not available on the server" error.

What happens to:
- **Existing LFS objects in storage:** preserved. `--lfs=false` only
  toggles the gateway routes; it does not touch storage. Re-enabling
  later resurfaces every object that was there before the disable.
- **In-flight client uploads:** the client's PUT against the (already-
  minted) presigned URL still works on cloud backends — the URL is
  signed by the backend, not the gateway, so the gateway has no say in
  whether the byte transfer completes. But the subsequent verify call
  returns 404, the client treats the push as failed, and re-running it
  on the next push attempt hits the Batch 404.
- **In-flight client downloads:** symmetric. The bytes may transfer
  successfully but the Batch call that produced the URL now 404s on the
  next attempt, so `git lfs pull` cannot get past Batch.

To re-enable, restart with `--lfs=true` (the default). No state migration
or repo touch is needed.

### 7.3 Removing stale LFS objects

LFS-aware reachability GC is available (§8.3) — use
`bucketvcs gc --lfs` for routine orphan cleanup. The out-of-band
recipe below remains documented for one-off forensic cleanups (e.g.
auditing what GC would remove before flipping it on, or scrubbing
specific OIDs the GC retention window has not yet released).

For one-off manual cleanup — for example after a misbehaving client
uploaded test data that was never committed — operators can use an
out-of-band procedure:

1. **List every LFS object the gateway holds for the repo:**
   ```bash
   # S3 / R2 example. Adapt the prefix for your backend's tool.
   aws s3 ls \
     "s3://my-bucket/tenants/<tenant>/repos/<repo>/lfs/objects/" \
     | awk '{print $4}' | sort > /tmp/stored.txt
   ```
2. **List every OID referenced by any commit reachable from any ref:**
   ```bash
   # Run from a clean clone of the repo.
   git lfs fetch --all
   git lfs ls-files --all --long \
     | awk '{print $1}' | sort -u > /tmp/referenced.txt
   ```
3. **Compute the set difference:**
   ```bash
   comm -23 /tmp/stored.txt /tmp/referenced.txt > /tmp/orphaned.txt
   ```
4. **Delete only after reviewing the orphan list manually.** Each line
   is a 64-char OID. Be extra cautious if the repo has dangling commits
   or active pull requests that may reference LFS objects not yet
   reachable from any ref. Tag-based archival branches are a common
   source of OIDs that look orphaned but should not be deleted.

This procedure is out-of-band and unsupported by the gateway directly.
It is superseded by `bucketvcs gc --lfs` (§8.3), which
mark-and-sweeps unreferenced LFS objects with carry-forward retention
semantics. The manual procedure is retained here only for operators
running a binary without LFS GC or for one-off audits of LFS state against
external systems.

---

## 8. Deferred work

Items in this section are deliberately deferred. Each subsection below
records why the item is deferred, the operational trigger condition that
should prompt operators to escalate, and the workaround available today.
The production-readiness table in the preamble cross-references each
item by section number.

> **The verify-token mechanism is implemented — see §5.4. The Locks API
> is implemented — see §8.1.**

### 8.1 LFS Locking API

**Status.** Implemented. The four endpoints
(`POST /locks`, `GET /locks`, `POST /locks/verify`,
`POST /locks/:id/unlock`) are served by the gateway under
`{tenant}/{repo}.git/info/lfs/locks`. Stock `git-lfs` clients
(`git lfs lock`, `git lfs unlock`, `git lfs locks`, `git lfs locks --verify`)
work without configuration.

**Storage.** Lock records live in the `lfs_locks` table on the authdb
sqlite file (the one `--auth-db` points at). Whatever backs up authdb
backs up locks too. The schema migration is additive and applied on
first boot of a locks-aware binary; older binaries on the same authdb file are
unaffected (they don't see the new table).

**Auth.** Create + Unlock require `ActionWrite` on the repo. List +
Verify require `ActionRead`. The gateway enforces these via
`RoutedRequest.RequiredAction` before the handler runs.

**Force unlock.** `POST /locks/<id>/unlock {"force": true}` is allowed
for any caller with `ActionWrite` on the repo (per LFS spec). The
`lfs.lock.delete` audit event with non-empty `force_target_user_id`
flags non-owner forced unlocks so operators can trace those after the
fact.

**Ref scoping.** Locks with a `ref.name` field set filter into
verify/list responses when the request's ref matches OR when the lock
itself was created without a ref (repo-wide). Locks without ref.name
match every filter.

**Cursor scope.** List and Verify cursors are scoped to the originating
filter tuple. Clients that change a filter (path / id / refspec) between
paginated calls receive undefined results.

**Pagination cap.** List and Verify cursors are bounded — the server
stops emitting cursors past offset 10,000 (10× the maxLimit of 1000).
A repo with more locks than this can still be fully enumerated by
narrowing the filter (path/refspec); the server intentionally rejects
deep-offset enumeration to avoid quadratic scans. In practice no
realistic LFS workflow approaches this limit (typical repos have
<1000 locks total).

**Verify pagination.** /locks/verify shares a single NextCursor across
ours and theirs; callers must iterate until NextCursor is empty even
if `ours` is empty in a given page — more of the caller's locks may
exist at a deeper offset.

**Observability.** Four new metrics + three new audit events (see §6
tables for fields). Verify failures and 503 outcomes show up under
`outcome="error"` for the relevant `lfs_locks_*_total` counter; the
detailed error is in the structured log via `slog` at level Error.

**Deferred** (tracked in §11):
- TTL / auto-expiry on stale locks.
- SSH-native lock transport (today SSH redirects to HTTPS via the existing
  `lfs.IssueSSHToken` flow).
- Lock notifications / webhooks.
- Integration with protected branches (orthogonal: protected
  branches refuse pushes; locks block specific paths within an
  otherwise-pushable branch).

### 8.2 Multipart upload (custom transfer adapter)

**Status.** Not implemented. Every upload action returned by Batch is a
single PUT URL. There is no Range-aware upload, no `tus`-style resume,
and no custom `lfs.<server>.transfer` adapter advertised in the Batch
response.

**Why deferred.** Multipart upload requires a custom git-lfs transfer
adapter (per `git-lfs/docs/custom-transfers.md`) plus per-backend
upload-part / complete-upload plumbing across all four canonical
backends. The single-PUT path covers most real workloads up to the
per-backend single-PUT limit; multipart pays back only above that
boundary.

**Today's hard limits.**

- **Proxied path (localfs).** The `/_lfs/` PUT handler caps the body at
  `maxLFSObjectSize = 5 << 30` (5 GiB) — see the `maxLFSObjectSize`
  constant in `internal/lfs/proxied.go`. Bodies above that limit are
  rejected with 413 before any
  storage write. The `--max-body-bytes` `bucketvcs serve` flag (default
  1 GiB; the `maxBody` flag declared in `runServeWithListener` in
  `cmd/bucketvcs/serve.go`) further caps the body
  globally across all gateway HTTP paths — when operators expect LFS
  objects above 1 GiB they must raise `--max-body-bytes` to at least
  the largest expected single-object size. Above 5 GiB the proxied path
  rejects regardless of the `--max-body-bytes` setting.
- **Direct path (S3 / R2 / GCS / Azure).** The gateway never sees the
  bytes; the limit is whatever the backend's single-PUT endpoint
  accepts. For S3-compatible backends (including R2 and MinIO) this is
  **5 GB (5×10⁹ bytes, decimal)** — slightly under the proxied path's
  5 GiB (5×2³⁰ ≈ 5.37 GB) cap. For GCS the V4 single-PUT limit is 5 TiB
  but in practice Google recommends resumable uploads above ~5 MiB and
  resumable is unsupported here. For Azure Block Blob the single-PUT
  limit is 5000 MiB.

Treat **5 GB (decimal, 5×10⁹ bytes)** — the S3 single-PUT limit — as
the effective ceiling on every backend if you want a single
number that holds everywhere; the proxied-path 5 GiB cap is slightly
larger but cannot rescue a single PUT that exceeds S3's limit on the
direct path.

**Trigger condition.** Real workloads with single LFS objects > 5 GiB —
typically datasets, large container images stored as LFS, or pre-built
ML model artifacts. Smaller-but-flaky network conditions (where a
1 GiB PUT routinely fails midway) also reach the deferred resumable-
upload territory.

**Workaround today.** Sharding the asset client-side (split a 12 GiB
dataset into three 4 GiB shards, each tracked as a separate LFS object)
or storing the asset outside of Git LFS entirely (direct object-store
upload behind a shared URL).

### 8.3 LFS-aware GC

**Status.** Implemented. Use:

```
bucketvcs gc --store=URL --repo=tenant/repo --lfs [--retention=168h] [--dry-run]
```

Or to GC both Git objects and LFS objects in one invocation:

```
bucketvcs gc --store=URL --repo=tenant/repo --lfs --include-git-objects
```

**Discovery.** `RunMark` materializes the repo's mirror (reusing the
maintenance materialize path), walks every reachable Git blob via
`git rev-list --objects --all` + `git cat-file --batch`, filters to
blobs ≤1024 bytes, and extracts the referenced LFS OID from the
pointer signature. Live set = the union of referenced OIDs across all
reachable trees on all refs. The mark phase then lists the LFS storage
prefix and records every object NOT in the live set as a mark
candidate.

**Cost.** O(reachable Git blobs) per mark — bounded by the blob count
in the mirror, not the LFS object size. Materialize itself is bounded
by the pack count.

**Retention.** Default 7 days (mirrors the Git-objects GC). An LFS
object becomes deletable only after it has been marked unreferenced
for at least the retention window. `first_seen_unreferenced_at`
carries forward across mark runs, so the retention clock survives
re-runs and is consistent with the Git-objects GC semantics.

**Storage.** Mark records live at
`tenants/<tenant>/repos/<repo>/gc/lfs-marks/<id>.json`; sweep records
at `tenants/<tenant>/repos/<repo>/gc/lfs-sweeps/<id>.json`. These are
parallel to the existing `gc/marks/` and `gc/sweeps/` paths — the
two kinds of GC keep their records cleanly separated.

**Push-race fail-soft.** Because LFS objects are content-addressed
(sha256) and the Batch upload path is idempotent, a wrongly-deleted
object is transparently re-PUT by the next client push. The 7-day
retention default makes the race practically impossible in normal
operation; shorter retention is appropriate only when the re-upload
cost is acceptable.

**Failure modes.**
| Symptom | Cause | Action |
|---|---|---|
| `materialize: ...` error | Materialize failed (storage IO, git fsck) | Investigate logs; retry GC later |
| `rev-list failed` / `batch-check failed` | `git` binary error or corrupt mirror | Investigate; possibly re-mirror via `bucketvcs maintenance` |
| Per-object delete error (network, permissions, IAM) | Backend rejected the delete | Counted as `error` in the sweep report; sweep continues; investigate the underlying backend issue |
| Head/Delete race on a candidate | Object modified between the sweep's Head and Delete | Counted as `skipped_concurrent`; retried automatically on next sweep |
| `ErrNoMarks` from `--sweep-only` | No prior mark on disk | Run `--mark-only` (or omit `--sweep-only`) first |
| `retention_overridden_by_mark` warning | `--retention` flag on `--sweep-only` differs from the mark's pinned value | Mark's frozen retention wins; re-mark with the desired retention |

**Operator workflow.** Schedule via cron at the cadence that matches
your push volume. A typical OSS deployment:

```
# Daily LFS GC at 03:00 local time
0 3 * * * /usr/local/bin/bucketvcs gc --store=$STORE_URL --all-repos --lfs --retention=168h
```

**Observability.** 3 new counter metrics + 2 new audit events. See §6.

### 8.4 Per-tenant byte quotas

**Status.** Implemented. Use:

```
bucketvcs quota set       --auth-db=PATH --tenant=T --limit=100GiB
bucketvcs quota show      --auth-db=PATH {--tenant=T | --all}
bucketvcs quota reconcile --auth-db=PATH --store=URL {--tenant=T | --all-tenants} [--dry-run]
bucketvcs quota clear     --auth-db=PATH --tenant=T
```

**Schema.** New `quotas` table on the authdb (migration 0004):
columns `tenant PRIMARY KEY`, `limit_bytes`, `used_bytes`, `updated_at`.

**Enforcement.** The Batch upload handler runs one atomic SQL read
against the quotas row before issuing upload URLs. If the sum of
requested object sizes plus `used_bytes` exceeds `limit_bytes`, every
object in the response gets a 507 ObjectError with a `tenant quota
exceeded` message; the client surfaces it through stock `git lfs push`.

**Counter accounting.**
- **Increment at verify-success** — `internal/lfs/proxied.go::serveVerify`
  calls `quota.Add(ctx, tenant, oid, size)` after the verify check
  passes. An LRU dedupe ring (capacity 1024) makes the increment
  idempotent within a verify-token TTL window.
- **Decrement at GC sweep-success** — `internal/lfs/gc/gc.go::RunSweep`
  calls `quota.Subtract(ctx, tenant, oid, sizeBytes)` from every
  success path (normal delete, Head-said-gone, Delete-said-gone-by-race).
  Subtract is floored at zero via `MAX(used_bytes - ?, 0)` so reconcile
  -vs-sweep races never produce a negative counter.
- **Drift correction** — `bucketvcs quota reconcile` walks
  `tenants/<t>/repos/*/lfs/objects/` across every repo, sums object
  sizes, and overwrites `used_bytes`. `--dry-run` reports the drift
  without writing. Operators run this on a daily cron alongside
  `bucketvcs gc`.

**Race semantics.**
- **Within-batch**: atomic via a single SQL read per pre-check; no
  race possible inside one Batch request.
- **Cross-batch**: best-effort. Two concurrent batches at 99/100 GiB
  each pushing 2 GiB can both pass the pre-check and both verify-
  increment, leaving the counter at 103 GiB. The next batch from that
  tenant is correctly rejected (103 > 100). Bounded by
  `concurrent_batches × max_batch_size`. The §10 bootstrap step and
  the daily reconcile cron are the operator's resets.
- **Verify-replay**: the dedupe ring suppresses double-counts within
  its capacity; beyond eviction (which can happen under sustained
  load) double-count can occur and is caught by reconcile.
- **Backend errors during Add/Subtract**: logged at warn, do NOT fail
  the verify or sweep operation. The trade-off favors LFS availability
  over counter accuracy; reconcile is the safety net.

**Failure modes.**
| Symptom | Cause | Action |
|---|---|---|
| Batch returns 507 on every upload object | Tenant at or above quota | `quota show --tenant=<t>` to confirm; raise the limit or wait for GC |
| Tenant exceeds quota despite enforcement | Cross-batch race (above) | `quota reconcile --tenant=<t>` to update the counter; consider lowering the limit by `concurrent_batches × max_object_size` as a buffer |
| Reconcile shows persistent drift | Add/Subtract write failures | Check authdb error logs; reconcile is the safety net |
| Reconcile reports drift after a multi-sweep run | Two concurrent sweeps both decremented for the same OID's ErrNotFound path | Counter is floored at zero by the `MAX(used - ?, 0)` clamp; reconcile restores truth; consider gating sweep concurrency to one process per repo |
| `quota show` reports `over_by=<bytes>` | Limit was lowered below current usage, or drift caught up | New uploads correctly rejected until GC + reconcile drain usage |
| Verify response latency increases when quotas are enabled | Quota Add is a synchronous sqlite write after `WriteHeader(200)` | Acceptable: response body has already been committed; the latency floor is sqlite write latency. No mitigation needed under normal load. |

**Bootstrap (one-time after upgrade).** Fresh deployments
running against pre-existing LFS objects must seed counters once:

```
bucketvcs quota reconcile --auth-db=PATH --store=URL --all-tenants
```

Idempotent — re-running overwrites with the current listing sum.

**Observability.** 2 new metrics + 2 new audit events. See §6.1 / §6.2.

**Opt-out.** Operators who don't want quotas simply don't wire the
`Service` into `Deps.Quota` / `ProxiedDeps.Quota` / `SweepOptions.Quota`.
Every integration seam is then a no-op. The `quotas` table from
migration 0004 exists but stays empty.

**Deferred work (still tracked separately):**
- **Per-repo quotas** — second table with the same Service shape.
- **Git pack quotas** — receive-pack and maintenance hooks.
- **Reservation system** — eliminates the cross-batch race; not worth
  the state-machine cost for OSS.
- **Quota tiers / plans / chargeback** — control-plane feature.
- **Soft-cap mode** — operators alert on the gauge in their own
  monitoring stack.
- **Webhooks on threshold crossing** — webhooks themselves are deferred.
- **Cross-repo dedupe / shared-OID pooling** — pooling LFS objects by
  OID across an entire tenant maximizes storage savings but costs
  control-plane complexity for repo-scoped access decisions. M13
  intentionally chose per-repo isolation to keep the access model
  identical to the Git object path. Trigger: a tenant whose LFS
  storage cost is dominated by the same asset replicated across many
  forks of one repo.

---

## 9. FAQ

### 9.1 Why does `git lfs push` say "object exists, skipping" but the verify still fires?

Not a bug. The Batch handler `internal/lfs/batch.go` `processObject`
performs a `store.Head` for every requested OID on the upload path.
When the object exists and the stored size matches the client's
claimed size, the handler returns an `out` value with an empty
`Actions` map and no `Error`. The empty `Actions` map
tells `git-lfs` "this object is already present; do not PUT it".

However, the stock `git-lfs` client's protocol contract still POSTs to
the verify endpoint for every object in the response, regardless of
whether an upload action was present. That POST flows through the
normal verify path (§5.1) and the `lfs.verify` audit event fires with
`result=ok`. The `lfs_verify_requests_total{result="ok"}` metric also
increments.

The net effect: a re-push of an already-uploaded LFS object shows up in
the gateway as one Batch request + N verify requests but zero PUTs.
This is the intended behaviour and is the most common cause of "I
expected an `lfs.object.served` audit but there isn't one" reports on
the proxied path — there genuinely was no upload PUT.

### 9.2 Why isn't there a per-repo LFS toggle?

By design, per spec §8. LFS object storage costs the gateway nothing
until a client pushes an object. A repo that never sees `git lfs push`
has zero LFS rows in the bucket, no LFS metric records, no LFS audit
events, and no LFS-shaped HTTP traffic against its mount point. The
storage layout (§2) gives every repo its own `lfs/objects/` prefix and
the prefix simply stays empty.

Adding a per-repo opt-out would introduce a control-plane state bit
(per-repo "LFS enabled / disabled" flag) that must be queried on every
Batch request, must be migrated on every gateway upgrade, and must
gracefully reject existing LFS objects when flipped off. The cost is
non-trivial; the saving is zero because empty `lfs/objects/` prefixes
do not bill.

The only operator switch is the gateway-wide `--lfs` flag (§7.2). When
flipped to `false`, every Batch request returns 404 across every repo
on the gateway. Operators who need per-tenant gating typically run
multiple gateway processes — one with `--lfs=false` for tenants that
should not have LFS — rather than trying to gate inside a single
gateway.

### 9.3 Why does the SSH-authenticate response carry Basic auth, not Bearer?

A deliberate compatibility choice documented in the godoc on
`IssueSSHToken` in `internal/lfs/auth.go`. The
gateway's `RunAuth` path in `internal/gateway/auth.go` only parses
HTTP Basic credentials — it calls `r.BasicAuth()` and
dispatches to `auth.BasicPassword` against the token store. There is
no Bearer-token parser on the inbound side today.

`IssueSSHToken` therefore mints a short-TTL token, packs it into a
Basic header (`Authorization: Basic <base64(user:bvts_token)>`), and
returns that as the LFS response's `Header.Authorization` value. The
`git-lfs` client treats the response Authorization header opaquely:
whatever bytes the server returned, the client replays verbatim on the
subsequent HTTPS Batch / verify requests. Basic works at the wire level
without any client-side change, even though the credential is
conceptually a single-use token rather than a username/password pair.

The downstream `verifyBasicPassword` path in the gateway requires the
Basic username to match the user's `users.name` column. `IssueSSHToken`
threads `userName` explicitly through to the Basic encoding for exactly
this reason — see the `userName` validation in `IssueSSHToken` that
rejects `:` and control characters.

A future Bearer-capable inbound auth path would let the SSH-authenticate
response return `Authorization: Bearer bvts_…` directly, removing the
Basic wrapper. Until then, Basic-over-the-wire is the only shape the
gateway can accept.

### 9.4 Where do I find the byte-count of uploaded LFS objects?

Deliberately not surfaced in `lfs_*_total` metrics. See spec §7's note
on transfer-byte observability: in direct mode the gateway never sees
the bytes (the PUT goes client → bucket and bypasses the gateway
entirely), and in proxied mode the gateway's byte count would be a
local-only signal that disagrees with the bucket's authoritative
counter for the same object. Surfacing a "lfs bytes transferred"
metric would either lie on cloud backends (always reading zero) or
double-count on localfs in deployments behind a CDN.

Operators get authoritative byte-usage data from the backend itself:

- **S3 / R2.** Enable S3 server access logging on the bucket. Each PUT
  / GET records `Bytes Sent` and `Object Size` columns. Storage Lens
  also exposes per-prefix aggregates for the `lfs/objects/` prefix on
  the AWS console. R2 surfaces the same data in the Cloudflare
  dashboard under bucket → metrics.
- **GCS.** Enable Cloud Storage audit logs (data-access category) and
  filter for the `lfs/objects/` prefix. Object Lifecycle Management
  metrics expose per-bucket byte totals at the storage-class level.
- **Azure Blob.** Enable storage analytics logging on the storage
  account. The logs include `RequestBodySize` and `ResponseBodySize`
  per request; aggregate by prefix.
- **localfs.** `du -sh tenants/<tenant>/repos/<repo>/lfs/objects/` on
  the host filesystem returns the authoritative byte count. For
  cross-repo aggregation, run `du --max-depth=4` against the tenants
  root.

For a gateway-side approximation without object-store logs, sum the
`size=…` attribute on `lfs.verify` audit lines (the client's claimed
size at verify time) or the `bytes=…` attribute on `lfs.object.served`
audit lines (the actual bytes the proxied handler streamed — only
emitted on the proxied path; cloud-direct deployments will not see
these). The `lfs.verify` `size` is an approximation (it is the client's
claim, not the actual transferred byte count) but is sufficient for
capacity-trending dashboards. `lfs.batch` carries `n_objects=<int>`
only, not per-object size.
