# M11 Phase 12.5 — Gateway-side Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the gateway-side metrics and audit events that were carved out of the original M11 Phase 12 plan, so M11 ships with a complete observability surface before Phase 13's operator guide is written.

**Architecture:** Add a `Logger *slog.Logger` field to `gateway.Options` (sshd.Options already has one) and to `uploadpack.EngineRequest`. Thread the logger from each transport's Options through into the engine. Create a small `internal/gateway/log.go` (mirroring `internal/maintenance/log.go`'s `emitMetric` shape) for the gateway-side and proxied-serve emission sites; emit advertise-time metrics from inside `internal/gitproto/uploadpack/service.go` where the bundle-uri / pack-uri response is actually produced (the engine is opaque to the gateway, so observability has to live where emission happens). Add a `countingWriter` wrapper in `internal/gateway/proxied_routes.go` so the served-bytes metric is accurate.

**Tech Stack:** Go 1.22+, `log/slog`, existing `internal/maintenance/log.go` as the canonical pattern, `net/http` for serve-side instrumentation, existing test pattern `bytes.Buffer + slog.NewJSONHandler`.

---

## Scope

**This phase ships:**

> **Note (post-merge correction):** the as-shipped label vocabulary differs from
> the original plan text below. Implementation review surfaced that the
> `freshness=retired` value conflated 4 distinct conditions (feature disabled,
> no full_default entry, ref deleted, EvaluateFreshness=Retired). The shipped
> code distinguishes them. Likewise the `proxied_url_token_invalid_total.reason`
> vocabulary gained a `missing` value to separate missing-token from HMAC
> failures.

8 metrics (as shipped):
- `bundle_advertised_total{repo_id, freshness}` — per bundle-uri command response. `freshness` ∈ {`disabled`, `no_bundle`, `no_ref`, `current`, `warm`, `stale`, `retired`}.
- `bundle_uri_advertised_total{repo_id, via}` — only when the response actually contained bundle URLs. `via` ∈ {`proxied`, `direct`}.
- `bundle_uri_served_total{via}` — per successful proxied-serve of a bundle (includes truncated serves; see emitServed doc).
- `bundle_uri_served_bytes{via}` — bytes actually written on the wire (may be less than Content-Length on client disconnect).
- `pack_uri_advertised_total{repo_id, via}` — when the in-fetch packfile-uris stanza fires.
- `pack_uri_served_total{via}` — per successful proxied-serve of a pack.
- `pack_uri_served_bytes{via}` — bytes actually written on successful pack serve.
- `proxied_url_token_invalid_total{reason}` — `reason` ∈ {`missing`, `expired`, `kind_mismatch`, `invalid`}.

2 audit events (flat-attrs shape per `internal/maintenance/log.go::emitBundleGenerated`):
- `bundle.uri.advertised` — emitted from `serveBundleURI` whenever the response contains URLs. Fields: `repo_id`, `freshness`, `via`, `bundle_count`, `first_tip_oid`.
- `proxied.url.served` — emitted from `proxiedHandler` on a successful 200/206 reply. Fields: `kind` (bundle|pack), `hash`, `bytes_served`, `status_code`, `range_request` (bool).

**Out of scope (deferred to M12 or successor):**

- Multi-tenant `ProxiedKeyResolver` impl + per-served-object `repo_id` label (carried from Phase 8).
- Histograms / percentile latency metrics (Prometheus migration question for M12).
- `bundle.retired` with `reason=gc_swept` audit — blocked on the full bundle GC pipeline wiring deferred from Phase 9.
- Renaming `bundle_byte_size` from Phase 12 to `bundle_bytes` for convention alignment (Phase 13 documentation-time decision).

**Why `repo_id` is omitted from served-* metrics:** The proxied URL endpoint at `internal/gateway/proxied_routes.go:60-247` resolves objects by hash alone via `ProxiedKeyResolver` (interface declared at line 16-32). The single-repo deployment assumption documented at lines 18-23 means the gateway instance is implicitly bound to one (tenant, repo), but that binding isn't reflected back to the handler at request time. Adding `repo_id` here requires either a `ProxiedKeyResolver` interface change or extracting tenant/repo from the URL path — both out of scope. The advertise-time metrics (`bundle_advertised_total`, `bundle_uri_advertised_total`, `pack_uri_advertised_total`) DO carry `repo_id` because their emission sites in `serveBundleURI` / `serveFetch` know the tenant/repo from the routed request (engine.EngineRequest carries that context).

---

## File Structure

**New files (1):**
- `internal/gateway/log.go` — package-local `emitMetric`, `emitBundleURIAdvertised`, `emitProxiedURLServed` helpers. Mirror of `internal/maintenance/log.go::emitMetric` (duplicated rather than extracted to a shared package; the two packages have no other shared infrastructure today and a premature `internal/obs` package would obscure the lineage).
- `internal/gateway/log_test.go` — unit tests for the three helpers using the existing `bytes.Buffer + slog.NewJSONHandler` pattern.

**Modified files (10):**
- `internal/gateway/server.go` — add `Logger *slog.Logger` to `Options`, default to `slog.Default()` in `NewServer` when nil. Store on the `server` struct.
- `internal/gateway/upload_pack.go` — thread `Logger` from `s.opts` into `EngineRequest`.
- `internal/gateway/proxied_routes.go` — accept `Logger` (or derive from `server.opts`); add `countingWriter` wrapper; emit served-* metrics + `proxied.url.served` audit + `proxied_url_token_invalid_total`.
- `internal/gateway/proxied_routes_test.go` — extend existing tests with metric/audit assertions; add 3 token-invalid sub-tests for the reason label.
- `internal/sshd/session.go` — thread `Logger` from `s.opts` (already exists in sshd.Options at server.go:40) into `EngineRequest`.
- `internal/gitproto/uploadpack/engine.go` — add `Logger *slog.Logger` field to `EngineRequest`. Default fallback in advertise.go / service.go where used.
- `internal/gitproto/uploadpack/service.go` — emit `bundle_advertised_total`, `bundle_uri_advertised_total`, `bundle.uri.advertised` audit from `serveBundleURI`; emit `pack_uri_advertised_total` from `serveFetch` when the packfile-uris stanza is produced.
- `internal/gitproto/uploadpack/service_test.go` — assertions for the new emission points.
- `internal/gateway/server_test.go` — assertion that nil-Logger Options does not panic in NewServer.

---

## Task 1: Logger plumbing infrastructure

**Files:**
- Create: `internal/gateway/log.go`
- Create: `internal/gateway/log_test.go`
- Modify: `internal/gateway/server.go` (add `Logger` field to `Options`, store on server, default fallback)
- Modify: `internal/gateway/server_test.go` (nil-Logger doesn't panic)
- Modify: `internal/gitproto/uploadpack/engine.go` (add `Logger` to `EngineRequest`)
- Modify: `internal/gateway/upload_pack.go` (thread `Logger` from `s.opts` to `EngineRequest`)
- Modify: `internal/sshd/session.go` (thread `Logger` from `s.opts` to `EngineRequest`)

### Step 1.1 — Write the failing test (gateway log helpers)

- [ ] Create `internal/gateway/log_test.go`:

```go
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestEmitMetric_HasMetricNameAndLabels(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitMetric(context.Background(), logger, "bundle_advertised_total", 1, "repo_id", "t/r", "freshness", "current")
	line := buf.String()
	if !strings.Contains(line, `"metric_name":"bundle_advertised_total"`) {
		t.Errorf("missing metric_name: %s", line)
	}
	if !strings.Contains(line, `"value":1`) {
		t.Errorf("missing value: %s", line)
	}
	if !strings.Contains(line, `"repo_id":"t/r"`) {
		t.Errorf("missing repo_id label: %s", line)
	}
	if !strings.Contains(line, `"freshness":"current"`) {
		t.Errorf("missing freshness label: %s", line)
	}
}

func TestEmitMetric_NilLoggerDoesNotPanic(t *testing.T) {
	// Must not panic when logger is nil; falls back to slog.Default().
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("emitMetric panicked with nil logger: %v", r)
		}
	}()
	emitMetric(context.Background(), nil, "x", 1)
}

func TestEmitBundleURIAdvertised_AuditFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitBundleURIAdvertised(context.Background(), logger, "t/r", "current", "proxied", 1, "abc123")
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if entry["audit"] != true {
		t.Errorf("missing audit=true")
	}
	if entry["event"] != "bundle.uri.advertised" {
		t.Errorf("event = %v, want bundle.uri.advertised", entry["event"])
	}
	if entry["repo_id"] != "t/r" {
		t.Errorf("repo_id = %v", entry["repo_id"])
	}
	if entry["freshness"] != "current" {
		t.Errorf("freshness = %v", entry["freshness"])
	}
	if entry["via"] != "proxied" {
		t.Errorf("via = %v", entry["via"])
	}
	if int(entry["bundle_count"].(float64)) != 1 {
		t.Errorf("bundle_count = %v", entry["bundle_count"])
	}
	if entry["first_tip_oid"] != "abc123" {
		t.Errorf("first_tip_oid = %v", entry["first_tip_oid"])
	}
}

func TestEmitProxiedURLServed_AuditFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitProxiedURLServed(context.Background(), logger, "bundle", "sha256-aa", 12345, 206, true)
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if entry["audit"] != true {
		t.Errorf("missing audit=true")
	}
	if entry["event"] != "proxied.url.served" {
		t.Errorf("event = %v, want proxied.url.served", entry["event"])
	}
	if entry["kind"] != "bundle" {
		t.Errorf("kind = %v", entry["kind"])
	}
	if entry["hash"] != "sha256-aa" {
		t.Errorf("hash = %v", entry["hash"])
	}
	if int64(entry["bytes_served"].(float64)) != 12345 {
		t.Errorf("bytes_served = %v", entry["bytes_served"])
	}
	if int(entry["status_code"].(float64)) != 206 {
		t.Errorf("status_code = %v", entry["status_code"])
	}
	if entry["range_request"] != true {
		t.Errorf("range_request = %v", entry["range_request"])
	}
}
```

### Step 1.2 — Run to verify compile failure

- [ ] Run: `go test ./internal/gateway/ -run "TestEmit" -v`
- Expected: COMPILE ERROR — `emitMetric`, `emitBundleURIAdvertised`, `emitProxiedURLServed` undefined.

### Step 1.3 — Implement the helpers

- [ ] Create `internal/gateway/log.go`:

```go
package gateway

import (
	"context"
	"log/slog"
)

// emitMetric logs a structured metric with a name, value, and optional
// label pairs. Mirrors internal/maintenance/log.go::emitMetric exactly so
// log consumers can apply the same parsing rules across the two packages.
// Pairs whose key isn't a string are skipped (debug-logged) rather than
// emitted with an empty key.
func emitMetric(ctx context.Context, logger *slog.Logger, name string, value int64, kvs ...any) {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []slog.Attr{
		slog.String("metric_name", name),
		slog.Int64("value", value),
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		k, ok := kvs[i].(string)
		if !ok {
			logger.LogAttrs(ctx, slog.LevelDebug, "emitMetric: skipping non-string label key",
				slog.String("metric_name", name),
				slog.Any("bad_key", kvs[i]))
			continue
		}
		attrs = append(attrs, slog.Any(k, kvs[i+1]))
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric", attrs...)
}

// emitBundleURIAdvertised logs a bundle.uri.advertised audit event after the
// gateway's bundle-uri command response contains at least one bundle entry.
// One event per request that returned URLs (an empty advertise emits nothing).
// Operators correlate first_tip_oid with bundle.generated (maintenance-side
// emitted at runBundlePhase) to attribute serves to the generating maintenance
// run.
func emitBundleURIAdvertised(ctx context.Context, logger *slog.Logger, repoID, freshness, via string, bundleCount int, firstTipOID string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "bundle.uri.advertised",
		slog.Bool("audit", true),
		slog.String("event", "bundle.uri.advertised"),
		slog.String("repo_id", repoID),
		slog.String("freshness", freshness),
		slog.String("via", via),
		slog.Int("bundle_count", bundleCount),
		slog.String("first_tip_oid", firstTipOID),
	)
}

// emitProxiedURLServed logs a proxied.url.served audit event after a
// successful 200/206 reply from the proxied URL endpoint. Emitted from
// proxiedHandler post-io.Copy; the bytes_served value is the actual bytes
// written (via countingWriter), not the Content-Length header.
func emitProxiedURLServed(ctx context.Context, logger *slog.Logger, kind, hash string, bytesServed int64, statusCode int, rangeRequest bool) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "proxied.url.served",
		slog.Bool("audit", true),
		slog.String("event", "proxied.url.served"),
		slog.String("kind", kind),
		slog.String("hash", hash),
		slog.Int64("bytes_served", bytesServed),
		slog.Int("status_code", statusCode),
		slog.Bool("range_request", rangeRequest),
	)
}
```

### Step 1.4 — Run tests to verify pass

- [ ] Run: `go test ./internal/gateway/ -run "TestEmit" -v`
- Expected: 4 PASS.

### Step 1.5 — Add `Logger` field to gateway.Options

- [ ] Edit `internal/gateway/server.go`. Add `Logger *slog.Logger` to the `Options` struct, placed near the existing `BundleURIBuildURL` cluster (around line 27-105 in the current file). Document that nil falls back to `slog.Default()` in `NewServer`:

```go
// Logger is used for structured metric + audit emission. When nil, the
// gateway falls back to slog.Default(). M11 Phase 12.5 adds this for
// gateway-side observability; before that the gateway only used slog.Default()
// ad-hoc for startup warnings.
Logger *slog.Logger
```

- [ ] In `NewServer`, after the existing validation block, normalize:
```go
if opts.Logger == nil {
    opts.Logger = slog.Default()
}
```

- [ ] Add a `logger *slog.Logger` field to the `server` struct, set from `opts.Logger`.

### Step 1.6 — Add `Logger` field to EngineRequest

- [ ] Edit `internal/gitproto/uploadpack/engine.go`. Add `Logger *slog.Logger` to the `EngineRequest` struct. Place it near the existing observability-adjacent fields (after the URI closures):

```go
// Logger is used by the engine to emit advertise-time metrics + audit events
// from internal/gitproto/uploadpack/service.go::serveFetch and serveBundleURI.
// When nil, falls back to slog.Default(). The HTTP gateway and SSH transport
// thread their own Logger from their respective Options.
Logger *slog.Logger
```

- [ ] Add an `import "log/slog"` line if not already present.

### Step 1.7 — Thread Logger from gateway → engine

- [ ] Edit `internal/gateway/upload_pack.go`. In the handler that builds `EngineRequest`, set `req.Logger = s.opts.Logger` (or `s.logger`, depending on the struct field name chosen in Step 1.5).

### Step 1.8 — Thread Logger from sshd → engine

- [ ] Edit `internal/sshd/session.go`. In the handler at lines 160-179 where URI fields are threaded into `EngineRequest`, add `req.Logger = s.opts.Logger`. The sshd.Options already has a required `Logger` field (server.go:40); just pass it through.

### Step 1.9 — Server-level test that nil Logger doesn't panic

- [ ] Edit `internal/gateway/server_test.go`. Add:

```go
func TestNewServer_NilLogger_DefaultsToSlogDefault(t *testing.T) {
	// NewServer must not panic with Options.Logger == nil; it should
	// fall back to slog.Default().
	opts := /* minimum-valid Options without Logger set; copy from an existing test */
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv == nil {
		t.Fatal("nil server")
	}
}
```

(If no existing test constructs a "minimum-valid" Options, look at the existing `TestNewServer_*` tests and replicate the pattern.)

### Step 1.10 — Verify

- [ ] `go test ./internal/gateway/... ./internal/sshd/... ./internal/gitproto/uploadpack/...` — all green.
- [ ] `go vet ./...` — clean.

### Step 1.11 — Commit

```bash
git add internal/gateway/log.go internal/gateway/log_test.go \
        internal/gateway/server.go internal/gateway/server_test.go \
        internal/gateway/upload_pack.go \
        internal/sshd/session.go \
        internal/gitproto/uploadpack/engine.go
git commit -m "$(cat <<'EOF'
gateway+sshd+uploadpack: Logger plumbing for Phase 12.5 observability

Add Logger field to gateway.Options + uploadpack.EngineRequest (sshd.Options
already has one); default to slog.Default() when nil. Create internal/gateway/
log.go with emitMetric / emitBundleURIAdvertised / emitProxiedURLServed
helpers mirroring internal/maintenance/log.go shape.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: bundle-uri command observability

**Files:**
- Modify: `internal/gitproto/uploadpack/service.go` (in `serveBundleURI` lines 513-624)
- Modify: `internal/gitproto/uploadpack/service_test.go` (or a sibling `*_test.go` covering bundle-uri command)

**Surface to instrument:** `serveBundleURI` (service.go:513-624). After `v2proto.HandleBundleURI` returns (line 623), the function knows:
- Whether the response contained URLs (compute from the deps the function fed to HandleBundleURI; see Step 2.1 for the detection strategy).
- The freshness state (computed at lines 554-622).
- The `via` value (proxied if the BuildURL closure produced a /_bundle/ URL, else direct — needs detection logic; see Step 2.2).

### Step 2.1 — Detection strategy: capture the URLs HandleBundleURI is about to emit

The cleanest detection point is BEFORE calling HandleBundleURI: the function already has computed the list of (key → URL) pairs in scope. Read `service.go` from line 513 to confirm the variable name carrying the URL slice. Use that variable to set `bundleCount`, `firstTipOID`, and `via`.

Detection rules:
- `freshness`: directly from the freshness-state evaluation. Map enum to string ("current", "warm", "stale", "not_ancestor", "empty"). If the engine's `freshness.State` exists as an enum with a `.String()` method, use it; otherwise add a `func (s State) MetricLabel() string` helper that produces the lowercased label form.
- `bundleCount`: `len(urlsList)`.
- `firstTipOID`: `urlsList[0].TipOID` when non-empty, else `""`.
- `via`: parse the first URL; if its path contains `"/_bundle/"`, set `"proxied"`, else `"direct"`. (Per Phase 6 routing: proxied URLs always carry the `/_bundle/<hash>` path segment.)

### Step 2.2 — Write the failing test

- [ ] Add to `internal/gitproto/uploadpack/service_test.go` (or the equivalent bundle-uri test file):

```go
func TestServeBundleURI_EmitsAdvertisedMetricsAndAudit_NonEmpty(t *testing.T) {
	// Construct an EngineRequest with BundleURIEnabled + a BuildURL closure
	// that returns a proxied URL. Drive serveBundleURI and capture logs via
	// bytes.Buffer + slog.NewJSONHandler. Assert:
	//   1. metric line: bundle_advertised_total {repo_id, freshness=current}
	//   2. metric line: bundle_uri_advertised_total {repo_id, via=proxied}
	//   3. audit line:  bundle.uri.advertised with bundle_count >= 1, firstTipOID set
	// ...
}

func TestServeBundleURI_EmitsAdvertisedMetric_EmptyResponse(t *testing.T) {
	// When the response is empty (no warm bundle), assert:
	//   - bundle_advertised_total fires with freshness="empty" (or appropriate)
	//   - bundle_uri_advertised_total does NOT fire
	//   - bundle.uri.advertised audit does NOT fire
}
```

Detailed test setup: copy from the existing `TestService_BundleURI_*` test in `service_test.go` (or wherever bundle-uri is tested), and add the slog capture pattern.

### Step 2.3 — Implement

- [ ] In `service.go::serveBundleURI`, after the URLs list is built and before/after `v2proto.HandleBundleURI` is called, emit:

```go
// Always emit bundle_advertised_total (per-command).
freshnessLabel := state.MetricLabel() // or state.String(), depending on the existing API
emitMetric(req.Ctx, req.Logger, "bundle_advertised_total", 1,
    "repo_id", repoID,
    "freshness", freshnessLabel,
)

if len(urlsList) > 0 {
    via := classifyVia(urlsList[0].URL) // "proxied" or "direct"
    emitMetric(req.Ctx, req.Logger, "bundle_uri_advertised_total", 1,
        "repo_id", repoID,
        "via", via,
    )
    emitBundleURIAdvertised(req.Ctx, req.Logger, repoID, freshnessLabel, via, len(urlsList), urlsList[0].TipOID)
}
```

**Important:** the engine and gateway live in different packages. To call `emitMetric` and `emitBundleURIAdvertised` from `internal/gitproto/uploadpack/service.go`, EITHER:
- Move the helpers from `internal/gateway/log.go` to a shared location (e.g., `internal/gwobs` or `internal/obs/metrics`), OR
- Duplicate the helpers in `internal/gitproto/uploadpack/log.go` (small package-local mirror).

**Pick option (b) for this phase: duplicate.** Rationale: the helpers are 30 lines each; pulling them into a shared package now adds dependency surface without payoff. Phase 13 can audit and extract if needed. The duplication is intentional and called out in package doc comments.

- [ ] Create `internal/gitproto/uploadpack/log.go` with `emitMetric` and `emitBundleURIAdvertised` (mirrors of `internal/gateway/log.go`). Document that the duplication is deliberate and references the gateway version as the convention source.

- [ ] Add a `classifyVia` helper either in `internal/gitproto/uploadpack/log.go` or in `service.go`:

```go
// classifyVia returns "proxied" if the URL path contains "/_bundle/" or
// "/_pack/" (the gateway-proxied route shape per Phase 6 routing), else
// "direct" (a signed cloud-backend URL).
func classifyVia(rawURL string) string {
    if rawURL == "" {
        return "direct"
    }
    if strings.Contains(rawURL, "/_bundle/") || strings.Contains(rawURL, "/_pack/") {
        return "proxied"
    }
    return "direct"
}
```

### Step 2.4 — Run tests

- [ ] `go test ./internal/gitproto/uploadpack/ -run "TestServeBundleURI_Emits" -v`
- Expected: both PASS.
- [ ] `go test ./internal/gitproto/uploadpack/...` — no regressions.

### Step 2.5 — Commit

```bash
git add internal/gitproto/uploadpack/log.go \
        internal/gitproto/uploadpack/service.go \
        internal/gitproto/uploadpack/service_test.go
git commit -m "$(cat <<'EOF'
uploadpack/service: emit bundle-uri advertise metrics + audit event

Emit bundle_advertised_total (per-command, freshness label) from
serveBundleURI; emit bundle_uri_advertised_total and bundle.uri.advertised
audit event only when the response contains URLs. Detect proxied vs direct
via /_bundle/ path segment. Duplicate emitMetric + emitBundleURIAdvertised
into internal/gitproto/uploadpack/log.go (mirror of internal/gateway/log.go)
since the engine package can't depend on internal/gateway.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: pack-uri advertise metric

**Files:**
- Modify: `internal/gitproto/uploadpack/service.go` (in `serveFetch`, near lines 355-390 where the packfile-uris gate fires)
- Modify: `internal/gitproto/uploadpack/service_test.go` (or sibling)

### Step 3.1 — Write the failing test

- [ ] Add a test that drives `serveFetch` with `PackURIEnabled=true`, a client opt-in for packfile-uris, and a full-pack-requested scenario. Capture logs and assert `pack_uri_advertised_total` fires when `packURIStanza` is non-empty (around service.go line 378). Assert it does NOT fire when the gate is closed.

### Step 3.2 — Implement

- [ ] In `service.go::serveFetch`, immediately after the line where `packURIStanza` is set (line 378 area):

```go
if packURIStanza != "" {
    via := classifyVia(stanzaURL) // first URL from the stanza
    emitMetric(req.Ctx, req.Logger, "pack_uri_advertised_total", 1,
        "repo_id", repoID,
        "via", via,
    )
}
```

Note: `stanzaURL` will require parsing the stanza line. If parsing is awkward, the simpler form is to compute `via` from the BuildURL closure result that produced the stanza. Inspect the actual code flow in service.go lines 355-390 to pick the cleanest hook point.

### Step 3.3 — Verify and commit

- [ ] `go test ./internal/gitproto/uploadpack/...` — green.

```bash
git add internal/gitproto/uploadpack/service.go internal/gitproto/uploadpack/service_test.go
git commit -m "$(cat <<'EOF'
uploadpack/service: emit pack_uri_advertised_total when stanza fires

Emit pack_uri_advertised_total{repo_id, via} from serveFetch immediately
after the packfile-uris gate produces a non-empty stanza. Mirror of
bundle_uri_advertised_total. No audit event for pack-uri advertise (the
maintenance-side bundle.generated audit suffices for the lifecycle; pack-uri
shipping happens per-fetch, not per-artifact).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Proxied-serve observability

**Files:**
- Modify: `internal/gateway/proxied_routes.go` (add `countingWriter`, emit served-* metrics + audit event)
- Modify: `internal/gateway/proxied_routes_test.go` (assert metric + audit + bytes)
- Modify: `internal/gateway/server.go` (thread Logger into proxiedHandler — if not already done in Task 1; verify)

### Step 4.1 — Write the failing test

- [ ] Add to `proxied_routes_test.go`:

```go
func TestProxiedHandler_BundleServe_EmitsServedMetricsAndAudit(t *testing.T) {
	// Set up a proxiedHandler with a small in-memory ObjectStore + a valid token.
	// Capture logs via bytes.Buffer + slog.NewJSONHandler attached to the gateway's Logger.
	// Drive GET /_bundle/<hash>?token=<valid> with a body of known size N.
	// Assert:
	//   - response status 200
	//   - response body bytes == N
	//   - log contains metric bundle_uri_served_total {via=proxied} value=1
	//   - log contains metric bundle_uri_served_bytes {via=proxied} value=N
	//   - log contains audit proxied.url.served {kind=bundle, hash=<hash>, bytes_served=N, status_code=200, range_request=false}
}

func TestProxiedHandler_PackServe_RangeRequest_EmitsServedMetricsAndAudit(t *testing.T) {
	// Same shape but for /_pack/, with a Range header.
	// Assert status 206, bytes_served < N (partial), range_request=true,
	// pack_uri_served_total {via=proxied} value=1, pack_uri_served_bytes {via=proxied} value=<partial>.
}

func TestProxiedHandler_ErrorPath_DoesNotEmitServedMetrics(t *testing.T) {
	// 404 (object not found) or 416 (range invalid): no served-* metrics emit;
	// no proxied.url.served audit emits.
}
```

### Step 4.2 — Implement `countingWriter`

- [ ] Add to `proxied_routes.go`:

```go
// countingWriter wraps an http.ResponseWriter to record the number of bytes
// written to the body, so the served-* metrics report actual bytes (which
// may be less than Content-Length when the client disconnects mid-stream).
type countingWriter struct {
    http.ResponseWriter
    bytes int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
    n, err := c.ResponseWriter.Write(p)
    c.bytes += int64(n)
    return n, err
}
```

### Step 4.3 — Wrap the ResponseWriter at handler entry

- [ ] In `proxied_routes.go::ServeHTTP` (line 60), wrap `w`:

```go
cw := &countingWriter{ResponseWriter: w}
w = cw
```

- [ ] At each success branch (after `io.Copy` returns at lines 191 and 246), emit metrics + audit:

```go
kind := "bundle" // or "pack" — determined by the route prefix
statusCode := http.StatusOK // or http.StatusPartialContent for range serves
rangeRequest := /* true if Range header present */
emitMetric(r.Context(), h.logger, kind+"_uri_served_total", 1, "via", "proxied")
emitMetric(r.Context(), h.logger, kind+"_uri_served_bytes", cw.bytes, "via", "proxied")
emitProxiedURLServed(r.Context(), h.logger, kind, hash, cw.bytes, statusCode, rangeRequest)
```

**Important:** the `kind` variable is determined at the top of `ServeHTTP` from the URL path (`/_bundle/` vs `/_pack/`). Hoist it into a local at handler entry so both the success-emit branch and the error-emit branch (Task 5) can use it.

### Step 4.4 — Thread Logger into proxiedHandler

- [ ] Verify (or add) a `logger *slog.Logger` field on `proxiedHandler`. Set it from `server.opts.Logger` in `NewServer` where the handler is registered (around server.go:273-276).

### Step 4.5 — Verify and commit

- [ ] `go test ./internal/gateway/ -run "TestProxiedHandler" -v` — all green.
- [ ] Full gateway tests pass: `go test ./internal/gateway/...`

```bash
git add internal/gateway/proxied_routes.go \
        internal/gateway/proxied_routes_test.go \
        internal/gateway/server.go
git commit -m "$(cat <<'EOF'
gateway/proxied_routes: emit served-* metrics + proxied.url.served audit

Wrap the proxied handler's ResponseWriter in countingWriter so served-bytes
reports actual bytes written (handles client mid-stream disconnect). After
successful 200 (full) or 206 (range), emit:
- bundle_uri_served_total / pack_uri_served_total {via=proxied} value=1
- bundle_uri_served_bytes / pack_uri_served_bytes {via=proxied} value=<actual>
- proxied.url.served audit event {kind, hash, bytes_served, status_code,
  range_request}

Error paths (404, 416, token failure) do not emit served-* metrics; the
token-invalid metric is added in the following commit.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Token-invalid metric with reason distinction

**Files:**
- Modify: `internal/gateway/proxied_routes.go` (token-failure branch, lines 95-107)
- Modify: `internal/gateway/proxied_routes_test.go` (3 sub-cases)
- Modify: `internal/proxiedurl/token.go` (optional: tighten error wrapping to preserve `ErrKindMismatch` distinguishability) — investigate first.

### Step 5.1 — Verify error-type distinguishability

- [ ] Read `internal/proxiedurl/errs.go` and `token.go` carefully. Confirm that calling code can distinguish via `errors.Is`:
  - `proxiedurl.ErrTokenExpired`
  - `proxiedurl.ErrKindMismatch`
  - any other failure → categorized as `invalid`

If `errors.Is(err, ErrKindMismatch)` does NOT work today because the kind-mismatch error is wrapped under `ErrTokenInvalid` somewhere, fix that first: use `errors.Join` or rework the wrap so the sentinel is reachable. (Per scout report at section 7: ErrKindMismatch IS a distinct sentinel today, returned at token.go:86 — but verify the return path in proxied_routes.go preserves it.)

### Step 5.2 — Write the failing test

- [ ] Add to `proxied_routes_test.go`:

```go
func TestProxiedHandler_TokenInvalid_EmitsMetric_Expired(t *testing.T) {
	// Drive with an expired token. Assert:
	//   - response status 403
	//   - log contains metric proxied_url_token_invalid_total {reason=expired} value=1
}

func TestProxiedHandler_TokenInvalid_EmitsMetric_KindMismatch(t *testing.T) {
	// Drive a /_bundle/ route with a token minted for kind=pack. Assert:
	//   - response status 403
	//   - log contains metric proxied_url_token_invalid_total {reason=kind_mismatch} value=1
}

func TestProxiedHandler_TokenInvalid_EmitsMetric_OtherInvalid(t *testing.T) {
	// Drive with a token whose HMAC signature is corrupted. Assert:
	//   - response status 403
	//   - log contains metric proxied_url_token_invalid_total {reason=invalid} value=1
}
```

### Step 5.3 — Implement

- [ ] In `proxied_routes.go::ServeHTTP`, replace the existing token-failure branch (lines 95-107):

```go
tok, err := proxiedurl.Verify(h.key, tokStr, kind, hash, h.now())
if err != nil {
    reason := "invalid"
    switch {
    case errors.Is(err, proxiedurl.ErrTokenExpired):
        reason = "expired"
    case errors.Is(err, proxiedurl.ErrKindMismatch):
        reason = "kind_mismatch"
    }
    emitMetric(r.Context(), h.logger, "proxied_url_token_invalid_total", 1, "reason", reason)
    switch reason {
    case "expired":
        http.Error(w, "token expired", http.StatusForbidden)
    default:
        http.Error(w, "invalid token", http.StatusForbidden)
    }
    return
}
```

Preserve the existing user-facing response distinction ("token expired" vs "invalid token") — only the metric label is changed.

### Step 5.4 — Verify and commit

- [ ] `go test ./internal/gateway/ -run "TestProxiedHandler_TokenInvalid" -v` — 3 PASS.
- [ ] Full package green.

```bash
git add internal/gateway/proxied_routes.go internal/gateway/proxied_routes_test.go
git commit -m "$(cat <<'EOF'
gateway/proxied_routes: emit proxied_url_token_invalid_total {reason}

Emit proxied_url_token_invalid_total with a 3-value reason label
(expired, kind_mismatch, invalid) on token-verification failure. The
verifier already distinguishes ErrTokenExpired and ErrKindMismatch as
sentinel errors; the third value (invalid) covers signature mismatch,
malformed-token, and hash-mismatch failures collapsed into a single bucket.
HTTP response status (403) and user-facing message (token expired vs
invalid token) are unchanged.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Final verification

- [ ] `go test ./...` — full suite green.
- [ ] `go vet ./...` — clean.
- [ ] Manual sanity: tail logs from `bucketvcs serve --bundle-uri-mode=auto --pack-uri-mode=auto ...` against a test repo, exercise a clone with `git -c protocol.version=2 -c transfer.bundleURI=true clone ...`, and verify the expected `bundle_advertised_total`, `bundle_uri_advertised_total`, `bundle_uri_served_total`/`_bytes`, and the two audit events all appear. Capture sample log output for the Phase 13 operator guide.

## Squash commit message (post-merge to main)

```
M11 Phase 12.5: gateway-side bundle/pack-uri observability (squash of N commits)

Add 8 gateway-side metrics and 2 audit events completing the M11 observability
surface that was carved out of Phase 12 (maintenance-side shipped earlier).

Metrics emitted from internal/gitproto/uploadpack/service.go (advertise time):
- bundle_advertised_total {repo_id, freshness}
- bundle_uri_advertised_total {repo_id, via}
- pack_uri_advertised_total {repo_id, via}

Metrics emitted from internal/gateway/proxied_routes.go (serve time):
- bundle_uri_served_total {via}, bundle_uri_served_bytes {via}
- pack_uri_served_total {via}, pack_uri_served_bytes {via}
- proxied_url_token_invalid_total {reason} (expired|kind_mismatch|invalid)

Audit events (flat-attrs shape, matching maintenance-side):
- bundle.uri.advertised (from serveBundleURI on non-empty response)
- proxied.url.served (from proxiedHandler post-io.Copy success)

Infrastructure: Logger field added to gateway.Options + uploadpack.EngineRequest
(sshd.Options already had one); plumbed through proxiedHandler. countingWriter
wraps the proxied ResponseWriter so served-bytes reports actual bytes written.
internal/gateway/log.go duplicates internal/maintenance/log.go::emitMetric for
package independence; same shape duplicated again in internal/gitproto/uploadpack/
log.go for engine-package emission sites.

Repo_id label intentionally omitted from served-* metrics (proxied endpoint
is hash-keyed via ProxiedKeyResolver; single-repo gateway assumption documented
at proxied_routes.go:18-23). Phase 12 carryover of multi-tenant ProxiedKeyResolver
remains the gating item for that label.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
```
