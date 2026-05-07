# M4 — HTTPS Token Authentication

Date: 2026-05-06
Status: design draft
Scope: bucketvcs OSS milestone M4
Source spec: `docs/original-spec.md` §30 (auth), §35 (OSS scope)
Decomposition: `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md`
Predecessor: M3 (HTTP smart-Git gateway, merged at `m3-complete`)

## 1. Purpose

M4 replaces M3's placeholder shared-bearer token with real per-actor HTTPS token authentication. It makes bucketvcs safe to expose to a second user, lets clients authenticate via standard `git credential` helpers using HTTP Basic (token-as-password), and establishes the transport-neutral authorization seam that M5 (cloud backend) and M6 (SSH) will plug into.

After M4, a fresh `bucketvcs serve` refuses every request until an admin user is added via CLI. From there, `git clone https://user:token@host/tenant/repo.git` works against private repos for users with `read` on that repo, and `git push` works for users with `write`. Anonymous clone/fetch is allowed only for repos explicitly flagged `public_read`. Push always requires authentication.

## 2. What changes versus M3

Before M4 the gateway accepts one of three modes selected by `--auth-mode`:

```
AuthAnonymous       no auth required, any request succeeds
AuthWriteOnly       reads anonymous, writes require shared bearer
AuthAll             all requests require shared bearer
```

After M4:

```
every request resolves to (Actor, Perm, RequiredAction, RepoFlags)
auth.Decide is the single allow/deny function
the shared-bearer modes are removed; --auth-mode and --auth-token flags are gone
```

The gateway gains a new dependency on `internal/auth` and on a SQLite-backed `auth.Store`. The push-serialization path, mirror manager, and all M3 protocol handlers are unchanged in behavior; they simply observe an `Actor` in the request context.

## 3. Non-goals

This milestone explicitly does not deliver:

- SSH public-key authentication (M6).
- Cloud-backend storage adapters (M5).
- LFS authentication or scopes (M13).
- Protected branches, required signed commits, force-push controls, fine-grained per-token scopes (M14 hooks/policy).
- Audit event tables and the §31 structured event stream (M15 webhooks/audit). M4 emits structured logs only.
- Failed-auth rate limiting and IP throttling (deferred hardening pass; real protection wants shared state).
- Token rotation UX. Operators rotate by `revoke` + `create`.
- Account passwords for Git-over-HTTPS. Per spec §30.1, tokens only.
- Bootstrap-over-HTTP or first-run admin token printed at server start. Bootstrap trust root is filesystem access on the gateway host.

## 4. Architecture

### 4.1 Package layout

```
internal/auth/
  doc.go             package overview + invariants
  types.go           Actor, Credential interface (BasicPassword, SSHKeyFingerprint),
                     Action, Perm, RepoFlags
  store.go           Store interface (transport-neutral)
  permissions.go     Decide(actor, perm, action, flags) (ok bool, reason string)
  tokens.go          token format, GenerateToken, HashSecret, VerifyHash
  errors.go          ErrInvalidCredential, ErrUnauthorized,
                     ErrTokenExpired, ErrTokenRevoked, ErrUserDisabled

internal/auth/sqlitestore/
  store.go           SQLite implementation of auth.Store
  schema.go          embedded migrations
  migrations/0001_init.sql
  store_test.go

internal/auth/conformance/
  conformance.go     portable test suite for any future auth.Store implementation

internal/gateway/
  authmw.go          NEW: HTTP auth middleware
  routes.go          NEW: URL -> (tenant, repo, op, requiredAction) parser
  server.go          MODIFIED: Options gains AuthStore; remove AuthMode/AuthToken
  ...                existing M3 handlers read Actor from ctx; no auth logic of their own

cmd/bucketvcs/
  user.go            NEW: bucketvcs user {add,list,disable,enable,delete}
  token.go           NEW: bucketvcs token {create,list,revoke}
  repo.go            NEW: bucketvcs repo {register,grant,revoke,public,list}
  serve.go           MODIFIED: --auth-db <path>, removes --auth-mode and --auth-token
```

### 4.2 Invariants

1. `internal/auth` has no HTTP, no SSH, no SQL imports. It defines types and pure logic only. Storage and transport live at the edges.
2. `Store` is the only seam with persistent state. Tests inject an in-memory implementation.
3. `Decide(actor, perm, action, flags)` is pure and table-driven. Public-read logic lives there, not scattered across middleware.
4. The gateway never makes authorization decisions inline — it always calls `Decide`. Auditable in one place; reusable from SSH (M6) without change.
5. Admin CLI subcommands open the SQLite file directly. They do not require a running gateway. There is no admin-over-HTTP API in M4.
6. Auth state lives outside the bucket root. SQLite cannot run over remote object storage; co-locating with bucket data would block M5.

### 4.3 Transport-neutral interfaces

```go
type Actor struct {
    UserID  string
    Name    string
    IsAdmin bool
}

type Credential interface{ isCredential() }
type BasicPassword     struct{ Username, Password string }
type SSHKeyFingerprint struct{ Fingerprint string } // populated by M6

type Action int
const (
    ActionRead Action = iota
    ActionWrite
)

type Perm int
const (
    PermNone Perm = iota
    PermRead
    PermWrite
    PermAdmin
)

type RepoFlags struct {
    PublicRead bool
}

type Store interface {
    VerifyCredential(ctx context.Context, c Credential) (*Actor, string, error)
        // returns (actor, tokenID, nil) on success; tokenID is empty for SSH
        // ErrInvalidCredential, ErrTokenExpired, ErrTokenRevoked, ErrUserDisabled

    LookupRepoPerm(ctx context.Context, actor *Actor, tenant, repo string) (Perm, error)
        // PermNone for anonymous or no grant; PermAdmin returned for is_admin actors

    GetRepoFlags(ctx context.Context, tenant, repo string) (RepoFlags, error)
        // returns ErrNoSuchRepo if the (tenant, repo) is not registered

    TouchTokenUsage(ctx context.Context, tokenID string) error
        // best-effort; called from a fire-and-forget goroutine
}
```

`internal/auth` does not depend on any package in `internal/gateway`. The reverse dependency is the only direction.

## 5. Data model

### 5.1 SQLite schema (migration `0001_init.sql`)

```sql
CREATE TABLE schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE users (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    is_admin    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    disabled_at INTEGER
);

CREATE TABLE tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    secret_hash  TEXT NOT NULL,
    label        TEXT,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER,
    last_used_at INTEGER,
    revoked_at   INTEGER
);
CREATE INDEX tokens_user_idx ON tokens(user_id);

CREATE TABLE repos (
    tenant      TEXT NOT NULL,
    name        TEXT NOT NULL,
    public_read INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (tenant, name)
);

CREATE TABLE repo_permissions (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tenant     TEXT NOT NULL,
    repo       TEXT NOT NULL,
    perm       TEXT NOT NULL CHECK (perm IN ('read','write','admin')),
    granted_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, tenant, repo),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);
```

Notes:

- `repos` is a registry, not a copy of bucket truth. The CLI keeps it in sync with M1 manifest state by wrapping `bucketvcs init`.
- All timestamps are unix-seconds INTEGER for portability and arithmetic.
- No `audit_events` table — M15 owns audit.
- No `roles` table — `users.is_admin` is sufficient for M4. Org/team roles are hosted-product concepts.
- `users.disabled_at` covers "stop letting them in without deleting their history."
- Foreign keys are enabled (`PRAGMA foreign_keys = ON`) at connection time.
- WAL journaling is enabled (`PRAGMA journal_mode = WAL`) at connection time.

### 5.2 Token format

```
bvts_<id>_<secret>

  bvts        literal prefix; identifies bucketvcs token, scannable in code/log search
  _           separator
  id          24 base32 chars (Crockford alphabet, no padding) = ~120 bits
  _           separator
  secret      52 base32 chars (Crockford alphabet, no padding) = ~256 bits
```

- **Lookup:** parse, fetch `tokens` row by `id` (PK), constant-time compare argon2id of `secret` against `secret_hash`.
- **Hashing:** argon2id with parameters `m=64 MiB, t=3, p=4`, encoded as PHC string (`$argon2id$v=19$...`). Parameters are self-describing for future migration.
- **One-shot display:** `bucketvcs token create` prints the full token to stdout exactly once. The DB never stores the secret in clear; the CLI never re-displays.
- **Why a separate id:** verification is one indexed row fetch plus one argon2id verify. Hash-and-scan would be acceptable at OSS scale but trivially worse and gets uglier as ops add tokens.

### 5.3 Decision table

`Decide(actor, perm, action, flags)`:

| actor          | perm        | action     | public_read | result |
|----------------|-------------|------------|-------------|--------|
| nil (anon)     | n/a         | read       | true        | allow  |
| nil (anon)     | n/a         | read       | false       | deny   |
| nil (anon)     | n/a         | write      | *           | deny   |
| user           | none        | read       | true        | allow  |
| user           | none        | read       | false       | deny   |
| user           | none        | write      | *           | deny   |
| user           | read        | read       | *           | allow  |
| user           | read        | write      | *           | deny   |
| user           | write       | read/write | *           | allow  |
| user           | admin       | read/write | *           | allow  |
| user (is_admin)| n/a         | *          | *           | allow  |

`is_admin` short-circuits before per-repo lookup. Admin actors can clone/push every repo without explicit grant.

## 6. Request flow

### 6.1 URL parser

`internal/gateway/routes.go` is the single source of truth for URL semantics:

```go
type Op int
const (
    OpInfoRefsUpload Op = iota
    OpInfoRefsReceive
    OpUploadPack
    OpReceivePack
)

type RoutedRequest struct {
    Tenant         string
    Repo           string  // no trailing ".git"
    Op             Op
    RequiredAction auth.Action
}

func ParseRoute(method, path, query string) (*RoutedRequest, error)
```

Mapping:

| method | path                                | query                       | Op                  | RequiredAction |
|--------|-------------------------------------|-----------------------------|---------------------|----------------|
| GET    | `/{t}/{r}.git/info/refs`            | `service=git-upload-pack`   | InfoRefsUpload      | Read           |
| GET    | `/{t}/{r}.git/info/refs`            | `service=git-receive-pack`  | InfoRefsReceive     | Write          |
| POST   | `/{t}/{r}.git/git-upload-pack`      | (any)                       | UploadPack          | Read           |
| POST   | `/{t}/{r}.git/git-receive-pack`     | (any)                       | ReceivePack         | Write          |

Anything that does not match returns 404. Unknown URLs do not produce auth challenges and do not differentiate "no such path" from "not allowed to know."

### 6.2 Middleware sequence

```
1. ParseRoute(req)
   on error -> 404; return.

2. Store.GetRepoFlags(tenant, repo)
   ErrNoSuchRepo -> 404; return.   # never differentiate from "no permission"

3. Extract HTTP Basic Authorization header
   - present, malformed (bad base64, missing colon) -> 401 + WWW-Authenticate
   - present, well-formed -> Store.VerifyCredential(BasicPassword{...})
       ErrInvalidCredential -> 401 + WWW-Authenticate
       ErrTokenExpired      -> 401 + WWW-Authenticate
       ErrTokenRevoked      -> 401 + WWW-Authenticate
       ErrUserDisabled      -> 401 + WWW-Authenticate
       success              -> actor populated; spawn TouchTokenUsage goroutine
   - absent -> actor = nil (anonymous)

4. Store.LookupRepoPerm(actor, tenant, repo)
   - actor == nil -> PermNone (without DB call)
   - else        -> PermRead | PermWrite | PermAdmin | PermNone

5. auth.Decide(actor, perm, requiredAction, flags)
   - allow -> attach actor to ctx; dispatch to existing M3 handler
   - deny  -> 401 if actor == nil   (challenge for credentials)
              403 if actor != nil   (authenticated but unauthorized)
```

Step 4 must skip the DB lookup when `actor == nil` — anonymous users need no permission lookup since `Decide` handles them via `flags.public_read` alone.

### 6.3 HTTP responses

```
401 Unauthorized
  WWW-Authenticate: Basic realm="bucketvcs"
  Content-Type: text/plain; charset=utf-8
  Body: short reason text

403 Forbidden
  Content-Type: text/plain; charset=utf-8
  Body: "insufficient permissions"

404 Not Found
  Content-Type: text/plain; charset=utf-8
  Body: "not found"
```

Git's HTTP client treats 401 as "ask the credential helper, retry." It treats 403 as "stop." This is the desired behavior in both cases.

### 6.4 Concurrency

- `TouchTokenUsage` runs in a fire-and-forget goroutine after a successful authenticated request (skipped for anonymous), with a 200 ms-deadline context. A single-row UPDATE; failures are logged and not surfaced.
- All login lookups are read-only; SQLite WAL mode allows concurrent readers and a single writer without contending on the request hot path.
- The middleware never holds a mutex across any storage call.

### 6.5 What does NOT change in M3 handlers

- `info/refs`, `git-upload-pack`, and `git-receive-pack` handlers do not import `internal/auth`. They read the actor from context only for logging.
- The push-serialization path (`mirror.Manager`, `BuildAndCommit`) is untouched.
- M3's per-repo on-disk mirror behavior, sentinel format, and capability advertisement are unchanged.
- Differential harness fixtures are unchanged; new auth-scenario fixtures are added separately.

## 7. CLI surface

### 7.1 New subcommands

```
bucketvcs user add <name> [--admin]
    Insert into users table. Refuses on existing name (exit 2).

bucketvcs user list [--json]
    name | admin | disabled | created | token-count

bucketvcs user disable <name>
bucketvcs user enable <name>
    Set/clear users.disabled_at. Disabled users fail auth; their tokens persist.

bucketvcs user delete <name>
    Hard delete; cascades tokens and repo_permissions.
    Refuses if it would remove the last admin.

bucketvcs token create <user> [--expires <duration>] [--label <text>]
    Generates token, inserts row, prints the full token to stdout EXACTLY ONCE.
    --expires accepts e.g. 90d, 24h. Omit for non-expiring.

bucketvcs token list <user> [--json]
    id | label | created | last-used | expires | revoked

bucketvcs token revoke <token-id>
    Sets revoked_at. Accepts the full id or any unique id prefix.

bucketvcs repo register <tenant>/<repo>
    Wraps `bucketvcs init` (M1) and inserts into repos table.
    Idempotent: if both exist, succeeds with no change.
    Repos created by an earlier direct `bucketvcs init` invocation
    (without registration) are unreachable through the gateway until
    `repo register` is run for them; a registration-only mode is
    available as `bucketvcs repo register --no-init <tenant>/<repo>`.

bucketvcs repo grant <user> <tenant>/<repo> <read|write|admin>
    Upserts repo_permissions; refuses if repo not registered.

bucketvcs repo revoke <user> <tenant>/<repo>
    Removes the row.

bucketvcs repo public <tenant>/<repo> <on|off>
    Toggles repos.public_read.

bucketvcs repo list [--tenant <name>] [--json]
    Lists registered repos with public flag.
```

### 7.2 `serve` command changes

```
bucketvcs serve
    --addr 127.0.0.1:8080         unchanged
    --bucket-root <path>          unchanged
    --auth-db <path>              NEW; default per §7.3
    --mirror-dir <path>           unchanged

  REMOVED in M4: --auth-mode, --auth-token
```

Starting `serve` with the removed flags fails fast with a message pointing to the migration doc.

### 7.3 Auth DB location

Default path resolution, in order:

1. `--auth-db <path>` (flag).
2. `$BUCKETVCS_AUTH_DB` (env).
3. `$XDG_STATE_HOME/bucketvcs/bucketvcs.db` if set.
4. `$HOME/.local/state/bucketvcs/bucketvcs.db`.

The DB is **not** placed inside the bucket root. Auth is operator state, not durable repo truth, and SQLite cannot run over remote object storage; co-locating would break M5 cloud backends.

`bucketvcs serve` and the admin CLI use the same resolution order, so a single configuration covers both.

### 7.4 Bootstrap

1. Operator runs `bucketvcs serve` for the first time. Empty `users`. Server refuses every request (401).
2. Operator (out of band, on the gateway host) runs:
   ```
   bucketvcs user add eran --admin
   bucketvcs token create eran --label "first admin"
   ```
3. CLI opens the SQLite file directly (WAL mode allows concurrent open with a running `serve`).
4. Token is printed to stdout. Operator copies it.

No first-run-over-HTTP, no startup-printed admin token, no `--insecure-bootstrap`. The threat model: anyone who can run admin CLI on the gateway host already has filesystem access to the SQLite file; the bootstrap trust root is filesystem access.

### 7.5 Migration from M3

- M3 deployments using `--auth-token` or `--auth-mode` fail to start on M4 with a message pointing to `docs/migration-m3-to-m4.md`.
- That document describes the four-step upgrade: install M4, run `bucketvcs user add`, run `bucketvcs token create`, update client `git credential` configuration.
- No automated migration tool. M3 was placeholder auth.

## 8. Logging

M4 emits structured logs at the auth boundary. M15 owns the durable audit table; M4 only logs.

Events logged:

```
auth.success      user, token_id (or empty), repo, action, source_ip, user_agent
auth.failure      reason (invalid|expired|revoked|disabled|malformed),
                  username (if known), source_ip, user_agent
authz.denied      user, repo, action, perm, public_read, source_ip
```

Logs use the existing M3 structured logger. No new dependency.

Note: `auth.failure` does not log password bytes. `authz.denied` records the perm seen so we can debug "why was this denied."

## 9. Testing

### 9.1 Unit tests

- `internal/auth/permissions_test.go` — table-driven tests for every row of the §5.3 decision table. Pure function; exhaustive coverage required.
- `internal/auth/tokens_test.go` — `GenerateToken` produces parseable tokens; `VerifyHash` rejects tampered secrets; format-mismatch rejected (wrong prefix, wrong segment count, wrong charset); constant-time compare benchmark.
- `internal/auth/sqlitestore/store_test.go` — every interface method against `:memory:` SQLite; cascade deletes; expired-token rejection; revoked-token rejection; concurrent reads; WAL durability.

### 9.2 Integration tests

- `internal/gateway/authmw_test.go` — every row of the decision table exercised through the middleware against a real handler stub. Asserts HTTP status, `WWW-Authenticate` header, and that 404 hides repo existence from unauthorized actors.
- `internal/gateway/routes_test.go` — every URL/method/query in the routing table → expected `Op` and `RequiredAction`. Negative cases: trailing slash variants, missing `.git`, unknown service param, percent-encoded names.

### 9.3 End-to-end against `git`

Extends the M3 e2e harness (`internal/gateway/e2e_auth_test.go`):

1. `git clone` no creds vs private repo → 401.
2. `git clone` no creds vs public repo → success.
3. `git clone` with valid creds → success.
4. `git clone` with revoked token → 401; helper retry also fails.
5. `git clone` with expired token → 401.
6. `git push` with read-only token → 403.
7. `git push` with write token → success.
8. `git push` no creds vs public repo → 401 challenge; with valid write creds → success.
9. Disabled user → 401.
10. Admin user → can clone/push every repo without explicit grant.

Tests configure `git` to use a script credential helper that returns the test token.

### 9.4 Differential harness extensions

- New oracle `clone-equivalence-with-auth`: clone via bucketvcs (with valid creds) vs reference `git http-backend` setup (with HTTP Basic); compare object closure. Confirms auth does not perturb the bytes returned.
- The existing 16 fixtures × 4 oracles are unchanged. Auth is orthogonal to packfile correctness; we do not multiply the matrix.
- One new fixture-level scenario set `auth_scenarios/` exercising the §9.3 cases against bucketvcs only. Vanilla `git http-backend` does not share our auth model, so there is no cross-oracle here.

### 9.5 Auth conformance suite

`internal/auth/conformance/conformance.go` is a portable test suite any future `Store` implementation must pass (e.g., a Postgres backend in the hosted product). M4 runs it against `sqlitestore`. Required tests:

```
1.  VerifyCredential rejects an unknown token id.
2.  VerifyCredential rejects a token whose secret does not hash to secret_hash.
3.  VerifyCredential rejects an expired token.
4.  VerifyCredential rejects a revoked token.
5.  VerifyCredential rejects a token belonging to a disabled user.
6.  LookupRepoPerm returns PermNone for a user with no grant.
7.  LookupRepoPerm returns the granted level for a user with a grant.
8.  LookupRepoPerm returns PermAdmin for is_admin actors regardless of grant.
9.  GetRepoFlags returns ErrNoSuchRepo for an unregistered repo.
10. GetRepoFlags returns the public_read flag for a registered repo.
11. TouchTokenUsage updates last_used_at and is idempotent on a missing id.
12. Cascade delete: deleting a user removes their tokens and repo_permissions.
13. Concurrent VerifyCredential calls produce consistent results under WAL.
```

### 9.6 Stress / smoke

- `+build stress`: 10,000 sequential auth verifications against a 1,000-token DB completes under 10s on a dev box. Catches argon2 parameter regressions and any accidental n² scan.
- 100 parallel `git ls-remote` against a single warm gateway with distinct tokens; no errors and no token-row update lost.

### 9.7 Review protocol

Per the M1+ review-protocol memory:

- Each PR gets superpowers `code-reviewer` + a per-task roborev pass.
- After implementation lands on the M4 branch, run `roborev-refine` on max reasoning until passing or diminishing returns.
- Auth is exactly the kind of code where "looks right but isn't" is dangerous. Bias toward more iterations.

## 10. Security considerations

- No plaintext token storage. Argon2id with PHC string encoding; parameters self-describing for future migration.
- Constant-time compare for hash verification; constant-time compare for the id-segment lookup is unnecessary because `id` is non-secret.
- Token id segment carries no user-identifying information. Leaks of an id alone do not compromise the user.
- HTTP Basic over TLS only. M4 does not include TLS termination — operators front the gateway with a reverse proxy (caddy/nginx) per the M3 invariant.
- Status-code choice trades a small enumeration leak for `git credential` usability:
  - URL does not parse as a Git request → 404.
  - Anonymous request to an unknown repo → 404.
  - Anonymous request to a known private repo → 401 with `WWW-Authenticate`. This is required so `git clone` invokes the credential helper and retries with credentials. The cost is that an anonymous probe can distinguish "private repo exists at this path" from "nothing exists at this path." For a self-hosted OSS Git server the trade-off is intentional.
  - Authenticated-but-unauthorized → 403, so the user can see "I am logged in but cannot access this." A tighter "hide existence from authenticated unauthorized" (404) is out of scope for OSS M4; revisit for hosted.
- Failed-auth events are logged. Rate limiting is deferred (see §3).
- Bootstrap trust root is filesystem access on the gateway host. There is no over-the-wire bootstrap.
- The spec §30.5 ban on plaintext token storage and reversible encryption is honored. SSH private keys are not stored in M4 (no SSH yet).

## 11. Open questions

1. Should `bucketvcs user add` accept an SSH public key inline as a convenience for M6 setup-ahead? The column does not exist until M6 lands. **Decision: defer; M6 adds the flag.**
2. Should public-read repos appear in any directory/listing endpoint? M4 has no listing endpoint and adds none. **Decision: defer; no listing endpoint in M4.**
3. Should the gateway bind to `127.0.0.1` by default and require explicit `0.0.0.0`? M3 already does this. **Decision: unchanged from M3.**

## 12. Acceptance criteria

M4 ships when:

1. All §9.1–§9.5 tests pass.
2. The §9.6 stress smoke meets the stated bounds.
3. `bucketvcs serve` started against an empty auth DB returns 401 to every request.
4. After CLI bootstrap, `git clone https://user:token@host/tenant/repo.git` succeeds for a user with `read` on a private repo and fails for a user without.
5. `git push` succeeds for a user with `write` and fails 403 for a user with `read`.
6. Anonymous `git clone` succeeds against a `public_read=1` repo and fails 401 against a private one.
7. M3 differential harness numbers (61 pass + 3 documented skips on clone-equivalence and push-equivalence) are unchanged.
8. `staticcheck`, `go vet`, and `gofmt` are clean across the M4 diff.
9. `roborev-refine` reports passing or diminishing returns.

## 13. Out of scope for this design spec

Per the brainstorming process, this spec is a design contract. Detailed choices deferred to the implementation plan:

- Exact CLI flag-parsing library and command-tree wiring.
- Exact SQLite migration tooling internals (we use embedded ordered SQL files; specifics of the runner are an implementation detail).
- Exact log line schemas.
- Exact `docs/migration-m3-to-m4.md` wording.
- Connection pool tuning for SQLite.
- Test fixture file naming.

## 14. References

- `docs/original-spec.md` §30 (auth), §35 (OSS scope), §41 (compatibility matrix), §42 (security)
- `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md`
- `docs/superpowers/specs/2026-05-06-m3-git-protocol-gateway-design.md`
- Memory: `m3_progress.md` (M3 ship state and invariants M4 must honor)
- Memory: `m1_review_protocol.md` (M1+ review protocol)
