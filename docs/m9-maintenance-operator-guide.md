# M9 Operator Guide: `bucketvcs maintenance`

This guide is for operators who deploy, schedule, and monitor `bucketvcs maintenance`
in production. It covers what the command does, when to run it, how to schedule it
alongside `bucketvcs gc`, how to tune the three threshold triggers, what changes after
a successful run, how to read the JSON output, and what to do when a run fails.

---

## 1. What `bucketvcs maintenance` Does

`bucketvcs maintenance` is a one-shot, operator-scheduled CLI command. It takes a
single bucketvcs repository (or every repository discovered under the store), downloads
all current canonical pack files into a temporary bare Git repository, repacks them
into a single consolidated pack via `git pack-objects`, rebuilds the object-map
(`.bvom`) and commit-graph (`.bvcg`) indexes against the new pack, and atomically
replaces the manifest's pack list via a compare-and-swap write.

After a successful run:

- `manifest.Packs` collapses from N entries to exactly 1 (the new full-repack pack),
  plus any packs that arrived via concurrent pushes during the run.
- `manifest.Indexes.ObjectMap` and `.CommitGraph` are fresh, covering the new pack.
- The new canonical pack carries a `.bitmap` sidecar at
  `packs/canonical/<pack-id>.bitmap`, recorded as `PackEntry.BitmapKey` on the
  manifest (M9.5+). The lazy mirror's real `git upload-pack` reads it on clone to
  short-circuit the per-object reachability walk. `receive-pack`-written packs do
  NOT carry bitmaps — they are small, recent, and replaced by the next repack.
  Note: the first maintenance run after upgrading from M9 → M9.5 produces a new
  pack-id for every repo, even on identical input — M9.5 invokes
  `pack-objects --revs --all` directly while M9 piped through `rev-list`, and
  the two paths choose different delta encodings. Downstream tooling that pins
  pack-ids across milestones should refresh after upgrade.
- The old canonical packs and their old indexes (and any orphaned `.bitmap` files
  from earlier maintenance runs) become unreachable from the manifest.
- `bucketvcs gc`, on its next scheduled run after the retention window elapses, sweeps
  the unreachable packs, indexes, and bitmaps.

`bucketvcs maintenance` does not delete any objects and does not modify ref state. It
only restructures pack layout. Object-level reclamation remains `bucketvcs gc`'s job.

---

## 2. When to Run It

Maintenance is a one-shot CLI command. Schedule it with whatever process scheduler
your infrastructure provides.

### 2.1 Cadence guidelines

| Repository size | Suggested cadence | Flag notes |
|-----------------|-------------------|------------|
| Small (<100 commits, <10 active developers) | Weekly | `--force` to skip threshold check |
| Medium (hundreds of commits, tens of developers) | Daily | `--force` or let thresholds gate |
| Hot-large (1000+ pushes/day) | Every 6 hours | Default thresholds; omit `--force` |

For hot-large repos, omit `--force` so the thresholds act as a gate. If the repo
has been quiet since the last run, maintenance exits as a no-op without doing any
IO. This lets you run the command frequently without wasting resources during low-push
windows.

### 2.2 Special occasions

**Right after `bucketvcs import`**: an import produces one pack per imported branch
or ref batch. A maintenance run immediately after import consolidates those into a
single canonical pack with a fresh `.bvom` and `.bvcg`, which reduces subsequent
fetch overhead. Use `--force` so it runs regardless of pack count.

**After bulk mirror operations**: mirroring a large external repository produces many
push packs. A maintenance run consolidates them.

```bash
# Import, then immediately consolidate
bucketvcs import --store=s3://my-bucket --repo=acme/site ...
bucketvcs maintenance --store=s3://my-bucket --repo=acme/site --force
```

---

## 3. Scheduling Recipes

Run `bucketvcs maintenance` before `bucketvcs gc` in the same schedule window.
Maintenance consolidates packs; GC then reclaims the old packs that maintenance
made unreachable. Either order is safe, but maintenance-first maximizes what GC
reclaims on the same run.

### 3.1 cron

```cron
# /etc/cron.d/bucketvcs-maintenance
# Run maintenance over all repos at 02:00 every night, then GC at 02:30.
0 2 * * * bucketvcs /usr/local/bin/bucketvcs maintenance \
    --all-repos \
    --store=s3://my-bucket \
    --output=json \
    >> /var/log/bucketvcs-maintenance.log 2>&1
30 2 * * * bucketvcs /usr/local/bin/bucketvcs gc \
    --all-repos \
    --store=s3://my-bucket \
    --retention=168h \
    >> /var/log/bucketvcs-gc.log 2>&1
```

For a GCS or Azure backend, substitute the store URL:

```cron
0 2 * * * bucketvcs /usr/local/bin/bucketvcs maintenance \
    --all-repos \
    --store=gcs://my-gc-bucket \
    --output=json \
    >> /var/log/bucketvcs-maintenance.log 2>&1
```

For a hot-large repo on a 6-hour cadence:

```cron
0 */6 * * * bucketvcs /usr/local/bin/bucketvcs maintenance \
    --repo=acme/site \
    --store=s3://my-bucket \
    --output=json \
    >> /var/log/bucketvcs-maintenance.log 2>&1
```

### 3.2 Kubernetes CronJob

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: bucketvcs-maintenance
  namespace: bucketvcs
spec:
  schedule: "0 2 * * *"
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 5
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          serviceAccountName: bucketvcs-maintenance
          containers:
            - name: maintenance
              image: your-registry/bucketvcs:latest
              command:
                - /usr/local/bin/bucketvcs
                - maintenance
                - --all-repos
                - --store=s3://my-bucket
                - --output=json
              env:
                - name: AWS_REGION
                  value: us-east-1
                - name: AWS_ACCESS_KEY_ID
                  valueFrom:
                    secretKeyRef:
                      name: bucketvcs-maintenance-creds
                      key: access-key-id
                - name: AWS_SECRET_ACCESS_KEY
                  valueFrom:
                    secretKeyRef:
                      name: bucketvcs-maintenance-creds
                      key: secret-access-key
              resources:
                requests:
                  cpu: "200m"
                  memory: "512Mi"
                limits:
                  cpu: "2"
                  memory: "4Gi"
```

Notes:

- `concurrencyPolicy: Forbid` prevents two maintenance jobs from racing against the
  same store. Concurrent runs are safe (both will do correct CAS-merges) but waste
  IO and produce duplicate upload artifacts that GC must later sweep. Forbid
  eliminates accidental concurrent runs at no cost.

- `restartPolicy: Never` prevents Kubernetes from retrying a failed maintenance job.
  A CAS-exhaustion failure (exit 1) needs human triage to determine push rate.

- Set `memory.limits` based on the total size of your canonical packs: maintenance
  downloads all packs into a temp directory and then repacks them, so peak disk and
  memory usage is proportional to pack-set size. The repack step itself is git's
  `pack-objects` subprocess; the Go process only streams.

- If you use a separate CronJob for `bucketvcs gc`, schedule it 30 minutes after
  maintenance to give maintenance time to complete before GC runs.

### 3.3 systemd timer

Two files: a `.service` and a `.timer`. Both live in `/etc/systemd/system/`.

**`/etc/systemd/system/bucketvcs-maintenance.service`**:

```ini
[Unit]
Description=bucketvcs pack maintenance
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=bucketvcs
Group=bucketvcs
ExecStart=/usr/local/bin/bucketvcs maintenance \
    --all-repos \
    --store=s3://my-bucket \
    --output=json
StandardOutput=journal
StandardError=journal
SyslogIdentifier=bucketvcs-maintenance
```

**`/etc/systemd/system/bucketvcs-maintenance.timer`**:

```ini
[Unit]
Description=Run bucketvcs maintenance nightly at 02:00
Requires=bucketvcs-maintenance.service

[Timer]
OnCalendar=*-*-* 02:00:00
RandomizedDelaySec=120
Persistent=true

[Install]
WantedBy=timers.target
```

Enable and start:

```bash
systemctl daemon-reload
systemctl enable --now bucketvcs-maintenance.timer
```

Verify the next trigger time:

```bash
systemctl list-timers bucketvcs-maintenance.timer
```

`Persistent=true` means that if the host was down at 02:00, systemd runs the timer
as soon as the host comes back up. This is the right behavior for maintenance: a
missed run accumulates pack fragmentation, not correctness risk.

---

## 4. Threshold Tuning

### 4.1 The four Phase-0 repack triggers

Maintenance evaluates four cheap-first threshold triggers at the start of each run (Phase 0).
The reachability-compaction triggers documented in §15 and the bundle-regeneration triggers
in §11 are evaluated separately — they fire the compact-only path or the bundle-refresh
phase respectively, not the full repack path.

If none of the four repack triggers fire and `--force` is not set, the run exits as a no-op
with `outcome=noop`.

| Flag | Default | What it measures | When it fires |
|------|---------|-----------------|---------------|
| `--recent-pack-threshold` | 1000 | Count of canonical packs created within `--recent-window` (default 24h) | More than N packs arrived in the last 24 hours |
| `--total-pack-threshold` | 10000 | Total count of canonical packs in the manifest | Manifest has more than N packs total |
| `--manifest-pack-bytes-threshold` | 8388608 (8 MiB) | JSON byte size of `manifest.Packs` | Pack metadata alone exceeds 8 MiB |
| `--bitmap-coverage-pct` (M9.5+) | 100 | Percent of canonical packs carrying a `.bitmap` sidecar | Fewer than N% of packs have a bitmap |

The first trigger that fires is reported in the text and JSON output as the `trigger`
reason. Triggers are evaluated cheap-first: `total_pack_count` and `manifest_pack_bytes`
are O(1) on the manifest body and decide on every run; `recent_pack_count` requires one
HEAD per pack and is only computed when the cheaper triggers haven't already fired AND
`--recent-pack-threshold > 0`. The JSON `trigger_eval.recent_pack_count` field will
therefore report `0` whenever a cheaper trigger fired or the recent-pack trigger was
disabled — that's not the actual count of recent packs, just the substrate the
short-circuit returned. The other two pack-count fields (`total_pack_count`,
`manifest_pack_bytes`) are always populated.

`bitmap_coverage_pct` is also always populated for observability (it's a cheap O(N) scan
of `manifest.Packs`), but it sets `triggered`/`reason` only when no higher-priority
trigger has already fired. The reason string is `bitmap_coverage(<pct>%<<threshold>%)`,
e.g. `bitmap_coverage(0%<100%)` for a pre-M9.5 manifest with the default threshold.

Setting any threshold to `0` disables that specific trigger. Setting all three to `0`
makes every run a no-op unless `--force` is also set — this is a valid configuration
for repos where you want manual-only maintenance.

### 4.2 Default rationale

**Recent pack count (default 1000)**: A repo receiving 1000+ pushes per 24-hour window
has accumulated enough pack fragmentation that fetch latency is measurably impacted.
At that scale, the object-to-pack lookup table in the `.bvom` has 1000+ entries, each
requiring a separate pack read on a cache miss.

**Total pack count (default 10000)**: This is a hard backstop. A manifest with 10000
pack entries is large enough that manifest reads — which happen on every push and every
fetch — dominate latency. Ten thousand packs is not a normal operating state; reaching
this threshold means the recent-pack trigger was either disabled or the repo received
no maintenance for an extended period.

**Manifest pack bytes (default 8 MiB)**: The manifest is read on every push and fetch.
At 8 MiB of pack metadata, the manifest body parse time is on the order of tens of
milliseconds on a modern server. That's a meaningful fraction of a push round-trip.
Each pack entry in the manifest is roughly 200-300 bytes of JSON, so 8 MiB corresponds
to approximately 27,000-40,000 entries — far past the point where fetch latency
degradation is observable. This threshold catches the case where pack entries are small
but numerous, even if the pack counts alone haven't crossed their thresholds.

**Worked example**: A repo whose manifest's pack metadata exceeds 8 MiB has accumulated
thousands of pack entries. Every fetch that misses the `.bvom` cache must walk that list
to find which pack contains a given object. After a maintenance run, the list collapses
to 1 entry. The `.bvom` covers only the new consolidated pack. Object lookup becomes
O(1) against the index.

### 4.3 `--force`

`--force` skips threshold evaluation entirely. The run always proceeds through all
phases. Use it for:

- Post-import warmup runs (the repo is fresh, thresholds have not yet been exceeded,
  but you want an optimized pack layout now).
- Scheduled weekly runs on small repos where you want deterministic behavior regardless
  of push activity.
- Manual operator intervention after a known event (bulk import, large mirror operation).

### 4.4 `--dry-run`

`--dry-run` runs Phase 0 (load manifest + threshold evaluation) and Phase 1
(materialize bare repo and verify with `git fsck`) but writes nothing. It reports
what would happen: whether thresholds are exceeded, how many objects would be repacked,
and the projected manifest-pack-bytes after the run.

Text output prefixes each line with `[DRY RUN]`. JSON output sets `"dry_run": true`.
Exit code is always 0 on a dry run.

Use `--dry-run` before changing thresholds, before the first run on a new repo, or to
verify that a repo is healthy before committing to a full repack.

```bash
bucketvcs maintenance --repo=acme/site --store=s3://my-bucket --dry-run
```

### 4.5 `--recent-window`

The `--recent-window` flag (default `24h`, minimum `1h`) sets the time window used
to count "recent" packs for the `--recent-pack-threshold` trigger. A pack is
"recent" if its object-store creation timestamp is within the window.

Tightening the window (e.g., `--recent-window=1h` with `--recent-pack-threshold=100`)
makes the trigger more sensitive to short burst activity. Widening it (e.g.,
`--recent-window=168h`) smooths over weekly cycles. Values below `1h` are rejected
with exit code 2.

### Reachability thresholds (M10)

M10 adds three thresholds (`--reachability-delta-commits`, `--reachability-delta-pushes`,
`--reachability-delta-bytes`) and a new "compact-only" outcome — maintenance refreshes
`.bvom` and `.bvcg` without producing a new pack. See `docs/m10-reachability-operator-guide.md`
for tuning guidance and the cold-fetch SLO contract.

### Bundle thresholds (M11)

Maintenance also generates default-branch bundles when M11 is enabled. The
bundle-specific flags (`--bundle-warm-commits`, `--bundle-warm-age`, the
freshness state machine that decides when a bundle counts as `current` /
`warm` / `stale` / `retired`) are documented separately. See
[M11 Bundles Operator Guide](m11-bundles-operator-guide.md), particularly
§2 Bundle Freshness Model for the tuning detail.

### Bitmap-coverage threshold (M9.5)

M9.5 adds `--bitmap-coverage-pct` (default 100). The trigger fires when fewer
than N% of canonical packs carry a `.bitmap` sidecar — i.e. when
`PackEntry.BitmapKey` is empty on more packs than the threshold tolerates.

| Coverage % | Default behavior | When to dial down |
|---|---|---|
| `100` | Strictest. Suitable for production. Pre-M9.5 manifests drain to fully-bitmapped on the next maintenance run. | Default; do not change without a reason. |
| `50-99` | Tolerant of one or two bitmap-less packs. | A repo where `pack-objects` occasionally declines to emit a bitmap (rare; usually indicates a degenerate ref set). |
| `0` | Disabled. No coverage check. | Repos where bitmap coverage should NOT drive repack scheduling — e.g. testing the other triggers in isolation, or during a controlled rollout of M9.5 across a fleet where you want to enable per-cluster after observing the metric. |

Operationally, bitmaps are produced by `git pack-objects --write-bitmap-index` during
the repack phase and uploaded alongside `.pack`/`.idx` to
`packs/canonical/<id>.bitmap`. The lazy mirror's real `git upload-pack` consumes them
on clone to short-circuit the per-object reachability walk; this is upstream-tooling
acceleration only — the pure-Go upload-pack negotiator (M10) does not read `.bitmap`.

`pack-objects` can decline to emit a bitmap in degenerate cases (empty pack, `--all`
resolving to no refs). The repack phase tolerates a missing `.bitmap` file and records
an empty `PackEntry.BitmapKey` rather than failing — the next maintenance cycle will
retry. A persistently empty bitmap field across multiple runs indicates an unusual
ref-graph shape and should prompt investigation — `trigger_eval.bitmap_coverage_pct`
stays below threshold and fires the trigger on every run, which gives operators a
clean signal
in the JSON output.

`receive-pack`-written packs never carry bitmaps (small, recent, replaced by the next
repack) and are expected to show `BitmapKey: ""` until the next maintenance run rolls
them into the consolidated canonical pack.

#### Operational hazard: trigger fires after any push at default 100%

`computeBitmapCoverage` uses integer arithmetic: `(packsWithBitmap * 100) / totalPacks`.
At `--bitmap-coverage-pct=100`, the trigger therefore fires whenever ANY canonical
pack lacks a bitmap — which is true immediately after every `receive-pack`-written
push, because those packs never carry bitmaps. In practice this means at the default
threshold, maintenance will repack on every run that follows a push. That is exactly
the design intent (drain `receive-pack` packs into the bitmapped consolidated pack
quickly), but operators wanting less aggressive repack cadence — for example, sites
that run maintenance hourly on a push-heavy repo and would rather defer repack to
when the other triggers fire — should set `--bitmap-coverage-pct` to a value like
`50` (force repack only when half or more of packs lack bitmaps) or `0` (disable the
trigger entirely; rely on `--recent-pack-threshold` for cadence).

#### Operational hazard: persistent bitmap upload failures cause repack churn

If the bitmap PUT to the object store persistently fails (auth scope misconfiguration,
key length, backend rejecting the content type), the manifest records `BitmapKey: ""`
for the new pack — and the next maintenance run sees coverage below 100% and forces
another full repack, which retries the bitmap upload, which fails again, and so on.
Bitmap is meant to be a cheap accelerator; a stuck upload turns into expensive repack
churn.

The signal in the run report is `bitmap_upload_error` (set on the JSON report when
the upload was attempted and a non-`ErrAlreadyExists` error came back). The signal
in the trigger eval is `trigger_eval.reason = "bitmap_coverage(N%<100%)"` on two
consecutive runs.

Runbook:
1. Read `bitmap_upload_error` from the last two maintenance reports — if both are
   non-empty with similar error text, the upload is stuck.
2. Mitigate by setting `--bitmap-coverage-pct=0` on subsequent runs while
   diagnosing the underlying storage error. This stops the repack churn without
   disabling maintenance.
3. Once the root cause is fixed (auth, capacity, content-type policy on the
   backend), re-enable `--bitmap-coverage-pct=100`. The next maintenance run repacks
   once, uploads the bitmap successfully, and steady state resumes.

---

## 5. What Changes After a Successful Run

A successful maintenance run (outcome=success) produces the following observable
changes:

| Manifest field | Before | After |
|----------------|--------|-------|
| `manifest.version` | N | N+1 |
| `manifest.Packs` | N entries (all canonical packs) | 1 entry (new consolidated pack) + any concurrent-push packs that landed during the run |
| `manifest.Indexes.ObjectMap` | Points to old `.bvom` | Points to new `.bvom` covering the new pack |
| `manifest.Indexes.CommitGraph` | Points to old `.bvcg` | Points to new `.bvcg` covering the new pack |
| `manifest.Refs` | Unchanged | Unchanged |
| `manifest.DefaultBranch` | Unchanged | Unchanged |

**Concurrent-push packs**: if one or more pushes landed while maintenance was running,
the CAS-merge detects them and appends those packs to the new manifest's pack list.
This preserves reachability for any commits added during the maintenance window. The
next maintenance run will consolidate those too.

**Old artifacts**: the old canonical packs and old indexes are no longer referenced by
the manifest after the CAS-merge. They are not deleted by maintenance. `bucketvcs gc`,
on its next scheduled run after the retention window elapses, identifies them as
unreachable and sweeps them.

---

## 6. JSON Output Schema

Use `--output=json` to get a machine-readable report. The output is a JSON array,
one object per repo. In single-repo mode (`--repo`), the array has exactly one element.
An empty `--all-repos` result is `[]`, never `null`.

```json
[
  {
    "repo_id": "acme/site",
    "outcome": "success",
    "dry_run": false,
    "manifest_version_at_start": 12,
    "manifest_version_after": 13,
    "trigger_eval": {
      "triggered": true,
      "reason": "recent_pack_count(1247>1000)",
      "recent_pack_count": 1247,
      "total_pack_count": 1247,
      "manifest_pack_bytes": 9856432,
      "thresholds": {
        "RecentPackCount": 1000,
        "TotalPackCount": 10000,
        "ManifestPackBytes": 8388608
      }
    },
    "before_pack_count": 1247,
    "after_pack_count": 1,
    "before_manifest_pack_bytes": 9856432,
    "after_manifest_pack_bytes": 191204,
    "new_pack_key": "packs/canonical/pack-a3f82c...pack",
    "new_pack_objects": 482310,
    "new_pack_bytes": 2459136000,
    "new_object_map_key": "indexes/object-map/d7e3c1....bvom",
    "new_commit_graph_key": "indexes/commit-graph/88ab42....bvcg",
    "repacked_pack_keys": [
      "packs/canonical/pack-001....pack",
      "packs/canonical/pack-002....pack"
    ],
    "cas_attempts": 1,
    "duration_ms": 47320
  }
]
```

### 6.1 Field reference

| Field | Type | Description |
|-------|------|-------------|
| `repo_id` | string | `<tenant>/<repo>` |
| `outcome` | string | One of: `success`, `noop`, `failed_walk`, `failed_pack_write`, `failed_cas`, `failed_other` |
| `dry_run` | bool | True if the run was a dry run; no writes were made |
| `manifest_version_at_start` | uint64 | Manifest version read at Phase 0 |
| `manifest_version_after` | uint64 | Manifest version after successful CAS; omitted on noop and failure |
| `trigger_eval.triggered` | bool | Whether any threshold fired |
| `trigger_eval.reason` | string | First threshold that fired, e.g. `recent_pack_count(1247>1000)`; empty on noop |
| `trigger_eval.recent_pack_count` | int | Observed value for recent-pack-count trigger |
| `trigger_eval.total_pack_count` | int | Observed value for total-pack-count trigger |
| `trigger_eval.manifest_pack_bytes` | int64 | Observed value for manifest-pack-bytes trigger |
| `trigger_eval.thresholds` | object | The configured thresholds that were evaluated |
| `before_pack_count` | int | Number of canonical packs in the manifest before the run |
| `after_pack_count` | int | Number of canonical packs in the manifest after the run |
| `before_manifest_pack_bytes` | int64 | JSON byte size of `manifest.Packs` before the run |
| `after_manifest_pack_bytes` | int64 | JSON byte size of `manifest.Packs` after the run |
| `new_pack_key` | string | Object-store key for the consolidated pack; omitted on noop and failure |
| `new_pack_objects` | int | Number of objects in the new pack |
| `new_pack_bytes` | int64 | Byte size of the new `.pack` file |
| `new_object_map_key` | string | Object-store key for the new `.bvom` |
| `new_commit_graph_key` | string | Object-store key for the new `.bvcg` |
| `repacked_pack_keys` | []string | Keys of the canonical packs that were consolidated; always a non-null array |
| `cas_attempts` | int | Number of CAS-merge attempts in Phase 6; 1 means no contention |
| `duration_ms` | int64 | Wall-clock duration of the full run in milliseconds |

### 6.2 Text output

Text mode emits one line per repo. A `[DRY RUN]` prefix appears when `--dry-run` is
set:

```
acme/site: outcome=success pack_count=1247→1 manifest_pack_bytes=9856432→191204 cas_attempts=1 duration=47320ms trigger=recent_pack_count(1247>1000)
[DRY RUN] acme/api: outcome=noop pack_count=3→3 manifest_pack_bytes=614→614 cas_attempts=0 duration=18ms
```

---

## 7. Failure-Mode Runbook

### 7.1 CAS exhaustion (`outcome=failed_cas`)

**What happened**: the CAS-merge in Phase 6 could not win the manifest compare-and-swap
within the configured retry limit (default 5, tunable via `--cas-retry`). This happens
when push traffic is so high that another manifest version lands before each of the
retry attempts.

**What to check**: look at `cas_attempts` in the output. If it equals `--cas-retry`,
the repo experienced sustained concurrent pushes throughout the entire maintenance run.

**Remediation options**:
- Increase `--cas-retry` (e.g., `--cas-retry=10`). Each retry re-reads the manifest,
  preserves any new push packs, and re-attempts the CAS. This is safe but adds latency.
- Schedule maintenance during quieter hours when push traffic is lower.
- For hot repos on a 6-hour cadence, the default threshold gate (no `--force`) may
  naturally defer runs to quieter periods.

**Cleanup**: the pack file and indexes uploaded during the failed run (Phases 4-5) remain
in the bucket as orphans. They are identified as tx-orphans and canonical-pack-orphans
by `bucketvcs gc` on its next run and swept after the retention window. No manual cleanup
is required.

### 7.2 Walk failure or corruption (`outcome=failed_walk`)

**What happened**: Phase 1 materialized the bare repo but `git fsck` reported corruption.
Either a canonical pack file is corrupt or a ref points to a commit OID that is not
present in any pack in the manifest.

**What to check**:
- Check the structured log for the `event=maintenance.completed` line; the `error` field
  wraps the `ErrCorruptInput` detail and typically names the problematic OID.
- Use `bucketvcs cat-object` against the OIDs listed in the error to verify which pack
  is the source of corruption.

**Remediation**: refer to `docs/m8-gc-operator-guide.md` §11 for pack corruption repair
guidance. A corrupt canonical pack requires investigation; do not run maintenance with
`--force` while packs are known corrupt, as the repack will embed the corrupt content.

### 7.3 Pack write failure (`outcome=failed_pack_write`)

**What happened**: `git pack-objects` failed during Phase 2. This is typically a disk
space issue in the temporary directory.

**What to check**:
- Check available space in `TMPDIR` (or `/tmp` if `TMPDIR` is unset). The repack
  process writes the full consolidated pack to a temp directory before uploading.
  Peak disk usage is approximately 2x the total size of all canonical packs (input
  pack set + output pack).
- Check the structured log for the subprocess error from `git pack-objects`.

**Remediation**: free disk space in `TMPDIR`, or set `TMPDIR` to a volume with
sufficient capacity before invoking maintenance.

### 7.4 Unexpected failure (`outcome=failed_other`)

**What happened**: an unexpected error occurred. Object-store connectivity loss,
authentication expiry, and unexpected nil-pointer conditions all land here.

**What to check**: look in the structured log for the `event=maintenance.completed`
line. The `error` field will contain the unwrapped error. For store connectivity
issues, verify that the store URL and credentials are valid, then re-run.

---

## 8. Interaction with `bucketvcs gc`

M9 maintenance and M8 GC are complementary operations. They share the same
correctness foundation (the §43.6 CAS-merge model with retention dominance) and are
both safe to run concurrently, but they do different things:

- **`bucketvcs maintenance`**: consolidates pack layout; produces new indexes;
  makes old packs unreachable.
- **`bucketvcs gc`**: reclaims storage; sweeps orphan tx records, unreachable canonical
  packs, and stale indexes; writes no new packs.

The recommended schedule: run maintenance first, then gc. Maintenance makes old packs
unreachable; GC sweeps them on the same run cycle rather than waiting until the next one.

**Concurrent runs** (two invocations of the same command against the same repo, or
maintenance and gc simultaneously) are safe: the CAS-merge model ensures neither
corrupts the manifest. Concurrent maintenance runs waste IO because both walk the same
pack set, both upload new artifacts, but only one wins the CAS-merge. The loser's
artifacts become GC targets. Concurrent maintenance + gc is equally safe: GC's mark
phase takes a manifest snapshot; maintenance's CAS win after that snapshot leaves the
GC's mark valid for the snapshot it took.

One-line summary: run maintenance to consolidate and refresh; run gc to reclaim.

---

## 9. Flag Reference

```
usage: bucketvcs maintenance --store=<URL> {--repo=<t>/<r> | --all-repos} [flags]

Flags:
  --store=URL                       Storage URL (required)
  --repo=<tenant>/<repo>            Single repo (mutex with --all-repos)
  --all-repos                       Process every discovered repo
  --force                           Skip threshold check
  --dry-run                         Walk + plan only; no writes
  --recent-pack-threshold=N         Default 1000 (0 disables)
  --total-pack-threshold=N          Default 10000 (0 disables)
  --manifest-pack-bytes-threshold=N Default 8388608 (0 disables)
  --recent-window=DURATION          Default 24h, minimum 1h
  --cas-retry=N                     Default 5
  --output=text|json                Default text

Exit codes:
  0  Success or dry-run completed
  1  At least one repo failed (including CAS exhaustion)
  2  Invalid flags
```
