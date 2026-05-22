# M14 — Hooks and policy: protected refs (design spec)

**Status:** Design.
**Date:** 2026-05-21.
**Author:** Eran (with Claude).
**Spec section:** §23 "Hooks, policy, and server-side validation."
**Predecessor:** M13.5 quotas (commit `52764b0`, tag `m13.5-quotas`) — sibling M4-authdb-backed feature; same Service / CLI / metrics pattern.

---

## 1. Goal

Add server-side enforcement of **protected refs** to the receive-pack pipeline. Operators configure per-repo rules that block deletion and/or force-push (non-fast-forward updates) of refs matching a glob pattern. Rejections fire as standard `ng <refname> protected-branch: <reason>` lines in the receive-pack report-status; the existing atomic/non-atomic batch handling propagates naturally.

This is **Tier 1** of the §23 hooks-and-policy roadmap — the smallest milestone that delivers the load-bearing operator pain point (rejecting force-push to `main`). The architecture established here is additive: Tier 2 rule families (file size, path restrictions, author/email, commit-message regex) and Tier 3 external hooks (shell scripts, HTTP webhooks) become new tables / new policy types under the same package, CLI namespace, and integration seam.

## 2. Non-goals (deferred to later milestones)

- **Tier 2 rules**: file size limits, path restrictions (filename allow/deny globs), commit author/committer email regex, commit message regex, signed-commit verification.
- **Tier 3 external hooks**: shell-script `pre-receive` / `update` / `post-receive` execution, HTTP webhooks. Spec §23 explicitly permits "MVP MAY implement policy-native equivalents before arbitrary user code."
- **Post-receive infrastructure.** Spec §23 says post-receive MUST run after commit, but nothing in Tier 1 needs a post-commit decision (both blocked-deletion and blocked-force-push are pre-commit checks). Post-receive becomes the natural injection point for §24 webhooks; both deferred together.
- **`update` hook class.** A per-ref hook that fires between pre-receive and the ref move. Tier 1's policy check IS effectively the update hook; we don't add a second abstraction.
- **Tenant-level default rules.** Per-repo configuration only in MVP. New repos start unprotected; the operator runs `policy refs add` per repo. Tenant defaults are a clean follow-up if operators ask.
- **Identity-based gating** (e.g., "only the CI service account can push to `main`"). Requires RBAC primitives we don't have; out of scope.
- **`block_create` toggle** (e.g., "no new `release/*` branches without admin"). Tier 2 ask that depends on identity.
- **Recursive glob (`**`)** in refname patterns. Stdlib `path.Match` doesn't support it; operators add multiple rows for `refs/heads/release/*` + `refs/heads/release/v*/*` as needed. Adding a recursive matcher adds a dependency or hand-rolled code without clear payoff.
- **Per-rule actor/user attribution** on `policy.ref.rejected` audit events beyond the existing receive-pack actor. Already covered by `actor` field; no separate "denied-by-rule" field.

## 3. Scope (in MVP)

- New `protected_refs` table on the M4 authdb (migration 0005).
- New `internal/policy` package with `Service.{Add, List, Remove, CheckUpdate}` and a typed `*PolicyError`.
- New `cmd/bucketvcs/policy.go` mounting `bucketvcs policy refs {add,list,remove}` under main's dispatch.
- One new field on `receivepack.EngineRequest`: `Policy *policy.Service` (nil = pre-M14 behavior, no enforcement).
- New step 8b in `internal/gitproto/receivepack/complete.go::completeReceivePack` that runs policy checks after connectivity verification and before the refUpdates map is built.
- One new metric (`policy_refs_check_total{outcome}`) + one new audit event (`policy.ref.rejected`).
- Localfs end-to-end smoke + unit tests.
- Operator guide: new `docs/m14-hooks-policy-operator-guide.md` (separate from the M13 LFS guide — different audience: server admin vs. LFS operator).

## 4. Architecture

### 4.1 State location

The protected-ref rules live on the M4 authdb sqlite — the same store that holds users, tokens, repo permissions, LFS locks (M13.3), and quotas (M13.5). Reusing the existing sqlite gives us:

- Single operational model for control-plane state.
- sqlite's single-writer serialization for atomic ref-pattern adds/removes.
- No new lifecycle (authdb is already opened by the gateway at startup and closed at shutdown).

Migration `0005_protected_refs.sql`:

```sql
CREATE TABLE protected_refs (
    tenant            TEXT NOT NULL,
    repo              TEXT NOT NULL,
    refname_pattern   TEXT NOT NULL,
    block_deletion    INTEGER NOT NULL DEFAULT 1 CHECK (block_deletion IN (0,1)),
    block_force_push  INTEGER NOT NULL DEFAULT 1 CHECK (block_force_push IN (0,1)),
    created_at        INTEGER NOT NULL,
    PRIMARY KEY (tenant, repo, refname_pattern),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);

INSERT INTO schema_version (version, applied_at) VALUES (5, strftime('%s','now'));
```

The FK to `repos(tenant, name)` means deleting a repo cascades through and drops its policy rules — no orphaned state.

### 4.2 New package `internal/policy`

Single type `Service`. All methods take `ctx` and return an error. The `bareDir` parameter on `CheckUpdate` lets the service call `git merge-base --is-ancestor` against the local bare without importing `internal/gitcli` — we use `os/exec` directly to keep the policy package a leaf.

```go
package policy

type ProtectedRef struct {
    Tenant          string
    Repo            string
    RefnamePattern  string
    BlockDeletion   bool
    BlockForcePush  bool
    CreatedAt       time.Time
}

// PolicyError is returned by CheckUpdate when a ref update is rejected.
// Callers (receive-pack) use errors.As to recover the structured fields
// for the `ng <refname> protected-branch: <reason>` report-status line
// and for the policy.ref.rejected audit event.
type PolicyError struct {
    Refname        string
    MatchedPattern string
    Reason         string // "deletion blocked" | "non-fast-forward push blocked"
    OldOID         string
    NewOID         string
}

func (e *PolicyError) Error() string {
    return fmt.Sprintf("protected-branch: %s by pattern %s (refname=%s)",
        e.Reason, e.MatchedPattern, e.Refname)
}

type Service struct {
    db     *sql.DB
    logger *slog.Logger
}

func New(db *sql.DB, logger *slog.Logger) *Service

// Add creates a protected-ref rule. Both block flags default to true
// in the CLI; passing false makes the rule a no-op for that toggle.
func (s *Service) Add(ctx context.Context, ref ProtectedRef) error

// List returns every rule for the given (tenant, repo) ordered by pattern.
func (s *Service) List(ctx context.Context, tenant, repo string) ([]ProtectedRef, error)

// Remove deletes the rule whose pattern matches exactly (no glob expansion).
func (s *Service) Remove(ctx context.Context, tenant, repo, pattern string) error

// CheckUpdate runs all matching rules against one ref update.
// bareDir is the local bare repository directory used for fast-forward
// detection via `git merge-base --is-ancestor`. Returns *PolicyError on
// rejection, or nil to accept. Returns nil for any non-error condition,
// including "no rules match this refname" and "no rules exist for repo".
func (s *Service) CheckUpdate(ctx context.Context, tenant, repo, bareDir string,
    refname, oldOID, newOID string) error
```

The Service is **opt-in** in the same way as M13.5's quota.Service: pass `Policy: nil` and the receive-pack pipeline does zero enforcement. Pre-M14 deployments and operators who don't want policy see no behavioral change.

### 4.3 Integration seam in `receivepack.completeReceivePack`

Current pipeline (annotated by step number from existing code comments):

1. Re-read manifest body under write lock.
2. Build refstore from body.
3. (no separate step)
4. (no separate step)
5. `precheckUpdates`: old-OID validation, set per-ref statuses for mismatches.
6. Atomic-batch poisoning when needed.
7. `IndexPackStrict`: stage the inbound pack into the bare.
8. Connectivity check via `git rev-list --not --all`.
9. Build `refUpdates` map from accepted updates.
10. `BuildAndCommit`: repack, upload, CAS commit.

**New Step 8b** (policy enforcement) goes between 8 (connectivity) and 9 (build refUpdates map):

```go
// Step 8b: policy enforcement. For each accepted update, walk the
// repo's protected_refs rules; reject the update if any matching
// rule blocks the operation. M14 opt-in via deps.Policy=nil.
if eng.Policy != nil {
    for i, u := range rp.Updates {
        if statuses[i] != "" {
            continue
        }
        if err := eng.Policy.CheckUpdate(ctx, tenant, repoID,
            m.BareDir(), u.Refname, u.OldOID, u.NewOID); err != nil {
            var perr *policy.PolicyError
            if errors.As(err, &perr) {
                statuses[i] = "ng " + u.Refname + " " + perr.Error()
                // metric + audit:
                policy.EmitRefCheckMetric(ctx, logger, perr.MetricOutcome())
                policy.EmitRefRejected(ctx, logger, tenant, repoID, perr, actorName)
                continue
            }
            // Internal error from CheckUpdate (git subprocess failed,
            // sqlite read failed). Surface as internal-error so the
            // client sees a clear distinction from policy rejection.
            statuses[i] = "ng " + u.Refname + " internal-error: " + err.Error()
        } else {
            policy.EmitRefCheckMetric(ctx, logger, "ok")
        }
    }
    // If atomic and any policy rejection landed, poison the whole
    // batch — same shape as the precheck atomic handling.
    if rp.IsAtomic && anyPolicyRejection(statuses) {
        // ... mirror precheckUpdates' atomic-batch handling ...
    }
}
```

`CheckUpdate`'s internal logic:

1. SELECT all rows from `protected_refs` for `(tenant, repo)`. Cache result for the duration of the call (one read per ref update is acceptable for Tier 1; per-repo caching is a Tier 2 perf optimization).
2. For each row, `path.Match(row.RefnamePattern, refname)`. Skip non-matches.
3. For each matching row:
   - If `newOID == oidconst.NullOIDHex` and `row.BlockDeletion`: build `*PolicyError{Reason: "deletion blocked"}` and return.
   - Otherwise if both `oldOID != NullOIDHex` and `newOID != NullOIDHex` and `row.BlockForcePush`: shell out to `git merge-base --is-ancestor <oldOID> <newOID>` in `bareDir`. Exit 0 = fast-forward (allow). Exit 1 = non-FF: build `*PolicyError{Reason: "non-fast-forward push blocked"}` and return. Other exit code = internal error; propagate.
4. New ref creations (`oldOID == NullOIDHex`) are never rejected by Tier 1 rules — neither toggle applies.

### 4.4 CLI

```
bucketvcs policy refs add    --auth-db=PATH --tenant=T --repo=R --pattern=PAT \
                              [--allow-deletion] [--allow-force-push]
bucketvcs policy refs list   --auth-db=PATH --tenant=T --repo=R [--format=text|json]
bucketvcs policy refs remove --auth-db=PATH --tenant=T --repo=R --pattern=PAT
```

Common conventions match M13.5 quota:
- `--auth-db=<path>` required on every subcommand.
- Exit codes: 0 ok, 1 operational error, 2 usage error.
- `--format=text` (default) is human-readable; `--format=json` for `list`.

**Default semantics**: `add` with no flags creates a rule that blocks both deletion and force-push. `--allow-deletion` and `--allow-force-push` are explicit opt-outs that flip the respective columns to 0. This biases toward safety: the operator who runs `bucketvcs policy refs add --pattern=refs/heads/main` and forgets the flags gets full protection.

`list` output (text):
```
tenant=acme  repo=site  pattern=refs/heads/main          block_deletion=true   block_force_push=true   created=2026-05-21T12:34:56Z
tenant=acme  repo=site  pattern=refs/heads/release/*     block_deletion=true   block_force_push=true   created=...
```

JSON:
```json
{"tenant":"acme","repo":"site","pattern":"refs/heads/main","block_deletion":true,"block_force_push":true,"created_at":"2026-05-21T12:34:56Z"}
```

`remove` takes an exact pattern (no glob expansion of the pattern itself).

## 5. Data flow

### 5.1 Happy path — accepted FF push to a protected branch

Operator has run:
```
bucketvcs policy refs add --auth-db=PATH --tenant=acme --repo=site --pattern=refs/heads/main
```

Developer pushes a FF commit to `refs/heads/main`:

1. Gateway routes `POST /acme/site.git/git-receive-pack` to `receivepack.Service`.
2. `completeReceivePack` runs through steps 1-8 (existing flow).
3. Step 8b: for the `refs/heads/main` update, `CheckUpdate` calls `path.Match("refs/heads/main", "refs/heads/main") = true`. The rule has `block_force_push=true`. Old-OID and new-OID are both non-null. `git merge-base --is-ancestor <old> <new>` returns exit 0 (FF). No `PolicyError` returned. `statuses[i]` stays empty.
4. `policy_refs_check_total{outcome=ok}` fires.
5. Pipeline proceeds to step 9-10 normally; the push succeeds.

### 5.2 Rejected — force-push to a protected branch

Same setup; developer attempts a force-push (`git push --force`):

1. Steps 1-8 succeed.
2. Step 8b: `CheckUpdate` matches the same rule. `git merge-base --is-ancestor <old> <new>` returns exit 1 (non-FF). `CheckUpdate` returns `*PolicyError{Refname: "refs/heads/main", MatchedPattern: "refs/heads/main", Reason: "non-fast-forward push blocked", OldOID: <old>, NewOID: <new>}`.
3. `statuses[i] = "ng refs/heads/main protected-branch: non-fast-forward push blocked by pattern refs/heads/main (refname=refs/heads/main)"`.
4. `policy_refs_check_total{outcome=blocked_force_push}` fires. `policy.ref.rejected` audit event fires with all structured fields.
5. The report-status line goes back to the client. `git push --force` exits non-zero with the operator's message visible.

### 5.3 Rejected — branch deletion

Setup: same rule (deletes blocked by default). Developer attempts `git push origin :main`:

1. Steps 1-8 succeed.
2. Step 8b: rule matches. `newOID == NullOIDHex`. `BlockDeletion=true`. Return `*PolicyError{Reason: "deletion blocked"}`.
3. `statuses[i] = "ng refs/heads/main protected-branch: deletion blocked by pattern refs/heads/main (refname=refs/heads/main)"`.
4. `policy_refs_check_total{outcome=blocked_deletion}` fires; audit fires.

### 5.4 Multiple rules matching one refname

Operator has added two rules:
```
bucketvcs policy refs add --pattern=refs/heads/main          # blocks both
bucketvcs policy refs add --pattern=refs/heads/*             # allows force-push (--allow-force-push), blocks delete
```

A force-push to `refs/heads/main`:

1. Step 8b iterates protected_refs ORDER BY pattern.
2. First matching rule: `refs/heads/main` with `block_force_push=true` → rejects. Loop short-circuits on the first rejection.

The "first match wins (on reject)" rule keeps the model simple: if ANY matching rule blocks the operation, reject. Operators who need union-vs-intersection semantics use a single more-specific rule.

### 5.5 Atomic-batch interaction

Client sends a batch of 3 ref updates with `atomic` capability:
- `refs/heads/feature/x`: regular FF (no rule matches)
- `refs/heads/main`: force-push (matched, rejected)
- `refs/tags/v1.0`: new tag creation (no rule matches)

Step 8b processes all three updates:
1. `feature/x`: ok.
2. `main`: rejected. `statuses[i]` set.
3. `v1.0`: ok.

After the loop, the existing atomic-batch poisoning logic (from `completeReceivePack` line ~120) detects `!allOK` AND `rp.IsAtomic`, and rewrites every empty status to `ng <refname> atomic-batch-failed`. All three updates are reported as failed; nothing is committed.

This matches the existing precheck atomic behavior — no new branch needed.

## 6. Race semantics and failure modes

### 6.1 Concurrent policy edits

Operator runs `policy refs add` and `policy refs remove` for the same `(tenant, repo, pattern)` concurrently with an in-flight push. The push is processing step 8b's `SELECT * FROM protected_refs WHERE tenant=? AND repo=?` while the operator's `INSERT`/`DELETE` runs.

sqlite's single-writer model serializes the operator's writes. The push's read either sees the row or not — both outcomes are consistent. There's no "half-applied" state. Operator-side double-runs (run `add` after `remove`) converge.

A push that began before the rule was added sees the pre-rule state and may succeed without rejection. The next push will see the new rule. No retroactive enforcement — that's by design (matches industry-standard hosted Git semantics).

### 6.2 Glob pattern misuse

Stdlib `path.Match` returns `(false, nil)` for non-matching, `(true, nil)` for matching, and `(false, ErrBadPattern)` for malformed patterns (unclosed brackets, etc.).

`CheckUpdate` treats `ErrBadPattern` as an internal error — surfacing it via the `internal-error:` status. Operators who paste a broken glob into the CLI get a clearer error path at `policy refs add` time: the Service `Add` method validates the pattern with `path.Match("", pattern)` before INSERT and returns a CLI usage error (exit code 2).

### 6.3 Force-push detection failures

`git merge-base --is-ancestor` shells out to git. Failure modes:

- **Exit 0**: ancestor (FF). Allow.
- **Exit 1**: not ancestor (non-FF). Block (per rule).
- **Exit 2 / other**: error (corrupted bare, missing OID, etc.). Treated as internal error: status `ng <refname> internal-error: merge-base failed: <stderr>`. The push fails; operator investigates the bare's integrity.

This is consistent with the existing connectivity check's posture: if git itself can't verify a property of the bare, the push doesn't proceed.

### 6.4 Race between policy lookup and ref update

Step 8b runs after IndexPackStrict (step 7) — the new commits are already in the bare. The CAS at step 10 (`BuildAndCommit`) is what commits the ref pointer. So policy makes a decision based on bare state that includes the inbound pack, then commits (or doesn't) shortly after.

If another push lands between step 8b and step 10 (concurrent push, different ref), BuildAndCommit's CAS catches the manifest version mismatch and retries — its outer loop re-reads the body and re-applies the updates. Policy doesn't need to re-run; the rule decision was about THIS ref update's old→new transition, not the manifest version.

If another push lands AT THE SAME REF between step 8b and step 10 (impossible inside the per-repo write lock; the lock is held across the entire `completeReceivePack`), this race can't happen.

### 6.5 Authdb failures

`CheckUpdate`'s SELECT can fail (transient sqlite error, disk full, etc.). The function returns the error; step 8b sets `statuses[i] = "ng <refname> internal-error: ..."`. This is **deliberately strict**: a policy lookup failure CANNOT silently allow a write to a protected branch. Operators alert on these errors; the client retries.

Contrast with M13.5's quota: a quota write failure on the verify path is logged but doesn't fail the verify (because the client already uploaded). Here we're at the pre-commit phase — failing closed is the correct posture.

## 7. Observability

### 7.1 Metric

`policy_refs_check_total{outcome}` — counter. One emission per ref update that goes through Step 8b.
- `outcome=ok`: rule didn't match OR all matching rules allowed.
- `outcome=blocked_deletion`: rejected because rule blocked deletion.
- `outcome=blocked_force_push`: rejected because rule blocked non-FF.

Implemented in `internal/policy/metrics.go` mirroring the M13.5 `lfs.metrics` shape. Exported for cross-package use (receive-pack calls it).

### 7.2 Audit event

`policy.ref.rejected` — fires only on rejection. Accepted pushes already emit the existing receive-pack audit (a per-batch event with all accepted refs); accepted policy checks are implicit in that flow.

Attrs:
- `tenant`, `repo` — `<tenant>/<repo>` slash form not used; separate fields for grep convenience.
- `refname` — the rejected ref.
- `matched_pattern` — the pattern that triggered the rejection.
- `reason` — `"deletion blocked"` or `"non-fast-forward push blocked"`.
- `actor` — actor name from `eng.Actor` (or `"anonymous"`).
- `old_oid`, `new_oid` — the rejected update's OIDs (the new_oid is the null hex for deletions).

Implemented in `internal/policy/audit.go`.

### 7.3 Recommended alerts

- `rate(policy_refs_check_total{outcome=~"blocked_.*"}[5m]) > 0` for a sustained period suggests either (a) operators forgetting the protection exists, or (b) a misconfigured CI client retrying. Worth surfacing.
- `policy.ref.rejected` events with the same `(actor, refname)` recurring within a short window suggest a confused user — manual outreach saves a support ticket.

## 8. Failure modes (operator-facing)

| Symptom | Cause | Action |
|---|---|---|
| Push fails with `protected-branch: deletion blocked` | Rule with `block_deletion=true` matched | Expected. Operator removes the rule (`policy refs remove`) or relaxes it (re-add with `--allow-deletion`). |
| Push fails with `protected-branch: non-fast-forward push blocked` | Rule with `block_force_push=true` matched a non-FF update | Expected. Developer must rebase / merge / open a PR. |
| Push fails with `protected-branch: ... by pattern <PAT>` but `PAT` doesn't seem to match | Wrong glob semantics. Stdlib `path.Match` doesn't support `**`; `*` doesn't cross `/`. | `policy refs list` to confirm patterns. Re-add with a more specific pattern (e.g., `refs/heads/release/*` instead of `refs/heads/release/**`). |
| Push fails with `internal-error: merge-base failed: ...` | The local bare is corrupted or the OIDs aren't reachable | Investigate bare integrity; likely needs `bucketvcs maintenance --force-rebuild` (M9). |
| Push fails with `internal-error: ...sqlite...` | authdb read failure (disk full, lock contention, file moved) | Failing closed is intentional — quota would be similar. Investigate authdb. |
| All pushes to `refs/heads/main` succeed despite a rule | Pattern doesn't match the refname literally — operators sometimes use `main` instead of `refs/heads/main` | `policy refs list` shows the actual stored pattern. Re-add with the full ref form. |

## 9. Migration / compatibility

### 9.1 Schema

- `0005_protected_refs.sql` adds the table + CHECK constraints. Forward-only.
- Bumps schema_version 4 → 5.
- Existing `RunMigrations` infrastructure applies it in sequence.

### 9.2 Backward compatibility

- A pre-M14 gateway running against an M14-migrated authdb works unchanged — it doesn't query `protected_refs`.
- An M14 gateway running against a pre-M14 authdb runs `RunMigrations` on startup (existing behavior), bumping 4 → 5 in-place.
- An M14 gateway with `EngineRequest.Policy = nil` (operator opts out, or gateway built without policy wiring) behaves exactly like pre-M14: no enforcement, no metric emissions for the policy family.

### 9.3 Removing the feature

Drop the CLI (`cmd/bucketvcs/policy.go`) and the gateway plumbing that constructs `*policy.Service`. The table from migration 0005 stays around (harmless when unused). A future migration could drop it.

## 10. Deferred (explicit non-goals, all separately tracked)

- **Tier 2 rule families** (file size, path restrictions, commit author email regex, commit message regex, signed-commit verification) — each is a new policy type under the same `internal/policy` package + a new CLI subcommand (`policy size-limits add/list/remove`, etc.).
- **Tier 3 external hooks** (shell-script pre-receive / update / post-receive) — requires sandboxing, time limits, output capture; spec §23 explicitly hedges as MAY-after-MVP.
- **HTTP webhook hooks** — overlaps with §24 webhooks; both deferred together.
- **Post-receive infrastructure** — natural extension point for webhooks; nothing in Tier 1 needs it.
- **Tenant-level default rules** — auto-apply a default ruleset to all repos under a tenant.
- **`block_create` toggle** — gates new ref creation; depends on identity to be useful.
- **Recursive glob patterns** (`**`) — stdlib `path.Match` doesn't support; not worth a dependency for Tier 1.
- **Bulk rule operations** — `policy refs apply --file=rules.json` for ops-as-code workflows.
- **Audit on accept** — `policy.ref.allowed` events would double the audit volume for the common case; the existing receive-pack audit already covers accepted pushes.
- **Per-rule actor allowlist** — "block force-push to main UNLESS actor in [...]"; requires identity gating (RBAC, deploy-key role tags, etc.).

## 11. Verification gates (before tag)

- `go test ./... -count=1` clean (modulo pre-existing importer / gitcli flake; passes in isolation).
- `go vet ./...` clean.
- `bash scripts/m14-policy-smoke.sh` exits `M14_POLICY_SMOKE_OK`.
- M11/M12/M12.1/M13/M13.3/M13.4/M13.5 smokes still green.
- Per the M1+ review protocol: superpowers spec + code-quality reviews until clean, then roborev-refine on max reasoning until pass or diminishing returns.

## 12. Implementation order (preview)

The implementation plan is generated separately in `docs/superpowers/plans/2026-05-21-m14-hooks-policy.md`. High-level order:

1. **Migration + Service core** — `0005_protected_refs.sql`, `internal/policy/` package with Add/List/Remove + `path.Match` pattern validation; unit tests.
2. **CheckUpdate + force-push detection** — `CheckUpdate` method, `git merge-base --is-ancestor` shell-out, `*PolicyError` type, table-driven unit tests covering the full decision matrix.
3. **Wire into receive-pack** — `EngineRequest.Policy` field, step 8b in `completeReceivePack`, atomic-batch interaction; integration tests in `internal/gitproto/receivepack/`.
4. **CLI** — `cmd/bucketvcs/policy.go` mounting `policy refs {add,list,remove}`; CLI tests.
5. **Observability + smoke + operator guide** — metric + audit emitters, `scripts/m14-policy-smoke.sh`, `docs/m14-hooks-policy-operator-guide.md`.
6. **Squash + tag `m14-protected-refs` + memory updates.**

## 13. Open questions

None at design time. Glob semantics, rule precedence (first-reject-wins), authdb-vs-bucket storage, atomic-batch interaction, failure-closed posture on authdb errors, observability shape, and the deferred-scope boundary are all answered above with explicit trade-offs.
