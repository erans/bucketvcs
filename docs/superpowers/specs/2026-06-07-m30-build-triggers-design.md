# M30: Build triggers (CI integration)

Date: 2026-06-07
Builds on: M15 webhooks (durable delivery), M22 OIDC token exchange (short-lived
repo-scoped tokens), M16 protected paths (`**`-aware glob matcher), M17 token
scopes, M25 egress policy (SSRF guard).

## 1. Goals

On push, fire a **durable, ref-filtered HTTP request** that starts a build in
GCP Cloud Build or AWS CodeBuild, and give the build a **short-lived,
single-repo token** to pull the code. The feature is configured per-repo (like
webhooks/policies) and reuses the existing delivery + token machinery rather
than inventing parallel mechanisms.

### 1.1 In scope

- **Three trigger kinds:**
  - `generic` — HMAC-signed JSON POST to any operator-supplied URL (an API
    Gateway/Lambda shim, a self-hosted CI, etc.).
  - `cloudbuild` — a *preset* of `generic`: identical POST delivery, but
    defaults a Cloud-Build-shaped body template and documents the
    `…/triggers/<ID>:webhook?key=<API_KEY>&secret=<SECRET>` URL convention.
    There is **no native GCP connector** — a generic webhook hits Cloud Build's
    native webhook trigger directly.
  - `codebuild` — native SigV4 `StartBuild` via
    `github.com/aws/aws-sdk-go-v2/service/codebuild` (AWS has no inbound
    webhook).
- **Ref filtering** — per-trigger include + exclude glob lists using the M16
  `**`-aware matcher, evaluated against each ref update in the push.
- **Short-lived pull tokens** — default OIDC-pull (build exchanges its
  cloud-issued OIDC token via the existing M22 `/_oidc/token`); optional
  mint-and-inject for CodeBuild and as an opt-in on the generic path.
- **Durable delivery** — retry/backoff/dead-letter/replay/reclaim, modeled on
  M15.
- **Config** — `bucketvcs build …` CLI (mirrors webhook/policy CLI), a
  declarative `bucketvcs build apply -f triggers.yml`, and a scoped operator
  YAML for `bucketvcs serve` (cloud connector config + defaults).

### 1.2 Out of scope (deferred, documented)

- **Native GCP `RunBuildTrigger` connector** — generic webhook covers Cloud
  Build; a credentialed GCP connector is a future extension.
- **Provider presets** beyond Cloud Build/CodeBuild (GitHub/GitLab/Bitbucket/
  Tekton/Jenkins).
- **Per-commit changed-path trigger filters** — ref-only filtering for v1
  (the M16 diff-tree machinery exists and can back this later).
- **Build-status callback** (bucketvcs surfacing build results) — one-way fire
  only.
- **Full server-wide config file** replacing all `serve` flags — only a
  *scoped* build-config YAML now, structured to grow into the whole-server
  config in a later milestone.
- **Cloud Build OIDC issuer auto-registration helper** — operator runs the
  existing M22 `oidc issuer/rule` commands; document the recipe.

## 2. Architecture overview

```
git push ──▶ receive-pack completion (internal/gateway/receive_pack.go:68,
             same point that enqueues M15 EventPush)
                │  load active build_triggers for (tenant, repo)
                │  ref-match each via M16 **-matcher (include ∧ ¬exclude)
                │  enqueue one build_trigger_deliveries row per matching trigger
                ▼  (fail-open: enqueue error → audit, push still succeeds)
         build-trigger worker (one goroutine per `serve`, modeled on M15)
                │  claim batch → backoff schedule → dead-letter on exhaustion
                ▼  Deliverer.Deliver(ctx, trigger, payload)
         ┌──────────────┬───────────────────┬────────────────────────┐
         │ generic       │ cloudbuild         │ codebuild               │
         │ render body   │ render body        │ aws-sdk-go-v2 codebuild │
         │ + HMAC sign   │ (CB preset) + sign │ StartBuild (SigV4)      │
         │ POST (egress) │ POST (egress)      │ env overrides           │
         └──────────────┴───────────────────┴────────────────────────┘
                │  if token_mode=inject: mint short-lived bvts first
                ▼
   build pulls code:  OIDC-pull (default)  →  POST /_oidc/token (M22, unchanged)
                      mint-and-inject       →  token already present in body/env
```

### 2.1 New package: `internal/buildtrigger`

- `store.go` — CRUD over `build_triggers` + delivery-queue ops
  (claim/mark/reclaim), structurally modeled on `internal/webhooks/service.go`.
- `match.go` — reuse/lift the M16 `**`-aware matcher; evaluate include/exclude
  over each `RefUpdate`. Fire if **any** updated ref matches an include and
  matches **no** exclude. Exclude wins. Empty include = all refs (excludes
  still apply). Surfaced visibly in `trigger list` so "match all" is never a
  silent surprise.
- `deliver.go` — `Deliverer` interface:
  `Deliver(ctx, trigger, payload) (statusCode int, err error)`.
  - `genericDeliverer` (also backs `cloudbuild`): render body template, optional
    HMAC signature (**reuse `internal/webhooks/sign.go`**), optional token
    injection into the rendered body, POST through the M25 egress client
    (`internal/webhooks/egress`) so the SSRF guard applies uniformly.
  - `codebuildDeliverer`: build a `codebuild.StartBuildInput` —
    `projectName`, `sourceVersion = head OID`, `environmentVariablesOverride =
    [BVTS_TOKEN?, BV_REF, BV_REPO, BV_COMMIT]`; call via an SDK client
    constructed from the operator AWS connector config.
- `mint.go` — mint a short-lived `bvts` via `auth` `CreateScopedToken` (the M22
  repo-binding sibling): `user_id='_build'`, `expires_at = now + ttl`,
  `scopes = trigger.token_scopes`, `scope_tenant/scope_repo` = the trigger's
  repo, `scope_perm='read'`, `label = build:<tenant>/<repo>:<trigger-name>`.
- `worker.go` — claim/backoff/dead-letter/reclaim, one goroutine per `serve`,
  modeled on `internal/webhooks/worker.go` (reuse its backoff schedule shape).
- `enqueue.go` — `Enqueue(ctx, push PushInfo)` called from the receive-pack
  completion path; loads active triggers, ref-matches, inserts deliveries.
  Fail-open (errors → `build.trigger.enqueue_failed` audit; push proceeds).
- `metrics.go` / `audit.go` — `build_trigger_*` metrics and `build.*` audit
  events.

`codebuildDeliverer` is the only place the AWS SDK is touched; the interface
keeps SigV4/cloud concerns out of the generic POST path and out of M15.

### 2.2 Store layer (`internal/auth/sqlitestore`) — new migration `0017_build_triggers.sql`

(Both `migrations/` and `migrations_postgres/` get a copy, per existing
convention.)

```sql
CREATE TABLE build_triggers (
    id                TEXT PRIMARY KEY,          -- bvbt_<id>
    tenant            TEXT NOT NULL,
    repo              TEXT NOT NULL,
    name              TEXT NOT NULL,             -- operator handle, unique per repo
    kind              TEXT NOT NULL,             -- 'generic' | 'cloudbuild' | 'codebuild'
    config_json       BLOB NOT NULL,             -- kind-specific (URL+secret, or AWS region/project/connector)
    ref_include       BLOB NOT NULL,             -- JSON array of globs ([] = all refs)
    ref_exclude       BLOB NOT NULL,             -- JSON array of globs
    token_mode        TEXT NOT NULL,             -- 'none' | 'inject'
    token_scopes      INTEGER NOT NULL,          -- M17 TokenScope bitmask
    token_ttl_seconds INTEGER NOT NULL,          -- validated 0 < ttl <= ceiling at insert
    active            INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1)),
    created_at        INTEGER NOT NULL,
    UNIQUE (tenant, repo, name),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);
CREATE INDEX build_triggers_by_repo ON build_triggers (tenant, repo, active);

CREATE TABLE build_trigger_deliveries (
    id                TEXT PRIMARY KEY,          -- bvbd_<id>
    trigger_id        TEXT NOT NULL,
    payload_json      BLOB NOT NULL,             -- the build payload snapshot
    status            TEXT NOT NULL,             -- 'pending' | 'in_flight' | 'delivered' | 'dead_letter'
    attempts          INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   INTEGER NOT NULL,
    last_attempt_at   INTEGER,
    last_status_code  INTEGER,
    last_error        TEXT,
    created_at        INTEGER NOT NULL
);
CREATE INDEX build_trigger_deliveries_claim ON build_trigger_deliveries (status, next_attempt_at);

-- Reserved system user so build-minted tokens satisfy tokens.user_id NOT NULL,
-- parallel to M22's '_oidc'.
INSERT INTO users (id, name, is_admin, created_at)
VALUES ('_build', '_build', 0, strftime('%s','now'));
```

`build_trigger_deliveries` deliberately does **not** FK to `build_triggers` so
that removing a trigger leaves in-flight deliveries to drain (same orphan-drain
reasoning M15 used for `webhook_deliveries`); the worker tolerates a missing
trigger by dead-lettering the delivery.

The `tokens.scope_tenant/scope_repo/scope_perm` columns already exist from M22
migration `0010` — build minting reuses them verbatim; **no token-schema change.**

### 2.3 Minted-token lifecycle

Build-minted tokens are ordinary `tokens` rows under reserved user `_build`,
exactly mirroring M22's `_oidc` tokens (short TTL, single-repo `auth.Scope`,
M17 bitmask). Every downstream auth path already understands them.

**Sweep:** generalize the M22 expired-token sweep from `user_id = '_oidc'` to a
reserved-minter set `user_id IN ('_oidc','_build')` so both are reaped on the
same periodic worker. (Operator-managed user tokens are still never touched.)
Emit `build_tokens_swept_total` alongside `oidc_tokens_swept_total`, or a single
`minted_tokens_swept_total{user}` — decide at implementation, leaning toward one
metric labeled by reserved user.

## 3. Token model (detail)

### 3.1 OIDC-pull (default, `token_mode=none`)

The trigger carries **no credential**. For Cloud Build:

1. Operator registers the Google issuer + a trust rule once (existing M22 CLI):
   ```
   bucketvcs oidc issuer add --alias=google --url=https://accounts.google.com
   bucketvcs oidc rule add --issuer=google --audience=<aud> \
       --tenant=myorg --repo=app --scopes=repo:read,lfs:read --ttl=15m \
       --claim=email=<cloudbuild-sa>@<project>.iam.gserviceaccount.com
   ```
2. In the build, the job mints a Google-signed OIDC token for that audience
   (`gcloud auth print-identity-token --audiences=<aud>`) and exchanges it at
   `POST /_oidc/token` for a `bvts`, then clones with it.

Nothing new is built for this path; the operator guide documents the recipe.

### 3.2 Mint-and-inject (`token_mode=inject`)

`buildtrigger.mint` mints a short-lived `bvts` and the deliverer injects it:

- `generic`/`cloudbuild` — into the rendered JSON body (e.g. a `bvts_token`
  field the Cloud Build trigger maps via `$(body.bvts_token)`).
- `codebuild` — as a `BVTS_TOKEN` entry in `environmentVariablesOverride`.

Default for `codebuild` (AWS cannot practically do OIDC-pull); **opt-in** for
`generic`/`cloudbuild` (default `none`). Injected token defaults to scopes
`repo:read,lfs:read`, TTL 15m (ceiling 1h, validated at trigger creation).

## 4. Config

### 4.1 CLI (`bucketvcs build …`, NDJSON output, mirrors webhook/policy shape)

```
build trigger add    --tenant --repo --name --kind=generic|cloudbuild|codebuild \
                     [--url=… --secret=…]                # generic/cloudbuild
                     [--aws-region=… --aws-project=… --aws-connector=…]  # codebuild
                     [--ref-include=glob,…] [--ref-exclude=glob,…] \
                     [--token-mode=none|inject] [--token-scopes=repo:read,lfs:read] \
                     [--token-ttl=15m]
build trigger list   [--tenant --repo]
build trigger remove --id=bvbt_…
build trigger enable --id=bvbt_…
build trigger disable --id=bvbt_…
build delivery list  [--trigger=bvbt_… --status=…] [--limit=N]
build delivery show  --id=bvbd_…
build delivery replay --id=bvbd_…
build test           --id=bvbt_…          # fire a synthetic delivery now
build apply -f triggers.yml [--prune]      # declarative reconcile
```

- `--ref-include`/`--ref-exclude` repeatable or csv; empty include = all refs.
- `--token-scopes` reuses the M17 csv / `all` / `repo:*` / `lfs:*` parser.
- `--token-ttl` Go duration; validated `> 0` and `<= ceiling` at insert.
- `--secret` shown/echoed per M15 secret conventions; stored like webhook
  secret.
- bad kind / missing kind-required flags / over-ceiling TTL rejected at insert.

### 4.2 Declarative `build apply -f triggers.yml`

Reconciles `build_triggers` rows. Upsert by `(tenant, repo, name)` (preserve
`created_at`). `--prune` removes rows for the covered repos that are absent from
the file. The **DB stays the runtime source of truth**; YAML is a GitOps
authoring surface.

```yaml
triggers:
  - tenant: myorg
    repo: app
    name: main-cloudbuild
    kind: cloudbuild
    url: https://cloudbuild.googleapis.com/v1/projects/P/triggers/T:webhook?key=…&secret=…
    ref_include: ["refs/heads/main", "refs/heads/release/*"]
    ref_exclude: ["refs/heads/dependabot/**"]
    token_mode: none
  - tenant: myorg
    repo: app
    name: tags-codebuild
    kind: codebuild
    aws_region: us-east-1
    aws_project: app-release
    aws_connector: default          # references serve-config connector
    ref_include: ["refs/tags/v*"]
    token_mode: inject
    token_scopes: ["repo:read", "lfs:read"]
    token_ttl: 15m
```

### 4.3 Scoped operator YAML (`bucketvcs serve --config build.yml`)

Operator-level settings only (not per-repo data):

```yaml
build:
  defaults:
    token_ttl: 15m
    token_scopes: ["repo:read", "lfs:read"]
    audience: https://bucketvcs.example          # documented for OIDC-pull
  aws_connectors:
    default:
      region: us-east-1
      # Credentials resolution prefers the ambient AWS chain / named profile.
      profile: bucketvcs-codebuild               # OR omit to use env/IRSA/instance role
      # access_key/secret_key supported but DISCOURAGED (documented foot-gun).
```

AWS credential resolution **prefers the standard ambient chain** (env vars,
shared profile, IRSA, instance role) via `config.LoadDefaultConfig` — the same
mechanism M5's s3compat already relies on. Storing long-lived keys in YAML is
supported for completeness but documented as a foot-gun. The YAML root is
namespaced under `build:` so a future milestone can add sibling sections and
absorb the rest of the `serve` flags without a breaking change.

### 4.4 Dependencies

`aws-sdk-go-v2` (`config`, `credentials`, `sts`) and `go-jose/v4` are already
vendored. New modules:

- `github.com/aws/aws-sdk-go-v2/service/codebuild` — same SDK family already in
  use; low marginal cost.
- `gopkg.in/yaml.v3` — the only genuinely new dependency, for `apply -f` and
  the serve config.

## 5. Security model

- **Minted token blast radius:** short TTL (≤1h ceiling, 15m default) +
  single-repo `auth.Scope` + read-only scopes + revocable + swept. Acceptable
  even when injected and surfaced in build env/logs.
- **mint-and-inject defaults OFF** on generic/cloudbuild; ON only for codebuild
  where OIDC-pull is impractical. The secure default (no token in transit) is
  the path most users land on.
- **Generic/cloudbuild POST:** HMAC-signed (reused M15 signer); M25 egress
  policy guards SSRF at dial time; secret + token travel only over TLS.
- **CodeBuild:** SigV4 over TLS to the AWS endpoint; AWS creds prefer
  ambient/profile, never required in the DB.
- **Trust boundary:** creating a trigger is an admin/`repo:admin` action; the
  operator trusts the configured cloud endpoint. The realistic foot-gun — an
  injected token on a generic endpoint the operator doesn't control — is
  documented; default `none` makes it opt-in.
- **No raw token logging:** audit records trigger id, kind, decision, and (for
  mint) the minted token id only — never the secret value.

## 6. Observability

### 6.1 Metrics (OTel)
- `build_trigger_fired_total{kind, result}` — `delivered | failed | dead_letter`
- `build_trigger_delivery_duration_seconds` (includes cloud-call tail)
- `build_trigger_deadletter_total`
- `build_token_minted_total`
- `minted_tokens_swept_total{user}` (generalizes M22's sweep metric)
- No ref/repo-valued labels (cardinality under push volume / probing).

### 6.2 Audit events (`build.*`)
- `build.trigger.fired` — trigger id, kind, matched refs (count), delivery id.
- `build.trigger.delivered` / `build.trigger.failed` / `build.trigger.deadletter`.
- `build.token.minted` — minted token id, tenant/repo, scopes, ttl (correlates
  with later push/LFS audits via token id).
- `build.trigger.enqueue_failed` — fail-open diagnostic.
- `build.trigger.added | removed | enabled | disabled`.

## 7. Error handling

- **Enqueue** is fail-open: any error → `build.trigger.enqueue_failed` audit,
  push proceeds (a CI hiccup must never block a push).
- **Delivery** failures retry on the M15-style backoff schedule; exhaustion →
  `dead_letter` + audit; recoverable via `build delivery replay`.
- **Missing trigger** for an orphaned delivery → dead-letter (no crash).
- **Token mint failure** at delivery time → the delivery fails and retries
  (so a transient DB blip doesn't silently drop the token); never delivers a
  trigger that was supposed to carry a token without it.
- **CodeBuild `StartBuild` errors** map AWS retryable vs terminal to
  retry-vs-dead-letter; the AWS error code is recorded in `last_error`.

## 8. Testing

### 8.1 `internal/buildtrigger` unit
- Matcher: include match, exclude wins, `**` subtree exclude, empty include =
  all, tag vs branch patterns.
- Deliverers: generic/cloudbuild via `httptest` (assert body template, HMAC
  header, injected token field presence/absence by `token_mode`); codebuild via
  a `StartBuild` fake behind the interface (assert `sourceVersion`, env
  overrides, token present only when `inject`).
- Minter: scope bitmask, single-repo `auth.Scope`, TTL/ceiling, `_build` user.
- Worker: backoff, retry, dead-letter on exhaustion, reclaim of stuck
  `in_flight`, replay (mirror M15 worker tests).

### 8.2 Store / migration
- `build_triggers` + `build_trigger_deliveries` round-trip; `UNIQUE(tenant,
  repo,name)`; FK cascade on `repos` delete; orphan-delivery drain after
  trigger removal; migration up on both sqlite + postgres.

### 8.3 Enqueue integration
- Push with matching ref enqueues exactly one delivery per matching trigger;
  non-matching ref enqueues none; enqueue error is fail-open (push succeeds).

### 8.4 CLI + apply
- trigger add/list/remove/enable/disable; delivery list/show/replay; `build
  test`; `apply -f` upsert + `--prune`; bad kind / missing flags / over-ceiling
  TTL / bad scopes rejected; NDJSON shape.

### 8.5 Smoke (localfs, end-to-end)
1. create a `generic` trigger (`token_mode=inject`) pointing at a local
   `httptest`-style receiver, `ref_include=refs/heads/main`;
2. push to `main` → assert the receiver got a signed POST with the expected
   body and a `bvts_token`;
3. assert that `bvts` clones the repo (and only that repo — wrong repo 403);
4. push to a non-matching branch → assert zero delivery;
5. expired minted token → clone fails;
6. OIDC-pull variant: register issuer+rule, exchange a locally-signed JWT,
   confirm the same clone works with no token in the trigger.

## 9. Acceptance criteria

- A push to a matching ref fires exactly the matching triggers, durably, with
  retry/dead-letter/replay.
- A non-matching ref fires nothing.
- Cloud Build path: a generic/cloudbuild trigger POSTs a signed, correctly
  templated body; OIDC-pull lets the build obtain a single-repo `bvts` with no
  credential in the trigger.
- CodeBuild path: `StartBuild` is invoked with the right project/sourceVersion
  and an injected single-repo read-only `bvts`.
- Injected/minted tokens are short-lived, single-repo, read-only, revocable,
  and reaped by the sweep.
- Enqueue is fail-open; no CI failure can block a push.
- M15 webhooks and all existing auth/LFS/scope tests still pass (no changes to
  the webhook subsystem; build triggers are additive).
- Only new go.mod modules are `aws-sdk-go-v2/service/codebuild` and
  `gopkg.in/yaml.v3`.

## 10. Open questions

- **Sweep metric:** one `minted_tokens_swept_total{user}` vs separate
  `build_/oidc_tokens_swept_total`. Lean: single labeled metric.
- **`cloudbuild` vs `generic`:** whether `cloudbuild` warrants a distinct kind
  or is just `generic` + documented defaults. Lean: keep it as a thin preset
  kind for discoverability, sharing the generic deliverer.
- **AWS connector reference:** per-trigger inline AWS config vs named connector
  in serve YAML (`aws_connector: default`). Lean: named connectors (keeps creds
  out of per-repo rows), with region/project still per-trigger.
- **TTL ceiling:** reuse M22's 1h. Confirm at implementation.
