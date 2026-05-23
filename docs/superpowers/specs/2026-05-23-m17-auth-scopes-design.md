# M17: Auth — token scopes + rotation

**Status:** Design.
**Date:** 2026-05-23.
**Scope:** Add fine-grained scopes to HTTPS Basic tokens (spec §30.1 SHOULD #2 + recommended token scopes list). Add atomic token rotation. Close the gap where every token inherits the full user permission set.

## 1. Goals

### 1.1 In scope

- Bitmask-based token scopes covering all 7 spec scopes: `repo:read`, `repo:write`, `repo:admin`, `lfs:read`, `lfs:write`, `webhook:admin`, `storage:admin`
- Implicit hierarchy: `repo:admin` ⊇ `repo:write` ⊇ `repo:read`; `lfs:write` ⊇ `lfs:read`; admin scopes stand alone
- Migration 0008 adds `scopes INTEGER NOT NULL DEFAULT 0` to the existing `tokens` table on the M4 authdb
- Backward-compat: legacy tokens with `scopes=0` bypass the scope check; new tokens with any bit set get full enforcement
- `bucketvcs token create --scopes=<csv|all|repo:*|lfs:*>` CLI flag
- `bucketvcs token rotate --id=<token-id>` atomic rotation (new secret, same id, same scopes/expiry)
- `bucketvcs token list` extended with a `scopes` column
- Scope enforcement at: upload-pack (clone/fetch), receive-pack (push), LFS download/verify, LFS locks
- New audit events `auth.token.rotated` + `auth.scope.denied`; new `auth_check_total{outcome=insufficient_scope}` metric outcome (or full new metric if M4 has none)
- Subject struct extended with `Scopes auth.Scope` field; populated on HTTPS Basic path, left zero on SSH (which exempts itself from token-scope checks)

### 1.2 Out of scope (deferred)

- **OIDC token exchange** (spec §30.1.5) — own milestone (IdP discovery, JWT validation, RFC 8693)
- **Machine tokens** (per-org service accounts) — doubles scope; defer to M17.1 or follow-on
- **Rate-limiting on auth failures** (spec §30.5 MUST) — cross-cuts every auth path, own milestone
- **SAML / UI auth** (spec §30.4 enterprise) — no UI exists
- **SCIM provisioning** — no UI
- **Ref-pattern scoped permissions** — M14 protected_refs addresses the common case
- **IP allowlists** (spec §30.4 enterprise) — operator config surface, separate concern
- **SSH key scopes** — SSH path doesn't carry a token; would need a parallel `ssh_keys.scopes` column
- **Custom/extensible scopes** — bitmask locks the set at 7; defer until operators ask
- **`token scopes update <id> <new-scopes>`** CLI — current model is revoke + create; add if operator pain surfaces
- **HTTP Bearer tokens** (spec §30.1.6 MAY) — Basic with token-as-password is the spec MUST that's already shipped; Bearer is operator-niche

## 2. Architecture overview

```
internal/auth/
  scopes.go       (new) — Scope bitmask + ParseScopes/FormatScopes/EffectiveScopes/ValidateScopes/Has
  scopes_test.go  (new)
  errors.go             — add ErrInsufficientScope sentinel
  store.go              — interface signatures updated (Token.Scopes, Subject.Scopes); no behavior here

internal/auth/sqlitestore/
  migrations/0008_token_scopes.sql  (new) — ALTER TABLE tokens ADD COLUMN scopes INTEGER NOT NULL DEFAULT 0
  store.go                                — Token.Scopes field; CreateToken signature gains scopes;
                                           RotateToken method (new); list/get plumb Scopes

internal/gateway/
  ...                                    — upload-pack + receive-pack handlers check Subject.Scopes
                                           after existing user-perm check; skip when ScopeLegacy

internal/lfs/
  handler.go, proxied.go                 — verify ScopeLFSRead / ScopeLFSWrite from Subject.Scopes
  locks_handler.go                       — verify ScopeLFSWrite for create + release

internal/sshd/...                        — no changes; SSH key path leaves Subject.Scopes at zero
                                           (which equals ScopeLegacy and skips the check)

cmd/bucketvcs/token.go                   — --scopes flag on `create`; new `rotate` subcommand;
                                           list shows scopes column

internal/auth/audit.go (or M4 equivalent) — EmitTokenRotated + EmitScopeDenied audit emitters
```

**Authorization decision (HTTPS Basic with token):**
```
required := operationToRequiredScope(op)            // e.g., push → ScopeRepoWrite
if subject.Scopes == auth.ScopeLegacy {
    // Pre-M17 token: skip scope check, fall through to existing user-perm check.
} else {
    effective := auth.EffectiveScopes(subject.Scopes)
    if !effective.Has(required) {
        emit auth.scope.denied audit
        return ErrInsufficientScope
    }
}
// Then check user's repo_permissions as today.
```

**Authorization decision (SSH key):** no token; `subject.Scopes` is the zero value (`ScopeLegacy`); scope check skipped. Operators wanting to scope SSH access use deploy keys + M14 protected refs.

**Optionality / backward-compat:** existing tokens get `scopes=0` from the migration's DEFAULT; `scopes=0` is the legacy sentinel that bypasses scope checks. No deployment breakage on rollout. New tokens default to `--scopes=` omitted → scopes=0 → legacy (with a stderr warning suggesting operators set explicit scopes).

## 3. Schema (migration 0008_token_scopes.sql)

```sql
ALTER TABLE tokens ADD COLUMN scopes INTEGER NOT NULL DEFAULT 0;

INSERT INTO schema_version (version, applied_at) VALUES (8, strftime('%s','now'));
```

**Notes:**
- Single ALTER. `DEFAULT 0` backs-fills every existing token with `scopes=0` (the legacy sentinel).
- No `CHECK` constraint on the bitmask — invalid bits rejected at the Service layer via `ValidateScopes`.
- No index — token lookup keys are `id` and `user_id`; scopes is read alongside the row.

## 4. Scope taxonomy (`internal/auth/scopes.go`)

```go
package auth

type Scope uint64

const (
    ScopeRepoRead     Scope = 1 << 0  // "repo:read"
    ScopeRepoWrite    Scope = 1 << 1  // "repo:write"
    ScopeRepoAdmin    Scope = 1 << 2  // "repo:admin"
    ScopeLFSRead      Scope = 1 << 3  // "lfs:read"
    ScopeLFSWrite     Scope = 1 << 4  // "lfs:write"
    ScopeWebhookAdmin Scope = 1 << 5  // "webhook:admin"
    ScopeStorageAdmin Scope = 1 << 6  // "storage:admin"
)

const ScopeMaskAll Scope = ScopeRepoRead | ScopeRepoWrite | ScopeRepoAdmin |
    ScopeLFSRead | ScopeLFSWrite | ScopeWebhookAdmin | ScopeStorageAdmin

// ScopeLegacy is the zero value sentinel — pre-M17 tokens and SSH key
// subjects have Scope = ScopeLegacy and bypass the scope check.
const ScopeLegacy Scope = 0
```

### 4.1 API

```go
// ParseScopes accepts:
//   - "all"     → ScopeMaskAll
//   - "repo:*"  → ScopeRepoRead | ScopeRepoWrite | ScopeRepoAdmin
//   - "lfs:*"   → ScopeLFSRead | ScopeLFSWrite
//   - comma-separated canonical names, whitespace-tolerant
//
// Empty string and unknown names return an error. Returns ScopeLegacy (0)
// only when the input is "legacy" explicitly (operator-visible label
// for the backward-compat sentinel; not generally used in --scopes).
func ParseScopes(s string) (Scope, error)

// FormatScopes returns:
//   - "legacy"  for ScopeLegacy (0)
//   - "all"     when mask == ScopeMaskAll
//   - csv       otherwise, in canonical order
func FormatScopes(s Scope) string

// EffectiveScopes applies the implicit hierarchy:
//   ScopeRepoAdmin → adds ScopeRepoWrite + ScopeRepoRead
//   ScopeRepoWrite → adds ScopeRepoRead
//   ScopeLFSWrite  → adds ScopeLFSRead
// Idempotent: EffectiveScopes(EffectiveScopes(x)) == EffectiveScopes(x).
func EffectiveScopes(s Scope) Scope

// ValidateScopes returns ErrInvalidInput if any bit outside ScopeMaskAll
// is set. Called at CreateToken/CLI parse time.
func ValidateScopes(s Scope) error

// Has reports whether mask s includes at least one bit from required.
//   subject.Scopes.Has(ScopeRepoWrite) → true if any of {ScopeRepoWrite,
//     ScopeRepoAdmin} is set after applying EffectiveScopes
func (s Scope) Has(required Scope) bool { return s & required != 0 }
```

### 4.2 Defense-in-depth test

Mirroring M15's `TestEventMaskAll_CountsAllEvents`, add `TestScopeMaskAll_CountsAllScopes` asserting `bits.OnesCount64(uint64(ScopeMaskAll)) == 7`. Catches drift if a future constant is added without updating the mask.

## 5. Operation → required scope

| Git / LFS operation | Required (any of, after EffectiveScopes) |
|---|---|
| ls-remote / fetch / clone (upload-pack) | `ScopeRepoRead` |
| push (receive-pack) | `ScopeRepoWrite` |
| LFS object download (`GET /:obj`) | `ScopeLFSRead` |
| LFS object upload + verify (`POST /verify`) | `ScopeLFSWrite` |
| LFS locks create | `ScopeLFSWrite` |
| LFS locks release | `ScopeLFSWrite` |
| Webhook endpoint CRUD (deferred remote API) | `ScopeWebhookAdmin` |
| Storage admin (deferred remote API) | `ScopeStorageAdmin` |

After `EffectiveScopes` is applied, `repo:admin` covers everything in `repo:*`, and `lfs:write` covers `lfs:read`. A token with `repo:admin,lfs:write` can do every Git + LFS operation.

## 6. Service API extensions

### 6.1 Token struct + CreateToken

```go
// internal/auth/sqlitestore/store.go
type Token struct {
    ID         string
    UserID     string
    SecretHash string
    Label      string
    CreatedAt  int64
    ExpiresAt  *int64
    LastUsedAt *int64
    RevokedAt  *int64
    Scopes     auth.Scope  // M17
}

// CreateToken signature extends with scopes.
func (s *Store) CreateToken(ctx context.Context, id, userID, secretHash, label string,
    expiresAt *int64, scopes auth.Scope) error
```

`GetTokenByID`, `ListTokensForUser`, and the auth resolver populate `Token.Scopes` from the new column.

### 6.2 RotateToken

```go
// RotateToken atomically replaces secret_hash. Old hash is overwritten;
// new plaintext secret is returned by the CLI caller (NOT this method).
// Does NOT change expires_at or scopes. Returns ErrNoSuchToken on missing id.
// Emits the auth.token.rotated audit event on success.
func (s *Store) RotateToken(ctx context.Context, id, newSecretHash string) error
```

CLI wraps this: generate new secret, hash it, call RotateToken, print plaintext secret once.

### 6.3 Subject extension

```go
// internal/auth/store.go
type Subject struct {
    User   string
    // ... existing fields ...
    Scopes Scope  // M17: populated by HTTPS Basic path; zero on SSH (= ScopeLegacy)
}
```

`RunAuth` (the HTTPS Basic resolver) sets `Subject.Scopes = token.Scopes` after the token is resolved. The SSH key path leaves it at the zero value.

### 6.4 Enforcement helper

```go
// internal/auth/scopes.go
// CheckScope returns ErrInsufficientScope when subject.Scopes is non-legacy
// AND its effective scopes don't satisfy required. Returns nil otherwise
// (including the legacy bypass).
func CheckScope(subject Subject, required Scope) error
```

Call sites:

```go
// internal/gateway upload-pack:
if err := auth.CheckScope(subject, auth.ScopeRepoRead); err != nil {
    auth.EmitScopeDenied(ctx, logger, subject, tenant, repo, "upload-pack", auth.ScopeRepoRead)
    return 403
}

// internal/gateway receive-pack:
if err := auth.CheckScope(subject, auth.ScopeRepoWrite); err != nil { ... }

// internal/lfs/handler download:
if err := auth.CheckScope(subject, auth.ScopeLFSRead); err != nil { ... }

// internal/lfs/proxied verify:
if err := auth.CheckScope(subject, auth.ScopeLFSWrite); err != nil { ... }

// internal/lfs/locks_handler create + release:
if err := auth.CheckScope(subject, auth.ScopeLFSWrite); err != nil { ... }
```

The existing per-operation `repo_permissions` check (read/write/admin per repo) is preserved AND runs alongside CheckScope; both must pass.

## 7. Operator CLI

```
bucketvcs token create <user> [--scopes=<csv|all|repo:*|lfs:*>]
                              [--expires=<duration>] [--label=<text>]
bucketvcs token list <user>                    # shows scopes column
bucketvcs token rotate --id=<token-id>         # new secret, same id
bucketvcs token revoke --id=<token-id>         # existing M4 behavior
```

### 7.1 `create` behavior
- `--scopes` omitted → `scopes=0` (legacy); stderr emits `"warning: no --scopes set; token has full user permissions"` so scripts get a nudge to migrate
- `--scopes=all` → ScopeMaskAll
- `--scopes=repo:read,lfs:read` → matching bits
- `--scopes=repo:*` → repo group shortcut
- `--scopes=` (empty string explicitly) → exit 2 (must omit the flag for legacy)
- Invalid scope name → exit 2 (ErrInvalidInput)
- Output unchanged from M4 except a new line: `scopes=<formatted>` after the existing `token=<plaintext>` line

### 7.2 `list` output
Add a `scopes` column. Text mode:
```
id            label    created                    expires    revoked  scopes
abc123        ci-prod  2026-05-23T10:00:00Z       —          —        repo:read,lfs:read
def456        old      2026-04-01T08:00:00Z       —          —        legacy
```
JSON / NDJSON mode (matches M15 / M16 convention if `token list --format=json` exists): include `"scopes": "<formatted>"`.

### 7.3 `rotate`
```
$ bucketvcs token rotate --auth-db=auth.db --id=abc123
token_id=abc123  rotated
token=<new-secret>   # store this now — it will not be shown again
```

Exit codes:
- 0 ok
- 1 operational (db unreachable)
- 2 ErrNoSuchToken (matches M15 webhook rotate-secret pattern)

`rotate` does NOT accept `--scopes` or `--expires` overrides — operators wanting to change scopes do revoke + create. This keeps rotate semantically focused on "compromised secret recovery" rather than entitlement changes.

## 8. Observability

### 8.1 New audit events

| Event | Trigger | Attrs |
|---|---|---|
| `auth.token.rotated` | `RotateToken` success | token_id, user_id, actor |
| `auth.scope.denied` | `CheckScope` fails at any enforcement site | user_id, token_id_prefix (8 chars), tenant, repo, operation, required_scope, granted_scopes |

If M4 doesn't already emit `auth.token.created` and `auth.token.revoked`, add them as part of this milestone — they're spec MUSTs (§30.5 audit requirements).

### 8.2 Metric extension

Existing `auth_check_total{outcome}` (if M4 has it) gains:
- `insufficient_scope` — scope check failed
- `token_rotated` — rotation succeeded

If M4 has no auth metric, this milestone adds one:
```
auth_check_total{outcome}
```
with outcomes `ok`, `expired`, `revoked`, `wrong_secret`, `no_such_token`, `insufficient_scope`, `token_rotated`.

## 9. Failure modes

| Failure | Behavior |
|---|---|
| `--scopes=bogus` | exit 2, `ErrInvalidInput` |
| `--scopes=` (empty string explicitly) | exit 2, "scopes must be non-empty if --scopes is specified; omit the flag for legacy" |
| Token has `repo:read`, client pushes | 403 with body `"insufficient scope: token lacks repo:write"`; audit `auth.scope.denied`; metric outcome `insufficient_scope` |
| Token has `scopes=0` (legacy) | scope check skipped; existing user-perm check still runs |
| `RotateToken` on non-existent id | exit 2, `ErrNoSuchToken` |
| Concurrent `RotateToken` + `RevokeToken` | sqlite serializes; one runs first. Outcomes safe in either order. |
| Migration 0008 on existing deployment | every existing token gets scopes=0 (legacy); auth behavior unchanged for those tokens |
| New token created with `--scopes=` AND `--expires=` both set | both apply (independent dimensions) |
| Token with scopes BUT user has no repo_permissions for repo | 403 from the existing user-perm check (no change in this path) |

## 10. Testing

### 10.1 Unit
- `ParseScopes` table-driven: all 7 canonical names, `all`, `repo:*`, `lfs:*`, whitespace tolerance, empty input, unknown names
- `FormatScopes`: `ScopeMaskAll` → `"all"`, `ScopeLegacy` → `"legacy"`, canonical order for partial sets, idempotent with ParseScopes (roundtrip)
- `EffectiveScopes`: ScopeRepoAdmin → admin|write|read; ScopeRepoWrite → write|read; ScopeLFSWrite → write|read; orthogonal scopes pass through; idempotent
- `ValidateScopes`: rejects bit positions outside ScopeMaskAll
- `CheckScope`: legacy bypass, sufficient/insufficient cases
- `TestScopeMaskAll_CountsAllScopes`: bits.OnesCount64 == 7 (defense-in-depth)

### 10.2 Store
- `CreateToken` with scopes; `GetTokenByID` returns Scopes; `ListTokensForUser` returns Scopes
- `RotateToken` happy + `ErrNoSuchToken`; verify secret_hash changed AND scopes preserved AND expires_at preserved
- Migration 0008 idempotency

### 10.3 Integration
- HTTPS Basic auth with a scoped token: clone with `repo:read` works, push fails 403 with body containing `insufficient scope`
- Clone with `repo:write` works (hierarchy: write implies read)
- LFS download with `lfs:read` works; upload fails; upload with `lfs:write` works
- SSH key auth: no scope check runs (SSH path exempt by design)

### 10.4 CLI
- `token create --scopes=repo:read,lfs:read` happy; output contains `scopes=repo:read,lfs:read`
- `token create` (no --scopes) emits warning + scopes=0
- `token create --scopes=bogus` exit 2
- `token list` shows scopes column; legacy tokens render as `legacy`
- `token rotate --id=<existing>` happy; output contains new secret
- `token rotate --id=<nonexistent>` exit 2

### 10.5 Smoke

New `scripts/m17-auth-scopes-smoke.sh` (NOT extension of an existing smoke — auth surface is foundational and worth its own end-to-end script):

1. `user create alice` + `repo register acme/site` + grant alice write
2. `token create alice --scopes=repo:read` → capture token
3. Configure local git remote with `https://alice:<token>@.../acme/site.git`
4. `git clone` succeeds
5. `git push` fails with 403 + `insufficient scope`
6. `token rotate --id=<token-id>` → capture new secret
7. Update remote URL with new secret
8. `git clone` still works (rotate preserves scopes)
9. `token create alice --scopes=repo:write` → capture full-write token
10. Push succeeds with the new token

End with `M17_AUTH_SCOPES_SMOKE_OK`.

## 11. Acceptance criteria

- All 7 scopes defined; ParseScopes/FormatScopes roundtrip cleanly
- Hierarchy correctly applied at every enforcement site (clone with `repo:write` works; LFS download with `lfs:write` works)
- Pre-M17 tokens (scopes=0) keep working without operator action
- `bucketvcs token create --scopes=...` documented in --help; `--scopes` omitted emits the migration warning
- `bucketvcs token list` shows scopes column
- `bucketvcs token rotate --id=...` produces a new secret without touching scopes/expires
- New audit events `auth.token.rotated` + `auth.scope.denied` emit at documented sites
- `scripts/m17-auth-scopes-smoke.sh` passes
- All prior smokes (M11/M12/M12.1/M13/M13.3/M13.4/M13.5/M14/M15/M15.1/M16) still pass
- `go test ./...` clean; `go vet ./...` clean

## 12. Open questions

None — all decisions captured above.
