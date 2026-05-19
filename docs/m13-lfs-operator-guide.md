# M13 — Git LFS Operator Guide

This guide is for operators who deploy, tune, monitor, and roll back M13 Git
LFS support in production. It covers the LFS production-readiness surface,
the per-repo storage layout, the M13-relevant `bucketvcs serve` flags, the
per-backend transfer-mode matrix, three minimum operator setup recipes, the
signed-URL TTL rule against M8 retention, the verify-failure forensic
procedure, the complete observability surface (6 metrics + 4 audit events),
operations runbooks (signing-key rotation, emergency disable, manual
cleanup), the deferred-work tracker, and an FAQ for the common stock
`git-lfs` operator questions. Stock `git-lfs ≥ 3.0` clients push and pull
unchanged against a bucketvcs gateway over both HTTPS and SSH.

---

## Production readiness

| Concern | Status | Notes |
|---|---|---|
| HTTPS LFS push / pull | ✅ shipped | P0–P3; direct signed URL on S3/R2/GCS/Azure, gateway-proxied URL on localfs |
| SSH `git-lfs-authenticate` | ✅ shipped | P4; Basic-auth bearer in the response header |
| Locks API | ❌ deferred | M13.x or later; clients fall back to no-locking transparently |
| Multipart upload | ❌ deferred | Single PUT only; proxied-path cap 5 GiB (5×2³⁰ bytes), direct-path cap is the backend's single-PUT limit (5 GB / 5×10⁹ bytes on S3/R2 — slightly under the proxied cap) |
| LFS GC | ❌ deferred | LFS objects only sweep when the repo is removed |
| Per-tenant byte quotas | ❌ deferred | Apply object-store-side quotas / lifecycle rules instead |
| LFS-aware bandwidth metering | ❌ deferred | Get byte usage from S3 access logs / GCS audit logs / Azure storage analytics |
| LFS-specific token scopes | ❌ deferred | Today every M4 write token can push LFS; every M4 read token can pull LFS |

The five deferred items are tracked in §8 with the trigger condition each
operator should watch for.

---

## 1. Overview

Git LFS (Large File Storage) stores large blobs out-of-band from the Git
object graph. The client replaces a tracked file with a small pointer blob in
Git; the actual bytes live in an LFS object store keyed by the SHA-256 of the
content. M13 implements the LFS server protocol so that stock `git-lfs`
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
single repo; cross-repo dedupe is deferred work (§8.4).

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
complicates the listing path used by operator-side audits and the (future)
LFS GC walk. The trade-off versus a `<aa>/<bb>/<rest>` 2/2 sharded layout —
the convention some filesystems use to bound directory entry counts — is that
on filesystems with hard caps on entries per directory (ext2/ext3 with the
default 32 000-entry limit, network filesystems with their own limits), a
flat layout can exhaust the directory at high object counts. In production,
M13 runs against cloud object stores (S3, R2, GCS, Azure Blob) where listing
is paginated and there is no directory-entry cap. Localfs is intended for
development; ext4 (the modern Linux default) has no per-directory limit, so
the flat layout is safe in practice.

See spec §4 for the rationale chain and the per-backend single-PUT size
limits that the flat layout interacts with.

---

## 3. Configuration

### 3.1 CLI flags

The five M13-relevant `bucketvcs serve` flags:

| Flag | Default | Purpose |
|---|---|---|
| `--lfs` | `true` | Enable the LFS Batch API. LFS routes (`/info/lfs/objects/batch`, `/info/lfs/objects/<oid>/verify`, `/lfs/objects/<oid>` proxied transfer) are mounted only when this is true. Set to `false` to make the gateway return 404 on every LFS route — see §7.2. |
| `--lfs-presign-ttl` | `15m` | TTL for LFS upload/download presigned URLs (direct mode) and HMAC-signed proxied URLs (localfs). The Batch response's `expires_at` field is set from `now + this`. Clients refresh by re-running Batch. |
| `--lfs-ssh-token-ttl` | `15m` | TTL for the bearer token issued via SSH `git-lfs-authenticate`. The client uses that bearer to drive the HTTPS Batch API and signed-URL transfers — once it expires, the client re-runs the SSH authenticate command. |
| `--proxied-url-signing-key` | (empty) | Path to a file holding an HMAC key (≥ 16 bytes) used to sign proxied `/_lfs/` URLs. Required when the store is localfs and `--lfs=true`. Shared with M11 bundle/pack proxied URLs. |
| `--proxied-url-base` | (empty) | External base URL of this gateway, e.g. `https://gw.example`. Required for SSH `git-lfs-authenticate` (no inbound HTTP request to derive the host from) and for proxied LFS URLs on localfs. HTTPS Batch on cloud backends works without it. |
| `--max-body-bytes` | `1073741824` (1 GiB) | Global HTTP body cap. Applies to every gateway HTTP path including the LFS proxied `/_lfs/` PUT — to allow LFS objects above 1 GiB on the proxied path, raise this to at least the largest expected single-object size (proxied path has a separate 5 GiB hard cap; see §8.2). Direct-path LFS PUTs do NOT pass through the gateway and ignore this flag. |

All defaults match `cmd/bucketvcs/serve.go`. The M8 retention flag
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
  --lfs-presign-ttl 15m
```

Cloud backend means direct presign. No `--proxied-url-signing-key` /
`--proxied-url-base` needed because the gateway is not minting any proxied
URLs. SSH is not started; clients use HTTPS Basic auth (M4 tokens) for the
Batch API.

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
  --proxied-url-base "https://gw.example.com"
```

`--proxied-url-base` is required for SSH `git-lfs-authenticate` — the SSH
session has no inbound HTTP request to derive the gateway URL from, so the
operator must supply it. The signing-key file is still not needed: the SSH
authenticate flow issues a real M4 bearer, not an HMAC-signed proxied URL,
and the LFS transfer URLs are S3-presigned (direct mode).

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
with M11 bundle / pack proxied URLs — if you already run M11 proxied mode,
the existing file is reused.

---

## 4. Signed-URL TTL rule

### 4.1 The hard rule

The M11 retention rule applies to LFS in the same form:

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

The 24× safety factor (the same as M11 §5.4) accommodates GC scheduling
jitter and the §43.6-style race window described in the M8 operator guide.
A URL minted right before a GC mark — but downloaded right after the
following sweep — must remain valid; the 24× headroom is what makes that
hold against the worst-case GC timing.

LFS GC itself is deferred work (§8.3). Today, LFS objects only disappear
when the repo is removed. Reachability-based GC (sweep unreachable LFS
objects after retention elapses) is the milestone that will make the TTL
rule load-bearing for LFS specifically. Until then, the rule is forward-
compatible: setting TTLs that respect it today means you do not need to
revisit your operator config when LFS GC ships.

### 4.2 Relevant flags

LFS TTL flags:

- `--lfs-presign-ttl` — default `15m`. Maximum lifetime of a minted LFS
  upload or download URL (direct on cloud, proxied on localfs).
- `--lfs-ssh-token-ttl` — default `15m`. Maximum lifetime of the bearer
  token issued via SSH `git-lfs-authenticate`. The client uses that bearer
  for the HTTPS Batch API; once it expires, `git-lfs` re-invokes the SSH
  authenticate command to mint a new one.

M8 retention flag:

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

The M11 proxied-URL TTL flags (`--proxied-url-bundle-ttl`,
`--proxied-url-pack-ttl`) are independent of the LFS TTLs and obey the
24× rule in their own right. M13 reuses the M11 signing-key file for
`/_lfs/` URLs but uses a separate TTL knob — there is no shared TTL
constraint between LFS and bundle/pack.

---

## 5. Verify failure forensics

### 5.1 The verify endpoint shape

After every successful LFS upload PUT, the stock `git-lfs` client POSTs to:

```
POST /<tenant>/<repo>.git/info/lfs/objects/<oid>/verify
Content-Type: application/vnd.git-lfs+json
Authorization: <whatever the Batch response's verify.header carried>

{"oid": "<sha256>", "size": <bytes>}
```

The handler decodes the body (cap: 64 KiB), confirms the body's `oid` matches
the URL's `<oid>` (422 on mismatch), and calls `Verify(store, oid, size)`,
which `Head`s the LFS object in storage:

| HTTP status | Trigger | Audit `result` | Metric `result` |
|---|---|---|---|
| `200 OK` | Object exists at the claimed size | `ok` | `ok` |
| `404 Not Found` | Object is absent from storage (`ErrVerifyNotFound`) | `missing` | `missing` |
| `422 Unprocessable Entity` | Object present, size differs (`ErrVerifySizeMismatch`); or malformed body / oid mismatch | `size_mismatch` (size mismatch) or `error` (decode / oid mismatch) | same |
| `500 Internal Server Error` | Backend `Head` returned a non-not-found error | `error` | `error` |

The route's `RequiredAction = ActionWrite` is enforced by `gateway.RunAuth`
upstream; the handler does not re-Decide for write inside `handleVerify`.
Source: `internal/lfs/handler.go` `handleVerify` and
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

### 5.4 The Authorization-echo SECURITY note

The verify request authenticates by echoing the inbound Batch request's
`Authorization` header back into the Batch response, where the
`git-lfs` client picks it up and replays it on the verify POST. The
canonical wording is in `internal/lfs/handler.go` in the SECURITY
comment block above the `bearerForVerify := r.Header.Get("Authorization")`
line inside `handleBatch`. Two live operational concerns:

1. **Client-side persistence.** `git-lfs` caches the verify action's
   `Authorization` value on disk. Under HTTP Basic auth this is
   `base64(user:password)` in plaintext — recoverable from any backup of
   the client machine.
2. **Response-body log exposure.** The `Authorization` header lands inside
   the Batch JSON response body. Any access log / reverse proxy / WAF /
   APM agent that captures response bodies will persist user credentials
   in the log store.

Operator mitigation set (in order of preference):

- **Issue short-TTL `bvts_` tokens as the Basic-auth password.** The
  M4 gateway only parses Basic credentials inbound (see §9.3), so the
  credential leaking through the response-echo path is still going to
  be Basic — but if the *password* portion is a short-TTL `bvts_…`
  token rather than a long-lived plaintext, the blast radius shrinks
  to the TTL window. `bucketvcs token create <user> --expires <duration>`
  mints such a token; clients use it as the password in their
  `Authorization: Basic base64(user:bvts_...)` header. Inbound Bearer
  auth is NOT currently supported.
- **Disable upstream response-body logging.** On any reverse proxy /
  WAF / APM agent between the gateway and the public internet, turn off
  response-body capture for `/info/lfs/objects/batch`. The request-body
  side is safe (the Batch request body contains no credentials).
- **Run `--lfs=false` until the verify-token mechanism lands.** Tracked
  for a later M13 phase: the verify call will be authenticated by a
  single-use HMAC token bound to (oid, repo, actor, expiry) rather than
  the echoed bearer. Until then, operators who cannot satisfy either of
  the two mitigations above should disable LFS at the gateway and run
  large-binary storage out-of-band.

`bucketvcs serve --lfs=true` emits a warning line to stderr at startup
documenting this caveat (see `cmd/bucketvcs/serve.go` in the `*lfsEnabled`
branch of `runServeWithListener`) —
the warning's wording is intentionally identical to the SECURITY block in
handler.go so on-call operators see the same text whether they read the
code or watch the boot log.

---

## 6. Observability reference

### 6.1 Metrics (every M13 emission)

All M13 metrics are emitted as slog text-format `metric` records with
`metric_name=<name> value=<int>` plus label key/value pairs. Below are
the six M13 metrics with valid label values and emission sites:

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
was missing or invalid. Localfs-only.
- `reason`: `missing` (no `?token=…` query parameter), `invalid` (token
  decode / HMAC verify failed), `expired` (token past its expiry), or
  `kind_mismatch` (token minted for `lfs-get` used on a PUT, or vice
  versa).

Site: `internal/lfs/proxied.go` request prologue. Alert on sustained
non-zero `expired` or `kind_mismatch` — they indicate clients holding
stale URLs (set TTL too short) or active enumeration / replay attempts.

#### `lfs_verify_requests_total{result}`

One record per verify request. No `op` label — verify is operation-less.
- `result`: `ok`, `missing`, `size_mismatch`, `error`. See §5.2 for
  operational meaning of each label.

Site: `internal/lfs/handler.go` `handleVerify`.

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
    too, since deploy actors cannot mint LFS bearers — see M13 P4 scope
    decision).
  - `error` — token mint failed, IO failed, or other internal error.
  - `client_disconnected` — token was minted and written to the wire
    but the client dropped before the gateway saw the close — useful for
    detecting flaky SSH transports, see EmitSSHAuthenticateMetric godoc.

Site: `internal/sshd/session.go` `handleLFSAuthenticate`.

### 6.2 Audit events

Four M13 audit events. All use the flat-attribute slog shape established
by M11 — each event has a top-level `event=<name>` attr plus event-
specific attrs. The audit stream is the same stdout/stderr stream that
carries metrics.

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

Attrs: `repo=<tenant>/<repo>`, `user=<actor>`, `oid=<sha256>`,
`size=<claimed size>`, `result=ok|missing|size_mismatch|error`. The
`user` attr is never empty in practice today because verify is
ActionWrite-only — anonymous callers never reach the handler.

Site: `internal/lfs/audit.go` `emitLFSVerify`, called from
`handleVerify`.

#### `event=lfs.ssh_authenticate`

Emitted at the end of every SSH `git-lfs-authenticate` exec command.

Attrs: `repo=<tenant>/<repo>`, `user=<actor name or empty>`,
`op=upload|download`, `ttl_seconds=<int64>` (0 on disabled / forbidden /
anon paths; the configured TTL otherwise), `result=ok|forbidden|disabled|
anon|error|client_disconnected`.

Site: `internal/lfs/audit.go` `EmitLFSSSHAuthenticate`, called from
`internal/sshd/session.go`.

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
`ProxiedURLSigningKey`, and the M11 `URLBuilder.ProxiedKey` simultaneously.
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
- The §5.4 SECURITY caveat is being actively exploited.
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
   `/info/lfs/objects/batch`, `/info/lfs/objects/<oid>/verify`, and
   `/_lfs/...`. The SSH `git-lfs-authenticate` exec command emits
   `result=disabled` on its audit event and returns a non-zero exit
   status; stock `git-lfs` clients surface this as a clear "LFS not
   available on the server" error.

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

There is no built-in command to garbage-collect orphaned LFS objects
today. LFS-aware reachability GC is deferred work; see §8. Objects are
removed only when the entire repo is removed (and even then, the actual
sweep depends on the storage adapter's behaviour at repo deletion).

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

This procedure is out-of-band and unsupported by the gateway directly. It
exists to bridge the gap until LFS GC ships. The trigger condition for
the deferred LFS-GC work is "storage costs growing without corresponding
repo activity" — operators who reach that threshold should track the
deferred item in §8.3 rather than running the manual procedure
indefinitely.

---

## 8. Deferred work

Five items are tracked deliberately outside M13. Each subsection below
records why the item is deferred, the operational trigger condition that
should prompt operators to escalate, and the workaround available today.
The production-readiness table in the preamble cross-references each
item by section number.

### 8.1 LFS Locking API

**Status.** Not implemented. The Locks API (`POST /locks`, `GET /locks`,
`POST /locks/:id/unlock`, `POST /locks/verify`) is part of the LFS
server protocol but is not served by M13. The `git-lfs` client treats
absence of the Locks endpoints as "this server does not support
locking" and falls back to no-locking transparently — `git lfs lock`
and `git lfs unlock` fail with a clear client-side error, and `git lfs
push` / `git lfs pull` are unaffected.

**Why deferred.** Lock state is a separate control-plane data model
(lock ID, OID or path, owning user, creation time, repo scope). It does
not fit into the per-repo object-store layout that M13 uses for the
blob payload itself. The spec §11 lists Locks as M13.x or later
precisely because the storage model is non-trivial: locks need either a
separate per-tenant relational store or a per-repo JSON ledger with
race-safe updates, and either choice deserves its own milestone.

**Trigger condition.** Ops teams asking about a file-level
"reservation" or "checkout" mechanism for large binary assets — usually
art / CAD / model-binary workflows where two engineers stepping on the
same `.psd` or `.fbx` would lose work that LFS dedupe cannot recover.

**Workaround today.** Either client-side locking conventions
(out-of-band lock files in a shared system) or none — modern asset
workflows often skip locks entirely and rely on review / branching
discipline. The gateway does not advertise Locks support, so clients
configured to require locks will surface the missing capability on
their first lock attempt rather than failing silently mid-push.

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
the effective ceiling on every backend for M13 if you want a single
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

**Status.** Not implemented. LFS objects are swept only when the entire
repo is removed (at which point the storage adapter's repo-deletion
path removes the `tenants/<tenant>/repos/<repo>/` prefix). There is no
mark-and-sweep against the set of OIDs referenced by reachable commits.

**Why deferred.** The M8 reachability GC (for Git objects) walks the
commit graph and never needs to leave the object database. LFS GC has
to additionally enumerate every LFS pointer blob inside every reachable
tree, then diff against the LFS object set in storage. That walk is
much more expensive than M8's pack-level walk and benefits from a
caching layer that does not exist today. The spec scopes LFS GC as a
post-M13 milestone.

**Trigger condition.** Storage-cost growth uncorrelated with repo push
activity — e.g. monthly LFS byte usage doubling while commit volume
stays flat. The signal usually surfaces in backend billing dashboards
rather than gateway metrics (see §8.4 and §9.4).

**Workaround today.** The manual out-of-band procedure in §7.3
(`aws s3 ls` against the LFS prefix, `git lfs ls-files --all` against a
clean clone, `comm -23` to compute the orphan set, manual review and
deletion). The procedure exists to bridge the gap until LFS GC ships
and is documented as unsupported.

### 8.4 Per-tenant byte quotas + pooling + cross-repo dedupe

**Status.** Not implemented. M13 has no in-process quota counter, no
shared-OID pool across repos, and no per-tenant ceiling enforced at the
Batch handler. Every LFS write is allowed as long as the upstream M4
authentication grants write to the target repo.

**Why deferred.** Per-tenant quotas are a control-plane feature: they
need a credible accounting store (counter durability, rollover at the
billing boundary, atomic decrement on object removal once GC ships),
and they only make sense once LFS GC also ships so that the counter is
authoritative. Cross-repo dedupe is the opposite trade — pooling LFS
objects by OID across an entire tenant maximizes storage savings but
costs control-plane complexity for repo-scoped access decisions. M13
intentionally chose per-repo isolation to keep the access model
identical to the Git object path.

**Trigger condition.** Contractual byte quotas required per tenant
(a SaaS offering with per-plan storage tiers, or an internal billing
chargeback model), or a tenant whose LFS storage cost is dominated by
the same asset replicated across many forks of one repo.

**Workaround today.** Object-store-side controls:

- S3: bucket policies + S3 Storage Lens metrics + lifecycle rules.
- R2: per-bucket usage in the Cloudflare dashboard.
- GCS: bucket-level Storage Lifecycle Management + IAM.
- Azure Blob: storage account quotas + lifecycle management policies.
- localfs: filesystem-level quotas (XFS / ZFS quotas, LVM thinpool
  reservations).

These backend-side controls have no concept of "tenant" — operators
who run a single bucket per tenant get clean accounting; operators who
multiplex tenants into one bucket need a separate counter outside the
gateway.

### 8.5 Verify-token mechanism

**Status.** Not implemented. The verify endpoint still authenticates by
echoing the inbound Batch request's Authorization header back into the
verify action of the Batch response — see §5.4 for the full SECURITY
note, the operator mitigation set, and the boot-time stderr warning.

**Why deferred.** A verify-token replacement (single-use HMAC token
bound to oid, repo, actor, expiry) requires its own signing-key
rotation story alongside the M11/M13 proxied-URL signing key, and its
own minting / validation surface in the handler. It is non-trivial
work and is tracked for a follow-on phase; until then the operator
mitigation set in §5.4 is the supported path.

**Trigger condition.** Any deployment where neither of §5.4's two
mitigations (Bearer-token rollout, or disabling upstream response-body
logging for `/info/lfs/objects/batch`) can be satisfied. Such operators
should run `--lfs=false` until the verify-token mechanism lands.

**Workaround today.** §5.4's mitigation set (use Bearer; or disable
response-body capture; or `--lfs=false`). The gateway's boot-time
stderr warning is intentionally worded identically to the SECURITY
block in `internal/lfs/handler.go` so on-call operators see the same
text whether they read the code or watch the boot log.

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
M4 gateway's `RunAuth` path in `internal/gateway/auth.go` only parses
HTTP Basic credentials — it calls `r.BasicAuth()` and
dispatches to `auth.BasicPassword` against the M4 token store. There is
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
