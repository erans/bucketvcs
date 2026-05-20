# M12 Ref Sharding Implementation Plan — Index

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add hash-based sharded refs to the bucketvcs root manifest so large repos can scale past the inline-refs ceiling, with a one-shot manual reshard CLI for opt-in migration.

**Architecture:** A new `internal/repo/refstore` package abstracts ref reads/writes behind a `RefStore` interface. `InlineRefStore` wraps `Body.Refs` (v1); `ShardedRefStore` wraps `Body.RefShards` + the ObjectStore (v2). Every ref consumer (uploadpack v0, protocol-v2 lsrefs, receivepack advertise + completion, exporter, importer) goes through the interface. Push-time shard writes happen inside the existing `Repo.Commit` buildBody callback — no Commit signature change required. A new `bucketvcs reshard-refs` subcommand performs one-shot v1→v2 migration.

**Tech Stack:** Go 1.x, existing bucketvcs internal packages, no new external dependencies.

**Spec:** [/home/eran/work/bucketvcs/docs/m12-ref-sharding-spec.md](../m12-ref-sharding-spec.md) (committed at ec8c041).

---

## How to read this plan

The plan is split across files because §19 ref sharding touches eight semi-independent surfaces. Each phase is a self-contained file with task-by-task TDD steps. **Read every phase before starting — later phases depend on types defined in earlier phases.**

| Phase | File | Theme | Approx steps |
|---|---|---|---|
| 0 | [phase-0-foundation.md](m12-plans/phase-0-foundation.md) | Schema additions + `UnmarshalBody` validator + `CurrentSchemaVersion` bump | ~25 |
| 1 | [phase-1-refstore-skeleton.md](m12-plans/phase-1-refstore-skeleton.md) | `refstore` package: interface, types, sentinel errors, `shardKey`, canonical-JSON marshal, `InlineRefStore` | ~30 |
| 2 | [phase-2-sharded-read.md](m12-plans/phase-2-sharded-read.md) | `ShardedRefStore.Lookup` + `List` with parallel fetch + hash verification | ~25 |
| 3 | [phase-3-sharded-write.md](m12-plans/phase-3-sharded-write.md) | `ShardedRefStore.Stage` — hash-bucket updates, load+mutate affected shards, compute new content | ~25 |
| 4 | [phase-4-conformance.md](m12-plans/phase-4-conformance.md) | `refstore/conformance` property tests: equivalence, round-trip, determinism | ~15 |
| 5 | [phase-5-consumer-switch.md](m12-plans/phase-5-consumer-switch.md) | Switch every ref consumer (uploadpack, lsrefs, receivepack, exporter, importer) to refstore. Importer also writes shard objects inside `Repo.Commit` buildBody. | ~30 |
| 6 | [phase-6-reshard-cli.md](m12-plans/phase-6-reshard-cli.md) | `internal/maintenance/reshard.go` + `bucketvcs reshard-refs` subcommand | ~25 |
| 7 | [phase-7-gc-integration.md](m12-plans/phase-7-gc-integration.md) | `gc.BuildLiveSet` learns `RefShards[*].Key`; sweep tests | ~10 |
| 8 | [phase-8-smoke-tag.md](m12-plans/phase-8-smoke-tag.md) | `scripts/m12-reshard-smoke.sh`, operator notes, memory update, squash + tag m12-complete | ~15 |

Total: ~200 steps across 8 phases.

---

## Worktree setup

M12 is a large milestone touching 10+ existing packages. **Implement in a dedicated worktree** to keep main clean while reviews iterate.

- [ ] **Setup step: enter a fresh worktree.** Use the EnterWorktree tool with name `m12-ref-sharding`. All subsequent work happens inside the worktree. The plan files were committed to main at the spec stage and are visible inside the worktree at the same paths.

Recent template: M13.2 ran in `.claude/worktrees/m132-signed-url-headers`. Same shape here.

---

## Review cadence (M1+ per-task protocol)

The project's review protocol is documented in MEMORY.md under "M1+ per-task review protocol":

> superpowers spec + code-quality reviews, THEN roborev-refine on max reasoning until pass or diminishing returns

**Per phase:**

1. Implement every task in the phase. Commit incrementally as the tasks instruct.
2. Run `go test ./... && go vet ./...` at the phase boundary; do not proceed if either fails.
3. Dispatch two reviewers in parallel:
   - **Spec-compliance review** (general-purpose subagent). Prompt template: "Review HEAD..main for compliance with `docs/m12-ref-sharding-spec.md` and `docs/m12-plans/phase-N-*.md`. Cite findings as HIGH/MEDIUM/LOW with file:line."
   - **Code-quality review** (superpowers:code-reviewer agent). Prompt template: "Review HEAD..main for code quality. Focus on idioms, error handling, naming, dead code, scope creep."
4. Fix HIGHs + MEDIUMs inline. Defer LOWs with a one-line rationale unless they're quick wins.
5. Commit fixups.
6. Run `roborev review --branch --wait`. Address findings, commit, comment + close, re-review until clean or diminishing returns (see m95_progress.md round protocol for the cadence; ~5 rounds max).
7. Move to the next phase.

**At the end (after Phase 8):** squash the branch to one commit on main with a comprehensive message (template in phase-8). Tag `m12-complete`. Update memory.

---

## Cross-phase invariants

These hold across all phases. Verify they're not violated when reviewing each phase:

1. **Root CAS remains the only commit point.** No phase introduces an alternative commit pathway. Shard objects are content-addressed via PutIfAbsent.
2. **`Body.Refs` and `Body.RefShards` are mutually exclusive on the wire.** Any code path that produces a body with both populated is a bug. `manifest.UnmarshalBody` rejects hybrids (Phase 0).
3. **Pre-M12 binaries must fail loudly on a v2 manifest.** The `SchemaGate` check (`SchemaVersion > CurrentSchemaVersion`) handles this; M12 bumps `CurrentSchemaVersion` to 2 in Phase 0 so pre-M12 builds reject.
4. **`PutIfAbsent` on content-addressed shard keys is idempotent.** Phase 3 and Phase 6 catch `storage.ErrAlreadyExists` via `errors.Is` and treat it as success. A bare error return on `ErrAlreadyExists` is a bug.
5. **Every ref consumer goes through `refstore.RefStore`.** After Phase 5 no caller reads `body.Refs` directly except inside the refstore package itself, the schema validator in `manifest.UnmarshalBody`, and the reshard CLI (which intentionally reads v1 `body.Refs` to migrate it).
6. **Determinism of shard serialization.** Sharding the same ref set twice MUST produce byte-identical shard objects. The conformance test in Phase 4 is the load-bearing assertion; Phase 1's marshal helper is where the determinism lives.

---

## Success criteria

The milestone is done when ALL of the following hold:

- [ ] `go test ./... && go vet ./...` clean on main.
- [ ] `scripts/m12-reshard-smoke.sh` passes end-to-end against localfs.
- [ ] A repo built with the M12 binary can be reshared via `bucketvcs reshard-refs` and the resulting v2 manifest survives push + clone + lsrefs.
- [ ] A pre-M12 binary attempting to read a v2 manifest fails with `ErrUnsupportedSchema` (verified by a test using a hand-crafted body).
- [ ] `gc.BuildLiveSet` includes `RefShards[*].Key` in the live set; a GC sweep against a v2 manifest does not delete live shards (verified by Phase 7's test).
- [ ] All four cloud backends (S3/MinIO, GCS/fake-gcs, Azure/Azurite, R2) still pass their conformance suites — refstore is backend-agnostic so this should be free, but verify.
- [ ] Spec + code-quality reviewers report no HIGH or MEDIUM findings on the squashed commit.
- [ ] `roborev review --branch --wait` reports "No issues found" (or last round shows only LOWs deferred with rationale).
- [ ] `m12_progress.md` written in memory; `MEMORY.md` index line added.
- [ ] `m12-complete` git tag points at the squashed commit on main.
