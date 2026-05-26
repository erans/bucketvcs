# M22: OIDC token exchange (CI/CD workload identity)

Date: 2026-05-25
Spec section: §30.1.5 ("MAY support short-lived OIDC-exchanged tokens"), deferred in M17.

## 1. Goals

Let a CI/CD workload present its IdP-issued OIDC `id_token` to bucketvcs and
receive a short-lived, repo-scoped `bvts` token in return (RFC 8693 token
exchange). No human, no browser, no UI. The minted token is an ordinary
`bvts` token row, so every downstream auth path (scope checks, expiry,
revocation, rate-limiting, LFS, Git credential helpers) understands it
without modification.

### 1.1 In scope

- **Generic OIDC issuers.** Operator registers any issuer by URL; we fetch its
  `/.well-known/openid-configuration` discovery doc + JWKS and validate
  signatures uniformly. No provider-specific hardcoding (works with GitHub
  Actions, GitLab, Buildkite, CircleCI, self-hosted Keycloak, …).
- **Claim-rules → direct grant.** A trust-rules table maps
  `(issuer, expected audience, exact claim constraints)` → grant
  `(tenant, repo, scope bitmask, ttl)`. First match wins. No new identity
  type; mirrors the existing deploy-key `auth.Scope` short-circuit.
- **Short-lived stored `bvts` tokens.** Exchange mints a normal token row with
  a short `expires_at` and the rule's scopes, bound to one repo. Reuses the
  entire auth stack verbatim.
- **Exact-equality claim matching.** A rule constraint matches a claim only on
  exact string equality. "Any branch" = omit the `ref` constraint. (Glob
  matching is a clean future extension via a `match_type` column.)
- **RFC 8693 wire shape** at `POST /_oidc/token`.
- **Token-expiry sweep** for minted tokens (new; today nothing reaps expired
  rows).
- **Operator CLI**: `bucketvcs oidc issuer …` and `bucketvcs oidc rule …`.

### 1.2 Out of scope (deferred, documented)

- **`jti` replay cache** — short TTL + mandatory exact `aud` make this
  low-value for v1. Note as follow-on.
- **Human / browser SSO** (§30.4) — no UI exists; separate milestone.
- **SCIM provisioning** — no UI.
- **mTLS subject-token type** — only `urn:ietf:params:oauth:token-type:jwt`
  accepted.
- **Glob claim matching** — exact-only v1; `match_type` column added when an
  operator needs "branches but not tags".
- **Provider presets** (auto-fill GitHub/GitLab claim names) — generic engine
  first; presets later if config pain surfaces.
- **Per-issuer signing-key pinning** beyond JWKS — trust the discovery doc.
- **Service accounts / machine tokens** (M17 deferral) — not reopened; the
  direct-grant model deliberately avoids them.

## 2. Architecture overview

```
CI workload                         bucketvcs gateway
-----------                         -----------------
id_token (JWT)  ──POST /_oidc/token──▶  oidc_exchange.go (outside Basic-auth mw)
                                          │ 1. parse RFC 8693 form
                                          │ 2. unverified iss → oidc_issuers lookup
                                          │ 3. internal/oidc.Verify(token, issuerCfg)
                                          │      discovery + JWKS + sig + iss/aud/exp
                                          │ 4. match first oidc_trust_rule (exact claims)
                                          │ 5. effective scopes = rule.scopes ∩ requested
                                          │ 6. Store.CreateToken(_oidc, ttl, scopes, repo-bind)
                                          ▼ 7. JSON { access_token: bvts_…, expires_in, … }
bvts token  ──git push (Basic pw)──▶  existing auth middleware
                                          VerifyCredential → Actor + auth.Scope
                                          + M17 CheckScope (unchanged downstream)
```

### 2.1 New package: `internal/oidc`

Pure verification + discovery; no DB, no gateway types.

- `discovery.go` — fetch & cache `<issuer>/.well-known/openid-configuration`
  (yields `jwks_uri`, `id_token_signing_alg_values_supported`). Cached per
  issuer with a TTL; HTTPS-only.
- `jwks.go` — JWKS cache keyed by issuer. Lazy fetch. **Refresh on unknown
  `kid`** (IdP key rotation), guarded by a min-refresh interval so a flood of
  bogus `kid`s cannot amplify into outbound IdP requests. Key parsing via the
  already-vendored `github.com/go-jose/go-jose/v4`.
- `verify.go` — `Verify(ctx, rawToken string, cfg IssuerConfig) (Claims, error)`:
  1. parse JWS; **reject `alg:none`**; require `alg` ∈ asymmetric allowlist
     (`RS256 RS384 RS512 ES256 ES384`).
  2. verify signature against the issuer keyset (fetch/rotate as needed).
  3. validate `iss` (exact), `exp`/`nbf`/`iat` with bounded ±60s skew;
     reject a token with no `exp`.
  4. return the decoded claims map. **`aud` is validated by the caller**
     against the matched rule's audience (claims returned include `aud`).

`internal/oidc` never touches HMAC verification — the allowlist makes
RS256↔HS256 confusion unrepresentable.

### 2.2 Store layer (`internal/auth/sqlitestore`) — migration `0010_oidc.sql`

```sql
CREATE TABLE oidc_issuers (
    alias       TEXT PRIMARY KEY,            -- short operator handle, used in audit/actor names
    issuer_url  TEXT NOT NULL UNIQUE,        -- exact-match against token `iss`
    created_at  INTEGER NOT NULL
);

CREATE TABLE oidc_trust_rules (
    id            TEXT PRIMARY KEY,          -- bvor_<id>
    issuer_alias  TEXT NOT NULL REFERENCES oidc_issuers(alias) ON DELETE CASCADE,
    audience      TEXT NOT NULL,             -- exact `aud`; empty rejected at insert
    tenant        TEXT NOT NULL,
    repo          TEXT NOT NULL,
    scopes        INTEGER NOT NULL,          -- M17 TokenScope bitmask
    ttl_seconds   INTEGER NOT NULL,          -- validated <= ceiling at insert
    created_at    INTEGER NOT NULL,
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);
CREATE INDEX oidc_rules_issuer_idx ON oidc_trust_rules(issuer_alias);
CREATE INDEX oidc_rules_repo_idx   ON oidc_trust_rules(tenant, repo);

CREATE TABLE oidc_rule_claims (
    rule_id     TEXT NOT NULL REFERENCES oidc_trust_rules(id) ON DELETE CASCADE,
    claim_name  TEXT NOT NULL,
    claim_value TEXT NOT NULL,               -- exact-equality match
    PRIMARY KEY (rule_id, claim_name)
);

-- Reserved system user so OIDC-minted tokens satisfy tokens.user_id NOT NULL.
INSERT INTO users (id, name, is_admin, created_at)
VALUES ('_oidc', '_oidc', 0, strftime('%s','now'));

-- Repo-binding columns on tokens (NULL for ordinary user tokens).
ALTER TABLE tokens ADD COLUMN scope_tenant TEXT;
ALTER TABLE tokens ADD COLUMN scope_repo   TEXT;
ALTER TABLE tokens ADD COLUMN scope_perm   TEXT;   -- 'read' | 'write' | NULL
```

Rule **matching** (in Go, over rows loaded for the issuer): a rule matches iff
`audience == token.aud` AND every `oidc_rule_claims` row for that rule is
present in the token claims and string-equal. A rule with zero claim rows
matches any token from its issuer — the operator's explicit choice, surfaced in
`oidc rule list` so it is never a silent surprise. **First match wins**, ordered
deterministically by `(tenant, repo, id)`.

### 2.3 Minted-token shape (deploy-key parallel)

OIDC tokens are real `tokens` rows:

- `user_id = '_oidc'`, `expires_at = now + rule.ttl`, `scopes = effective`,
  `label = oidc:<issuer-alias>:<sub>`.
- `scope_tenant/scope_repo/scope_perm` set from the rule.

When `VerifyCredential` matches a token whose `scope_*` columns are non-NULL it
returns a non-nil `auth.Scope{Tenant, Repo, Perm}` — exactly the deploy-key
short-circuit — so `LookupRepoPerm` is bypassed and the token cannot reach any
other repo. The M17 `scopes` bitmask still narrows operations via `CheckScope`.
Ordinary user tokens leave `scope_*` NULL and behave identically to today.

`Store.CreateToken` gains optional repo-binding parameters (or a sibling
`CreateScopedToken`); the existing call sites pass nil/empty and are unchanged.

### 2.4 Gateway endpoint: `internal/gateway/oidc_exchange.go`

Mounted `s.mux.Handle("/_oidc/", …)` alongside `/_lfs/`, `/_bundle/`, `/_pack/`
— **outside** the Basic-auth middleware (the JWT is the credential). Reuses the
M18 rate-limiter on validation failures, keyed per-IP via the same
rightmost-XFF logic (honoring `--trust-proxy-headers`).

### 2.5 Token-expiry sweep

A bounded periodic sweep
`DELETE FROM tokens WHERE expires_at IS NOT NULL AND expires_at < now AND user_id = '_oidc'`
on the one-goroutine-per-`serve` worker pattern M15 webhooks established
(ticks on a multi-minute interval; emits `oidc_tokens_swept_total`). Scoped to
`_oidc` so it never touches operator-managed user tokens.

## 3. Exchange flow (`POST /_oidc/token`)

Request — `application/x-www-form-urlencoded`, RFC 8693:

```
grant_type=urn:ietf:params:oauth:grant-type:token-exchange
subject_token=<IdP id_token JWT>
subject_token_type=urn:ietf:params:oauth:token-type:jwt
scope=repo:write lfs:write        # OPTIONAL down-scope request
resource=myorg/app                # OPTIONAL repo hint for rule disambiguation
```

Server steps (fail-closed at every gate):

1. **Parse form.** Non-token-exchange `grant_type` → `400 unsupported_grant_type`.
   Missing `subject_token` or non-JWT `subject_token_type` → `400 invalid_request`.
2. **Unverified claims peek** to read `iss`; look up `oidc_issuers` by
   `issuer_url`. Unknown issuer → `400 invalid_request` (uniform; **no JWKS
   fetch** for unregistered issuers — anti-amplification).
3. **Verify** via `internal/oidc.Verify` (alg allowlist, JWKS, `iss`/`exp`/`nbf`/
   `iat`/skew). Failure → `401 invalid_token`; increments rate-limiter.
4. **Match a rule.** Load issuer's rules (optionally narrowed by `resource`),
   first-match-wins by `(tenant, repo, id)`: `audience == aud` AND all claim
   constraints equal. No match → `403 access_denied` (uniform).
5. **Effective scopes** = `rule.scopes`; if request sent `scope=`, intersect
   (down-scope only — never widen). Empty intersection → `400 invalid_scope`.
6. **Mint** via `Store.CreateToken` (see §2.3).
7. **Respond** `200`, JSON per RFC 8693:
   ```json
   {
     "access_token": "bvts_...",
     "issued_token_type": "urn:ietf:params:oauth:token-type:access-token",
     "token_type": "Bearer",
     "expires_in": 900,
     "scope": "repo:write lfs:write"
   }
   ```
8. **Audit** `auth.oidc.exchanged` on success; `auth.oidc.rejected` on any gate
   failure. The raw `subject_token` is never logged.

Client usage afterward: the `access_token` is a plain `bvts` token used as the
Basic password (`git clone https://x-access-token:$TOKEN@host/myorg/app`) or via
any Git credential helper. LFS, scope checks, expiry, revocation, rate-limiting
already understand it — nothing downstream is OIDC-aware.

### 3.1 Key integration point

Returning a non-nil `auth.Scope` on the **BasicPassword** path is new behavior
(today only deploy-key SSH credentials do it). The gateway authorization
middleware must, for an OIDC token:

- assert `Scope.Tenant`/`Scope.Repo` match the requested repo → cross-repo use
  rejected `403`;
- use `Scope.Perm` directly (skip `LookupRepoPerm`);
- **and still** run M17 `CheckScope` on the bitmask.

This interaction is called out explicitly so the plan tests it both ways
(correct repo succeeds; wrong repo 403).

## 4. Security model

### 4.1 Signature & algorithm
- `alg:none` rejected; asymmetric-only allowlist (`RS*`/`ES*`). Closes
  JWT-bypass and RS256↔HS256 confusion (public key fed as HMAC secret) — the
  allowlist makes feeding a public key into an HMAC verifier unrepresentable.
- JWKS fetched HTTPS-only, only from the issuer's own discovery `jwks_uri`, only
  for registered issuers.

### 4.2 Claim validation
- `iss` exact-match against a registered issuer (no prefix/substring).
- `aud` **mandatory and exact** against the rule's audience — primary
  confused-deputy control; a token minted for another service cannot be
  replayed here. Empty `audience` rejected at rule creation.
- `exp`/`nbf`/`iat` enforced, bounded ±60s skew. Missing `exp` rejected.
- `jti` replay cache deferred (documented).

### 4.3 Grant confinement
- Minted token bound to exactly one `(tenant, repo)` via `auth.Scope`; other
  repos → `403`; no `LookupRepoPerm` widening.
- Down-scope only: `scope=` request can shrink, never exceed `rule.scopes`.
- Short TTL (default 15 min, hard ceiling ≤ 1 h enforced at rule creation)
  bounds leaked-token blast radius; revocable early via existing token-revoke.

### 4.4 Abuse / DoS
- M18 rate-limiter per-IP on validation failures caps brute-force and
  JWKS-amplification; rightmost-XFF behind `--trust-proxy-headers`.
- Discovery/JWKS caching + min-refresh interval; unknown-`kid` flood cannot
  amplify into IdP requests.
- Unregistered issuer → zero network egress (rejected before any fetch).

### 4.5 Information disclosure
- Uniform error vocabulary (`invalid_request` / `invalid_token` /
  `access_denied` / `invalid_scope`); failures reveal nothing about whether an
  issuer/rule/repo exists.
- Raw `subject_token` never logged; audit records issuer alias + `sub` +
  decision only.

### 4.6 Trust boundary
Registering an issuer + rule is an **admin** action (admin actor / `repo:admin`).
The operator explicitly trusts that IdP to assert the configured claims
truthfully. The realistic foot-gun — a misconfigured `aud` or an over-broad
rule with no claim constraints — is documented prominently in the operator
guide.

## 5. Observability

### 5.1 Metrics (OTel)
- `oidc_exchange_total{result}` — `minted | invalid_token | no_rule | bad_request | invalid_scope`
- `oidc_exchange_duration_seconds` — includes JWKS-fetch tail
- `oidc_jwks_fetch_total{issuer_alias, result}` — `ok | error`
- `oidc_tokens_swept_total`
- No `sub`/claim-valued labels (unbounded cardinality under probing).

### 5.2 Audit events (`auth.*`)
- `auth.oidc.exchanged` — issuer alias, sub, tenant/repo, granted scopes, ttl,
  minted token id (correlates with later push/LFS audits via token id).
- `auth.oidc.rejected` — reason enum, issuer (alias if registered else
  `unknown`), client IP. Never the raw JWT.
- `auth.oidc.issuer_added` / `auth.oidc.issuer_removed`
- `auth.oidc.rule_added` / `auth.oidc.rule_removed`

## 6. Error handling

- HTTP body: small JSON `{"error": "...", "error_description": "..."}` per
  RFC 6749/8693, `error_description` generic (no enumeration leak).
- DB/JWKS internal failures → `500` + `oidc.internal_error` audit; **not**
  counted against the rate-limiter (client not punished for our outage).
- Discovery/JWKS unreachable at exchange time → `503` (retryable), distinct
  from `401`.

## 7. Operator CLI (`bucketvcs oidc …`)

NDJSON output, mirrors M14/M16 policy CLI shape. Admin-gated.

```
oidc issuer add   --alias=github --url=https://token.actions.githubusercontent.com
oidc issuer list
oidc issuer remove --alias=github

oidc rule add     --issuer=github --audience=https://bucketvcs.example \
                  --tenant=myorg --repo=app --scopes=repo:write,lfs:write \
                  --ttl=15m --claim=repository=myorg/app --claim=ref=refs/heads/main
oidc rule list    [--issuer=… | --repo=…]
oidc rule remove  --id=bvor_…
```

- `--claim name=value` repeatable; both halves required.
- `--scopes` reuses the M17 csv / `all` / `repo:*` / `lfs:*` parser.
- `--ttl` Go duration; validated `> 0` and `<= ceiling` at insert.
- empty `--audience` rejected.
- `oidc rule list` flags zero-claim (wildcard) rules visibly.

## 8. Testing

### 8.1 `internal/oidc` unit
- valid RSA + EC token round-trip (self-signed test keys);
- `alg:none` rejected; HS256-with-public-key-as-secret rejected;
- expired / `nbf`-future / missing-`exp` / wrong-`aud`(caller) / wrong-`iss` rejected;
- JWKS rotation: new `kid` triggers exactly one refresh; unknown `kid` rate-limited.

### 8.2 Store
- rule matching: all-claims-equal match; missing constraint → no match; extra
  token claim ignored; zero-claim wildcard matches;
- first-match-wins ordering;
- FK cascade on `repos` / `oidc_issuers` delete;
- migration `0010` round-trip + `tokens.scope_*` read/write.

### 8.3 Gateway
- happy-path exchange → mint → JSON shape;
- cross-repo use of a minted token → `403`;
- down-scope intersection (request narrower than rule);
- rate-limiter increments on bad token; unregistered issuer → **zero JWKS
  fetch** (fake keyset asserts no calls);
- `503` on JWKS-unreachable.

### 8.4 CLI
- issuer/rule add/list/remove; bad `--ttl`/`--audience`/`--scopes`/`--claim`
  rejected; NDJSON shape.

### 8.5 Smoke (localfs, end-to-end)
1. register issuer + rule;
2. sign a JWT locally with a test key matching the issuer JWKS (served by a
   local stub);
3. exchange → receive `bvts` token;
4. `git push` with it → success;
5. push to a different repo with the same token → fail (403);
6. expired token → fail.

## 9. Acceptance criteria

- A locally-signed JWT matching a registered issuer + rule exchanges for a
  `bvts` token that pushes successfully to exactly the bound repo and no other.
- `alg:none`, wrong `aud`, wrong `iss`, expired, and no-matching-rule are all
  rejected with uniform, non-enumerating errors.
- Unregistered issuer causes zero outbound network requests.
- Minted tokens expire at `now + ttl` and are reaped by the sweep.
- All existing auth/LFS/scope tests still pass (no downstream OIDC awareness).
- `internal/oidc` adds no new go.mod module (uses vendored `go-jose/v4`).

## 10. Open questions

- **Sweep cadence**: fixed default (e.g. 5 min) vs `--oidc-sweep-interval` flag.
  Lean: fixed default, add flag only if operators ask.
- **TTL ceiling value**: 1 h proposed; confirm during implementation.
- **`resource` semantics**: pure disambiguation hint (current design) vs
  required when multiple rules match. Lean: hint only; first-match-wins already
  deterministic.
