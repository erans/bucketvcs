# AWS CodeBuild trigger example (inject mode)

End-to-end recipe for starting an **AWS CodeBuild** build whenever you push to a
repository on your own **bucketvcs** instance, using a `codebuild` build
trigger.

**Key difference from Cloud Build:** CodeBuild has **no inbound webhook** for
custom sources. Instead, bucketvcs calls the CodeBuild **`StartBuild` API
directly (SigV4)** and injects the build context as environment-variable
overrides. So there is no webhook URL to register — you create a CodeBuild
project, give bucketvcs AWS credentials allowed to start it, and register a
`--kind=codebuild` trigger naming the region + project.

```
git push ──▶ bucketvcs (--build-triggers, AWS creds in env)
                │ matches ref-include refs/heads/main
                │ mints a short-lived repo:read token
                ▼ codebuild:StartBuild (SigV4)  env overrides: BVTS_TOKEN/BV_REPO/BV_COMMIT/BV_REF
            AWS CodeBuild (NO_SOURCE project, image golang:1.26)
                │ runs buildspec.yml
                ▼ git clone https://x-access-token:$BVTS_TOKEN@$BUCKETVCS_HOST/$BV_REPO
            build container ──▶ bucketvcs (public HTTPS) ── clone @ BV_COMMIT, go build/test
```

Files in this directory:
- `buildspec.yml` — the build steps (reference; paste into the project in the console).
- `create-project.yaml` — input for `aws codebuild create-project --cli-input-yaml` (embeds the same buildspec, NO_SOURCE).

For the security-hardened OIDC-pull variant, see
[`docs/operator-guides/build-triggers.md`](../../docs/operator-guides/build-triggers.md) §3
— note OIDC-pull is awkward from CodeBuild (AWS does not natively mint
arbitrary-audience OIDC tokens), so inject is the practical default here.

---

## Prerequisites

- A running bucketvcs gateway started with `--build-triggers`, reachable from
  CodeBuild over **public HTTPS** (the build container must `git clone` it).
- The bucketvcs process must have **AWS credentials** allowed to call
  `codebuild:StartBuild` on the project (ambient chain — env vars, profile, or
  instance role — or a named connector via `--build-config`).
- `aws` CLI authenticated; a CodeBuild **service role** (standard CloudWatch
  Logs permissions) — create one in the IAM console if you don't have it.

Replace `HOST` (bucketvcs public host, no scheme), `ACCOUNT_ID`, and `REGION`
throughout.

---

## 1. Create the CodeBuild project

Edit `create-project.yaml` (set `BUCKETVCS_HOST`, `serviceRole`), then:

```bash
aws codebuild create-project --region REGION --cli-input-yaml file://create-project.yaml
```

(Or in the console: create a project, **Source: No source**, **Environment image:
`golang:1.26`** with image pull credentials = CodeBuild, add an environment
variable `BUCKETVCS_HOST=HOST`, and paste `buildspec.yml` as the buildspec.)

## 2. Let bucketvcs call StartBuild

Attach this policy to the IAM identity whose credentials bucketvcs runs with:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": "codebuild:StartBuild",
    "Resource": "arn:aws:codebuild:REGION:ACCOUNT_ID:project/bvcs-build"
  }]
}
```

Then run the gateway with those credentials available (ambient chain shown;
`--lfs=false` because LFS otherwise requires proxied-URL signing keys):

```bash
export AWS_ACCESS_KEY_ID=...        # or AWS_PROFILE=..., or an instance role
export AWS_SECRET_ACCESS_KEY=...
bucketvcs serve --store="$STORE" --auth-db="$AUTHDB" \
  --addr=127.0.0.1:8080 --build-triggers --lfs=false
```

> **Named profile instead of ambient creds (optional):** create a
> `build-config.yaml` and pass `--build-config build-config.yaml`, then add
> `--aws-connector=default` to the trigger in step 3:
> ```yaml
> build:
>   aws_connectors:
>     default:
>       region: REGION
>       profile: bucketvcs-codebuild
> ```

## 3. Register the codebuild trigger

```bash
bucketvcs build trigger add --auth-db="$AUTHDB" \
  --tenant=acme --repo=bucketvcs --name=codebuild-main \
  --kind=codebuild \
  --aws-region=REGION --aws-project=bvcs-build \
  --ref-include=refs/heads/main
  # token-mode defaults to inject for codebuild; token-scopes default repo:read,lfs:read
```

## 4. Push and verify

```bash
git commit --allow-empty -m "trigger codebuild" && git push <bucketvcs-remote> main

bucketvcs build delivery list --auth-db="$AUTHDB"     # → a 'delivered' row
aws codebuild list-builds-for-project --project-name bvcs-build --region REGION
```

If a delivery shows `failed`/`dead_letter`, inspect it:

```bash
bucketvcs build delivery show --auth-db="$AUTHDB" --id=bvbd_...
```

- An AWS auth/permission error → the bucketvcs identity lacks `codebuild:StartBuild`
  on the project ARN, or the region/project name in the trigger is wrong.
- Build starts but clone fails → check `BUCKETVCS_HOST` on the project and that
  `BV_REPO` matches the registered repo path.
