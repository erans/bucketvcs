# Multi-region read replicas (operator guide)

This guide explains how to run regional read gateways that serve clone and fetch
traffic close to your developers while all writes continue to flow through a
single write-region gateway. It covers topology, provider replication setup,
freshness modes, auth requirements, what replicas refuse, monitoring, and doctor
checks.

Regional read replicas are activated by adding `--replica-of` to a `bucketvcs
serve` command. Everything else in the serve command line — `--store`, `--auth-db`,
`--addr`, observability flags — works exactly the same.

---

## 1. Topology

The model has three components:

1. **Canonical bucket** — the authoritative object-storage bucket in your
   write region. All pushed objects land here first.

2. **Write-region gateway** — a normal `bucketvcs serve` deployment backed by
   the canonical bucket. Developers push to this gateway (HTTPS or SSH). It
   runs the full stack: webhooks, web UI, OIDC, maintenance, GC.

3. **Regional gateways** — one `bucketvcs serve` per additional region, each
   backed by a provider-replicated copy of the canonical bucket and started
   with `--replica-of`. These gateways serve clone and fetch traffic
   locally; they refuse pushes with a clear error pointing developers at the
   write-region URL.

Provider replication carries all written objects from the canonical bucket to
regional buckets. Because Git pack files, index files, commit graphs, bundle
files, and LFS objects are content-addressed and immutable once written, the
replica gateway can serve them directly from the regional bucket the moment
they arrive — no coordination with the write region is needed. Only the tiny
per-repo root manifest changes on every push; that manifest is what the
replica monitors for freshness.

All pushes go to the write region. `git push` from every region hits the
same write-region gateway URL; the round-trip cost of a push is the same
regardless of where the developer is located. Clones and fetches — the
read-heavy traffic — are served locally.

### Worked serve example

```bash
# Start a replica gateway in EU, replicating from a canonical US bucket.
bucketvcs serve \
  --store s3://bucket-eu/prefix \
  --replica-of s3://bucket-us/prefix \
  --replica-mode strong-current \
  --write-region-url https://gw-us.example \
  --auth-db 'postgres://bv@central-host/bucketvcs_auth?sslmode=require' \
  --addr :8080
```

`--store` is the regional bucket. `--replica-of` is the canonical (write-region)
bucket. The regional gateway reads pack bytes locally from `--store`; it consults
`--replica-of` only for root manifests (strong-current mode) or lag sampling
(bounded-stale mode). `--write-region-url` is embedded in push-refusal messages
so developers know where to push.

---

## 2. Setting up provider replication

Provider replication is a cloud-side feature; you configure it once and the
cloud carries objects between buckets automatically. bucketvcs does not manage
replication — it only reads from the regional bucket.

**Important:** Provider replication is eventually consistent and UNORDERED.
A push that writes ten pack objects may deliver them to the regional bucket in
any order and with non-deterministic per-object delays. The replica gateway
handles this automatically: everything except the root manifest is immutable and
content-addressed, so objects that have already arrived can be served immediately
regardless of what order they arrived in. The root manifest is the only mutable
object; the replica's freshness logic resolves ordering races by falling back to
the canonical bucket for any manifest it cannot trust to be current.

### Amazon S3 Cross-Region Replication

Enable S3 CRR from the AWS console or CLI. The source bucket is in your write
region; the destination bucket is the regional bucket you pass to `--store`:

```
Source:      s3://bucket-us/prefix  (canonical, write region)
Destination: s3://bucket-eu/prefix  (regional replica)
```

Set the IAM replication role to allow `s3:ReplicateObject`,
`s3:ReplicateDelete`, and `s3:ReplicateTags`. BucketVCS never issues deletes
against object-key prefixes that CRR tracks (GC runs in the write region),
so replication of deletes is optional; replicating writes is the critical path.

Replication lag is typically seconds to a few minutes under normal conditions.
AWS publishes per-bucket replication metrics in CloudWatch; wire them to your
dashboards alongside `replica_lag_seconds` from the gateway.

### Google Cloud Storage dual-region and turbo replication

Create a **dual-region** or **multi-region** bucket that spans your write region
and at least one read region, or use independent regional buckets with
**Object Replication** (Storage Transfer Service). For the lowest-latency option
within a country, turbo replication (`rpo=ASYNC_TURBO`) guarantees 15-minute
replication SLO within a dual-region pair.

Point `--store` at the replica bucket (a standard regional bucket in the target
region) and `--replica-of` at the canonical bucket (in the write region). The
gateway reads manifests from the canonical bucket; actual object downloads come
from the regional bucket via GCS backend presigned URLs.

### Azure Blob Storage RA-GRS

Create a **Read-Access Geo-Redundant Storage (RA-GRS)** account in your primary
region. Azure automatically replicates all writes to a paired secondary region
and exposes it at a read-only secondary endpoint:
`https://<account>-secondary.blob.core.windows.net`.

Use the secondary endpoint as the `--store` URL for the replica gateway:

```bash
# Write-region gateway uses the primary endpoint (read-write).
--store azureblob://STORAGE_ACCOUNT.blob.core.windows.net/container

# Replica gateway uses the secondary endpoint (read-only via RA-GRS).
--store azureblob://STORAGE_ACCOUNT-secondary.blob.core.windows.net/container
--replica-of azureblob://STORAGE_ACCOUNT.blob.core.windows.net/container
```

Azure RA-GRS typically replicates within seconds; the Azure Portal shows
"Last Sync Time" per storage account.

### Cloudflare R2

Cloudflare R2 does not offer native cross-region replication. Regional gateways
backed by separate R2 buckets are **not supported** today — there is no mechanism
to synchronise objects between R2 buckets without running your own replication
layer. If you need geographic distribution with R2, consider fronting a single
R2 bucket with Cloudflare CDN for download acceleration while keeping a single
write-region gateway. The replica flags (`--replica-of`, `--replica-mode`) must
not be used with R2 regional buckets.

---

## 3. Freshness modes

The replica gateway can run in one of two modes, selected with `--replica-mode`.

### strong-current (default)

Every `git fetch` or `git clone` reads the root manifest from the canonical
(write-region) bucket before building the ref advertisement. This guarantees
that the developer always sees the very latest pushed refs, at the cost of one
small cross-region read per request. Pack bytes — the bulk of clone traffic — are
still served from the regional bucket, so clones are fast even though the ref
tip is fetched from the write region.

Strong-current is the right choice for interactive developer remotes. It
eliminates the class of problem where a developer pushes from one region and
immediately clones from another and sees stale refs.

### bounded-stale

The replica serves the root manifest from its regional bucket and only contacts
the canonical bucket for lag sampling, which happens on the
`--replica-check-interval` schedule (default: `--replica-lag-budget / 4`, floor
15 s). As long as the observed replication lag stays within `--replica-lag-budget`
(default 5 m, minimum 30 s), ref advertisement proceeds normally from the
regional bucket. When the lag budget is exceeded, the replica stops advertising
refs and returns 503 for fetch and clone requests; exact-key bundle, pack, and
LFS downloads continue to work so that any in-progress transfers can complete.
The replica recovers automatically when the next lag sample shows the region has
caught up.

**Conservative cold-start behavior:** a bounded-stale replica that has never
successfully reached the canonical bucket cannot prove that its lag is within
budget. It refuses ref advertisement immediately — "cannot determine replication
lag" — until it completes its first successful canonical read. This prevents a
newly-started replica with a stale regional bucket from appearing healthy.

Bounded-stale is the right choice for CI mirrors, read-heavy fleets, or
deployments where minimizing cross-region reads matters more than zero staleness.
Developers whose workflow always involves a push before a fetch (e.g. CI build
agents that run after a push event) rarely see a 503 because the lag budget
typically absorbs normal cloud replication delays.

**Mode guidance at a glance:**

| Use case | Recommended mode |
|---|---|
| Interactive developer remote (`git push` + `git pull`) | `strong-current` |
| CI build agents, read-only mirrors | `bounded-stale` |
| Latency-critical automation that reads immediately after push | `strong-current` |
| High-read-volume fleet (minimize canonical cross-region reads) | `bounded-stale` |

---

## 4. Auth

Replicas require the same central PostgreSQL auth database as every other
gateway node. SQLite and libSQL backends are refused at startup — they are
single-node storage and cannot be safely shared across regions.

```bash
# Replica must point at the same postgres instance (or a PG read replica)
# as the write-region gateway.
--auth-db 'postgres://bv@central-host/bucketvcs_auth?sslmode=require'
```

The replica performs token/key lookup and scope checks on every request — the
full read path through the auth layer — and pays one cross-region round-trip to
the central Postgres on each request unless you place a Postgres read replica in
the same region. For high-throughput deployments, pointing `--auth-db` at a
regional Postgres read replica reduces auth latency to a local round-trip:

```bash
--auth-db 'postgres://bv@regional-pg-replica.eu/bucketvcs_auth?sslmode=require'
```

Writes to the auth database (user creation, token rotation, policy changes)
always go through the write-region gateway or the `bucketvcs` CLI pointed at the
primary Postgres. The replica's auth path is read-only.

---

## 5. What replicas refuse or disable

**Pushes.** The replica returns a clear Forbidden error on any receive-pack
request (HTTPS or SSH) explaining that it is a read-only replica and, when
`--write-region-url` is set, naming the write-region gateway URL. Developers
who accidentally configure their remote for the replica URL can fix it with a
pushurl override:

```bash
# Keep fetch from the EU replica, send pushes to the US write-region gateway.
git config remote.origin.pushurl https://gw-us.example/acme/web.git
```

Or use a global insteadOf rule to redirect all push traffic for the replica
host to the write region:

```bash
git config --global url."https://gw-us.example/".pushInsteadOf "https://gw-eu.example/"
```

**LFS uploads.** The Batch API returns an error for upload operations. LFS
downloads work normally (object bytes come from the regional bucket). Proxied
LFS upload and verify URLs (`/_lfs/...`) minted by the write region are also
refused with a clean 403 if replayed against a replica — only downloads on that
path are served.

**LFS lock APIs.** All lock endpoints (create, list, verify, unlock) are refused;
manage locks through the write-region gateway. LFS downloads are unaffected.

**Web UI.** Passing `--ui=true` alongside `--replica-of` is a startup error.
The web UI requires read-write access to operate correctly; point it at the
write-region gateway.

**OIDC token exchange.** The `/_oidc/token` endpoint is not available on
replicas. Pipelines that need OIDC tokens should exchange against the write-region
gateway; the short-lived scoped tokens they receive work against any gateway
sharing the same auth database.

**Webhook delivery.** The webhook worker does not start on replicas. All
webhook delivery — for pushes, LFS uploads, repo events — originates from the
write-region gateway.

**GC and maintenance.** Never run `bucketvcs gc` or `bucketvcs maintenance`
against a replica bucket. GC sweeps objects by reachability; if the replica
bucket has diverged slightly from the canonical (objects in-flight via
replication), a sweep could delete objects that are still reachable but have not
yet been confirmed present in the regional bucket. Always run GC and maintenance
in the write region and let the results propagate via normal replication.

---

## 6. Monitoring

### /healthz vs /healthz/replica

`GET /healthz` is unchanged — it returns the plain text `ok` that your load
balancer uses as a liveness probe. Use it the same way you would for a normal
gateway.

`GET /healthz/replica` is available only on replicas (returns 404 on non-replica
gateways). It returns a JSON snapshot of the replica's current health:

```json
{
  "role":               "replica",
  "mode":               "strong-current",
  "repos_tracked":      42,
  "repos_lagging":      0,
  "max_lag_seconds":    1.8,
  "canonical_reachable": true
}
```

| Field | Meaning |
|---|---|
| `role` | Always `"replica"` |
| `mode` | `"strong-current"` or `"bounded-stale"` |
| `repos_tracked` | Repos that have been sampled at least once since startup |
| `repos_lagging` | Repos whose regional version is behind the canonical version |
| `max_lag_seconds` | Highest observed lag across all tracked repos (0 when none lagging) |
| `canonical_reachable` | `true` when at least one tracked repo has reached the canonical bucket within the lag budget; `true` also when no repos have been sampled yet (idle replica) |

Wire `/healthz/replica` into your monitoring dashboard for operational awareness.
The load balancer liveness probe should stay on `/healthz`.

### Metrics

Four metrics are emitted as structured slog records with `msg="metric"` and
`name=<metric>`:

| Metric | Type | Meaning |
|---|---|---|
| `replica_lag_seconds` | gauge | Maximum observed replication lag across all tracked repos at the time of the last canonical sample. 0 when no repos are lagging. |
| `replica_repos_lagging` | gauge | Count of repos currently behind the canonical version. |
| `replica_advert_unhealthy_total` | counter | Incremented each time a ref-advertisement request is refused because the replica is past its lag budget (bounded-stale mode only). |
| `replica_fallback_reads_total` | counter | Incremented each time a regional read misses and the gateway falls back to the canonical bucket (any object, not just manifests). Near-zero once a region is warm — in both modes; sustained non-zero means objects are being read before they replicate (cold or broken replication). |

`replica_fallback_reads_total` is the most operationally significant metric.
In both modes it should be near-zero once the region is warm; if it is steadily
non-zero over minutes, investigate whether provider replication is running.

### Audit events

Two structured audit events track per-repo health transitions:

| Event | Level | Meaning |
|---|---|---|
| `replica.repo.unhealthy` | WARN | A repo's lag exceeded the budget or became unmeasurable; ref advertisement is now refused for that repo. |
| `replica.repo.recovered` | INFO | The repo's lag has returned within budget; ref advertisement is restored. |

Both carry `tenant` and `repo` attributes; `replica.repo.unhealthy` additionally
carries a `reason` attribute. Transitions fire at most
once per state change — there is no per-request event flood when a repo remains
unhealthy.

---

## 7. Doctor

Run `bucketvcs doctor` with the same flags as your replica `serve` command to
validate the configuration without binding any ports:

```bash
bucketvcs doctor \
  --store s3://bucket-eu/prefix \
  --replica-of s3://bucket-us/prefix \
  --replica-mode strong-current \
  --write-region-url https://gw-us.example \
  --auth-db 'postgres://bv@central-host/bucketvcs_auth?sslmode=require'
```

Two checks are added when `--replica-of` is present:

| Check | What it verifies |
|---|---|
| `replica.canonical` | Opens the `--replica-of` store URL and confirms a List succeeds; fails if the canonical bucket is unreachable from this region |
| `config.replica` | `--replica-mode` is a valid value; `--replica-lag-budget` is >= 30 s; `--auth-db` resolves to a postgres URL (SQLite/libSQL rejected) |

The `storage.writable` check is skipped on replicas — the gateway never writes
to the regional bucket, so the probe write/delete would be misleading. All other
checks (`storage.reachable`, `authdb.open`, `authdb.migrations`, etc.) run
normally against the regional bucket and the central auth database.

---

## 8. Limits and deferred features

- **Per-tenant signing keys.** All proxied URL bundles/packs use the single
  `--proxied-url-signing-key`. Per-tenant keys are not available.
- **Subdomain routing.** All repos are served under a single domain on the
  replica gateway; subdomain-per-tenant routing is deferred.
- **SSH replica mode.** SSH clone and fetch work on replicas; SSH push is refused
  in the same way as HTTPS push.
- **Replica-to-replica fan-out.** A replica gateway always consults the canonical
  bucket for lag sampling or manifests; there is no replica-of-a-replica topology.
