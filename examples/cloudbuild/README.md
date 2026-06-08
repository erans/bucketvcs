# Cloud Build trigger example (inject mode)

End-to-end recipe for firing a **Google Cloud Build** build whenever you push to
a repository hosted on your own **bucketvcs** instance, using a `cloudbuild`
build trigger in **`token-mode=inject`** (the short-lived pull token is sent in
the webhook body and mapped to a Cloud Build substitution — no OIDC setup).

For the security-hardened **OIDC-pull** variant (no token in the request), see
[`docs/operator-guides/build-triggers.md`](../../docs/operator-guides/build-triggers.md) §3.

`cloudbuild.yaml` in this directory is the inline build config: a webhook
trigger has no connected source repo, so the build clones the bucketvcs repo
itself, checks out the pushed commit, and builds it.

---

## Prerequisites

- A running bucketvcs gateway started with `--build-triggers`, reachable from
  Cloud Build over **public HTTPS** (the build VM must be able to `git clone`
  it). A `cloudflared tunnel --url http://127.0.0.1:8080` quick tunnel is the
  fastest way to get a public HTTPS URL for testing.
- A repository registered + pushed to that instance (e.g. `acme/bucketvcs`).
- `gcloud` authenticated against your GCP project, the Cloud Build API enabled.

Throughout, replace:

| placeholder | meaning |
|---|---|
| `HOST` | your bucketvcs public host, **no scheme** (e.g. `abc.trycloudflare.com`) |
| `PROJECT` | your GCP project id |
| `API_KEY` | a GCP API key string (Credentials → API key) |
| `SECRET_VALUE` | the webhook shared-secret string you store below |

---

## 1. Create the webhook shared secret (Cloud Build validates the inbound call)

```bash
printf 'SECRET_VALUE' | gcloud secrets create bvcs-webhook --data-file=- --project=PROJECT

PN=$(gcloud projects describe PROJECT --format='value(projectNumber)')
gcloud secrets add-iam-policy-binding bvcs-webhook --project=PROJECT \
  --member="serviceAccount:service-${PN}@gcp-sa-cloudbuild.iam.gserviceaccount.com" \
  --role="roles/secretmanager.secretAccessor"
```

## 2. Create the Cloud Build webhook trigger (with body → substitution mapping)

```bash
gcloud builds triggers create webhook --project=PROJECT \
  --name=bvcs-main \
  --secret="projects/PROJECT/secrets/bvcs-webhook/versions/1" \
  --inline-config=cloudbuild.yaml \
  --substitutions='_BVTS_TOKEN=$(body.bvts_token),_COMMIT=$(body.commit),_TENANT=$(body.tenant),_REPO=$(body.repo),_REPO_HOST=HOST'
```

The trigger's webhook URL is:

```
https://cloudbuild.googleapis.com/v1/projects/PROJECT/triggers/bvcs-main:webhook?key=API_KEY&secret=SECRET_VALUE
```

## 3. Register that URL as a bucketvcs trigger

```bash
bucketvcs build trigger add --auth-db=./auth.db \
  --tenant=acme --repo=bucketvcs --name=cloudbuild-main \
  --kind=cloudbuild \
  --url='https://cloudbuild.googleapis.com/v1/projects/PROJECT/triggers/bvcs-main:webhook?key=API_KEY&secret=SECRET_VALUE' \
  --ref-include=refs/heads/main \
  --token-mode=inject \
  --token-scopes=repo:read
```

> Single-quote the `--url` — it contains `&`. Cloud Build authenticates the
> inbound call via the URL `secret`, so it ignores bucketvcs's `BucketVCS-Signature`
> HMAC header; that is expected.

## 4. Push and verify

```bash
git commit --allow-empty -m "trigger cloud build" && git push <bucketvcs-remote> main

bucketvcs build delivery list --auth-db=./auth.db      # → a 'delivered' row
gcloud builds list --project=PROJECT --limit=3          # → a running/finished build
```

If a delivery shows `failed`/`dead_letter`, inspect it:

```bash
bucketvcs build delivery show --auth-db=./auth.db --id=bvbd_...
```

- HTTP 400 from Cloud Build → wrong `key`/`secret` in the registered URL.
- Build clones but fails → check `_REPO_HOST` and that the repo path
  (`_TENANT`/`_REPO`) matches what you registered.

---

## How the pieces connect

```
git push ──▶ bucketvcs (--build-triggers)
                │ matches ref-include refs/heads/main
                │ mints a short-lived repo:read token, renders cloudbuild body
                ▼ POST  …/triggers/bvcs-main:webhook?key=…&secret=…
            Cloud Build  ── maps $(body.*) → _BVTS_TOKEN/_COMMIT/_TENANT/_REPO
                │ runs cloudbuild.yaml
                ▼ git clone https://x-access-token:$_BVTS_TOKEN@HOST/$_TENANT/$_REPO
            build VM ──▶ bucketvcs (public HTTPS) ── clone @ _COMMIT, go build/test
```
