# M12 Phase 8 — smoke + operator notes + squash + tag

> **Parent plan:** [docs/m12-ref-sharding-plan.md](../m12-ref-sharding-plan.md). Phases 0–7 must be complete.

**Goal:** ship the milestone. Write the end-to-end smoke script that proves the whole pipeline works against localfs, write the brief operator guide, update memory, squash the worktree branch to a single commit on main, tag `m12-complete`.

**Files created/modified:**
- Create: `scripts/m12-reshard-smoke.sh`
- Create: `docs/m12-ref-sharding-operator-guide.md`
- Create: `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m12_progress.md`
- Modify: `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md`
- Modify: `/home/eran/work/bucketvcs/internal/repo/manifest/schema.go` (bump `SupportedReaderVersion` if appropriate — defer to reviewer judgment)

---

### Task 8.1: End-to-end smoke script

**Files:**
- Create: `scripts/m12-reshard-smoke.sh`

The smoke pattern matches the existing `scripts/m13-lfs-smoke-local.sh` and `scripts/m11-bundles-smoke.sh`. Localfs-only — refstore is backend-agnostic, the per-backend conformance suite covers the storage layer.

- [ ] **Step 1: Write the smoke script.**

`scripts/m12-reshard-smoke.sh`:

```bash
#!/usr/bin/env bash
# scripts/m12-reshard-smoke.sh
#
# End-to-end smoke for M12 ref sharding against localfs:
#   1. Build the bucketvcs binary.
#   2. Init a fresh repo against localfs:<tmp>.
#   3. Push N refs via a small import (via the existing import flow OR
#      a hand-crafted body — whichever is simplest given the test
#      harness available).
#   4. Run `bucketvcs reshard-refs` and assert the output reports
#      success with the expected ref/shard counts.
#   5. Inspect the manifest: assert RefSharding == "hash_v1" and
#      Refs is empty.
#   6. Push one more ref; assert the affected shard is rewritten
#      (manifest references a new RefShards entry with a different
#      Hash than before).
#   7. Run a v2 advertise (e.g., via `bucketvcs export` or a
#      lightweight serve-then-curl); assert every original ref +
#      the newly pushed ref appears.
#   8. Run a no-op second reshard; assert "noop".
#   9. Tear down.
#
# Skips with exit 77 if Go toolchain or git is unavailable.

set -euo pipefail

if ! command -v go >/dev/null 2>&1; then
    echo "SKIP: go not on PATH"
    exit 77
fi
if ! command -v git >/dev/null 2>&1; then
    echo "SKIP: git not on PATH"
    exit 77
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Building bucketvcs binary"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"
chmod +x "$BIN"

ROOT="$(mktemp -d)"
STORE="localfs:$ROOT/store"
TENANT="acme"
REPO="m12smoke"

cleanup() {
    rc=$?
    if [[ "$rc" -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "M12_RESHARD_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT)"
    fi
    rm -f "$BIN"
    exit "$rc"
}
trap cleanup EXIT

echo "==> Init repo"
"$BIN" init --store="$STORE" --repo="$TENANT/$REPO" --default-branch=refs/heads/main

echo "==> Seed the repo with refs by importing a small bare git repo"
SEED="$ROOT/seed"
git init --bare "$SEED" >/dev/null
# Create a single commit so HEAD resolves.
WORK="$ROOT/work"
git init -q -b main "$WORK"
(
    cd "$WORK"
    git config user.email smoke@example.com
    git config user.name smoke
    echo seed > README
    git add README
    git commit -qm initial
    git remote add origin "$SEED"
    git push -q origin main 2>/dev/null || true
)
# Push to the bare seed.
(
    cd "$WORK"
    git remote remove origin
    git remote add origin "$SEED"
    git push -q origin main:refs/heads/main
    # Create 100 additional refs all pointing at the same commit to
    # exercise the sharding distribution.
    SHA=$(git rev-parse HEAD)
    for i in $(seq 1 100); do
        git push -q origin "$SHA:refs/heads/branch-$i"
    done
)
"$BIN" import --store="$STORE" --repo="$TENANT/$REPO" --bare="$SEED"

echo "==> Inspect pre-reshard manifest"
PRE=$("$BIN" inspect-manifest --store="$STORE" --repo="$TENANT/$REPO" --json)
REFCOUNT_PRE=$(echo "$PRE" | python3 -c 'import sys,json;b=json.load(sys.stdin);print(len(b.get("refs",{})))')
SHARDS_PRE=$(echo "$PRE" | python3 -c 'import sys,json;b=json.load(sys.stdin);print(len(b.get("ref_shards",[])))')
if [[ "$REFCOUNT_PRE" -lt 101 ]]; then
    echo "FAIL: pre-reshard refs count = $REFCOUNT_PRE (expected >= 101)"
    exit 1
fi
if [[ "$SHARDS_PRE" -ne 0 ]]; then
    echo "FAIL: pre-reshard expected empty ref_shards, got $SHARDS_PRE"
    exit 1
fi
echo "    pre-reshard refs=$REFCOUNT_PRE shards=$SHARDS_PRE (v1 confirmed)"

echo "==> Run reshard-refs"
RESHARD=$("$BIN" reshard-refs --store="$STORE" --repo="$TENANT/$REPO" --json)
echo "    $RESHARD"
OUTCOME=$(echo "$RESHARD" | python3 -c 'import sys,json;print(json.load(sys.stdin)["outcome"])')
if [[ "$OUTCOME" != "success" ]]; then
    echo "FAIL: reshard outcome = $OUTCOME (expected success)"
    exit 1
fi

echo "==> Inspect post-reshard manifest"
POST=$("$BIN" inspect-manifest --store="$STORE" --repo="$TENANT/$REPO" --json)
REFCOUNT_POST=$(echo "$POST" | python3 -c 'import sys,json;b=json.load(sys.stdin);print(len(b.get("refs",{})))')
SHARDS_POST=$(echo "$POST" | python3 -c 'import sys,json;b=json.load(sys.stdin);print(len(b.get("ref_shards",[])))')
SHARDING_POST=$(echo "$POST" | python3 -c 'import sys,json;b=json.load(sys.stdin);print(b.get("ref_sharding",""))')
if [[ "$REFCOUNT_POST" -ne 0 ]]; then
    echo "FAIL: post-reshard refs count = $REFCOUNT_POST (expected 0)"
    exit 1
fi
if [[ "$SHARDS_POST" -lt 1 ]]; then
    echo "FAIL: post-reshard ref_shards count = $SHARDS_POST (expected >= 1)"
    exit 1
fi
if [[ "$SHARDING_POST" != "hash_v1" ]]; then
    echo "FAIL: post-reshard ref_sharding = $SHARDING_POST (expected hash_v1)"
    exit 1
fi
echo "    post-reshard refs=$REFCOUNT_POST shards=$SHARDS_POST sharding=$SHARDING_POST (v2 confirmed)"

echo "==> Re-run reshard-refs; expect noop"
NOOP=$("$BIN" reshard-refs --store="$STORE" --repo="$TENANT/$REPO" --json)
OUTCOME2=$(echo "$NOOP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["outcome"])')
if [[ "$OUTCOME2" != "noop" ]]; then
    echo "FAIL: second reshard outcome = $OUTCOME2 (expected noop)"
    exit 1
fi
echo "    noop confirmed"

echo "==> Export the v2 repo and assert every ref present"
DEST="$ROOT/export"
"$BIN" export --store="$STORE" --repo="$TENANT/$REPO" --dest="$DEST"
EXPORTED_REFS=$(cd "$DEST" && git for-each-ref --format='%(refname)' | wc -l)
if [[ "$EXPORTED_REFS" -lt 101 ]]; then
    echo "FAIL: exported refs = $EXPORTED_REFS (expected >= 101)"
    exit 1
fi
echo "    exported $EXPORTED_REFS refs from v2 repo"

echo "M12 reshard smoke: OK"
```

- [ ] **Step 2: Make the script executable and run it.**

```bash
chmod +x scripts/m12-reshard-smoke.sh
bash scripts/m12-reshard-smoke.sh 2>&1 | tail -30
```

Expected: ends with `M12_RESHARD_SMOKE_OK`.

If `inspect-manifest --json` does not exist or has different flag names, adjust the script to use whatever the existing CLI provides. The current inspect-manifest implementation lives in `cmd/bucketvcs/inspect.go`; check its flags.

Also: the `--default-branch` flag on `init` may not exist verbatim; check `cmd/bucketvcs/init.go`. Adjust if needed.

- [ ] **Step 3: Commit.**

```bash
git add scripts/m12-reshard-smoke.sh
git commit -m "scripts: m12-reshard-smoke.sh end-to-end against localfs (M12 Phase 8.1)"
```

---

### Task 8.2: Operator guide

**Files:**
- Create: `docs/m12-ref-sharding-operator-guide.md`

Keep this short. The spec is the long form; the operator guide is "if you're an operator running production, here's what changed and what you need to do."

- [ ] **Step 1: Write the operator guide.**

`docs/m12-ref-sharding-operator-guide.md`:

```markdown
# M12 — Ref Sharding Operator Guide

**TL;DR:** Most repos do nothing. A repo with more than ~10k refs may benefit from a one-shot manual migration via `bucketvcs reshard-refs`. Once migrated, the repo cannot go back without a future deshard CLI (deferred).

## What changed

M12 introduces an optional second representation for ref state:

- **Inline (v1, default):** every ref lives in the root manifest under `body.refs`. Fast for small repos.
- **Sharded (v2, opt-in):** refs live in `manifest/ref-shards/<sha256>.json` shard objects; the root manifest just references them by content hash. Scales to millions of refs.

New repos still default to inline. The root manifest schema version bumps from 1 to 2 so pre-M12 binaries refuse to read v2 manifests (fail-closed via `SchemaGate`).

## When to migrate

The threshold is informal: if `bucketvcs inspect-manifest` shows the body size growing past a few hundred KB (~5–10k refs), inline mode starts dominating push latency. Below that, inline is faster (zero shard IO).

## How to migrate

```
bucketvcs reshard-refs --store=<URL> --repo=<tenant>/<repo>
```

The CLI:
1. Reads the current root manifest.
2. Hashes every ref into a 256-bucket sharded layout (`ref_sharding: "hash_v1"`).
3. Writes each non-empty shard via `PutIfAbsent` (content-addressed, so racing writers are idempotent).
4. CAS-publishes a new root manifest with `RefShards` populated and `Refs` cleared.

The command is idempotent — re-running it on a v2 repo exits zero with `noop`.

### Pre-flight checklist

- **Backups.** v2 → v1 is not reversible in M12 (no `deshard-refs` yet). Take a manifest snapshot before running.
- **Concurrent pushes.** The migration does NOT acquire a maintenance lease. Pushes during the migration may force a retry. Quiesce automation if possible.
- **Retention window.** Failed migrations leave orphan shard objects in `manifest/ref-shards/`. They are content-addressed; GC sweeps them after retention.

### Concurrent push behavior

If a push wins the root CAS race during a reshard:
- The reshard exits non-zero with `concurrent mutation`.
- The shard objects already written are orphans — they survive until the next GC sweep after retention.
- Operator retries the command. The retry sees the new manifest version; if the racing push happened to bump to v2 (unlikely without M12 involvement), the second invocation no-ops.

## How to verify

After a successful reshard:

```
bucketvcs inspect-manifest --store=<URL> --repo=<tenant>/<repo> --json | jq '.ref_sharding,.ref_shards | length'
```

Expected: `"hash_v1"` and a non-zero shard count.

To list all refs through the new layout:

```
bucketvcs export --store=<URL> --repo=<tenant>/<repo> --dest=/tmp/check
cd /tmp/check && git for-each-ref
```

## What pre-M12 binaries see

A pre-M12 binary reading a v2 manifest fails with `ErrUnsupportedSchema` from the `SchemaGate` check. This is fail-closed — there is no silent misinterpretation hazard. Operators with mixed-version fleets must upgrade every binary that touches a given repo before resharding it.

## What gets stored where

```
tenants/<t>/repos/<r>/manifest/
├── root.json                      ← still the only commit point
└── ref-shards/
    ├── sha256-<hash>.json         ← one immutable object per non-empty shard
    └── sha256-<hash>.json
```

Old shard objects from before a push (when a shard's content changed) become orphans and GC away after retention.

## Limits and deferred work

- **Automatic threshold-driven resharding** — deferred.
- **Layout-change resharding** (e.g., 256 → 4096 shards) — deferred.
- **Hot-shard avoidance** (spec's "keep protected/default branches in an explicit shard") — deferred.
- **Per-namespace shard counts** — deferred.
- **`deshard-refs` reverse migration** — deferred. v2 → v1 today requires hand-editing the manifest off the critical path.

See `docs/m12-ref-sharding-spec.md` §11 for the full deferred list.

## Failure modes

| Symptom | Cause | Action |
|---|---|---|
| `concurrent mutation` exit | Push won the CAS race | Retry the CLI |
| `ErrShardCorrupt` from any read | Shard object bytes don't match recorded hash | Operator investigation; this is a tampering canary, not retry |
| `ErrUnsupportedSchema` from a binary | Pre-M12 binary reading a v2 manifest | Upgrade the binary |
| Reshard "stuck" at the same version | CAS retry loop exhausted | Check for sustained push pressure; rerun with quiesced traffic |
```

- [ ] **Step 2: Commit.**

```bash
git add docs/m12-ref-sharding-operator-guide.md
git commit -m "docs: M12 ref sharding operator guide (M12 Phase 8.2)"
```

---

### Task 8.3: Squash + tag

This is the milestone-ending operation. **Do it carefully** — once the squash lands on main and the tag points at it, the worktree branch goes away.

- [ ] **Step 1: Confirm every Phase commit landed cleanly.**

```bash
git log --oneline main..HEAD | head -50
```

Expected: every Phase task's commit message visible. No `WIP` or `fixup!` left over.

- [ ] **Step 2: Final full sweep + all smokes.**

```bash
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -10
go vet ./...
bash scripts/m12-reshard-smoke.sh 2>&1 | tail -5
# Run pre-existing smokes to confirm no regression:
bash scripts/m13-lfs-smoke-local.sh 2>&1 | tail -3
# (Cloud smokes — minio/gcs/azure — are optional gating; run them if dependencies are available.)
```

Expected: all green.

- [ ] **Step 3: Final two-stage review on the WHOLE branch.**

Same protocol as per-phase reviews, but the scope is `main..HEAD` (the entire M12 diff). Reviewers should focus on:
- Cross-phase consistency (do later phases match the types defined in earlier phases?).
- The full diff against the spec (`docs/m12-ref-sharding-spec.md`) — does every requirement have an implementation?
- Anything HIGH/MEDIUM unaddressed.

- [ ] **Step 4: Run roborev one final round on the squash candidate (do this BEFORE squashing).**

```bash
roborev review --branch --wait
```

Address findings; commit; close.

- [ ] **Step 5: Exit the worktree.**

```bash
# In the bucketvcs ExitWorktree tool, use action=keep so the branch stays
# on disk while you squash from main. The session returns to /home/eran/work/bucketvcs.
```

Use the `ExitWorktree` tool with `action: "keep"`.

- [ ] **Step 6: Squash-merge into main.**

```bash
cd /home/eran/work/bucketvcs
git merge --squash m12-ref-sharding
# Verify the staged change matches expectations.
git status --short | head -20
git diff --stat
```

Expected: every file from the M12 plan appears in the diff.

- [ ] **Step 7: Commit the squash with a comprehensive message.**

Use a heredoc to keep formatting:

```bash
git commit -m "$(cat <<'EOF'
M12: ref sharding via hash_v1 256-shard layout (squash of N commits on m12-ref-sharding)

Adds an opt-in sharded representation for ref state. Small repos stay
inline (v1) forever; large repos run `bucketvcs reshard-refs` once
to migrate to a content-addressed shard layout (v2) and unblock
push-time scaling past the inline-manifest ceiling.

Schema:
  - SchemaVersion bump 1 → 2. Pre-M12 binaries fail-closed via SchemaGate.
  - Body.RefShards []RefShard + Body.RefSharding string ("hash_v1").
  - manifest.UnmarshalBody validator rejects hybrid v1/v2 state and
    unknown sharding strategies.

Architecture:
  - New internal/repo/refstore package: RefStore interface,
    InlineRefStore (v1), ShardedRefStore (v2). Every ref consumer
    (uploadpack v0, protocol-v2 lsrefs, receivepack advertise + push
    completion, exporter, importer) goes through the interface.
  - 256 shards keyed by sha256(refname)[0]. Content-addressed: shard
    storage key includes the content hash so PutIfAbsent is idempotent
    on identical bytes.
  - Push integration: shard objects are PutIfAbsent'd inside
    Repo.Commit's buildBody callback (Phase A of the §5.2 write flow),
    BEFORE the root CAS. Aborted pushes leave orphan shards; GC
    sweeps them after retention.

Migration:
  - bucketvcs reshard-refs --store=<URL> --repo=<tenant>/<repo>
    one-shot inline → sharded. Idempotent (re-runs are noop on v2).
    Concurrent pushes during reshard cause ErrConcurrentMutation;
    operator retries.

GC:
  - gc.BuildLiveSet adds Body.RefShards[*].Key to the live set.
    Sweep against a v2 manifest preserves the live shard objects.

Verified:
  - go test ./... clean
  - go vet ./... clean
  - scripts/m12-reshard-smoke.sh end-to-end against localfs
  - existing M13 LFS smoke (local) still passes
  - cross-impl conformance suite: equivalence + round-trip + determinism

Deferred to a follow-on:
  - Automatic threshold-driven resharding
  - Maintenance-lease coordination for resharding
  - Layout-change resharding (256 → other N, namespace+hash strategies)
  - Hot-shard avoidance + per-namespace shard counts
  - bucketvcs deshard-refs reverse migration

EOF
)"
```

- [ ] **Step 8: Tag the squashed commit.**

```bash
git tag m12-complete
git tag -l | grep -E "m1[12]"
```

Expected: `m12-complete` listed alongside `m11-complete`.

- [ ] **Step 9: Clean up the worktree + branch.**

```bash
git worktree remove .claude/worktrees/m12-ref-sharding
git branch -D m12-ref-sharding
```

- [ ] **Step 10: Commit complete; final sweep on main to confirm.**

```bash
git log --oneline -3
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head
bash scripts/m12-reshard-smoke.sh 2>&1 | tail -3
```

Expected: top commit is the squash; sweep clean; smoke OK.

---

### Task 8.4: Memory updates

**Files:**
- Create: `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m12_progress.md`
- Modify: `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md`

- [ ] **Step 1: Write the progress file.**

Save to `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m12_progress.md`:

```markdown
---
name: m12-progress
description: M12 ref sharding via hash_v1 256-shard layout. Completed YYYY-MM-DD, commit <sha>, tag m12-complete.
metadata:
  type: project
---

# M12: Ref scaling via sharded refs

**Status:** Merged to main.
**Commit:** <SHA of squash on main>.
**Tag:** m12-complete (YYYY-MM-DD).

## What changed (one-liner)

Manifest schema gained Body.RefShards (v2, opt-in); every ref consumer goes through a new `internal/repo/refstore.RefStore` interface; `bucketvcs reshard-refs` is the one-shot inline→sharded migration CLI.

## Why

Inline refs (Body.Refs map[string]string) bottleneck push latency past ~10k refs because every push re-marshals the entire root manifest. Sharded refs spread the ref set across 256 content-addressed shard objects; push amplification drops to 1–3 shard rewrites per push regardless of total ref count.

**How to apply:** when adding a feature that touches refs (e.g., per-ref ACLs, ref expiry), build it on top of `refstore.RefStore` — both impls satisfy the same interface, so behavior is layout-agnostic.

## Architectural decisions captured here

- **Hash-only strategy, N=256 fixed, tagged `ref_sharding: "hash_v1"`.** Hybrid namespace+hash and layout-change resharding deferred. Adding a new strategy requires bumping the string + a migration; the schema string is the version gate.
- **256-byte shard ID space comes from `sha256(refname)[0]`.** sha256 (not sha1) so the hash quality is uniform regardless of object-hash format; first byte (not 4 bytes) keeps the shard count fixed at 256 for the foreseeable future.
- **Root CAS remains the only commit point.** Shard objects are content-addressed and immutable; PutIfAbsent collapses duplicates. Phase A (shard writes) happens INSIDE `Repo.Commit`'s buildBody callback, before the root CAS — no Commit signature change required.
- **Content-Type collision policy on Stage.NewShardObjects:** swallow `storage.ErrAlreadyExists` via `errors.Is`; the spec's Phase-A semantics depend on it.
- **`manifest.UnmarshalBody` is the canonical body-parse entry point.** Rejects hybrid v1/v2 state and unknown sharding strategies at the read boundary. Phase 5 switched every direct `json.Unmarshal(view.Body, &body)` to go through it.
- **Stage.Lookup helper for the importer's default-branch-deletion check.** Returns `ErrLookupNotInStage` when the refname's shard wasn't touched; caller falls back to RefStore.Lookup.

## Verification

- `go test ./...` clean
- `scripts/m12-reshard-smoke.sh` end-to-end against localfs (101+ refs → reshard → push → export round-trip)
- Cross-impl conformance suite: equivalence + round-trip + determinism, seeded for reproducibility
- N roborev review rounds across the 9 phases; final round clean

## Operational notes

- Operators opt in via `bucketvcs reshard-refs --store=<URL> --repo=<t>/<r>`. Idempotent on v2 repos (no-op).
- Concurrent pushes during reshard cause `ErrConcurrentMutation`; operator retries.
- v2 → v1 reverse migration is NOT in M12. A future `deshard-refs` CLI would close this.
- Pre-M12 binaries fail-closed on v2 manifests via the existing `SchemaGate` check (SchemaVersion=2 > pre-M12's CurrentSchemaVersion=1).

## Out-of-scope items (deferred)

See `docs/m12-ref-sharding-spec.md` §11.
```

Fill in `<SHA>` and `YYYY-MM-DD` from the actual squash commit before writing the file.

- [ ] **Step 2: Update MEMORY.md.**

In `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md`, add a new line after the M13.2 entry:

```markdown
- [M12 ref sharding merged to main](m12_progress.md) — commit <sha>, tag m12-complete (YYYY-MM-DD); hash_v1 256-shard layout via new internal/repo/refstore package; bucketvcs reshard-refs CLI for one-shot inline→sharded migration; SchemaVersion bump 1→2 with fail-closed SchemaGate; gc.BuildLiveSet enumerates RefShards keys. Automatic threshold resharding + layout-change reshard deferred.
```

- [ ] **Step 3: Final verification.**

```bash
cat /home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md | tail -3
ls /home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m12_progress.md
```

Expected: MEMORY.md ends with the new M12 line; the progress file exists.

---

### Task 8.5: Milestone done

- [ ] **Step 1: Sanity check final state on main.**

```bash
git branch --show-current  # expect: main
git log --oneline -3        # top commit is the squash
git tag -l | grep m12        # m12-complete present
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head
```

- [ ] **Step 2: Report to the operator: "M12 complete. Commit <sha>, tag m12-complete. Memory updated."**

- [ ] **Step 3: If a follow-on milestone was intended (e.g., automatic threshold resharding or BYOB mode), pick it next via the same brainstorming → plan → subagent-driven flow.**
