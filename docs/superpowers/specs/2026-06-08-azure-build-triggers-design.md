# Azure build triggers (Azure DevOps CI integration)

Date: 2026-06-08
Builds on: M30 build triggers (kinds, delivery queue, worker, token minting, CLI,
wireshape golden tests), M25 egress policy (SSRF guard), M22 OIDC/short-lived
token minting, M16 `**`-aware ref matcher.

## 1. Goals

Give Azure DevOps users the **same two integration philosophies** that GCP and
AWS already have under M30, by adding Azure as a third provider behind the
existing `Deliverer` interface. No new delivery machinery — the M30 enqueue,
ref-matching, retry/backoff/dead-letter, queue, CLI lifecycle, token minting,
metrics, and audit events are all provider-agnostic and are reused unchanged.

The two existing M30 modes are two different shapes, and Azure maps onto them
one-to-one:

| M30 mode | Philosophy | Azure twin (this milestone) |
|---|---|---|
| `cloudbuild` / `generic` | Signed HMAC POST to a URL; BucketVCS holds **no** cloud credential; the cloud's own trigger system catches it | **`azurewebhook`** — Azure Pipelines incoming-webhook (HMAC-SHA1 `X-Hub-Signature` → public URL) |
| `codebuild` | Direct authenticated API call; BucketVCS holds a credential (named connector) | **`azurepipelines`** — Azure Pipelines `Run Pipeline` REST (PAT via named connector) |

This is the minimal set that achieves **parity** with Cloud Build + CodeBuild.
It is explicitly *not* an attempt to cover every Azure build surface (see §1.2).

### 1.1 In scope

- **Two new trigger kinds:**
  - `azurewebhook` — signed JSON POST to an Azure Pipelines incoming-webhook
    URL. Reuses the existing `httpDeliverer`, generalized with a **signature
    profile** (algorithm + header name) so generic/cloudbuild keep SHA-256 /
    `BucketVCS-Signature` and Azure gets SHA-1 / `X-Hub-Signature`.
  - `azurepipelines` — authenticated `Run Pipeline` REST call. New
    `azurePipelinesDeliverer` (new file `azurepipelines.go`), parallels
    `codebuild.go`. Resolves a **named `AzureConnector`** (org URL + PAT) from
    the server `--build-config` YAML; the PAT never lives in the authdb.
- **PAT authentication** for the REST path (HTTP Basic, empty username + PAT as
  password).
- **Named connector** model for the PAT, mirroring `AWSConnector`: trigger
  stores only a connector *name*; the credential + org URL live server-side.
- **Reuse of all M30 cross-cutting machinery**: ref include/exclude filtering,
  `TokenMode` mint-and-inject, durable retry/backoff/dead-letter/replay/reclaim,
  `delivery list/show/replay`, the `_build` system user, metrics, audit events.
- **CLI**: new `--azure-*` flags on `build trigger add`; `apply -f` schema +
  `toInput()` extended; all other subcommands work unchanged.
- **Wireshape golden tests** for both new kinds; a regression guard that
  existing generic/cloudbuild goldens stay byte-identical after the
  signature-profile refactor.
- **Docs**: Azure section in the operator guide + `examples/azure-pipelines/`.

### 1.2 Out of scope (deferred, documented)

- **ACR Tasks `Schedule Run`** (container-image builds, ARM bearer auth). Not
  parity — Cloud Build/CodeBuild are general-purpose build services. A future
  milestone.
- **Entra service-principal / managed-identity auth** for the REST path. PAT
  only for now; the connector struct is shaped so an auth-mode field can be
  added later without a schema change to triggers.
- **Legacy `Builds - Queue` endpoint.** We use `Run Pipeline`
  (`_apis/pipelines/{id}/runs`) only.
- **`templateParameters`** on the REST body. We use run `variables` (no
  pipeline-YAML pre-declaration required).
- **Azure-specific permanent-vs-retryable error classification.** All delivery
  errors (incl. missing connector, 401/404) retry then dead-letter, same as
  today's HTTP deliverer. A permanent-failure fast-path is a future worker
  enhancement.
- **HMAC algorithm as a knob.** Azure computes SHA-1 for incoming webhooks; only
  the header *name* is configurable.

## 2. Architecture overview

```
git push ──▶ receive-pack completion (Step 14, M30, unchanged)
             eng.BuildTriggers.Enqueue(ctx, push)
                   │  (provider-agnostic: ref match → INSERT delivery rows)
                   ▼
             build_trigger_deliveries  (unchanged schema, new kind values)
                   │
             worker.go claim/backoff loop (unchanged)
                   │  Deliverers[trigger.Kind].Deliver(...)
                   ▼
   ┌───────────────┴───────────────────────────────────────┐
   │ KindAzureWebhook   → httpDeliverer (sig profile = SHA1) │
   │ KindAzurePipelines → azurePipelinesDeliverer (PAT REST) │
   └────────────────────────────────────────────────────────┘
```

The only edits to existing shared code are: two new `Kind` constants, the
signature-profile generalization of `httpDeliverer`, and two new entries in
`ProductionDeliverers()`. Everything else is additive.

## 3. Kinds & deliverer wiring

`internal/buildtrigger/types.go`:

```go
KindAzureWebhook   Kind = "azurewebhook"
KindAzurePipelines Kind = "azurepipelines"
```

`internal/buildtrigger/worker.go` `ProductionDeliverers()` gains the Azure
connector map and two registrations:

```go
func ProductionDeliverers(mint MintFunc, aws map[string]AWSConnector,
    azure map[string]AzureConnector, egress *webhooks.EgressPolicy,
    timeout time.Duration) map[Kind]Deliverer {
    ...
    httpD := &httpDeliverer{client: webhooks.NewHTTPClient(egress, timeout), mintFn: mint}
    azD := &azurePipelinesDeliverer{
        clientFor: newAzurePipelinesClientFactory(azure),
        client:    webhooks.NewHTTPClient(egress, timeout),
        mintFn:    mint,
    }
    return map[Kind]Deliverer{
        KindGeneric:        httpD,
        KindCloudBuild:     httpD,
        KindCodeBuild:      cbD,
        KindAzureWebhook:   httpD,   // sig profile selected inside httpDeliverer by Kind
        KindAzurePipelines: azD,
    }
}
```

## 4. Config schema & connector model

### 4.1 `Config` additions (`types.go`), all `omitempty`

```go
// Azure webhook (KindAzureWebhook); reuses existing Config.Secret for the HMAC secret
AzureWebhookURL string `json:"azure_webhook_url,omitempty"`
AzureSigHeader  string `json:"azure_sig_header,omitempty"` // default "X-Hub-Signature"

// Azure Pipelines REST (KindAzurePipelines)
AzureConnector  string `json:"azure_connector,omitempty"`  // name → server --build-config
AzureProject    string `json:"azure_project,omitempty"`    // project name or ID
AzurePipelineID int    `json:"azure_pipeline_id,omitempty"`
```

### 4.2 Connector (server `--build-config` YAML; never in authdb)

```go
type AzureConnector struct {
    OrgURL string // e.g. https://dev.azure.com/MyOrg
    PAT    string // Personal Access Token; Basic auth, empty username
}
```

YAML, alongside the existing `aws_connectors:`:

```yaml
azure_connectors:
  prod:
    org_url: https://dev.azure.com/MyOrg
    pat: ${AZURE_DEVOPS_PAT}   # same env-substitution convention as aws_connectors
```

Resolved at worker startup into `map[string]AzureConnector` and passed to
`ProductionDeliverers()`. The deliverer looks up `Config.AzureConnector` by
name; a missing name returns an error. The M30 worker has no permanent-failure
fast-path, so that error flows through the normal backoff schedule and
dead-letters on exhaustion (an operator who deletes a still-referenced
connector will see those triggers dead-letter — surfaced via the existing
dead-letter metric/audit).

### 4.3 Validation at `store.Create()` (extends the existing `switch kind`)

- `KindAzureWebhook`: require `AzureWebhookURL` and run the existing
  HTTPS/egress scheme check the generic kind already applies; `Secret` optional
  (Azure permits unsigned webhooks). Unlike generic/cloudbuild, the secret is
  **not auto-generated** — it must match the secret configured on the Azure
  incoming-webhook service connection, so an omitted secret means "unsigned",
  not "generate one". `AzureSigHeader` defaults to `X-Hub-Signature` when empty.
- `KindAzurePipelines`: require `AzureConnector`, `AzureProject`, and
  `AzurePipelineID > 0`. Default `TokenMode` is `inject` (mirroring its
  CodeBuild twin); `azurewebhook` defaults to `none` (mirroring generic).

## 5. Webhook deliverer — signature-profile generalization

Today `httpDeliverer` hardcodes SHA-256 + `BucketVCS-Signature` +
`t=<unix>,v1=<hex>` via `webhooks.Sign()`. Introduce:

```go
type sigProfile struct {
    header string
    sign   func(secret string, body []byte, t int64) string
}
```

Profiles selected by `Kind`:

- **generic / cloudbuild** (unchanged): header `BucketVCS-Signature`, value
  `t=<unix>,v1=<hmac-sha256>` — byte-for-byte identical to today.
- **azurewebhook**: header `X-Hub-Signature` (or `Config.AzureSigHeader`
  override), value `sha1=<hmac-sha1(body)>`. Azure signs the **raw body only**,
  so this profile's `sign` ignores `t`.

Body is the **same `RenderBody` output** already produced
(tenant/repo/actor/tx_id/head_oid/ref_update [+ injected token]); operators map
fields inside the pipeline via `${{ parameters.<hook>.<jsonPath> }}`. Cloud
Build's flattened top-level `ref`/`commit` fields are Cloud-Build-only and are
**not** added to the Azure body.

**Unsigned case:** when no `Secret` is set, no signature header is sent (Azure
permits this), matching generic-with-no-secret behavior today.

## 6. Pipelines REST deliverer (`azurepipelines.go`)

```go
type azurePipelinesDeliverer struct {
    clientFor func(Trigger) (azurePipelinesAPI, error)
    client    *http.Client // egress-policy-bound
    mintFn    MintFunc
}
```

**Endpoint** (connector `OrgURL` + trigger fields):

```
POST {OrgURL}/{AzureProject}/_apis/pipelines/{AzurePipelineID}/runs?api-version=7.1
```

**Auth:** `Authorization: Basic base64(":" + PAT)` (empty username).

**Body** (`RunPipelineParameters`):

```json
{
  "resources": { "repositories": { "self": { "refName": "refs/heads/main" } } },
  "variables": {
    "BV_REPO":    { "value": "acme/app" },
    "BV_REF":     { "value": "refs/heads/main" },
    "BV_COMMIT":  { "value": "a1b2c3..." },
    "BV_ACTOR":   { "value": "alice" },
    "BV_TX_ID":   { "value": "..." },
    "BVTS_TOKEN": { "value": "bvts_...", "isSecret": true }
  }
}
```

Baked-in decisions:

1. **`variables`, not `templateParameters`** — no pipeline-YAML pre-declaration
   required, works against any pipeline. `BVTS_TOKEN` carries `isSecret: true`
   so Azure masks it in logs; emitted only when `TokenMode == TokenInject`.
2. **Ref pinning** via `resources.repositories.self.refName` using the full
   refname (`refs/heads/…` / `refs/tags/…`), consistent with CodeBuild's
   `BV_REF`.
3. **`BV_*` variable naming** matches the CodeBuild env-var convention exactly,
   so operators see identical names across AWS and Azure.
4. **Success = HTTP 2xx** (200 with a `Run` object). Any non-2xx is recorded
   with its status code and flows through normal retry/backoff/dead-letter. No
   error special-casing — missing connector and 4xx alike retry then
   dead-letter.

**Implementation:** hand-rolled HTTP over the egress-policy-bound client
(consistent with the existing outbound style; small dependency surface). The
`azurePipelinesAPI` interface lets tests inject a fake, exactly like the
CodeBuild client factory. `newAzurePipelinesClientFactory(map[string]AzureConnector)`
mirrors `newCodeBuildClientFactory`.

## 7. CLI surface (`cmd/bucketvcs/build.go`)

New flags on `build trigger add` (existing `--ref-include/-exclude`,
`--token-mode/-scopes/-ttl` apply to both new kinds unchanged):

```bash
# Cloud Build twin — no stored credential
bucketvcs build trigger add --auth-db=<path> \
    --tenant=<t> --repo=<r> --name=<n> --kind=azurewebhook \
    --azure-webhook-url=<https://dev.azure.com/Org/_apis/public/distributedtask/webhooks/Hook?api-version=6.0-preview> \
    [--secret=<s>] [--azure-sig-header=X-Hub-Signature] \
    [--ref-include=<csv>] [--ref-exclude=<csv>]

# CodeBuild twin — named connector
bucketvcs build trigger add --auth-db=<path> \
    --tenant=<t> --repo=<r> --name=<n> --kind=azurepipelines \
    --azure-connector=<name> --azure-project=<proj> --azure-pipeline-id=<int> \
    [--token-mode=inject --token-scopes=<csv> --token-ttl=<dur>] \
    [--ref-include=<csv>] [--ref-exclude=<csv>]
```

- `trigger list/remove/enable/disable`, `delivery list/show/replay`, `test` —
  unchanged (provider-agnostic). `list` shows the new kinds; the existing secret
  preview handles the optional `Secret`.
- **`build apply -f`**: add the Azure fields to the apply schema and `toInput()`
  for declarative reconciliation.
- **No new server flag**: Azure connectors parsed from the existing
  `--build-config` YAML.

## 8. Storage

No migration. The `build_triggers` / `build_trigger_deliveries` schema
(migration 0017) is unchanged — only new `kind` string values and new
`config_json` fields are stored. The PAT lives in server config, not the DB.

## 9. Testing

- **Wireshape golden tests** (core correctness proof, `-update` supported):
  - `TestWireShape_AzureWebhook`: capture POST; assert `X-Hub-Signature: sha1=<hex>`
    over the raw body; assert body matches `testdata/azurewebhook_body.golden.json`;
    assert custom-header override; assert unsigned case sends no signature header.
  - `TestWireShape_AzurePipelines`: fake endpoint; assert URL path
    `/{project}/_apis/pipelines/{id}/runs?api-version=7.1`; assert
    `Authorization: Basic` (empty user + PAT); assert body =
    `resources.repositories.self.refName` + `BV_*` variables; assert
    `BVTS_TOKEN` present with `isSecret:true` only under `TokenInject`.
- **Regression guard:** existing `generic_body.golden.json` /
  `cloudbuild_body.golden.json` and their signature headers remain
  byte-identical after the sig-profile refactor.
- **Unit tests:** `store.Create()` validation (missing url / connector /
  project / pipeline-id ≤ 0); connector resolution (missing name → permanent
  error); one ref-match smoke per kind (machinery is shared/free).

## 10. Observability

No new metrics or audit events. The M30 set
(`build_trigger_fired_total`, delivery duration, dead-letter, token-minted; the
7 audit events) is keyed by `kind`, so Azure flows through labeled
`azurewebhook` / `azurepipelines` automatically.

## 11. Docs

- Extend `docs/operator-guides/build-triggers.md` with an Azure section: both
  modes, the SHA-1 / `X-Hub-Signature` note, the "run the pipeline once to arm
  the incoming webhook" gotcha, the connector YAML, and PAT scope guidance
  (minimum: Build → Read & execute).
- New `examples/azure-pipelines/README.md`: worked setup for (a) the
  incoming-webhook service connection + YAML `resources.webhooks` block, and
  (b) the Run-Pipeline PAT connector path.

## 12. Extension point summary (recap of where Azure plugs in)

1. `types.go`: two `Kind` constants + Azure `Config` fields + `AzureConnector`.
2. `azurepipelines.go` (new): `azurePipelinesDeliverer`, `azurePipelinesAPI`,
   `newAzurePipelinesClientFactory`.
3. `deliver.go`/`render.go`: signature-profile generalization of `httpDeliverer`.
4. `store.go`: `Create()` validation cases.
5. `config.go`: parse `azure_connectors:` from `--build-config`.
6. `worker.go`: `ProductionDeliverers()` signature + two registrations.
7. `cmd/bucketvcs/build.go`: `--azure-*` flags + `apply` schema/`toInput()`.
8. Tests + docs + examples per §9 / §11.

The interface-based M30 design means the worker loop, claim logic, queue, and
delivery infrastructure need **zero** changes.
