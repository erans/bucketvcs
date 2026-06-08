# Build-Trigger Permanent-Error Classification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make non-transient build-trigger delivery failures (4xx except 408/429, bad URL scheme, unknown connector) dead-letter immediately instead of retrying the full ~14h backoff, with a `reason={permanent,exhausted}` breakdown on the dead-letter metric and audit.

**Architecture:** A package sentinel `ErrPermanent` + `permanentf` wrapper. The two HTTP deliverers wrap non-transient errors with `permanentf`; `recordResult` checks `errors.Is(err, ErrPermanent)` and routes such errors to the existing `dead_letter` state regardless of attempt count. The `Deliverer` interface, the worker call site, and `codeBuildDeliverer` are unchanged. No DB migration, no CLI change, no wire-shape change.

**Tech Stack:** Go, stdlib `errors`/`fmt`, `net/http/httptest` for deliverer tests, the existing build-trigger worker-integration test harness (`newTestSvc`, `countByStatus`, `waitUntil`), `log/slog` capture for the reason-emission test.

**Spec:** `docs/superpowers/specs/2026-06-08-build-trigger-permanent-errors-design.md`

---

## File Structure

**Modify:**
- `internal/buildtrigger/deliver.go` — add `ErrPermanent`, `permanentf`, `httpStatusPermanent`; classify in `httpDeliverer.Deliver`.
- `internal/buildtrigger/azurepipelines.go` — classify in `azurePipelinesDeliverer.Deliver` (connector + non-2xx).
- `internal/buildtrigger/metrics.go` — `EmitDeadLetterMetric` gains a `reason` param.
- `internal/buildtrigger/audit.go` — `EmitDeadLetter` gains a `reason` param.
- `internal/buildtrigger/worker.go` — `recordResult` permanent routing + `reason` threading; add `errors` import.
- `docs/operator-guides/build-triggers.md`, `docs/build-triggers.md` — error-handling notes.

**Test files (modify/append):**
- `internal/buildtrigger/deliver_test.go` — `httpStatusPermanent` table + httpDeliverer permanence.
- `internal/buildtrigger/azurepipelines_test.go` — azure permanence.
- `internal/buildtrigger/worker_test.go` — permanent-routing + reason-emission integration test (adds a slog capture helper).

---

## Task 1: Permanence primitives in deliver.go

**Files:**
- Modify: `internal/buildtrigger/deliver.go`
- Test: `internal/buildtrigger/deliver_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/buildtrigger/deliver_test.go`:

```go
func TestHTTPStatusPermanent(t *testing.T) {
	cases := map[int]bool{
		400: true, 401: true, 403: true, 404: true, 422: true,
		408: false, 429: false,
		500: false, 502: false, 503: false,
		200: false, 302: false,
	}
	for code, want := range cases {
		if got := httpStatusPermanent(code); got != want {
			t.Errorf("httpStatusPermanent(%d)=%v, want %v", code, got, want)
		}
	}
}

func TestPermanentf_IsErrPermanent(t *testing.T) {
	err := permanentf("HTTP %d", 404)
	if !errors.Is(err, ErrPermanent) {
		t.Fatal("permanentf result should satisfy errors.Is(err, ErrPermanent)")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("error message lost detail: %q", err.Error())
	}
	if errors.Is(fmt.Errorf("plain"), ErrPermanent) {
		t.Error("a plain error must not be ErrPermanent")
	}
}
```

Ensure `deliver_test.go` imports `errors` and `fmt` (it already imports `strings`). Add them to its import block if missing.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run 'HTTPStatusPermanent|Permanentf' -v`
Expected: FAIL — `undefined: httpStatusPermanent` / `undefined: permanentf` / `undefined: ErrPermanent`.

- [ ] **Step 3: Add the primitives**

In `internal/buildtrigger/deliver.go`, add `"errors"` to the import block (alphabetically, before `"fmt"`). Then append to the file:

```go
// ErrPermanent marks a delivery error that must NOT be retried (a configuration
// error or a non-transient 4xx). recordResult routes it straight to dead_letter
// regardless of attempt count.
var ErrPermanent = errors.New("permanent delivery error")

// permanentf wraps a formatted error so errors.Is(err, ErrPermanent) is true.
func permanentf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrPermanent}, a...)...)
}

// httpStatusPermanent reports whether an HTTP status is a permanent failure:
// any 4xx except 408 (Request Timeout) and 429 (Too Many Requests), which are
// transient and should be retried.
func httpStatusPermanent(code int) bool {
	return code >= 400 && code < 500 && code != 408 && code != 429
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/buildtrigger/ -run 'HTTPStatusPermanent|Permanentf' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/deliver.go internal/buildtrigger/deliver_test.go
git commit -m "feat(buildtrigger): ErrPermanent sentinel + permanentf + httpStatusPermanent"
```

---

## Task 2: Add `reason` to dead-letter emitters (no behavior change)

**Files:**
- Modify: `internal/buildtrigger/metrics.go`
- Modify: `internal/buildtrigger/audit.go`
- Modify: `internal/buildtrigger/worker.go` (the one dead_letter call site)

This is a pure refactor: add the `reason` parameter and pass `"exhausted"` at the existing call site, preserving current behavior and keeping the build green.

- [ ] **Step 1: Update `EmitDeadLetterMetric`**

In `internal/buildtrigger/metrics.go`, replace the `EmitDeadLetterMetric` function with:

```go
// EmitDeadLetterMetric logs one build_trigger_deadletter_total{reason} sample.
// reason is one of: permanent, exhausted.
func EmitDeadLetterMetric(ctx context.Context, logger *slog.Logger, reason string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "build_trigger_deadletter_total"),
		slog.String("reason", reason),
		slog.Int("value", 1),
	)
}
```

- [ ] **Step 2: Update `EmitDeadLetter`**

In `internal/buildtrigger/audit.go`, replace the `EmitDeadLetter` function with:

```go
func EmitDeadLetter(ctx context.Context, logger *slog.Logger,
	deliveryID, triggerID string, totalAttempts, finalStatusCode int, reason string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelError, "build.trigger.deadletter",
		slog.Bool("audit", true),
		slog.String("event", "build.trigger.deadletter"),
		slog.String("delivery_id", deliveryID),
		slog.String("trigger_id", triggerID),
		slog.Int("total_attempts", totalAttempts),
		slog.Int("final_status_code", finalStatusCode),
		slog.String("reason", reason),
	)
}
```

- [ ] **Step 3: Update the call site in `recordResult`**

In `internal/buildtrigger/worker.go`, inside the existing dead_letter branch (`if row.Attempts >= maxAttempts {`), change the two emitter calls to pass `"exhausted"`:

```go
		EmitFired(ctx, logger, row.Kind, "dead_letter")
		EmitAttemptDuration(ctx, logger, "dead_letter", durationMs)
		EmitDeadLetterMetric(ctx, logger, "exhausted")
		EmitDeadLetter(ctx, logger, row.ID, row.TriggerID, row.Attempts, statusCode, "exhausted")
		return
```

- [ ] **Step 4: Verify the package builds and all tests pass (no behavior change)**

Run: `go build ./internal/buildtrigger/ && go test ./internal/buildtrigger/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/metrics.go internal/buildtrigger/audit.go internal/buildtrigger/worker.go
git commit -m "refactor(buildtrigger): add reason param to dead-letter metric + audit"
```

---

## Task 3: Permanent routing in `recordResult`

**Files:**
- Modify: `internal/buildtrigger/worker.go`
- Test: `internal/buildtrigger/worker_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/buildtrigger/worker_test.go`:

```go
// permanentDeliverer always returns a permanent error, recording call count.
type permanentDeliverer struct{ calls int32 }

func (d *permanentDeliverer) Deliver(ctx context.Context, tr Trigger, p BuildPayload) (int, error) {
	atomic.AddInt32(&d.calls, 1)
	return 404, permanentf("HTTP 404")
}

// captureHandler is a concurrency-safe slog.Handler that records emitted records.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// hasMetricReason reports whether any captured record is a metric with the
// given metric_name and reason attrs.
func (h *captureHandler) hasMetricReason(metricName, reason string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		var name, rsn string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "metric_name":
				name = a.Value.String()
			case "reason":
				rsn = a.Value.String()
			}
			return true
		})
		if name == metricName && rsn == reason {
			return true
		}
	}
	return false
}

func TestWorker_PermanentErrorDeadLettersImmediately(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "n",
		Kind: KindGeneric, Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/main"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Enqueue(ctx, PushInfo{Tenant: "acme", Repo: "app", HeadOID: "a",
		RefUpdates: []RefUpdate{{Refname: "refs/heads/main", NewOID: "a"}}}); err != nil {
		t.Fatal(err)
	}

	capH := &captureHandler{}
	d := &permanentDeliverer{}
	cfg := WorkerConfig{
		TickInterval: 5 * time.Millisecond, ClaimBatchSize: 16,
		// A long schedule would allow many retries — a permanent error must
		// dead-letter on the first attempt anyway.
		BackoffSchedule: []time.Duration{time.Hour, time.Hour, time.Hour},
		Deliverers:      map[Kind]Deliverer{KindGeneric: d},
		Logger:          slog.New(capH),
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go StartWorker(wctx, svc, cfg)

	waitUntil(t, 3*time.Second, func() bool { return svc.countByStatus(ctx, "dead_letter") == 1 })
	if c := atomic.LoadInt32(&d.calls); c != 1 {
		t.Fatalf("permanent error must dead-letter after 1 attempt, got %d calls", c)
	}
	if svc.countByStatus(ctx, "pending") != 0 {
		t.Errorf("no row should remain pending after a permanent failure")
	}
	if !capH.hasMetricReason("build_trigger_deadletter_total", "permanent") {
		t.Error("expected build_trigger_deadletter_total with reason=permanent")
	}
}
```

Add `"log/slog"` and `"sync"` to the `worker_test.go` import block (it already imports `context`, `sync/atomic`, `testing`, `time`).

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run 'PermanentErrorDeadLetters' -v`
Expected: FAIL — the permanent error currently takes the retry path, so the row goes `pending` (with a 1h backoff) and `dead_letter` never reaches 1 within the timeout; `calls` would be 1 but the status assertion fails.

- [ ] **Step 3: Implement permanent routing**

In `internal/buildtrigger/worker.go`, add `"errors"` to the import block (alphabetically, before `"encoding/json"`). Then in `recordResult`, replace the dead_letter branch header and emitter calls. Change:

```go
	maxAttempts := len(cfg.BackoffSchedule) + 1
	if row.Attempts >= maxAttempts {
```

to:

```go
	maxAttempts := len(cfg.BackoffSchedule) + 1
	permanent := errors.Is(err, ErrPermanent)
	if permanent || row.Attempts >= maxAttempts {
		reason := "exhausted"
		if permanent {
			reason = "permanent"
		}
```

and within that branch change the two emitter calls from `"exhausted"` (set in Task 2) to `reason`:

```go
		EmitFired(ctx, logger, row.Kind, "dead_letter")
		EmitAttemptDuration(ctx, logger, "dead_letter", durationMs)
		EmitDeadLetterMetric(ctx, logger, reason)
		EmitDeadLetter(ctx, logger, row.ID, row.TriggerID, row.Attempts, statusCode, reason)
		return
	}
```

(The dead_letter UPDATE statement in the middle of the branch is unchanged.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/buildtrigger/ -run 'PermanentErrorDeadLetters' -v`
Expected: PASS.

- [ ] **Step 5: Run the full package (confirm retry path unchanged)**

Run: `go test ./internal/buildtrigger/`
Expected: PASS — including `TestWorker_RetriesThenDelivers` and `TestWorker_DeadLetterAfterExhaustion`, proving ordinary (non-permanent) errors still retry and still dead-letter only on exhaustion (now with `reason=exhausted`).

- [ ] **Step 6: Commit**

```bash
git add internal/buildtrigger/worker.go internal/buildtrigger/worker_test.go
git commit -m "feat(buildtrigger): route permanent errors straight to dead_letter"
```

---

## Task 4: Classify in `httpDeliverer`

**Files:**
- Modify: `internal/buildtrigger/deliver.go`
- Test: `internal/buildtrigger/deliver_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/buildtrigger/deliver_test.go`:

```go
func TestHTTPDeliverer_4xxIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	tr := Trigger{Kind: KindGeneric, TokenMode: TokenNone, Config: Config{URL: srv.URL, Secret: "s"}}
	d := &httpDeliverer{client: srv.Client()}
	code, err := d.Deliver(context.Background(), tr, BuildPayload{Repo: "app"})
	if code != 404 || err == nil {
		t.Fatalf("want 404+error, got code=%d err=%v", code, err)
	}
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("404 should be permanent: %v", err)
	}
}

func TestHTTPDeliverer_5xxIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	tr := Trigger{Kind: KindGeneric, TokenMode: TokenNone, Config: Config{URL: srv.URL, Secret: "s"}}
	d := &httpDeliverer{client: srv.Client()}
	_, err := d.Deliver(context.Background(), tr, BuildPayload{Repo: "app"})
	if err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("503 must be a retryable (non-permanent) error, got %v", err)
	}
}

func TestHTTPDeliverer_429IsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}))
	defer srv.Close()
	tr := Trigger{Kind: KindGeneric, TokenMode: TokenNone, Config: Config{URL: srv.URL, Secret: "s"}}
	d := &httpDeliverer{client: srv.Client()}
	_, err := d.Deliver(context.Background(), tr, BuildPayload{Repo: "app"})
	if err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("429 must be retryable (rate limit), got %v", err)
	}
}

func TestHTTPDeliverer_BadSchemeIsPermanent(t *testing.T) {
	tr := Trigger{Kind: KindGeneric, TokenMode: TokenNone, Config: Config{URL: "ftp://x", Secret: "s"}}
	d := &httpDeliverer{client: http.DefaultClient}
	_, err := d.Deliver(context.Background(), tr, BuildPayload{})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("bad URL scheme should be permanent, got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/buildtrigger/ -run 'HTTPDeliverer_(4xxIsPermanent|BadSchemeIsPermanent)' -v`
Expected: FAIL — currently both the scheme error and the non-2xx error are plain `fmt.Errorf`, so `errors.Is(err, ErrPermanent)` is false. (`5xxIsRetryable`/`429IsRetryable` already pass — they assert non-permanence.)

- [ ] **Step 3: Classify in `httpDeliverer.Deliver`**

In `internal/buildtrigger/deliver.go`, change the scheme check error from:

```go
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return 0, fmt.Errorf("egress denied: trigger URL scheme must be http or https")
	}
```

to:

```go
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return 0, permanentf("egress denied: trigger URL scheme must be http or https")
	}
```

And change the non-2xx return at the end of `Deliver` from:

```go
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
```

to:

```go
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	if httpStatusPermanent(resp.StatusCode) {
		return resp.StatusCode, permanentf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
```

(Mint failure and `client.Do` network error returns are unchanged — they stay retryable.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/buildtrigger/ -run 'HTTPDeliverer' -v`
Expected: PASS — including the pre-existing `TestHTTPDeliverer_Non2xxIsError` (500, still an error) and `TestHTTPDeliverer_MintErrorIsRetryable`.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/deliver.go internal/buildtrigger/deliver_test.go
git commit -m "feat(buildtrigger): httpDeliverer marks 4xx + bad-scheme as permanent"
```

---

## Task 5: Classify in `azurePipelinesDeliverer`

**Files:**
- Modify: `internal/buildtrigger/azurepipelines.go`
- Test: `internal/buildtrigger/azurepipelines_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/buildtrigger/azurepipelines_test.go`. It currently imports `encoding/json`, `net/http`, `testing`; add `context`, `errors`, and `net/http/httptest`:

```go
func TestAzurePipelines_4xxIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	d := &azurePipelinesDeliverer{
		clientFor: func(Trigger) (azureConn, error) {
			return azureConn{orgURL: srv.URL, pat: "p", client: srv.Client()}, nil
		},
	}
	tr := Trigger{Kind: KindAzurePipelines, TokenMode: TokenNone,
		Config: Config{AzureConnector: "prod", AzureProject: "P", AzurePipelineID: 1}}
	code, err := d.Deliver(context.Background(), tr, BuildPayload{RefUpdate: RefUpdate{Refname: "refs/heads/main"}})
	if code != 401 || !errors.Is(err, ErrPermanent) {
		t.Fatalf("401 should be permanent, got code=%d err=%v", code, err)
	}
}

func TestAzurePipelines_5xxIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	d := &azurePipelinesDeliverer{
		clientFor: func(Trigger) (azureConn, error) {
			return azureConn{orgURL: srv.URL, pat: "p", client: srv.Client()}, nil
		},
	}
	tr := Trigger{Kind: KindAzurePipelines, TokenMode: TokenNone,
		Config: Config{AzureConnector: "prod", AzureProject: "P", AzurePipelineID: 1}}
	_, err := d.Deliver(context.Background(), tr, BuildPayload{RefUpdate: RefUpdate{Refname: "refs/heads/main"}})
	if err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("500 must be retryable, got %v", err)
	}
}

func TestAzurePipelines_UnknownConnectorIsPermanent(t *testing.T) {
	// Empty connector map → factory returns an error for any connector name.
	d := &azurePipelinesDeliverer{
		clientFor: newAzurePipelinesClientFactory(map[string]AzureConnector{}, http.DefaultClient),
	}
	tr := Trigger{Kind: KindAzurePipelines, TokenMode: TokenNone,
		Config: Config{AzureConnector: "missing", AzureProject: "P", AzurePipelineID: 1}}
	_, err := d.Deliver(context.Background(), tr, BuildPayload{RefUpdate: RefUpdate{Refname: "refs/heads/main"}})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("unknown connector should be permanent, got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/buildtrigger/ -run 'AzurePipelines_(4xxIsPermanent|UnknownConnectorIsPermanent)' -v`
Expected: FAIL — connector error and non-2xx are currently plain `fmt.Errorf`.

- [ ] **Step 3: Classify in `azurePipelinesDeliverer.Deliver`**

In `internal/buildtrigger/azurepipelines.go`, change the connector-resolution error from:

```go
	conn, err := d.clientFor(tr)
	if err != nil {
		return 0, fmt.Errorf("azure connector: %w", err)
	}
```

to:

```go
	conn, err := d.clientFor(tr)
	if err != nil {
		return 0, permanentf("azure connector: %v", err)
	}
```

And change the non-2xx return from:

```go
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
```

to:

```go
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	if httpStatusPermanent(resp.StatusCode) {
		return resp.StatusCode, permanentf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
```

(Mint failure, body-build error, and `conn.client.Do` network error are unchanged — still retryable. After this change `fmt` is still used by the remaining `fmt.Errorf` calls in the file, so the import stays.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/buildtrigger/ -run 'AzurePipelines' -v`
Expected: PASS — including the existing `TestNewAzurePipelinesClientFactory_ResolvesAndErrors` and the wire-shape test.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/azurepipelines.go internal/buildtrigger/azurepipelines_test.go
git commit -m "feat(buildtrigger): azurepipelines marks 4xx + unknown-connector as permanent"
```

---

## Task 6: Documentation

**Files:**
- Modify: `docs/operator-guides/build-triggers.md`
- Modify: `docs/build-triggers.md`

- [ ] **Step 1: Update the operator guide**

Open `docs/operator-guides/build-triggers.md` and find the Azure error-handling content (§5.3) and the metrics table (§9.2, the `build_trigger_deadletter_total` row). Make two edits.

(a) Add a general error-handling note. After the existing retry-schedule description in the operations/observability area (near where `build_trigger_deadletter_total` is introduced), add this paragraph:

```markdown
### Permanent vs. transient failures

For HTTP-delivered triggers (`generic`, `cloudbuild`, `azurewebhook`,
`azurepipelines`), bucketvcs distinguishes failures that cannot succeed on retry
from transient ones:

- **Permanent** (dead-letters **immediately**, `reason=permanent`): any 4xx
  response except `408` and `429`, a non-`http(s)` URL scheme, or an unknown
  `azure_connector`. Fix the configuration, then `bucketvcs build delivery
  replay --id=<id>`.
- **Transient** (retries `1m → 30m → 2h → 12h`, then dead-letters with
  `reason=exhausted`): `5xx`, `408`, `429`, network errors, and token-mint blips.

`codebuild` errors are currently always treated as transient (retry-only).
The breakdown is exposed as the `reason` label on
`build_trigger_deadletter_total` and as the `reason` attribute on the
`build.trigger.deadletter` audit event.
```

(b) In the metrics table row for `build_trigger_deadletter_total`, update its label cell to include `reason={permanent,exhausted}`. For example, change the description/labels cell so it reads (match the table's existing column format):

```markdown
| `build_trigger_deadletter_total` | `reason={permanent,exhausted}` | incremented once when a delivery is dead-lettered |
```

(Read the existing row first and preserve the table's exact column count/format; only add the `reason` label.)

- [ ] **Step 2: Update the overview doc troubleshooting**

In `docs/build-triggers.md`, find the Azure troubleshooting bullet added previously (the "Azure returns HTTP 401/404" line) and append one sentence to that bullet, plus extend the general durability note. Specifically, replace the durability paragraph:

```markdown
Deliveries are enqueued durably and retried on a backoff schedule
(`1m → 30m → 2h → 12h`), then dead-lettered and replayable — a momentary blip at
the build system never loses the event. Enqueue is **fail-open**: if the trigger
machinery hiccups, your push still succeeds. (Details:
[operator guide §8](operator-guides/build-triggers.md).)
```

with:

```markdown
Deliveries are enqueued durably and retried on a backoff schedule
(`1m → 30m → 2h → 12h`), then dead-lettered and replayable — a momentary blip at
the build system never loses the event. **Permanent** failures (a 4xx other than
408/429, a bad URL scheme, or an unknown Azure connector) skip the retries and
dead-letter immediately with `reason=permanent`, so a misconfiguration surfaces
in seconds rather than ~14h; fix it and `build delivery replay`. Enqueue is
**fail-open**: if the trigger machinery hiccups, your push still succeeds.
(Details: [operator guide §8](operator-guides/build-triggers.md).)
```

- [ ] **Step 3: Commit**

```bash
git add docs/operator-guides/build-triggers.md docs/build-triggers.md
git commit -m "docs(buildtrigger): document permanent vs transient delivery failures"
```

---

## Task 7: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 2: Vet**

Run: `go vet ./internal/buildtrigger/`
Expected: no findings.

- [ ] **Step 3: Full package test**

Run: `go test ./internal/buildtrigger/ -v`
Expected: PASS, including the new tests (`TestHTTPStatusPermanent`, `TestPermanentf_IsErrPermanent`, `TestWorker_PermanentErrorDeadLettersImmediately`, `TestHTTPDeliverer_4xxIsPermanent`/`5xxIsRetryable`/`429IsRetryable`/`BadSchemeIsPermanent`, `TestAzurePipelines_4xxIsPermanent`/`5xxIsRetryable`/`UnknownConnectorIsPermanent`) and all pre-existing tests.

- [ ] **Step 4: Confirm no migration/wire-shape drift**

Run: `git diff --stat main...HEAD -- internal/buildtrigger/testdata/ internal/auth/sqlitestore/migrations/`
Expected: empty output — no golden-file or migration changes (the request bodies and schema are untouched).

- [ ] **Step 5: Request code review**

Use the superpowers:requesting-code-review skill (or `/roborev-review-branch`) before merging.
