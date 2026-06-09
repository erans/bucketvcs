# Build Triggers (operator guide)

This guide covers the M30 build triggers feature. It explains the three trigger kinds, how the gateway enqueues and delivers builds on push, how minted tokens work, how to manage triggers with the `bucketvcs build` CLI, and how to read the metrics + audit events.

The companion design document is `docs/superpowers/specs/2026-06-07-m30-build-triggers-design.md`.

Production readiness summary:

- Generic (signed JSON POST to any HTTPS endpoint) — **shipped**.
- Google Cloud Build (signed JSON POST, OIDC-pull recommended) — **shipped**.
- AWS CodeBuild (SigV4 `StartBuild` API) — **shipped**.
- Azure DevOps incoming webhook (`azurewebhook`, HMAC-SHA1 POST) — **shipped**.
- Azure DevOps Run Pipeline REST (`azurepipelines`, PAT via named connector) — **shipped**.
- Short-lived minted `bvts_` tokens for OIDC-pull and inject modes — **shipped**.
- At-least-once delivery with bounded retries + dead-letter (same schedule as webhooks) — **shipped**.
- Single-writer worker per `bucketvcs serve` process (in-process sqlite queue) — **shipped**.
- Ref include/exclude glob filtering (`**`-aware, M16 matcher) — **shipped**.
- Declarative `build apply -f` with `--prune` — **shipped**.
- Manual operator test / replay via CLI — **shipped**.
- Egress SSRF protection via shared webhook egress policy — **shipped** (see §2).
- Fail-open enqueue (a CI hiccup never blocks a push) — **shipped**.
- Native GCP `RunBuildTrigger` connector, GitHub/GitLab/Tekton presets, per-commit path filters, build-status callback, full server config file, Cloud Build issuer auto-registration helper — **deferred** (see §12).
- Schema 16 → 17 (`0017_build_triggers.sql`) is forward-only and applied by the existing `RunMigrations`.

---

## 1. Overview and concepts

### 1.1 Five trigger kinds

| Kind | How it fires | Credential model |
|---|---|---|
| `generic` | Signed JSON POST to any `http://` or `https://` URL | Shared HMAC secret (same scheme as webhooks); optional `bvts_` token injection |
| `cloudbuild` | Signed JSON POST to a Google Cloud Build HTTP trigger URL | Same as generic; OIDC-pull (recommended, no credential in trigger) or inject |
| `codebuild` | SigV4 `StartBuild` call to AWS CodeBuild | Ambient credential chain or named connector; optional `bvts_` token injection |
| `azurewebhook` | HMAC-SHA1 signed JSON POST to an Azure Pipelines incoming-webhook URL | Shared secret matching the Azure service-connection secret (no Azure credential stored); optional `bvts_` token injection |
| `azurepipelines` | `Run Pipeline` REST API call to Azure DevOps | PAT via named connector in `--build-config`; optional `bvts_` token injection |

Every trigger fires once per matching ref per push. One push that updates two matching refs produces two separate delivery rows.

### 1.2 Token modes

| Mode | Effect |
|---|---|
| `none` (default) | No token minted; build system authenticates via its own mechanism (OIDC-pull for Cloud Build, IAM for CodeBuild) |
| `inject` | A short-lived `bvts_` token is minted at delivery time and included in the POST body (`bvts_token` field) or as CodeBuild env var `BVTS_TOKEN` |

Minted tokens are:

- **Short-lived**: hard ceiling 1 hour; operator-configured per trigger via `--token-ttl` (default: `--build-sweep-interval`, effectively 5 min unless overridden in `--build-config`).
- **Single-repo**: scope_perm is bound to `(tenant, repo)` — the injected token cannot access any other repo.
- **Read-only by default**: default scopes `repo:read,lfs:read` — the build can also pull LFS objects from the same repo. Override with `--token-scopes`.
- **Revocable**: expired tokens are swept on the `--build-sweep-interval` tick (default 5 min).
- **Owned by `_build`**: a reserved system user that cannot be disabled, deleted, or granted repo permissions manually.

Wire format: `bvts_<24-char-Crockford-id>_<52-char-Crockford-secret>` — the same format as all BucketVCS tokens.

### 1.3 Ref include/exclude semantics

- Both `--ref-include` and `--ref-exclude` accept comma-separated glob patterns using the `**`-aware M16 matcher (`policy.MatchPath`).
- **Exclude wins**: if any exclude pattern matches, the ref does not fire regardless of include.
- **Empty include = match all**: omitting `--ref-include` fires on every ref update that is not excluded.
- Patterns match against the full refname (e.g. `refs/heads/main`, `refs/tags/v1.0`).

Examples:
- Fire only on `refs/heads/main`: `--ref-include refs/heads/main`
- Fire on all branches, not tags: `--ref-include 'refs/heads/**'`
- Fire on everything except `refs/heads/dev`: `--ref-exclude refs/heads/dev`
- Fire on main and all release branches: `--ref-include 'refs/heads/main,refs/heads/release/**'`

---

## 2. Enabling build triggers

Add three flags to `bucketvcs serve`:

```bash
bucketvcs serve \
    --addr=127.0.0.1:8080 \
    --store=localfs:/var/lib/bucketvcs \
    --auth-db=/var/lib/bucketvcs/auth.db \
    --build-triggers \
    --build-config=/etc/bucketvcs/build.yaml \   # optional; omit if no AWS connectors
    --build-sweep-interval=5m \                   # optional; default 5m
    ...
```

| Flag | Default | Effect |
|---|---|---|
| `--build-triggers` | off | Enable the build trigger subsystem (worker + sweep) |
| `--build-config=<path>` | — | Path to YAML config for AWS connectors and global defaults (see §4.2) |
| `--build-sweep-interval=<dur>` | `5m` | How often to sweep expired `_build` tokens |

### 2.1 Egress policy

`generic` and `cloudbuild` triggers POST over HTTP. They share the webhook egress policy: loopback, link-local (including `169.254.169.254`), and private/ULA ranges are **denied by default**. To deliver to a receiver in a private range:

```bash
bucketvcs serve ... --build-triggers --webhook-allow-cidr=192.168.1.0/24
```

`--webhook-allow-cidr=0.0.0.0/0` disables private-range blocking entirely (not recommended for production). `--webhook-deny-host=*.internal.example.com` adds a hostname-based deny. Both flags are repeatable.

See the [Webhooks operator guide §6](webhooks.md) for full egress policy documentation, including the DNS-rebinding-safe design.

`codebuild` triggers call the AWS SigV4 API directly; the egress policy does not apply (the AWS SDK manages connectivity).

---

## 3. Cloud Build (OIDC-pull, recommended)

The OIDC-pull model lets Cloud Build workers fetch a short-lived BucketVCS token by presenting their Google-signed OIDC identity token to the `POST /_oidc/token` endpoint. No BucketVCS credential is stored in the trigger — the trust relationship lives entirely in the OIDC issuer + rule configuration.

### 3.1 Prerequisites

- `--oidc=true` must be set on `bucketvcs serve` (OIDC token-exchange, M22).
- `--build-triggers` must be set.
- The Cloud Build build has a service account with an email you will match against.

### 3.2 Register the Google issuer and trust rule

```bash
# Register the Google issuer (do this once per authdb).
bucketvcs oidc issuer add \
    --auth-db=/var/lib/bucketvcs/auth.db \
    --alias=google \
    --url=https://accounts.google.com

# Create a trust rule scoped to this repo.
# --claim email=<sa>@<project>.iam.gserviceaccount.com matches the build SA.
bucketvcs oidc rule add \
    --auth-db=/var/lib/bucketvcs/auth.db \
    --issuer=google \
    --audience=https://gw.example.com \
    --tenant=acme \
    --repo=app \
    --scopes=repo:read \
    --ttl=15m \
    --claim email=cloudbuild-sa@my-gcp-project.iam.gserviceaccount.com
```

Flags:

| Flag | Required | Notes |
|---|---|---|
| `--alias` | yes | Short name for the issuer, referenced by `--issuer` in rules |
| `--url` | yes | Issuer URL; Google's is `https://accounts.google.com` |
| `--issuer` | yes | Alias set above |
| `--audience` | yes | Must match the `--audiences` argument in the build-side `gcloud` call |
| `--tenant`, `--repo` | yes | Scope of the minted token |
| `--scopes` | yes | Comma-separated scopes (`repo:read`, `lfs:read`, etc.) |
| `--ttl` | no | Default `15m`; max `1h`; use a Go duration string |
| `--claim` | no | Repeatable; exact-match constraint on a JWT claim. Omit for issuer-wide wildcard (matches ANY token from this issuer/audience — use only for single-tenant trusted setups) |

### 3.3 Create a cloudbuild trigger with token_mode=none

```bash
bucketvcs build trigger add \
    --auth-db=/var/lib/bucketvcs/auth.db \
    --tenant=acme \
    --repo=app \
    --name=main-build \
    --kind=cloudbuild \
    --url=https://cloudbuild.googleapis.com/v1/projects/my-gcp-project/triggers/TRIGGER_ID:run \
    --ref-include=refs/heads/main
```

Note: `--token-mode` defaults to `none` for `cloudbuild`. No secret needs to be embedded in the trigger when using OIDC-pull.

### 3.4 Build-side snippet (Cloud Build `cloudbuild.yaml`)

Cloud Build substitutions map POST body fields via `$(body.<field>)`. When the trigger fires, the POST body contains `ref` and `commit` at the top level (in addition to the full `ref_update` object):

```yaml
steps:
  - name: 'gcr.io/google.com/cloudsdktool/cloud-sdk'
    entrypoint: bash
    args:
      - -c
      - |
        # 1. Get an identity token for this build's service account.
        ID_TOKEN=$(gcloud auth print-identity-token --audiences=https://gw.example.com)
        # 2. Exchange it for a BucketVCS token.
        BVTS_TOKEN=$(curl -sf -X POST https://gw.example.com/_oidc/token \
            -H "Authorization: Bearer $ID_TOKEN" | jq -r .access_token)
        # 3. Clone using the minted token.
        git clone "https://x-access-token:$BVTS_TOKEN@gw.example.com/acme/app.git" /workspace/app
        git -C /workspace/app checkout $(ref)

substitutions:
  _REF: $(body.ref)
  _COMMIT: $(body.commit)
```

`$(body.ref)` maps to `ref_update.refname` (e.g. `refs/heads/main`). `$(body.commit)` maps to `ref_update.new_oid`.

The `BucketVCS-Signature` header is present on all `cloudbuild` POST requests. For build-history security (proving BucketVCS triggered the run, not an external replay), verify it in a pre-step using the same algorithm as webhooks — see [Webhooks §4](webhooks.md) for Python and Go snippets (substitute the trigger's `--secret` value).

---

## 4. CodeBuild (mint-and-inject)

### 4.1 Create a codebuild trigger

```bash
bucketvcs build trigger add \
    --auth-db=/var/lib/bucketvcs/auth.db \
    --tenant=acme \
    --repo=app \
    --name=main-codebuild \
    --kind=codebuild \
    --aws-region=us-east-1 \
    --aws-project=my-codebuild-project \
    --aws-connector=prod \        # optional; omit to use ambient credentials
    --token-mode=inject \
    --token-scopes=repo:read \
    --token-ttl=15m \
    --ref-include=refs/heads/main
```

Flags specific to `codebuild`:

| Flag | Required | Notes |
|---|---|---|
| `--aws-region` | yes | AWS region (e.g. `us-east-1`) |
| `--aws-project` | yes | CodeBuild project name |
| `--aws-connector` | no | Named connector from `--build-config`; omit to use ambient credential chain |

Note: for `codebuild` triggers, `--token-mode` defaults to `inject` when omitted (because AWS CodeBuild cannot perform OIDC-pull against the gateway). For `generic` and `cloudbuild` triggers the default is `none`.

### 4.2 AWS connector configuration (`--build-config`)

The optional `--build-config` YAML lets operators define named AWS connectors shared across triggers, and set server-wide token defaults.

```yaml
# /etc/bucketvcs/build.yaml
build:
  defaults:
    token_ttl: "15m"              # Go duration; default when trigger omits --token-ttl
    token_scopes:
      - "repo:read"
    audience: ""                  # optional audience claim for minted tokens
  aws_connectors:
    prod:
      region: "us-east-1"
      profile: "bucketvcs-prod"   # AWS shared config profile; preferred over static keys
    staging:
      region: "eu-west-1"
      access_key: "AKIA..."       # static keys — foot-gun; prefer ambient chain or profile
      secret_key: "..."
```

**Credential precedence** (highest first):
1. Static `access_key` / `secret_key` in the connector.
2. `profile` — AWS shared config/credentials profile.
3. **Ambient credential chain** (env vars, instance/IMDS role, ECS task role) — strongly preferred. Never commit static keys.

Connector values override the trigger's `--aws-region` when the connector specifies a region.

### 4.3 Injected environment variables

When `--token-mode=inject`, the delivery worker calls `StartBuild` with these environment variable overrides:

| Variable | Value |
|---|---|
| `BV_REF` | Full refname that triggered the build (e.g. `refs/heads/main`) |
| `BV_REPO` | `<tenant>/<repo>` (e.g. `acme/app`) |
| `BV_COMMIT` | Head OID (the resolved commit SHA) |
| `BVTS_TOKEN` | Minted `bvts_` token wire string (only when `token_mode=inject`) |

### 4.4 BuildSpec snippet

```yaml
# buildspec.yml
version: 0.2
phases:
  install:
    commands:
      - yum install -y git  # or apt-get install git
  build:
    commands:
      # Clone using the minted single-repo read token.
      - git clone "https://x-access-token:${BVTS_TOKEN}@gw.example.com/${BV_REPO}.git" workspace
      - git -C workspace checkout ${BV_COMMIT}
      # ... your build steps ...
```

---

## 5. Azure DevOps

BucketVCS supports two Azure modes, mirroring the two integration styles used for Cloud Build and CodeBuild:

- **`azurewebhook`** — the Cloud Build twin. BucketVCS POSTs a JSON body to an Azure Pipelines *incoming-webhook* URL, signed with HMAC-SHA1 in the `X-Hub-Signature` header. BucketVCS holds **no** Azure credential — only a shared secret that must match the one configured on the Azure service connection.
- **`azurepipelines`** — the CodeBuild twin. BucketVCS calls the `Run Pipeline` REST API directly, authenticating with a Personal Access Token resolved from a **named connector** in `--build-config` (the PAT is never stored in the authdb).

### 5.1 azurewebhook setup

1. In Azure DevOps: **Project Settings → Service connections → New → Incoming WebHook.** Set a **WebHook Name**, a **Secret**, and the **HTTP header name** (default `X-Hub-Signature`).
2. Reference it in your pipeline YAML and run the pipeline once so the trigger arms:

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

   The pushed payload is available inside the pipeline as `${{ parameters.MyHook.<jsonPath> }}` (e.g. `${{ parameters.MyHook.head_oid }}`).

   > **Note:** the signature is HMAC-**SHA1** (`sha1=<hex>`), not SHA-256, and covers the raw body only. Omitting `--secret` sends the webhook **unsigned** (Azure permits this; BucketVCS does not auto-generate an Azure secret).

### 5.2 azurepipelines setup

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

   BucketVCS POSTs to `{org_url}/MyProject/_apis/pipelines/42/runs?api-version=7.1`, pins the run to the pushed ref via `resources.repositories.self.refName`, and passes push metadata as `BV_*` run variables (`BV_REPO`, `BV_REF`, `BV_COMMIT`, `BV_ACTOR`, `BV_TX_ID`). With `--token-mode=inject` (the default for this kind), a short-lived `BVTS_TOKEN` variable is added with `isSecret: true`.

### 5.3 Azure error handling

A missing/misconfigured connector or a non-transient 4xx (e.g. 401/404) is a permanent failure: it dead-letters **immediately** with `reason=permanent` rather than retrying. Transient failures (5xx, 408, 429, network errors) retry on the standard backoff schedule (1m, 30m, 2h, 12h) and then dead-letter with `reason=exhausted`. Observe via the `build_trigger_deadletter_total` metric and the `build.trigger.deadletter` audit event (both carry the `reason` label/attr), and recover with `bucketvcs build delivery replay` after fixing the configuration. See §9.2 for the full permanent-vs-transient breakdown.

---

## 6. Generic trigger

The `generic` kind signs a JSON POST to any HTTP/HTTPS endpoint. Use it for API Gateway/Lambda, Jenkins, Tekton, or any custom receiver.

### 6.1 Create a generic trigger

```bash
RESULT=$(bucketvcs build trigger add \
    --auth-db=/var/lib/bucketvcs/auth.db \
    --tenant=acme \
    --repo=app \
    --name=lambda-ci \
    --kind=generic \
    --url=https://api.example.com/build-hook \
    --ref-include=refs/heads/main \
    --token-mode=inject \
    --token-scopes=repo:read \
    --token-ttl=15m)
echo "$RESULT"
# trigger_id=bvbt_...  tenant=acme  repo=app  name=lambda-ci  kind=generic
# secret=NQpV4o7...    # store this now — it will not be shown again
```

The `--secret` flag lets you supply your own secret. When omitted, the gateway generates a random 32-byte secret (shown once at creation; not retrievable later).

If `--token-mode` is omitted the default is `none`. Specifying `inject` causes the POST body to include a `bvts_token` field.

### 6.2 POST body shape

Every `generic` and `cloudbuild` POST carries this JSON body:

```json
{
  "tenant":     "acme",
  "repo":       "app",
  "actor":      "alice",
  "tx_id":      "...",
  "head_oid":   "a1b2c3...",
  "ref_update": {
    "refname": "refs/heads/main",
    "old_oid": "0000000000000000000000000000000000000000",
    "new_oid": "a1b2c3..."
  },
  "bvts_token": "bvts_..."    // only when token_mode=inject
}
```

`cloudbuild` additionally flattens `ref` and `commit` to the top level for `$(body.ref)` / `$(body.commit)` substitution ergonomics:

```json
{
  ...,
  "ref":    "refs/heads/main",
  "commit": "a1b2c3...",
  ...
}
```

`old_oid == "0000...0"` means a ref creation. `new_oid == "0000...0"` means a ref deletion.

### 6.3 Signature verification

Every POST carries:

```
Content-Type: application/json
BucketVCS-Signature: t=1749000000,v1=4b3f...
User-Agent: bucketvcs-buildtrigger/1
```

The scheme is identical to the webhook signature:

```
v1 = hex(HMAC_SHA256(secret, "<t>." + body_bytes))
```

The `t=<unix>` value must be within ±300 s of the receiver's wall clock. The worker re-signs with a fresh `t` on every retry.

For receiver-side Python and Go verification snippets, see [Webhooks §4](webhooks.md). Substitute your trigger's `secret` for the webhook endpoint secret — the algorithm is identical.

---

## 7. Declarative `build apply -f`

For GitOps-style trigger management, describe all triggers in a single YAML file and apply it idempotently.

### 7.1 Document shape

```yaml
# triggers.yml
triggers:
  - tenant: acme
    repo: app
    name: main-build
    kind: cloudbuild
    url: https://cloudbuild.googleapis.com/v1/projects/my-gcp-project/triggers/TRIGGER_ID:run
    ref_include:
      - refs/heads/main
    token_mode: none

  - tenant: acme
    repo: app
    name: lambda-ci
    kind: generic
    url: https://api.example.com/build-hook
    secret: ""          # generate a new secret on each apply if empty
    ref_include:
      - refs/heads/main
      - refs/heads/release/**
    ref_exclude:
      - refs/heads/release/rc-*
    token_mode: inject
    token_scopes:
      - repo:read
    token_ttl: "15m"

  - tenant: acme
    repo: app
    name: codebuild-prod
    kind: codebuild
    aws_region: us-east-1
    aws_project: my-codebuild-project
    aws_connector: prod
    ref_include:
      - refs/heads/main
    token_mode: inject
    token_scopes:
      - repo:read
    token_ttl: "15m"
```

Field reference:

| Field | Kind | Notes |
|---|---|---|
| `tenant`, `repo`, `name` | all | Primary key; `name` must be unique per `(tenant, repo)` |
| `kind` | all | `generic`, `cloudbuild`, `codebuild`, `azurewebhook`, or `azurepipelines` |
| `url` | generic, cloudbuild | Receiver URL |
| `secret` | generic, cloudbuild | Shared HMAC secret; generated if empty |
| `aws_region` | codebuild | AWS region |
| `aws_project` | codebuild | CodeBuild project name |
| `aws_connector` | codebuild | Named connector from `--build-config`; optional |
| `ref_include` | all | List of glob patterns; empty = match all |
| `ref_exclude` | all | List of glob patterns; exclude wins |
| `token_mode` | all | `none` or `inject` |
| `token_scopes` | all | List of scope names (`repo:read`, etc.) |
| `token_ttl` | all | Go duration string; max `1h` |

### 7.2 Apply

```bash
bucketvcs build apply \
    --auth-db=/var/lib/bucketvcs/auth.db \
    -f triggers.yml

# created=2 updated=0 pruned=0
```

With `--prune`, any trigger in a covered `(tenant, repo)` that is NOT present in the document is removed:

```bash
bucketvcs build apply \
    --auth-db=/var/lib/bucketvcs/auth.db \
    -f triggers.yml \
    --prune

# created=0 updated=1 pruned=1
```

**Important**: `apply` implements update as `remove + create`. This resets `created_at` on every update run. Triggers are operator configuration, not append-only records; this is acceptable by design.

---

## 8. CLI reference

All `build` subcommands require `--auth-db=<path>` pointing to the authdb (`bucketvcs.db`).

### 8.1 Trigger management

```
bucketvcs build trigger add \
    --auth-db=<path> \
    --tenant=<t> --repo=<r> --name=<n> --kind=<generic|cloudbuild|codebuild|azurewebhook|azurepipelines> \
    # generic/cloudbuild/azurewebhook:
    [--url=<https://...>] [--secret=<s>] \
    # azurewebhook:
    [--azure-webhook-url=<https://...>] [--azure-sig-header=<header>] \
    # codebuild:
    [--aws-region=<r>] [--aws-project=<p>] [--aws-connector=<c>] \
    # azurepipelines:
    [--azure-connector=<c>] [--azure-project=<p>] [--azure-pipeline-id=<n>] \
    # common:
    [--ref-include=<csv>] [--ref-exclude=<csv>] \
    [--token-mode=<none|inject>] [--token-scopes=<csv|all|repo:*|lfs:*>] \
    [--token-ttl=<dur>]

bucketvcs build trigger list   --auth-db=<path> --tenant=<t> --repo=<r> [--format=text|json]
bucketvcs build trigger remove --auth-db=<path> --id=<trigger-id>
bucketvcs build trigger enable --auth-db=<path> --id=<trigger-id>
bucketvcs build trigger disable --auth-db=<path> --id=<trigger-id>
```

`trigger add` prints the trigger ID and, for `generic`/`cloudbuild`/`azurewebhook`, the secret (shown **once only** — there is no way to retrieve it later; remove and re-add to rotate).

`trigger disable` keeps the row but stops new enqueues. Pending deliveries for the disabled trigger are also skipped by the worker (the claim query filters `active=1`). `trigger enable` reverses it.

### 8.2 Test a trigger

```bash
bucketvcs build test --auth-db=<path> --id=<trigger-id>
# delivery_id=bvbd_...  trigger_id=...  ref=refs/heads/main  status=pending
```

`build test` synthesizes a push against the trigger's first non-glob `ref_include` entry (or `refs/heads/main` when none is set) and enqueues a delivery. The resulting `delivery_id` can be tracked with `build delivery show`.

### 8.3 Delivery operations

```bash
# List deliveries (filter by trigger, status, limit):
bucketvcs build delivery list \
    --auth-db=<path> \
    [--trigger=<trigger-id>] \
    [--status=<pending|in_flight|delivered|dead_letter>] \
    [--limit=<n>] \              # default 500; 0 = no limit
    [--format=text|json]

# Show one delivery (pretty JSON):
bucketvcs build delivery show --auth-db=<path> --id=<delivery-id>

# Replay a dead-lettered delivery:
bucketvcs build delivery replay --auth-db=<path> --id=<delivery-id>
# id=bvbd_...  replay-scheduled
```

`replay` resets `status=pending`, `attempts=0`, `next_attempt_at=NOW`. The worker picks it up on the next tick (≤1 s). The delivery ID is preserved (same `delivery_id`).

`replay` is refused with exit 1 if the row is `in_flight` — wait for the attempt to finish first.

---

## 9. Operations and observability

### 9.1 Retry semantics

Build triggers use the same schedule as webhooks:

| Attempt | Delay before next | Cumulative |
|---|---|---|
| 1 | ~1 min | ~0 |
| 2 | ~30 min | ~1 min |
| 3 | ~2 hours | ~31 min |
| 4 | ~12 hours | ~2.5 hours |
| 5 (final) | dead_letter | ~14.5 hours |

Backoff carries ±25% uniform jitter. After 5 failures the row moves to `dead_letter`. Operators replay via `build delivery replay` (see §8.3).

### 9.2 Permanent vs. transient failures

For HTTP-delivered triggers (`generic`, `cloudbuild`, `azurewebhook`,
`azurepipelines`), bucketvcs distinguishes failures that cannot succeed on retry
from transient ones:

- **Permanent** (dead-letters **immediately**, `reason=permanent`): any 4xx
  response except `408` and `429`, a non-`http(s)` URL scheme, or an unknown
  `azure_connector`. Fix the configuration, then `bucketvcs build delivery
  replay --id=<id>`.
- **Transient** (retries `1m → 30m → 2h → 12h`, then dead-letters with
  `reason=exhausted`): `5xx`, `408`, `429`, network errors, and token-mint blips.

`codebuild` `StartBuild` errors are classified the same way: `ResourceNotFoundException`
(project missing), `InvalidInputException`, `AccessDeniedException`, and other
non-throttling 4xx responses are permanent; throttling (`ThrottlingException`,
`RequestLimitExceeded`) and 5xx remain transient. The AWS config/credential-load
step stays retry-only (it can fail transiently, and real credential errors
surface at `StartBuild` as a 403).
The breakdown is exposed as the `reason` label on
`build_trigger_deadletter_total` and as the `reason` attribute on the
`build.trigger.deadletter` audit event.

### 9.3 Metrics

Four metrics emitted as structured slog records with `msg="metric"` and `metric_name=<name>`:

| Metric | Labels | Emission point |
|---|---|---|
| `build_trigger_fired_total` | `kind={generic,cloudbuild,codebuild,azurewebhook,azurepipelines}`, `result={delivered,failed_retry,dead_letter}` | once per attempt outcome |
| `build_trigger_delivery_duration_ms` | `result=...` | once per attempt, measures wall time |
| `build_trigger_deadletter_total` | `reason={permanent,exhausted}` | once per retry-budget exhaustion or immediate permanent dead-letter |
| `build_token_minted_total` | none | once per token mint |

### 9.4 Audit events

Seven structured events:

| Event | Level | Key attributes |
|---|---|---|
| `build.trigger.fired` | INFO | delivery_id, trigger_id, kind, ref_count |
| `build.trigger.delivered` | INFO | delivery_id, trigger_id, attempts, duration_ms |
| `build.trigger.failed` | WARN | delivery_id, trigger_id, attempts, status_code, error, next_attempt_at |
| `build.trigger.deadletter` | ERROR | delivery_id, trigger_id, total_attempts, final_status_code, reason={permanent,exhausted} |
| `build.token.minted` | INFO | tenant, repo, token_label, ttl_seconds (token value is NEVER logged) |
| `build.trigger.enqueue_failed` | ERROR | tenant, repo, error |
| Lifecycle (`build.trigger.added/removed/enabled/disabled`) | INFO | trigger_id, tenant, repo |

### 9.5 Fail-open enqueue

If the `Enqueue` INSERT fails (sqlite write failure, schema error), the push **does not abort**. The gateway emits `build.trigger.enqueue_failed` and the push reports success to the client. Operators MUST treat repeated `enqueue_failed` events as P1 — builds will be silently missed.

### 9.6 Replica behavior

The build trigger worker (`StartWorker`) and the token sweep (`SweepExpiredBuildTokens`) run only in the write-region gateway process. Read-replica gateways (M26 `--replica-of`) do not run a worker and do not enqueue deliveries; pushes are refused by replicas, so no deliveries are created there.

### 9.7 Quick log filter

```bash
# Dead-lettered deliveries that need operator attention:
journalctl -u bucketvcs --since "24 hours ago" | grep "build.trigger.deadletter"

# Token mint rate (last hour):
journalctl -u bucketvcs --since "1 hour ago" \
    | grep 'metric_name=build_token_minted_total' | wc -l

# Delivery duration p99 proxy (last hour):
journalctl -u bucketvcs --since "1 hour ago" \
    | grep 'metric_name=build_trigger_delivery_duration_ms.*result=delivered' \
    | awk -F'value=' '{print $2}' | sort -n | tail -1
```

---

## 10. Security notes

### 10.1 Minted token blast radius

An injected `bvts_` token is single-repo and read-only by default. A leaked token can read the target repo until it expires (at most 1 hour; swept within `--build-sweep-interval` after expiry). It cannot access any other repo, write to the repo, or administer BucketVCS. Keep TTLs short (≤15 min for most workloads).

### 10.2 Egress policy on generic/cloudbuild

Build triggers and webhooks share the egress policy. Any endpoint URL that resolves to a loopback, link-local, or private address is denied by default. A misconfigured trigger pointing at an internal metadata endpoint (e.g. `169.254.169.254`) will dead-letter after 5 attempts — it will not leak credentials to the metadata service.

### 10.3 Never commit static AWS keys

The `aws_connectors[*].access_key` and `aws_connectors[*].secret_key` fields in `--build-config` should be left empty when the build host can use the ambient credential chain (IAM instance profile, ECS task role, EKS pod identity). Static keys in a YAML config file are a foot-gun — use the `profile` field and the AWS shared credentials file, or rely on the ambient chain.

### 10.4 Trigger management is admin-scoped

The `bucketvcs build` CLI operates on the authdb directly (out-of-band, not via the HTTPS API). Guard access to `--auth-db` like any privileged credential. A future release may expose trigger CRUD via a `storage:admin`-scoped API endpoint.

### 10.5 Secret storage and rotation

For `generic`, `cloudbuild`, and `azurewebhook` triggers, the HMAC secret is shown **once at creation**. To rotate: `bucketvcs build trigger remove --id=<id>` then `bucketvcs build trigger add ...` with new parameters. Update the receiver's secret in lockstep. Pending deliveries for the removed trigger will never be delivered.

---

## 11. Worked example: generic + inject (localfs)

This mirrors `scripts/smoke_buildtriggers.sh`.

```bash
# 1. Seed user and repos.
bucketvcs user add alice --auth-db auth.db
TOKEN=$(bucketvcs token create alice --auth-db auth.db | sed -n 's/^token=//p')
bucketvcs repo register acme/app   --auth-db auth.db --store localfs:/var/lib/bv
bucketvcs repo grant alice acme/app write --auth-db auth.db

# 2. Start the gateway with build triggers + loopback egress allowed.
bucketvcs serve \
    --addr=127.0.0.1:8080 \
    --store=localfs:/var/lib/bv \
    --auth-db=auth.db \
    --build-triggers \
    --webhook-allow-cidr=127.0.0.1/32 &

# 3. Register a generic trigger for refs/heads/main, with token injection.
bucketvcs build trigger add \
    --auth-db=auth.db \
    --tenant=acme --repo=app \
    --name=main \
    --kind=generic \
    --url=http://127.0.0.1:9999/ \
    --ref-include=refs/heads/main \
    --token-mode=inject \
    --token-scopes=repo:read

# 4. Push to refs/heads/main — the worker fires a delivery within ~1 s.
git -C /tmp/clone push origin main

# 5. Check delivery status.
bucketvcs build delivery list --auth-db=auth.db --status=delivered

# 6. Smoke a trigger manually (without pushing).
bucketvcs build test --auth-db=auth.db --id=<trigger-id>
```

The POST body received by the endpoint contains `"bvts_token":"bvts_..."`. Extract it and use it as HTTP Basic password (username ignored, or use `x-access-token`):

```bash
git clone "https://x-access-token:${BVTS_TOKEN}@127.0.0.1:8080/acme/app.git"
```

---

## 12. Deferred / not yet supported

The following items from the design spec §1.2 are explicitly out of scope for M30:

- **Native GCP `RunBuildTrigger` connector.** The `cloudbuild` kind POSTs to a URL; it does not call the Cloud Build API directly. Operators must configure a Cloud Build HTTP trigger or use OIDC-pull.
- **GitHub/GitLab/Tekton presets.** No preconfigured body shapes or custom headers for third-party CI systems; use `generic` with a matching receiver.
- **Per-commit path filters.** Ref-level filtering only; there is no equivalent of the M16 path protection `--path-pattern` for trigger firing.
- **Build-status callback.** There is no API for a build to report its outcome back to BucketVCS (no green/red commit status, no commit checks).
- **Full server config file.** `--build-config` covers only `build.defaults` and `build.aws_connectors`; the broader gateway configuration has no single YAML file today.
- **Cloud Build issuer auto-registration helper.** Operators must run `bucketvcs oidc issuer add` and `oidc rule add` manually (see §3.2).
