# M16: Hooks and policy — Tier 2 path restrictions

**Status:** Design.
**Date:** 2026-05-22.
**Scope:** Extend M14 protected refs with per-(refname × path) policy rules that block updates whose new commits modify matching paths. Closes spec §23 line "path restrictions".

## 1. Goals

### 1.1 In scope

- Per-(tenant, repo, refname_pattern, path_pattern) protected-path rules backed by a new sqlite table (migration 0007) on the M4 authdb
- `bucketvcs policy paths add/list/remove` CLI mirroring the M14 `policy refs` shape
- Custom path-glob matcher supporting `**` (zero-or-more segments), `*` (one segment), `?` (one byte), and `[abc]` character classes
- Step 8b extension: after the M14 CheckUpdate succeeds, run `git diff-tree` to enumerate paths changed by the new commits and call `CheckPaths` against the rules
- First-match-rejects semantics (alphabetical by `path_pattern` for deterministic `matched_path`)
- Webhook + audit + metric extensions reusing the M15 `EventPolicyRefRejected` event with new `reason="blocked_path"` and `matched_path` attr
- Failure-closed posture: sqlite read errors and `git diff-tree` subprocess failures cause the update to reject with `internal-error` status
- Cap of 10000 paths on new-ref creation (oldOID = null OID); over-cap returns `internal-error` with operator-friendly squash-or-rebase guidance

### 1.2 Out of scope (deferred to follow-on milestones)

- Required signed commits (own milestone — GPG/SSH key store, signature verification, allowed_signers format)
- File size limits (separate check — walks blobs via `git ls-tree`, not the same primitive as path-diff)
- Author/email rules (commit walk + email pattern match)
- Commit message rules (commit walk + regex/conventional-commits match)
- Secret scanning hook integration (needs Tier 3 hook infrastructure)
- Pre-receive/update/post-receive arbitrary hooks (Tier 3 — sandboxed user-supplied code)
- Required status/check integration (explicitly deferred by spec)
- `policy paths add --action=warn` (Tier 1.5 — non-blocking soft policy)
- Path-rule reordering / priorities (alphabetical first-match good enough for Tier 1)
- Per-change-type rules (added vs modified vs deleted — all currently match)
- "Allow paths that pre-existed" semantics

## 2. Architecture overview

```
internal/policy/
  policy.go       (existing M14)  ← unchanged; CheckUpdate still does deletion + force-push
  paths.go        (new)           ← AddPathRule/ListPathRules/RemovePathRule + CheckPaths
  match.go        (new)           ← MatchPath + ValidatePathPattern with ** support
  paths_test.go   (new)
  match_test.go   (new)

internal/auth/sqlitestore/migrations/0007_protected_paths.sql  (new)

internal/gitcli/gitcli.go      (modified — add DiffTreeChangedPaths helper)
internal/gitproto/receivepack/complete.go  (modified — step 8b extension)
internal/webhooks/event.go     (modified — PolicyRefRejectedPayload gains MatchedPath)

cmd/bucketvcs/policy.go        (modified — paths add/list/remove subcommands)
```

**Step 8b wiring:** the existing per-update loop calls `eng.Policy.CheckUpdate(...)` (M14 deletion + force-push). After that succeeds, the loop ALSO runs `gitcli.DiffTreeChangedPaths(bareDir, oldOID, newOID)` and calls `eng.Policy.CheckPaths(...)` with the result. Both checks are first-rejection-wins; any rejection short-circuits subsequent checks for that ref.

**Optionality** preserved: nil `eng.Policy` → no checks at all. A repo with M14 policy rules but no `protected_paths` rows sees identical behavior — `CheckPaths` returns nil when no rules match.

**Failure-closed posture** matches M14: sqlite read errors and `git diff-tree` subprocess failures cause the update to reject with `internal-error` status (NOT silent accept).

## 3. Schema (migration 0007_protected_paths.sql)

```sql
CREATE TABLE protected_paths (
    tenant            TEXT NOT NULL,
    repo              TEXT NOT NULL,
    refname_pattern   TEXT NOT NULL,
    path_pattern      TEXT NOT NULL,
    created_at        INTEGER NOT NULL,
    PRIMARY KEY (tenant, repo, refname_pattern, path_pattern),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);
CREATE INDEX protected_paths_by_repo ON protected_paths (tenant, repo);

INSERT INTO schema_version (version, applied_at) VALUES (7, strftime('%s','now'));
```

**Design notes:**
- Composite PK `(tenant, repo, refname_pattern, path_pattern)` enforces uniqueness. Re-adding the same tuple is a no-op via `INSERT ... ON CONFLICT DO NOTHING` — the existing `created_at` is preserved and no error is returned. Unlike M14 protected_refs (which has updatable flag columns), M16 protected_paths has no non-PK column to update, so DO NOTHING is the cleanest idempotency contract.
- No `action` column. Tier 1 is always block. Adding warn/require-approval becomes a column without schema break.
- FK cascade to `repos` handles `repo delete` cleanup.
- No FK to `protected_refs` — path rules are independent of ref rules. A ref can have only path rules, only ref rules, both, or neither.

## 4. Path-glob matcher (`internal/policy/match.go`)

### 4.1 Pattern syntax

```
**     matches one or more path segments greedily. (Non-trailing **
       matches zero or more — only trailing ** requires at least one
       segment, so `secrets/**` matches files IN secrets/ but not the
       bare directory entry.)
*      matches anything within one segment (does NOT cross /)
?      matches one byte (does NOT cross /)
[abc]  character class (forwarded to stdlib path.Match)
/      literal segment separator
```

### 4.2 Examples

| Pattern | Matches | Does NOT match |
|---|---|---|
| `secrets/**` | `secrets/keys.txt`, `secrets/dev/.env` | `not-secrets/x`, `secrets` (no trailing path) |
| `.github/workflows/*` | `.github/workflows/ci.yml` | `.github/workflows/nested/run.yml` |
| `**/*.lock` | `go.sum.lock`, `frontend/yarn.lock`, `a/b/c.lock` | `lock.go`, `prefix.lockx` |
| `**` | every path | (empty input rejected at validation time) |
| `*.md` | `README.md` | `docs/README.md` |
| `**/secrets/**` | `app/secrets/x`, `secrets/k`, `a/b/secrets/c/d` | `mysecrets/x`, `secrets-old/x` |

### 4.3 API

```go
// MatchPath reports whether path matches pattern. See §4.1 for syntax.
// Returns (matched bool, err error); error is non-nil only for malformed
// patterns. Empty path or pattern is a validation error.
func MatchPath(pattern, path string) (bool, error)

// ValidatePathPattern returns an error if pattern is malformed. Called at
// rule-add time to reject bad input before storing it.
func ValidatePathPattern(pattern string) error
```

### 4.4 Implementation sketch

Split both pattern and path on `/`. Walk segments left-to-right with backtracking on `**`:
- `**` consumes zero or more path segments; try every split position
- Non-`**` segment matched via stdlib `path.Match` (single-segment globs work as documented)
- Empty pattern segment is invalid (rejects `//foo` and `foo//`)

~30 lines plus ~15 table-driven test cases.

## 5. Policy package extension (`internal/policy/paths.go`)

### 5.1 Path-rule CRUD

```go
// ProtectedPath is one row in protected_paths.
type ProtectedPath struct {
    Tenant         string
    Repo           string
    RefnamePattern string
    PathPattern    string
    CreatedAt      time.Time
}

// AddPathRule inserts a path rule. Validates path_pattern via
// ValidatePathPattern before insert. Returns ErrInvalidInput on bad
// pattern. Idempotent via INSERT ... ON CONFLICT DO NOTHING; re-adding
// the same (tenant, repo, refname_pattern, path_pattern) returns nil
// without modifying the existing row's created_at.
func (s *Service) AddPathRule(ctx context.Context, in ProtectedPath) error

// ListPathRules returns rules for (tenant, repo) ordered by
// (refname_pattern, path_pattern) ascending.
func (s *Service) ListPathRules(ctx context.Context, tenant, repo string) ([]ProtectedPath, error)

// RemovePathRule deletes the row matching (tenant, repo, refname_pattern,
// path_pattern). Returns ErrNotFound if no row matches.
func (s *Service) RemovePathRule(ctx context.Context, tenant, repo, refnamePattern, pathPattern string) error
```

### 5.2 Path check

```go
// CheckPaths walks the protected_paths rules for (tenant, repo). For each
// rule whose refname_pattern matches refname (via path.Match per M14
// convention), checks every entry in changedPaths against path_pattern
// (via MatchPath in this package). First-match-rejects.
//
// Returns nil if no rule fires. Returns *PolicyError with
// Reason="blocked_path", MatchedPattern=<path_pattern>, MatchedPath=<changed path>
// on rejection. Rule iteration is alphabetical by (refname_pattern,
// path_pattern) for deterministic MatchedPath.
func (s *Service) CheckPaths(ctx context.Context, tenant, repo, refname string,
    changedPaths []string) error
```

### 5.3 PolicyError extension

The existing M14 `PolicyError` struct gains an optional `MatchedPath string` field. Empty for ref-level rejections (`blocked_deletion`, `blocked_force_push`); populated for `blocked_path`. The existing `MatchedPattern` field is interpreted per-reason:
- `blocked_deletion` / `blocked_force_push`: refname_pattern that matched
- `blocked_path`: path_pattern that matched

`PolicyError.MetricOutcome()` adds the `blocked_path` case returning `"blocked_path"`.

## 6. Step 8b integration (`receivepack/complete.go`)

Insert between the existing CheckUpdate call and the next-iteration of the per-update loop:

```go
// Existing M14 check:
if perr := eng.Policy.CheckUpdate(ctx, tenant, repoID, m.BareDir(), u.Refname, u.OldOID, u.NewOID); perr != nil {
    // ... existing error handling ...
    continue
}

// M16 path check: only runs if CheckUpdate passed.
changedPaths, perr := gitcli.DiffTreeChangedPaths(m.BareDir(), u.OldOID, u.NewOID)
if perr != nil {
    statuses[i] = "ng " + u.Refname + " internal-error: " + perr.Error()
    policy.EmitRefCheckMetric(ctx, eng.loggerOrDefault(), "blocked_path_internal_error")
    policy.EmitRefInternalError(ctx, eng.loggerOrDefault(),
        tenant, repoID, u.Refname, actorNameFromEng(eng), perr)
    continue
}
if cerr := eng.Policy.CheckPaths(ctx, tenant, repoID, u.Refname, changedPaths); cerr != nil {
    var pathErr *policy.PolicyError
    if errors.As(cerr, &pathErr) {
        statuses[i] = "ng " + u.Refname + " " + pathErr.Error()
        policy.EmitRefCheckMetric(ctx, eng.loggerOrDefault(), pathErr.MetricOutcome())
        policy.EmitRefRejected(ctx, eng.loggerOrDefault(), tenant, repoID, pathErr, actorNameFromEng(eng))
        if eng.Webhooks != nil {
            payload := webhooks.PolicyRefRejectedPayload{
                Refname:        pathErr.Refname,
                MatchedPattern: pathErr.MatchedPattern,
                MatchedPath:    pathErr.MatchedPath,
                Reason:         pathErr.Reason,
                OldOID:         pathErr.OldOID,
                NewOID:         pathErr.NewOID,
            }
            if werr := eng.Webhooks.Enqueue(ctx, webhooks.EventPolicyRefRejected,
                tenant, repoID, actorNameFromEng(eng), payload); werr != nil {
                webhooks.EmitEnqueueFailed(ctx, eng.loggerOrDefault(),
                    tenant, repoID, "policy.ref.rejected", werr.Error())
            }
        }
        continue
    }
    // Non-PolicyError = sqlite read failure during CheckPaths.
    statuses[i] = "ng " + u.Refname + " internal-error: " + cerr.Error()
    policy.EmitRefCheckMetric(ctx, eng.loggerOrDefault(), "blocked_path_internal_error")
    policy.EmitRefInternalError(ctx, eng.loggerOrDefault(),
        tenant, repoID, u.Refname, actorNameFromEng(eng), cerr)
}
```

Atomic-batch poisoning continues to work — `anyStatusNonEmpty(statuses)` covers both M14 and M16 rejections.

## 7. `gitcli.DiffTreeChangedPaths` helper

New function in `internal/gitcli/gitcli.go`:

```go
// DiffTreeChangedPaths returns the set of paths touched by commits between
// oldOID and newOID (exclusive-inclusive). Uses `git diff-tree --raw` for
// a single subprocess invocation regardless of commit count.
//
// Special cases:
//   - newOID = NullOID (ref deletion): returns nil, nil (no paths to check)
//   - oldOID = NullOID (ref creation): walks ALL paths reachable from newOID
//     with a cap of 10000 entries. Returns a non-nil error indicating
//     truncation if the cap is hit — caller treats as internal-error
//     (operator-friendly guidance: squash or rebase).
//   - oldOID = newOID (no-op): returns nil, nil
//
// Path order is the natural diff-tree output (sorted by tree position).
// Duplicate paths (changed in multiple commits) appear once.
func DiffTreeChangedPaths(bareDir, oldOID, newOID string) ([]string, error)
```

**Implementation:**
- FF/non-FF update (both OIDs non-null): `git diff-tree --no-commit-id --name-only -r --diff-filter=ACMDRT <oldOID>..<newOID>`. The `..` range gives the set difference.
- New ref (oldOID = NullOID): `git diff-tree --no-commit-id --name-only -r --diff-filter=ACMDRT --root <newOID>` with the output piped through a 10000-line cap.
- Deletion (newOID = NullOID): return nil immediately, no subprocess.

Use `--diff-filter=ACMDRT` to capture Added, Copied, Modified, Deleted, Renamed, Typechange paths — every change type.

Deduplicate output by reading into a set then ordering keys.

## 8. Operator CLI

```
bucketvcs policy paths add    --auth-db=<path> --tenant=<t> --repo=<r>
                              --refname-pattern=<glob> --path-pattern=<glob>
bucketvcs policy paths list   --auth-db=<path> --tenant=<t> --repo=<r>
                              [--format=text|json]
bucketvcs policy paths remove --auth-db=<path> --tenant=<t> --repo=<r>
                              --refname-pattern=<glob> --path-pattern=<glob>
```

Mirrors the M14 `policy refs` shape exactly:
- NDJSON output (one record per line, no enclosing array)
- Exit codes: 0 ok, 1 operational, 2 usage
- `ErrInvalidInput` sentinel mapped to exit 2
- ON CONFLICT DO UPDATE on re-add (idempotent)
- Empty result set: text prints `tenant=X repo=Y (no protected paths)`; JSON emits nothing
- `--path-pattern` validated via `ValidatePathPattern` at add time

The existing `bucketvcs policy refs` dispatch (M14) is unchanged.

## 9. Observability

### 9.1 Metric extension

Existing `policy_refs_check_total{outcome}` gains two new outcome values:
- `blocked_path` — path rule rejected the update
- `blocked_path_internal_error` — `git diff-tree` subprocess failure or sqlite read failure during CheckPaths

No new metric name — the existing counter is sufficient for the dashboard.

### 9.2 Audit event extension

Existing `policy.ref.rejected` audit event gains:
- `matched_path` attr (empty unless reason is `blocked_path`)

The `matched_pattern` attr is now interpreted per-reason:
- `blocked_deletion` / `blocked_force_push`: the refname_pattern that matched
- `blocked_path`: the path_pattern that matched

Existing dashboards keying on `matched_pattern` continue to work; operators alerting on path rejections key on the new `matched_path` attr.

### 9.3 Webhook payload extension

`PolicyRefRejectedPayload` gains:

```go
MatchedPath string `json:"matched_path,omitempty"`
```

Receivers tolerant of unknown JSON fields see no breakage. The single existing event type `EventPolicyRefRejected` covers both Tier 1 and Tier 2 — receivers use the `reason` field to distinguish.

## 10. Failure modes

| Failure | Behavior |
| --- | --- |
| `--path-pattern` malformed at add time | exit 2, `ErrInvalidInput` |
| `git diff-tree` subprocess fails | reject with `internal-error` status (fail-closed) |
| `git diff-tree` returns >10000 paths on new-ref creation | reject with `internal-error: too many paths in new-ref creation; squash or rebase` |
| sqlite read fails during CheckPaths | reject with `internal-error` (fail-closed) |
| Multiple path rules match the same change | first-match-rejects (alphabetical by `path_pattern`) — `matched_path` and `matched_pattern` reflect the first match |
| Ref deletion (NewOID = null OID) | path check skipped — no commits introduced |
| Empty diff (e.g. ref reset to same OID) | path check no-ops |
| Webhook enqueue fails after rejection | fail-open per M15 — `webhooks.enqueue_failed` audit; the rejection still applies |

## 11. Testing

### 11.1 Unit
- `MatchPath` table-driven (~15 cases covering `**`, `*`, `?`, `[]`, edges)
- `ValidatePathPattern` accepts valid, rejects empty/`//`/lone `[` etc.
- `Service.AddPathRule` / `ListPathRules` / `RemovePathRule` against on-disk sqlite seeded with a repos row; upsert idempotency; ErrNotFound on remove of missing row
- `Service.CheckPaths` with: zero rules, single match, no match, multiple matches with deterministic first-match order

### 11.2 Integration
- `receivepack` test extends the M14 policy harness: push attempts to modify `.github/workflows/ci.yml` with a path rule `refname=refs/heads/main` × `path=.github/workflows/**`; assert rejection with `reason=blocked_path` and `matched_path=.github/workflows/ci.yml`
- `gitcli.DiffTreeChangedPaths`: real bare repo with 3 commits; assert path set for FF, non-FF, new-ref (with and without truncation), and ref-deletion

### 11.3 CLI
- `policy paths add` happy + duplicate (upsert) + bad pattern (exit 2)
- `policy paths list` empty + non-empty + JSON format
- `policy paths remove` happy + ErrNotFound (exit 1)

### 11.4 Smoke

Extend `scripts/m14-policy-smoke.sh` (NOT a new script; M16 is a policy extension):

After the existing protected-refs scenarios:
1. Register a path rule: `policy paths add --refname-pattern=refs/heads/main --path-pattern=secrets/**`
2. Push a commit modifying `secrets/api.key` → assert rejection with `reason=blocked_path`
3. Push a commit modifying `README.md` → assert acceptance (no rule matches)
4. Remove the path rule → push to `secrets/api.key` succeeds

## 12. Acceptance criteria

- All 8 design dimensions implemented per spec
- `MatchPath` passes the documented examples
- `git diff-tree` shell-out works on real repos (smoke verifies)
- Step 8b extension preserves M14 behavior (existing tests still pass)
- Webhook receivers see `matched_path` only for `reason=blocked_path` events
- `scripts/m14-policy-smoke.sh` passes with the new Tier 2 scenarios
- All prior smokes still pass
- `go test ./...` clean, `go vet ./...` clean
- M14 path-pattern-free repos see identical behavior to today

## 13. Open questions

None — all decisions captured above.
