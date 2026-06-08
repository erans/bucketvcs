# Build-trigger Outbound-Shape Verification Tests — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Hermetic Go tests that drive the real `buildtrigger` delivery worker and assert each target kind (generic/cloudbuild/codebuild) emits a correctly-shaped outbound request — golden body files + valid signature for HTTP targets, decoded `StartBuild` params + SigV4-present for CodeBuild.

**Architecture:** White-box tests in `package buildtrigger`. Each test seeds an authdb, creates a trigger pointed at a local capture server, enqueues a fixed push, runs `StartWorker` with the **real deliverer injected via `cfg.Deliverers`** (plain HTTP client so loopback isn't egress-blocked; real aws-sdk client at a fake `BaseEndpoint` for codebuild), and asserts the captured request. Determinism (fixed push + fixed minted token) makes the rendered body byte-exact, so goldens need no normalization.

**Tech Stack:** Go `testing`, `net/http/httptest`, `aws-sdk-go-v2` (`config`/`credentials`/`codebuild` — already vendored), `internal/webhooks.Sign` (signature recompute), existing `internal/buildtrigger` test helpers (`newTestSvc`, `waitUntil`, `countByStatus`).

**Spec correction baked in:** the design's §2.1 said "`WorkerConfig.MintFn` — test supplies a deterministic `MintFunc`." In the real code `cfg.MintFn` is consulted ONLY when `cfg.Deliverers == nil` (`worker.go:99-100`). Because these tests inject `cfg.Deliverers`, the fixed mint is supplied via each deliverer's own `mintFn` field instead. Everything else in the spec holds.

**Pre-read (real signatures the plan relies on):**
- `internal/buildtrigger/deliver.go` — `Deliverer` iface `Deliver(ctx, tr Trigger, p BuildPayload) (int, error)`; `MintFunc = func(ctx, Trigger, BuildPayload) (string, error)`; `httpDeliverer{client *http.Client; mintFn MintFunc}`.
- `internal/buildtrigger/codebuild.go` — `startBuildAPI` iface (one `StartBuild` method); `codeBuildDeliverer{clientFor func(Trigger)(startBuildAPI,error); mintFn MintFunc}`.
- `internal/buildtrigger/worker.go` — `WorkerConfig` (has `Deliverers map[Kind]Deliverer`); `StartWorker(ctx, svc *Service, cfg WorkerConfig)`; `deliver` uses `cfg.Deliverers[tr.Kind]`.
- `internal/buildtrigger/enqueue.go` — `PushInfo{Tenant,Repo,Actor,TxID,HeadOID string; RefUpdates []RefUpdate}`; `(*Service).Enqueue(ctx, PushInfo) error`.
- `internal/buildtrigger/render.go` — `RenderBody(kind Kind, p BuildPayload, token string) ([]byte, error)`: keys `tenant/repo/actor/tx_id/head_oid/ref_update`, + cloudbuild `ref`/`commit`, + `bvts_token` when non-empty. (Go `json.Marshal` of the underlying `map[string]any` sorts keys → byte-deterministic.)
- `internal/webhooks/sign.go` — `Sign(secret string, t int64, body []byte) string` returns the full `t=<unix>,v1=<hex>` header value.
- Existing test helpers (read `store_test.go` / `worker_test.go`): `newTestSvc(t) (*Service, <db>)`, `waitUntil(t, timeout, cond)`, `(*Service).countByStatus(ctx, status) int`. **Confirm these names/signatures before use; match them exactly.**

---

## Task 1: Test harness + generic wire-shape test + golden

**Files:**
- Create: `internal/buildtrigger/wireshape_test.go`
- Create (generated): `internal/buildtrigger/testdata/generic_body.golden.json`

- [ ] **Step 1: Write the harness + the generic test (it will fail to compile / lack the golden)**

Create `internal/buildtrigger/wireshape_test.go`:

```go
package buildtrigger

import (
	"bytes"
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// -update rewrites the *.golden.json testdata from the captured bodies.
// Regenerate after an intentional shape change:
//   go test ./internal/buildtrigger/ -run WireShape -update
var updateGolden = flag.Bool("update", false, "rewrite *.golden.json testdata")

const wsHeadOID = "1111111111111111111111111111111111111111"
const wsZeroOID = "0000000000000000000000000000000000000000"

// fixedPush is the deterministic push every wire-shape test enqueues.
func fixedPush() PushInfo {
	return PushInfo{
		Tenant: "acme", Repo: "app", Actor: "tester", TxID: "tx-test", HeadOID: wsHeadOID,
		RefUpdates: []RefUpdate{{Refname: "refs/heads/main", OldOID: wsZeroOID, NewOID: wsHeadOID}},
	}
}

// fixedMint returns a constant token so rendered bodies are byte-deterministic.
func fixedMint(context.Context, Trigger, BuildPayload) (string, error) { return "bvts_TESTTOKEN", nil }

// capturedHTTP is one captured request, passed over a channel (race-safe).
type capturedHTTP struct {
	method, path string
	headers      http.Header
	body         []byte
}

// runWorkerOnce enqueues fixedPush and runs StartWorker with the supplied
// deliverers until exactly one delivery is marked delivered, then cancels.
func runWorkerOnce(t *testing.T, svc *Service, deliverers map[Kind]Deliverer) {
	t.Helper()
	ctx := context.Background()
	if err := svc.Enqueue(ctx, fixedPush()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	cfg := WorkerConfig{
		TickInterval:    5 * time.Millisecond,
		ClaimBatchSize:  8,
		BackoffSchedule: []time.Duration{5 * time.Millisecond},
		Deliverers:      deliverers,
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go StartWorker(wctx, svc, cfg)
	waitUntil(t, 3*time.Second, func() bool { return svc.countByStatus(ctx, "delivered") == 1 })
}

// assertSig recomputes webhooks.Sign over body with the trigger secret and the
// t embedded in the header, and asserts it equals the header value (proves we
// signed the exact bytes we sent).
func assertSig(t *testing.T, secret, header string, body []byte) {
	t.Helper()
	parts := strings.SplitN(header, ",", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") {
		t.Fatalf("malformed signature header %q", header)
	}
	tsec, err := strconv.ParseInt(strings.TrimPrefix(parts[0], "t="), 10, 64)
	if err != nil {
		t.Fatalf("bad t in signature %q: %v", header, err)
	}
	if want := webhooks.Sign(secret, tsec, body); want != header {
		t.Fatalf("signature mismatch:\n got: %s\nwant: %s", header, want)
	}
}

// assertGolden compares got against testdata/<name>, or rewrites it under -update.
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run -update to create)", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("body mismatch for %s:\n got: %s\nwant: %s", name, got, want)
	}
}

func TestWireShape_Generic(t *testing.T) {
	recv := make(chan capturedHTTP, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- capturedHTTP{method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: b}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	svc, _ := newTestSvc(t)
	tr, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "g", Kind: KindGeneric,
		Config: Config{URL: srv.URL}, RefInclude: []string{"refs/heads/main"},
		TokenMode: TokenInject,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d := &httpDeliverer{client: srv.Client(), mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindGeneric: d})

	var got capturedHTTP
	select {
	case got = <-recv:
	case <-time.After(3 * time.Second):
		t.Fatal("no request received within 3s — worker did not deliver")
	}
	if got.method != http.MethodPost || got.path != "/" {
		t.Fatalf("got %s %s, want POST /", got.method, got.path)
	}
	if ct := got.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
	if ua := got.headers.Get("User-Agent"); ua != "bucketvcs-buildtrigger/1" {
		t.Errorf("User-Agent=%q, want bucketvcs-buildtrigger/1", ua)
	}
	assertSig(t, tr.Secret, got.headers.Get("BucketVCS-Signature"), got.body)
	assertGolden(t, "generic_body.golden.json", got.body)
}
```

> Before relying on `newTestSvc`/`waitUntil`/`countByStatus`, open `internal/buildtrigger/store_test.go` and `worker_test.go` and confirm the exact names/signatures (they were added in M30 Tasks 4 and 9). If `newTestSvc` returns a different second value or `waitUntil`/`countByStatus` differ, adapt the calls. Do NOT redefine them — reuse.

- [ ] **Step 2: Run to verify it fails (missing golden)**

Run: `go test ./internal/buildtrigger/ -run TestWireShape_Generic -v`
Expected: FAIL — `read golden generic_body.golden.json: ... (run -update to create)`.

- [ ] **Step 3: Generate the golden, then eyeball it**

Run: `go test ./internal/buildtrigger/ -run TestWireShape_Generic -update`
Then inspect the file:
Run: `cat internal/buildtrigger/testdata/generic_body.golden.json`
Expected content (compact JSON, sorted keys) — verify it is exactly:
```json
{"actor":"tester","head_oid":"1111111111111111111111111111111111111111","ref_update":{"refname":"refs/heads/main","old_oid":"0000000000000000000000000000000000000000","new_oid":"1111111111111111111111111111111111111111"},"repo":"app","tenant":"acme","tx_id":"tx-test","bvts_token":"bvts_TESTTOKEN"}
```
(Key order: top-level keys sorted alphabetically by Go's json map marshalling — `actor, bvts_token, head_oid, ref_update, repo, tenant, tx_id`. The nested `ref_update` keys come from the `RefUpdate` struct tags in source order: `refname, old_oid, new_oid`. If the actual generated bytes differ from this, that is the real contract — keep the generated file; just confirm it contains `bvts_token` and NO top-level `ref`/`commit`.)

**Sanity assertions on the golden:** it MUST contain `"bvts_token":"bvts_TESTTOKEN"`, MUST NOT contain a top-level `"ref":` or `"commit":` key (those are cloudbuild-only), and MUST contain `repo`, `tenant`, `head_oid`, `tx_id`, `ref_update`.

- [ ] **Step 4: Run without -update to verify it passes**

Run: `go test ./internal/buildtrigger/ -run TestWireShape_Generic -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/wireshape_test.go internal/buildtrigger/testdata/generic_body.golden.json
git commit -m "test(buildtrigger): worker-driven generic wire-shape test + golden"
```

---

## Task 2: CloudBuild wire-shape test + golden

**Files:**
- Modify: `internal/buildtrigger/wireshape_test.go` (add one test func)
- Create (generated): `internal/buildtrigger/testdata/cloudbuild_body.golden.json`

- [ ] **Step 1: Add the cloudbuild test**

Append to `internal/buildtrigger/wireshape_test.go`:

```go
func TestWireShape_CloudBuild(t *testing.T) {
	recv := make(chan capturedHTTP, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- capturedHTTP{method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: b}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	svc, _ := newTestSvc(t)
	tr, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "cb", Kind: KindCloudBuild,
		Config: Config{URL: srv.URL}, RefInclude: []string{"refs/heads/main"},
		TokenMode: TokenInject,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d := &httpDeliverer{client: srv.Client(), mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindCloudBuild: d})

	var got capturedHTTP
	select {
	case got = <-recv:
	case <-time.After(3 * time.Second):
		t.Fatal("no request received within 3s — worker did not deliver")
	}
	assertSig(t, tr.Secret, got.headers.Get("BucketVCS-Signature"), got.body)
	assertGolden(t, "cloudbuild_body.golden.json", got.body)

	// Spot-check the cloudbuild-only flattened fields are present (defense in
	// depth beyond the golden, so a reviewer sees intent).
	if !bytes.Contains(got.body, []byte(`"ref":"refs/heads/main"`)) {
		t.Errorf("cloudbuild body missing flattened ref: %s", got.body)
	}
	if !bytes.Contains(got.body, []byte(`"commit":"`+wsHeadOID+`"`)) {
		t.Errorf("cloudbuild body missing flattened commit: %s", got.body)
	}
}
```

- [ ] **Step 2: Run to verify it fails (missing golden)**

Run: `go test ./internal/buildtrigger/ -run TestWireShape_CloudBuild -v`
Expected: FAIL — golden not found.

- [ ] **Step 3: Generate + eyeball the golden**

Run: `go test ./internal/buildtrigger/ -run TestWireShape_CloudBuild -update`
Run: `cat internal/buildtrigger/testdata/cloudbuild_body.golden.json`
**Sanity:** MUST contain `"ref":"refs/heads/main"`, `"commit":"1111...1111"`, AND `"bvts_token":"bvts_TESTTOKEN"`, plus the same base fields as generic. (This is the only difference from the generic golden: the two extra top-level keys.)

- [ ] **Step 4: Run without -update to verify it passes**

Run: `go test ./internal/buildtrigger/ -run TestWireShape_CloudBuild -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/wireshape_test.go internal/buildtrigger/testdata/cloudbuild_body.golden.json
git commit -m "test(buildtrigger): cloudbuild wire-shape test + golden (flattened ref/commit)"
```

---

## Task 3: CodeBuild wire-shape test (fake StartBuild endpoint, inline params)

**Files:**
- Modify: `internal/buildtrigger/wireshape_test.go` (add fake + test func + imports)

- [ ] **Step 1: Add the codebuild test + fake endpoint**

Add the AWS imports to the import block of `internal/buildtrigger/wireshape_test.go`:

```go
	"encoding/json"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
```

Append the test:

```go
// sbWire is the minimal decoded shape of a CodeBuild StartBuild JSON request.
type sbWire struct {
	ProjectName                  string `json:"projectName"`
	SourceVersion                string `json:"sourceVersion"`
	EnvironmentVariablesOverride []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
		Type  string `json:"type"`
	} `json:"environmentVariablesOverride"`
}

type sbCapture struct {
	req    sbWire
	target string
	auth   string
}

func TestWireShape_CodeBuild(t *testing.T) {
	recv := make(chan sbCapture, 1)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req sbWire
		_ = json.Unmarshal(b, &req)
		recv <- sbCapture{req: req, target: r.Header.Get("X-Amz-Target"), auth: r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_, _ = w.Write([]byte(`{"build":{"id":"b-1"}}`))
	}))
	defer fake.Close()

	clientFor := func(Trigger) (startBuildAPI, error) {
		cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion("us-east-1"),
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider("AKIATEST", "secret", "")))
		if err != nil {
			return nil, err
		}
		return codebuild.NewFromConfig(cfg, func(o *codebuild.Options) {
			o.BaseEndpoint = aws.String(fake.URL)
		}), nil
	}

	svc, _ := newTestSvc(t)
	if _, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "cbld", Kind: KindCodeBuild,
		Config:    Config{AWSRegion: "us-east-1", AWSProject: "app-release"},
		RefInclude: []string{"refs/heads/main"}, TokenMode: TokenInject,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := &codeBuildDeliverer{clientFor: clientFor, mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindCodeBuild: d})

	var got sbCapture
	select {
	case got = <-recv:
	case <-time.After(3 * time.Second):
		t.Fatal("no StartBuild received within 3s — worker did not deliver")
	}

	// SigV4 actually ran (the SDK signed the request).
	if got.auth == "" {
		t.Error("missing Authorization header — request was not SigV4-signed")
	}
	// AWS JSON 1.1 operation target (date prefix may vary; match the suffix).
	if !strings.HasSuffix(got.target, ".StartBuild") {
		t.Errorf("X-Amz-Target=%q, want a *.StartBuild target", got.target)
	}
	// The StartBuild input params we construct.
	if got.req.ProjectName != "app-release" {
		t.Errorf("projectName=%q, want app-release", got.req.ProjectName)
	}
	if got.req.SourceVersion != wsHeadOID {
		t.Errorf("sourceVersion=%q, want %s", got.req.SourceVersion, wsHeadOID)
	}
	wantEnv := map[string]string{
		"BV_REF":     "refs/heads/main",
		"BV_REPO":    "acme/app",
		"BV_COMMIT":  wsHeadOID,
		"BVTS_TOKEN": "bvts_TESTTOKEN",
	}
	gotEnv := map[string]string{}
	for _, ev := range got.req.EnvironmentVariablesOverride {
		gotEnv[ev.Name] = ev.Value
		if ev.Type != "PLAINTEXT" {
			t.Errorf("env %s type=%q, want PLAINTEXT", ev.Name, ev.Type)
		}
	}
	for k, v := range wantEnv {
		if gotEnv[k] != v {
			t.Errorf("env %s=%q, want %q", k, gotEnv[k], v)
		}
	}
}
```

> Notes: `httptest` serves `http://`; the SDK accepts an `http` `BaseEndpoint` and still signs SigV4. The fake must return a non-error JSON body so the SDK marks the call successful and the worker records `delivered`. The `X-Amz-Target` for CodeBuild is `CodeBuild_20161006.StartBuild`; the suffix match keeps the test robust if the SDK's service date label ever changes. If `EnvironmentVariableType` serializes as something other than the literal `PLAINTEXT`, capture+log it once and adjust the expected string to the real wire value (it is `PLAINTEXT` for `aws-sdk-go-v2/service/codebuild`).

- [ ] **Step 2: Run to verify it fails (or compiles then runs)**

Run: `go test ./internal/buildtrigger/ -run TestWireShape_CodeBuild -v`
Expected: FAIL only if something is off; if the implementation is correct it may pass immediately (there is no golden to seed for codebuild). Acceptable outcomes: a clear assertion failure you then fix, then PASS. If it fails because of the `PLAINTEXT` literal or target string, adjust per the note and re-run.

- [ ] **Step 3: Run with race to verify it passes**

Run: `go test ./internal/buildtrigger/ -run TestWireShape_CodeBuild -race -v`
Expected: PASS — Authorization present, `*.StartBuild` target, projectName/sourceVersion/env all correct.

- [ ] **Step 4: Full package sweep**

Run: `go test ./internal/buildtrigger/ -run WireShape -race -v` (all three pass)
Run: `go test ./internal/buildtrigger/ -race` (whole package still green)
Run: `go vet ./internal/buildtrigger/` and `gofmt -l internal/buildtrigger/` (empty)
Run: `go build ./...`

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/wireshape_test.go
git commit -m "test(buildtrigger): codebuild wire-shape test (SigV4 vs fake BaseEndpoint, StartBuild params)"
```

---

## Self-review notes (for the implementer)

- **Spec coverage:** §1.1 worker-driven capture → `runWorkerOnce` (real `StartWorker` + `cfg.Deliverers`). Three targets inject-only → Tasks 1/2/3. Golden bodies → Tasks 1/2; inline `StartBuild` params → Task 3. Signature validation → `assertSig` (Tasks 1/2). §1.2 out-of-scope items (OIDC/SSH/serve-push/none-mode/retry/real-cloud/SigV4-reverify) are not touched. §2.1 seams used as-is, no production change. §2.2 determinism via `fixedPush`/`fixedMint`. §4 assertions all present. §7 acceptance: hermetic, deterministic, default `go test`, test-only files.
- **Spec deviation (documented above):** mint is injected via the deliverer `mintFn` field, not `cfg.MintFn` (which the worker ignores when `Deliverers` is set). No behavior change to production.
- **Type consistency:** `capturedHTTP`, `runWorkerOnce`, `assertSig`, `assertGolden`, `fixedPush`, `fixedMint`, `wsHeadOID`, `wsZeroOID` defined once in Task 1 and reused in Tasks 2/3. `httpDeliverer{client,mintFn}` and `codeBuildDeliverer{clientFor,mintFn}` match the real struct fields. `webhooks.Sign(secret,t,body)` returns the full header value, so `assertSig` compares to the whole header string.
- **No production code touched** — only `wireshape_test.go` + two `testdata/*.golden.json`.
- **Risk:** the CodeBuild env-var wire type literal (`PLAINTEXT`) and the `X-Amz-Target` suffix are the two SDK-coupled spots; both are guarded (suffix match) or trivially adjustable from a one-time capture. The loopback HTTP client for the http deliverer bypasses the egress policy on purpose (egress is separately tested).
