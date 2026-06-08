# Azure Build Triggers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Azure DevOps as a third build-trigger provider (two kinds: `azurewebhook` and `azurepipelines`) behind the existing M30 `Deliverer` interface, achieving parity with Cloud Build + CodeBuild.

**Architecture:** Two new `Kind`s plug into the existing `map[Kind]Deliverer`. `azurewebhook` reuses the existing `httpDeliverer`, generalized with a per-kind *signature profile* (SHA-1 / `X-Hub-Signature` for Azure; unchanged SHA-256 / `BucketVCS-Signature` for generic/cloudbuild). `azurepipelines` is a new hand-rolled-HTTP deliverer (`azurepipelines.go`) that resolves a named `AzureConnector` (org URL + PAT) and POSTs a `Run Pipeline` request with Basic auth. All M30 cross-cutting machinery (enqueue, ref matching, retry/backoff/dead-letter, queue, CLI lifecycle, token minting, metrics, audit) is reused unchanged. No DB migration.

**Tech Stack:** Go, sqlite/libsql/postgres authdb (M4), `internal/webhooks` egress + signing helpers, `gopkg.in/yaml.v3`, stdlib `crypto/hmac` + `crypto/sha1`, `net/http/httptest` for wire-shape golden tests.

**Spec:** `docs/superpowers/specs/2026-06-08-azure-build-triggers-design.md`

---

## File Structure

**Modify:**
- `internal/buildtrigger/types.go` — two `Kind` consts + Azure `Config` fields.
- `internal/buildtrigger/store.go` — `Create()` validation cases + token-mode default for `azurepipelines`.
- `internal/buildtrigger/deliver.go` — signature-profile generalization of `httpDeliverer` + per-kind webhook URL.
- `internal/buildtrigger/worker.go` — `WorkerConfig.AzureConnectors`, `ProductionDeliverers()` signature + registrations, `StartWorker` internal call.
- `internal/buildtrigger/config.go` — `BuildSection.AzureConnectors`.
- `internal/buildtrigger/apply.go` — Azure fields in `applyTrigger` + `toInput()`.
- `internal/buildtrigger/wireshape_test.go` — Azure golden tests + regression guard.
- `cmd/bucketvcs/build.go` — `--azure-*` flags on `trigger add` + help text.
- `cmd/bucketvcs/serve.go` — parse + wire `azure_connectors`.
- `docs/operator-guides/build-triggers.md` — Azure section.

**Create:**
- `internal/buildtrigger/azurepipelines.go` — `AzureConnector`, `azurePipelinesDeliverer`, body builder, client factory.
- `internal/buildtrigger/azurepipelines_test.go` — unit tests for the body builder + connector resolution.
- `internal/buildtrigger/testdata/azurewebhook_body.golden.json` — golden body (created via `-update`).
- `internal/buildtrigger/testdata/azurepipelines_body.golden.json` — golden body (created via `-update`).
- `examples/azure-pipelines/README.md` — worked operator setup.

---

## Task 1: Azure kinds + Config fields

**Files:**
- Modify: `internal/buildtrigger/types.go`

- [ ] **Step 1: Add the two new Kind constants**

In `internal/buildtrigger/types.go`, change the `const` block:

```go
const (
	KindGeneric        Kind = "generic"
	KindCloudBuild     Kind = "cloudbuild"
	KindCodeBuild      Kind = "codebuild"
	KindAzureWebhook   Kind = "azurewebhook"
	KindAzurePipelines Kind = "azurepipelines"
)
```

- [ ] **Step 2: Add Azure fields to the Config struct**

Replace the `Config` struct in `internal/buildtrigger/types.go`:

```go
// Config is the kind-specific configuration stored as config_json.
type Config struct {
	URL          string `json:"url,omitempty"`
	Secret       string `json:"secret,omitempty"`
	AWSRegion    string `json:"aws_region,omitempty"`
	AWSProject   string `json:"aws_project,omitempty"`
	AWSConnector string `json:"aws_connector,omitempty"`

	// Azure webhook (KindAzureWebhook). Reuses Secret for the HMAC shared
	// secret (SHA-1). AzureSigHeader defaults to "X-Hub-Signature".
	AzureWebhookURL string `json:"azure_webhook_url,omitempty"`
	AzureSigHeader  string `json:"azure_sig_header,omitempty"`

	// Azure Pipelines REST (KindAzurePipelines). AzureConnector names a
	// connector resolved from the server --build-config YAML (holds org URL +
	// PAT); never stored in the authdb.
	AzureConnector  string `json:"azure_connector,omitempty"`
	AzureProject    string `json:"azure_project,omitempty"`
	AzurePipelineID int    `json:"azure_pipeline_id,omitempty"`
}
```

- [ ] **Step 3: Verify it compiles**

Run: `go build ./internal/buildtrigger/`
Expected: success (no usages yet).

- [ ] **Step 4: Commit**

```bash
git add internal/buildtrigger/types.go
git commit -m "feat(buildtrigger): add azurewebhook + azurepipelines kinds and Config fields"
```

---

## Task 2: store.Create validation for Azure kinds

**Files:**
- Modify: `internal/buildtrigger/store.go`
- Test: `internal/buildtrigger/store_test.go`

- [ ] **Step 1: Write failing tests**

Append to `internal/buildtrigger/store_test.go`:

```go
func TestCreate_AzureWebhook_RequiresURL(t *testing.T) {
	svc, _ := newTestSvc(t)
	_, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "aw", Kind: KindAzureWebhook,
		Config: Config{}, // no AzureWebhookURL
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestCreate_AzureWebhook_SecretNotAutoGenerated(t *testing.T) {
	svc, _ := newTestSvc(t)
	tr, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "aw", Kind: KindAzureWebhook,
		Config: Config{AzureWebhookURL: "https://dev.azure.com/Org/_apis/public/distributedtask/webhooks/Hook?api-version=6.0-preview"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tr.Secret != "" {
		t.Errorf("azurewebhook secret should not be auto-generated, got %q", tr.Secret)
	}
	if tr.TokenMode != TokenNone {
		t.Errorf("azurewebhook default token mode = %q, want none", tr.TokenMode)
	}
}

func TestCreate_AzurePipelines_RequiresConnectorProjectAndPipelineID(t *testing.T) {
	svc, _ := newTestSvc(t)
	cases := []Config{
		{AzureProject: "proj", AzurePipelineID: 7},                 // missing connector
		{AzureConnector: "prod", AzurePipelineID: 7},               // missing project
		{AzureConnector: "prod", AzureProject: "proj"},             // missing/zero pipeline id
		{AzureConnector: "prod", AzureProject: "proj", AzurePipelineID: 0},
	}
	for i, cfg := range cases {
		_, err := svc.Create(context.Background(), TriggerInput{
			Tenant: "acme", Repo: "app", Name: "ap", Kind: KindAzurePipelines, Config: cfg,
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("case %d: want ErrInvalidInput, got %v", i, err)
		}
	}
}

func TestCreate_AzurePipelines_DefaultsTokenInject(t *testing.T) {
	svc, _ := newTestSvc(t)
	tr, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "ap", Kind: KindAzurePipelines,
		Config: Config{AzureConnector: "prod", AzureProject: "proj", AzurePipelineID: 7},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tr.TokenMode != TokenInject {
		t.Errorf("azurepipelines default token mode = %q, want inject", tr.TokenMode)
	}
}
```

(If `store_test.go` does not already import `errors`/`context`, add them. `newTestSvc` already exists — it's used by the wire-shape tests.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/buildtrigger/ -run 'Create_Azure' -v`
Expected: FAIL — `unknown kind "azurewebhook"` / `"azurepipelines"` from the existing default case.

- [ ] **Step 3: Add the kinds to the accepted-kind switch**

In `internal/buildtrigger/store.go`, change the early kind-validation switch:

```go
	switch in.Kind {
	case KindGeneric, KindCloudBuild, KindCodeBuild, KindAzureWebhook, KindAzurePipelines:
	default:
		return Trigger{}, fmt.Errorf("%w: unknown kind %q", ErrInvalidInput, in.Kind)
	}
```

- [ ] **Step 4: Default token mode for azurepipelines = inject**

In `internal/buildtrigger/store.go`, change the token-mode default block:

```go
	// Token mode: default none, except codebuild/azurepipelines default inject.
	mode := in.TokenMode
	if mode == "" {
		if in.Kind == KindCodeBuild || in.Kind == KindAzurePipelines {
			mode = TokenInject
		} else {
			mode = TokenNone
		}
	}
```

- [ ] **Step 5: Add per-kind config validation**

In `internal/buildtrigger/store.go`, extend the config `switch in.Kind` block (the one that currently handles `KindGeneric, KindCloudBuild` and `KindCodeBuild`). Add these two cases:

```go
	case KindAzureWebhook:
		if cfg.AzureWebhookURL == "" {
			return Trigger{}, fmt.Errorf("%w: azurewebhook requires azure_webhook_url", ErrInvalidInput)
		}
		if !strings.HasPrefix(cfg.AzureWebhookURL, "http://") && !strings.HasPrefix(cfg.AzureWebhookURL, "https://") {
			return Trigger{}, fmt.Errorf("%w: azurewebhook azure_webhook_url must be http or https", ErrInvalidInput)
		}
		// Secret is operator-supplied (must match the Azure service-connection
		// secret) and is NOT auto-generated; an empty secret means unsigned.
	case KindAzurePipelines:
		if cfg.AzureConnector == "" || cfg.AzureProject == "" || cfg.AzurePipelineID <= 0 {
			return Trigger{}, fmt.Errorf("%w: azurepipelines requires azure_connector, azure_project, and azure_pipeline_id > 0", ErrInvalidInput)
		}
```

Add `"strings"` to the `store.go` import block if not already present.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/buildtrigger/ -run 'Create_Azure' -v`
Expected: PASS (all four tests).

- [ ] **Step 7: Run the full package to confirm no regressions**

Run: `go test ./internal/buildtrigger/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/buildtrigger/store.go internal/buildtrigger/store_test.go
git commit -m "feat(buildtrigger): validate azure kinds in store.Create"
```

---

## Task 3: Signature-profile generalization of httpDeliverer

**Files:**
- Modify: `internal/buildtrigger/deliver.go`

This makes `httpDeliverer` serve `azurewebhook` (SHA-1 / `X-Hub-Signature`, raw-body signing, configurable header, optional/unsigned) while leaving generic/cloudbuild byte-identical. The Task 4 wire-shape tests are the behavioral proof; this task is verified by the existing generic/cloudbuild golden tests staying green.

- [ ] **Step 1: Add imports**

In `internal/buildtrigger/deliver.go`, add to the import block:

```go
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
```

- [ ] **Step 2: Add the signature-profile + URL helpers**

Append to `internal/buildtrigger/deliver.go`:

```go
// sigProfile selects the signature header name and value format for an
// httpDeliverer kind. generic/cloudbuild keep the M15 SHA-256 scheme; Azure
// webhooks use GitHub-compatible HMAC-SHA1 over the raw body.
type sigProfile struct {
	header string
	sign   func(secret string, body []byte, t int64) string
}

func sigProfileFor(tr Trigger) sigProfile {
	if tr.Kind == KindAzureWebhook {
		header := tr.Config.AzureSigHeader
		if header == "" {
			header = "X-Hub-Signature"
		}
		return sigProfile{header: header, sign: signAzureSHA1}
	}
	return sigProfile{
		header: "BucketVCS-Signature",
		sign:   func(secret string, body []byte, t int64) string { return webhooks.Sign(secret, t, body) },
	}
}

// signAzureSHA1 returns "sha1=<hex(HMAC-SHA1(secret, body))>" — the format
// Azure DevOps incoming webhooks verify against the configured header. Azure
// signs the raw body only (no timestamp prefix), so t is ignored.
func signAzureSHA1(secret string, body []byte, _ int64) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write(body)
	return "sha1=" + hex.EncodeToString(mac.Sum(nil))
}

// webhookURL returns the POST target for an httpDeliverer kind. azurewebhook
// uses Config.AzureWebhookURL; generic/cloudbuild use Config.URL.
func webhookURL(tr Trigger) string {
	if tr.Kind == KindAzureWebhook {
		return tr.Config.AzureWebhookURL
	}
	return tr.Config.URL
}
```

- [ ] **Step 3: Rewrite httpDeliverer.Deliver to use the helpers**

Replace the entire `func (d *httpDeliverer) Deliver(...)` body in `internal/buildtrigger/deliver.go` with:

```go
func (d *httpDeliverer) Deliver(ctx context.Context, tr Trigger, p BuildPayload) (int, error) {
	url := webhookURL(tr)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return 0, fmt.Errorf("egress denied: trigger URL scheme must be http or https")
	}
	var token string
	if tr.TokenMode == TokenInject {
		tok, err := d.mintFn(ctx, tr, p)
		if err != nil {
			return 0, fmt.Errorf("mint token: %w", err)
		}
		token = tok
	}
	body, err := RenderBody(tr.Kind, p, token)
	if err != nil {
		return 0, fmt.Errorf("render body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bucketvcs-buildtrigger/1")
	// Sign only when a secret is configured. generic/cloudbuild always have a
	// secret (auto-generated at Create), so their behavior is unchanged;
	// azurewebhook may be unsigned.
	if tr.Config.Secret != "" {
		prof := sigProfileFor(tr)
		req.Header.Set(prof.header, prof.sign(tr.Config.Secret, body, time.Now().Unix()))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 512)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
}
```

- [ ] **Step 4: Run the existing wire-shape tests to prove no regression**

Run: `go test ./internal/buildtrigger/ -run 'WireShape_(Generic|CloudBuild)' -v`
Expected: PASS — generic/cloudbuild bodies and `BucketVCS-Signature` headers unchanged (golden files match byte-for-byte).

- [ ] **Step 5: Run the full package**

Run: `go test ./internal/buildtrigger/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/buildtrigger/deliver.go
git commit -m "feat(buildtrigger): per-kind signature profile in httpDeliverer (SHA-1 for azurewebhook)"
```

---

## Task 4: Azure webhook wire-shape golden test

**Files:**
- Modify: `internal/buildtrigger/wireshape_test.go`
- Create: `internal/buildtrigger/testdata/azurewebhook_body.golden.json`

- [ ] **Step 1: Add the Azure webhook wire-shape test**

Append to `internal/buildtrigger/wireshape_test.go`:

```go
func TestWireShape_AzureWebhook(t *testing.T) {
	recv := make(chan capturedHTTP, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- capturedHTTP{method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: b}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	const secret = "azure-shared-secret"
	svc, _ := newTestSvc(t)
	tr, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "aw", Kind: KindAzureWebhook,
		Config:     Config{AzureWebhookURL: srv.URL, Secret: secret},
		RefInclude: []string{"refs/heads/main"}, TokenMode: TokenInject,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d := &httpDeliverer{client: srv.Client(), mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindAzureWebhook: d})

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
	// Azure default signature header, HMAC-SHA1 over the raw body (no timestamp).
	wantSig := signAzureSHA1(tr.Secret, got.body, 0)
	if sig := got.headers.Get("X-Hub-Signature"); sig != wantSig {
		t.Errorf("X-Hub-Signature=%q, want %q", sig, wantSig)
	}
	assertGolden(t, "azurewebhook_body.golden.json", got.body)
}

func TestWireShape_AzureWebhook_CustomHeaderAndUnsigned(t *testing.T) {
	// Custom header when a secret is set.
	recv := make(chan capturedHTTP, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- capturedHTTP{headers: r.Header.Clone(), body: b}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	svc, _ := newTestSvc(t)
	tr, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "awc", Kind: KindAzureWebhook,
		Config:     Config{AzureWebhookURL: srv.URL, Secret: "s", AzureSigHeader: "X-Custom-Sig"},
		RefInclude: []string{"refs/heads/main"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d := &httpDeliverer{client: srv.Client(), mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindAzureWebhook: d})
	got := <-recv
	if got.headers.Get("X-Custom-Sig") == "" {
		t.Error("custom header X-Custom-Sig not set")
	}
	if got.headers.Get("X-Hub-Signature") != "" {
		t.Error("default X-Hub-Signature should not be set when custom header configured")
	}
	_ = tr

	// Unsigned: no secret → no signature header at all.
	recv2 := make(chan capturedHTTP, 1)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv2 <- capturedHTTP{headers: r.Header.Clone(), body: b}
		w.WriteHeader(200)
	}))
	defer srv2.Close()
	svc2, _ := newTestSvc(t)
	if _, err := svc2.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "awu", Kind: KindAzureWebhook,
		Config:     Config{AzureWebhookURL: srv2.URL}, // no secret
		RefInclude: []string{"refs/heads/main"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	d2 := &httpDeliverer{client: srv2.Client(), mintFn: fixedMint}
	runWorkerOnce(t, svc2, map[Kind]Deliverer{KindAzureWebhook: d2})
	got2 := <-recv2
	if got2.headers.Get("X-Hub-Signature") != "" || got2.headers.Get("X-Custom-Sig") != "" {
		t.Error("unsigned azurewebhook must send no signature header")
	}
}
```

Note: this test calls `signAzureSHA1` (defined in `deliver.go`, same package) to recompute the expected signature, and `assertGolden`/`runWorkerOnce`/`fixedMint`/`capturedHTTP` (already defined in `wireshape_test.go`).

- [ ] **Step 2: Generate the golden file**

Run: `go test ./internal/buildtrigger/ -run 'WireShape_AzureWebhook$' -update`
Expected: PASS; creates `internal/buildtrigger/testdata/azurewebhook_body.golden.json`.

- [ ] **Step 3: Verify the golden content**

Run: `cat internal/buildtrigger/testdata/azurewebhook_body.golden.json`
Expected (identical to the generic body — azurewebhook does no flattening):

```json
{"actor":"tester","bvts_token":"bvts_TESTTOKEN","head_oid":"1111111111111111111111111111111111111111","ref_update":{"refname":"refs/heads/main","old_oid":"0000000000000000000000000000000000000000","new_oid":"1111111111111111111111111111111111111111"},"repo":"app","tenant":"acme","tx_id":"tx-test"}
```

If it differs, stop and investigate — the body shape is wrong.

- [ ] **Step 4: Run without -update to confirm the goldens lock**

Run: `go test ./internal/buildtrigger/ -run 'WireShape' -v`
Expected: PASS for all wire-shape tests (generic, cloudbuild, codebuild, azurewebhook, azurewebhook custom/unsigned).

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/wireshape_test.go internal/buildtrigger/testdata/azurewebhook_body.golden.json
git commit -m "test(buildtrigger): azurewebhook wire-shape golden + custom-header/unsigned cases"
```

---

## Task 5: Azure Pipelines deliverer

**Files:**
- Create: `internal/buildtrigger/azurepipelines.go`
- Create: `internal/buildtrigger/azurepipelines_test.go`

- [ ] **Step 1: Write the failing body-builder + URL unit test**

Create `internal/buildtrigger/azurepipelines_test.go`:

```go
package buildtrigger

import (
	"encoding/json"
	"testing"
)

func TestBuildAzureRunBody_ShapeAndSecretFlag(t *testing.T) {
	p := BuildPayload{
		Tenant: "acme", Repo: "app", Actor: "tester", TxID: "tx-test",
		HeadOID:   "1111111111111111111111111111111111111111",
		RefUpdate: RefUpdate{Refname: "refs/heads/main", OldOID: "0", NewOID: "1111111111111111111111111111111111111111"},
	}
	body, err := buildAzureRunBody(p, "bvts_TESTTOKEN")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	var got struct {
		Resources struct {
			Repositories struct {
				Self struct {
					RefName string `json:"refName"`
				} `json:"self"`
			} `json:"repositories"`
		} `json:"resources"`
		Variables map[string]struct {
			Value    string `json:"value"`
			IsSecret bool   `json:"isSecret"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Resources.Repositories.Self.RefName != "refs/heads/main" {
		t.Errorf("refName=%q, want refs/heads/main", got.Resources.Repositories.Self.RefName)
	}
	want := map[string]string{
		"BV_REPO": "acme/app", "BV_REF": "refs/heads/main",
		"BV_COMMIT": "1111111111111111111111111111111111111111",
		"BV_ACTOR":  "tester", "BV_TX_ID": "tx-test", "BVTS_TOKEN": "bvts_TESTTOKEN",
	}
	for k, v := range want {
		if got.Variables[k].Value != v {
			t.Errorf("var %s=%q, want %q", k, got.Variables[k].Value, v)
		}
	}
	if !got.Variables["BVTS_TOKEN"].IsSecret {
		t.Error("BVTS_TOKEN must have isSecret=true")
	}
	if got.Variables["BV_REPO"].IsSecret {
		t.Error("BV_REPO must not be secret")
	}
}

func TestBuildAzureRunBody_NoTokenOmitsBVTS(t *testing.T) {
	body, err := buildAzureRunBody(BuildPayload{Tenant: "a", Repo: "b",
		RefUpdate: RefUpdate{Refname: "refs/heads/x"}}, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got struct {
		Variables map[string]json.RawMessage `json:"variables"`
	}
	_ = json.Unmarshal(body, &got)
	if _, ok := got.Variables["BVTS_TOKEN"]; ok {
		t.Error("BVTS_TOKEN must be absent when no token injected")
	}
}

func TestAzureRunURL(t *testing.T) {
	got := azureRunURL("https://dev.azure.com/MyOrg/", "MyProject", 42)
	want := "https://dev.azure.com/MyOrg/MyProject/_apis/pipelines/42/runs?api-version=7.1"
	if got != want {
		t.Errorf("url=%q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/buildtrigger/ -run 'BuildAzureRunBody|AzureRunURL' -v`
Expected: FAIL — `undefined: buildAzureRunBody` / `azureRunURL`.

- [ ] **Step 3: Implement azurepipelines.go**

Create `internal/buildtrigger/azurepipelines.go`:

```go
package buildtrigger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// AzureConnector is operator-level Azure DevOps configuration shared across
// triggers via a named connector. It holds the organization URL and a Personal
// Access Token; the PAT is sent as HTTP Basic auth (empty username) and never
// stored in the authdb.
type AzureConnector struct {
	OrgURL string `yaml:"org_url"`
	PAT    string `yaml:"pat"`
}

// azureConn is the resolved per-trigger client view used by the deliverer.
type azureConn struct {
	orgURL string
	pat    string
	client *http.Client
}

// azurePipelinesDeliverer queues an Azure Pipelines run via the Run Pipeline
// REST API. clientFor resolves a named connector to an authenticated client;
// mintFn is injectable so tests can fake token minting.
type azurePipelinesDeliverer struct {
	clientFor func(Trigger) (azureConn, error)
	mintFn    MintFunc
}

func (d *azurePipelinesDeliverer) Deliver(ctx context.Context, tr Trigger, p BuildPayload) (int, error) {
	conn, err := d.clientFor(tr)
	if err != nil {
		return 0, fmt.Errorf("azure connector: %w", err)
	}
	var token string
	if tr.TokenMode == TokenInject {
		tok, err := d.mintFn(ctx, tr, p)
		if err != nil {
			return 0, fmt.Errorf("mint token: %w", err)
		}
		token = tok
	}
	body, err := buildAzureRunBody(p, token)
	if err != nil {
		return 0, fmt.Errorf("build body: %w", err)
	}

	url := azureRunURL(conn.orgURL, tr.Config.AzureProject, tr.Config.AzurePipelineID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bucketvcs-buildtrigger/1")
	req.SetBasicAuth("", conn.pat)

	resp, err := conn.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 512)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
}

// azureRunURL builds the Run Pipeline REST endpoint for an org/project/pipeline.
func azureRunURL(orgURL, project string, pipelineID int) string {
	return strings.TrimRight(orgURL, "/") + "/" + project +
		"/_apis/pipelines/" + strconv.Itoa(pipelineID) + "/runs?api-version=7.1"
}

// azureVar is one entry in the RunPipelineParameters.variables map.
type azureVar struct {
	Value    string `json:"value"`
	IsSecret bool   `json:"isSecret,omitempty"`
}

// buildAzureRunBody renders the RunPipelineParameters JSON: the pushed ref is
// pinned via resources.repositories.self.refName, and push metadata is passed
// as BV_* run variables (matching the CodeBuild env-var convention). The
// injected token, when present, is marked isSecret so Azure masks it in logs.
func buildAzureRunBody(p BuildPayload, token string) ([]byte, error) {
	vars := map[string]azureVar{
		"BV_REPO":   {Value: p.Tenant + "/" + p.Repo},
		"BV_REF":    {Value: p.RefUpdate.Refname},
		"BV_COMMIT": {Value: p.HeadOID},
		"BV_ACTOR":  {Value: p.Actor},
		"BV_TX_ID":  {Value: p.TxID},
	}
	if token != "" {
		vars["BVTS_TOKEN"] = azureVar{Value: token, IsSecret: true}
	}
	body := map[string]any{
		"resources": map[string]any{
			"repositories": map[string]any{
				"self": map[string]any{"refName": p.RefUpdate.Refname},
			},
		},
		"variables": vars,
	}
	return json.Marshal(body)
}

// newAzurePipelinesClientFactory builds a clientFor that resolves the named
// connector for a trigger. A missing connector returns an error (the delivery
// then retries on the backoff schedule and dead-letters on exhaustion). The
// shared *http.Client (egress-gated, built in ProductionDeliverers) is reused
// for every connector.
func newAzurePipelinesClientFactory(connectors map[string]AzureConnector, client *http.Client) func(Trigger) (azureConn, error) {
	return func(tr Trigger) (azureConn, error) {
		conn, ok := connectors[tr.Config.AzureConnector]
		if !ok {
			return azureConn{}, fmt.Errorf("unknown azure connector %q", tr.Config.AzureConnector)
		}
		if conn.OrgURL == "" || conn.PAT == "" {
			return azureConn{}, fmt.Errorf("azure connector %q missing org_url or pat", tr.Config.AzureConnector)
		}
		return azureConn{orgURL: conn.OrgURL, pat: conn.PAT, client: client}, nil
	}
}
```

- [ ] **Step 4: Run the unit tests to verify they pass**

Run: `go test ./internal/buildtrigger/ -run 'BuildAzureRunBody|AzureRunURL' -v`
Expected: PASS.

- [ ] **Step 5: Run the full package**

Run: `go test ./internal/buildtrigger/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/buildtrigger/azurepipelines.go internal/buildtrigger/azurepipelines_test.go
git commit -m "feat(buildtrigger): azurePipelinesDeliverer (Run Pipeline REST, PAT Basic auth)"
```

---

## Task 6: Azure Pipelines wire-shape golden test

**Files:**
- Modify: `internal/buildtrigger/wireshape_test.go`
- Create: `internal/buildtrigger/testdata/azurepipelines_body.golden.json`

- [ ] **Step 1: Add the wire-shape test**

Append to `internal/buildtrigger/wireshape_test.go`:

```go
func TestWireShape_AzurePipelines(t *testing.T) {
	recv := make(chan capturedHTTP, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- capturedHTTP{method: r.Method, path: r.URL.Path + "?" + r.URL.RawQuery, headers: r.Header.Clone(), body: b}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":123,"state":"inProgress"}`))
	}))
	defer srv.Close()

	clientFor := func(Trigger) (azureConn, error) {
		return azureConn{orgURL: srv.URL, pat: "testpat", client: srv.Client()}, nil
	}

	svc, _ := newTestSvc(t)
	if _, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "ap", Kind: KindAzurePipelines,
		Config:     Config{AzureConnector: "prod", AzureProject: "MyProject", AzurePipelineID: 42},
		RefInclude: []string{"refs/heads/main"}, TokenMode: TokenInject,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := &azurePipelinesDeliverer{clientFor: clientFor, mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindAzurePipelines: d})

	var got capturedHTTP
	select {
	case got = <-recv:
	case <-time.After(3 * time.Second):
		t.Fatal("no request received within 3s — worker did not deliver")
	}
	if got.method != http.MethodPost {
		t.Errorf("method=%s, want POST", got.method)
	}
	if got.path != "/MyProject/_apis/pipelines/42/runs?api-version=7.1" {
		t.Errorf("path=%q, want /MyProject/_apis/pipelines/42/runs?api-version=7.1", got.path)
	}
	// Basic auth: empty username + PAT as password.
	user, pass, ok := parseBasicAuth(got.headers.Get("Authorization"))
	if !ok || user != "" || pass != "testpat" {
		t.Errorf("Authorization basic user=%q pass=%q ok=%v, want user=\"\" pass=\"testpat\"", user, pass, ok)
	}
	assertGolden(t, "azurepipelines_body.golden.json", got.body)
}

// parseBasicAuth decodes a "Basic base64(user:pass)" header for assertions.
func parseBasicAuth(h string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(h, prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(h[len(prefix):])
	if err != nil {
		return "", "", false
	}
	u, p, found := strings.Cut(string(raw), ":")
	return u, p, found
}
```

Add `"encoding/base64"` to the `wireshape_test.go` import block.

- [ ] **Step 2: Run to verify it fails (golden missing)**

Run: `go test ./internal/buildtrigger/ -run 'WireShape_AzurePipelines' -v`
Expected: FAIL — `read golden azurepipelines_body.golden.json ... (run -update to create)`.

- [ ] **Step 3: Generate the golden file**

Run: `go test ./internal/buildtrigger/ -run 'WireShape_AzurePipelines' -update`
Expected: PASS; creates `internal/buildtrigger/testdata/azurepipelines_body.golden.json`.

- [ ] **Step 4: Verify the golden content**

Run: `cat internal/buildtrigger/testdata/azurepipelines_body.golden.json`
Expected exactly:

```json
{"resources":{"repositories":{"self":{"refName":"refs/heads/main"}}},"variables":{"BV_ACTOR":{"value":"tester"},"BV_COMMIT":{"value":"1111111111111111111111111111111111111111"},"BV_REF":{"value":"refs/heads/main"},"BV_REPO":{"value":"acme/app"},"BV_TX_ID":{"value":"tx-test"},"BVTS_TOKEN":{"value":"bvts_TESTTOKEN","isSecret":true}}
```

If it differs, stop and investigate.

- [ ] **Step 5: Lock the golden (run without -update)**

Run: `go test ./internal/buildtrigger/ -run 'WireShape' -v`
Expected: PASS for all wire-shape tests.

- [ ] **Step 6: Commit**

```bash
git add internal/buildtrigger/wireshape_test.go internal/buildtrigger/testdata/azurepipelines_body.golden.json
git commit -m "test(buildtrigger): azurepipelines wire-shape golden (URL, Basic auth, body)"
```

---

## Task 7: Wire deliverers into the worker

**Files:**
- Modify: `internal/buildtrigger/worker.go`

- [ ] **Step 1: Add AzureConnectors to WorkerConfig**

In `internal/buildtrigger/worker.go`, add a field to `WorkerConfig` right after the existing `Connectors map[string]AWSConnector` field:

```go
	// AzureConnectors is the named Azure DevOps connector map used to build the
	// production Azure Pipelines deliverer (Deliverers == nil).
	AzureConnectors map[string]AzureConnector
```

- [ ] **Step 2: Update the StartWorker internal ProductionDeliverers call**

In `internal/buildtrigger/worker.go`, change the line inside `StartWorker` (currently `cfg.Deliverers = ProductionDeliverers(cfg.MintFn, cfg.Connectors, cfg.Egress, cfg.HTTPTimeout)`) to:

```go
		cfg.Deliverers = ProductionDeliverers(cfg.MintFn, cfg.Connectors, cfg.AzureConnectors, cfg.Egress, cfg.HTTPTimeout)
```

- [ ] **Step 3: Update ProductionDeliverers signature + registrations**

In `internal/buildtrigger/worker.go`, replace the whole `ProductionDeliverers` function:

```go
// ProductionDeliverers builds the per-kind deliverer set used in production:
// generic + cloudbuild + azurewebhook share a signed-JSON HTTP deliverer over
// an egress-gated client (signature scheme selected per-kind); codebuild uses
// the SigV4 StartBuild deliverer; azurepipelines uses the PAT Run Pipeline
// REST deliverer.
func ProductionDeliverers(mint MintFunc, connectors map[string]AWSConnector, azureConnectors map[string]AzureConnector, egress *webhooks.EgressPolicy, timeout time.Duration) map[Kind]Deliverer {
	if egress == nil {
		egress = &webhooks.EgressPolicy{} // secure default: deny private/loopback/link-local
	}
	client := webhooks.NewHTTPClient(egress, timeout)
	httpD := &httpDeliverer{
		client: client,
		mintFn: mint,
	}
	cbD := &codeBuildDeliverer{
		clientFor: newCodeBuildClientFactory(connectors),
		mintFn:    mint,
	}
	azD := &azurePipelinesDeliverer{
		clientFor: newAzurePipelinesClientFactory(azureConnectors, client),
		mintFn:    mint,
	}
	return map[Kind]Deliverer{
		KindGeneric:        httpD,
		KindCloudBuild:     httpD,
		KindAzureWebhook:   httpD,
		KindCodeBuild:      cbD,
		KindAzurePipelines: azD,
	}
}
```

- [ ] **Step 4: Verify build + full package tests**

Run: `go build ./... && go test ./internal/buildtrigger/`
Expected: PASS (the serve.go caller is updated in Task 9; `go build ./...` will fail until then ONLY if serve.go calls the old signature — fix order: if `go build ./...` errors on serve.go here, that's expected and resolved in Task 9. To keep this task green, run the package build instead:)

Run: `go build ./internal/buildtrigger/ && go test ./internal/buildtrigger/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/worker.go
git commit -m "feat(buildtrigger): register azure deliverers in ProductionDeliverers"
```

---

## Task 8: Server config — azure_connectors

**Files:**
- Modify: `internal/buildtrigger/config.go`
- Test: `internal/buildtrigger/config_test.go`

- [ ] **Step 1: Write the failing config-parse test**

Append to `internal/buildtrigger/config_test.go`:

```go
func TestParseServeConfig_AzureConnectors(t *testing.T) {
	yaml := `
build:
  azure_connectors:
    prod:
      org_url: https://dev.azure.com/MyOrg
      pat: secret-pat
`
	cfg, err := ParseServeConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, ok := cfg.Build.AzureConnectors["prod"]
	if !ok {
		t.Fatal("azure connector \"prod\" not parsed")
	}
	if c.OrgURL != "https://dev.azure.com/MyOrg" || c.PAT != "secret-pat" {
		t.Errorf("parsed connector = %+v", c)
	}
}
```

(If `config_test.go` doesn't exist, create it with `package buildtrigger` and `import "testing"`.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/buildtrigger/ -run 'ParseServeConfig_Azure' -v`
Expected: FAIL — `cfg.Build.AzureConnectors` undefined.

- [ ] **Step 3: Add the field to BuildSection**

In `internal/buildtrigger/config.go`, add to `BuildSection`:

```go
// BuildSection groups all build-trigger operator configuration.
type BuildSection struct {
	Defaults        Defaults                  `yaml:"defaults"`
	AWSConnectors   map[string]AWSConnector   `yaml:"aws_connectors"`
	AzureConnectors map[string]AzureConnector `yaml:"azure_connectors"`
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/buildtrigger/ -run 'ParseServeConfig' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/config.go internal/buildtrigger/config_test.go
git commit -m "feat(buildtrigger): parse azure_connectors from --build-config YAML"
```

---

## Task 9: CLI flags + apply schema + serve wiring

**Files:**
- Modify: `cmd/bucketvcs/build.go`
- Modify: `internal/buildtrigger/apply.go`
- Modify: `cmd/bucketvcs/serve.go`

- [ ] **Step 1: Add apply YAML fields + toInput mapping**

In `internal/buildtrigger/apply.go`, add fields to `applyTrigger`:

```go
	AzureWebhookURL string   `yaml:"azure_webhook_url"`
	AzureSigHeader  string   `yaml:"azure_sig_header"`
	AzureConnector  string   `yaml:"azure_connector"`
	AzureProject    string   `yaml:"azure_project"`
	AzurePipelineID int      `yaml:"azure_pipeline_id"`
```

(Place them alongside the existing `AWSConnector` field, before `RefInclude`.)

Then in `toInput`, extend the `Config` literal:

```go
		Config: Config{
			URL:             at.URL,
			Secret:          at.Secret,
			AWSRegion:       at.AWSRegion,
			AWSProject:      at.AWSProject,
			AWSConnector:    at.AWSConnector,
			AzureWebhookURL: at.AzureWebhookURL,
			AzureSigHeader:  at.AzureSigHeader,
			AzureConnector:  at.AzureConnector,
			AzureProject:    at.AzureProject,
			AzurePipelineID: at.AzurePipelineID,
		},
```

- [ ] **Step 2: Add a failing apply round-trip test**

Append to `internal/buildtrigger/apply_test.go`:

```go
func TestApply_AzureKinds(t *testing.T) {
	svc, _ := newTestSvc(t)
	doc := `
triggers:
  - tenant: acme
    repo: app
    name: aw
    kind: azurewebhook
    azure_webhook_url: https://dev.azure.com/Org/_apis/public/distributedtask/webhooks/Hook?api-version=6.0-preview
    secret: shared
  - tenant: acme
    repo: app
    name: ap
    kind: azurepipelines
    azure_connector: prod
    azure_project: MyProject
    azure_pipeline_id: 42
`
	res, err := Apply(context.Background(), svc, []byte(doc), false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Created != 2 {
		t.Fatalf("created=%d, want 2", res.Created)
	}
	ap, err := svc.findByName(context.Background(), "acme", "app", "ap")
	if err != nil {
		t.Fatalf("findByName: %v", err)
	}
	if ap.Config.AzurePipelineID != 42 || ap.Config.AzureProject != "MyProject" || ap.Config.AzureConnector != "prod" {
		t.Errorf("azurepipelines config = %+v", ap.Config)
	}
}
```

Run: `go test ./internal/buildtrigger/ -run 'Apply_AzureKinds' -v`
Expected: PASS (toInput now maps the fields).

- [ ] **Step 3: Add CLI flags to `trigger add`**

In `cmd/bucketvcs/build.go` `runBuildTriggerAdd`, add these flag declarations next to the existing `awsConnector` flag:

```go
	azureWebhookURL := fs.String("azure-webhook-url", "", "Azure incoming-webhook URL (azurewebhook)")
	azureSigHeader := fs.String("azure-sig-header", "", "Azure webhook signature header (azurewebhook, default X-Hub-Signature)")
	azureConnector := fs.String("azure-connector", "", "Azure connector name (azurepipelines)")
	azureProject := fs.String("azure-project", "", "Azure DevOps project (azurepipelines)")
	azurePipelineID := fs.Int("azure-pipeline-id", 0, "Azure pipeline ID (azurepipelines)")
```

Then extend the `Config` literal in the `in := buildtrigger.TriggerInput{...}` construction:

```go
		Config: buildtrigger.Config{
			URL:             *urlFlag,
			Secret:          *secret,
			AWSRegion:       *awsRegion,
			AWSProject:      *awsProject,
			AWSConnector:    *awsConnector,
			AzureWebhookURL: *azureWebhookURL,
			AzureSigHeader:  *azureSigHeader,
			AzureConnector:  *azureConnector,
			AzureProject:    *azureProject,
			AzurePipelineID: *azurePipelineID,
		},
```

- [ ] **Step 4: Update the CLI usage/help text**

In `cmd/bucketvcs/build.go`, update the usage block (around line 23-25) to document the new kinds. Change the `--kind` line and add Azure rows:

```go
//  trigger add     --auth-db=<path> --tenant=<t> --repo=<r> --name=<n> --kind=<generic|cloudbuild|codebuild|azurewebhook|azurepipelines>
//                  generic/cloudbuild: --url=<u> [--secret=<s>]
//                  codebuild:          --aws-region=<r> --aws-project=<p> [--aws-connector=<c>]
//                  azurewebhook:       --azure-webhook-url=<u> [--secret=<s>] [--azure-sig-header=<h>]
//                  azurepipelines:     --azure-connector=<c> --azure-project=<p> --azure-pipeline-id=<n>
```

(Match the existing comment/format style of that usage block exactly — adjust the leading `//` vs string-literal form to whatever the file uses.)

- [ ] **Step 5: Wire serve.go**

In `cmd/bucketvcs/serve.go`, near the existing `var buildConnectors map[string]buildtrigger.AWSConnector` (line ~517), add:

```go
	var buildAzureConnectors map[string]buildtrigger.AzureConnector
```

In the config-load block where `buildConnectors = cfg.Build.AWSConnectors` is set (line ~531), add:

```go
		buildAzureConnectors = cfg.Build.AzureConnectors
```

In the worker-start block (lines ~720-725), set the new config field and pass it to ProductionDeliverers:

```go
			bcfg := buildtrigger.DefaultWorkerConfig()
			bcfg.MintFn = buildtrigger.NewMintFunc(authS, logger)
			bcfg.Connectors = buildConnectors
			bcfg.AzureConnectors = buildAzureConnectors
			bcfg.Deliverers = buildtrigger.ProductionDeliverers(bcfg.MintFn, buildConnectors, buildAzureConnectors, webhookEgress, bcfg.HTTPTimeout)
			go buildtrigger.StartWorker(serveCtx, buildSvc, bcfg)
```

- [ ] **Step 6: Full build + test**

Run: `go build ./... && go test ./internal/buildtrigger/ ./cmd/bucketvcs/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/bucketvcs/build.go internal/buildtrigger/apply.go internal/buildtrigger/apply_test.go cmd/bucketvcs/serve.go
git commit -m "feat(buildtrigger): azure CLI flags, apply schema, serve wiring"
```

---

## Task 10: Documentation + example

**Files:**
- Modify: `docs/operator-guides/build-triggers.md`
- Create: `examples/azure-pipelines/README.md`

- [ ] **Step 1: Add an Azure section to the operator guide**

Open `docs/operator-guides/build-triggers.md`, find where the CodeBuild section ends, and add a new section after it. Use this content (adapt heading numbering to match the file's existing scheme):

````markdown
## Azure DevOps

BucketVCS supports two Azure modes, mirroring the two integration styles used
for Cloud Build and CodeBuild:

- **`azurewebhook`** — the Cloud Build twin. BucketVCS POSTs a JSON body to an
  Azure Pipelines *incoming-webhook* URL, signed with HMAC-SHA1 in the
  `X-Hub-Signature` header. BucketVCS holds **no** Azure credential — only a
  shared secret that must match the one configured on the Azure service
  connection.
- **`azurepipelines`** — the CodeBuild twin. BucketVCS calls the
  `Run Pipeline` REST API directly, authenticating with a Personal Access Token
  resolved from a **named connector** in `--build-config` (the PAT is never
  stored in the authdb).

### azurewebhook setup

1. In Azure DevOps: **Project Settings → Service connections → New → Incoming
   WebHook.** Set a **WebHook Name**, a **Secret**, and the **HTTP header name**
   (default `X-Hub-Signature`).
2. Reference it in your pipeline YAML and run the pipeline once so the trigger
   arms:

   ```yaml
   resources:
     webhooks:
       - webhook: MyHook
         connection: MyIncomingWebhookConnection
   ```

3. Create the trigger (the secret must equal the service-connection secret):

   ```bash
   bucketvcs build trigger add --auth-db=/var/lib/bucketvcs/auth.db \
     --tenant=acme --repo=app --name=azure-ci --kind=azurewebhook \
     --azure-webhook-url='https://dev.azure.com/MyOrg/_apis/public/distributedtask/webhooks/MyHook?api-version=6.0-preview' \
     --secret='<same-secret-as-service-connection>' \
     --ref-include='refs/heads/main'
   ```

   The pushed payload is available inside the pipeline as
   `${{ parameters.MyHook.<jsonPath> }}` (e.g. `${{ parameters.MyHook.head_oid }}`).

   > **Note:** the signature is HMAC-**SHA1** (`sha1=<hex>`), not SHA-256, and
   > covers the raw body only. Omitting `--secret` sends the webhook **unsigned**
   > (Azure permits this; BucketVCS does not auto-generate an Azure secret).

### azurepipelines setup

1. Create a PAT in Azure DevOps with **Build → Read & execute** scope.
2. Add a connector to `--build-config`:

   ```yaml
   build:
     azure_connectors:
       prod:
         org_url: https://dev.azure.com/MyOrg
         pat: ${AZURE_DEVOPS_PAT}
   ```

3. Create the trigger:

   ```bash
   bucketvcs build trigger add --auth-db=/var/lib/bucketvcs/auth.db \
     --tenant=acme --repo=app --name=azure-run --kind=azurepipelines \
     --azure-connector=prod --azure-project=MyProject --azure-pipeline-id=42 \
     --ref-include='refs/heads/main'
   ```

   BucketVCS POSTs to
   `{org_url}/MyProject/_apis/pipelines/42/runs?api-version=7.1`, pins the run to
   the pushed ref via `resources.repositories.self.refName`, and passes push
   metadata as `BV_*` run variables (`BV_REPO`, `BV_REF`, `BV_COMMIT`,
   `BV_ACTOR`, `BV_TX_ID`). With `--token-mode=inject` (the default for this
   kind), a short-lived `BVTS_TOKEN` variable is added with `isSecret: true`.

### Error handling

A missing/misconfigured connector, a 401/404, or any non-2xx response is
retried on the standard backoff schedule (1m, 30m, 2h, 12h) and then
dead-lettered — observe via the `build_trigger_deadletter_total` metric and the
`build.trigger.deadletter` audit event, and recover with
`bucketvcs build delivery replay`.
````

- [ ] **Step 2: Create the example README**

Create `examples/azure-pipelines/README.md`:

````markdown
# Azure DevOps build-trigger examples

Two ways to start an Azure build on push, mirroring the Cloud Build and
CodeBuild examples.

## 1. Incoming webhook (`azurewebhook`)

`azure-pipelines.yml` in your Azure repo:

```yaml
resources:
  webhooks:
    - webhook: BucketVCS
      connection: BucketVCSIncomingWebhook   # Incoming WebHook service connection

trigger: none

steps:
  - script: |
      echo "ref=${{ parameters.BucketVCS.ref_update.refname }}"
      echo "commit=${{ parameters.BucketVCS.head_oid }}"
    displayName: Show pushed commit
```

Register the trigger:

```bash
bucketvcs build trigger add --auth-db=$AUTH_DB \
  --tenant=acme --repo=app --name=azure-ci --kind=azurewebhook \
  --azure-webhook-url="https://dev.azure.com/$ORG/_apis/public/distributedtask/webhooks/BucketVCS?api-version=6.0-preview" \
  --secret="$WEBHOOK_SECRET" \
  --ref-include='refs/heads/main'
```

## 2. Run Pipeline REST (`azurepipelines`)

`--build-config` connector:

```yaml
build:
  azure_connectors:
    prod:
      org_url: https://dev.azure.com/MyOrg
      pat: ${AZURE_DEVOPS_PAT}
```

Register the trigger:

```bash
bucketvcs build trigger add --auth-db=$AUTH_DB \
  --tenant=acme --repo=app --name=azure-run --kind=azurepipelines \
  --azure-connector=prod --azure-project=MyProject --azure-pipeline-id=42 \
  --ref-include='refs/heads/main'
```

Your pipeline reads the push via the injected variables: `$(BV_REF)`,
`$(BV_COMMIT)`, `$(BV_REPO)`, and (when token injection is on) `$(BVTS_TOKEN)`
for cloning the repo.
````

- [ ] **Step 3: Commit**

```bash
git add docs/operator-guides/build-triggers.md examples/azure-pipelines/README.md
git commit -m "docs(buildtrigger): Azure operator guide section + examples"
```

---

## Task 11: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full build**

Run: `go build ./...`
Expected: success, no errors.

- [ ] **Step 2: Full buildtrigger + CLI test suite**

Run: `go test ./internal/buildtrigger/ ./cmd/bucketvcs/ -v`
Expected: PASS, including all five wire-shape tests (generic, cloudbuild, codebuild, azurewebhook, azurepipelines) and the Azure store/config/apply tests.

- [ ] **Step 3: Vet**

Run: `go vet ./internal/buildtrigger/ ./cmd/bucketvcs/`
Expected: no findings.

- [ ] **Step 4: Confirm golden regression guard**

Run: `git diff --stat -- internal/buildtrigger/testdata/`
Expected: only the two NEW golden files (`azurewebhook_body.golden.json`, `azurepipelines_body.golden.json`) appear as additions across the branch; the existing `generic_body.golden.json` and `cloudbuild_body.golden.json` are unchanged (proves the signature-profile refactor was behavior-preserving).

- [ ] **Step 5: Request code review**

Use the superpowers:requesting-code-review skill (or `/roborev-review-branch`) before merging.
````
