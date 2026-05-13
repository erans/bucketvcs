# M11 Phase 13 — Operator guide

**Status:** approved 2026-05-12. Carved as a stand-alone Phase 13 sub-spec because the master M11 plan (`docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md:5741`) was written before Phases 11/12/12.5 landed and its observability coverage is undersized for the shipped surface (11 metrics + 4 audit events with non-trivial label vocabulary, rate-amplification gotchas, and a known multi-tenant proxied-mode gap).

This spec is the contract for the Phase 13 implementation plan. The master M11 plan's `Phase 13 — Operator guide` block (lines 5741-5783) is superseded by this document.

---

## 0. Goal and scope

Ship a single operator guide (`docs/m11-bundles-operator-guide.md`) that lets a competent operator deploy, tune, monitor, diagnose, and roll back M11 bundle-uri and packfile-uri acceleration **without reading the M11 source code**. The guide is the reference for the metric and audit-event vocabulary shipped by Phases 12 and 12.5.

In scope:

- New file `docs/m11-bundles-operator-guide.md` — single document, ~900-1100 lines, mirroring the M8 / M9 / M10 guide structure for cross-doc readability.
- Cross-reference edits:
  - `docs/m9-maintenance-operator-guide.md` — "See also" pointer near the threshold-tuning section.
  - `docs/m8-gc-operator-guide.md` — forward-pointer noting M11's `TTL ≤ retention/24` rule depends on GC retention.
  - `README.md` — one-paragraph M11 announcement near the M10 paragraph, with the proxied-mode phrase qualified to "audit-strict single-repo deployments" so the README does not oversell proxied as multi-tenant ready.
- Code rename `bundle_byte_size` → `bundle_bytes` in `internal/maintenance/log.go` and `internal/maintenance/log_test.go`, plus the two references in the master M11 plan. Pre-1.0, no external dashboards; clean break, no shim. This brings the maintenance bundle-byte metric in line with the established `_bytes` suffix convention (`maintenance_pack_bytes_out`) before the operator guide commits the canonical name.

Out of scope (called out in the guide's deferred-work appendix, not fixed here):

- Multi-tenant `ProxiedKeyResolver` implementation (carryover from Phase 8). Until this lands, `--bundle-uri-mode=proxied` and `--pack-uri-mode=proxied` are single-repo-only.
- Full bundle GC pipeline wiring (carryover from Phase 9). `DiscoverBundles` is exported but has no production caller.
- `serveFetchLazyPath` short-circuit (Phase 8 deferred).
- Deprecation of legacy `gateway.Options` URI fields (Phase 8 deferred).
- The three concurrent BundleSafety sub-cases deferred to M11.x (`push_during_bundle`, `bundle_during_compaction`, `sweep_after_retire`).
- `FreshnessResult` sub-reason preservation in audit logs (Phase 12.5 round-2 note).
- `doServeBundleURI` vs `HandleBundleURI` redundant short-circuit consolidation (Phase 12.5 round-3 note).
- Production dashboards, Prometheus alert rules, Grafana panel definitions. The guide documents the metric vocabulary so operators can build their own; shipping the dashboards themselves is not bucketvcs-OSS work.

---

## 1. Production readiness matrix

The operator guide opens (before §1) with a production-readiness callout, repeated in the §4 mode-tradeoff table and §9 troubleshooting matrix:

| Mode | `--bundle-uri-mode` | `--pack-uri-mode` |
|------|---------------------|-------------------|
| `direct` | **GA** | **GA** |
| `proxied` | **Single-repo deployments only.** Multi-tenant deployments must use `direct` or `off`. The proxied handler resolves objects by hash alone (`ProxiedKeyResolver` is single-repo); a multi-tenant `ProxiedKeyResolver` is deferred work. | Same caveat as bundle. |
| `auto` | **GA on direct-capable backends** (S3, R2, GCS, Azure Blob). On localfs, `auto` falls back to proxied behavior — same single-repo caveat applies. | Same as bundle. |
| `off` | **GA.** | **GA.** |

The callout is intentionally repeated three times in the guide (top callout, §4 table, §9 troubleshooting) so an operator skimming any one of those entry points sees it. The cost is ~15 lines of doc; the alternative (one-line caveat in §4 only) relies on careful reading.

---

## 2. Document outline

Top of file:

- One-paragraph TL;DR.
- Production-readiness matrix (§1 above).

Sections, with target sizes (approximate):

1. **Overview** (~50 lines) — what bundle-uri and packfile-uri are, the cold-clone win, when they don't help (small repos, deep partial fetches, force-pushes that retire the bundle).
2. **Bundle freshness model** (~80 lines) — 7-state vocabulary (`disabled`, `no_bundle`, `no_ref`, `current`, `warm`, `stale`, `retired`); transition diagram; how `--bundle-warm-commits` (default 5000) and `--bundle-warm-age` (default 24h) gate `warm` vs `stale`.
3. **Maintenance scheduling** (~80 lines) — cron, Kubernetes CronJob, systemd timer recipes; co-scheduling repack + bundle-refresh + compact so the materialized mirror is reused once per run.
4. **Signed-URL vs gateway-proxied tradeoff** (~70 lines) — full 4-mode table (direct / proxied / auto / off), with the single-repo caveat repeated in the proxied row.
5. **TTL vs M8 retention** (~40 lines) — hard rule `TTL ≤ retention/24`; CLI enforces; defaults `--proxied-url-pack-ttl=1h`, `--proxied-url-bundle-ttl=4h` vs the M8 default `--retention-window=168h`.
6. **Bandwidth and cost economics** (~50 lines) — direct mode shifts egress to object storage; R2-style economics dominate; how to reason about hosted vs OSS deployments.
7. **Disabling acceleration** (~25 lines) — `--bundle-uri-mode=off --pack-uri-mode=off`; behavior reverts to pre-M11; M9 / M10 paths unchanged.
8. **Observability reference** (~250 lines — the meaty new section) — split into two subsections:
   - 8.1 Metrics — table per metric: name, labels with the closed vocabulary, semantics, rate-amplification notes where applicable. Eleven metrics total (8 gateway + 3 maintenance). See §3 below for the canonical list.
   - 8.2 Audit events — four events (`bundle.generated`, `bundle.retired`, `bundle.uri.advertised`, `proxied.url.served`); field list, when emitted, slog-grep example.
9. **Troubleshooting matrix** (~120 lines) — eight entries: bundle never generated; clone slow despite bundle (client did not opt in); proxied-URL 403 (token expired, signing key rotated, or kind mismatch); direct-URL 403 (TTL expired or backend rejected); pack-uri never advertised (`PackChecksum` missing on legacy manifest, backfill not yet run); freshness=stale persists; multi-repo proxied confusion; truncated-serve metric spike.
10. **Migration from pre-M11** (~40 lines) — deploy M11 binaries → `bucketvcs maintenance --force` once per repo to backfill `PackChecksum` and seed bundles → enable `--bundle-uri-mode=auto` on `bucketvcs serve`. No manifest schema break; rollback is safe (M11 fields are `omitempty`).
11. **Forensics** (~50 lines) — inspect-manifest recipes (`bucketvcs inspect-manifest` walking `body.Bundles[]`); audit-log grep cookbook.
12. **Deferred work** (~30 lines) — visible appendix: multi-tenant proxied, full bundle GC pipeline, lazy-path short-circuit, etc. Lets operators plan around what's coming.

Total target: ~900 lines. Acceptable range 850-1100. M8 guide is 1105 lines; M9 is 568; M10 is 688.

---

## 3. Observability reference content (canonical inventory)

The guide's §8 is the canonical operator-facing description of the M11 observability surface. Names and label vocabularies below are locked at source as of Phase 12.5 (commits `a7aae5f` and `915f787`) modulo the `bundle_byte_size` → `bundle_bytes` rename shipped in Task 13.0.

### 3.1 Metrics

**Maintenance-side (3):**

| Metric | Labels | Semantics |
|--------|--------|-----------|
| `bundle_generated_total` | `outcome` ∈ {`success`, `noop`, `failure`}, `repo_id`, `trigger_reason` | Count of bundle-generation attempts. `success` = `BundleResult.Generated == true`. `noop` = generation skipped intentionally (trigger declined OR recoverable per-repo skip like `skipped_reachability_load_error` / `skipped_trigger_eval_error`). `failure` = unexpected error path. |
| `bundle_generation_duration_seconds` | `repo_id` | Wall-clock seconds for the bundle-generation phase. Emitted on every pipeline run that enters the bundle phase, regardless of outcome. |
| `bundle_bytes` (renamed from `bundle_byte_size`) | `repo_id` | Final bundle file size in bytes. Emitted only when `br.Generated && br.ByteSize > 0`. |

**Gateway-side (8):**

| Metric | Labels | Semantics + rate-amplification |
|--------|--------|-------------------------------|
| `bundle_advertised_total` | `repo_id`, `freshness` ∈ {`disabled`, `no_bundle`, `no_ref`, `current`, `warm`, `stale`} | Count of `command=bundle-uri` dispatches per repo. Includes encode-error path (counts dispatch attempts, not successful encodes). **Rate-amplification:** rogue clients can pump this at unbounded rate — alert on `freshness=current/warm/stale` rates for legitimate advertise traffic, not on the raw total. The internal state machine has a 7th value `retired`, but the gateway always routes those code paths through `outcome.Reason = no_bundle` or `no_ref`, so the literal label value `retired` is reserved for future use and not currently emitted. |
| `bundle_uri_advertised_total` | `repo_id`, `via` ∈ {`proxied`, `direct`} | Only emitted when the bundle-uri response actually contains URLs. Per Phase 13 Task 13.1 verification, only `freshness ∈ {current, warm}` produce URLs; `stale` is dispatch-counted but does not advertise. |
| `bundle_uri_served_total` | `via` | Per successful proxied bundle serve. **Counts truncated serves too** — io.Copy mid-stream errors still emit. |
| `bundle_uri_served_bytes` | `via` | Actual bytes written via `countingWriter` (may be less than Content-Length on client disconnect). |
| `pack_uri_advertised_total` | `repo_id`, `via` | Emitted when the `packfile-uris` stanza fires. |
| `pack_uri_served_total` | `via` | Per successful proxied pack serve. Truncation semantics same as bundle. |
| `pack_uri_served_bytes` | `via` | Same shape as bundle. |
| `proxied_url_token_invalid_total` | `reason` ∈ {`missing`, `expired`, `kind_mismatch`, `invalid`} | Per token-validation failure on `/_bundle/` or `/_pack/`. The user-facing 403 body does NOT leak the `kind_mismatch` distinction (collapsed to "invalid token"). |

**Cardinality note** (must appear in the guide): `repo_id` is intentionally absent from all served-* metrics because the proxied handler is hash-keyed via the single-repo `ProxiedKeyResolver`. When the multi-tenant `ProxiedKeyResolver` lands, the label will be added.

### 3.2 Audit events

All four events use the flat-attrs shape (`slog.Bool("audit", true)` + `slog.String("event", "...")` + top-level attrs), matching `internal/maintenance/log.go::emitBundleGenerated`. No `slog.Group("audit", ...)`.

**Maintenance-side (2):**

- `bundle.generated` — emitted from `bundle.go::runBundlePhase` after `RunBundleCASMerge` succeeds. Fields: `repo_id`, `bundle_id`, `bundle_hash`, `tip_oid`, `covers_manifest_version`, `byte_size`, `duration_ms`. Join `bundle_id` against `bundle.retired.replaced_by` to detect replacement pairs.
- `bundle.retired` — emitted from the same site, before the paired `bundle.generated`, when a CAS-merge supersedes an existing `full_default` entry. Fields: `repo_id`, `bundle_id` (the retired bundle's ID), `reason` (= `"replaced"` for M11; vocabulary will gain `"gc_swept"` when full bundle GC lands), `replaced_by` (the new `bundle_id` from the paired `bundle.generated`).

**Gateway-side (2):**

- `bundle.uri.advertised` — emitted from `serveBundleURI` when the response contains URLs. Fields: `repo_id`, `freshness`, `via`, `bundle_count` (= 1 in M11), `first_tip_oid`.
- `proxied.url.served` — emitted from the proxied handler on 200 / 206. Fields: `kind` ∈ {`bundle`, `pack`}, `hash`, `bytes_served`, `status_code`, `range_request`.

### 3.3 Rate-amplification gotchas (must be called out explicitly in the guide)

- `bundle_advertised_total`: rogue or misconfigured clients can pump dispatches at arbitrary rate. Build alerts on the per-freshness derived rates, not on the total.
- `bundle_uri_served_total` and `pack_uri_served_total`: count truncated mid-serves (the metric increments before the io.Copy completes). Pair with `*_served_bytes` and the `proxied.url.served` audit event's `status_code` field to distinguish full from truncated serves.
- All served-* metrics lack `repo_id` (single-repo handler assumption). Multi-tenant deployments using proxied mode are explicitly unsupported today.

---

## 4. Task shape

Phase 13 ships as one squash commit on a `m11-bundles-p13` branch off main, with four sub-tasks:

- **Task 13.0 — Metric rename** (~20 occurrences across 4 files)
  - `internal/maintenance/log.go`: rename the `"bundle_byte_size"` string literal in `emitBundleResultMetrics` to `"bundle_bytes"`, plus the matching reference in the function-level doc comment.
  - `internal/maintenance/log_test.go`: ~14 references across four tests (`Generated`, `Failure`, `Noop`, `Generated+NonZeroByteSize`, `Generated+ZeroByteSize` cases) that assert either the metric name or its non-emission. All update mechanically (`s/bundle_byte_size/bundle_bytes/g`).
  - `docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md`: 3 references in the Phase 12 task body (2 in test pseudocode, 1 in emit pseudocode). Update so the plan-vs-shipping vocabulary stays in sync.
  - `docs/superpowers/plans/2026-05-12-m11-phase-12-5-gateway-observability.md`: 1 reference in the open-questions section noting "Phase 13 documentation-time decision" — replace with a back-pointer to this spec.
  - Verification: `go test ./internal/maintenance/... -run BundleResult -v` + `grep -RIn bundle_byte_size internal/ docs/superpowers/plans/` returns nothing (this spec keeps the old name in historical references; that's intentional).
- **Task 13.1 — Author the operator guide**
  - Create `docs/m11-bundles-operator-guide.md` with the structure in §2 above.
  - The §8 observability reference must use the locked-at-source vocabulary from §3.
- **Task 13.2 — Cross-references**
  - `docs/m9-maintenance-operator-guide.md`: insert "See also: M11 bundle thresholds" near the threshold-tuning section (~6 lines).
  - `docs/m8-gc-operator-guide.md`: insert forward-pointer noting M11's `TTL ≤ retention/24` rule near the retention-window section (~4 lines).
  - `README.md`: insert M11 announcement paragraph near the M10 paragraph, with the proxied phrase qualified to "audit-strict single-repo deployments".
- **Task 13.3 — Verification and commit**
  - Run the vocab-consistency grep (see §5).
  - Run the internal-link check (see §5).
  - Run the code-snippet smoke (see §5).
  - Confirm `wc -l docs/m11-bundles-operator-guide.md` lands in 850-1100.
  - Final squash commit on the branch.

Per [[m1_review_protocol]], each task gets superpowers spec review + relevant follow-up reviewer (code-quality for 13.0; doc-quality / writing-clearly for 13.1; spec review only for 13.2 and 13.3), then a single roborev-refine pass on maximum reasoning. Iterate until pass or diminishing returns.

---

## 5. Verification

The guide is correct when all five checks pass:

1. **Vocab consistency** — every metric name, label value, and audit-event name appearing in the guide also appears in the canonical source files (`internal/maintenance/log.go`, `internal/gateway/log.go`, `internal/gitproto/uploadpack/log.go`). Conversely, every name in those source files that is operator-facing is documented in the guide. Run as a pre-commit grep:

   ```bash
   # Names in the guide must appear in code:
   grep -oE '`[a-z][a-z_]+_(total|seconds|bytes|advertised|served|generated|retired)`' docs/m11-bundles-operator-guide.md \
     | sort -u \
     | while read name; do
         literal=$(echo "$name" | tr -d '`')
         if ! grep -RIn "\"$literal\"" internal/maintenance/log.go internal/gateway/log.go internal/gitproto/uploadpack/log.go; then
           echo "MISSING IN CODE: $literal"
         fi
       done
   ```

2. **Internal-link sanity** — every `[...](#anchor)` and `[...](file.md)` in the guide resolves. Anchors are auto-derived from heading text; verify the file targets exist.

3. **Code-snippet smoke** — every CLI invocation in the guide (`bucketvcs maintenance ...`, `bucketvcs serve ...`, `bucketvcs inspect-manifest ...`) cites flags that `go run ./cmd/bucketvcs <subcommand> --help` advertises. No need to execute fully — just confirm the surface.

4. **Length sanity** — `wc -l docs/m11-bundles-operator-guide.md` in the 850-1100 band. Outside the band = re-examine for either redundancy (too long) or skipped content (too short).

5. **Review protocol** — per [[m1_review_protocol]]:
   - superpowers spec review per task
   - superpowers code-quality review on Task 13.0 (rename only — doc tasks don't fit code-quality)
   - superpowers doc-quality / writing-clearly review on Task 13.1
   - roborev-refine on max reasoning, one pass per task; iterate until pass or diminishing returns

No e2e gate at Phase 13. Phase 14 (smoke scripts) is the milestone-level e2e gate.

---

## 6. Open questions explicitly closed by this spec

1. **Multi-tenant proxied gap visibility** — prominent warning callout in three places (top matrix, §4 mode table, §9 troubleshooting). Closed.
2. **Observability section depth** — full reference with closed-vocab tables and rate-amplification notes, not a single bullet. Closed.
3. **`bundle_byte_size` rename** — rename in code to `bundle_bytes` (Task 13.0) before the doc cites the canonical name. Closed.
4. **Document layout** — keep the plan's 10-section ordering for cross-doc readability with M8/M9/M10. Closed.
5. **Single doc vs split** — single doc per milestone. Closed.

---

## 7. Cross-references

- Master M11 plan: `docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md` (Phase 13 starts at line 5741; superseded by this spec).
- Phase 12.5 sub-plan: `docs/superpowers/plans/2026-05-12-m11-phase-12-5-gateway-observability.md` (observability vocabulary source).
- Prior operator guides for style reference: `docs/m8-gc-operator-guide.md` (1105 lines), `docs/m9-maintenance-operator-guide.md` (568 lines), `docs/m10-reachability-operator-guide.md` (688 lines).
- M11 progress journal: [[m11_progress]] (Phase 12.5 retro is the most recent entry; cites the open `bundle_byte_size` rename question that Task 13.0 closes).
- Review protocol: [[m1_review_protocol]].
