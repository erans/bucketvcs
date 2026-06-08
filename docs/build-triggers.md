# Build triggers: fire CI/CD on push

bucketvcs can call out to a build system every time someone pushes — to start a
Google Cloud Build, an AWS CodeBuild, or any HTTP endpoint you control — and
hand that build a **short-lived, single-repo, read-only token** to clone the
code. This page explains how it works and walks through a quickstart for each
cloud.

- For the complete reference (CLI, declarative `apply`, all flags, security,
  observability), see the **[operator guide](operator-guides/build-triggers.md)**.
- Copy-paste setups live in **[`examples/cloudbuild/`](../examples/cloudbuild/)**
  and **[`examples/codebuild/`](../examples/codebuild/)**.

---

## How it works

### Two network directions (only one needs a public address)

A build trigger involves two independent flows, and confusing them is the most
common setup snag:

1. **The trigger** — bucketvcs → the build system. This is **outbound** from
   bucketvcs (to `cloudbuild.googleapis.com` or the CodeBuild API). It works
   from anywhere with internet; nothing inbound is required.
2. **The clone** — the build worker → bucketvcs. This is **inbound**: the build
   VM runs `git clone` against your bucketvcs gateway, so that gateway must be
   reachable over **public HTTPS**. This is the real prerequisite (see
   [Prerequisite](#prerequisite-a-publicly-reachable-bucketvcs) below).

```
git push ──▶ bucketvcs gateway (--build-triggers)
                │ 1. ref filter: does the pushed ref match this trigger?
                │ 2. (optional) mint a short-lived repo-scoped token
                │ 3. deliver — durably, with retry/dead-letter/replay
                ▼
        ┌───────────────────────────┬──────────────────────────────┐
        │ generic / cloudbuild       │ codebuild                     │
        │ signed JSON POST           │ SigV4 StartBuild API call     │
        │ → your URL / Cloud Build   │ → AWS CodeBuild               │
        └───────────────────────────┴──────────────────────────────┘
                │ build obtains a token, then…
                ▼ git clone https://…@<bucketvcs-host>/<tenant>/<repo>  (inbound, public HTTPS)
        build worker ──▶ bucketvcs ── clones at the pushed commit, builds
```

### Three trigger kinds

| Kind         | How bucketvcs delivers                        | Use it for |
|--------------|-----------------------------------------------|------------|
| `generic`    | HMAC-signed JSON `POST` to any URL            | your own receiver / API Gateway / Lambda |
| `cloudbuild` | same signed `POST`, Cloud-Build-shaped body   | Google Cloud Build's native webhook trigger |
| `codebuild`  | native SigV4 `StartBuild` API call            | AWS CodeBuild (which has **no** inbound webhook) |

`cloudbuild` is just `generic` with a body pre-shaped for Cloud Build's
`$(body.…)` substitutions. `codebuild` is genuinely different: AWS has no
generic webhook for custom sources, so bucketvcs calls the CodeBuild API
directly and passes the build context as environment-variable overrides.

### How the build gets a token

The build must authenticate to clone. Two models:

- **OIDC-pull (default, most secure):** the trigger carries **no credential**.
  The build presents its own cloud-issued OIDC identity to bucketvcs's
  `/_oidc/token` endpoint and exchanges it for a `bvts` token. Nothing secret
  travels through the trigger. Clean on Cloud Build; awkward on CodeBuild (AWS
  doesn't natively mint arbitrary-audience OIDC tokens).
- **Mint-and-inject:** bucketvcs mints a short-lived token at trigger time and
  injects it into the delivery (a body field for generic/cloudbuild, an env
  override for codebuild). Dead simple for the build; the token is visible in
  the build's environment/logs, but it is short-TTL, single-repo, and
  read-only, so the blast radius is small.

The quickstarts below use **inject** (fewest moving parts). The
[operator guide §3](operator-guides/build-triggers.md) covers the OIDC-pull
hardening.

### What bucketvcs sends

- **generic / cloudbuild** — `POST` with `Content-Type: application/json`, a
  `BucketVCS-Signature: t=<unix>,v1=<hmac>` header (HMAC-SHA256 of the body with
  the trigger secret), and a JSON body:
  `tenant, repo, actor, tx_id, head_oid, ref_update{refname,old_oid,new_oid}` —
  plus, for `cloudbuild`, top-level `ref` and `commit`, and (inject mode)
  `bvts_token`.
- **codebuild** — `StartBuild` with `projectName`, `sourceVersion` = the pushed
  commit, and environment overrides `BV_REF`, `BV_REPO` (`<tenant>/<repo>`),
  `BV_COMMIT`, and (inject mode) `BVTS_TOKEN`.

### Durable and fail-open

Deliveries are enqueued durably and retried on a backoff schedule
(`1m → 30m → 2h → 12h`), then dead-lettered and replayable — a momentary blip at
the build system never loses the event. Enqueue is **fail-open**: if the trigger
machinery hiccups, your push still succeeds. (Details:
[operator guide §8](operator-guides/build-triggers.md).)

---

## Prerequisite: a publicly reachable bucketvcs

Because the build worker clones over HTTPS, your gateway needs a public HTTPS
URL.

- **For a quick test:** a tunnel gives you one instantly —
  `cloudflared tunnel --url http://127.0.0.1:8080` (or `ngrok http 8080`) prints
  a `https://….trycloudflare.com` host.
- **For real use:** run bucketvcs behind a TLS-terminating reverse proxy
  (Caddy/nginx) on a domain, or on Cloud Run / a VM with a cert.

Throughout the quickstarts, `HOST` is that public host **without a scheme**
(e.g. `abc.trycloudflare.com`).

Start the gateway with build triggers enabled (LFS off here only to skip its
proxied-URL signing-key requirement):

```bash
export STORE="localfs:/var/lib/bucketvcs"
export AUTHDB="./auth.db"
bucketvcs serve --store="$STORE" --auth-db="$AUTHDB" \
  --addr=127.0.0.1:8080 --build-triggers --lfs=false
```

Create access and push the repo you want built (see the
[main quickstart](quickstart.md) for the full walk-through):

```bash
bucketvcs user add alice --auth-db="$AUTHDB"
TOKEN=$(bucketvcs token create alice --auth-db="$AUTHDB" \
  --scopes=repo:read,repo:write --label=push | sed -n 's/^token=//p')
bucketvcs repo register acme/app --auth-db="$AUTHDB" --store="$STORE"
bucketvcs repo grant alice acme/app write --auth-db="$AUTHDB"
git remote add bvcs "https://x-access-token:${TOKEN}@${HOST}/acme/app"
git push bvcs main
```

---

## Quickstart A — Google Cloud Build (inject)

Full detail + the ready-to-run config: **[`examples/cloudbuild/`](../examples/cloudbuild/)**.

1. **Store the webhook secret** Cloud Build validates the inbound call against:
   ```bash
   printf 'A_LONG_RANDOM_SECRET' | gcloud secrets create bvcs-webhook --data-file=- --project=PROJECT
   PN=$(gcloud projects describe PROJECT --format='value(projectNumber)')
   gcloud secrets add-iam-policy-binding bvcs-webhook --project=PROJECT \
     --member="serviceAccount:service-${PN}@gcp-sa-cloudbuild.iam.gserviceaccount.com" \
     --role="roles/secretmanager.secretAccessor"
   ```
2. **Create the Cloud Build webhook trigger** with the inline build config and
   the body→substitution mapping (the config is
   [`examples/cloudbuild/cloudbuild.yaml`](../examples/cloudbuild/cloudbuild.yaml)):
   ```bash
   gcloud builds triggers create webhook --project=PROJECT \
     --name=bvcs-main \
     --secret="projects/PROJECT/secrets/bvcs-webhook/versions/1" \
     --inline-config=examples/cloudbuild/cloudbuild.yaml \
     --substitutions='_BVTS_TOKEN=$(body.bvts_token),_COMMIT=$(body.commit),_TENANT=$(body.tenant),_REPO=$(body.repo),_REPO_HOST=HOST'
   ```
   The trigger's webhook URL is
   `https://cloudbuild.googleapis.com/v1/projects/PROJECT/triggers/bvcs-main:webhook?key=API_KEY&secret=A_LONG_RANDOM_SECRET`.
3. **Register it as a bucketvcs trigger** (single-quote the URL — it has `&`):
   ```bash
   bucketvcs build trigger add --auth-db="$AUTHDB" \
     --tenant=acme --repo=app --name=cloudbuild-main --kind=cloudbuild \
     --url='https://cloudbuild.googleapis.com/v1/projects/PROJECT/triggers/bvcs-main:webhook?key=API_KEY&secret=A_LONG_RANDOM_SECRET' \
     --ref-include=refs/heads/main --token-mode=inject --token-scopes=repo:read
   ```
4. **Push and verify:**
   ```bash
   git commit --allow-empty -m "trigger cloud build" && git push bvcs main
   bucketvcs build delivery list --auth-db="$AUTHDB"   # → a 'delivered' row
   gcloud builds list --project=PROJECT --limit=3
   ```

Cloud Build authenticates the inbound call with the URL `secret`, so it ignores
bucketvcs's `BucketVCS-Signature` header — that's expected.

---

## Quickstart B — AWS CodeBuild (inject)

CodeBuild has **no inbound webhook**, so there's no URL to register — bucketvcs
drives it via SigV4 `StartBuild`. Full detail + ready-to-run config:
**[`examples/codebuild/`](../examples/codebuild/)**.

1. **Create a `NO_SOURCE` CodeBuild project** (it clones bucketvcs itself). Edit
   [`examples/codebuild/create-project.yaml`](../examples/codebuild/create-project.yaml)
   (set `BUCKETVCS_HOST` and the service-role ARN), then:
   ```bash
   aws codebuild create-project --region REGION --cli-input-yaml file://examples/codebuild/create-project.yaml
   ```
   It uses the `golang:1.26` image (Go + git) and the buildspec from
   [`examples/codebuild/buildspec.yml`](../examples/codebuild/buildspec.yml).
2. **Let bucketvcs call StartBuild** — attach a policy allowing
   `codebuild:StartBuild` on the project ARN to the identity bucketvcs runs
   with, and start the gateway with those AWS credentials available (ambient
   chain, a named profile via `--build-config`, or an instance role):
   ```bash
   export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...   # or AWS_PROFILE / instance role
   bucketvcs serve --store="$STORE" --auth-db="$AUTHDB" \
     --addr=127.0.0.1:8080 --build-triggers --lfs=false
   ```
3. **Register the trigger** (no URL — region + project):
   ```bash
   bucketvcs build trigger add --auth-db="$AUTHDB" \
     --tenant=acme --repo=app --name=codebuild-main --kind=codebuild \
     --aws-region=REGION --aws-project=bvcs-build --ref-include=refs/heads/main
     # token-mode defaults to inject for codebuild
   ```
4. **Push and verify:**
   ```bash
   git commit --allow-empty -m "trigger codebuild" && git push bvcs main
   bucketvcs build delivery list --auth-db="$AUTHDB"   # → a 'delivered' row
   aws codebuild list-builds-for-project --project-name bvcs-build --region REGION
   ```

---

## Troubleshooting

When a delivery shows `failed` or `dead_letter`, inspect it:

```bash
bucketvcs build delivery list --auth-db="$AUTHDB"
bucketvcs build delivery show --auth-db="$AUTHDB" --id=bvbd_...   # status code + error
bucketvcs build delivery replay --auth-db="$AUTHDB" --id=bvbd_... # re-queue after a fix
```

- **Cloud Build returns HTTP 400** → wrong `key`/`secret` in the registered URL.
- **AWS auth/permission error** → the bucketvcs identity lacks
  `codebuild:StartBuild`, or the trigger's region/project is wrong.
- **Build starts but the clone fails** → the build can't reach bucketvcs (not
  publicly resolvable/HTTPS), or `HOST` / the repo path is wrong.
- **No delivery at all** → the pushed ref didn't match `--ref-include`; confirm
  with `bucketvcs build trigger list --tenant=acme --repo=app`, or fire a
  synthetic one with `bucketvcs build test --id=bvbt_...`.

---

## Going further

- **Harden the token path** with OIDC-pull (no token in transit):
  [operator guide §3](operator-guides/build-triggers.md).
- **Generic receivers / signature verification**, declarative `apply -f`,
  metrics, audit events, retry semantics, replica behavior, secret rotation:
  [operator guide](operator-guides/build-triggers.md).
- **Worked, runnable configs:** [`examples/cloudbuild/`](../examples/cloudbuild/),
  [`examples/codebuild/`](../examples/codebuild/).
