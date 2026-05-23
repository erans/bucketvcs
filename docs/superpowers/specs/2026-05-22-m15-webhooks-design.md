# M15: Webhooks (spec §24) — Tier 1 broad design

**Status:** Design.
**Date:** 2026-05-22.
**Scope:** Implement spec §24 Tier 1 broad — durable asynchronous webhook delivery for push, LFS upload, LFS lock create/release, repo lifecycle (create/delete/rename), and policy.ref.rejected events.

## 1. Scope and goals

### 1.1 In scope

- Per-(tenant, repo) webhook endpoints with event-type filtering via bitmask
- Durable enqueue on the M4 authdb sqlite (migration 0006), at-least-once delivery
- In-process background worker: 1 goroutine, drains the queue, retries with backoff, dead-letters after 5 attempts over ~14.5h
- HMAC-SHA256 signing with timestamp prefix (`BucketVCS-Signature: t=<unix>,v1=<hex>`)
- Operator CLI: `bucketvcs webhook endpoint add/list/remove/enable/disable`, `bucketvcs webhook delivery list/show/replay`
- 8 typed events covering everything M11/M13/M13.3/M14 emit today as user-meaningful audit signals
- Crash recovery via `in_flight` row reclaim on worker boot
- Failure-closed receivers (4xx/5xx/timeout → retry), fail-open emitters (enqueue errors do NOT block pushes)
- 4 metrics + 6 audit events covering the full delivery lifecycle

### 1.2 Out of scope (deferred to follow-on milestones)

- Per-commit walk in `commits_summary` (Tier 1 sends count + head only)
- Storage binding change event (admin path not yet implemented)
- `webhook endpoint rotate-secret` CLI (operators rotate via remove + re-add)
- Permanent-failure short-circuit on specific 4xx codes (Tier 1 retries all non-2xx the same way)
- `webhook prune` / maintenance integration to age out delivered rows
- Per-endpoint retry policy overrides
- Webhook delivery ordering guarantee per (tenant, repo) — Tier 1 concurrency=8 means two pushes can land out-of-order; receivers use `timestamp` + `delivery_id` to order
- Multi-worker fan-out across `bucketvcs serve` instances (Tier 1 single-writer; sqlite `BEGIN IMMEDIATE` makes multi-instance safe but not horizontally scalable)
- mTLS to receivers, HTTP/2 pool tuning (defaults from `net/http`)

## 2. Architecture overview

```
internal/webhooks/
  service.go      Service.Create/List/Remove/Get/Enable/Disable for endpoints
  event.go        typed Event enum (8 events) + Payload structs per event
  enqueue.go      Service.Enqueue(ctx, event, payload) — INSERT into webhook_deliveries
  worker.go       background loop: SELECT due → POST → mark delivered/failed/retry/dead_letter
  sign.go         HMAC-SHA256 with t=<unix>,v1=<hex> signature
  metrics.go      4 emitters (delivery_total, queue_depth, attempt_duration_ms, endpoints_active)
  audit.go        6 audit events (delivered, failed, dead_letter, enqueue_failed,
                  endpoint_created, endpoint_removed)
  reclaim.go      one-shot startup reaper for stuck in_flight rows

cmd/bucketvcs/webhook.go   bucketvcs webhook endpoint add/list/remove/enable/disable
                           bucketvcs webhook delivery list/show/replay

cmd/bucketvcs/serve.go     constructs webhooks.Service when --auth-db is set;
                           starts worker via webhooks.StartWorker(ctx, svc, cfg);
                           threads svc into gateway.Options + sshd.Options
```

**Integration shape:** typed event taxonomy with explicit emitters at call sites. The webhook subsystem owns the event enum; existing audit slog emissions stay as-is. Only events operators would subscribe to become webhook events (this avoids "every internal-error becomes a webhook" failure mode).

**Wiring points (mirrors the M14 Policy + M13.5 Quota patterns):**
- `gateway.Options.Webhooks`, `sshd.Options.Webhooks` → threaded into `receivepack.EngineRequest.Webhooks`
- `lfs.Deps.Webhooks`, `lfsLocks.Deps.Webhooks`, `policy.Deps.Webhooks` — explicit `Enqueue` after the canonical operation succeeds
- `cmd/bucketvcs/repo.go` (create/delete/rename) — Enqueue after manifest write
- Every integration point no-ops on a nil `*webhooks.Service` so pre-M15 deployments are unchanged

## 3. Schema (migration 0006_webhooks.sql)

```sql
CREATE TABLE webhook_endpoints (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant          TEXT NOT NULL,
    repo            TEXT NOT NULL,
    url             TEXT NOT NULL,
    secret          TEXT NOT NULL,           -- raw HMAC key (base64url, 43 chars)
    event_mask      INTEGER NOT NULL,        -- bitfield over Event enum (see §4)
    active          INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1)),
    created_at      INTEGER NOT NULL,
    UNIQUE (tenant, repo, url),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);
CREATE INDEX webhook_endpoints_by_repo ON webhook_endpoints (tenant, repo, active);

CREATE TABLE webhook_deliveries (
    id                TEXT PRIMARY KEY,       -- UUID, also the X-BucketVCS-Delivery-ID header
    endpoint_id       INTEGER NOT NULL,
    event_type        TEXT NOT NULL,          -- "push", "lfs.upload", etc.
    payload_json      BLOB NOT NULL,          -- serialized once at enqueue
    status            TEXT NOT NULL,          -- "pending"|"in_flight"|"delivered"|"dead_letter"
    attempts          INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   INTEGER NOT NULL,
    last_attempt_at   INTEGER,                -- nullable until first attempt
    last_status_code  INTEGER,                -- nullable; HTTP status of last attempt
    last_error        TEXT,                   -- nullable; transport error or body excerpt (cap 512 bytes)
    created_at        INTEGER NOT NULL,
    delivered_at      INTEGER,                -- nullable; set when status flips to delivered
    FOREIGN KEY (endpoint_id) REFERENCES webhook_endpoints(id) ON DELETE CASCADE
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries (status, next_attempt_at)
    WHERE status = 'pending';

INSERT INTO schema_version (version, applied_at) VALUES (6, strftime('%s','now'));
```

**Design notes:**
- `secret` stored unencrypted; the worker needs it to sign. authdb sqlite is already the trust boundary (token hashes, lock owners, quotas, policy rules). Operators protect the file.
- `payload_json` materialized once at enqueue so the request body stays byte-identical across retries (idempotent delivery). The `BucketVCS-Signature` header is **recomputed per attempt** using the current `t`, so the value of the header changes across retries — this preserves the 5-min replay window over a 14h backoff (matches Stripe convention). Receivers verify by re-running HMAC over `<current t>.<body>`.
- `webhook_deliveries_due` is a partial index over only pending rows; the worker's "due" SELECT stays cheap even after months of `delivered` rows accumulate.
- `ON DELETE CASCADE` on both FKs: deleting a repo or endpoint cleans up dependent rows; the worker never trips over orphans.

## 4. Event taxonomy

```go
type Event uint64

const (
    EventPush              Event = 1 << 0  // "push"
    EventLFSUpload         Event = 1 << 1  // "lfs.upload"
    EventLFSLockCreated    Event = 1 << 2  // "lfs.lock.created"
    EventLFSLockReleased   Event = 1 << 3  // "lfs.lock.released"
    EventRepoCreated       Event = 1 << 4  // "repo.created"
    EventRepoDeleted       Event = 1 << 5  // "repo.deleted"
    EventRepoRenamed       Event = 1 << 6  // "repo.renamed"
    EventPolicyRefRejected Event = 1 << 7  // "policy.ref.rejected"
)

const EventMaskAll Event = EventPush | EventLFSUpload | EventLFSLockCreated |
    EventLFSLockReleased | EventRepoCreated | EventRepoDeleted | EventRepoRenamed |
    EventPolicyRefRejected
```

**Trigger sites:**

| Event                    | Trigger site                                                      |
| ------------------------ | ----------------------------------------------------------------- |
| `push`                   | After `BuildAndCommit` succeeds in receive-pack step 9            |
| `lfs.upload`             | After `verify` writes 200 (object accepted)                       |
| `lfs.lock.created`       | After `POST /locks` returns 201                                   |
| `lfs.lock.released`      | After `POST /locks/:id/unlock` returns 200                        |
| `repo.created`           | After `bucketvcs repo create` manifest write succeeds             |
| `repo.deleted`           | After `bucketvcs repo delete` confirms                            |
| `repo.renamed`           | After `bucketvcs repo rename` updates both repo rows              |
| `policy.ref.rejected`    | Inside step 8b at the existing `EmitRefRejected` site             |

Branch/tag create/delete from §24 are **derived from `push`** — receivers inspect `ref_updates[].old_oid == nullOID` (create) / `new_oid == nullOID` (delete). Keeps the taxonomy tight and the receiver semantics consistent.

## 5. Payload schema

### 5.1 Common envelope (every event)

```json
{
  "delivery_id": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": 1716393600,
  "event": "push",
  "tenant": "acme",
  "repo": "site",
  "actor": "alice"
}
```

### 5.2 Per-event bodies (merged into envelope)

```json
// push
{
  "tx_id": "tx-0001-abcd...",
  "manifest_version": 42,
  "storage_backend": "s3compat",
  "ref_updates": [
    {"refname": "refs/heads/main", "old_oid": "abc...", "new_oid": "def..."}
  ],
  "commits_summary": {"count": 3, "head": "def..."}
}

// lfs.upload
{ "oid": "sha256-hex", "size_bytes": 12345 }

// lfs.lock.created / lfs.lock.released
{ "lock_id": "ulid-or-int", "path": "assets/hero.psd", "ref": "refs/heads/main" }

// repo.created / repo.deleted
{}  // envelope alone suffices

// repo.renamed
{ "old_name": "old-site", "new_name": "site" }

// policy.ref.rejected
{
  "refname": "refs/heads/main",
  "matched_pattern": "refs/heads/*",
  "reason": "blocked_force_push",
  "old_oid": "abc...",
  "new_oid": "def..."
}
```

### 5.3 §24 SHOULD field coverage (push event)

| §24 field         | M15 location                              |
| ----------------- | ----------------------------------------- |
| org               | `tenant` in envelope                      |
| repo              | `repo` in envelope                        |
| actor             | `actor` in envelope                       |
| tx_id             | `tx_id` in push body                      |
| old refs          | `ref_updates[].old_oid`                   |
| new refs          | `ref_updates[].new_oid`                   |
| commits summary   | `commits_summary` (count + head, minimal) |
| manifest version  | `manifest_version`                        |
| storage backend   | `storage_backend`                         |

## 6. Worker loop

**One goroutine**, started at `bucketvcs serve` boot, lifetime tied to the server context.

```
Tick (every 1s, jittered to 0.8s..1.2s):
  1. BEGIN IMMEDIATE
  2. SELECT id, endpoint_id, event_type, payload_json, attempts
       FROM webhook_deliveries
       WHERE status='pending' AND next_attempt_at <= NOW()
       ORDER BY next_attempt_at
       LIMIT 32
  3. For each row: UPDATE status='in_flight', last_attempt_at=NOW(),
                   attempts=attempts+1
  4. COMMIT
  5. For each claimed row (parallel POST, bounded concurrency = 8):
       - Build body: serialize envelope + per-event body
       - Sign: BucketVCS-Signature: t=<unix>,v1=<hex>
       - Headers: Content-Type: application/json,
                  X-BucketVCS-Delivery-ID: <delivery.id>,
                  X-BucketVCS-Event: <event_type>,
                  User-Agent: bucketvcs-webhook/M15
       - POST with 10s timeout
  6. For each result:
       - 2xx → UPDATE status='delivered', delivered_at=NOW()
       - Non-2xx / timeout / network error → backoff:
           if attempts >= 5 → status='dead_letter'
           else next_attempt_at = NOW() + backoff(attempts)
                status='pending'
         Store last_status_code + last_error (excerpt of body, capped 512 bytes)
```

**Backoff schedule** (5 attempts total — attempt 1 fires on the first tick after enqueue, then 4 retries):

| Transition         | Delay before next attempt |
| ------------------ | ------------------------- |
| attempt 1 → 2      | 1 minute                  |
| attempt 2 → 3      | 30 minutes                |
| attempt 3 → 4      | 2 hours                   |
| attempt 4 → 5      | 12 hours                  |
| attempt 5 fails    | dead_letter (no more retries) |

Quick first retry (1m) recovers transient connectivity blips; the long tail (12h before the final attempt) survives a half-day receiver outage. Total elapsed from enqueue to dead_letter: ~14.5h. Each interval gets ±25% jitter to prevent thundering herds against a returning receiver.

**Concurrency:**
- Single-writer claim via sqlite `BEGIN IMMEDIATE` — safe across multiple `bucketvcs serve` instances pointed at the same authdb
- Within one worker, up to 8 in-flight POSTs at once; tuneable via `--webhook-worker-concurrency` flag, default 8
- POST timeout: 10s; tuneable via `--webhook-worker-timeout`, default 10s

**Shutdown:** server ctx cancel → worker stops claiming new rows; waits up to 15s for in-flight POSTs (with their own 10s timeout); rows that don't complete are left in `status='in_flight'`.

**Crash recovery (reclaim.go):** on worker start, one-shot reaper marks all `in_flight` rows with `last_attempt_at < NOW() - 60s` as `pending` (with their existing `attempts` count). This covers serve crashes mid-delivery. The 60s threshold is well above the 10s POST timeout + 15s drain window, so it can't fire on rows that are currently being delivered by another active worker.

## 7. Signing

**Header format:** `BucketVCS-Signature: t=1716393600,v1=<64-char-hex>`

**Computation:** `v1 = hex(HMAC-SHA256(secret, "<t>.<body>"))` where `<t>` is the unix timestamp (string), `<body>` is the raw JSON request body bytes.

**Receiver verification (in operator guide). The HMAC key is the secret text as UTF-8 bytes — operators do NOT base64url-decode it (matches Stripe convention):**
```python
import hmac, hashlib, time
def verify(secret_text: str, body_bytes: bytes, header: str) -> bool:
    parts = dict(p.split('=', 1) for p in header.split(','))
    t = int(parts['t'])
    if abs(time.time() - t) > 300:   # 5-min replay window
        return False
    signed = f"{t}.".encode() + body_bytes
    expected = hmac.new(secret_text.encode(), signed, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, parts['v1'])
```

**Secret material:**
- Generated server-side at endpoint creation: 32 random bytes from `crypto/rand`, base64-url encoded (43 chars)
- Returned **once** in the CLI output (stdout) and stored in `webhook_endpoints.secret` for the worker
- Subsequent `webhook endpoint list` shows `secret_preview: <first 6 chars>...` — never the full secret
- Rotation deferred (operators rotate via remove + re-add)

## 8. Operator CLI

```
bucketvcs webhook endpoint add     --auth-db=<path> --tenant=<t> --repo=<r>
                                   --url=<https://...> --events=<csv|all>
bucketvcs webhook endpoint list    --auth-db=<path> --tenant=<t> --repo=<r>
                                   [--format=text|json]
bucketvcs webhook endpoint remove  --auth-db=<path> --id=<N>
bucketvcs webhook endpoint enable  --auth-db=<path> --id=<N>
bucketvcs webhook endpoint disable --auth-db=<path> --id=<N>

bucketvcs webhook delivery list    --auth-db=<path> [--endpoint-id=<N>]
                                   [--status=pending|in_flight|delivered|dead_letter]
                                   [--since=<RFC3339>] [--format=text|json]
bucketvcs webhook delivery show    --auth-db=<path> --id=<uuid>
bucketvcs webhook delivery replay  --auth-db=<path> --id=<uuid>
```

**`--events` flag accepts:**
- CSV of canonical names: `push,lfs.upload,policy.ref.rejected`
- Shortcut `all` (sets all bits to `EventMaskAll`)
- Shortcut groups: `lfs.*` (lfs.upload + lfs.lock.*), `repo.*` (repo.created/deleted/renamed)

**JSON output:** NDJSON (one record per line, no enclosing array, empty emits nothing) — matches the M13.5 quota / M14 policy CLI convention.

**`endpoint add` output** (the only secret-exposing command):
```
endpoint_id=42  tenant=acme  repo=site  url=https://hooks.example.com/bucketvcs  events=push,lfs.upload
secret=k8s_NQpV4o7e3xR2mJ5tYqH9cL1pZ6wB0aDfGsHiUjOk   # store this now — it will not be shown again
```

**Subcommand semantics:**
- `enable` flips `active=1`; `disable` flips `active=0` (deliveries stop enqueuing immediately; existing pending rows continue to drain)
- `remove` is a hard delete (`webhook_deliveries` cascades)
- `delivery replay` resets `status='pending'`, `attempts=0`, `next_attempt_at=NOW()` on a `dead_letter` row
- `delivery show` prints the full payload, all status transitions, last error/status code

**Exit codes:**
- 0 ok
- 1 operational error (db unreachable, etc.)
- 2 usage error (bad flags, malformed URL, unknown event name)

## 9. Observability

### 9.1 Metrics

| Name                              | Labels                | Type      |
| --------------------------------- | --------------------- | --------- |
| `webhooks_delivery_total`         | outcome               | counter   |
| `webhooks_queue_depth`            | status                | gauge     |
| `webhooks_attempt_duration_ms`    | outcome               | histogram |
| `webhooks_endpoints_active`       | (none)                | gauge     |

Outcomes for `delivery_total`: `delivered`, `failed_retry`, `dead_letter`, `enqueue_failed`.
Statuses for `queue_depth`: `pending`, `in_flight`.
Queue-depth + endpoints-active gauges sampled every worker tick (~1s).

### 9.2 Audit events

| Event                          | Trigger                                                  | Key attrs |
| ------------------------------ | -------------------------------------------------------- | --------- |
| `webhooks.delivered`           | 2xx received                                             | delivery_id, endpoint_id, event_type, attempts, duration_ms |
| `webhooks.failed`              | Non-2xx/timeout but still retrying                       | + status_code, error, next_attempt_at |
| `webhooks.dead_letter`         | Attempt 5 failed                                         | + total_attempts, final_status_code |
| `webhooks.enqueue_failed`      | Queue INSERT failed (fail-open path)                     | tenant, repo, event_type, error |
| `webhooks.endpoint_created`    | CLI add succeeded                                        | endpoint_id, tenant, repo, url, events |
| `webhooks.endpoint_removed`    | CLI remove succeeded                                     | endpoint_id, tenant, repo |

## 10. Failure modes

| Failure                                    | Behavior                                                  |
| ------------------------------------------ | --------------------------------------------------------- |
| Receiver returns 5xx                       | Retry per backoff schedule, dead_letter after 5 attempts  |
| Receiver returns 4xx                       | Same as 5xx — no special "permanent" treatment (Tier 1)   |
| Receiver times out (>10s)                  | Treated as failed attempt, retry                          |
| Receiver TLS error / connection refused    | Treated as failed attempt, retry                          |
| authdb sqlite locked at enqueue            | Caller logs + audits `webhooks.enqueue_failed`; push succeeds (fail-open) |
| authdb sqlite locked at worker SELECT      | Tick logs error, retries next tick (~1s)                  |
| Worker goroutine panic                     | recovered + logged; tick restarts (`defer recover`)       |
| `bucketvcs serve` crashes mid-delivery     | `in_flight` rows older than 60s reclaimed as `pending` on next boot |
| Receiver responds 2xx but with body error  | Treated as delivered — operator's responsibility to validate downstream |
| Endpoint URL is plain `http://`            | Allowed but logs a warning at `endpoint add` time (no TLS = no replay protection beyond the 5-min window) |
| Endpoint URL DNS fails                     | Same as connection refused — retry per schedule           |

## 11. Optionality and rollout

- `webhooks.Service` is optional everywhere — nil at every integration point produces a no-op (matches M14 Policy / M13.5 Quota pattern)
- `bucketvcs serve` constructs the service only when `--auth-db` is set; without it, every emitter call short-circuits
- Pre-M15 deployments require zero migration (migration 0006 is additive; old `bucketvcs serve` binaries silently skip the new tables)
- Operators opt in per (tenant, repo) by running `bucketvcs webhook endpoint add` — no global enable flag

## 12. Testing strategy

### 12.1 Unit (internal/webhooks/*_test.go)
- `Service.Create/List/Remove/Get/Enable/Disable` against a real on-disk sqlite seeded with a `repos` row
- Event mask: `parseEvents("push,lfs.upload") == EventPush|EventLFSUpload`, `parseEvents("all")` round-trips
- `Sign(secret, t, body)` matches a reference `hmac.New(...)` computation
- Backoff schedule: `nextAttemptAt(attempts=N)` returns the documented values within jitter bounds
- Reclaim: in_flight rows older than 60s are returned to pending with attempts intact; younger rows are left alone
- Payload serialization: each event type serializes to a JSON shape matching §5.2
- `Enqueue` rejects nil Service (no-op), unknown event types, invalid (tenant, repo) FKs

### 12.2 Worker (internal/webhooks/worker_test.go)
- `httptest.Server` with configurable response (status, latency, body) — assert delivery state transitions
- 2xx → status=delivered, delivered_at set, attempts=1
- 500 retried up to attempts=5, then dead_letter
- Timeout via `httptest.Server` with deliberate 11s delay — counted as failed attempt
- Concurrency: 8 in-flight at once, 9th waits — assert via mock receiver counter
- Idempotency: receiver receives same `X-BucketVCS-Delivery-ID` across retries
- Shutdown: ctx cancel during in-flight POST → row stays in_flight; reaper at next start reclaims it

### 12.3 Integration (cmd/bucketvcs)
- Real `bucketvcs serve` with `--auth-db` + a sidecar `httptest`-style receiver
- Register endpoint via `bucketvcs webhook endpoint add`, push to the server, assert receiver sees `push` event with correct payload + valid signature
- Force receiver to 500 3x, then 200 — assert webhooks.delivered audit eventually fires
- `webhook delivery list` shows the row history; `webhook delivery show` includes payload + status timeline

### 12.4 Smoke (scripts/m15-webhook-smoke.sh)
- End-to-end script: start `bucketvcs serve`, start a local Go receiver as sidecar, register endpoint, perform git push + LFS upload + LFS lock create/release + policy reject, assert receiver logged all 5 events with valid signatures
- Final assertion: `M15_WEBHOOK_SMOKE_OK`
- Uses `--lfs=false` only for the policy-rejection sub-test (matches M14 smoke convention); other sub-tests need LFS enabled

## 13. Open questions

None. All decisions captured above.

## 14. Acceptance criteria

- All 8 events fire at their documented trigger sites
- HMAC signature verifies with the documented receiver code
- Worker drains the queue, retries failed deliveries per schedule, dead-letters after 5
- Crash recovery reclaims stuck in_flight rows on boot
- CLI subcommands work end-to-end against a real authdb
- `scripts/m15-webhook-smoke.sh` passes
- `go test ./...` and `go vet ./...` clean
- All prior smokes (M11/M12/M12.1/M13/M13.3/M13.4/M13.5/M14) still pass
- 4 metrics + 6 audit events emitted at the documented sites
- Operator guide covers: CLI, signature verification, retry semantics, observability, alerting recommendations, failure modes
