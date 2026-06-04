# Webhooks (operator guide)

This guide covers the webhooks feature. It explains how to register and manage endpoints with the `bucketvcs webhook` CLI, how the gateway enqueues and delivers events, how to verify signatures on the receiver side, and how to read the metrics + audit events.

The companion design document is `docs/superpowers/specs/2026-05-22-m15-webhooks-design.md`; the implementation plan is `docs/superpowers/plans/2026-05-22-m15-webhooks.md`.

Production readiness summary:

- Per-repo endpoints with event-mask filtering — **shipped**.
- HMAC-SHA256 signed payloads with replay protection — **shipped**.
- At-least-once delivery with bounded retries + dead-letter — **shipped**.
- Single-writer worker per gateway process (in-process sqlite queue) — **shipped**.
- Manual operator replay via CLI — **shipped**.
- Cross-tenant fan-out, configurable per-endpoint retry policy, alternate signing schemes — **deferred**.
- Schema 5 → 6 (`0006_webhooks.sql`) is forward-only and applied by the existing `RunMigrations`.

---

## 1. Overview

The webhook subsystem provides outbound delivery layered on top of the existing audit emitters. Each tenant/repo can register one or more endpoints subscribing to a subset of the canonical event taxonomy (`push`, `lfs.upload`, `lfs.lock.created`, `lfs.lock.released`, `repo.created`, `policy.ref.rejected`, …). Gateway operations that succeed and need to be advertised externally call `webhooks.Service.Enqueue`, which writes a `webhook_deliveries` row keyed off the matching endpoints.

A background worker (`StartWorker`) loops over `pending` deliveries every second, claims a batch, POSTs the JSON payload to each endpoint URL, and updates the row to `delivered`, back to `pending` (with a backoff), or to `dead_letter` once the retry budget is exhausted. Each attempt re-signs the payload with the current Unix timestamp so the BucketVCS-Signature header always fits inside a tight replay window on the receiver.

Webhook enqueue is **fail-open**: an INSERT failure cannot abort a push, an LFS verify, or a CLI repo registration. The originating operation completes; the gateway emits `webhooks.enqueue_failed` as a structured audit event so the operator can detect drift.

What ships:

- `internal/webhooks` package with `Service.Create/List/Remove/Enable/Disable`, `Service.Enqueue`, `webhooks.Sign`, and the `StartWorker` loop.
- `bucketvcs webhook endpoint add | list | remove | enable | disable` CLI.
- `bucketvcs webhook delivery list | show | replay` CLI for diagnostics + manual recovery.
- 6 enqueue call sites already wired in receivepack, LFS handlers, LFS locks, and `bucketvcs repo register`.
- 4 metrics + 6 audit events documented in §6.
- Migration `0006_webhooks.sql`.

What does not ship (full list in §11):

- `repo.deleted` and `repo.renamed` events are reserved in the taxonomy but have no CLI emission today (`bucketvcs repo` has no `delete` or `rename` subcommand).
- The `storage_backend` field on `PushPayload` is wired through the API but currently always empty — set to the live backend in a future release.
- Per-endpoint retry policy / per-endpoint backoff override (every endpoint uses the global schedule).
- Multi-process worker / horizontal scale-out (single writer per gateway process; see §11 for the upgrade path).
- HMAC-SHA256 is the only signing scheme; v2/ed25519 is reserved in the header format but not emitted.
- TLS-CA pinning per endpoint, mTLS to receivers.

---

## 2. CLI quickstart

All `webhook` subcommands act on rows in the gateway's authdb (`bucketvcs.db`). They require `--auth-db <path>`; the CLI fails with usage error 2 if it is omitted.

### 2.1 Register an endpoint

```
bucketvcs webhook endpoint add \
    --auth-db=<path> \
    --tenant=<tenant> \
    --repo=<repo> \
    --url=<https://example.com/hook> \
    --events=<csv|all|lfs.*|repo.*>
```

`--events` accepts:

- the literal `all` (every event in the taxonomy);
- the shortcuts `lfs.*` (lfs.upload + lfs.lock.*) and `repo.*` (repo.created + repo.deleted + repo.renamed);
- a comma-separated list of canonical names (`push,lfs.upload`).

On success, the command prints **the secret exactly once**:

```
endpoint_id=12  tenant=acme  repo=site  url=https://...  events=all
secret=NQpV4o7e3xR2mJ5tYqH9cL1pZ6wB0aDfGsHiUjOk-7e   # store this now — it will not be shown again
```

The secret is a 32-byte random value rendered as 43 base64url chars (no padding, no prefix). Subsequent `list` calls only show a six-character `secret_preview` followed by `...`. If the operator loses the secret, the only remedy is `endpoint remove` + `endpoint add` (which mints a new secret); the receiver must be updated in lockstep.

URL validation requires an `https://` (or `http://` for development) scheme and a non-empty host. Path, port, and query string are passed through unmodified.

### 2.2 List endpoints

```
bucketvcs webhook endpoint list --auth-db=<path> --tenant=acme --repo=site
```

Default format is `text` (one line per row, key=value). Pass `--format=json` for NDJSON — one JSON object per line, no enclosing array. An empty result emits zero bytes on stdout in JSON mode and a single `(no endpoints)` line in text mode.

### 2.3 Remove / enable / disable

```
bucketvcs webhook endpoint remove  --auth-db=<path> --id=<N>
bucketvcs webhook endpoint disable --auth-db=<path> --id=<N>
bucketvcs webhook endpoint enable  --auth-db=<path> --id=<N>
```

`disable` keeps the row but suppresses future enqueues. `remove` deletes the row outright (and detaches any pending deliveries via the FK ON DELETE). `enable` reverses `disable`. All three are idempotent — running `disable` on an already-disabled endpoint exits 0.

### 2.4 Re-registering a deleted repo

When `bucketvcs repo delete` removes a (tenant, repo), its webhook endpoints are NOT cascade-deleted (we keep them so the final `repo.deleted` event has somewhere to drain). If you later re-register the same (tenant, repo) via `bucketvcs repo register`, the orphan endpoints become active subscriptions for the new repo — your new repo will fire webhooks at receivers configured for the deleted one, signed with the deleted endpoint's secret.

**Recommended pre-registration check:**

```
bucketvcs webhook endpoint list --auth-db=auth.db --tenant=acme --repo=site
```

Remove any unwanted carry-overs:

```
bucketvcs webhook endpoint remove --auth-db=auth.db --id=<N>
```

A future release will add an automated webhook-prune sweep for endpoints whose (tenant, repo) has no matching `repos` row AND zero pending deliveries.

### 2.5 Inspect and replay deliveries

```
bucketvcs webhook delivery list --auth-db=<path> [--endpoint-id=<N>] \
    [--status=pending|in_flight|delivered|dead_letter] \
    [--since=<RFC3339>] [--limit=<N>] [--format=text|json]

bucketvcs webhook delivery show   --auth-db=<path> --id=<uuid>
bucketvcs webhook delivery replay --auth-db=<path> --id=<uuid>
```

`--limit` defaults to 500 and is capped at 10000 (values above the cap are silently clamped). When a result set hits the cap, narrow the query with `--since`, `--endpoint-id`, or `--status` rather than raising `--limit` past 10000.

`replay` resets a `dead_letter` (or any other non-`in_flight`) row back to `pending` with `attempts=0` and a `next_attempt_at=NOW`. The original `delivery_id` is preserved so the receiver sees the same `X-BucketVCS-Delivery-ID` header — receivers MUST deduplicate by that value (see §4). Replay on an `in_flight` row is refused (a worker is mid-delivery); wait until it terminates (delivered or dead_letter) before retrying.

---

## 3. Event reference

Every payload embeds a `CommonEnvelope`:

```json
{
  "delivery_id": "uuid-v4",
  "timestamp":   1747934201,
  "event":       "push",
  "tenant":      "acme",
  "repo":        "site",
  "actor":       "alice"
}
```

Per-event fields layered on top:

### push

Triggered after receive-pack commits all accepted ref updates (atomic batch + non-atomic per-ref). Emitted once per push, regardless of how many refs the push updated.

Extra fields:

| Field | Type | Notes |
|---|---|---|
| `tx_id` | string | The repo-tx ID associated with the push. |
| `manifest_version` | int64 | Manifest version after the push. |
| `storage_backend` | string | Always empty currently (reserved for future). |
| `ref_updates` | `[]{refname, old_oid, new_oid}` | One entry per accepted update; `old_oid == "0000…"` means create; `new_oid == "0000…"` means delete. |
| `commits_summary.count` | int | Number of accepted ref updates (NOT commits walked). |
| `commits_summary.head` | string | OID of the push's head ref, best-effort. |

### lfs.upload

Triggered when an LFS verify returns 200 (object stored, size matches batch claim). One event per OID.

| Field | Type | Notes |
|---|---|---|
| `oid` | string | LFS object hex OID (sha256). |
| `size_bytes` | int64 | Verified size in bytes. |

`actor` is empty for LFS verify (the verify endpoint runs unauthenticated within a verify-token-bound session — operators correlate via tenant/repo/oid).

### lfs.lock.created / lfs.lock.released

Triggered when the LFS locks API records a successful POST `/locks` or a successful `verify+unlock` cycle. One event per lock state transition.

| Field | Type | Notes |
|---|---|---|
| `lock_id` | string | The lock's stable ID. |
| `path` | string | The file path. |
| `ref` | string (optional) | Ref the lock is scoped to, if any. |

### policy.ref.rejected

Triggered when the receive-pack ref-protection guard rejects a ref update (force-push or deletion blocked by a `protected_refs` rule). Emitted once per rejected ref; not emitted for accepted refs.

| Field | Type | Notes |
|---|---|---|
| `refname` | string | The ref the client tried to update. |
| `matched_pattern` | string | Which `protected_refs` row matched. |
| `reason` | string | `protected-branch:no-deletion` / `protected-branch:no-force-push` / `internal-error`. |
| `old_oid` | string | OID seen on the wire. |
| `new_oid` | string | OID the client wanted to write. |

### repo.created

Triggered when `bucketvcs repo register` registers a tenant/repo for the first time. Re-registration of an existing repo is a no-op and does NOT enqueue.

Carries only the envelope — no extra fields.

### repo.deleted / repo.renamed (reserved)

Listed in the taxonomy and `--events=all` will match them, but no CLI exists to emit them today. They are reserved for when `bucketvcs repo delete` / `bucketvcs repo rename` ship.

---

## 4. Signature verification

Every POST carries:

```
Content-Type: application/json
User-Agent: bucketvcs-webhook/1
X-BucketVCS-Event: push
X-BucketVCS-Delivery-ID: <uuid>
BucketVCS-Signature: t=1747934201,v1=4b3f…
```

The signature is HMAC-SHA256 over the **exact request body bytes** with a colon-separated timestamp prefix:

```
v1 = hex(HMAC_SHA256(secret, "<t>." + body_bytes))
```

The `t=<unix>` value MUST be within a 5-minute (±300 s) window of the receiver's wall clock — otherwise the request is a replay attempt.

**The worker re-signs with the current `t` on every retry.** That means the same delivery (same `delivery_id`, same `body`) carries a different `BucketVCS-Signature` on attempts 2/3/4/5. Receivers MUST verify the signature against the timestamp in the header, not their own clock at first sight, and MUST NOT cache prior signatures for a given delivery.

### 4.1 Python receiver snippet

```python
import hashlib, hmac, time
from flask import Flask, abort, request

SECRET = b"NQpV4o7e3xR2mJ5tYqH9cL1pZ6wB0aDfGsHiUjOk-7e"
TOLERANCE_S = 300

app = Flask(__name__)

@app.post("/hook")
def hook():
    raw = request.get_data()  # exact bytes — DO NOT request.json
    header = request.headers.get("BucketVCS-Signature", "")
    parts = dict(p.split("=", 1) for p in header.split(",") if "=" in p)
    t_s = parts.get("t")
    v1 = parts.get("v1")
    if not t_s or not v1:
        abort(400, "missing t/v1")
    t = int(t_s)
    if abs(time.time() - t) > TOLERANCE_S:
        abort(400, "stale signature")
    expected = hmac.new(SECRET, f"{t}.".encode() + raw, hashlib.sha256).hexdigest()
    if not hmac.compare_digest(expected, v1):
        abort(400, "bad signature")
    # Process here.
    return "", 204
```

Three gotchas this snippet handles:

1. `request.get_data()` returns the **raw body bytes** before Flask's JSON parser touches them; using `request.json` would re-encode whitespace and break HMAC.
2. `hmac.compare_digest` is timing-safe; a `==` compare is not.
3. The wall-clock check is on the `t=` value, not on the request arrival time, because we explicitly support clock skew between gateway and receiver up to the tolerance.

### 4.2 Go receiver snippet

```go
import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "io"
    "net/http"
    "strconv"
    "strings"
    "time"
)

const secret = "NQpV4o7e3xR2mJ5tYqH9cL1pZ6wB0aDfGsHiUjOk-7e"
const tolerance = 300 * time.Second

func verify(r *http.Request) (bool, error) {
    body, err := io.ReadAll(r.Body)
    if err != nil {
        return false, err
    }
    sig := r.Header.Get("BucketVCS-Signature")
    var tStr, v1 string
    for _, p := range strings.Split(sig, ",") {
        k, v, _ := strings.Cut(p, "=")
        switch k {
        case "t":
            tStr = v
        case "v1":
            v1 = v
        }
    }
    t, err := strconv.ParseInt(tStr, 10, 64)
    if err != nil {
        return false, err
    }
    if d := time.Since(time.Unix(t, 0)); d > tolerance || d < -tolerance {
        return false, nil
    }
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(strconv.FormatInt(t, 10)))
    mac.Write([]byte("."))
    mac.Write(body)
    want, _ := hex.DecodeString(v1)
    return hmac.Equal(mac.Sum(nil), want), nil
}
```

### 4.3 Replay protection

The 5-minute window bounds an attacker's ability to replay a captured payload, but receivers should ALSO deduplicate by `X-BucketVCS-Delivery-ID` — the gateway re-sends the same `delivery_id` for every retry of one event, and the operator can additionally fire `bucketvcs webhook delivery replay` to re-send a dead-lettered delivery. Receivers that don't deduplicate will process the same logical event multiple times.

---

## 5. Retry semantics

Every endpoint shares the same retry policy:

| Attempt | Trigger | Delay before next | Cumulative wall time |
|---|---|---|---|
| 1 | initial post | + ~1 min | ~0 |
| 2 | retry | + ~30 min | ~1 min |
| 3 | retry | + ~2 hours | ~31 min |
| 4 | retry | + ~12 hours | ~2.5 hours |
| 5 | final retry | dead_letter | ~14.5 hours |

Backoff intervals carry ±25 % uniform jitter to avoid synchronised thundering-herd retries when a popular receiver flaps. The schedule is hard-coded in `DefaultWorkerConfig()`; per-endpoint overrides are deferred.

A 2xx response advances the delivery to `delivered` (terminal). Any non-2xx (including 1xx, 3xx, 4xx, 5xx), connect error, TLS handshake failure, or 10-second HTTP timeout counts as a failed attempt. After the 5th failure, the row moves to `dead_letter` and stays there until an operator inspects and (optionally) replays.

To replay a dead-lettered delivery:

```
bucketvcs webhook delivery replay --auth-db=<path> --id=<uuid>
```

This resets `attempts=0`, `next_attempt_at=NOW`, `status=pending`. The worker picks it up on the next tick (≤1 s). Same `delivery_id`, same `body`, fresh signature.

---

## 6. Observability

### 6.1 Metrics

Four metrics, all emitted as structured slog records with `msg="metric"` and a `name=<metric>` attr to distinguish them from audit events:

| Metric | Type | Labels | Emission point |
|---|---|---|---|
| `webhooks_delivery_total` | counter | `outcome={delivered,failed_retry,dead_letter,enqueue_failed}` | once per attempt outcome |
| `webhooks_attempt_duration_ms` | histogram (point sample) | `outcome=...` | once per attempt, measures wall time including DNS/connect/TLS/wait |
| `webhooks_queue_depth` | gauge | `status={pending,in_flight,dead_letter}` | reserved for periodic gauge emission (see §6.4) |
| `webhooks_endpoints_active` | gauge | none | reserved for periodic gauge emission |

The point-sample shape matches the policy + LFS metrics; a scraping sidecar can aggregate by `(name, outcome)` from the raw slog stream.

### 6.2 Audit events

Six structured events:

| Event | Level | Key attrs | Cardinality |
|---|---|---|---|
| `webhooks.delivered` | INFO | delivery_id, endpoint_id, event_type, attempts, duration_ms | one per successful delivery |
| `webhooks.failed` | WARN | delivery_id, endpoint_id, event_type, attempts, status_code, error, next_attempt_at | one per non-terminal failure |
| `webhooks.dead_letter` | ERROR | delivery_id, endpoint_id, event_type, total_attempts, final_status_code | one per retry-budget exhaustion |
| `webhooks.enqueue_failed` | ERROR | tenant, repo, event_type, error | one per fail-open enqueue failure |
| `webhooks.endpoint_created` | INFO | endpoint_id, tenant, repo, url, events | reserved for future CLI hook (the CLI currently writes the row but does not call the emitter) |
| `webhooks.endpoint_removed` | INFO | endpoint_id, tenant, repo | reserved for future CLI hook |

The two `endpoint_*` emitters are exported and tested; wiring them into the `webhook endpoint add` / `remove` CLI is a small follow-up and intentionally deferred to keep the emitter API stable.

### 6.3 Quick log filter

```
# Successful deliveries in the last hour:
journalctl -u bucketvcs --since "1 hour ago" | grep "webhooks.delivered"

# Dead-lettered events that need operator attention:
journalctl -u bucketvcs --since "24 hours ago" | grep "webhooks.dead_letter"

# Delivery counter aggregated by outcome:
journalctl -u bucketvcs --since "1 hour ago" \
  | grep 'name=webhooks_delivery_total' \
  | sed -E 's/.*outcome=([^ ]+).*/\1/' | sort | uniq -c
```

### 6.4 Queue-depth gauge

`EmitQueueDepthGauge` is exported but not currently invoked by the worker on a timer. Operators who want a `webhooks_queue_depth` time series should either:

1. Sample the gauge from a sidecar via `SELECT status, COUNT(*) FROM webhook_deliveries GROUP BY status`; or
2. Add a goroutine wrapper in their `bucketvcs serve` fork that calls `webhooks.EmitQueueDepthGauge` every 30 s. The function is stable.

The queue-depth + endpoints-active samplers are deferred to a follow-up because the right cadence depends on the scraper.

---

## 7. Failure modes

### 7.1 Receiver returns 4xx / 5xx / timeout

The worker treats all non-2xx outcomes identically: failed attempt, back to `pending` with backoff. A receiver that returns 410 (Gone) cannot signal "give up" today — operators who decommission a receiver should `webhook endpoint disable` or `remove` the row; otherwise the delivery wastes retry budget for the full 14.5 hours.

### 7.2 Receiver TLS error

A failed TLS handshake (cert expired, hostname mismatch, untrusted CA) counts as a failed attempt with `status_code=0`. The error message goes into `last_error` (truncated at 512 chars). Per-endpoint TLS pinning is not available; deploy your receivers behind a public CA or wire an internal CA bundle into the gateway's system trust store.

### 7.3 Gateway crashes mid-delivery

A row stuck in `in_flight` for ≥ 60 s is reclaimed back to `pending` (without incrementing `attempts`) by the `Reclaim` sweep. The sweep runs both at worker boot AND periodically from the worker loop (every ~60 ticks ≈ 1 minute at the default 1 s tick interval). This catches both gateway crashes (caught at next-boot reclaim) and in-process failures where `recordResult`'s UPDATE failed after a context cancel or sqlite-busy (caught by the periodic sweep, no restart required).

A delivery that timed out at 9.9 s and crashed before the result row was written will be re-attempted as if the attempt never started — receivers WILL see duplicates here and MUST deduplicate by `delivery_id`.

The 60 s threshold is tuned against the 10 s HTTP timeout; lowering it risks declaring a slow attempt dead while it's still inflight.

### 7.4 Enqueue failure (fail-open)

If `webhooks.Enqueue` returns an error (sqlite write failure, FK violation), the originating operation does NOT abort. The error is logged via `webhooks.enqueue_failed` and the operation reports success to the client. Operators MUST treat repeated `enqueue_failed` events as a P1 — drift between gateway state and webhook state means consumers will silently miss events.

### 7.5 Endpoint URL points at a public sink with no auth

The signature is the only authentication. There is no IP allowlist or mTLS. Receivers SHOULD verify the signature on every request and reject unsigned / mis-signed payloads with 400.

---

## 8. Recommended alerts

Prometheus-style (translate the slog stream to a metrics backend first):

```
# Dead-letter rate — typically zero. Any > 0 is operator-visible.
ALERT WebhooksDeadLetters
  IF rate(webhooks_delivery_total{outcome="dead_letter"}[5m]) > 0
  FOR 5m

# Enqueue drift — fail-open path. Means consumers are missing events.
ALERT WebhooksEnqueueFailures
  IF rate(webhooks_delivery_total{outcome="enqueue_failed"}[5m]) > 0
  FOR 1m

# Queue backlog — pending should drain in <2 ticks under healthy load.
ALERT WebhooksBacklog
  IF webhooks_queue_depth{status="pending"} > 1000
  FOR 10m

# In-flight stuck — should be near zero except during a delivery burst.
ALERT WebhooksStuckInFlight
  IF webhooks_queue_depth{status="in_flight"} > 50
  FOR 15m
```

The `webhooks_attempt_duration_ms{outcome="delivered"}` p99 is a useful SLO target — most healthy receivers respond in <200 ms; values consistently above 2 s suggest the receiver is doing heavy work synchronously and should move to a queue.

---

## 9. Wiring reference

`bucketvcs serve` constructs the webhook service exactly once at boot:

```go
authStore := /* sqlitestore opened from --auth-db */
webhookSvc := webhooks.New(authStore.DB())
// … pass webhookSvc to receivepack engine, LFS handlers, locks handler …
go webhooks.StartWorker(ctx, webhookSvc, webhooks.DefaultWorkerConfig())
```

The constructor is one-arg (`webhooks.New(db *sql.DB)`); the migration applies idempotently on first contact. `DefaultWorkerConfig()` ships the production-safe defaults (1 s tick, batch=32, concurrency=8, schedule=1m/30m/2h/12h, jitter=25 %, reclaim=60 s).

`WorkerConfig.Logger` defaults to `slog.Default()`. Operators who want a dedicated handler for webhook events (separate JSON file, Datadog, …) can wire a child logger:

```go
cfg := webhooks.DefaultWorkerConfig()
cfg.Logger = slog.New(jsonHandler).With("component", "webhooks")
go webhooks.StartWorker(ctx, webhookSvc, cfg)
```

The 6 call sites — receivepack push, receivepack policy-reject, LFS verify, LFS lock create, LFS lock release, `bucketvcs repo register` — each invoke `webhookSvc.Enqueue(ctx, EventX, tenant, repo, actor, payload)`. The service handles matching enabled endpoints against the event mask and writing one `webhook_deliveries` row per match.

---

## 10. Troubleshooting

### 10.1 Receiver reports "stale signature" but I just pushed

Check the receiver's clock. The 5-minute window is bilateral — receiver clock drifted >5 min into the future or the past will reject otherwise valid signatures. Synchronise with NTP or widen the tolerance temporarily for debugging.

### 10.2 Receiver gets duplicate deliveries for the same event

By design — `at-least-once` semantics. Two common causes:

1. The receiver returned 2xx but the worker treated it as failure (response body parse error in the gateway). Check `webhooks.failed` audit lines for the actual `status_code`.
2. The gateway crashed mid-attempt; the `Reclaim` sweep re-queued the row. Both are unavoidable in an at-least-once system.

Deduplicate by `X-BucketVCS-Delivery-ID`.

### 10.3 Signature verification fails but the rest of the request looks fine

Three usual culprits:

1. The receiver is reading the request body twice (e.g. logging middleware that drains, then handler that re-parses). The first read consumes the buffer. Capture the raw bytes once at the very top of the handler.
2. JSON-aware middleware reformats the body (re-indenting, sorting keys). HMAC is over the **exact bytes**. Read the raw stream before any parser sees it.
3. The secret was copied with leading/trailing whitespace.

### 10.4 `webhooks.enqueue_failed` keeps firing

Indicates the gateway's sqlite (authdb) is unhealthy — usually disk full, schema-gate failure, or a stale FK. Run `bucketvcs inspect-manifest` and check `sqlite_master` schema_version. If the migration didn't apply on first boot, the table won't exist and every enqueue fails. Re-run with `--auth-db=<same path>` and confirm the boot log shows `migration 0006_webhooks applied`.

### 10.5 An endpoint is firing into the wrong tenant

Endpoints are keyed by `(tenant, repo)`, not by URL. An operator who created two endpoints with the same URL across tenants will see both fire — that's intentional fan-out. To stop one without affecting the other, use `webhook endpoint disable --id=<N>` against the specific row.

---

## 11. Limits

- **Single-writer worker.** One `StartWorker` per gateway process. Horizontal scale-out (multiple gateway processes against the same authdb) is not safe; the claim transaction relies on sqlite's serialised writer.
- **No CLI for `repo.deleted` / `repo.renamed`.** The events are reserved in the taxonomy and `--events=all` will subscribe to them, but no code path emits them today.
- **`storage_backend` field is empty.** Wired through the payload struct but always `""` currently — populated in a future release when receivepack knows the live backend.
- **Per-endpoint retry policy is not configurable.** Every endpoint shares the global schedule.
- **No webhook secret rotation.** To rotate, remove + re-add the endpoint; receivers must be updated in lockstep.
- **No per-endpoint event-mask edit.** To change the subscription list, remove + re-add (the secret will rotate too).
- **`endpoint_created` / `endpoint_removed` audit events are emitter-only.** The CLI does not call them today — listed in §6 for forward-compat.
- **Queue-depth and endpoints-active gauges are emitter-only** until an operator wires a sampler.
- **No backpressure.** If the worker can't drain the queue, deliveries accumulate in `pending` indefinitely. Operators should alert on backlog (§8) and either scale the receiver or temporarily `endpoint disable` the slow one.
- **HMAC-SHA256 only.** The header reserves `v1=` for SHA256; `v2=` is parked for future schemes.
- **No egress allowlist on endpoint URLs (SSRF surface).** Endpoint registration was operator-CLI-only when this was designed, but the web UI now lets any repo-admin register endpoint URLs — including ones that resolve to internal addresses (link-local metadata services, RFC 1918 hosts, `localhost`). The worker will dutifully POST to whatever it is given. Operators who delegate repo-admin to semi-trusted users SHOULD front the worker's egress with network policy (firewall rules, an egress proxy, or a locked-down network namespace) so it cannot reach internal targets. A built-in egress deny-list is a known deferral; until then, network-level isolation is the only control.

The companion design document (§9 "Out of scope") enumerates the future path: multi-process worker via a leader-elected sqlite row, per-endpoint backoff schedules, signed-URL artefacts in the payload, and cross-tenant fan-out.

---

## 12. Webhook delivery retention

The `webhook_deliveries` table grows monotonically: every push, lock, lfs.upload, repo.created/deleted/renamed, and policy.ref.rejected adds one row per subscribed endpoint, and rows in terminal states (`delivered`, `dead_letter`) are never removed by the worker itself. On a high-traffic monorepo this table can reach millions of rows in weeks and degrade the worker's claim query (which scans `(status, next_attempt_at)`).

`bucketvcs webhook prune` sweeps terminal-state rows past a retention window.

### 12.1 What it does

```
bucketvcs webhook prune \
    --auth-db=/var/lib/bucketvcs/auth.db \
    [--delivered-older-than=720h] \
    [--dead-letter-older-than=2160h] \
    [--dry-run] \
    [--actor=<string>]
```

- Two independent retention windows. `--delivered-older-than` keys off `delivered_at` (the time the receiver returned 2xx). `--dead-letter-older-than` keys off `last_attempt_at` (the time the worker gave up after backoff exhaustion).
- Defaults: 720h (30 days) for delivered, 2160h (90 days) for dead-letter. Pick longer windows if your incident response needs the audit trail; pick shorter if disk pressure is the concern.
- `pending` and `in_flight` rows are NEVER touched — only the two terminal states.
- Minimum retention is 1h on both flags. Sub-hour windows are rejected at the CLI surface to prevent a fat-finger from sweeping rows the receiver might still be processing in-flight.
- `--dry-run` reports the counts that WOULD be deleted without mutating the table. Use it before tightening retention windows in production.
- `--actor` overrides the default attribution (`$USER` if set, else `"unknown"`). Cron jobs should pass an explicit actor (e.g. `--actor=ops-cron`) so the audit trail names the responsible automation.

### 12.2 Recommended cron schedule

Run nightly at off-peak. The DELETE is a single statement per status (two statements total) and holds the sqlite writer for the duration, so wall-clock cost scales with the number of rows being deleted, not the table size.

```
# /etc/cron.d/bucketvcs-webhook-prune
17 3 * * * bucketvcs /usr/local/bin/bucketvcs webhook prune \
    --auth-db=/var/lib/bucketvcs/auth.db \
    --actor=ops-cron
```

A first-time prune against a never-pruned table can delete millions of rows in one shot. If you anticipate this, run a `--dry-run` first to gauge wall-clock cost, and consider a one-time low-retention sweep (`--delivered-older-than=24h`) on a maintenance window before switching to nightly defaults.

### 12.3 Observability

Each invocation emits:

- One `webhooks.pruned` audit event with `delivered_deleted`, `dead_letter_deleted`, both cutoffs, `dry_run`, and `actor` attributes. Dry-runs emit the audit with `dry_run=true` and the WOULD-DELETE counts.
- Two `webhook_deliveries_pruned_total` metric samples (one per outcome label: `delivered`, `dead_letter`). The total over time gives you a backlog-growth dashboard: spikes correlate with traffic spikes or endpoint outages that ended in dead-letter.

Both signals land in the CLI process's slog stderr — the prune CLI runs out-of-process from the gateway, so its observability flows through whatever sink the operator wires for command output (systemd journal, file redirection, etc.). Production deployments should redirect prune CLI stdout/stderr to the same log aggregator as `bucketvcs serve`.

### 12.4 What it does NOT do

- It does not VACUUM the database. Sqlite reclaims space lazily via the freelist; the disk file size stays roughly flat after a large delete. Run `VACUUM` manually if you need the bytes back (note: VACUUM rewrites the entire file and requires roughly 2x free space).
- It does not respect FK cascades from `webhook_endpoints`. If you `webhook endpoint remove`, the FK cascade already drops the dependent delivery rows synchronously. Prune is for rows whose endpoint is still active but whose delivery is past retention.
- It does not deduplicate. Each `(tenant, repo, event)` push that matched N endpoints created N rows; the prune walks them independently.

---

## 13. Repo rename: auth-only semantics

```
bucketvcs repo rename <tenant>/<old-name> <new-name> \
    --auth-db=/var/lib/bucketvcs/auth.db \
    --store=<storage-url> \
    [--actor=<string>]
```

The rename CLI updates **auth.db only**. Storage keys at `tenants/<tenant>/repos/<old-name>/...` are NOT migrated by this command — the operator is responsible for moving them out of band (`aws s3 mv`, `gsutil mv`, etc.) AND for rewriting the absolute key references in the manifest body (`pack_key`, `idx_key`, index keys all contain the old prefix).

### 13.1 What the CLI does atomically

A single sqlite transaction over the `RenameRepo` helper updates every FK-bearing dependent table plus a small set of repo-scoped tables without FKs:

- `repos(tenant, name)` — the primary row
- `repo_permissions` — user grants (FK)
- `ssh_keys` — per-repo deploy keys (FK; column names: `scope_tenant`, `scope_repo`)
- `protected_refs` — ref protection rules (FK)
- `protected_paths` — path protection rules (FK)
- `hooks` — pre/post-receive hook rules (FK)
- `webhook_endpoints` — endpoint registrations (FK)
- `lfs_locks` — active locks (no FK to `repos`; updated for value consistency)
- `webhook_deliveries` — historical webhook delivery rows are joined by `endpoint_id` FK; they follow the endpoint row automatically (no separate UPDATE)

NOT touched by `RenameRepo`:

- `quotas` — keyed by `tenant` only (`PRIMARY KEY tenant` from migration 0004), no `repo` column. Same-tenant rename leaves the tenant-wide byte counter unchanged.

The transaction runs with `PRAGMA defer_foreign_keys = TRUE` so intermediate states (rows pointing at a row that hasn't been renamed yet) are tolerated until the COMMIT.

### 13.2 Refusal conditions

The CLI refuses (exit 1, no auth mutation, no webhook delivery) if any of these is true:

- Source `<tenant>/<old-name>` does not exist in auth.db (`not_found` outcome metric).
- Destination `<tenant>/<new-name>` already exists in auth.db (`collision_auth` outcome metric).
- Destination storage prefix `tenants/<tenant>/repos/<new-name>/` is non-empty — the CLI does a `List(prefix, MaxKeys=1)` probe to detect leftover blobs (`collision_storage` outcome metric).
- The `<new-name>` argument contains `/` or `\` — cross-tenant rename is not supported (`cross_tenant` outcome metric). A future "transfer" verb will support cross-tenant motion separately.

Successful rename emits the `ok` outcome metric.

### 13.3 Webhook ordering — at-least-once before commit

The `repo.renamed` webhook is enqueued **BEFORE** the auth transaction runs. This matches the `repo.deleted` precedent: endpoints scoped to `(tenant, old-name)` are still present in `webhook_endpoints` when `Enqueue` resolves subscribers — the rename would move those rows to the new name in the same transaction, and a worker reading `webhook_endpoints` AFTER the rename would not match the old `(tenant, repo)` payload key.

Consequence: if the auth transaction subsequently fails (sqlite I/O error, constraint violation that wasn't caught by pre-check), the webhook still delivers. Operators must treat `repo.renamed` as an **at-least-once** signal and reconcile by querying current state (`bucketvcs repo list` against the destination name) rather than assuming the rename committed.

If enqueue itself fails, a `webhooks.enqueue_failed` audit fires and the rename proceeds fail-open — the auth transaction still runs. The rename is not blocked by webhook-subsystem health.

### 13.4 Storage migration runbook

After `bucketvcs repo rename <tenant>/<old> <new>` succeeds:

1. Stop the gateway (or rely on the localfs single-writer lock during the auth-rename step on localfs; cloud backends require a controlled cutover).
2. Move the storage tree: `aws s3 mv s3://bucket/tenants/<tenant>/repos/<old>/ s3://bucket/tenants/<tenant>/repos/<new>/ --recursive` (or backend-equivalent).
3. Rewrite absolute path references in the manifest body — every `pack_key`, `idx_key`, and `indexes.*.key` field currently embeds the old prefix. The simplest operator-side approach is to download `tenants/<tenant>/repos/<new>/manifest/root.json`, sed-replace `tenants/<tenant>/repos/<old>/` to `tenants/<tenant>/repos/<new>/`, and PUT it back atomically.
4. Restart the gateway. Push/clone against the new name should now succeed; the old name returns 404 from the auth layer.

A future release may automate this storage step via a `bucketvcs storage rename` helper that respects the manifest indirection. For now it is an operator runbook.

### 13.5 Limits

- Same-tenant only. Cross-tenant motion requires a separate verb.
- No undo. The rename is committed when the sqlite transaction commits; reverse direction must be done with a second `repo rename` call.
- LFS quotas are per-tenant (`quotas(tenant)` from migration 0004 has no `repo` column), so a same-tenant rename leaves the tenant-wide byte counter unaffected. If you complete an out-of-band storage migration afterwards, run `bucketvcs quota reconcile --tenant=<tenant>` to correct for any tenant-level drift that accumulated during the cutover.
- The `webhook_endpoints` row for the old name is migrated to the new name; subscribers continue to receive events under their existing endpoint ID. The endpoint secret is NOT rotated. If you want the new name to surface as a different endpoint, `webhook endpoint rotate-secret --id=<N>` after the rename.
