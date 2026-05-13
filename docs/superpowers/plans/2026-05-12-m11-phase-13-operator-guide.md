# M11 Phase 13 — Operator Guide Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the M11 bundle-uri and packfile-uri operator guide, plus the supporting metric rename and cross-references, per the spec at `docs/superpowers/specs/2026-05-12-m11-phase-13-operator-guide-design.md`.

**Architecture:** Four sequential tasks in one squash commit: (13.0) rename `bundle_byte_size` → `bundle_bytes` so the operator guide cites the canonical metric name; (13.1) author `docs/m11-bundles-operator-guide.md`; (13.2) cross-reference edits to M9 guide, M8 guide, and README; (13.3) verification (vocab consistency grep, link sanity, CLI flag smoke, length check).

**Tech Stack:** Go (rename surface), Markdown (operator guide). No new dependencies. Verification uses standard shell tools (`grep`, `wc`, `awk`).

---

## File map

**Create:**
- `docs/m11-bundles-operator-guide.md` — the new operator guide (~900 lines, 12 sections per spec §2).

**Modify:**
- `internal/maintenance/log.go` — rename `bundle_byte_size` → `bundle_bytes` (1 emit string literal + 1 doc-comment line).
- `internal/maintenance/log_test.go` — rename `bundle_byte_size` → `bundle_bytes` in 4 tests (~14 occurrences).
- `docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md` — rename 3 `bundle_byte_size` references in the Phase 12 task body.
- `docs/superpowers/plans/2026-05-12-m11-phase-12-5-gateway-observability.md` — replace the 1 open-question reference with a back-pointer to the Phase 13 spec.
- `docs/m9-maintenance-operator-guide.md` — add "See also: M11 bundle thresholds" subsection near §4 Threshold Tuning (~6 lines).
- `docs/m8-gc-operator-guide.md` — add forward-pointer near §3.3 retention warning section, noting M11's `TTL ≤ retention/24` rule depends on GC retention (~5 lines).
- `README.md` — add M11 line under "CLI subcommands" referencing bundle/pack URI advertisement, and add a Documentation entry linking to the new guide.

---

## Task 13.0 — Metric rename `bundle_byte_size` → `bundle_bytes`

**Files:**
- Modify: `internal/maintenance/log.go`
- Modify: `internal/maintenance/log_test.go`
- Modify: `docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md`
- Modify: `docs/superpowers/plans/2026-05-12-m11-phase-12-5-gateway-observability.md`

**Why first:** the operator guide written in Task 13.1 will cite `bundle_bytes` as the canonical name. Renaming the code first means the guide and source stay consistent inside the same squash commit; reviewer can read either in any order.

- [ ] **Step 1: Confirm the rename surface**

Run: `grep -RIn "bundle_byte_size" internal/ docs/superpowers/plans/`

Expected output (exact occurrence counts may differ slightly; verify before editing):

```
internal/maintenance/log.go:69:  - bundle_byte_size: emitted ONLY when Generated is true AND ByteSize > 0.
internal/maintenance/log.go:103:		emitMetric(ctx, logger, "bundle_byte_size", br.ByteSize,
internal/maintenance/log_test.go:125: 	// Line 2: bundle_byte_size (1234)
internal/maintenance/log_test.go:130:	if e2["metric_name"] != "bundle_byte_size" {
internal/maintenance/log_test.go:131:		t.Errorf("line 2 metric_name = %v, want bundle_byte_size", e2["metric_name"])
internal/maintenance/log_test.go:143: // but NOT bundle_byte_size (ByteSize is 0 and Generated is false).
internal/maintenance/log_test.go:173:	// Confirm bundle_byte_size is NOT anywhere in the output.
internal/maintenance/log_test.go:174:	if strings.Contains(buf.String(), "bundle_byte_size") {
internal/maintenance/log_test.go:175:		t.Errorf("bundle_byte_size should not be emitted on failure, got: %s", buf.String())
internal/maintenance/log_test.go:181: // bundle_generation_duration_seconds but NOT bundle_byte_size.
internal/maintenance/log_test.go:213:	// Confirm bundle_byte_size is NOT anywhere in the output.
internal/maintenance/log_test.go:214:	if strings.Contains(buf.String(), "bundle_byte_size") {
internal/maintenance/log_test.go:215:		t.Errorf("bundle_byte_size should not be emitted on noop, got: %s", buf.String())
internal/maintenance/log_test.go:253:	// Line 2: bundle_byte_size must be emitted (Generated && ByteSize > 0 both hold).
internal/maintenance/log_test.go:258:	if e2["metric_name"] != "bundle_byte_size" {
internal/maintenance/log_test.go:259:		t.Errorf("line 2 metric_name = %v, want bundle_byte_size", e2["metric_name"])
internal/maintenance/log_test.go:264: // bundle_byte_size gate requires BOTH Generated=true AND ByteSize>0. A result
internal/maintenance/log_test.go:266: // must NOT emit bundle_byte_size.
internal/maintenance/log_test.go:296:	// bundle_byte_size must NOT appear: Generated=false gates the emission even
internal/maintenance/log_test.go:298:	if strings.Contains(buf.String(), "bundle_byte_size") {
internal/maintenance/log_test.go:299:		t.Errorf("bundle_byte_size should not be emitted when Generated=false, got: %s", buf.String())
docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md:5596:    if !rec.HasMetric("bundle_byte_size") {
docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md:5597:        t.Errorf("missing bundle_byte_size")
docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md:5625:        emitMetric(ctx, logger, "bundle_byte_size", br.ByteSize, "repo_id", repoID)
docs/superpowers/plans/2026-05-12-m11-phase-12-5-gateway-observability.md:44:- Renaming `bundle_byte_size` from Phase 12 to `bundle_bytes` for convention alignment (Phase 13 documentation-time decision).
```

If your grep output diverges meaningfully (different line numbers are fine; new files appearing is not), STOP and re-read this plan; the rename surface may have shifted since the plan was written.

- [ ] **Step 2: Rename in `internal/maintenance/log.go`**

Two edits in one file:

Replace the doc comment (around line 69):

```go
//   - bundle_byte_size: emitted ONLY when Generated is true AND ByteSize > 0.
```

with:

```go
//   - bundle_bytes: emitted ONLY when Generated is true AND ByteSize > 0.
```

Replace the emit string (around line 103):

```go
		emitMetric(ctx, logger, "bundle_byte_size", br.ByteSize,
```

with:

```go
		emitMetric(ctx, logger, "bundle_bytes", br.ByteSize,
```

- [ ] **Step 3: Rename in `internal/maintenance/log_test.go`**

All references in this file refer to the same metric name. Do a file-wide replace:

```bash
sed -i 's/bundle_byte_size/bundle_bytes/g' internal/maintenance/log_test.go
```

Then verify with `grep -n "bundle_byte_size\|bundle_bytes" internal/maintenance/log_test.go`. Expected: only `bundle_bytes` occurrences; zero `bundle_byte_size`.

- [ ] **Step 4: Run the maintenance log tests to confirm green**

Run: `go test ./internal/maintenance/... -run BundleResult -v -count=1`

Expected: all subtests PASS. The renamed name is now what the assertions look for.

If any test fails, READ the failure carefully. The only legitimate cause is a typo in step 2 or step 3; if the failure is about a different metric, do not "fix" it — surface it as a finding.

- [ ] **Step 5: Rename in the master M11 plan**

In `docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md`, three references at approximately lines 5596, 5597, 5625. Use:

```bash
sed -i 's/bundle_byte_size/bundle_bytes/g' docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md
```

Then verify: `grep -n "bundle_byte_size\|bundle_bytes" docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md`. Expected: only `bundle_bytes`.

- [ ] **Step 6: Update the Phase 12.5 open-questions reference**

In `docs/superpowers/plans/2026-05-12-m11-phase-12-5-gateway-observability.md`, around line 44, replace the existing line:

```
- Renaming `bundle_byte_size` from Phase 12 to `bundle_bytes` for convention alignment (Phase 13 documentation-time decision).
```

with:

```
- ~~Renaming `bundle_byte_size` from Phase 12 to `bundle_bytes` for convention alignment.~~ Closed by Phase 13 spec `docs/superpowers/specs/2026-05-12-m11-phase-13-operator-guide-design.md` §4 Task 13.0; renamed in code under the same squash.
```

The strikethrough preserves the historical note; the back-pointer documents the closure.

- [ ] **Step 7: Final grep verification**

Run: `grep -RIn "bundle_byte_size" internal/ docs/superpowers/plans/ docs/superpowers/specs/2026-05-12-m11-phase-13-operator-guide-design.md`

Expected output: only matches inside the Phase 13 spec (where the old name appears as historical/reference context — that is intentional and is explicitly called out in the spec at line ~80). No matches in `internal/` and no live `bundle_byte_size` strings in plan documents.

If matches appear outside the spec's historical-reference lines, fix them before moving on.

- [ ] **Step 8: Run the full maintenance test suite once more**

Run: `go test ./internal/maintenance/... -count=1`

Expected: all PASS. `-count=1` defeats the test cache to confirm the actual rename.

- [ ] **Step 9: Commit**

```bash
git add internal/maintenance/log.go internal/maintenance/log_test.go \
        docs/superpowers/plans/2026-05-10-m11-bundle-and-packfile-uri.md \
        docs/superpowers/plans/2026-05-12-m11-phase-12-5-gateway-observability.md
git commit -m "$(cat <<'EOF'
M11 Phase 13 Task 13.0: rename bundle_byte_size -> bundle_bytes

Aligns the maintenance bundle-byte metric with the established _bytes
suffix convention (maintenance_pack_bytes_out). Pre-1.0, no external
dashboards; clean break, no shim. Lands inside the Phase 13 squash so the
operator guide written in Task 13.1 can cite the canonical name.

Closes the rename open-question carried from Phase 12.5 retro.
EOF
)"
```

---

## Task 13.1 — Author `docs/m11-bundles-operator-guide.md`

**Files:**
- Create: `docs/m11-bundles-operator-guide.md`

This task is the bulk of Phase 13. Build the guide bottom-up: scaffold first (TL;DR + production-readiness matrix + section headers + footer), then fill each section in spec-order. Verify the §8 observability tables against the source files in `internal/maintenance/log.go`, `internal/gateway/log.go`, and `internal/gitproto/uploadpack/log.go` before committing the section — the vocabulary tables are the most precision-sensitive content in the document.

**Style anchor:** mirror prose density and section structure from `docs/m10-reachability-operator-guide.md`. The M10 guide uses numbered `## N. Title` top-level sections with `### N.M Subsection` second-level headers, opens with a `---`-fenced overview paragraph, and uses inline `` `code` `` for flag names and file extensions. Match that.

**Length target:** 850-1100 lines total. Lower bound prevents under-writing; upper bound prevents drift. M8 guide is 1105 lines, M9 is 568, M10 is 688.

- [ ] **Step 1: Verify the canonical observability surface against source**

Before writing a single line of the guide, confirm the metric and audit-event names + fields in source. Run:

```bash
grep -n 'emitMetric.*"' internal/maintenance/log.go internal/gateway/log.go internal/gitproto/uploadpack/log.go
grep -n 'emit.*audit\|LogAttrs.*"' internal/maintenance/log.go internal/gateway/log.go internal/gitproto/uploadpack/log.go | grep -i 'bundle\|proxied\|pack_uri\|pack-uri'
```

Cross-check against the spec's §3 inventory (`docs/superpowers/specs/2026-05-12-m11-phase-13-operator-guide-design.md`). Eleven metrics (3 maintenance + 8 gateway) and four audit events (`bundle.generated`, `bundle.retired`, `bundle.uri.advertised`, `proxied.url.served`). If the source disagrees with the spec inventory, the spec needs an inline correction first — STOP and surface it.

After Task 13.0 lands, the maintenance metrics are: `bundle_generated_total`, `bundle_generation_duration_seconds`, `bundle_bytes`.

- [ ] **Step 2: Create the file with scaffold**

Write `docs/m11-bundles-operator-guide.md` with the scaffolding below. Each `## N. Title` body will be filled in subsequent steps. Section headers, TL;DR, and the production-readiness matrix go in NOW so the file is structurally complete and reviewers can navigate.

```markdown
# M11 Operator Guide: Bundle-URI and Packfile-URI Acceleration

This guide is for operators who deploy, tune, monitor, and roll back M11
bundle-uri and packfile-uri acceleration in production. It covers what
bundle-uri and packfile-uri are, the bundle freshness state machine, how to
schedule bundle generation alongside `bucketvcs maintenance`, when to use
signed-URL vs gateway-proxied delivery, how the M11 TTL rule interacts with
M8 retention, the eleven observability metrics and four audit events
shipped in M11, an eight-entry troubleshooting matrix, the pre-M11 → M11
migration recipe, post-incident forensics, and the deferred-work tracker
so operators can plan around what is not yet production-ready.

---

## Production readiness

| Mode | `--bundle-uri-mode` | `--pack-uri-mode` |
|------|---------------------|-------------------|
| `direct` | **GA** | **GA** |
| `proxied` | **Single-repo deployments only.** Multi-tenant deployments must use `direct` or `off`. The proxied handler resolves objects by hash alone (`ProxiedKeyResolver` is single-repo today); a multi-tenant `ProxiedKeyResolver` is deferred work. | Same caveat as bundle. |
| `auto` | **GA on direct-capable backends** (S3, R2, GCS, Azure Blob). On localfs, `auto` falls back to proxied behavior — same single-repo caveat applies. | Same as bundle. |
| `off` | **GA.** Behavior reverts to pre-M11 (standard `upload-pack`); M9/M10 paths unchanged. | Same. |

If you operate more than one bucketvcs repository through a single `bucketvcs serve`, do not enable `--bundle-uri-mode=proxied` or `--pack-uri-mode=proxied` yet. Use `direct` (signed URLs to cloud storage) or `off`. See §4 for the full tradeoff and §9 for troubleshooting.

---

## 1. Overview

(filled in Step 3)

---

## 2. Bundle Freshness Model

(filled in Step 4)

---

## 3. Maintenance Scheduling

(filled in Step 5)

---

## 4. Signed-URL vs Gateway-Proxied

(filled in Step 6)

---

## 5. TTL vs M8 Retention

(filled in Step 7)

---

## 6. Bandwidth and Cost Economics

(filled in Step 8)

---

## 7. Disabling Acceleration

(filled in Step 9)

---

## 8. Observability Reference

(filled in Step 10)

---

## 9. Troubleshooting Matrix

(filled in Step 11)

---

## 10. Migration from Pre-M11

(filled in Step 12)

---

## 11. Forensics

(filled in Step 13)

---

## 12. Deferred Work

(filled in Step 14)
```

Then run: `wc -l docs/m11-bundles-operator-guide.md`. Expected: ~90 lines (scaffolding only).

- [ ] **Step 3: Fill §1 Overview** (target 40-60 lines)

Replace the `(filled in Step 3)` placeholder with §1 prose. Cover:

- What bundle-uri is (Git protocol v2 capability §16.3): the gateway advertises a URL the client can download as a pre-built `*.bundle` file before the regular fetch. The client unpacks the bundle, then runs a smaller incremental fetch for any commits arrived since the bundle was generated.
- What packfile-uri is (Git protocol v2 capability §16.4): for full-clone requests where the manifest has exactly one canonical pack, the gateway advertises a URL the client can download as a `.pack` file directly. The inline pack alongside is reduced to whatever is not covered by the URI (M11 ships with `--keep-pack` elision so the bytes are not shipped twice).
- The cold-clone win: a fresh `git clone` of a large repo without bundle-uri runs upload-pack server-side and streams the full pack. With bundle-uri active and a current bundle, the heavy bytes flow client-to-bucket (direct mode) or client-to-gateway-to-bucket (proxied mode), so the gateway's CPU and the bucketvcs-side reachability walk are bypassed for the bulk of the data.
- When neither helps: small repos (the cold pack is already cheap), deep partial fetches (the bundle covers the full default branch, not the partial subset), and force-pushes that retire the bundle's tip (the freshness state machine moves the bundle to `retired` and the client falls back to standard fetch).
- The two are independent: an operator can enable one and not the other. Bundle-uri targets cold clones; packfile-uri targets full-clone-after-repack scenarios where the manifest is in single-pack shape.

Cross-reference at the bottom: [`bucketvcs maintenance`](m9-maintenance-operator-guide.md), [`bucketvcs gc`](m8-gc-operator-guide.md), [reachability index](m10-reachability-operator-guide.md).

After writing, run `wc -l docs/m11-bundles-operator-guide.md`. Should be around 130-150 lines total (scaffold + §1).

- [ ] **Step 4: Fill §2 Bundle Freshness Model** (target 70-90 lines)

Replace `(filled in Step 4)` with §2. Required content:

The seven freshness states (closed vocabulary from `internal/gitproto/uploadpack/freshness.go`):

| State | Meaning | When emitted |
|-------|---------|---------------|
| `disabled` | The gateway has bundle-uri turned off (`--bundle-uri-mode=off` or no `BuildURL` configured). | `command=bundle-uri` arrived; gateway emits `bundle_advertised_total{freshness=disabled}` and returns an empty response. |
| `no_bundle` | The manifest has no `full_default` bundle entry. Maintenance has never generated one, or it was retired and not yet regenerated. | Operator action: run `bucketvcs maintenance --bundle-only`. |
| `no_ref` | A `full_default` bundle entry exists but its `TipOID` references a ref that no longer exists in the manifest (likely force-pushed away or branch renamed). | Operator action: next maintenance run will retire the stale entry and may generate a fresh one. |
| `current` | Bundle covers the current default-branch tip. Client gets the bundle URL with no walkback. | The hot path. |
| `warm` | Bundle is behind the current tip by ≤ `--bundle-warm-commits` (default 5000) AND younger than `--bundle-warm-age` (default 24h). Client gets the bundle URL plus an incremental fetch. | Acceptable while maintenance is between scheduled runs. |
| `stale` | Bundle is behind by more than `--bundle-warm-commits` OR older than `--bundle-warm-age`. Gateway still advertises (the client savings still exceed the incremental fetch cost), but emits a `freshness=stale` metric so dashboards can alert on persistent stale state (maintenance behind schedule). | Operator action: tighten maintenance cadence or raise `--bundle-warm-commits`. |
| `retired` | Bundle's TipOID is no longer reachable from any ref (force-push retired the commit). Gateway does NOT advertise the bundle to the client. | Operator action: next maintenance run will generate a fresh entry. |

State transitions diagram (ASCII):

```
                  +----------------+
                  | maintenance    |
                  | --bundle-only  |
                  +-------+--------+
                          | generate
                          v
       no_bundle ---> current ---(walkback)--> warm
            ^             |                      |
            |             | (force-push)         | (>--bundle-warm-* threshold)
            |             v                      v
       retired <-------- no_ref              stale
            ^             ^                      |
            |             |                      |
            +-------------+----------------------+
                (next maintenance run regenerates from current ref tip)
```

Tuning guidance:

- `--bundle-warm-commits` default 5000. Increase for repos with high commit-rate-per-day (so `warm` stays valid between maintenance runs). Decrease for low-churn repos where bundle staleness is more noticeable.
- `--bundle-warm-age` default 24h. Decrease only if you run maintenance more often than once per day. Increase only if maintenance cadence is genuinely > 24h (this is unusual; most operators keep maintenance ≤ 24h).
- Hot rule: `warm` and `stale` both still advertise the bundle. The difference is observability — `stale` is the signal that maintenance is falling behind. If you alert on anything, alert on a sustained nonzero rate of `bundle_advertised_total{freshness=stale}` against legitimate advertise traffic.

- [ ] **Step 5: Fill §3 Maintenance Scheduling** (target 70-90 lines)

Replace `(filled in Step 5)` with §3. Required content:

Required prose:

- `bucketvcs maintenance --bundle-only` runs only the bundle phase. `bucketvcs maintenance --force` (without `--bundle-only`) runs the full pipeline: repack + reachability compaction + bundle refresh. For most operators, the right pattern is to run the full pipeline at the same cadence they run repack — the materialized mirror is reused once per pipeline run, so bundling is nearly free when colocated with repack.
- Co-scheduling rule: run repack + reachability compaction + bundle-refresh together. Do not schedule `--bundle-only` on a separate cron line unless you have a specific reason — it builds a temporary mirror that the full pipeline would otherwise have built anyway.

Three recipes:

1. **cron** (`/etc/cron.d/bucketvcs-maintenance` or user crontab):

   ```
   # Every 4 hours, run full maintenance for one specific repo.
   17 */4 * * *  bucketvcs  /usr/local/bin/bucketvcs maintenance \
                              --store=s3://my-bucket?endpoint=https://r2... \
                              --repo=tenant/repo \
                              --force \
                              >> /var/log/bucketvcs/maintenance.log 2>&1
   ```

   Use `--all-repos` instead of `--repo=...` to iterate every repo in the store.

2. **Kubernetes CronJob** (`bucketvcs-maintenance-cronjob.yaml`):

   ```yaml
   apiVersion: batch/v1
   kind: CronJob
   metadata:
     name: bucketvcs-maintenance
   spec:
     schedule: "17 */4 * * *"
     concurrencyPolicy: Forbid
     jobTemplate:
       spec:
         template:
           spec:
             restartPolicy: OnFailure
             containers:
               - name: maintenance
                 image: bucketvcs:latest
                 command:
                   - bucketvcs
                   - maintenance
                   - --store=s3://my-bucket?endpoint=https://r2...
                   - --all-repos
                   - --force
                 env:
                   - name: AWS_ACCESS_KEY_ID
                     valueFrom: { secretKeyRef: { name: r2-creds, key: access } }
                   - name: AWS_SECRET_ACCESS_KEY
                     valueFrom: { secretKeyRef: { name: r2-creds, key: secret } }
   ```

   `concurrencyPolicy: Forbid` is important: two concurrent maintenance runs against the same repo will fight on CAS and one will fail.

3. **systemd timer** (`/etc/systemd/system/bucketvcs-maintenance.{service,timer}`):

   ```ini
   # bucketvcs-maintenance.service
   [Unit]
   Description=bucketvcs maintenance run

   [Service]
   Type=oneshot
   ExecStart=/usr/local/bin/bucketvcs maintenance \
       --store=s3://my-bucket?endpoint=https://r2... \
       --all-repos \
       --force
   User=bucketvcs
   ```

   ```ini
   # bucketvcs-maintenance.timer
   [Unit]
   Description=bucketvcs maintenance every 4 hours

   [Timer]
   OnCalendar=*-*-* 0/4:17:00
   Persistent=true

   [Install]
   WantedBy=timers.target
   ```

   `Persistent=true` runs the timer immediately after a missed boot window. Useful for single-host deployments that may reboot mid-window.

Closing note: the M11 bundle phase has its own threshold logic for skipping bundle generation when the prior bundle is still current (the freshness state machine — see §2). You almost never need to gate maintenance externally; let the pipeline decide whether bundle work runs.

- [ ] **Step 6: Fill §4 Signed-URL vs Gateway-Proxied** (target 60-80 lines)

Replace `(filled in Step 6)` with §4. Full 4-mode table, with the proxied caveat REPEATED:

| Mode | Backend | Bandwidth path | Audit visibility | Multi-tenant ready? | When |
|------|---------|---------------|------------------|---------------------|------|
| `direct` | cloud (S3, R2, GCS, Azure Blob) | client → bucket | none at gateway | yes | public-internet repos; lowest gateway cost; egress charged to bucket |
| `proxied` | localfs OR cloud | client → gateway → bucket | full (every serve emits `proxied.url.served` audit event) | **NO — single-repo deployments only** | audit-strict single-repo deployments; localfs deployments (no signed URLs available) |
| `auto` | any | direct if backend supports signed URLs; proxied otherwise | partial (direct serves are invisible to gateway audit) | yes for direct-capable backends; NO for localfs | default for most operators; covers both backends without per-deploy config |
| `off` | any | n/a (capability disabled) | n/a | yes | fall back to standard fetch; useful for rollback or known-incompatible clients |

Prose elaboration:

- **Direct mode** mints a signed URL (S3-style presigned GET, GCS V4 signature, Azure SAS, R2 presigned). The client downloads from the bucket directly. The gateway sees the request to advertise the URL but never sees the bytes. TTL is bounded by `--proxied-url-bundle-ttl` (4h default) and `--proxied-url-pack-ttl` (1h default). The TTL must also satisfy the M8 retention rule: `TTL ≤ retention/24` (see §5).
- **Proxied mode** mints a gateway URL of the form `https://<gateway>/_bundle/<hash>?token=<HMAC>` (or `/_pack/<hash>`). The client downloads from the gateway, which streams from the bucket. Every serve emits `proxied.url.served`. The `ProxiedKeyResolver` that maps `<hash>` to a storage key is single-repo today; a multi-tenant resolver is deferred work. **Do not use proxied mode if you serve more than one bucketvcs repo through a single gateway.**
- **Auto mode** is the recommended default. The gateway picks direct on backends that can sign URLs; falls back to proxied on localfs. If you run a hybrid (cloud + localfs) deployment, set `auto` and the gateway routes correctly per backend.
- **Off mode** disables advertisement entirely. Use for rollback or when integrating with a client that mishandles bundle-uri (rare; stock git ≥ 2.41 is correct).

The Phase 12.5 retro notes a third class of failure under proxied mode that operators should know about: when the signing key file is rotated, all unexpired tokens minted against the old key produce a `proxied_url_token_invalid_total{reason=invalid}` metric. Plan signing-key rotations to align with the longest active TTL window.

- [ ] **Step 7: Fill §5 TTL vs M8 Retention** (target 30-50 lines)

Replace `(filled in Step 7)` with §5. Required content:

The hard rule (CLI-enforced): `TTL ≤ retention/24`. M11's TTL flags:

- `--proxied-url-bundle-ttl` — default `4h`. Maximum lifetime of a minted bundle URL (direct or proxied).
- `--proxied-url-pack-ttl` — default `1h`. Maximum lifetime of a minted pack URL.

M8's retention flag:

- `bucketvcs gc --retention-window` — default `168h` (7 days).

The constraint:

```
proxied-url-bundle-ttl ≤ retention-window / 24    → 4h ≤ 168h/24=7h    ✓ OK at defaults
proxied-url-pack-ttl   ≤ retention-window / 24    → 1h ≤ 168h/24=7h    ✓ OK at defaults
```

Why: a URL minted at the start of a TTL window references a bundle (or pack) by storage key. If that object is GC-swept before the URL expires, the client gets a 404 (direct mode) or 500 (proxied mode). The 24× safety factor accommodates GC scheduling jitter and the §43.6 race window (see [M8 §4](m8-gc-operator-guide.md#4-the-436-race-window)) — the TTL must be short enough that GC's next scheduled run will always run while the URL is still live, leaving no opportunity for the client to receive a URL referencing a swept object.

If you decrease `--retention-window` below 24h, `bucketvcs serve` will reject the configuration at startup. Tune retention upward, not TTL downward — TTL governs client-side cache windows, not just URL validity.

If you decrease retention to e.g. 48h (`--retention-window=48h`), reduce both TTLs proportionally: `--proxied-url-bundle-ttl=2h --proxied-url-pack-ttl=2h` (since `48h/24 = 2h`).

- [ ] **Step 8: Fill §6 Bandwidth and Cost Economics** (target 40-60 lines)

Replace `(filled in Step 8)` with §6. Required content:

The shift:

- Pre-M11: every clone's bytes flow through `bucketvcs serve` → gateway VM/container egress, gateway CPU for upload-pack. Bandwidth bill is gateway-side.
- M11 direct mode: bytes flow client → bucket directly (signed URL). Egress is billed by the object-storage provider (S3 egress, R2 egress-free, GCS egress, Azure egress). Gateway VM only sees the small advertise-bundle response.
- M11 proxied mode: bytes flow client → gateway → bucket. Gateway VM sees full egress AND bucket egress (twice the bandwidth for the same clone). The benefit is audit visibility.

How to reason about your deployment:

- **R2 (Cloudflare)** has zero egress fees. Direct mode is a strict cost win — there is no per-byte charge for bucket egress. Use direct mode whenever you do not need per-clone audit logs.
- **S3 / AWS** charges egress per GB. If the gateway VM is in the same region as the bucket, gateway-VM egress (proxied mode) is more expensive (each byte hits S3-egress AND VM-egress). Direct mode bypasses VM-egress. If the bucket is in a different region from the gateway, direct mode is also faster (one network hop instead of two).
- **GCS** is similar to S3. Direct mode saves the VM-egress hop.
- **Azure Blob** is similar. Direct mode saves the VM-egress hop.

For hosted bucketvcs deployments (the dispatch user runs a managed gateway), R2 with `auto` mode is the cheapest configuration: clones flow direct to R2 (no egress charge), pushes still flow through the gateway (CAS, auth), and the gateway VM stays small.

For air-gapped or audit-strict OSS deployments, `proxied` is the only option that gives you a `proxied.url.served` audit event per clone. Accept the 2× bandwidth cost for the audit benefit. Remember: single-repo only (until multi-tenant `ProxiedKeyResolver` lands — see §12).

- [ ] **Step 9: Fill §7 Disabling Acceleration** (target 20-30 lines)

Replace `(filled in Step 9)` with §7. Required content:

Single-command disable:

```bash
bucketvcs serve --bundle-uri-mode=off --pack-uri-mode=off  ...other flags...
```

What happens after `--bundle-uri-mode=off`:

- `command=bundle-uri` requests return an empty response (no URL advertised).
- `bundle_advertised_total{freshness=disabled}` increments per request.
- Clients fall back to standard `command=fetch`. M9/M10 paths (reachability index, delta chain, repack-aware pack walk) are unchanged.
- Maintenance still generates bundles. The bundle entries accumulate in the manifest; they are just not advertised. If you later re-enable bundle-uri, the freshness state machine evaluates them as-is — there is no special re-warming needed.

What happens after `--pack-uri-mode=off`:

- `packfile-uris` capability is not advertised. Stock clients silently downgrade to inline packs.
- `pack_uri_advertised_total` does not increment.

Rollback is fully reversible. M11 manifest fields are `omitempty`; the manifest schema does not change shape based on whether the gateway advertises or not. If you disable M11 in production, redeploy with the modes back to `auto` (or `direct`) and behavior resumes immediately — no manifest migration needed.

- [ ] **Step 10: Fill §8 Observability Reference** (target 220-280 lines)

This is the largest section. Replace `(filled in Step 10)` with §8. Use the structure below VERBATIM for the §8.1 metric tables and §8.2 audit-event tables; the closed vocabularies and field lists are spec-locked.

```markdown
M11 ships eleven metrics (three maintenance-side, eight gateway-side) and four audit events (two maintenance-side, two gateway-side). All metrics and audit events emit via slog. The format is one JSON line per metric or event; operators ingest into Loki, Vector, or any slog-compatible pipeline.

### 8.1 Metrics

#### 8.1.1 Maintenance-side metrics

Emitted from `internal/maintenance/log.go::emitBundleResultMetrics`, called inside `pipeline.go::emitFinalReport` when the maintenance pipeline ran the bundle phase.

| Metric | Labels | Semantics |
|--------|--------|-----------|
| `bundle_generated_total` | `outcome` ∈ {`success`, `noop`, `failure`}, `repo_id`, `trigger_reason` | Count of bundle-generation attempts. `success` = `BundleResult.Generated == true`. `noop` = generation skipped intentionally (the freshness trigger declined, OR a recoverable per-repo skip fired: `skipped_reachability_load_error`, `skipped_trigger_eval_error`). `failure` = unexpected error path. The `trigger_reason` label carries the specific cause string (e.g. `tip_unchanged`, `walk_distance_under_threshold`, `skipped_reachability_load_error`). |
| `bundle_generation_duration_seconds` | `repo_id` | Wall-clock seconds for the bundle-generation phase. Emitted on every pipeline run that produced a `BundleResult`, regardless of outcome. |
| `bundle_bytes` | `repo_id` | Final bundle file size in bytes. Emitted only when `BundleResult.Generated && BundleResult.ByteSize > 0`. |

#### 8.1.2 Gateway-side metrics

Emitted from `internal/gitproto/uploadpack/log.go::emitMetric` (HTTP gateway path) and `internal/gateway/log.go::emitMetric` (proxied serve path). Both paths reach the same metric names; operators do not need to distinguish them.

| Metric | Labels | Semantics + rate-amplification |
|--------|--------|-------------------------------|
| `bundle_advertised_total` | `repo_id`, `freshness` ∈ {`disabled`, `no_bundle`, `no_ref`, `current`, `warm`, `stale`, `retired`} | Per `command=bundle-uri` dispatch. **Includes the encode-error path** — this counts dispatch attempts, not successful encodes. **Rate-amplification: rogue or misconfigured clients can pump this at arbitrary rate.** Alert on the per-freshness rates for legitimate advertise traffic (`current` + `warm` + `stale`); do not alert on the raw total. |
| `bundle_uri_advertised_total` | `repo_id`, `via` ∈ {`proxied`, `direct`} | Only emitted when the bundle-uri response actually contains URLs (`freshness ∈ {current, warm, stale}`). The `via` label is derived from the URL path (`/_bundle/` → `proxied`, otherwise → `direct`). |
| `bundle_uri_served_total` | `via` | Per successful proxied bundle serve. **Counts truncated serves too** — io.Copy mid-stream errors still emit. Pair with `bundle_uri_served_bytes` and the `proxied.url.served` audit event's `status_code` field to distinguish full from truncated. |
| `bundle_uri_served_bytes` | `via` | Actual bytes written via `countingWriter`. May be less than the bundle's full size on client disconnect. |
| `pack_uri_advertised_total` | `repo_id`, `via` | Emitted when the `packfile-uris` stanza fires. |
| `pack_uri_served_total` | `via` | Per successful proxied pack serve. Truncation semantics same as `bundle_uri_served_total`. |
| `pack_uri_served_bytes` | `via` | Same shape as `bundle_uri_served_bytes`. |
| `proxied_url_token_invalid_total` | `reason` ∈ {`missing`, `expired`, `kind_mismatch`, `invalid`} | Per token-validation failure on `/_bundle/` or `/_pack/`. `missing` = no `token` query parameter. `expired` = signature past TTL. `kind_mismatch` = bundle token presented to `/_pack/` (or vice versa). `invalid` = signature failure. The user-facing 403 body collapses `kind_mismatch` to "invalid token" — do not rely on the response body to distinguish; use this metric. |

**Cardinality note.** `repo_id` is intentionally absent from all served-* metrics (`bundle_uri_served_*`, `pack_uri_served_*`, `proxied_url_token_invalid_total`). The proxied handler is hash-keyed via the single-repo `ProxiedKeyResolver`; there is no repo dimension at request time. When the multi-tenant `ProxiedKeyResolver` lands (see §12), this label will be added.

### 8.2 Audit events

All four events use the flat-attrs shape (`slog.Bool("audit", true)` + `slog.String("event", "...")` + top-level attribute fields). Grep with `jq 'select(.audit == true and (.event | startswith("bundle.")))'` to extract all M11 bundle audit events.

#### 8.2.1 Maintenance-side audit events

Both emitted from `internal/maintenance/bundle.go::runBundlePhase`, inside the `if !opts.DryRun { ... }` guard, after `RunBundleCASMerge` succeeds. Retired-before-generated emission order pairs the events atomically.

- **`bundle.generated`** — one event per generated bundle.
  Fields: `repo_id`, `bundle_id`, `bundle_hash`, `tip_oid`, `covers_manifest_version`, `byte_size`, `duration_ms`.
  Use case: dashboard ingestion of bundle production rate; join `bundle_id` against `bundle.retired.replaced_by` for replacement-pair tracking.

- **`bundle.retired`** — emitted before the paired `bundle.generated` when a CAS-merge supersedes an existing `full_default` entry.
  Fields: `repo_id`, `bundle_id` (the retired bundle's ID), `reason`, `replaced_by` (the new bundle ID from the paired `bundle.generated`).
  M11 `reason` vocabulary: `"replaced"` only. Future work will add `"gc_swept"` when full bundle GC pipeline lands (see §12).

DryRun mode emits no audit events.

#### 8.2.2 Gateway-side audit events

- **`bundle.uri.advertised`** — emitted from `serveBundleURI` when the response contains URLs (`freshness ∈ {current, warm, stale}`).
  Fields: `repo_id`, `freshness`, `via`, `bundle_count` (= 1 in M11; pluralized in a future milestone), `first_tip_oid` (matches the `bundle.generated.tip_oid` of the advertised bundle — operators correlate the two events).

- **`proxied.url.served`** — emitted from the proxied handler post-`io.Copy` on 200 or 206.
  Fields: `kind` ∈ {`bundle`, `pack`}, `hash`, `bytes_served` (via `countingWriter`, may be less than full size on disconnect), `status_code` ∈ {200, 206}, `range_request` (boolean — whether the client sent a `Range:` header).

### 8.3 Rate-amplification gotchas

Three properties of the M11 observability surface that operators MUST factor into alerting:

1. `bundle_advertised_total` increments per `command=bundle-uri` dispatch, regardless of whether URLs were returned, regardless of whether the response encoded successfully. A rogue or misconfigured client can pump this metric arbitrarily. **Alert on per-freshness rates** (`bundle_advertised_total{freshness="current"}` etc.) against legitimate traffic baselines; do not alert on the raw total.
2. `bundle_uri_served_total` and `pack_uri_served_total` increment when the proxied handler begins serving, before `io.Copy` completes. A connection that disconnects mid-serve increments both `*_served_total` and a partial `*_served_bytes`. To distinguish full from truncated serves, join against the `proxied.url.served` audit event's `status_code` and `bytes_served` fields.
3. All served-* metrics lack a `repo_id` label. Multi-repo proxied deployments cannot be observed per-repo until the multi-tenant `ProxiedKeyResolver` lands. If you operate multiple repos through a single gateway in proxied mode, you are in unsupported territory regardless of label cardinality (see §1 production-readiness matrix).

### 8.4 Example slog grep recipes

Grep all M11 audit events from a slog JSON-line stream:

```bash
jq -c 'select(.audit == true and ((.event | startswith("bundle.")) or .event == "proxied.url.served"))' \
    /var/log/bucketvcs/bucketvcs.json
```

Get the bundle-generation rate per repo for the last hour:

```bash
jq -c 'select(.event == "bundle.generated") | {time, repo_id, bundle_hash, byte_size}' \
    /var/log/bucketvcs/bucketvcs.json
```

Correlate a `bundle.uri.advertised` event to its generating `bundle.generated` event:

```bash
TIP_OID=$(jq -r 'select(.event == "bundle.uri.advertised") | .first_tip_oid' < session.json | head -1)
jq -c "select(.event == \"bundle.generated\" and .tip_oid == \"$TIP_OID\")" /var/log/bucketvcs/bucketvcs.json
```
```

After Step 10, run: `wc -l docs/m11-bundles-operator-guide.md`. Should be roughly 600-700 lines.

- [ ] **Step 11: Fill §9 Troubleshooting Matrix** (target 100-130 lines)

Replace `(filled in Step 11)` with §9. Required entries:

1. **Bundle never generated.** Symptom: `bundle_advertised_total{freshness="no_bundle"}` is the only nonzero freshness value; `bundle_generated_total{outcome="success"}` is zero. Likely causes: maintenance has never run with the bundle phase enabled (run `bucketvcs maintenance --bundle-only --force` for one repo to test), OR every maintenance run hit a `skipped_*` trigger reason (check `bundle_generated_total{outcome="noop"}` for `trigger_reason` distribution). Operator action: check the trigger_reason label; if it's `skipped_reachability_load_error`, the .bvom is corrupted or the reachability index is missing — likely needs a separate `bucketvcs maintenance --force` to rebuild.

2. **Clone is slow despite bundle being current.** Symptom: `bundle_advertised_total{freshness="current"}` is nonzero, but client-side clone times have not improved. Likely cause: the client did not opt in to bundle-uri. Verify with the client: `git -c transfer.bundleURI=true clone <url>` (note `transfer.bundleURI`, not `fetch.bundleURI`; the latter is for incremental fetch). For globally enabling on a client: `git config --global transfer.bundleURI true`.

3. **Proxied-URL 403 with `proxied_url_token_invalid_total{reason="expired"}`.** Cause: client downloaded a URL after its TTL expired. Reasons: long-duration clones (the URL was minted at clone start, expired before bundle download finished); clock skew between client and gateway; client retried after delay. Operator action: increase `--proxied-url-bundle-ttl` (subject to the §5 TTL ≤ retention/24 rule); investigate clock skew if it correlates with specific client subnets.

4. **Proxied-URL 403 with `proxied_url_token_invalid_total{reason="invalid"}`.** Cause: signing-key rotation invalidated unexpired tokens. The user-facing 403 says "invalid token" — does NOT distinguish `invalid` from `kind_mismatch`; check the metric label for the real reason. Operator action: align signing-key rotations with the longest active TTL window. If you must rotate immediately, expect a brief 403 spike until all in-flight URLs expire naturally (max `--proxied-url-bundle-ttl`).

5. **Direct-URL 403 from bucket.** Cause: TTL expired (same root cause as proxied `reason="expired"`), backend rejected the signature (clock skew between gateway and bucket, signing-key revocation at the cloud provider, or signature-version mismatch on AWS Sigv2/Sigv4). Operator action: check bucket-side audit logs (S3 server access logs, R2 audit, GCS data access logs). The gateway has no visibility into direct-mode failures.

6. **Pack-uri never advertised even though full clones happen.** Symptom: `pack_uri_advertised_total` is zero. Likely cause: the legacy pack does not have `PackChecksum` set. The packfile-uri gate (per Phase 8) requires `len(manifest.Packs) == 1` AND `manifest.Packs[0].PackChecksum != ""`. Pre-M11 manifests have empty `PackChecksum`; the lazy backfill runs on the next `bucketvcs maintenance` invocation. Operator action: run `bucketvcs maintenance --force` once per affected repo to backfill `PackChecksum`. Verify with `bucketvcs inspect-manifest --json | jq '.Packs[].PackChecksum'`.

7. **`bundle_advertised_total{freshness="stale"}` is sustained.** Cause: maintenance is falling behind the commit-rate-vs-threshold curve. The bundle is still advertised (clients still benefit), but staleness is observable. Operator action: tighten maintenance cadence, OR raise `--bundle-warm-commits` / `--bundle-warm-age` if your repo's normal commit rate has changed (e.g. you onboarded a new team and the commit rate doubled).

8. **Multi-repo deployment, proxied mode, intermittent serves to wrong repo.** Cause: the proxied handler is hash-keyed via the single-repo `ProxiedKeyResolver`. If two repos happen to have an object with the same hash (rare in practice — only canonical bundle/pack hashes are eligible), the resolver picks one deterministically; that may not be the requested repo. Operator action: switch to `direct` mode immediately; do not run proxied mode in multi-tenant deployments until the multi-tenant `ProxiedKeyResolver` lands (see §12).

- [ ] **Step 12: Fill §10 Migration from Pre-M11** (target 30-50 lines)

Replace `(filled in Step 12)` with prose for §10 Migration. Required content:

Migration is a three-step sequence:

1. **Deploy M11 binaries.** Replace `bucketvcs` binary across maintenance hosts and gateway hosts. Roll deployment as you normally would; M11 binary handles pre-M11 manifests identically except for the `PackChecksum` backfill.

2. **Run `bucketvcs maintenance --force` once per repo.** This:
   - Backfills `PackChecksum` for any single-pack repo where the field is empty (precondition for packfile-uri).
   - Generates the first `full_default` bundle entry per repo.
   - Updates the `.bvom` and `.bvcg` indexes to include bundle metadata.

   For repos managed by `--all-repos`, run `bucketvcs maintenance --all-repos --force` once. This is a one-time pre-rollout step; subsequent runs use thresholds normally.

3. **Enable serve flags.** Add `--bundle-uri-mode=auto --pack-uri-mode=auto --proxied-url-signing-key=/etc/bucketvcs/signing-key --proxied-url-base=https://gateway.example.com` to `bucketvcs serve` invocations. Restart the gateway.

Rollback safety:

- M11 manifest fields are `omitempty` — pre-M11 binaries read M11-shaped manifests without error.
- `bundle.generated` and `bundle.retired` audit events are additive; pre-M11 log consumers ignore them.
- To roll back, redeploy the old binary; manifest state remains compatible. The new `BundleEntry` array stays in place (pre-M11 binaries ignore it). On the next maintenance run with the old binary, the bundle entries do not refresh — they become `stale` and eventually `retired` per the freshness rules, but they do not corrupt anything.

- [ ] **Step 13: Fill §11 Forensics** (target 40-60 lines)

Replace `(filled in Step 13)` with §11. Required content:

Inspecting the manifest's bundle entries:

```bash
bucketvcs inspect-manifest --store=s3://my-bucket?endpoint=... --repo=tenant/repo --json | jq '.Bundles'
```

Output example:

```json
[
  {
    "ID": "bundle-20260512-abc123",
    "Kind": "full_default",
    "BundleHash": "f3e2b1...",
    "BundleKey": "bundles/f3/e2b1...",
    "SidecarKey": "bundles/f3/e2b1....json",
    "TipOID": "9c8b7a...",
    "CoversManifestVersion": 42,
    "ByteSize": 12345678,
    "GeneratedAt": "2026-05-12T10:23:45Z"
  }
]
```

Field meanings:

- `ID` — unique identifier for the entry; matches `bundle.generated.bundle_id` audit field.
- `Kind` — `"full_default"` in M11. Other kinds (e.g. release-tag bundles) are out of scope.
- `BundleHash` — content hash of the bundle file; appears in proxied URLs as `/_bundle/<BundleHash>`.
- `BundleKey` — storage key of the bundle blob.
- `SidecarKey` — storage key of the JSON sidecar with per-OID bundle metadata.
- `TipOID` — Git OID of the ref tip the bundle was generated against; matches `bundle.generated.tip_oid` and `bundle.uri.advertised.first_tip_oid` for cross-event correlation.
- `CoversManifestVersion` — manifest version at bundle-generation time; used by freshness state machine to detect staleness.
- `ByteSize` — bundle file size; matches `bundle.generated.byte_size`.

Post-incident grep cookbook (Loki / Vector / slog files):

```bash
# All bundle.generated events for one repo in the last 24h:
jq -c 'select(.event=="bundle.generated" and .repo_id=="tenant/repo")' /var/log/bucketvcs/*.json

# All retired bundles paired with their successors:
jq -c 'select(.event=="bundle.retired") | {time, retired_id: .bundle_id, replaced_by, repo_id}' \
    /var/log/bucketvcs/*.json

# All token-validation failures with reason breakdown:
jq -c 'select(.metric_name=="proxied_url_token_invalid_total")' /var/log/bucketvcs/*.json \
    | jq -s 'group_by(.reason) | map({reason: .[0].reason, count: length})'
```

For incidents involving a specific client clone failure, the order to grep:

1. Find the `bundle.uri.advertised` for that client (filter by `first_tip_oid` if known).
2. Trace its `bundle.uri.advertised.via` field. If `direct`, gateway has no further visibility — check bucket logs. If `proxied`, continue to step 3.
3. Find the corresponding `proxied.url.served` event(s). The `hash` field matches the advertised URL's path component.
4. If `proxied.url.served.status_code == 200` and `bytes_served` < expected size: truncated serve, client disconnect mid-stream.
5. If no `proxied.url.served` event found: the client never connected, OR the token failed validation — check `proxied_url_token_invalid_total` rates at the relevant timestamp.

- [ ] **Step 14: Fill §12 Deferred Work** (target 25-40 lines)

Replace `(filled in Step 14)` with §12. Required content (operators know what is coming):

M11 ships the bundle-uri + packfile-uri minimum viable acceleration. Several follow-up items are deferred to successor milestones; operators should plan around them:

- **Multi-tenant `ProxiedKeyResolver`.** Today's `ProxiedKeyResolver` resolves `/_bundle/<hash>` and `/_pack/<hash>` requests by hash alone, without per-repo discrimination. Multi-tenant deployments must use `--bundle-uri-mode=direct` or `=off`. When the multi-tenant resolver lands, `repo_id` will be added to the served-* metric labels and proxied mode will be GA for multi-tenant. **Until then, do not run proxied mode in multi-repo deployments.**

- **Full bundle GC pipeline.** `bucketvcs gc` does not yet sweep retired bundle blobs. Retired bundles linger in object storage at zero serve cost but nonzero storage cost. The `DiscoverBundles` API exists; production wiring (mark-phase integration, sweep-record extension) is the deferred work. When it lands, the `bundle.retired.reason` vocabulary gains a `"gc_swept"` value.

- **Lazy-path short-circuit for packfile-uri.** Today, the gateway invokes `pack-objects` even when the entire pack is URI-eligible (the inline pack is then `--keep-pack`-elided down to near-empty). A future optimization will skip `pack-objects` entirely when `FullPackRequested` holds. Cost-relevant for full-clone-heavy deployments.

- **Legacy `gateway.Options` URI fields.** The Phase 8 closure-pattern API (`BundleURIBuildURL`, `PackURIBuildURL`) coexists with the legacy fields (`BundleURIMode`, `BundleURITTL`, `ProxiedURLSigningKey`, etc.). The legacy fields are deprecated and will be removed once the closure pattern has soaked AND the multi-tenant `ProxiedKeyResolver` follow-up lands.

- **Concurrent bundle-safety conformance.** The `RunPropertyBundleSafety` factory ships solo localfs green; three concurrent sub-cases (`push_during_bundle`, `bundle_during_compaction`, `sweep_after_retire`) ship as `t.Skip` stubs deferred to M11.x. These test the manifest's atomicity guarantees under concurrent bundle generation + push + GC sweep; their absence does not affect production correctness today.

- **`FreshnessResult` sub-reason preservation in audit logs.** Operators wanting to distinguish stale-by-age from stale-by-walkback must currently grep `FreshnessResult` log lines (debug-grade) rather than relying on `bundle.uri.advertised.freshness`. A future enhancement will surface the detail on the audit event.

- [ ] **Step 15: Final length check and prose pass**

Run: `wc -l docs/m11-bundles-operator-guide.md`. Expected: 850-1100 lines. If below 850, sections are under-written — re-read against the spec §2 targets and elaborate. If above 1100, look for prose redundancy across sections (especially §4 vs §1, §6 vs §3, §11 vs §8).

Read the guide top-to-bottom once for prose flow. Common issues to fix:
- "We" / "you" / passive-voice inconsistency — the M9/M10 guides use "you" addressed to the operator; match that.
- Backtick consistency: flag names always `--bundle-uri-mode=...`; metric names always in backticks; event names always in backticks.
- Cross-references within the doc use `(§N)` not `(#section-n)` for readability in raw markdown. Cross-references to other M-guides use `[file.md](file.md#anchor)` format.

- [ ] **Step 16: Commit Task 13.1**

```bash
git add docs/m11-bundles-operator-guide.md
git commit -m "$(cat <<'EOF'
M11 Phase 13 Task 13.1: author operator guide

Single ~900-line operator guide for M11 bundle-uri + packfile-uri
acceleration. Twelve sections: production-readiness matrix, overview,
bundle freshness model, maintenance scheduling, signed-URL vs proxied
tradeoff, TTL vs M8 retention rule, bandwidth and cost economics,
disabling, full observability reference (eleven metrics + four audit
events + rate-amplification gotchas + slog grep recipes), eight-entry
troubleshooting matrix, pre-M11 migration recipe, forensics, deferred-work
appendix.

The multi-tenant proxied-mode gap is surfaced in three places (top
production-readiness matrix, mode tradeoff table, troubleshooting entry 8)
so an operator skimming any one entry point sees it. Bundle freshness
seven-state vocabulary, eleven metric names with closed label
vocabularies, and four audit event field lists are verified against
source files (internal/maintenance/log.go, internal/gateway/log.go,
internal/gitproto/uploadpack/log.go).

Per spec docs/superpowers/specs/2026-05-12-m11-phase-13-operator-guide-design.md.
EOF
)"
```

---

## Task 13.2 — Cross-references

**Files:**
- Modify: `docs/m9-maintenance-operator-guide.md`
- Modify: `docs/m8-gc-operator-guide.md`
- Modify: `README.md`

- [ ] **Step 1: Add "See also" pointer to M9 guide**

Open `docs/m9-maintenance-operator-guide.md`. Find the `### Reachability thresholds (M10)` subsection (around line 334). Insert a new `### Bundle thresholds (M11)` subsection AFTER it, before the `## 5. What Changes After a Successful Run` section.

Content to insert:

```markdown
### Bundle thresholds (M11)

Maintenance also generates default-branch bundles when M11 is enabled. The
bundle-specific flags (`--bundle-warm-commits`, `--bundle-warm-age`, the
freshness state machine that decides when a bundle counts as `current` /
`warm` / `stale` / `retired`) are documented separately. See
[M11 Bundles Operator Guide](m11-bundles-operator-guide.md), particularly
§2 Bundle Freshness Model for the tuning detail.

---

```

- [ ] **Step 2: Add forward-pointer to M8 guide**

Open `docs/m8-gc-operator-guide.md`. Find `### 3.3 The `< 24h` warning` (around line 278). Insert a new subsection AFTER §3.3 and before `### 3.4 Force-push workflows`:

```markdown
### 3.4 M11 TTL ≤ retention/24 rule

If you operate M11 bundle-uri or packfile-uri acceleration, the URL TTLs
configured on `bucketvcs serve` (`--proxied-url-bundle-ttl`,
`--proxied-url-pack-ttl`) must satisfy `TTL ≤ retention-window / 24`.
`bucketvcs serve` enforces this at startup. If you tighten `--retention`
below the default 168h, lower the M11 TTL flags proportionally. See
[M11 §5 TTL vs M8 Retention](m11-bundles-operator-guide.md#5-ttl-vs-m8-retention).

---

```

Then renumber the original `### 3.4 Force-push workflows` to `### 3.5 Force-push workflows`. Search the rest of the document for cross-references to `§3.4` — there should be none, but verify with `grep -n '§3.4\|3\.4' docs/m8-gc-operator-guide.md` before committing.

- [ ] **Step 3: Update README**

Open `README.md`. Two edits:

**Edit A: CLI subcommand bullet (around line 32).** Replace:

```
- `bucketvcs maintenance` — operator-driven repack + commit-graph / object-map refresh + reachability compaction per spec §15.3 (M9/M10)
```

with:

```
- `bucketvcs maintenance` — operator-driven repack + commit-graph / object-map refresh + reachability compaction + bundle generation per spec §15.3 / §16.3 (M9/M10/M11)
```

**Edit B: Add `bucketvcs serve` reference if missing.** Find the existing `bucketvcs serve` bullet (around line 34) and replace:

```
- `bucketvcs serve` — start the Git-protocol HTTPS/SSH gateway
```

with:

```
- `bucketvcs serve` — start the Git-protocol HTTPS/SSH gateway; advertises M11 bundle-URI (§16.3) and packfile-URI (§16.4) to v2-capable clients via direct signed URLs (cloud backends) or HMAC-gated gateway-proxied endpoints (localfs and audit-strict single-repo deployments)
```

**Edit C: Documentation link (after line 39).** Insert after the existing M10 link:

```
- [`docs/m11-bundles-operator-guide.md`](docs/m11-bundles-operator-guide.md) — M11 bundle-URI and packfile-URI acceleration
```

- [ ] **Step 4: Confirm no broken links**

Run: `grep -RIn "\(docs/m11-bundles-operator-guide.md" docs/ README.md`. Expected: at least 3 matches (the M9 guide pointer, the M8 guide pointer, and the README link). All targets should resolve — `ls docs/m11-bundles-operator-guide.md` returns the file.

- [ ] **Step 5: Commit Task 13.2**

```bash
git add docs/m9-maintenance-operator-guide.md docs/m8-gc-operator-guide.md README.md
git commit -m "$(cat <<'EOF'
M11 Phase 13 Task 13.2: cross-references

Adds M9 "Bundle thresholds (M11)" subsection pointing to the M11 operator
guide for bundle-warm tuning. Adds M8 §3.4 "M11 TTL <= retention/24 rule"
subsection (renumbering force-push to §3.5) so M8 operators see the M11
TTL constraint at the retention configuration point. README gains the M11
acceleration phrase on the bucketvcs serve subcommand bullet (with
proxied qualified to "single-repo deployments") plus the new docs link.
EOF
)"
```

---

## Task 13.3 — Verification and final commit

This task runs the verification gates from spec §5. No new files; the goal is to gate the milestone.

- [ ] **Step 1: Vocab consistency check — metric names in guide must appear in code**

Run:

```bash
# Extract metric/event names from the guide and confirm each exists in source:
grep -oE '`[a-z][a-z_.]+(_total|_seconds|_bytes|\.generated|\.retired|\.advertised|\.served)`' \
    docs/m11-bundles-operator-guide.md \
    | sort -u \
    | while read name; do
        literal=$(echo "$name" | tr -d '`')
        if ! grep -RIql "\"$literal\"" internal/maintenance/log.go internal/gateway/log.go internal/gitproto/uploadpack/log.go internal/maintenance/bundle.go; then
          echo "MISSING IN CODE: $literal"
        fi
      done
```

Expected output: empty. Every metric and event name in the guide appears as a quoted string in at least one of the listed source files. If any name is reported MISSING IN CODE, the guide cites a name that does not exist — fix the guide. If a name is in the guide as `bundle_byte_size`, it means Task 13.0 left a stale reference — fix the guide.

- [ ] **Step 2: Vocab consistency check — metric names in code must appear in guide (where operator-facing)**

Run:

```bash
# Extract operator-facing metric names from source:
grep -oE '"(bundle|pack|proxied)_[a-z_]+"' \
    internal/maintenance/log.go internal/gateway/log.go internal/gitproto/uploadpack/log.go \
    | sed 's/.*"\(.*\)".*/\1/' \
    | sort -u \
    | while read name; do
        if ! grep -q "\`$name\`" docs/m11-bundles-operator-guide.md; then
          echo "MISSING IN GUIDE: $name"
        fi
      done
```

Expected output: empty. If a name reported is genuinely internal (e.g. helper-only, not operator-facing), document why it should not be in the guide and surface as a finding rather than silently adding it.

- [ ] **Step 3: Internal-link sanity check**

Run:

```bash
# Find all internal markdown links in the guide:
grep -oE '\]\([^)]+\)' docs/m11-bundles-operator-guide.md \
    | sed 's/](\(.*\))/\1/' \
    | grep -E '\.md(#|$)' \
    | while read target; do
        path=$(echo "$target" | cut -d'#' -f1)
        if [ "$path" = "" ]; then continue; fi
        if [ ! -f "docs/$path" ] && [ ! -f "$path" ]; then
          echo "BROKEN LINK: $target"
        fi
      done
```

Expected output: empty. Every relative `.md` link in the guide points to a file that exists.

- [ ] **Step 4: CLI flag smoke**

Run:

```bash
# Every --flag mentioned in the guide should exist on at least one bucketvcs subcommand.
grep -oE '\-\-[a-z][a-z-]+' docs/m11-bundles-operator-guide.md \
    | sort -u \
    | while read flag; do
        if ! go run ./cmd/bucketvcs maintenance --help 2>&1 | grep -q -- "$flag" \
           && ! go run ./cmd/bucketvcs serve --help 2>&1 | grep -q -- "$flag" \
           && ! go run ./cmd/bucketvcs gc --help 2>&1 | grep -q -- "$flag" \
           && ! go run ./cmd/bucketvcs inspect-manifest --help 2>&1 | grep -q -- "$flag" \
           && ! go run ./cmd/bucketvcs import --help 2>&1 | grep -q -- "$flag" \
           && ! go run ./cmd/bucketvcs init --help 2>&1 | grep -q -- "$flag"; then
          echo "UNKNOWN FLAG IN GUIDE: $flag"
        fi
      done
```

Expected output: empty. If any flag is reported UNKNOWN, the guide cites a flag that does not exist on any subcommand — typo, or feature that was renamed.

Allowable false-positives: flags inside YAML examples (CronJob spec uses `--store`, `--all-repos`, `--force` — all valid). If a flag is purely illustrative (`--bundle-warm-commits=N` where N is a placeholder), the flag itself must still be real.

- [ ] **Step 5: Length sanity check**

Run: `wc -l docs/m11-bundles-operator-guide.md`

Expected: between 850 and 1100 lines.

If below 850: a section was under-written. Compare against spec §2 targets and elaborate.
If above 1100: prose redundancy. Re-read for cross-section repetition.

- [ ] **Step 6: Final smoke — full test suite untouched**

Run: `go test ./... -count=1`

Expected: PASS. Sanity check that the Task 13.0 rename did not break anything outside `internal/maintenance/`.

- [ ] **Step 7: `go vet`**

Run: `go vet ./...`

Expected: no output.

- [ ] **Step 8: Confirm branch is ready for squash**

Run: `git log --oneline main..HEAD`

Expected: three commits in order:
1. Task 13.0: rename bundle_byte_size -> bundle_bytes
2. Task 13.1: author operator guide
3. Task 13.2: cross-references

If your output has more (e.g. follow-up review-fix commits per the M1+ review protocol), that is expected — the branch will be squashed to main at merge time.

- [ ] **Step 9: No commit at this step**

Task 13.3 is a verification gate, not a code-change task. If everything in Steps 1-8 passed, the phase is complete. Surface a brief verification summary; the squash to main is the next operator-driven action, not part of this plan.

If any of Steps 1-8 failed, return to the relevant earlier task and fix. Re-run Steps 1-8 in full after the fix.

---

## Review protocol (per [[m1_review_protocol]])

Each task gets:

1. **superpowers spec review** (per task) — checks the implementation against this plan.
2. **superpowers code-quality review** — Task 13.0 only (the rename is code).
3. **superpowers doc-quality / writing-clearly review** — Task 13.1 only (the guide prose).
4. **roborev-refine on max reasoning** — single pass per task; iterate until pass or diminishing returns.

Task 13.2 and Task 13.3 get spec review only (mechanical edits and verification gates).

If any review finds a metric name or audit-event field that the guide cites but source disagrees, this is a real bug — fix the guide (or surface that source needs correction). Do not paper over with "documented as intended."

---

## Self-review checklist

- Every spec requirement in `docs/superpowers/specs/2026-05-12-m11-phase-13-operator-guide-design.md` is implemented by a task above:
  - Spec §0 Goal/scope: deliverables = Task 13.1 (guide), Task 13.2 (cross-refs); out-of-scope items surfaced in §12 of the guide written in Task 13.1 Step 14.
  - Spec §1 Production readiness matrix: written in Task 13.1 Step 2 (scaffold) and reinforced in §4 (Step 6) and §9 (Step 11 entry 8).
  - Spec §2 Document outline: 12 sections, sized per Step 3 through Step 14. Length target verified Task 13.3 Step 5.
  - Spec §3 Observability reference content: written verbatim in Task 13.1 Step 10; vocab cross-checked Task 13.1 Step 1 and Task 13.3 Steps 1 + 2.
  - Spec §4 Task shape: Task 13.0 metric rename = Steps 1-9 of the rename task; Task 13.1 guide authoring = Steps 1-16; Task 13.2 cross-references = Steps 1-5; Task 13.3 verification = Steps 1-9.
  - Spec §5 Verification: vocab consistency (Task 13.3 Steps 1-2), link sanity (Step 3), CLI flag smoke (Step 4), length (Step 5), test suite (Step 6).
  - Spec §6 Open questions closed: addressed in the plan deliverables (the rename + the production-readiness matrix + the obs reference + the single-doc layout).
  - Spec §7 Cross-references: external links inserted in Task 13.2; internal cross-refs verified Task 13.3 Step 3.
- No placeholder language ("TBD", "fill in", "add appropriate") in any step.
- Every code change shows the exact before/after string.
- Every command shows the exact invocation and expected output shape.
