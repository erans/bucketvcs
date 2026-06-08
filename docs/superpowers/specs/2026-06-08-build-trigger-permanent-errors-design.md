# Build-trigger permanent-error classification

Date: 2026-06-08
Builds on: M30 build triggers (the delivery worker, `Deliverer` interface,
`recordResult` retry/backoff/dead-letter, metrics + audit) and M31 Azure build
triggers ([[m31 azure build triggers]]).

## 1. Goal

Today every failed build-trigger delivery — including unmistakable
*configuration* errors (a 401/404, an unknown connector, a non-http(s) URL) —
retries the full backoff schedule (1m → 30m → 2h → 12h, ~14.5h) before
dead-lettering. The worker has only one failure path: "retry until exhausted."

Add a **permanent-error** classification so non-transient failures dead-letter
**immediately** instead of churning for 14h. This gives operators fast feedback
on misconfiguration and stops pointless retries against a request that can never
succeed. It benefits all HTTP-based providers (generic, cloudbuild, azurewebhook,
azurepipelines) at once.

### 1.1 In scope

- A package-level `ErrPermanent` sentinel + `permanentf` wrapper helper.
- A one-branch change in `recordResult`: a permanent error routes to the
  existing `dead_letter` terminal state regardless of attempt count.
- Classification in the two HTTP deliverers:
  - `httpDeliverer` (generic / cloudbuild / azurewebhook): non-transient 4xx and
    the bad-URL-scheme error are permanent.
  - `azurePipelinesDeliverer`: non-transient 4xx and connector-resolution
    failures are permanent.
- Observability: a `reason={permanent,exhausted}` label on the dead-letter
  metric and audit event so operators can tell misconfiguration from an
  outage.

### 1.2 Out of scope (deferred)

- **`codeBuildDeliverer` classification.** AWS SDK typed-error mapping
  (`ResourceNotFoundException`, `AccessDeniedException`, throttling, …) is
  deferred; all CodeBuild errors remain retryable, exactly as today.
- **A distinct terminal status** (e.g. `failed_permanent`). We reuse
  `dead_letter` — no migration, and permanent dead-letters stay replayable via
  `bucketvcs build delivery replay` once the operator fixes the config.
- **New retry/backoff tuning** or per-trigger retry policy.

## 2. The signal (`internal/buildtrigger/deliver.go`)

```go
// ErrPermanent marks a delivery error that must NOT be retried (a configuration
// error or a non-transient 4xx). recordResult routes it straight to dead_letter
// regardless of attempt count.
var ErrPermanent = errors.New("permanent delivery error")

// permanentf wraps a formatted error so errors.Is(err, ErrPermanent) is true.
func permanentf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrPermanent}, a...)...)
}
```

(`errors` is added to the `deliver.go` import block.)

## 3. `recordResult` change (`internal/buildtrigger/worker.go`)

The error path currently is: `if row.Attempts >= maxAttempts { dead_letter } else { retry }`.

Change the condition so a permanent error also enters the dead_letter branch,
and thread a `reason` into the dead-letter emitters:

```go
maxAttempts := len(cfg.BackoffSchedule) + 1
permanent := errors.Is(err, ErrPermanent)
if permanent || row.Attempts >= maxAttempts {
	reason := "exhausted"
	if permanent {
		reason = "permanent"
	}
	// ... existing dead_letter UPDATE (status='dead_letter', last_status_code,
	//     last_error=truncErr(err.Error()), next_attempt_at=now) ...
	EmitFired(ctx, logger, row.Kind, "dead_letter")
	EmitAttemptDuration(ctx, logger, "dead_letter", durationMs)
	EmitDeadLetterMetric(ctx, logger, reason)
	EmitDeadLetter(ctx, logger, row.ID, row.TriggerID, row.Attempts, statusCode, reason)
	return
}
// ... unchanged retry branch ...
```

The delivered branch, the retry branch, `jitter`, and `truncErr` are unchanged.
A permanent error short-circuits to the existing dead_letter UPDATE at whatever
attempt count it occurred on (typically attempt 1).

`worker.go` does not currently import `errors` — add it to the import block.

## 4. Classification rules

Shared helper in `deliver.go`:

```go
// httpStatusPermanent reports whether an HTTP status is a permanent failure:
// any 4xx except 408 (Request Timeout) and 429 (Too Many Requests), which are
// transient and should be retried.
func httpStatusPermanent(code int) bool {
	return code >= 400 && code < 500 && code != 408 && code != 429
}
```

### 4.1 `httpDeliverer` (generic / cloudbuild / azurewebhook)

- Bad URL scheme → permanent:
  `return 0, permanentf("egress denied: trigger URL scheme must be http or https")`
- Non-2xx response:
  ```go
  if httpStatusPermanent(resp.StatusCode) {
  	return resp.StatusCode, permanentf("HTTP %d", resp.StatusCode)
  }
  return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
  ```
- Unchanged (remain retryable): mint failure, `client.Do` network error, render error.

### 4.2 `azurePipelinesDeliverer`

- Connector resolution failure (unknown connector / missing org_url or pat) is a
  config error → permanent:
  `return 0, permanentf("azure connector: %v", err)`
  (currently `fmt.Errorf("azure connector: %w", err)`).
- Non-2xx response: same `httpStatusPermanent` split as §4.1.
- Unchanged (remain retryable): mint failure, `conn.client.Do` network error,
  body-build error.

### 4.3 `codeBuildDeliverer`

Untouched. All errors remain retryable (deferred per §1.2).

A `statusCode 0` config error (bad scheme, unknown connector) is expressible as
permanent precisely because permanence rides on the wrapped error, not the
status code — which is why the central-classification-by-status-code alternative
was rejected.

## 5. Observability (`internal/buildtrigger/metrics.go`, `audit.go`)

- `EmitDeadLetterMetric(ctx, logger, reason string)` — gains a `reason` param;
  emits `build_trigger_deadletter_total{reason}` with `reason ∈ {permanent,
  exhausted}`.
- `EmitDeadLetter(ctx, logger, id, triggerID, attempts, statusCode, reason string)`
  — gains a `reason` attr on the `build.trigger.deadletter` audit event.
- `build_trigger_fired_total{kind,result}` keeps `result="dead_letter"` for
  **both** permanent and exhausted, so existing dashboards/aggregates that count
  total dead-letters are unaffected. The breakdown lives on the dead-letter
  metric/audit only.

These two signatures have a single call site each (the dead_letter branch in
`recordResult`); update those calls.

## 6. Testing (`internal/buildtrigger`)

1. **`httpStatusPermanent`** — table: 400/401/403/404/422 → true; 408/429 → false;
   500/502/503 → false; 200/302 → false.
2. **`recordResult` routing** — drive `recordResult` directly with a row at
   `Attempts: 1` (well under `maxAttempts`):
   - permanent error → row ends `status='dead_letter'`, `last_error` retained;
   - ordinary error at the same attempt count → row ends `status='pending'`
     (proves the default retry path is unchanged).
3. **`httpDeliverer` permanence** — httptest server returns 404 →
   `errors.Is(err, ErrPermanent)` true; 503 → false; `url: "ftp://x"` → permanent.
4. **`azurePipelinesDeliverer` permanence** — fake endpoint 401 → permanent;
   500 → retryable; unknown connector → permanent.
5. **Reason emission** — capture slog output (matching existing emitter-test
   style) and assert `build_trigger_deadletter_total` carries `reason` and the
   `build.trigger.deadletter` audit carries the `reason` attr, for both values.

No wire-shape golden changes (request bodies unchanged). No migration. No CLI
change.

## 7. Docs

`docs/operator-guides/build-triggers.md` — in the error-handling subsection,
note that configuration failures (4xx other than 408/429, bad URL scheme,
unknown connector) now dead-letter **immediately** with `reason=permanent`,
while transient failures (5xx, 408, 429, network errors, token-mint blips) still
retry on the 1m/30m/2h/12h schedule and dead-letter with `reason=exhausted`.
Mention the new `reason` label on `build_trigger_deadletter_total`. CodeBuild
errors remain retry-only for now. `docs/build-triggers.md` troubleshooting:
one-line note that Azure/HTTP 4xx now fail fast.
