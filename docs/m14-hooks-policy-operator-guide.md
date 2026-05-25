# M14 — Protected refs (operator guide)

This guide covers the M14 Tier 1 protected-refs feature. It explains what M14 ships, how to configure rules via the `bucketvcs policy` CLI, how enforcement integrates with receive-pack, how to read the metrics + audit events, and the common operator recipes.

The companion design document is `docs/superpowers/specs/2026-05-21-m14-hooks-policy-design.md`; the implementation plan is `docs/superpowers/plans/2026-05-21-m14-hooks-policy.md`.

Production readiness summary:

- Tier 1 protected refs (deletion + force-push blocking on glob-matched refnames) — **shipped** (M14).
- Tier 2 path restrictions (`**`-aware glob matching against the diff-tree) — **shipped** (M16).
- Tier 3 external hooks (shell-script `pre-receive` + `post-receive` under a `bwrap` namespace sandbox) — **shipped** (M20, see §11).
- Tier 2 file-size / commit-metadata / signing rules — **deferred**.
- Schema 4 → 9 (`0005_protected_refs.sql` ... `0009_hooks.sql`) are forward-only and applied by the existing `RunMigrations`.

---

## 1. Overview

M14 introduces a single protected-refs rule family. An operator registers a rule by `(tenant, repo, refname_pattern)`, and the gateway's receive-pack engine consults the rule for every ref update on that repo. Rules block two operations:

- **Deletion** of a matching ref (`old_oid != 0…0`, `new_oid == 0…0`).
- **Non-fast-forward update** of a matching ref (`merge-base --is-ancestor old new` exits non-zero).

A rule that doesn't match the inbound refname is a no-op; a rule whose ref class doesn't apply (creation, FF update) is a no-op. New-ref creation is **always allowed** in Tier 1 — `block_create` is a Tier 2 extension.

The MVP intentionally omits webhooks, signed-commit verification, file-size limits, commit-author email enforcement, and per-actor allowlists. Operators who need any of those today must run an external pre-receive proxy or wait for Tier 2 / 3.

What ships:

- `internal/policy` package with `Service.Add / List / Remove / CheckUpdate` and `*PolicyError`.
- `bucketvcs policy refs add | list | remove` CLI.
- Receive-pack step 8b enforcement (HTTP + SSH transports).
- `policy_refs_check_total{outcome}` metric and `policy.ref.rejected` + `policy.ref.internal_error` audit events.
- Migration `0005_protected_refs.sql`.

What does not ship (full list in §9):

- Recursive globs (`**`).
- Per-actor allowlists.
- Tenant-level default rules.
- File / commit / author / signature checks.
- External hooks and webhooks.
- A `block_create` toggle.

---

## 2. CLI reference

All `policy` subcommands act on rows in the gateway's authdb (`bucketvcs.db`). They require `--auth-db <path>`; if you omit it, the CLI fails with usage error 2 rather than picking up a default.

### 2.1 `policy refs add`

```
bucketvcs policy refs add \
    --auth-db=<path> \
    --tenant=<tenant> \
    --repo=<repo> \
    --pattern=<glob> \
    [--allow-deletion] \
    [--allow-force-push]
```

A freshly-added rule blocks both deletion and force-push. The two `--allow-*` flags loosen specific protections; passing both reduces the rule to a no-op (no operation is blocked) but the row stays in the table so subsequent `list` shows it.

Output is one line summarising the row:

```
tenant=acme  repo=site  pattern=refs/heads/main  block_deletion=true  block_force_push=true
```

Exit codes:

- `0` — rule inserted.
- `1` — operational error (authdb unreachable, schema gate failed, …).
- `2` — usage error (missing flag, malformed glob).

Re-adding the same `(tenant, repo, pattern)` updates the existing row's `block_deletion` and `block_force_push` flags in place. `created_at` is preserved. Exit code is 0.

### 2.2 `policy refs list`

```
bucketvcs policy refs list \
    --auth-db=<path> \
    --tenant=<tenant> \
    --repo=<repo> \
    [--format=text|json]
```

- `--format=text` (default): one line per rule, key=value style.
- `--format=json`: **NDJSON** — one JSON object per line, no enclosing array. An empty result set emits nothing (zero bytes on stdout).

Sample JSON output for one rule:

```json
{"tenant":"acme","repo":"site","pattern":"refs/heads/main","block_deletion":true,"block_force_push":true,"created_at":"2026-05-22T15:38:21Z"}
```

Exit codes: `0` always (a repo with no rules is a normal state, not an error).

### 2.3 `policy refs remove`

```
bucketvcs policy refs remove \
    --auth-db=<path> \
    --tenant=<tenant> \
    --repo=<repo> \
    --pattern=<glob>
```

Removes the row matching the exact `(tenant, repo, pattern)` triple. The pattern is compared as a string — not as a glob — so a pattern of `refs/heads/*` is **not** removed by `refs/heads/main`.

Removing a non-existent rule is **not an error** — `policy refs remove` is idempotent so that ops-as-code workflows can drive a desired state without first reconciling. Stdout shows the removed/no-op line.

Exit codes: `0` (including no-op), `1` operational, `2` usage.

---

## 3. Pattern semantics

Patterns use stdlib `path.Match` globs. This is intentional — pulling in a richer glob package would either bloat dependencies or skew semantics from Go's path manipulation.

| Construct | Behaviour |
|---|---|
| `*` | Matches any run of characters **within one segment** — does NOT cross `/`. |
| `?` | Matches exactly one character (not `/`). |
| `[abc]` / `[a-z]` | Character class. |
| `\?` | Escapes a literal `?`. |

Patterns must be syntactically valid at `Add` time; `policy refs add` returns a usage error if the pattern is malformed. Any other `path.Match` error at enforcement time is surfaced via `internal-error:` status (see §4).

### 3.1 Examples

| Pattern | Matches |
|---|---|
| `refs/heads/main` | exactly `refs/heads/main` |
| `refs/heads/release/*` | `refs/heads/release/v1`, `refs/heads/release/v2-rc` — but NOT `refs/heads/release/v1/x` (no `/` crossing) |
| `refs/heads/*` | `refs/heads/main`, `refs/heads/dev` — but NOT `refs/heads/feature/x` |
| `refs/tags/v*` | `refs/tags/v1`, `refs/tags/v2.0` |
| `refs/heads/[mr]ain` | `refs/heads/main`, `refs/heads/rain` |

There is no `**`. To protect nested namespaces, add multiple rules.

### 3.2 Combine semantics: "any blocking rule rejects"

Multiple rules may match the same refname. The engine walks all matching rules (ordered alphabetically by `refname_pattern`) and rejects the update if any of them blocks the operation in question. When multiple rules match a single ref update, ANY rule that blocks the operation rejects (not strict precedence by order). The `matched_pattern` field in the rejection error / audit event is the first alphabetically-matching rule that triggered the rejection. Operators alerting on `matched_pattern` should account for this lexicographic ordering (e.g., `*` (0x2A) sorts before `m` (0x6D), so `refs/heads/*` appears before `refs/heads/main`).

A relaxing rule (`--allow-force-push` set, `--allow-deletion` set) does **not** unblock another rule that still blocks the same operation. The model is monotone-restrictive: more rules can only add restrictions on the same `(tenant, repo)`, never remove them.

If this is too restrictive for a deployment, remove the more permissive pattern and re-add the strict one, or wait for a future per-rule "allow" semantic (Tier 2 candidate).

---

## 4. Enforcement model

The integration point is `internal/gitproto/receivepack/complete.go` step 8b. It runs after IndexPackStrict (step 7) — the pack and any new commits are already in the bare — but before BuildAndCommit (step 10). The decision is therefore made against bare state that includes the inbound pack.

### 4.1 What step 8b does

For each ref update in the receive-pack command list whose status is still empty (not pre-rejected by anti-shallow, ref-namespace, or atomic-precheck logic):

1. Call `Service.CheckUpdate(ctx, tenant, repo, bareDir, refname, old, new)`.
2. If the result is `nil`, emit `policy_refs_check_total{outcome=ok}` and continue.
3. If the result is `*PolicyError`, set the slot's status to `ng <refname> protected-branch: <reason> by pattern <pattern> (refname=...)`, emit `policy_refs_check_total{outcome=blocked_*}` and a `policy.ref.rejected` audit event, and continue.
4. If the result is any other error (sqlite read failure, `git merge-base` subprocess failure, malformed pattern bypassing the Add-time guard), set the status to `ng <refname> internal-error: <message>` and emit `policy_refs_check_total{outcome=internal_error}`.

Note: this is the **only** point where `policy.ref.rejected` fires. Accepted pushes are not double-audited — the existing receive-pack audit event covers them.

### 4.2 Opt-out

`EngineRequest.Policy` is `*policy.Service`. When `nil`, step 8b is skipped entirely — no enforcement, no metric emission for the policy family. This matches pre-M14 behavior bit-for-bit.

The bundled CLI wires `policy.Service` unconditionally whenever `--auth-db` is configured for `bucketvcs serve`. Operators who want to disable enforcement at the gateway must build their own server entry point that leaves `gateway.Options.Policy` unset.

### 4.3 Atomic-batch interaction

If the client sent `atomic` and at least one ref was rejected by step 8b but not all, the engine marks every remaining empty-status slot with `ng <refname> atomic-batch-failed` and short-circuits the response. No CAS happens; the bare's commits are still present but unreferenced (will be GCed). This mirrors the existing atomic-precheck handling, so no new branch was introduced.

If every ref was rejected, the engine short-circuits with the same report — nothing to commit.

### 4.4 Fail-closed posture

A `CheckUpdate` failure due to authdb unreachability, lock contention, or `git merge-base` failure is treated as `internal_error` and **rejects the push**. This is deliberate. A policy lookup that fails CANNOT silently allow a write to a protected branch — the contrast is M13.5 quota, where verify-path lookup failures degrade-open because the client has already uploaded. Step 8b is pre-commit; failing closed is the correct posture.

Operators alert on `policy_refs_check_total{outcome=internal_error}` and investigate authdb health.

---

## 5. Observability

### 5.1 Metric: `policy_refs_check_total{outcome}`

One emission per ref-update slot that step 8b processes. Outcomes:

- `ok` — no matching rule blocked the update.
- `blocked_deletion` — at least one matching rule had `block_deletion=true` and the update was a deletion (`new_oid == 0…0`).
- `blocked_force_push` — at least one matching rule had `block_force_push=true` and `merge-base --is-ancestor` returned non-zero.
- `internal_error` — `CheckUpdate` returned an error other than `*PolicyError`.

Example slog text emission (default handler):

```
2026/05/22 08:38:23 INFO metric metric_name=policy_refs_check_total value=1 outcome=blocked_force_push
```

JSON handler:

```json
{"time":"2026-05-22T08:38:23Z","level":"INFO","msg":"metric","metric_name":"policy_refs_check_total","value":1,"outcome":"blocked_force_push"}
```

### 5.2 Audit event: `policy.ref.rejected`

Fires only on rejection (any of the `blocked_*` outcomes).

Attrs:

- `event` — always `"policy.ref.rejected"`.
- `tenant`, `repo` — separate fields; not joined with `/` so grep / structured-log queries are simpler.
- `refname` — the rejected ref.
- `matched_pattern` — the first matching blocking rule's pattern.
- `reason` — `"deletion blocked"` or `"non-fast-forward push blocked"`.
- `actor` — actor name from the engine request (e.g. token user or SSH key owner). Empty/anonymous becomes the literal string `anonymous`.
- `old_oid`, `new_oid` — the rejected update's OIDs. `new_oid` is the 40-zero hex on deletion.

Example slog text emission:

```
2026/05/22 08:38:23 INFO policy.ref.rejected event=policy.ref.rejected tenant=acme repo=site refname=refs/heads/main matched_pattern=refs/heads/main reason="non-fast-forward push blocked" actor=alice old_oid=3c7f982342e3389d7e6b473c14f466293951a9f1 new_oid=2130f458f1a76e010f17159f680d346316e7fd41
```

### 5.2.1 Audit event: `policy.ref.internal_error`

Fires when step 8b `CheckUpdate` returns a non-`PolicyError` — that is, when the policy lookup itself failed (sqlite read error, `merge-base` subprocess failure). The push is rejected (fail-closed), so operators reading the audit log alone get a single trail for **all** step 8b rejections, not just policy decisions.

Attrs:

- `event` — always `"policy.ref.internal_error"`.
- `tenant`, `repo` — separate fields.
- `refname` — the rejected ref.
- `actor` — actor name from the engine request. Empty/anonymous becomes the literal string `anonymous`.
- `error` — the underlying error message (sqlite, exec, etc.).

Triggers: step 8b `CheckUpdate` non-`PolicyError` (sqlite read, `merge-base` subprocess). Pair with the `policy_refs_check_total{outcome=internal_error}` metric alert in §5.3.

### 5.3 Recommended alerts

- `sum(rate(policy_refs_check_total{outcome=~"blocked_.*"}[5m])) > 0` for a sustained window suggests either an operator who forgot the rule exists, or a CI client retrying a doomed force-push. Surface this.
- `sum(rate(policy_refs_check_total{outcome="internal_error"}[5m])) > 0` is **always** worth paging on — it indicates an authdb fault and the gateway is now rejecting protected-ref pushes that would have been accepted.
- Repeated `policy.ref.rejected` events with the same `(actor, refname)` within a short window indicate a confused user; manual outreach saves a support ticket.

---

## 6. Failure modes

| Symptom | Cause | Action |
|---|---|---|
| `protected-branch: deletion blocked by pattern <PAT>` | Rule with `block_deletion=true` matched the deletion. | Expected. Remove (`policy refs remove`) or relax (`policy refs add … --allow-deletion`). |
| `protected-branch: non-fast-forward push blocked by pattern <PAT>` | Rule with `block_force_push=true` matched a non-FF update. | Expected. Developer must rebase, merge, or open a PR. |
| `protected-branch: … by pattern <PAT>` but PAT seems wrong | `*` does not cross `/`; `**` is not supported; literal vs full-ref form mismatch. | `policy refs list` to inspect actual stored patterns. Re-add with a more specific pattern (e.g. `refs/heads/release/*` not `refs/heads/release/**`). |
| `internal-error: merge-base failed: …` | Local bare corrupted, or OID not reachable. | Investigate bare integrity; consider `bucketvcs maintenance --force-rebuild` (M9). |
| `internal-error: …sqlite…` | authdb read failed (disk full, lock contention, file moved). | Failing closed is intentional. Investigate authdb. Alert on `outcome=internal_error` for early warning. |
| All pushes succeed despite a rule | Operator stored `main` instead of `refs/heads/main`. | `policy refs list` shows the stored pattern. Re-add with the full ref form. |
| Rule appears in `list` but isn't enforced | Gateway built with `EngineRequest.Policy = nil`, or running against a different authdb than the CLI wrote to. | Confirm `bucketvcs serve --auth-db=…` points at the same path as the `policy` CLI. |

---

## 7. Operator recipes

These assume `AUTHDB=/var/lib/bucketvcs/auth.db` and a repo at `acme/site`.

### 7.1 Protect `main` from deletion + force-push (default)

```bash
bucketvcs policy refs add \
    --auth-db="$AUTHDB" \
    --tenant=acme --repo=site \
    --pattern=refs/heads/main
```

A FF push to `main` succeeds; force-push and deletion are rejected.

### 7.2 Protect all release branches but allow CI to force-push

Stdlib `path.Match` doesn't recurse, so each level needs its own rule.

```bash
bucketvcs policy refs add \
    --auth-db="$AUTHDB" --tenant=acme --repo=site \
    --pattern=refs/heads/release/* \
    --allow-force-push
```

Deletion is still blocked. CI's force-push to e.g. `refs/heads/release/v3` succeeds; a `git push :refs/heads/release/v3` is rejected.

If "force-push allowed but deletion blocked" should NOT apply to every nested namespace, add narrower rules — Tier 1 has no `**`.

### 7.3 Block force-push to tags

```bash
bucketvcs policy refs add \
    --auth-db="$AUTHDB" --tenant=acme --repo=site \
    --pattern=refs/tags/* \
    --allow-deletion
```

Tags can be deleted (some workflows tag-and-retag), but force-update is blocked. Note `*` covers `refs/tags/v1` but not `refs/tags/sub/v1` — add additional rules for nested tag namespaces.

### 7.4 Allow force-push to feature branches but block deletion

The simplest model: do **not** add a rule for feature branches. Tier 1 protects nothing by default. If you want to express "I have considered this branch and decided to leave it open," that becomes documentation, not a rule.

If you really need a no-op marker rule, set both `--allow-deletion` and `--allow-force-push`:

```bash
bucketvcs policy refs add \
    --auth-db="$AUTHDB" --tenant=acme --repo=site \
    --pattern=refs/heads/feature/* \
    --allow-deletion --allow-force-push
```

`policy refs list` will show it. CheckUpdate will short-circuit to `outcome=ok` for every matching update.

### 7.5 Inspect rules for a repo

```bash
bucketvcs policy refs list \
    --auth-db="$AUTHDB" --tenant=acme --repo=site

# Or for tooling:
bucketvcs policy refs list \
    --auth-db="$AUTHDB" --tenant=acme --repo=site \
    --format=json | jq .
```

### 7.6 Remove a rule

```bash
bucketvcs policy refs remove \
    --auth-db="$AUTHDB" --tenant=acme --repo=site \
    --pattern=refs/heads/main
```

Pattern is matched as a literal string; mismatches are no-ops with exit code 0.

---

## 8. Migration / opt-out

### 8.1 Schema

`0005_protected_refs.sql` adds the `protected_refs` table with CHECK constraints, bumping schema_version 4 → 5. The migration is forward-only and applied automatically by `RunMigrations` on the next `bucketvcs serve` (or `bucketvcs policy refs *`) startup.

### 8.2 Compatibility

- A pre-M14 gateway running against an M14-migrated authdb works unchanged — it doesn't query `protected_refs`.
- An M14 gateway running against a pre-M14 authdb migrates 4 → 5 in-place on startup.
- An M14 gateway with `EngineRequest.Policy = nil` (operator-built; `bucketvcs serve` always wires `policy.Service`) behaves exactly like pre-M14: no enforcement, no metrics, no audit events for the policy family.

### 8.3 Disabling per deployment

The bundled `bucketvcs serve` constructs `policy.New(authS.DB())` whenever `--auth-db` is in play. The only way to disable Tier 1 enforcement today is to:

1. Build a custom server entrypoint that leaves `gateway.Options.Policy` unset, or
2. Remove every row from the `protected_refs` table — `CheckUpdate` short-circuits when `List` returns zero rows, so the enforcement path becomes a single sqlite SELECT per push (no `merge-base` shell-out, no audit event).

Option 2 is the recommended approach for ops who want the M14 binary but no enforcement.

---

## 9. Deferred work

The MVP shipped Tier 1 only. The following items are explicitly scoped out:

- **Tier 2 rule families** — file-size limits, path restrictions, commit-author email regex, commit-message regex, signed-commit verification. Each is a new policy type under the same `internal/policy` package + a new CLI subcommand.
- **HTTP webhook hooks** — shipped in M15; see the M15 operator guide.
- **Post-receive infrastructure** — natural extension point for webhooks; nothing in Tier 1 needs it.
- **Tenant-level default rules** — auto-apply a default ruleset to all repos under a tenant.
- **`block_create` toggle** — gates new ref creation; depends on identity to be useful.
- **Recursive glob patterns** (`**`) — `path.Match` doesn't support; not worth a dependency for Tier 1.
- **Bulk rule operations** — `policy refs apply --file=rules.json` for ops-as-code workflows.
- **Audit on accept** (`policy.ref.allowed`) — would double the audit volume; the existing receive-pack audit already covers accepted pushes.
- **Per-rule actor allowlist** — "block force-push to main UNLESS actor in [...]"; requires identity gating (RBAC, deploy-key role tags, etc.).
- **Pattern semantics richer than `path.Match`** — glob extensions like `{a,b,c}` alternation, leading `!` negation, etc.

---

## 11. Tier 3 — custom hooks (M20)

Tier 3 ships in M20: per-`(tenant, repo, trigger)` shell-script hooks for `pre-receive` (gate the push) and `post-receive` (run after a successful push). Scripts execute in a `bwrap` namespace sandbox with read-only `/repo`, no network by default, no `$HOOME`, no `/etc`, and a fresh `/tmp`. The design spec is `docs/superpowers/specs/2026-05-23-m20-hooks-tier3-design.md`; the schema is migration `0009_hooks.sql`.

### 11.1 Install `bwrap`

Sandboxed mode is the default. The gateway requires the [`bubblewrap`](https://github.com/containers/bubblewrap) binary on `PATH`:

```bash
# Debian / Ubuntu
sudo apt install bubblewrap

# Fedora / RHEL / Rocky / Alma
sudo dnf install bubblewrap

# Alpine
sudo apk add bubblewrap

# Arch
sudo pacman -S bubblewrap

# macOS / Windows-native
# bwrap is Linux-only. See §11.8 for the --hooks-unsafe-no-sandbox escape hatch.
```

Bubblewrap **0.12 or newer** is required because the runner passes `--rlimit-cpu` and `--rlimit-as`. Older 0.11.x packages will fail with `bwrap: Unknown option --rlimit-cpu`; upgrade or switch to `--hooks-unsafe-no-sandbox=true`.

### 11.2 Hook layout under `--hooks-root`

A single flat directory containing one executable file per hook script:

```
/var/lib/bucketvcs/hooks/
├── reject-secrets.sh          # chmod +x; referenced by --script=reject-secrets.sh
├── enforce-ticket-link.sh
├── notify-slack.sh
└── audit-to-syslog.sh
```

Rules:

- Filenames must match `[A-Za-z0-9._-]+` (no slashes, no spaces). The CLI rejects invalid names at registration time.
- Each script needs the executable bit (`chmod +x`). The runner does not `chmod`.
- One file per hook. Bash, POSIX shell, Python, compiled binaries — anything with a valid shebang works. `/usr/bin/python3` and friends are mounted into the sandbox by default.

### 11.3 Register a hook

```bash
# Pre-receive hook: reject commits that touch secrets/
bucketvcs policy hooks add \
    --auth-db=/var/lib/bucketvcs/auth.db \
    --tenant=acme --repo=site \
    --trigger=pre-receive \
    --script=reject-secrets.sh

# Post-receive hook: notify Slack on push (runs async, doesn't block)
bucketvcs policy hooks add \
    --auth-db=/var/lib/bucketvcs/auth.db \
    --tenant=acme --repo=site \
    --trigger=post-receive \
    --script=notify-slack.sh \
    --order=10
```

`--order` controls execution sequence (ascending) when multiple hooks are registered for the same trigger. Use `bucketvcs policy hooks list`, `... remove`, `... disable`, `... enable` to manage the row. A disabled hook stays in the DB but skips execution; this is the recommended "feature flag" path.

### 11.4 Hook stdin contract

**pre-receive** receives the native git pre-receive format, one ref update per line:

```
<oldoid> <newoid> <refname>
<oldoid> <newoid> <refname>
...
```

`<oldoid>` is 40 zeros for ref creation; `<newoid>` is 40 zeros for ref deletion. Existing pre-receive scripts written for stock git work unchanged.

**post-receive** receives the same native lines followed by a blank line and a JSON envelope on the last logical block:

```
<oldoid> <newoid> <refname>
\n
{"tenant":"acme","repo":"site","push_id":"…","actor":"alice","tx_id":"…","manifest_version":42,"storage_backend":"localfs","updates":[…]}
```

Native git scripts that read until EOF and ignore the JSON envelope still work; M20-aware scripts can `awk` past the blank line for the richer payload.

### 11.5 Environment variables

Every hook (pre and post) inherits:

| Variable | Description |
|---|---|
| `BUCKETVCS_TENANT` | Tenant ID, e.g. `acme` |
| `BUCKETVCS_REPO` | Repo ID, e.g. `site` |
| `BUCKETVCS_TRIGGER` | `pre-receive` or `post-receive` |
| `BUCKETVCS_PUSH_ID` | Per-push UUID, correlates pre+post |
| `BUCKETVCS_ACTOR` | Authenticated user (empty for anonymous) |

post-receive hooks additionally see:

| Variable | Description |
|---|---|
| `BUCKETVCS_TX_ID` | Manifest commit transaction ID |
| `BUCKETVCS_STORAGE_BACKEND` | Storage backend name (e.g. `localfs`, `s3compat`) |

Custom env entries supplied via `--hooks-env=KEY=VALUE,KEY2=VALUE2` on `bucketvcs serve` are merged on top.

### 11.6 Sandbox guarantees

Default sandbox (`bwrap` enabled):

- `/repo` is a **read-only bind mount** of the canonical bare repo. Scripts can `git --git-dir=/repo ...` to read history but cannot mutate refs or objects.
- `/usr`, `/lib`, `/lib64`, `/bin` are read-only mounts — system binaries work.
- `/tmp` is a fresh tmpfs per invocation; nothing leaks between hooks or between pushes.
- `/proc` and `/dev` are minimal namespace mounts.
- **No `$HOME`, no `/etc`** — scripts cannot read user credentials or system config from the host.
- **No network**: `--unshare-net` is unconditional unless the script appears in `--hooks-allow-network=<comma-list>`. Use that opt-in for hooks that must talk to Slack, PagerDuty, JIRA, etc.

### 11.7 Resource limits

Four caps apply per hook invocation:

| Flag | Default | Meaning |
|---|---|---|
| `--hooks-cpu-sec` | 10 | `RLIMIT_CPU` — CPU-seconds budget |
| `--hooks-memory-mb` | 256 | `RLIMIT_AS` — virtual memory cap (MiB) |
| `--hooks-timeout-sec` | 30 | Wall-clock timeout (kills the process group) |
| `--hooks-output-max-kb` | 64 | Combined stdout+stderr cap; excess is dropped |

Hitting a CPU/memory limit makes the kernel kill the script (non-zero exit → pre-receive rejects the push or post-receive logs an error). Hitting the wall-clock timeout sends `SIGTERM`, waits 1s, then `SIGKILL`s the whole process group.

### 11.8 macOS / Windows-native: `--hooks-unsafe-no-sandbox`

`bwrap` is Linux-only. On macOS, Windows, or any host where bwrap isn't viable, set `--hooks-unsafe-no-sandbox=true`. The gateway emits an `ERROR` log on startup:

```
hooks: running without sandbox; NOT multi-tenant safe; for single-tenant local development only
```

In this mode hook scripts run with the same uid/gid as `bucketvcs serve`, with full filesystem access. Use this **only** for single-tenant dev hosts. Production multi-tenant deployments must run Linux with `bwrap >= 0.12`.

### 11.9 Failure modes

| Scenario | Push outcome | Operator-visible signal |
|---|---|---|
| pre-receive exit 0 | accepted | metric `hooks_pre_receive_total{outcome=accepted}` |
| pre-receive exit ≠ 0 | rejected; stderr (capped) sent to git client | audit `policy.hook.rejected`, metric `hooks_pre_receive_total{outcome=rejected}` |
| pre-receive timeout | rejected | audit `policy.hook.internal_error`, metric `hooks_pre_receive_total{outcome=error}` |
| pre-receive script missing / chmod | rejected (default) or allowed (with `--hooks-on-internal-error=allow`) | audit `policy.hook.internal_error` |
| post-receive exit 0 | (already accepted) | metric `hooks_post_receive_total{outcome=ok}` |
| post-receive exit ≠ 0 / timeout | (already accepted) | audit `policy.hook.rejected` at WARN, metric `hooks_post_receive_total{outcome=rejected,error}` — push does NOT roll back |

Critical distinction: **post-receive failures cannot abort the push**. The push has already committed (manifest is written, sentinel is updated). Use pre-receive for guard rails; reserve post-receive for notifications, analytics, mirroring.

### 11.10 Worked example: enforce a ticket link in every commit

`/var/lib/bucketvcs/hooks/enforce-ticket-link.sh`:

```sh
#!/bin/sh
# Reject any push that includes a commit whose message lacks a JIRA-style
# ticket reference. The git binary is provided by the sandbox; /repo is the
# read-only canonical bare.
exit_code=0
while read -r oldoid newoid refname; do
    # Skip deletions.
    [ "$newoid" = "0000000000000000000000000000000000000000" ] && continue
    if [ "$oldoid" = "0000000000000000000000000000000000000000" ]; then
        # New branch — check every reachable commit.
        range="$newoid"
    else
        range="${oldoid}..${newoid}"
    fi
    git --git-dir=/repo log --format=%B "$range" | while read -r line; do
        # PROJ-1234 anywhere in the commit message body satisfies the rule.
        if echo "$line" | grep -Eq "[A-Z]{2,}-[0-9]+"; then
            continue
        fi
    done || exit_code=1
done
if [ "$exit_code" -ne 0 ]; then
    echo "rejected: every commit must reference a JIRA ticket (e.g. PROJ-1234) in its message" >&2
    exit 1
fi
exit 0
```

Register:

```bash
bucketvcs policy hooks add \
    --auth-db=/var/lib/bucketvcs/auth.db \
    --tenant=acme --repo=site \
    --trigger=pre-receive \
    --script=enforce-ticket-link.sh
```

A push with a non-conforming commit is rejected client-side with the script's stderr embedded in the git error.

---

## 12. FAQ

### Q: Why doesn't `*` cross `/`?

The patterns use Go's stdlib `path.Match`. Its design treats path-segment boundaries as significant — `*` matches within a segment only. If you need recursive coverage, add multiple rules (`refs/heads/release/*`, `refs/heads/release/*/*`, etc.). A future pattern engine could add `**`; the cost-benefit didn't justify it for Tier 1.

### Q: What happens if the authdb is unreachable mid-push?

Step 8b's `CheckUpdate` returns a non-`*PolicyError` error. The receive-pack engine sets the slot status to `ng <refname> internal-error: <message>` and emits `policy_refs_check_total{outcome=internal_error}`. The push fails. This is **deliberately strict** — a policy lookup that fails cannot silently allow a write to a protected branch. Alert on `outcome=internal_error` for early warning.

If you want degrade-open behavior, you'd have to fork the engine or remove the rule rows during the outage. We don't recommend either; investigate authdb health instead.

### Q: Can I have different rules per branch type?

Yes — add multiple rules. Each `(tenant, repo, pattern)` triple is independent. The engine walks all matching rules and applies the union of "block" semantics: any blocking rule rejects.

### Q: Do my rules apply to existing pushes already in-flight?

No. A push that began before the rule was added sees the pre-rule state (its read happens inside its own per-repo write lock). The next push sees the new rule. There's no retroactive enforcement — this matches industry-standard hosted-Git semantics.

### Q: Why does the rejection message look so verbose?

The `ng` line surfaces three pieces of information the developer needs in one string: the reason, the matched pattern (so they can confirm WHICH rule fired), and the refname (for atomic-batch contexts where multiple refs appear in the report). For example:

```
remote: protected-branch: non-fast-forward push blocked by pattern refs/heads/main (refname=refs/heads/main)
```

If you wrap your CLI's output, this stays on one line because protocol-v2 report-status lines are pkt-line bounded.

### Q: How do I undo a rule I just added?

`bucketvcs policy refs remove --auth-db=… --tenant=… --repo=… --pattern=<exact-glob>`. The remove is idempotent — running it twice or against a non-existent rule exits 0.

### Q: Can I see who's been pushing to protected refs?

`policy.ref.rejected` events carry `actor`. Accepted pushes don't emit a policy event (the existing receive-pack audit covers them with all accepted refs in one event). For a full history of WHO pushed WHAT to a protected ref, you need both event types.

### Q: Does this work over SSH?

Yes. Both the HTTP gateway (`internal/gateway/receive_pack.go`) and the SSH listener (`internal/sshd/session.go`) construct the same `receivepack.EngineRequest` shape with the same `Policy` field. The CLI wires both transports from one `*policy.Service` instance.

### Q: Are protected-refs rules backed up with the repo?

No — they live in `auth.db`, not in the bucket. Operator-side backups should include the authdb (it already holds users, tokens, grants, SSH keys, LFS locks, and quotas; M14 adds the protected_refs table to that list).
