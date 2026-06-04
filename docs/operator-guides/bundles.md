# Operator Guide: Bundle-URI and Packfile-URI Acceleration

This guide is for operators who deploy, tune, monitor, and roll back
bundle-uri and packfile-uri acceleration in production. It covers what
bundle-uri and packfile-uri are, the bundle freshness state machine, how to
schedule bundle generation alongside `bucketvcs maintenance`, when to use
signed-URL vs gateway-proxied delivery, how the URL TTL rule interacts with
GC retention, the eleven observability metrics and four audit events,
an eight-entry troubleshooting matrix, the migration recipe for enabling
acceleration, post-incident forensics, and the deferred-work tracker
so operators can plan around what is not yet production-ready.

---

## Production readiness

| Mode | `--bundle-uri-mode` | `--pack-uri-mode` |
|------|---------------------|-------------------|
| `direct` | **GA** | **GA** |
| `proxied` | **GA (multi-tenant).** The proxied handler keys URLs as `/_bundle/<tenant>/<repo>/<hash>?token=...` and the HMAC binds the composite `(tenant, repo, hash)` so cross-tenant URL tampering is rejected. One `bucketvcs serve` may host many tenants. | Same — `/_pack/<tenant>/<repo>/<hash>?token=...` with the same composite binding. |
| `auto` | **GA on direct-capable backends** (S3, R2, GCS, Azure Blob). On localfs, `auto` falls back to proxied behavior — multi-tenant safe. | Same as bundle. |
| `off` | **GA.** Behavior reverts to standard `upload-pack`; reachability and negotiation paths unchanged. | Same. |

The proxied handler keys URLs by `<tenant>/<repo>` segments and computes the HMAC over the composite `tenant/repo/hash`, so any segment swap fails token verification with HTTP 403. This is what lets a single proxied gateway safely host many repositories. See §4 for the full tradeoff and §9 for troubleshooting.

---

## 1. Overview

Bundle-uri (Git protocol v2 §16.3) is an acceleration mechanism for cold
clones. When a client sends `command=bundle-uri`, the gateway advertises a URL
for a pre-built `*.bundle` file rather than streaming a server-generated pack.
The client downloads the bundle — which contains all objects reachable from the
default branch tip at bundle-generation time — then runs a smaller incremental
fetch for any commits that arrived since the bundle was generated. For large
repositories, this eliminates the most expensive path through `upload-pack`:
the full reachability walk and pack assembly on the server.

Packfile-uri (Git protocol v2 §16.4) is a complementary mechanism for
full-clone requests where the repository manifest contains exactly one
canonical pack. Instead of running `pack-objects` to stream a new pack, the
gateway advertises a URL for the existing `.pack` file on storage. Packfile-uri
uses `--keep-pack` elision so the inline pack alongside the advertised URI is
reduced to whatever objects are not covered by the URI; the bulk bytes are not
shipped twice.

The cold-clone win is the primary motivation for acceleration. Without bundle-uri, a
fresh `git clone` of a large repository causes the gateway to run
`upload-pack`, walk all reachable objects, and stream a full pack through the
gateway VM. With bundle-uri active and a current bundle, the heavy bytes flow
from the object-storage bucket directly to the client (direct mode) or through
a lightweight gateway proxy (proxied mode). The gateway's CPU and the
bucketvcs-side reachability walk are bypassed for the bulk of the data.

Bundle-uri and packfile-uri are independent features. An operator can enable
one without the other:

- Bundle-uri targets cold clones. Enable it whenever you have a repository
  large enough that server-side pack assembly is the cloning bottleneck.
- Packfile-uri targets full-clone-after-repack scenarios where `bucketvcs
  maintenance` has consolidated the repository into a single canonical pack.
  It yields nothing if the manifest has multiple packs (the gateway falls back
  to inline pack emission).

Neither mechanism helps in every situation. Small repositories are already
cheap to clone; the overhead of signing a URL exceeds any win. Deep partial
fetches (e.g., sparse checkouts of a subtree) are not covered by a bundle
built against the default branch. Force-pushes retire the bundle's tip commit
from the current ref set, causing the freshness state machine to move the
bundle to `retired` and fall back to standard fetch for that client request.

See also:
- [`bucketvcs maintenance`](maintenance.md) for the repack
  and index pipeline that bundle generation runs alongside.
- [`bucketvcs gc`](gc.md) for the retention rules that
  constrain bundle URL TTLs.
- [Reachability index](reachability.md) for the M10
  compaction that keeps negotiation fast for incremental fetches.

---

## 2. Bundle Freshness Model

Each time the gateway receives a `command=bundle-uri` request, it evaluates
the freshness of the repository's `full_default` bundle entry. The result is
one of seven states (closed vocabulary from
`internal/v2proto/bundleuri_freshness.go`):

| State | Meaning | When emitted |
|-------|---------|--------------|
| `disabled` | The gateway has bundle-uri turned off (`--bundle-uri-mode=off` or no `BuildURL` configured). | `command=bundle-uri` arrived; gateway emits `bundle_advertised_total{freshness=disabled}` and returns an empty response. |
| `no_bundle` | The manifest has no `full_default` bundle entry. Maintenance has never generated one, or it was retired and not yet regenerated. | Operator action: run `bucketvcs maintenance --bundle-only`. |
| `no_ref` | A `full_default` bundle entry exists but its `TipOID` references a ref that no longer exists in the manifest (likely force-pushed away or branch renamed). | Operator action: next maintenance run will retire the stale entry and may generate a fresh one. |
| `current` | Bundle covers the current default-branch tip. Client gets the bundle URL with no walkback. | The hot path. |
| `warm` | Bundle is behind the current tip by ≤ `--bundle-warm-commits` (default 5000) AND younger than `--bundle-warm-age` (default 24h). Client gets the bundle URL plus an incremental fetch. | Acceptable while maintenance is between scheduled runs. |
| `stale` | Bundle is behind by more than `--bundle-warm-commits` OR older than `--bundle-warm-age`. Gateway does NOT advertise the bundle URL — the client falls through to standard fetch. `bundle_advertised_total{freshness=stale}` still increments (the dispatch attempt is counted), but no URL is returned. The label exists so operators can alert on sustained `stale` rates indicating maintenance is falling behind. | Operator action: tighten maintenance cadence or raise `--bundle-warm-commits`. |
| `retired` | Internal state: bundle's TipOID is no longer reachable from any ref (force-push retired the commit). Gateway does NOT advertise the bundle to the client. This state surfaces as `bundle_advertised_total{freshness=no_ref}` or similar — the literal label value `retired` is reserved for future use and is never emitted by the current metric implementation (the gateway routes all `retired`-state dispatches through the `no_bundle` or `no_ref` Reason codes before the state machine runs). | Operator action: next maintenance run will generate a fresh entry. |

### 2.1 State transition diagram

```
                  +----------------+
                  | maintenance    |
                  | --bundle-only  |
                  +-------+--------+
                          | generate
                          v
       no_bundle ---> current ---(walkback)--> warm
            ^             |                      |
            |             | (force-push)         | (>--bundle-warm-* threshold)
            |             v                      v
       retired <-------- no_ref              stale
            ^             ^                      |
            |             |                      |
            +-------------+----------------------+
                (next maintenance run regenerates from current ref tip)
```

A bundle that enters `retired` state is never advertised. The next maintenance
run detects that the prior `full_default` entry's `TipOID` is no longer
reachable, retires it (emitting a `bundle.retired` audit event), and generates
a fresh entry from the current ref tip.

### 2.2 Threshold tuning

Two flags govern the `current` → `warm` and `warm` → `stale` transitions:

- `--bundle-warm-commits` (default `5000`). The maximum number of commits the
  current default-branch tip may be ahead of the bundle tip before the bundle
  moves from `warm` to `stale`. Increase for repositories with high
  commit-rate-per-day. Decrease for low-churn repositories where any staleness
  is noticeable.

- `--bundle-warm-age` (default `24h`). The maximum age of the bundle before it
  moves from `warm` to `stale`, regardless of commit distance. Decrease only if
  you run maintenance more often than once per day. Increase only if your
  maintenance cadence is genuinely greater than 24h (unusual).

Only `current` and `warm` advertise the bundle URL to the client. `stale` is
observable via `bundle_advertised_total{freshness=stale}` but does not produce
a URL; if you alert on anything, alert on a sustained nonzero rate against
legitimate advertise traffic. See §8.3 for rate-amplification gotchas.

---

## 3. Maintenance Scheduling

### 3.1 Overview

`bucketvcs maintenance --bundle-only` runs only the bundle phase. `bucketvcs
maintenance --force` (without `--bundle-only`) runs the full pipeline: repack
+ reachability compaction + bundle refresh. For most operators, run the full
pipeline at the same cadence as repack — the materialized mirror is built once
per pipeline run, so bundling is nearly free when colocated with repack.

Co-scheduling rule: run repack + reachability compaction + bundle-refresh
together. Do not schedule `--bundle-only` on a separate cron line unless you
have a specific reason — it builds a temporary mirror that the full pipeline
would otherwise have built anyway. If you want more frequent bundle refresh
than full maintenance allows (e.g., hourly bundles with 4-hourly repacks), a
separate `--bundle-only` invocation is appropriate; otherwise keep them
together.

The bundle phase has its own threshold logic for skipping bundle generation
when the prior bundle is still current (the freshness state machine described
in §2). You almost never need to gate maintenance externally; let the pipeline
decide whether bundle work runs.

### 3.2 Recipe 1 — cron

```
# Every 4 hours, run full maintenance for one specific repo.
17 */4 * * *  bucketvcs  /usr/local/bin/bucketvcs maintenance \
                            --store=s3://my-bucket?endpoint=https://r2... \
                            --repo=tenant/repo \
                            --force \
                            >> /var/log/bucketvcs/maintenance.log 2>&1
```

Use `--all-repos` instead of `--repo=...` to iterate every repository in the
store.

### 3.3 Recipe 2 — Kubernetes CronJob

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: bucketvcs-maintenance
spec:
  schedule: "17 */4 * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: OnFailure
          containers:
            - name: maintenance
              image: bucketvcs:latest
              command:
                - bucketvcs
                - maintenance
                - --store=s3://my-bucket?endpoint=https://r2...
                - --all-repos
                - --force
              env:
                - name: AWS_ACCESS_KEY_ID
                  valueFrom: { secretKeyRef: { name: r2-creds, key: access } }
                - name: AWS_SECRET_ACCESS_KEY
                  valueFrom: { secretKeyRef: { name: r2-creds, key: secret } }
```

`concurrencyPolicy: Forbid` is important — two concurrent maintenance runs
against the same repository will fight on CAS and one will fail.

### 3.4 Recipe 3 — systemd timer

```ini
# bucketvcs-maintenance.service
[Unit]
Description=bucketvcs maintenance run

[Service]
Type=oneshot
ExecStart=/usr/local/bin/bucketvcs maintenance \
    --store=s3://my-bucket?endpoint=https://r2... \
    --all-repos \
    --force
User=bucketvcs
```

```ini
# bucketvcs-maintenance.timer
[Unit]
Description=bucketvcs maintenance every 4 hours

[Timer]
OnCalendar=*-*-* 0/4:17:00
Persistent=true

[Install]
WantedBy=timers.target
```

`Persistent=true` runs the timer immediately after a missed boot window.
Useful for single-host deployments that may reboot mid-window.

---

## 4. Signed-URL vs Gateway-Proxied

### 4.1 Mode comparison

| Mode | Backend | Bandwidth path | Audit visibility | Multi-tenant ready? | When |
|------|---------|----------------|------------------|---------------------|------|
| `direct` | cloud (S3, R2, GCS, Azure Blob) | client → bucket | none at gateway | yes | public-internet repos; lowest gateway cost; egress charged to bucket |
| `proxied` | localfs OR cloud | client → gateway → bucket | full (every serve emits `proxied.url.served` audit event with `tenant`+`repo` labels) | **yes** — URLs embed `<tenant>/<repo>` and HMAC binds the composite | audit-strict deployments; localfs deployments (no signed URLs available); multi-tenant gateways that want one mount per cluster |
| `auto` | any | direct if backend supports signed URLs; proxied otherwise | partial (direct serves are invisible to gateway audit) | yes (both direct-capable backends and localfs) | default for most operators; covers both backends without per-deploy config |
| `off` | any | n/a (capability disabled) | n/a | yes | fall back to standard fetch; useful for rollback or known-incompatible clients |

### 4.2 Direct mode

Direct mode mints a signed URL (S3-style presigned GET, GCS V4 signature,
Azure SAS, R2 presigned). The client downloads from the bucket directly. The
gateway sees the advertise request but never sees the bytes. TTL is bounded
by `--proxied-url-bundle-ttl` (4h default) and `--proxied-url-pack-ttl` (1h
default). The TTL must also satisfy the retention rule:
`TTL ≤ retention / 24` (see §5).

### 4.3 Proxied mode

Proxied mode mints a gateway URL of the form
`https://<gateway>/_bundle/<tenant>/<repo>/<hash>?token=<HMAC>` (or
`/_pack/<tenant>/<repo>/<hash>`). The HMAC token is computed over the
composite `tenant/repo/hash`, so any segment swap (different tenant,
different repo, or different hash) on a stolen URL fails verification with
HTTP 403. The client downloads from the gateway, which streams from the
bucket. Every serve emits `proxied.url.served` with `tenant` + `repo`
attribution labels. A single `bucketvcs serve` may host many
tenants under one mount.

### 4.4 Auto mode

Auto mode is the recommended default. The gateway picks direct on backends
that can sign URLs (S3, R2, GCS, Azure Blob); falls back to proxied on
localfs. If you run a hybrid deployment (cloud backend for some repos, localfs
for others), set `auto` and the gateway routes correctly per backend without
per-deployment configuration.

### 4.5 Off mode

Off mode disables advertisement entirely. Use for rollback or when integrating
with a client that mishandles bundle-uri (rare; stock git ≥ 2.41 handles it
correctly). See §7 for what continues to function after disabling.

### 4.6 Signing-key rotation

When the signing-key file is rotated, all unexpired tokens minted against the
old key produce a `proxied_url_token_invalid_total{reason=invalid}` metric.
Plan signing-key rotations to align with the longest active TTL window. If you
must rotate immediately, accept a brief 403 spike until in-flight URLs expire
naturally (max `--proxied-url-bundle-ttl`).

---

## 5. TTL vs Retention

### 5.1 The hard rule

TTLs must be tuned against the GC retention window:

```
TTL ≤ retention / 24
```

This is an operational rule, not a CLI-enforced check. If you violate it, the
binary will run, but operators may receive bundles or packs that reference
GC-swept blobs (404 in direct mode, 500 in proxied mode). Treat the rule as a
hard pre-deploy lint.

### 5.2 Relevant flags

TTL flags:

- `--proxied-url-bundle-ttl` — default `4h`. Maximum lifetime of a minted
  bundle URL (direct or proxied).
- `--proxied-url-pack-ttl` — default `1h`. Maximum lifetime of a minted pack
  URL.

Retention flag:

- `bucketvcs gc --retention` — default `168h` (7 days).

### 5.3 Constraint demonstration

```
proxied-url-bundle-ttl ≤ retention / 24    → 4h ≤ 168h/24=7h    ✓ OK at defaults
proxied-url-pack-ttl   ≤ retention / 24    → 1h ≤ 168h/24=7h    ✓ OK at defaults
```

### 5.4 Why the 24× factor exists

A URL minted at the start of a TTL window references a bundle (or pack) by
storage key. If that object is GC-swept before the URL expires, the client
receives a 404 (direct mode) or 500 (proxied mode). The TTL must be short
enough that any outstanding URL expires well before the retention window could
elapse on the referenced bundle or pack. Otherwise, a client could receive a
URL, hold it through a GC cycle, and find the object gone when finally
downloading. The 24× safety factor accommodates GC scheduling jitter and the
§43.6 race window (see [GC race window](gc.md#4-the-436-race-window)).

### 5.5 Adjusting retention

If you decrease `--retention` below 24h, you are violating the TTL rule at
default TTLs. This is an operational rule, not a CLI-enforced check — the
binary will run, but operators may receive bundles or packs that reference
GC-swept blobs. Tune retention upward, not TTL downward — TTL
governs client-side cache windows, not just URL validity.

If you decrease retention to, for example, 48h, reduce both TTLs
proportionally:

```
--proxied-url-bundle-ttl=2h --proxied-url-pack-ttl=2h
```

(Since `48h / 24 = 2h`.)

---

## 6. Bandwidth and Cost Economics

### 6.1 The shift to acceleration

- **Without acceleration**: every clone's bytes flow through `bucketvcs serve` → gateway
  VM/container egress, with gateway CPU running upload-pack. Bandwidth is billed
  gateway-side.
- **Direct mode**: bytes flow client → bucket directly via a signed URL.
  Egress is billed by the object-storage provider. The gateway VM only sees
  the small advertise-bundle response.
- **Proxied mode**: bytes flow client → gateway → bucket. The gateway VM
  sees full egress AND bucket egress — effectively 2× bandwidth for the same
  clone. The benefit is full audit visibility via `proxied.url.served` events.

### 6.2 Per-backend cost reasoning

- **R2 (Cloudflare)** has zero egress fees. Direct mode is a strict cost win —
  no per-byte charge for bucket egress. Use direct mode whenever you do not
  need per-clone audit logs.

- **S3 / AWS** charges egress per GB. If the gateway VM is in the same region
  as the bucket, proxied mode is more expensive (each byte hits both S3-egress
  and VM-egress). Direct mode bypasses VM-egress. If the bucket is in a
  different region from the gateway, direct mode is also faster (one network
  hop instead of two).

- **GCS** is similar to S3. Direct mode saves the VM-egress hop.

- **Azure Blob** is similar. Direct mode saves the VM-egress hop.

### 6.3 Recommended configurations

For hosted deployments where the dispatch user runs a managed gateway, R2 with
`auto` mode is the cheapest configuration: clones flow direct to R2 (no egress
charge), pushes still flow through the gateway (CAS, auth), and the gateway VM
stays small.

For air-gapped or audit-strict OSS deployments, `proxied` is the only option
that gives you a `proxied.url.served` audit event per clone. Accept the 2×
bandwidth cost for the audit benefit. Proxied mode is multi-tenant
safe — a single gateway may host many tenants and the `proxied.url.served`
events carry `tenant` + `repo` labels so operators can attribute serve volume
per repository.

---

## 7. Disabling Acceleration

### 7.1 Single-command disable

```bash
bucketvcs serve --bundle-uri-mode=off --pack-uri-mode=off  ...other flags...
```

### 7.2 What happens after `--bundle-uri-mode=off`

- `command=bundle-uri` requests return an empty response (no URL advertised).
- `bundle_advertised_total{freshness=disabled}` increments per request.
- Clients fall back to standard `command=fetch`. Reachability and negotiation
  paths (reachability index, delta chain, repack-aware pack walk) are unchanged.
- Maintenance still generates bundles. The bundle entries accumulate in the
  manifest; they are simply not advertised. If you later re-enable bundle-uri,
  the freshness state machine evaluates them as-is — no special re-warming is
  needed.

### 7.3 What happens after `--pack-uri-mode=off`

- The `packfile-uris` capability is not advertised. Stock clients silently
  downgrade to inline packs.
- `pack_uri_advertised_total` does not increment.

### 7.4 Rollback safety

Rollback is fully reversible. Bundle/pack manifest fields are `omitempty`; the manifest
schema does not change shape based on whether the gateway advertises or not. If
you disable acceleration in production, redeploy with the modes back to `auto` (or
`direct`) and behavior resumes immediately — no manifest migration is needed.

---

## 8. Observability Reference

Acceleration ships eleven metrics (three maintenance-side, eight gateway-side) and four
audit events (two maintenance-side, two gateway-side). All metrics and audit
events emit via slog. The format is one JSON line per metric or event;
operators ingest into Loki, Vector, or any slog-compatible pipeline.

### 8.1 Metrics

#### 8.1.1 Maintenance-side metrics

Emitted from `internal/maintenance/log.go::emitBundleResultMetrics`, called
inside `pipeline.go::emitFinalReport` when the maintenance pipeline ran the
bundle phase.

| Metric | Labels | Semantics |
|--------|--------|-----------|
| `bundle_generated_total` | `outcome` ∈ {`success`, `noop`, `failure`}, `repo_id`, `trigger_reason` | Count of bundle-generation attempts. `success` = `BundleResult.Generated == true`. `noop` = generation skipped intentionally (the freshness trigger declined, OR a recoverable per-repo skip fired: `skipped_reachability_load_error`, `skipped_trigger_eval_error`). `failure` = unexpected error path. The `trigger_reason` label carries the specific cause string (e.g. `tip_unchanged`, `walk_distance_under_threshold`, `skipped_reachability_load_error`). |
| `bundle_generation_duration_seconds` | `repo_id` | Wall-clock seconds for the bundle-generation phase. Emitted on every pipeline run that produced a `BundleResult`, regardless of outcome. |
| `bundle_bytes` | `repo_id` | Final bundle file size in bytes. Emitted only when `BundleResult.Generated && BundleResult.ByteSize > 0`. |

#### 8.1.2 Gateway-side metrics

Emitted from `internal/gitproto/uploadpack/log.go::emitMetric` (HTTP gateway
path) and `internal/gateway/log.go::emitMetric` (proxied serve path). Both
paths reach the same metric names; operators do not need to distinguish them.

| Metric | Labels | Semantics + rate-amplification |
|--------|--------|--------------------------------|
| `bundle_advertised_total` | `repo_id`, `freshness` ∈ {`disabled`, `no_bundle`, `no_ref`, `current`, `warm`, `stale`} | Per `command=bundle-uri` dispatch. **Includes the encode-error path** — this counts dispatch attempts, not successful encodes. **Rate-amplification: rogue or misconfigured clients can pump this at arbitrary rate.** Alert on the per-freshness rates for legitimate advertise traffic (`current` + `warm`); do not alert on the raw total. |
| `bundle_uri_advertised_total` | `repo_id`, `via` ∈ {`proxied`, `direct`} | Only emitted when the bundle-uri response actually contains URLs (`freshness ∈ {current, warm}`). The `via` label is derived from the URL path (`/_bundle/` → `proxied`, otherwise → `direct`). |
| `bundle_uri_served_total` | `via`, `tenant`, `repo` | Per successful proxied bundle serve. **Counts truncated serves too** — `io.Copy` mid-stream errors still emit. Pair with `bundle_uri_served_bytes` and the `proxied.url.served` audit event's `status_code` field to distinguish full from truncated. The `tenant` + `repo` labels carry the URL's `/_bundle/<tenant>/<repo>/<hash>` segments. |
| `bundle_uri_served_bytes` | `via`, `tenant`, `repo` | Actual bytes written via `countingWriter`. May be less than the bundle's full size on client disconnect. |
| `pack_uri_advertised_total` | `repo_id`, `via` | Emitted when the `packfile-uris` stanza fires. |
| `pack_uri_served_total` | `via`, `tenant`, `repo` | Per successful proxied pack serve. Truncation semantics same as `bundle_uri_served_total`. |
| `pack_uri_served_bytes` | `via`, `tenant`, `repo` | Same shape as `bundle_uri_served_bytes`. |
| `proxied_url_token_invalid_total` | `reason` ∈ {`missing`, `expired`, `kind_mismatch`, `invalid`} | Per token-validation failure on `/_bundle/` or `/_pack/`. `missing` = no `token` query parameter. `expired` = signature past TTL. `kind_mismatch` = bundle token presented to `/_pack/` (or vice versa). `invalid` = signature failure (including cross-tenant URL tamper — the composite HMAC mismatches when any segment is swapped). The user-facing 403 body collapses `kind_mismatch` to "invalid token" — do not rely on the response body to distinguish; use this metric. |

**Cardinality note.** The served-* metrics (`bundle_uri_served_*`,
`pack_uri_served_*`) carry `tenant` + `repo` labels — both values are
validated by the URL parser before reaching the metric emit, so values are
safe and bounded by the registered-repos set. Operators with thousands of
repos should size their Prometheus storage accordingly.
`proxied_url_token_invalid_total` deliberately has no `tenant` or `repo`
label: the failure path fires _before_ the URL has been verified, so
attributing a forged URL to its claimed tenant would leak the existence of
that (tenant, repo) pair to anonymous probers.

### 8.2 Audit events

All four events use the flat-attrs shape (`slog.Bool("audit", true)` +
`slog.String("event", "...")` + top-level attribute fields). Grep with:

```bash
jq 'select(.audit == true and (.event | startswith("bundle.")))'
```

to extract all bundle audit events.

#### 8.2.1 Maintenance-side audit events

Both emitted from `internal/maintenance/bundle.go::runBundlePhase`, inside the
`if !opts.DryRun { ... }` guard, after `RunBundleCASMerge` succeeds. Retired-
before-generated emission order pairs the events atomically.

- **`bundle.generated`** — one event per generated bundle.
  Fields: `repo_id`, `bundle_id`, `bundle_hash`, `tip_oid`,
  `covers_manifest_version`, `byte_size`, `duration_ms`.
  Use case: dashboard ingestion of bundle production rate; join `bundle_id`
  against `bundle.retired.replaced_by` for replacement-pair tracking.

- **`bundle.retired`** — emitted before the paired `bundle.generated` when a
  CAS-merge supersedes an existing `full_default` entry.
  Fields: `repo_id`, `bundle_id` (the retired bundle's ID), `reason`,
  `replaced_by` (the new bundle ID from the paired `bundle.generated`).
  Current `reason` vocabulary: `"replaced"` only. Future work will add
  `"gc_swept"` when the full bundle GC pipeline lands (see §12).

DryRun mode emits no audit events.

#### 8.2.2 Gateway-side audit events

- **`bundle.uri.advertised`** — emitted from `serveBundleURI` when the
  response contains URLs (`freshness ∈ {current, warm}`).
  Fields: `repo_id`, `freshness`, `via`, `bundle_count` (= 1 today;
  pluralized in a future release), `first_tip_oid` (matches the
  `bundle.generated.tip_oid` of the advertised bundle — operators correlate
  the two events).

- **`proxied.url.served`** — emitted from the proxied handler post-`io.Copy`
  on 200 or 206.
  Fields: `kind` ∈ {`bundle`, `pack`}, `hash`, `bytes_served` (via
  `countingWriter`, may be less than full size on disconnect),
  `status_code` ∈ {200, 206}, `range_request` (boolean — whether the client
  sent a `Range:` header).

### 8.3 Rate-amplification gotchas

Three properties of the observability surface that operators must factor
into alerting:

1. `bundle_advertised_total` increments per `command=bundle-uri` dispatch,
   regardless of whether URLs were returned, regardless of whether the response
   encoded successfully. A rogue or misconfigured client can pump this metric
   arbitrarily. **Alert on per-freshness rates**
   (`bundle_advertised_total{freshness="current"}` etc.) against legitimate
   traffic baselines; do not alert on the raw total.

2. `bundle_uri_served_total` and `pack_uri_served_total` fire after `io.Copy`
   returns, regardless of whether the copy completed in full. A connection
   that disconnects mid-stream still increments `*_served_total` (the copy
   returned) with a partial `*_served_bytes` reflecting the bytes actually
   written. To distinguish full from truncated serves, compare `bytes_served`
   to the expected bundle/pack size (use the matching `bundle.uri.advertised`
   event's first_tip_oid to fetch the manifest entry, or join against the
   `proxied.url.served` audit event's `status_code` and `bytes_served`
   fields).

3. All served-* metrics carry `tenant` + `repo` labels. Multi-repo
   proxied deployments can be observed per-repo by selecting on
   `tenant=...,repo=...`. Deployments that predate these labels lacked them; if you are
   parsing historical metrics, join via the `proxied.url.served` audit event
   (which carries the same fields in both eras: the labels and the audit-event
   attribute names match).

### 8.4 Example slog grep recipes

Grep all bundle audit events from a slog JSON-line stream:

```bash
jq -c 'select(.audit == true and ((.event | startswith("bundle.")) or .event == "proxied.url.served"))' \
    /var/log/bucketvcs/bucketvcs.json
```

Get the bundle-generation rate per repo for the last hour:

```bash
jq -c 'select(.event == "bundle.generated") | {time, repo_id, bundle_hash, byte_size}' \
    /var/log/bucketvcs/bucketvcs.json
```

Correlate a `bundle.uri.advertised` event to its generating `bundle.generated`
event:

```bash
TIP_OID=$(jq -r 'select(.event == "bundle.uri.advertised") | .first_tip_oid' < session.json | head -1)
jq -c "select(.event == \"bundle.generated\" and .tip_oid == \"$TIP_OID\")" /var/log/bucketvcs/bucketvcs.json
```

---

## 9. Troubleshooting Matrix

Each entry names a symptom observable in metrics or logs, states the most
likely cause, and prescribes the operator action. Read §8 for the metric and
audit event vocabulary before using this section.

### 9.1 Bundle never generated

**Symptom.** `bundle_advertised_total{freshness="no_bundle"}` is the only
nonzero freshness value. `bundle_generated_total{outcome="success"}` is zero.

**Likely causes.**
- Maintenance has never run with the bundle phase enabled.
- Every maintenance run hit a `skipped_*` trigger reason — check
  `bundle_generated_total{outcome="noop"}` broken down by the `trigger_reason`
  label to see which skip reason dominates.

**Operator action.**
Run `bucketvcs maintenance --bundle-only --force` against one specific
repository to test whether the bundle phase works in isolation. If generation
still does not fire, inspect the `trigger_reason` label. If it is
`skipped_reachability_load_error`, the `.bvom` is corrupted or the
reachability index is missing; run `bucketvcs maintenance --force` (without
`--bundle-only`) to rebuild the full index pipeline, which will then make the
reachability data available to the bundle phase on the next run.

### 9.2 Clone is slow despite bundle being current

**Symptom.** `bundle_advertised_total{freshness="current"}` is nonzero, but
client-side clone times have not improved relative to the standard-fetch baseline.

**Likely cause.** The client did not opt in to bundle-uri. By default, git
clients require explicit configuration to use bundle-uri.

**Operator action.** Verify that the client is opting in:

```bash
git -c transfer.bundleURI=true clone <url>
```

Note the config key: `transfer.bundleURI` governs cold clones;
`fetch.bundleURI` governs incremental fetches (a separate setting). To enable
bundle-uri globally on all client machines in your deployment:

```bash
git config --global transfer.bundleURI true
```

If the client is already configured correctly and clones are still slow, check
whether `bundle_uri_served_bytes` is incrementing for proxied-mode deployments
(proving the bytes are flowing from the bundle), or check bucket-side logs for
direct-mode deployments.

### 9.3 Proxied-URL 403 with `reason="expired"`

**Symptom.** `proxied_url_token_invalid_total{reason="expired"}` is nonzero.
Clients report 403 errors when downloading bundles or packs via the proxied
`/_bundle/` or `/_pack/` routes.

**Likely causes.**
- Long-duration clones: the URL was minted at the start of the advertise
  request, and the client did not begin downloading until after the TTL
  elapsed (e.g., the client queued the clone behind other work).
- Clock skew between client and gateway causing premature expiry.
- Client retried after a delay that pushed the request past the TTL window.

**Operator action.** Increase `--proxied-url-bundle-ttl` (subject to the §5
rule: `TTL ≤ retention / 24`). Investigate clock skew if the error
rate correlates with specific client subnets; NTP drift of more than a few
seconds can cause premature expiry even at the default 4h TTL.

### 9.4 Proxied-URL 403 with `reason="invalid"`

**Symptom.** `proxied_url_token_invalid_total{reason="invalid"}` spikes,
typically correlated with a signing-key rotation event.

**Cause.** Rotating the signing key immediately invalidates all unexpired
tokens minted against the old key. The user-facing 403 response body says
"invalid token" for both `reason="invalid"` and `reason="kind_mismatch"` —
do not rely on the response body to distinguish them; use the metric label.

**Operator action.** Align future signing-key rotations with the longest
active TTL window (`--proxied-url-bundle-ttl`, default 4h): rotate after all
in-flight URLs from the old key have expired. If you must rotate immediately
(e.g., key compromise), accept a brief 403 spike for up to `max(bundle-ttl,
pack-ttl)` after the rotation. Alert on this metric at signing-key rotation
time; a spike followed by recovery is expected behavior.

### 9.5 Direct-URL 403 from bucket

**Symptom.** Clients report 403 errors when downloading bundles or packs
directly from the object-storage bucket. `proxied_url_token_invalid_total` is
zero (this is a direct-mode failure, not a gateway token failure).

**Likely causes.**
- TTL expired: same root cause as §9.3, but the signed URL is bucket-side.
- Backend rejected the signature: clock skew between the gateway VM and the
  bucket endpoint, signing-key revocation at the cloud provider, or a
  signature-version mismatch (AWS Sigv2 vs Sigv4 negotiation failures).

**Operator action.** Check bucket-side audit logs — S3 server access logs, R2
audit events, GCS data access logs, or Azure Blob storage analytics. The
gateway has no visibility into direct-mode failures; once a signed URL is
minted and handed to the client, the gateway is out of the loop. If clock skew
is suspected, verify the gateway host's system clock against an NTP source.

### 9.6 Pack-uri never advertised even though full clones happen

**Symptom.** `pack_uri_advertised_total` is zero or never increments even
during full-clone sessions. `bundle_uri_advertised_total` may be nonzero
(bundle-uri is working but pack-uri is not).

**Likely cause.** The packfile-uri gate requires two conditions:
`len(manifest.Packs) == 1` AND `manifest.Packs[0].PackChecksum != ""`.
Manifests written before pack-uri support have an empty `PackChecksum` field. If no maintenance run
has occurred since enabling pack-uri, `PackChecksum` remains empty and pack-uri
is gated off. Operator action: run `bucketvcs maintenance --force` once per
affected repo to backfill `PackChecksum`.

**Operator action.** Run `bucketvcs maintenance --force` once per affected
repository to backfill `PackChecksum`. Verify the backfill succeeded:

```bash
bucketvcs inspect-manifest --json | jq '.Packs[].PackChecksum'
```

The output should be a non-empty hash string. If the manifest still has
multiple packs after forcing maintenance, pack-uri will not fire regardless —
it requires single-pack shape, achieved by the maintenance repack pipeline
consolidating fragmented packs.

### 9.7 Sustained `freshness="stale"`

**Symptom.** `bundle_advertised_total{freshness="stale"}` is nonzero and
persistent — not a transient blip between maintenance runs, but a sustained
rate that does not clear when maintenance runs.

**Cause.** Maintenance is falling behind the commit-rate-vs-threshold curve.
The repository is receiving commits faster than the bundle generation + freshness
threshold permits. In `stale` state the bundle URL is NOT advertised — clients
fall through to standard fetch. The metric is still emitted (the dispatch
attempt is counted), making the stale rate visible for alerting. The incremental
fetch cost is borne in full until maintenance catches up.

**Operator action.** Two levers are available (not mutually exclusive):
- Tighten the maintenance cadence: schedule `bucketvcs maintenance` more
  frequently, down to hourly if needed.
- Raise `--bundle-warm-commits` or `--bundle-warm-age` if your repository's
  normal commit rate has genuinely increased (e.g., a new team onboarded and
  doubled the push frequency). This moves the `warm` → `stale` threshold
  further out, reducing the stale signal without changing actual bundle age.

Note that raising the warm threshold widens the gap clients must bridge with
an incremental fetch; there is a tradeoff between metric noise and actual
clone performance.

### 9.8 Multi-repo deployment, proxied mode, intermittent wrong-repo serves (historical)

**Status.** Fixed. This section is retained for operators upgrading from older
binaries who may have seen the symptom historically.

**Symptom (older binaries).** In a multi-repository deployment with
`--bundle-uri-mode=proxied`, some clients received bundle or pack content that
did not match the repository they cloned. The symptom was typically a Git
object consistency error on the client after the bundle download.

**Cause (older binaries).** The proxied handler was hash-keyed via a single-repo
resolver that mapped `/_bundle/<hash>` to a storage key without
per-repo discrimination. If two repositories produced a bundle with the same
content hash, the resolver picked one deterministically — which might not be
the requested repository.

**Fix.** URLs now embed `<tenant>/<repo>` segments (`/_bundle/<t>/<r>/<h>`,
`/_pack/<t>/<r>/<h>`), the proxied handler resolves the storage key from the
URL path directly (no hash-only lookup), and the HMAC token is bound to the
composite `tenant/repo/hash` so segment swaps fail token verification with
HTTP 403. No operator action required after upgrade — restart of
`bucketvcs serve` is sufficient. Verify with a multi-tenant proxied clone
against two distinct repositories that produce identically-hashed bundles.

---

## 10. Migration to Acceleration

### 10.1 Three-step migration sequence

**Step 1 — Deploy acceleration-capable binaries.** Replace the `bucketvcs` binary across
maintenance hosts and gateway hosts. Roll the deployment as you normally would;
the binary handles older manifests identically except for the
`PackChecksum` backfill.

**Step 2 — Run `bucketvcs maintenance --force` once per repository.** This
operation:

- Backfills `PackChecksum` for any single-pack repository where the field is
  empty (precondition for packfile-uri).
- Generates the first `full_default` bundle entry per repository.
- Updates the `.bvom` and `.bvcg` indexes to include bundle metadata.

For repositories managed by `--all-repos`, run:

```bash
bucketvcs maintenance --all-repos --force
```

This is a one-time pre-rollout step; subsequent runs use thresholds normally.

**Step 3 — Enable serve flags.** Add the following flags to `bucketvcs serve`
invocations and restart the gateway:

```
--bundle-uri-mode=auto
--pack-uri-mode=auto
--proxied-url-signing-key=/etc/bucketvcs/signing-key
--proxied-url-base=https://gateway.example.com
```

### 10.2 Rollback safety

- Bundle/pack manifest fields are `omitempty` — older binaries read
  acceleration-shaped manifests without error.
- `bundle.generated` and `bundle.retired` audit events are additive; older
  log consumers ignore them.
- To roll back, redeploy the old binary; manifest state remains compatible.
  The `BundleEntry` array stays in place (older binaries ignore it). On the
  next maintenance run with the old binary, bundle entries do not refresh —
  they become `stale` and eventually `retired` per the freshness rules, but
  they do not corrupt anything.

---

## 11. Forensics

### 11.1 Inspecting the manifest's bundle entries

```bash
bucketvcs inspect-manifest --store=s3://my-bucket?endpoint=... --repo=tenant/repo --json | jq '.Bundles'
```

Example output:

```json
[
  {
    "ID": "bundle-20260512-abc123",
    "Kind": "full_default",
    "BundleHash": "f3e2b1...",
    "BundleKey": "bundles/f3/e2b1...",
    "SidecarKey": "bundles/f3/e2b1....json",
    "TipOID": "9c8b7a...",
    "CoversManifestVersion": 42,
    "ByteSize": 12345678,
    "GeneratedAt": "2026-05-12T10:23:45Z"
  }
]
```

### 11.2 Bundle entry field meanings

- `ID` — unique identifier for the entry; matches `bundle.generated.bundle_id`
  audit field.
- `Kind` — `"full_default"` today. Other kinds (e.g. release-tag bundles)
  are out of scope.
- `BundleHash` — content hash of the bundle file; appears in proxied URLs as
  `/_bundle/<tenant>/<repo>/<BundleHash>` (the URL also embeds the
  `<tenant>/<repo>` segments).
- `BundleKey` — storage key of the bundle blob.
- `SidecarKey` — storage key of the JSON sidecar with per-OID bundle metadata.
- `TipOID` — Git OID of the ref tip the bundle was generated against; matches
  `bundle.generated.tip_oid` and `bundle.uri.advertised.first_tip_oid` for
  cross-event correlation.
- `CoversManifestVersion` — manifest version at bundle-generation time; used by
  the freshness state machine to detect staleness.
- `ByteSize` — bundle file size; matches `bundle.generated.byte_size`.

### 11.3 Post-incident grep cookbook

```bash
# All bundle.generated events for one repo in the last 24h:
jq -c 'select(.event=="bundle.generated" and .repo_id=="tenant/repo")' /var/log/bucketvcs/*.json

# All retired bundles paired with their successors:
jq -c 'select(.event=="bundle.retired") | {time, retired_id: .bundle_id, replaced_by, repo_id}' \
    /var/log/bucketvcs/*.json

# All token-validation failures with reason breakdown:
jq -c 'select(.metric_name=="proxied_url_token_invalid_total")' /var/log/bucketvcs/*.json \
    | jq -s 'group_by(.reason) | map({reason: .[0].reason, count: length})'
```

### 11.4 Tracing a specific client clone failure

For incidents involving a specific client clone failure, grep in this order:

1. Find the `bundle.uri.advertised` event for that client (filter by
   `first_tip_oid` if known).
2. Trace its `bundle.uri.advertised.via` field. If `direct`, the gateway has
   no further visibility — check bucket-side logs. If `proxied`, continue to
   step 3.
3. Find the corresponding `proxied.url.served` event(s). The `hash` field
   matches the advertised URL's path component.
4. If `proxied.url.served.status_code == 200` and `bytes_served` is less than
   the expected size: truncated serve, client disconnected mid-stream.
5. If no `proxied.url.served` event is found: the client never connected, OR
   the token failed validation — check `proxied_url_token_invalid_total` rates
   at the relevant timestamp.

---

## 12. Deferred Work

Bundle-uri + packfile-uri provide the minimum viable acceleration. Several
follow-up items are deferred; operators should plan around them:

- **Full bundle GC pipeline.** `bucketvcs gc` does not yet sweep retired bundle
  blobs. Retired bundles linger in object storage at zero serve cost but nonzero
  storage cost. The `DiscoverBundles` API exists; production wiring
  (mark-phase integration, sweep-record extension) is the deferred work. When it
  lands, the `bundle.retired.reason` vocabulary gains a `"gc_swept"` value.

- **Lazy-path short-circuit for packfile-uri.** Today, the gateway invokes
  `pack-objects` even when the entire pack is URI-eligible (the inline pack is
  then `--keep-pack`-elided down to near-empty). A future optimization will skip
  `pack-objects` entirely when `FullPackRequested` holds. Cost-relevant for
  full-clone-heavy deployments.

- **Legacy `gateway.Options` URI fields.** The closure-pattern API
  (`BundleURIBuildURL`, `PackURIBuildURL`) coexists with the legacy fields
  (`BundleURIMode`, `BundleURITTL`, `ProxiedURLSigningKey`, etc.). The legacy
  fields are deprecated and will be removed once the closure pattern has soaked
  in production.

- **Concurrent bundle-safety conformance.** The `RunPropertyBundleSafety`
  factory ships solo localfs green; three concurrent sub-cases
  (`push_during_bundle`, `bundle_during_compaction`, `sweep_after_retire`) ship
  as `t.Skip` stubs. These test the manifest's atomicity
  guarantees under concurrent bundle generation + push + GC sweep; their absence
  does not affect production correctness today.

- **`FreshnessResult` sub-reason preservation in audit logs.** Operators
  wanting to distinguish stale-by-age from stale-by-walkback must currently grep
  `FreshnessResult` log lines (debug-grade) rather than relying on
  `bundle.uri.advertised.freshness`. A future enhancement will surface the
  detail on the audit event.
