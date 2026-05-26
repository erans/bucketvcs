# M22 — OIDC Token Exchange (operator guide)

This guide covers the M22 OIDC token-exchange feature. It explains what shipped, how to register trusted issuers and trust rules with the `bucketvcs oidc` CLI, how CI workloads obtain short-lived access tokens, security guidance and limitations, and how to read the metrics and audit events.

Production readiness summary:

- RFC 8693 token-exchange endpoint `POST /_oidc/token` — **shipped**.
- Per-issuer JWKS discovery and signature verification (RS256/384/512, ES256/384) — **shipped**.
- Trust rules: `(issuer, audience, claim constraints) → (tenant, repo, scopes, ttl)` — **shipped**.
- Scope down-scoping via request `scope=` parameter — **shipped**.
- Short-lived repo-bound tokens swept by background goroutine — **shipped**.
- Rate-limiting on credential failures (shared M18 limiter) — **shipped**.
- Human/browser SSO, array-valued `aud`, tokens without `kid`, `jti` replay cache — **not shipped (v1 limitations, see §6)**.
- Migration `0010_oidc.sql` is forward-only and applied by the existing `RunMigrations`.

---

## 1. Overview

M22 lets CI/CD workloads exchange a short-lived IdP-issued identity token (an OIDC `id_token`) for a BucketVCS access token scoped to a single repository. The exchange is governed by operator-defined trust rules stored in the gateway's authdb.

**Why bother?** Long-lived static tokens checked into CI secrets rotate infrequently and are valid indefinitely. An OIDC-minted token lives for at most 1 hour (default 15 minutes), is bound to one repo, and carries only the scopes the rule grants. A leaked token expires on its own; there is no standing credential to rotate.

**Scope:** this feature is for machine workloads (CI runners, deployment pipelines). It is not a browser SSO or human login system. There is no redirect flow, no session cookie, and no interactive consent screen.

The endpoint implements [RFC 8693 OAuth 2.0 Token Exchange](https://www.rfc-editor.org/rfc/rfc8693). The CI workload:

1. Obtains an `id_token` from its CI platform.
2. POSTs it to `/_oidc/token` with `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`.
3. Receives a JSON response with `access_token`, `token_type: "Bearer"`, `expires_in`, and `scope`.
4. Uses the `access_token` as a Basic password to clone/push: `git clone http://x-access-token:$TOKEN@host/tenant/repo`.

---

## 2. Enabling

Add `--oidc` to `bucketvcs serve`:

```
bucketvcs serve \
    --addr=:8080 \
    --store=<backend-url> \
    --auth-db=<path> \
    --oidc \
    [--oidc-sweep-interval=5m]
```

**Flags:**

| Flag | Default | Description |
|---|---|---|
| `--oidc` | `false` | Enable the `/_oidc/token` endpoint and the token sweep goroutine. |
| `--oidc-sweep-interval` | `5m` | How often to delete expired OIDC-minted tokens from the authdb. |

**Important:** `/_oidc/token` is served on the HTTP listener only. If `--oidc` is set but no `--addr` is configured (HTTP disabled, SSH-only mode), the gateway logs a WARN at startup — the sweep goroutine still runs, but no exchange requests can reach the endpoint. CI workloads need direct HTTP(S) access to the gateway.

---

## 3. Registering trust

Trust is established in two steps: registering a trusted issuer, then adding one or more rules that map token claims to repo grants.

### 3.1 Register an issuer

```
bucketvcs oidc issuer add \
    --auth-db=<path> \
    --alias=<name> \
    --url=<issuer-url>
```

`--alias` is a short local name (e.g. `github`, `gitlab`). `--url` is the OIDC issuer URL exactly as it appears in the `iss` claim of tokens from that IdP.

On success:

```
alias=github  url=https://token.actions.githubusercontent.com
```

To list registered issuers:

```
bucketvcs oidc issuer list --auth-db=<path> [--format=text|json]
```

To remove an issuer (also removes all rules for that alias):

```
bucketvcs oidc issuer remove --auth-db=<path> --alias=<name>
```

### 3.2 Add a trust rule

```
bucketvcs oidc rule add \
    --auth-db=<path> \
    --issuer=<alias> \
    --audience=<aud> \
    --tenant=<tenant> \
    --repo=<repo> \
    --scopes=<csv> \
    --ttl=<duration> \
    [--claim name=value ...]
```

`--ttl` is a Go duration (e.g. `15m`, `1h`). Maximum is `1h`; values above the ceiling are rejected at the CLI surface. Default if omitted is `15m`.

`--scopes` accepts the same CSV format as `bucketvcs token create`: `repo:read`, `repo:write`, `lfs:read`, `lfs:write`, or `repo:read,lfs:read` etc.

**Caution on scope breadth:** although `--scopes` accepts the full token-scope vocabulary (`repo:admin`, `webhook:admin`, `storage:admin`, `all`), OIDC trust rules for CI runners should in practice grant only `repo:read`, `repo:write`, `lfs:read`, and/or `lfs:write`. Granting admin, webhook, or storage scopes to an automated workload is rarely appropriate and significantly widens the blast radius of a stolen or misused token.

`--claim name=value` can be repeated to require multiple claim constraints. Omit `--claim` entirely to create a wildcard rule (matches any token from the issuer — see §5).

On success:

```
id=bvor_01hq...  issuer=github  tenant=acme  repo=api  scopes=repo:read,lfs:read  ttl=15m0s  claims=1
```

### 3.3 Worked example — GitHub Actions

GitHub Actions provides OIDC tokens with `iss=https://token.actions.githubusercontent.com`. Tokens carry a `repository` claim (`org/repo`) and optionally `ref` (branch/tag ref), `workflow`, and others.

```
# 1. Register the issuer (idempotent; skip if already registered).
bucketvcs oidc issuer add \
    --auth-db=auth.db \
    --alias=github \
    --url=https://token.actions.githubusercontent.com

# 2. Rule granting read access to acme/api from the main branch of org/app.
bucketvcs oidc rule add \
    --auth-db=auth.db \
    --issuer=github \
    --audience=https://bucketvcs.example \
    --tenant=acme \
    --repo=api \
    --scopes=repo:read \
    --ttl=15m \
    --claim repository=org/app \
    --claim ref=refs/heads/main

# 3. Rule granting write access to the same repo from any branch of org/app
#    (omit --claim ref=... to match any branch).
bucketvcs oidc rule add \
    --auth-db=auth.db \
    --issuer=github \
    --audience=https://bucketvcs.example \
    --tenant=acme \
    --repo=api \
    --scopes=repo:write,lfs:write \
    --ttl=15m \
    --claim repository=org/app
```

`--audience` must match the `aud` claim in the token exactly. For GitHub Actions you request a specific audience when fetching the token; the value above must be passed to `getIDToken` in the workflow (see §4).

### 3.4 Worked example — GitLab CI

GitLab CI provides OIDC tokens with `iss=https://gitlab.com`. Tokens carry a `project_path` claim (`namespace/project`), `ref` (branch name or tag), `environment`, and others.

```
# 1. Register the issuer.
bucketvcs oidc issuer add \
    --auth-db=auth.db \
    --alias=gitlab \
    --url=https://gitlab.com

# 2. Rule granting push access to acme/infra from the main branch of mygroup/infra.
bucketvcs oidc rule add \
    --auth-db=auth.db \
    --issuer=gitlab \
    --audience=https://bucketvcs.example \
    --tenant=acme \
    --repo=infra \
    --scopes=repo:write \
    --ttl=15m \
    --claim project_path=mygroup/infra \
    --claim ref=main
```

### 3.5 List and remove rules

```
# List all rules for an issuer:
bucketvcs oidc rule list --auth-db=<path> --issuer=github [--format=text|json]

# List all rules for a specific repo:
bucketvcs oidc rule list --auth-db=<path> --repo=acme/api

# Remove a rule by its ID (shown in rule add output and rule list):
bucketvcs oidc rule remove --auth-db=<path> --id=bvor_01hq...
```

Either `--issuer` or `--repo` is required for `rule list`; they are mutually exclusive.

Wildcard rules (no claim constraints) are flagged in text output:

```
id=bvor_...  issuer=github  aud=...  tenant=acme  repo=api  scopes=repo:read  ttl=900s  claims=0  [WILDCARD: matches any token from issuer]
```

In JSON output the `wildcard` field is `true` for zero-claim rules.

---

## 4. Using from CI

### 4.1 GitHub Actions workflow snippet

The workflow must have `id-token: write` permission to request OIDC tokens. Pass the `--audience` value you registered in §3.3.

```yaml
jobs:
  deploy:
    runs-on: ubuntu-latest
    permissions:
      id-token: write
      contents: read
    steps:
      - name: Exchange OIDC token for BucketVCS access token
        id: bvtoken
        env:
          BUCKETVCS_URL: https://bucketvcs.example
          AUDIENCE: https://bucketvcs.example
        run: |
          # Request the id_token from the Actions token endpoint.
          IDTOKEN=$(curl -sSfL \
            -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
            -H "Accept: application/json;api-version=2.0" \
            "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=$AUDIENCE" \
            | jq -r .value)

          # Exchange it for a BucketVCS access token.
          ACCESS_TOKEN=$(curl -sSf \
            -X POST "$BUCKETVCS_URL/_oidc/token" \
            --data-urlencode "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
            --data-urlencode "subject_token=$IDTOKEN" \
            | jq -r .access_token)

          echo "::add-mask::$ACCESS_TOKEN"
          echo "access_token=$ACCESS_TOKEN" >> "$GITHUB_OUTPUT"

      - name: Clone repository
        run: |
          git clone \
            "http://x-access-token:${{ steps.bvtoken.outputs.access_token }}@bucketvcs.example/acme/api" \
            repo
```

**Username slot:** The username in the `user:password@host` URL is ignored for OIDC-minted tokens. Any value works; `x-access-token` and `_oidc` are conventional.

**Down-scoping at exchange time:** To request fewer scopes than the rule grants, add a `scope` form parameter to the exchange POST:

```sh
curl -X POST "$BUCKETVCS_URL/_oidc/token" \
  --data-urlencode "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
  --data-urlencode "subject_token=$IDTOKEN" \
  --data-urlencode "scope=repo:read"
```

The `scope` parameter can only narrow the grant; it cannot widen it.

### 4.2 GitLab CI snippet

GitLab exposes the OIDC token as a CI variable when `id_tokens` is configured:

```yaml
deploy:
  id_tokens:
    GITLAB_OIDC_TOKEN:
      aud: https://bucketvcs.example
  script:
    - |
      ACCESS_TOKEN=$(curl -sSf \
        -X POST "https://bucketvcs.example/_oidc/token" \
        --data-urlencode "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
        --data-urlencode "subject_token=$GITLAB_OIDC_TOKEN" \
        | jq -r .access_token)
      git clone "http://x-access-token:$ACCESS_TOKEN@bucketvcs.example/acme/infra" repo
```

---

## 5. Security guidance

### 5.1 You trust the IdP to assert claims truthfully

When a token passes signature verification and claim constraints, the gateway mints an access token. There is no additional out-of-band verification. The operator trusts the IdP to enforce that the `repository`, `project_path`, and other claims it issues accurately reflect the workload's identity. A compromised IdP or a misconfigured claim namespace breaks the trust model.

### 5.2 Always set `--audience` — never leave it unscoped

Audience is mandatory; `bucketvcs oidc rule add` refuses rules without `--audience`. Use an audience string unique to your BucketVCS deployment (e.g. its public URL). This prevents tokens issued for one system (say, a cloud deploy role) from being replayed against your BucketVCS gateway.

### 5.3 A no-`--claim` rule is an issuer-wide wildcard — the main foot-gun

A rule created without any `--claim` flags matches **every** token from that issuer whose `aud` matches, regardless of which workflow, project, or repository issued it. For GitHub Actions this means any workflow in any repository in any organization that requests a token with your audience can clone/push to the target repo.

Only use wildcard rules if you genuinely want any token from the IdP to have access. For production use, always constrain at minimum by `repository` (GitHub) or `project_path` (GitLab). The CLI flags wildcard rules visibly in `rule list` output.

### 5.4 Keep `--ttl` short

The default TTL is 15 minutes. OIDC-minted tokens are not revocable before expiry (the authdb row is present until the sweep removes it). Short TTLs limit the exposure window of a leaked token. The 1-hour ceiling is enforced at both rule creation (`bucketvcs oidc rule add`) and in the store; values above 1h are rejected.

### 5.5 Signature algorithm allowlist

Only asymmetric algorithms are accepted: RS256, RS384, RS512, ES256, ES384. The `alg:none` bypass and all HMAC algorithms (HS256, HS384, HS512) are rejected. This closes the RS256↔HS256 key-confusion attack family.

### 5.6 Discovery and JWKS are fetched over HTTPS only

HTTP issuer URLs are permitted solely for loopback addresses (`localhost`, `127.0.0.1`, `::1`) — for local testing. Any non-loopback `http://` issuer URL is rejected before any network call is made.

### 5.7 Rate limiting

Credential failures at `/_oidc/token` count against the M18 per-IP rate limiter (shared with Basic auth failures). Repeated invalid tokens will trigger 429 responses. The `--auth-rate-limit-*` serve flags control burst and refill. Issuer-unavailable (503) responses do NOT count as credential failures and do not increment the failure bucket.

---

## 6. Limitations (v1)

- **Array-valued `aud` is not matched.** The `aud` field in the JWT is read as a single string via a Go type assertion `claims["aud"].(string)`. If the IdP issues a token whose `aud` is a JSON array (`["https://...", "..."]`), the assertion fails → `aud` is treated as empty string → no rule matches → 403. This is a hard limitation of the current matcher. Affected tokens produce 403 `access_denied`, not 401 `invalid_token`. Workaround: configure the IdP to issue a single-string audience.

- **Issuers and tokens without a `kid` are not supported.** Key selection is by the `kid` header field. Issuers that publish keys without a `kid` in the JWKS, or tokens that omit `kid` in the JWS header, will fail verification (no multi-key fallback). Both GitHub Actions and GitLab CI include `kid`; most compliant OIDC issuers do.

- **No `jti` replay cache.** A stolen token can be exchanged multiple times until it expires. The mandatory audience and short TTL (default 15m, max 1h) are the primary mitigations. A per-token replay cache is deferred.

- **Exact claim matching only.** Claim constraints use exact string equality. Glob patterns, prefix matching, and regular expressions are not supported. To match multiple branches, create one rule per branch or omit the branch constraint and rely on `repository`/`project_path` alone.

- **No human/browser SSO.** There is no redirect-based authorization flow, no session cookie, and no consent screen. The feature is for machine-to-machine token exchange only.

- **`subject_token_type` is optional for client compat.** A present but non-JWT `subject_token_type` is rejected (400). An omitted `subject_token_type` is tolerated — the token is still fully verified.

---

## 7. Observability

### 7.1 Audit events

Two structured events emitted to the gateway's slog stream:

| Event | Level | Key attrs | When |
|---|---|---|---|
| `auth.oidc.exchanged` | INFO | `issuer`, `sub`, `tenant`, `repo`, `scopes`, `ttl_sec` | Successful exchange — token minted. |
| `auth.oidc.rejected` | WARN | `issuer`, `ip`, `reason` | Exchange rejected for any reason. |

`reason` values for `auth.oidc.rejected`:

| Value | Meaning |
|---|---|
| `unknown_issuer` | The token's `iss` is not registered in the authdb. |
| `invalid_token` | Signature or standard-claims check failed. |
| `no_rule` | Token is valid but no rule matches its `aud` + claims. |
| `issuer_unavailable` | Discovery or JWKS endpoint unreachable (retryable). |

### 7.2 Metrics

Two metrics, emitted as structured slog records with `msg="metric"` and `metric_name=<name>`:

| Metric | Type | Labels | Emission point |
|---|---|---|---|
| `oidc_exchange_total` | counter | `result` | Once per exchange attempt. |
| `oidc_tokens_swept_total` | counter | none | Once per sweep tick that deletes at least one token (value = count deleted). |

`result` values for `oidc_exchange_total`:

| Value | HTTP status | Cause |
|---|---|---|
| `minted` | 200 | Successful exchange. |
| `bad_request` | 400 | Bad form, unknown issuer, or malformed token structure. |
| `invalid_token` | 401 | Signature or claims verification failure. |
| `no_rule` | 403 | No matching trust rule. |
| `invalid_scope` | 400 | Requested scope exceeds or is invalid relative to the rule's grant. |
| `rate_limited` | 429 | Per-IP failure budget exhausted (M18 limiter). |
| `issuer_unavailable` | 503 | Discovery or JWKS endpoint unreachable. |

### 7.3 Quick log filter

```sh
# All successful exchanges (shows who got what, for which repo):
journalctl -u bucketvcs | grep "auth.oidc.exchanged"

# Rejected attempts (useful for diagnosing misconfigured rules):
journalctl -u bucketvcs | grep "auth.oidc.rejected"

# Exchange counter aggregated by result:
journalctl -u bucketvcs \
  | grep 'metric_name=oidc_exchange_total' \
  | grep -oP 'result=\S+' | sort | uniq -c
```

---

## 8. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| **401 `invalid_token`** | Token signature failed (wrong key, expired, issuer mismatch), or gateway clock is >60s skewed relative to the IdP. | Check NTP sync on the gateway host. Confirm the token has `exp` set and is not expired before you POST it. Confirm `--url` for the issuer matches `iss` in the token exactly (including trailing slashes). |
| **401 `invalid_token` — clock skew** | The gateway and IdP clocks differ by more than 60 seconds. | Synchronise with NTP. The 60s skew tolerance is fixed; it is not configurable. |
| **401 `invalid_token` — no kid** | The token's JWS header omits `kid`, or the issuer's JWKS has no `kid` on its keys. | Use an IdP that publishes keyed JWKs. GitHub Actions and GitLab CI both include `kid`. |
| **403 `access_denied`** | No trust rule matched the token. | Run `bucketvcs oidc rule list --auth-db=<path> --issuer=<alias>` and confirm the `aud` and claim values match what the token actually contains. Decode the token payload with `jq -R 'split(".") | .[1] | @base64d | fromjson' <<< "$TOKEN"`. Check for array `aud` — if the token has `"aud": ["val1", "val2"]` rather than `"aud": "val1"`, the matcher silently fails (v1 limitation; see §6). |
| **400 `invalid_request` — unknown issuer** | The token's `iss` value is not registered in the authdb. | Register the issuer: `bucketvcs oidc issuer add --alias=... --url=<exact-iss-value>`. |
| **400 `invalid_request` — bad form** | `grant_type` is missing or wrong, `subject_token` is empty, or the POST body is not `application/x-www-form-urlencoded`. | Verify the `curl` invocation uses `--data-urlencode` (not `-d`) for each parameter, and that `grant_type` is exactly `urn:ietf:params:oauth:grant-type:token-exchange`. |
| **400 `invalid_scope`** | The `scope=` request parameter contains a scope not in the rule's grant, or an unrecognised scope name. | Omit `scope=` to use the full rule grant, or request only scopes that are a subset of the rule's scopes. |
| **503 `temporarily_unavailable`** | The gateway could not reach the issuer's discovery endpoint (`<issuer-url>/.well-known/openid-configuration`) or its JWKS URI. | Check network connectivity from the gateway to the IdP. Check DNS resolution for the issuer URL. The error is retryable — the next exchange attempt will re-fetch discovery. |
| **429** | Per-IP rate limit on credential failures (M18). | Reduce retry rate. Wait for the bucket to refill (default: 1 failure cleared per minute when idle, burst=10). Operator can adjust with `--auth-rate-limit-burst` and `--auth-rate-limit-refill-per-minute`. |
| **Token works but git clone prompts for password** | OIDC token bound to a different repo than the one being cloned. | OIDC-minted tokens are bound to exactly one `(tenant, repo)` pair. The attempt to use the token against a different repo returns 403 at the scope-check layer, which git surfaces as an auth prompt. Mint a separate token for each repo. |
