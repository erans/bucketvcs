# M20: Tier 3 custom hooks (subprocess pre-receive + post-receive)

**Status:** Design.
**Date:** 2026-05-24.
**Scope:** Close the Tier 3 deferral carried by M14 and M16 (custom shell-script hooks). Operator-defined pre-receive scripts (sync, can reject) and post-receive scripts (async, fire-and-forget) executed under bwrap sandbox on Linux. Builds on M14 (`policy.Service`), M16 (path policy), and M15 (worker pattern). Closes the imperative escape hatch operators need for CI integration, ticket-link enforcement, custom signed-commit checks, secret scanning, and language-specific lint gates.

## 1. Goals

### 1.1 In scope

- Two trigger points: `pre-receive` (sync, blocks push, native git contract) and `post-receive` (async, fire-and-forget).
- Per-(tenant, repo) hook registration via new `hooks` table (migration 0009) mirroring the M14 `protected_refs` + M16 `protected_paths` shape.
- Multiple hooks per (tenant, repo, trigger), ordered by integer `sort_order`. Sequential execution; pre-receive fails-fast on first non-zero exit.
- Filesystem-resident script files under operator-configured `--hooks-root`. sqlite stores only metadata (script name, order, enabled flag). `script_name` constrained to `routenames.ValidateName` charset (no `..`, no `/`) — defense against path traversal.
- `bucketvcs policy hooks add|list|remove|enable|disable` CLI mirroring M14/M16's `policy refs|paths` shape.
- Bubblewrap sandbox on Linux: mount + PID + network + IPC namespaces, read-only bare-repo mount, no `$HOME`/`/etc`/`/var`, fresh `/tmp`, no network unless `--hooks-allow-network=<script-list>` opts a script in.
- Resource caps via Go `syscall.Setrlimit` in `exec.Cmd.SysProcAttr` plus `exec.CommandContext` wall-clock timeout: `--hooks-cpu-sec=10`, `--hooks-memory-mb=256`, `--hooks-timeout-sec=30`, `--hooks-output-max-kb=64`.
- `--hooks-unsafe-no-sandbox=true` opt-in for macOS / Windows-native / any platform without bwrap; runs subprocess with rlimits + clean env + timeout but no namespaces. ERROR-level startup log; default `false`; never the production path.
- Pre-receive stdin: native git format (`<oldoid> <newoid> <refname>\n` per ref update). Post-receive stdin: native lines + a blank-line separator + a JSON object carrying the full `webhooks.PushPayload` (TxID, ManifestVersion, storage_backend, commits_summary, actor) for bucketvcs-aware scripts.
- Pre-receive stderr (capped at 64 KB) flows back to the git client over sideband on rejection; the M14-style `policy.hook.rejected` audit event captures the same.
- In-memory bounded worker pool for post-receive (default concurrency=8, queue cap=256). Channel-full drops the job with a metric + WARN — durable post-receive needs M15 webhooks instead.
- 5 metrics + 3-4 audit events; smoke + integration tests including a cross-tenant containment probe.

### 1.2 Out of scope (deferred)

- **update trigger.** Native git's per-ref hook between pre-receive and refUpdates-apply. pre-receive already covers what most update hooks want; defer.
- **Built-in named checks** (`require-signed-commits`, `require-conventional-commit`). Useful but deferrable — operators can write a 10-line shell wrapper today. Candidate for M21.
- **Per-hook resource cap overrides.** All hooks use serve-level defaults today. Per-row `timeout_sec` / `memory_mb` columns can be added later if operators ask.
- **Hook scripts in the bare repo itself** (e.g. `.bucketvcs-hooks/pre-receive.d/*`). All scripts live under operator-controlled `--hooks-root`; tenants cannot inject hooks via push.
- **Subdirectories under `--hooks-root`.** MVP rejects `script_name` containing `/`; flat layout only.
- **Global / per-tenant hooks** (apply to all repos in tenant or all repos in cluster). Per-(tenant, repo) only; matches M14/M16.
- **Hook scripts that take command-line args.** stdin + env vars only.
- **Concurrent hook execution within one push.** Sequential always; the natural ordering is the only model that maps onto fail-fast semantics.
- **Durable post-receive queue + retry.** Operators with critical post-receive needs use M15 webhooks.
- **Hook script bundling / packaging.** No `bucketvcs hook upload` CLI — scripts deployed via the operator's normal config-management story (Ansible, Salt, etc.).
- **Cross-tenant hook sharing.** Each (tenant, repo) registers its own hooks; if a tenant runs the same script on every repo, that's the operator's automation problem.

## 2. Architecture

```
internal/hooks/
  service.go        — Service{store, runner, logger}; RunPreReceive + EnqueuePostReceive
  store.go          — sqlite CRUD over the hooks table
  runner.go         — subprocess execution: bwrap argv builder, rlimits, stdin marshaling
  payload.go        — PreReceivePayload + post-receive JSON envelope
  errors.go         — HookRejection + sentinels (ErrScriptNotFound, ErrTimeout, ErrInternal)
  worker.go         — postReceiveJob channel + worker pool
  metrics.go        — slog metric emitters
  audit.go          — policy.hook.* event emitters
  *_test.go

internal/auth/sqlitestore/migrations/
  0009_hooks.sql    — table + index + schema_version bump

internal/gitproto/receivepack/
  complete.go       — Step 8c (pre-receive) at L268, post-receive enqueue at L446

internal/gateway/server.go
internal/sshd/server.go
internal/gitproto/uploadpack/engine.go (EngineRequest.Hooks field; nil = opt-out)

cmd/bucketvcs/
  policy.go         — adds 'hooks' subcommand group alongside 'refs' and 'paths'
  serve.go          — adds --hooks-enabled, --hooks-root, --hooks-cpu-sec, --hooks-memory-mb,
                      --hooks-timeout-sec, --hooks-output-max-kb, --hooks-allow-network,
                      --hooks-postreceive-concurrency, --hooks-postreceive-queue,
                      --hooks-on-internal-error, --hooks-unsafe-no-sandbox

scripts/m20-hooks-smoke.sh   — end-to-end smoke
docs/m14-hooks-policy-operator-guide.md  — extend (M14 already has the operator guide; M20
                                            adds a Tier 3 section + bwrap install instructions)
```

**Wiring (HTTPS + SSH):** Both transports construct `hooks.Service` once at startup (when `--hooks-enabled=true`) and pass it into `EngineRequest.Hooks`. `cmd/bucketvcs/serve.go` does the wiring; the service is shared across both transports.

**Lifecycle:**
1. `serve` startup: validate bwrap exists (Linux) OR `--hooks-unsafe-no-sandbox=true`. Validate `--hooks-root` is a real directory. Construct `hooks.Service`. Start worker goroutines.
2. Push arrives → receivepack runs through Step 8b (M14/M16 policy) → Step 8c (this milestone): if `eng.Hooks != nil`, look up active hooks for (tenant, repo, "pre-receive") ordered by `sort_order`; run sequentially; first non-zero exit aborts the push.
3. On accept, pipeline runs Steps 9–10 (apply refs, BuildAndCommit) → Step 14 (markMirrorStale) → enqueue post-receive job for this push (`eng.Hooks.EnqueuePostReceive(payload)`).
4. Worker dequeues, runs each post-receive hook for (tenant, repo) sequentially, ignores exit codes (best-effort), logs failures.
5. On serve shutdown: close worker channel; bounded wait for in-flight goroutines.

## 3. Schema

`internal/auth/sqlitestore/migrations/0009_hooks.sql`:

```sql
CREATE TABLE hooks (
    tenant       TEXT NOT NULL,
    repo         TEXT NOT NULL,
    trigger      TEXT NOT NULL CHECK (trigger IN ('pre-receive', 'post-receive')),
    script_name  TEXT NOT NULL,
    sort_order   INTEGER NOT NULL DEFAULT 0,
    enabled      INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (tenant, repo, trigger, script_name),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);
CREATE INDEX hooks_lookup ON hooks(tenant, repo, trigger, enabled, sort_order);

INSERT INTO schema_version (version) VALUES (9);
```

- Composite PK on `(tenant, repo, trigger, script_name)` makes `INSERT...ON CONFLICT DO UPDATE` idempotent for re-adds (matches M14 `add` semantics).
- `enabled=0` rows stay registered but are skipped by the runtime lookup. CLI `enable`/`disable` flips this column.
- FK cascade ensures repo deletion (M15.1) sweeps hook rows.
- `script_name` validated by `routenames.ValidateName` (charset `[A-Za-z0-9._-]+`) at insert time; runtime re-validates as defense-in-depth.

## 4. CLI surface

```
bucketvcs policy hooks add     --tenant=acme --repo=site \
                                --trigger=pre-receive --script=secrets-scan.sh \
                                [--order=10] [--actor=admin]

bucketvcs policy hooks list    --tenant=acme --repo=site
  # NDJSON: one object per row: {tenant, repo, trigger, script_name, sort_order, enabled, created_at, updated_at}
  # --limit defaults to no limit; --trigger filters to one trigger.

bucketvcs policy hooks remove  --tenant=acme --repo=site \
                                --trigger=pre-receive --script=secrets-scan.sh

bucketvcs policy hooks enable  --tenant=acme --repo=site \
                                --trigger=pre-receive --script=secrets-scan.sh

bucketvcs policy hooks disable --tenant=acme --repo=site \
                                --trigger=pre-receive --script=secrets-scan.sh
```

All five subcommands fail with exit 2 if `--tenant`/`--repo`/`--trigger`/`--script` are missing. `add` validates `script_name` charset client-side (early error) and also re-validates server-side. `--actor` follows the M15.1 fs.Func closure pattern (distinguishes "not passed" from `--actor=`).

## 5. Sandbox + execution contract

### 5.1 bwrap argv (Linux default)

```
bwrap \
  --die-with-parent \
  --unshare-all \
  --ro-bind <bareDir> /repo \
  --ro-bind /usr /usr --ro-bind /lib /lib --ro-bind /lib64 /lib64 --ro-bind /bin /bin \
  --tmpfs /tmp \
  --tmpfs /run \
  --proc /proc \
  --dev /dev \
  --chdir /repo \
  --setenv PATH /usr/bin:/bin \
  --setenv BUCKETVCS_TENANT <tenant> \
  --setenv BUCKETVCS_REPO <repo> \
  --setenv BUCKETVCS_TRIGGER <pre-receive|post-receive> \
  --setenv BUCKETVCS_PUSH_ID <uuid> \
  --setenv BUCKETVCS_HOOK_SCRIPT <script_name> \
  -- <hooks-root>/<script_name>
```

Per-script additions:
- `--share-net` appended when `script_name ∈ --hooks-allow-network` set.
- Additional `--setenv KEY VALUE` entries from `--hooks-env KEY=VALUE` flag (operator-wide; passed to every hook).

### 5.2 Resource caps (both modes)

Applied via `exec.Cmd.SysProcAttr.AmbientCaps`-style pre-exec hook calling `syscall.Setrlimit`:
- `RLIMIT_CPU = --hooks-cpu-sec` (default 10s)
- `RLIMIT_AS  = --hooks-memory-mb * 1024 * 1024` (default 256 MB)
- `RLIMIT_FSIZE = 0` (hooks can't write large files; tmpfs is the only writable surface)
- Wall-clock timeout via `exec.CommandContext(parentCtx, ...)` with a derived ctx that cancels at `--hooks-timeout-sec` (default 30s).
- SIGKILL grace: 1 second after SIGTERM if subprocess hasn't exited.

### 5.3 Stdin / stdout / stderr

- **stdin:** built in-process before exec, fed via `cmd.Stdin = bytes.NewReader(...)`.
  - Pre-receive: native git format only.
  - Post-receive: native git lines + blank line + JSON-encoded `webhooks.PushPayload`.
- **stdout:** captured into a 64 KB-capped ring buffer; logged to slog DEBUG on success, INFO on failure. Not surfaced to client.
- **stderr:** same 64 KB cap. On pre-receive non-zero exit, prepended with `hook "<script>" (exit N):` and sent to client via sideband. Always captured to slog WARN.

**Bare-repo state at pre-receive time:** Step 7 (`IndexPackStrict`) has already placed the inbound pack in the bare, so `git cat-file -p <newoid>` against any object the client pushed will succeed inside the sandbox. But Step 9b (`applyRefUpdateInBare`) has NOT yet run — so `refs/heads/<branch>` still points at the OLD tip. Hooks that want to inspect the proposed history must use the new OIDs from stdin (e.g. `git log <oldoid>..<newoid>`), NOT the ref names. This matches native git pre-receive semantics exactly.

**Bare-repo state at post-receive time:** Steps 9b + 10 have committed; refs reflect the new state. `git log refs/heads/<branch>` shows the new tip.

### 5.4 Exit code semantics

| Trigger | Exit 0 | Non-zero | Killed (timeout/OOM) |
|---|---|---|---|
| pre-receive | next hook runs; eventually pipeline proceeds | abort push, return stderr to client | abort push, error message "hook timed out" or "hook OOM-killed" |
| post-receive | next hook runs | next hook still runs; WARN + metric | WARN + metric |

### 5.5 Internal errors (bwrap missing, exec failed, fork failed, etc.)

- **Pre-receive (default)**: fail-closed. Push rejected with internal-error message. `policy.hook.internal_error` audit at ERROR level.
- **Pre-receive (`--hooks-on-internal-error=allow`)**: fail-open. Push proceeds as if hooks didn't exist. Same audit event still fires so operators can monitor. Intended for high-availability environments where rare sandbox setup glitches shouldn't block all pushes.
- **Post-receive**: always treated as the hook failed; WARN log; next hook still runs.

## 6. Failure modes

| Failure | Behavior |
|---|---|
| Hooks disabled (`--hooks-enabled=false`) | `eng.Hooks == nil`; pipeline runs as today. |
| No hook rows for (tenant, repo, trigger) | `RunPreReceive` returns nil immediately; no subprocess; no metric increment. |
| Hook row's script file missing on disk | Pre-receive: HookRejection ("script not found"); push rejected. Post-receive: WARN + metric + drop. |
| `script_name` charset invalid at registration | CLI rejects with exit 2 + clear error. |
| `--hooks-root` outside-of-dir traversal in `script_name` | Blocked at registration time AND at runtime (defense-in-depth). |
| bwrap missing + `--hooks-enabled=true` + not `--hooks-unsafe-no-sandbox=true` | `serve` errors at startup with actionable message. |
| Hook stdout/stderr exceeds 64 KB | Truncated; "[output truncated at 64KB]" appended; full content not recovered. |
| Hook exceeds wall timeout | SIGKILL after 1s SIGTERM grace; pre-receive rejects with "hook timed out"; non-zero exit semantics apply. |
| Hook exceeds memory rlimit | Kernel OOM-kills the subprocess; non-zero exit. |
| Subprocess setup fails (bwrap argv invalid, exec error, fork fail) | Pre-receive: fail-closed by default; `--hooks-on-internal-error=allow` flips to fail-open. Post-receive: WARN + drop. |
| Pre-receive hook exits non-zero | First failure stops the chain; remaining hooks don't run; pipeline aborts before Step 9; no commit; no post-receive. |
| Post-receive hook exits non-zero | Logged at WARN; remaining post-receive hooks still run; push is already committed. |
| Post-receive worker queue full | Job dropped; `hooks_postreceive_dropped_total` increments; WARN. |
| `--hooks-root` removed at runtime | Next push: script missing → HookRejection. No periodic re-scan. |
| Hook script not executable (no +x) | Subprocess exec fails; treated as internal error. |
| Concurrent push to same (tenant, repo) | Each push gets its own bwrap subprocess; the read-only bare mount is safe to share. |
| Tenant A's hook attempts to read tenant B's repo | bwrap mount namespace exposes only tenant A's bare; access denied at the FS layer. Integration test pins this. |
| `--hooks-unsafe-no-sandbox=true` set on Linux with bwrap available | bwrap still used. Flag is purely a fallback for systems without bwrap. |

## 7. Operator CLI flags (bucketvcs serve)

```
--hooks-enabled=false                    # opt-in; default off
--hooks-root=/var/lib/bucketvcs/hooks    # required when hooks-enabled; must be a real dir
--hooks-unsafe-no-sandbox=false          # required on platforms without bwrap; ERROR log on enable
--hooks-on-internal-error=reject         # reject | allow
--hooks-timeout-sec=30                   # wall-clock per hook
--hooks-cpu-sec=10                       # RLIMIT_CPU per hook
--hooks-memory-mb=256                    # RLIMIT_AS per hook
--hooks-output-max-kb=64                 # stdout+stderr cap per hook
--hooks-allow-network=                   # comma-separated script_name list; default empty
--hooks-env=                             # comma-separated KEY=VALUE; default empty
--hooks-postreceive-concurrency=8
--hooks-postreceive-queue=256
```

## 8. Observability

### 8.1 Metrics

```
hooks_pre_receive_total{tenant, repo, outcome=accepted|rejected|error}
hooks_pre_receive_duration_seconds{tenant, repo}    # accepted-path only
hooks_post_receive_total{tenant, repo, outcome=ok|nonzero|error|dropped}
hooks_post_receive_duration_seconds{tenant, repo}   # ok-path only
hooks_postreceive_queue_depth                       # gauge sampled every 30s
```

Cardinality bounded by number of registered repos.

### 8.2 Audit events

| Event | Level | Attrs |
|---|---|---|
| `policy.hook.rejected` | WARN | tenant, repo, trigger, script_name, exit_code, push_id, actor, stderr_truncated |
| `policy.hook.internal_error` | ERROR | tenant, repo, trigger, script_name, error, push_id |
| `policy.hook.added` | INFO | tenant, repo, trigger, script_name, sort_order, actor |
| `policy.hook.removed` / `.enabled` / `.disabled` | INFO | tenant, repo, trigger, script_name, actor |

No new event-name prefixes; reuses the M14 `policy.*` namespace.

## 9. Testing

### 9.1 Unit (internal/hooks/...)

- `Service.RunPreReceive` ordering: 3 hooks, run in `sort_order` ASC, ties broken by `script_name`
- `Service.RunPreReceive` fail-fast: 3 hooks, middle one exits non-zero, third hook never runs
- `Service.RunPreReceive` internal error fail-closed default + fail-open override
- `Service.EnqueuePostReceive` drops on full queue + emits metric
- bwrap argv builder produces the expected argv for a given (bareDir, script, env)
- stdin format: native pre-receive lines for pre-receive trigger; native + JSON for post-receive

### 9.2 Integration (real bwrap)

- Pre-receive hook that asserts `$BUCKETVCS_TENANT`/`$BUCKETVCS_REPO`/`$BUCKETVCS_TRIGGER`/`$BUCKETVCS_PUSH_ID` and reads stdin → exits 0
- Pre-receive that exits 1 with stderr "test rejection" → push rejected; client sees the message
- Pre-receive that attempts `ls /etc` → fails (no /etc mount)
- Pre-receive that attempts `curl example.com` → fails (network namespace blocks DNS)
- Pre-receive that attempts `cat /repo/HEAD` → succeeds (read-only bare mount works)
- Pre-receive that attempts `echo foo > /repo/test` → fails (read-only mount)
- Cross-tenant containment: tenant A hook can't see tenant B's bareDir (different mount namespace)

### 9.3 Smoke (`scripts/m20-hooks-smoke.sh`)

1. Create `hooks-root` with two scripts: `reject-bad-ref.sh` (rejects `refs/heads/bad`) and `audit-log.sh` (writes a marker file)
2. `bucketvcs serve --hooks-enabled --hooks-root=...`
3. Register both hooks via CLI: reject-bad-ref as pre-receive, audit-log as post-receive
4. Push `refs/heads/good` → assert accepted; assert marker file exists (post-receive ran)
5. Push `refs/heads/bad` → assert rejected; assert client stderr contains "test rejection from script"
6. Assert `policy.hook.rejected` audit in serve log
7. Disable reject-bad-ref via CLI; re-push `refs/heads/bad` → accepted
8. Echo `M20_HOOKS_SMOKE_OK`

## 10. Acceptance criteria

- 9.1 unit tests pass
- 9.2 integration tests pass (bwrap required for these)
- 9.3 smoke passes
- All prior smokes pass unmodified
- `bucketvcs policy hooks --help` shows all 5 subcommands with the documented shape
- `bucketvcs serve --help` shows all 12 new flags
- `docs/m14-hooks-policy-operator-guide.md` gains a Tier 3 section with: bwrap install (`apt install bubblewrap`), `--hooks-root` directory layout, example reject-bad-ref script, sandbox guarantees, common pitfalls (script must be `chmod +x`, etc.)
- Migration 0009 is the only schema change

## 11. Open questions

None — all decisions captured above.
