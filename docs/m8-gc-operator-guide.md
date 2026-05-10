# M8 Operator Guide: `bucketvcs gc`

This guide is for operators who deploy, schedule, and monitor `bucketvcs gc`
in production. It covers what the command does, when to run it, how to tune
retention, the fundamental race window you need to understand before depending
on GC in anger, per-cloud lifecycle recipes for incomplete multipart uploads,
localfs operational notes, how to read audit records after an incident, and how
to wire exit codes into your alerting stack.

---

## 1. What `bucketvcs gc` Does

`bucketvcs gc` is a one-shot, operator-scheduled CLI command. It reclaims
storage from four categories of orphaned objects inside a bucketvcs repository.
It does not touch Git object content inside pack files and does not restructure
any pack.

### 1.1 In-scope sweep targets

- **Orphan tx records** — Transaction records left behind by lost compare-and-swap
  attempts in `repo.Commit`. Every commit writes a tx record before it races to
  swap in the new manifest. Only one racer wins per round; all losers leave tx
  records with no corresponding commit marker. After the retention window, GC
  sweeps these losers.

- **Orphan canonical packs** — Pack files (`.pack`, `.idx`, optional `.bitmap`)
  that were uploaded to the canonical packs prefix but never committed into a
  manifest entry, typically because the importing process crashed between the
  upload and the manifest CAS. GC identifies them by listing
  `packs/canonical/` and subtracting the live set derived from the current
  manifest.

- **Unreachable canonical packs from history** — Packs that were once referenced
  by a manifest but became unreachable because a force-push or branch deletion
  rewrote history. After the retention window, GC sweeps these former live
  objects.

- **Stale indexes** — Reachability indexes (object-map, commit-graph, reachability
  JSON) that are no longer pointed to by the current manifest. These accumulate
  whenever a new index is generated and the old one is superseded.

### 1.2 Explicitly out of scope (not swept by M8)

- **Object-level GC and repack inside packs** — Reclaiming individual Git objects
  and repacking loose or redundant data belongs to M9. `bucketvcs gc` does not
  open pack files and does not rewrite them.

- **Generated packs** (`packs/generated/`)  — Dynamic pack writers are not yet
  implemented. GC for generated packs is deferred to the milestone that
  introduces those writers.

- **In-binary multipart cleanup** — Aborting incomplete multipart uploads from
  inside the binary requires extending the `ObjectStore` surface with
  `ListIncompleteMultipartUploads` and `AbortMultipart`. That extension is a
  focused future milestone. In the interim, use per-cloud bucket lifecycle
  policies (see §5 below).

---

## 2. Recommended Schedule

GC is a one-shot CLI command. You schedule it with whatever process scheduler
your infrastructure provides. Run it during low-traffic windows where possible;
see §4 for why low-traffic windows reduce (but do not eliminate) the §43.6 race
surface.

### 2.1 cron

A nightly run at 03:00 is a reasonable starting point for most repositories.
The `--store` flag takes the same scheme URL you use for `bucketvcs serve`.

```cron
# /etc/cron.d/bucketvcs-gc
# Run GC over all repos at 03:00 every night.
0 3 * * * bucketvcs /usr/local/bin/bucketvcs gc \
    --all-repos \
    --store=s3://my-bucket \
    --retention=168h \
    >> /var/log/bucketvcs-gc.log 2>&1
```

For a GCS or Azure backend, substitute the store URL:

```cron
0 3 * * * bucketvcs /usr/local/bin/bucketvcs gc \
    --all-repos \
    --store=gcs://my-gc-bucket \
    --retention=168h \
    >> /var/log/bucketvcs-gc.log 2>&1
```

The `bucketvcs` username at the start of the cron entry is the system user
that runs the command; adjust to match your deployment.

### 2.2 Kubernetes CronJob

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: bucketvcs-gc
  namespace: bucketvcs
spec:
  schedule: "0 3 * * *"
  concurrencyPolicy: Forbid        # never run two GC jobs in parallel
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 5
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          serviceAccountName: bucketvcs-gc
          containers:
            - name: gc
              image: your-registry/bucketvcs:latest
              command:
                - /usr/local/bin/bucketvcs
                - gc
                - --all-repos
                - --store=s3://my-bucket
                - --retention=168h
                - --format=json
              env:
                - name: AWS_REGION
                  value: us-east-1
                - name: AWS_ACCESS_KEY_ID
                  valueFrom:
                    secretKeyRef:
                      name: bucketvcs-gc-creds
                      key: access-key-id
                - name: AWS_SECRET_ACCESS_KEY
                  valueFrom:
                    secretKeyRef:
                      name: bucketvcs-gc-creds
                      key: secret-access-key
              resources:
                requests:
                  cpu: "100m"
                  memory: "256Mi"
                limits:
                  cpu: "500m"
                  memory: "1Gi"
```

Notes on this CronJob:

- `concurrencyPolicy: Forbid` is important. Running two GC instances against
  the same store simultaneously is safe due to the `version_mismatch` protocol
  (see §2.3 in the design spec), but it wastes API quota and produces confusing
  audit records. Forbid eliminates accidental concurrent runs at no cost.

- `restartPolicy: Never` prevents Kubernetes from retrying a failed GC job
  automatically. An exit-1 failure needs human triage; a silent retry loop can
  mask a persistent store problem. Configure your cluster's job alerting on
  `.status.failed > 0` for the CronJob.

- `--format=json` in a Kubernetes environment makes logs parseable by
  Fluentd/Loki pipelines without additional parsing rules.

- Tune `memory.limits` for your repository sizes. See §9 for the memory
  scaling model.

### 2.3 systemd timer

Two files are needed: a `.service` and a `.timer`. Both live in
`/etc/systemd/system/` on the host that runs GC.

**`/etc/systemd/system/bucketvcs-gc.service`**:

```ini
[Unit]
Description=bucketvcs garbage collection
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=bucketvcs
Group=bucketvcs
ExecStart=/usr/local/bin/bucketvcs gc \
    --all-repos \
    --store=s3://my-bucket \
    --retention=168h \
    --format=json
StandardOutput=journal
StandardError=journal
SyslogIdentifier=bucketvcs-gc

# Treat exit 2 as success for systemd's service tracking — it means
# "ran but left work behind," which you handle via alerting on the
# journal content, not via systemd failure escalation. Exit 1 is a
# genuine failure and will surface as a failed unit.
SuccessExitStatus=2
```

**`/etc/systemd/system/bucketvcs-gc.timer`**:

```ini
[Unit]
Description=Run bucketvcs GC nightly at 03:00
Requires=bucketvcs-gc.service

[Timer]
OnCalendar=*-*-* 03:00:00
RandomizedDelaySec=300
Persistent=true

[Install]
WantedBy=timers.target
```

Enable and start:

```bash
systemctl daemon-reload
systemctl enable --now bucketvcs-gc.timer
```

Verify the next trigger time:

```bash
systemctl list-timers bucketvcs-gc.timer
```

`RandomizedDelaySec=300` spreads the start time by up to five minutes, which
is useful if you run GC on multiple hosts against different store partitions —
it avoids a thundering herd against the metadata plane.

`Persistent=true` means that if the host was down at 03:00, systemd will run
the timer as soon as the host comes back up. This is the right behavior for
GC: a missed run accumulates garbage, not correctness risk.

---

## 3. Retention Defaults and Choices

### 3.1 Default: 7 days (168h)

The default retention window is 7 days (`168h`). An object that GC classifies
as unreachable during the mark phase is not deleted until it has been
continuously classified as unreachable for at least 7 days. The first time GC
observes the object as unreachable, it records `first_seen_unreachable_at` in
the mark record. On subsequent runs the field is carried forward unchanged; the
retention check compares the current time against that original timestamp.

This means the retention window is measured from the time an object became
unreachable (as first observed by GC), not from the time GC runs.

### 3.2 Overriding with `--retention`

The flag accepts any `time.Duration` string that Go's `time.ParseDuration`
accepts:

```bash
# One week (the default — explicit for clarity in scripts)
bucketvcs gc --all-repos --store=s3://my-bucket --retention=168h

# Two weeks (recommended for active force-push environments)
bucketvcs gc --all-repos --store=s3://my-bucket --retention=336h

# One month
bucketvcs gc --all-repos --store=s3://my-bucket --retention=720h

# 30 minutes — for development / integration testing only
bucketvcs gc --repo=test-tenant/scratch --store=localfs:///tmp/bv-dev --retention=30m
```

The retention value is recorded inside the mark record (`retention_seconds`)
at mark time. If you later run `--sweep-only` against an existing mark, the
sweep uses the retention recorded in the mark, not whatever `--retention` flag
you pass to the sweep invocation. This prevents a short `--sweep-only` run
from retroactively shortening a mark that was computed with a longer window.

### 3.3 The `< 24h` warning

If you set `--retention` below 24 hours, GC emits a warning to stderr:

```
warning: --retention is less than 24h; this may delete objects during active clone or import sessions
```

This is intentional. A realistic git clone session can take tens of minutes for
large repositories, and a buggy or slow importer might hold an in-progress pack
upload for several hours. Objects that look unreachable during the mark phase
might be mid-session artifacts that will be committed to the manifest before the
session completes. A 24-hour window provides a generous safety margin over the
longest realistic session lifetime.

Retention values below 1 hour should only appear in automated testing scenarios
where you have full control over the store and there are no concurrent sessions.
Never run sub-1h retention against a production store.

### 3.4 Force-push workflows

A force-push that drops pack X, followed later by a push that revives pack X
(same content, same content-addressed key), creates a window in which X is
unreachable. If that window spans a GC run and the retention period has elapsed,
GC will delete X. When the revival push then tries to reference X in a new
manifest, a serving read against the now-missing pack will fail.

For repositories where force-push and subsequent revival of the same content is
a normal workflow (monorepo history rewrites, interactive rebase cleanup cycles,
CI that force-pushes branches frequently), increase the retention window to be
longer than the maximum expected gap between the drop and the revival:

```bash
# Force-push workflows where drops and revivals can be days apart
bucketvcs gc --all-repos --store=s3://my-bucket --retention=720h  # 30 days
```

If you are uncertain about your force-push patterns, err on the side of a
longer retention window. Storage is cheap; a deleted-then-unavailable pack
requires an operator to re-import the missing content.

---

## 4. The §43.6 Race Window

### 4.1 What the race is

GC operates in two phases: mark (compute which objects are unreachable) and
sweep (delete the ones that have been unreachable long enough). Between mark and
sweep, the repository can receive pushes. This is intentional — GC does not hold
a repository lock. The following sequence describes a race that GC cannot fully
prevent:

```
Time    Push side                           GC side
------  ---------------------------------   ----------------------------------
T0      manifest v=V references pack X      —
T1      force-push rewrites to v=V+1,       —
        dropping X from the manifest
T2      —                                   GC mark phase runs against v=V+1.
                                            X is not in the live set.
                                            X is added as a candidate with
                                            first_seen_unreachable_at = T2.
T3      —                                   Mark record written. GC waits
                                            retention period.
T2+ret  —                                   GC sweep starts. Sweep re-reads
                                            the manifest — sees v=V+K.
T2+ret  A push commits v=V+K+1 reviving X. Sweep computes fresh_live from
        (This happens AFTER sweep's         v=V+K (one version behind the
        fresh re-read.)                     revival). X is not in fresh_live.
T2+ret  —                                   Sweep does Head(X) → gets
           +ε                               current version v_X.
                                            DeleteIfVersionMatches(X, v_X)
                                            succeeds. X is deleted.
T2+ret  Revival push has committed v=V+K+1  X is missing. Reads against the
           +ε                               pack fail.
```

The window between sweep's fresh manifest re-read and the per-key `Delete` call
is sub-second in normal operation (a single `Head` RPC plus the `Delete` RPC
across a cloud API). This is a genuine time-of-check to time-of-use (TOCTOU)
window that cannot be fully closed without cross-process coordination that GC
does not implement.

The `DeleteIfVersionMatches` conditional delete would catch a concurrent
re-upload at the same key with *different* bytes (it would return
`ErrVersionMismatch`). But packs are content-addressed: a pack with the same
content will have the same ETag/version after re-upload. The version check does
not protect against same-content revivals that race the sub-second TOCTOU window.

### 4.2 Mitigations

Four mitigations, in order of effectiveness:

**1. Long retention window (primary defense)**

The default 7-day retention dominates any plausible "drop pack X and revive it
7 days later" scenario. In practice, force-push revivals of the same content
within a single repository happen over hours, not weeks. A 7-day window provides
an enormous safety margin. Lengthening to 30 days (720h) for active force-push
workflows essentially eliminates operational risk at the cost of slower GC.

**2. Low-traffic scheduling**

Running GC during periods of low push activity reduces the probability that a
push is in-flight during the sweep phase. It does not eliminate the window — a
single concurrent push is enough to create the race — but it makes the race
vanishingly unlikely in practice. Scheduling GC at 03:00 local time, when push
rates are near zero, is a straightforward operational hedge.

**3. Content-addressing limits the race surface**

A pack can only be re-referenced in a manifest if it exists in the store. A pack
that was deleted by GC cannot be referenced by a subsequent manifest commit that
is not aware of the deletion (the import would have to re-upload it). The race
requires a push that *revives* a pack that already exists in the store —
specifically, one that was uploaded in the window between mark and sweep's
fresh re-read. This is a narrow slice of concurrent activity.

**4. Audit fields enable post-incident analysis**

The mark record carries `first_seen_unreachable_at`, `mark_manifest_version`,
and `last_seen_reachable_at` per candidate. The sweep record carries
`current_manifest_version` (the version swept against) and the complete deleted,
skipped, and error lists. If a pack disappears unexpectedly, an operator can
consult the sweep record to see exactly which manifest version was current at
sweep time, and compare it against push history to determine whether a race
occurred. See §7 for how to read these records.

### 4.3 Residual TOCTOU quantification

The TOCTOU window between sweep's manifest re-read and the per-key `Delete` is
bounded by the time it takes to execute `Head(key)` followed by
`DeleteIfVersionMatches(key, version)` — two sequential round-trips to the cloud
store. Against well-provisioned cloud storage (AWS S3, GCS, Azure Blob, R2), this
is typically 10–150 ms per key. For the race to produce data loss, a push commit
must arrive and complete within that window for *exactly* the key being swept at
that moment. The probability of this in any given sweep is extremely low; the
probability of it happening with a 7-day retention window (meaning the revived
pack must have been continuously unreachable for 7 days before the sub-second
window opens) is operationally negligible for any realistic push rate.

---

## 5. Bucket Lifecycle for Incomplete Multipart Uploads (§33.5)

Multipart uploads that crash mid-flight leave orphaned upload sessions in the
cloud store. These sessions consume storage quota and (on some providers)
incur costs. GC does not abort these sessions; instead, configure a per-cloud
bucket lifecycle rule to abort them automatically after a fixed number of days.

7 days is a reasonable lifecycle duration. Any legitimate import session that
runs longer than 7 days is a problem in its own right, and leaving the session
open for 7 days before aborting it ensures no ongoing session is interrupted.

### 5.1 AWS S3

Create a file `lifecycle.json` with the following content:

```json
{
  "Rules": [
    {
      "ID": "abort-incomplete-mpu",
      "Status": "Enabled",
      "Filter": {
        "Prefix": ""
      },
      "AbortIncompleteMultipartUpload": {
        "DaysAfterInitiation": 7
      }
    }
  ]
}
```

Apply it to your bucket:

```bash
aws s3api put-bucket-lifecycle-configuration \
  --bucket my-bucket \
  --lifecycle-configuration file://lifecycle.json
```

Verify:

```bash
aws s3api get-bucket-lifecycle-configuration --bucket my-bucket
```

The `"Prefix": ""` filter applies the rule to all objects in the bucket. If
your bucket stores data from multiple systems and you want to limit the scope
to bucketvcs paths, set `"Prefix": "tenants/"` (or whatever tenant prefix you
use).

S3 evaluates lifecycle rules daily. An incomplete multipart upload initiated at
day 0 will be aborted on or after day 7, not necessarily exactly at day 7.

### 5.2 Cloudflare R2

R2 supports the same S3-compatible lifecycle API. You can either use
`aws s3api` with an R2 endpoint, or use `wrangler`:

**Via `aws s3api` with R2 endpoint:**

```bash
aws s3api put-bucket-lifecycle-configuration \
  --endpoint-url https://<account-id>.r2.cloudflarestorage.com \
  --bucket my-bucket \
  --lifecycle-configuration file://lifecycle.json
```

Use the same `lifecycle.json` as the AWS S3 example above.

**Via `wrangler`:**

```bash
wrangler r2 bucket lifecycle add my-bucket \
  --abort-incomplete-multipart-upload-days 7
```

Verify:

```bash
wrangler r2 bucket lifecycle list my-bucket
```

Note: `wrangler r2 bucket lifecycle add` sets the rule for the entire bucket
without prefix filtering. If you need prefix-scoped rules, use the
`aws s3api put-bucket-lifecycle-configuration` path with a `Filter.Prefix`
set.

### 5.3 Google Cloud Storage

GCS lifecycle configuration uses a JSON file with an `action` of type
`AbortIncompleteMultipartUpload` and a `condition` of `age` in days.

Create `lifecycle.json`:

```json
{
  "lifecycle": {
    "rule": [
      {
        "action": {
          "type": "AbortIncompleteMultipartUpload"
        },
        "condition": {
          "age": 7
        }
      }
    ]
  }
}
```

Apply it:

```bash
gsutil lifecycle set lifecycle.json gs://my-bucket
```

Verify:

```bash
gsutil lifecycle get gs://my-bucket
```

If you use the newer `gcloud storage` CLI:

```bash
gcloud storage buckets update gs://my-bucket \
  --lifecycle-file=lifecycle.json
```

GCS processes lifecycle rules once per day. The `age` condition counts the
number of days since the upload was initiated; an upload with `age >= 7` is
eligible for the AbortIncompleteMultipartUpload action.

Note: The `AbortIncompleteMultipartUpload` action in GCS lifecycle rules
applies only to XML API multipart uploads (which is what the S3-compatible
surface and the GCS XML API use). The GCS JSON API's "resumable upload"
sessions are handled separately and expire automatically after a week of
inactivity regardless of lifecycle configuration. The bucketvcs GCS adapter
uses the XML API path via the S3-compatible client library, so the lifecycle
rule above applies.

### 5.4 Azure Blob Storage

Azure Blob Storage calls uncommitted blocks from an incomplete `Put Block` +
`Put Block List` sequence "uncommitted blobs." A management policy can delete
them after a specified number of days.

Create `policy.json`:

```json
{
  "rules": [
    {
      "name": "abort-uncommitted-blobs",
      "enabled": true,
      "type": "Lifecycle",
      "definition": {
        "filters": {
          "blobTypes": ["blockBlob"]
        },
        "actions": {
          "baseBlob": {
            "delete": {
              "daysAfterModificationGreaterThan": 7
            }
          },
          "snapshot": {},
          "version": {}
        }
      }
    }
  ]
}
```

Wait — the above targets committed blobs. For uncommitted blocks specifically,
use the `enableAutoTierToHotFromCool` or the `delete_uncommitted_blob_after`
sub-property. The correct structure for the Azure management-policy API
(2023-08-03 and later) is:

```json
{
  "rules": [
    {
      "name": "abort-uncommitted-blobs",
      "enabled": true,
      "type": "Lifecycle",
      "definition": {
        "filters": {
          "blobTypes": ["blockBlob"]
        },
        "actions": {
          "baseBlob": {},
          "snapshot": {},
          "version": {},
          "uncommittedBlock": {
            "delete": {
              "daysAfterLastModificationGreaterThan": 7
            }
          }
        }
      }
    }
  ]
}
```

Apply it to your storage account:

```bash
az storage account management-policy create \
  --account-name mystorageacct \
  --resource-group my-resource-group \
  --policy @policy.json
```

Verify:

```bash
az storage account management-policy show \
  --account-name mystorageacct \
  --resource-group my-resource-group
```

Notes:

- Azure evaluates lifecycle policies once per day.
- The `uncommittedBlock` action requires that the storage account has the
  lifecycle management feature enabled (enabled by default on General Purpose
  v2 accounts).
- Ensure the storage account `--kind` is `StorageV2`; the `uncommittedBlock`
  lifecycle action is not available on legacy V1 accounts.
- The `daysAfterLastModificationGreaterThan` condition measures days since the
  block was last written to. An upload that stalls for 7 days will have its
  blocks cleaned up.

---

## 6. localfs Operational Notes

### 6.1 Multipart session cleanup behavior

The localfs adapter stores in-progress multipart upload sessions as
subdirectories under `<root>/uploads/<upload-id>/`. Each session directory
contains a `manifest.json` and a `parts/` subdirectory.

**Multipart sessions do not self-clean on process exit.** If a `bucketvcs
serve` process is killed mid-import, or if an import tool crashes, the upload
directory remains on disk. The code in `CompleteMultipartIfAbsent` explicitly
notes this:

```
// Non-fatal: the object is committed; the upload dir leak is a
// gc concern, not a correctness one.
```

This is distinct from cloud storage multipart sessions, which cloud providers
time out independently of the client process.

### 6.2 Cleaning up stale localfs upload directories

Since `bucketvcs gc` does not currently enumerate or abort incomplete localfs
multipart sessions, operators must clean them up manually. The `uploads/`
directory is safe to inspect at any time — each session directory's
`manifest.json` contains the `created_at` timestamp.

To list upload sessions older than 7 days:

```bash
find /var/lib/bucketvcs/uploads -maxdepth 1 -mindepth 1 -type d \
  -mtime +7 -print
```

To remove them (after verifying no active `bucketvcs serve` process is using
them — stop the server first if needed):

```bash
# Stop the server before manual cleanup
systemctl stop bucketvcs-serve

# Remove upload sessions older than 7 days
find /var/lib/bucketvcs/uploads -maxdepth 1 -mindepth 1 -type d \
  -mtime +7 -exec rm -rf {} +

# Restart
systemctl start bucketvcs-serve
```

Do not remove upload directories while `bucketvcs serve` is running against
the same `<root>`. The localfs adapter holds a process-level lockfile
(`<root>/.lock`) that prevents a second `localfs.Open` from succeeding, but
it does not prevent external `rm -rf` from racing an active `UploadPart` call.
Always stop the server first.

### 6.3 Scheduled cleanup alternative

If stopping the server for manual cleanup is not acceptable, run a scheduled
`find ... -delete` against the `uploads/` directory during a maintenance
window, or add a cron job that prunes sessions older than a conservative age
(30 days):

```bash
# Example cron entry — runs weekly, removes sessions older than 30 days
0 4 * * 0 find /var/lib/bucketvcs/uploads -maxdepth 1 -mindepth 1 \
    -type d -mtime +30 -exec rm -rf {} + 2>&1 | logger -t bv-uploads-prune
```

This approach carries a small risk if a session directory is removed while its
upload is in progress. The localfs upload implementation uses an `atomic.Bool`
`terminated` flag and validates the on-disk `manifest.json` before each
`UploadPart` call; a removed directory will cause subsequent `UploadPart` calls
to return `ErrInvalidArgument` rather than silently corrupting data. The risk
is an aborted import, not data corruption.

---

## 7. Reading Mark and Sweep Records for Post-Incident Analysis

Mark records are stored at `gc/marks/mk_<ulid>.json` within each repository
prefix. Sweep records are stored at `gc/sweeps/sw_<ulid>.json`. Both use
ULID keys that sort lexicographically by time (most recent last in ascending
order).

### 7.1 Dry-run preview with JSON output

Before running a live GC sweep, you can see the full candidate list without
writing any mark record or deleting anything:

```bash
bucketvcs gc \
  --repo=my-tenant/my-repo \
  --store=s3://my-bucket \
  --dry-run \
  --format=json
```

Pipe through `jq` to summarize candidate counts:

```bash
bucketvcs gc \
  --repo=my-tenant/my-repo \
  --store=s3://my-bucket \
  --dry-run \
  --format=json \
| jq '{
    repo: .repo_id,
    candidates: {
      tx_records: (.candidates.tx_records | length),
      canonical_packs: (.candidates.canonical_packs | length),
      indexes: (.candidates.indexes | length)
    }
  }'
```

To list the actual candidate keys:

```bash
bucketvcs gc \
  --repo=my-tenant/my-repo \
  --store=s3://my-bucket \
  --dry-run \
  --format=json \
| jq -r '.candidates.canonical_packs[].key'
```

### 7.2 Reading stored mark records

Mark records from past GC runs are the primary forensic artifact. To read
the most recent mark record for a repository from S3:

```bash
aws s3 ls s3://my-bucket/tenants/my-tenant/repos/my-repo/gc/marks/ \
  | sort | tail -5
```

Then read the latest:

```bash
aws s3 cp \
  s3://my-bucket/tenants/my-tenant/repos/my-repo/gc/marks/mk_01HZXXX.json \
  - | jq .
```

For GCS:

```bash
gsutil ls gs://my-bucket/tenants/my-tenant/repos/my-repo/gc/marks/ \
  | sort | tail -5

gsutil cat \
  gs://my-bucket/tenants/my-tenant/repos/my-repo/gc/marks/mk_01HZXXX.json \
  | jq .
```

For Azure Blob:

```bash
az storage blob list \
  --account-name mystorageacct \
  --container-name my-container \
  --prefix tenants/my-tenant/repos/my-repo/gc/marks/ \
  --query "[].name" -o tsv | sort | tail -5

az storage blob download \
  --account-name mystorageacct \
  --container-name my-container \
  --name tenants/my-tenant/repos/my-repo/gc/marks/mk_01HZXXX.json \
  --file - | jq .
```

### 7.3 Key audit fields explained

**`first_seen_unreachable_at`**

The timestamp at which GC first observed this object as unreachable (not in the
live set derived from the current manifest). On subsequent GC runs, this value
is carried forward unchanged from the previous mark record — it always reflects
the original observation time, not the time of the most recent mark run. The
retention check compares `now - first_seen_unreachable_at` against the retention
window.

If you see an object deleted that you believe should still be reachable, check
`first_seen_unreachable_at` in the sweep record against your push history. A
`first_seen_unreachable_at` that precedes a force-push tells you the object was
already considered unreachable before that push — the push was not the cause.

**`mark_manifest_version`**

The manifest version that was current when GC first classified this object as
unreachable (i.e., not in the live set derived from that manifest version). For
canonical packs, this is the manifest version under which the pack became a
candidate. Comparing `mark_manifest_version` against your push log tells you
which force-push or branch deletion dropped the pack from the live set.

**`last_seen_reachable_at`**

For canonical packs only. The approximate timestamp of the last GC run that
saw this pack as reachable (present in the live set). This is set on the run
that observes the transition from reachable to unreachable; it approximates the
time of a force-push or branch deletion. Null if GC never observed the pack in
a reachable state (for example, an orphan pack from a crashed import that was
never committed to a manifest).

**`current_manifest_object_version`** (mark record)

The object storage version token (ETag for S3/R2/GCS, ETag for Azure) of the
manifest object at mark time. This is an audit field, not a sweep safety field
— sweep does its own fresh re-read. Comparing the mark's
`current_manifest_object_version` against the sweep's
`current_manifest_object_version` shows how much the manifest advanced between
mark and sweep.

### 7.4 Post-incident workflow: "a pack disappeared"

1. Identify the pack key from the serving error log (the server logs
   `key=<key>` when an object is not found).

2. Search recent sweep records for the key:

   ```bash
   # S3
   aws s3 ls s3://my-bucket/tenants/my-tenant/repos/my-repo/gc/sweeps/ \
     | sort | tail -20

   aws s3 cp \
     s3://my-bucket/tenants/my-tenant/repos/my-repo/gc/sweeps/sw_01HZXXX.json \
     - | jq '.deleted.canonical_packs[] | select(. == "tenants/my-tenant/repos/my-repo/packs/canonical/abc123.pack")'
   ```

3. If the key appears in a sweep record's `deleted.canonical_packs`, find the
   corresponding mark record (the sweep record contains `mark_id`):

   ```bash
   aws s3 cp \
     s3://my-bucket/tenants/my-tenant/repos/my-repo/gc/marks/mk_YYYY.json \
     - | jq '.candidates.canonical_packs[] | select(.key == "tenants/my-tenant/repos/my-repo/packs/canonical/abc123.pack")'
   ```

4. Read `first_seen_unreachable_at` and `mark_manifest_version` from the mark
   record entry, then cross-reference against your push history
   (`bucketvcs inspect-manifest` at that version, git log on the affected
   branches) to determine whether the deletion was correct or a race.

5. If the deletion was a race, re-import the missing content and open a bug
   against the operation. Increase the retention window to reduce future
   exposure.

---

## 8. Exit Code Interpretation and Cron Alerting

### 8.1 Exit code reference

| Code | Meaning | Recommended response |
|------|---------|---------------------|
| `0` | Clean run. Zero per-key errors, zero `version_mismatch` skips. All candidate processing completed normally. | No action required. |
| `1` | Operational error. The store was unreachable, the manifest schema is unsupported, `--repo` did not find a repo, or flags were invalid. | **Treat as a page.** Something is wrong with the store or the invocation. Investigate immediately. In `--all-repos` mode, exit 1 means at least one repo's GC failed completely. |
| `2` | Ran successfully but left work behind. At least one candidate was skipped with `reason=version_mismatch`, or at least one per-key error occurred. GC completed its sweep for all other candidates. | **Treat as a soft alert.** Investigate at normal business hours. Common cause: a concurrent push during the sweep window caused a `version_mismatch`. The affected candidates will be re-evaluated on the next GC run. A persistent exit 2 (same candidates always producing `version_mismatch`) indicates a store-level conflict that needs operator attention. |

### 8.2 Cron alerting patterns

**Simple pattern: escalate exit 1, surface exit 2**

Wrap the GC invocation in a small shell script:

```bash
#!/usr/bin/env bash
# /usr/local/bin/bucketvcs-gc-monitored
set -euo pipefail

LOG=/var/log/bucketvcs-gc.log
BIN=/usr/local/bin/bucketvcs

"$BIN" gc --all-repos --store=s3://my-bucket --retention=168h --format=json \
  >> "$LOG" 2>&1
EXIT=$?

if [ "$EXIT" -eq 1 ]; then
  # Page: operational failure
  echo "bucketvcs gc FAILED (exit 1) — check $LOG" \
    | mail -s "ALERT: bucketvcs gc operational error" ops@example.com
elif [ "$EXIT" -eq 2 ]; then
  # Soft alert: left work behind
  echo "bucketvcs gc completed with warnings (exit 2) — check $LOG" \
    | mail -s "WARNING: bucketvcs gc left work behind" ops-noisy@example.com
fi

exit "$EXIT"
```

**Healthcheck endpoint pattern**

If you expose a healthcheck endpoint from your alerting stack (Alertmanager,
PagerDuty, Opsgenie), replace the `mail` calls with `curl`:

```bash
if [ "$EXIT" -eq 1 ]; then
  curl -s -X POST "https://events.pagerduty.com/v2/enqueue" \
    -H "Content-Type: application/json" \
    -d "{\"routing_key\":\"$PD_KEY\",\"event_action\":\"trigger\",\"payload\":{\"summary\":\"bucketvcs gc failed\",\"severity\":\"critical\"}}"
fi
```

**Kubernetes pattern**

In Kubernetes, rely on `.status.failed > 0` alerting via Prometheus:

```yaml
# PrometheusRule
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: bucketvcs-gc-alerts
  namespace: bucketvcs
spec:
  groups:
    - name: bucketvcs-gc
      rules:
        - alert: BucketvcsGCFailed
          expr: |
            kube_job_status_failed{job_name=~"bucketvcs-gc-.*", namespace="bucketvcs"} > 0
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: "bucketvcs GC job failed"
            description: "The bucketvcs GC CronJob has a failed execution. Check job logs."
```

For exit 2 alerting in Kubernetes, note that with `SuccessExitStatus=2` set
(in the systemd case) or by catching the exit code in a wrapper script (in the
Kubernetes case), you need to parse the JSON output to detect the `version_mismatch`
count. A simple approach: push `--format=json` output to a log aggregator, then
alert on `version_mismatch > 0` in the parsed JSON stream.

### 8.3 Interpreting `version_mismatch` counts

A nonzero `version_mismatch` count in the sweep record is the most common cause
of exit 2. Each `version_mismatch` means:

1. GC identified the object as a sweep candidate (unreachable + retention met).
2. GC called `Head(key)` to get the current object version.
3. Between `Head` and `DeleteIfVersionMatches`, the object's version changed —
   meaning it was written by another process (another GC run, a concurrent
   import, or a repair tool).
4. The delete was abandoned for that key.

On the next GC run, the key will appear in the candidate list again (carried
forward from the previous mark). If the object is still unreachable and still
meets retention, GC will attempt the delete again.

A `version_mismatch` on every successive GC run against the same key is
unusual. Investigate if you see a key appearing in `version_mismatch` across
three or more consecutive sweep records.

---

## 9. Known Limit: In-Memory Candidate Accumulation

### 9.1 The issue

During the mark phase, GC lists all objects under each sweep-target prefix and
accumulates the full candidate list in memory before writing the mark record.
For very large repositories with millions of pack files, this can be a
significant memory consumer.

### 9.2 Estimate

Each candidate entry in memory consists of:
- The full storage key string (~80–120 bytes for a typical bucketvcs key)
- Timestamp fields and other metadata (~20–40 bytes)

A conservative estimate is ~100–150 bytes per entry. At scale:

| Candidate count | Estimated memory |
|-----------------|-----------------|
| 100,000 | ~15 MB |
| 1,000,000 | ~150 MB |
| 10,000,000 | ~1.5 GB |

A repository with 10 million pack files (an extremely large monorepo or a
repository that has accumulated decades of import artifacts without any GC)
could cause the GC process to consume 1–1.5 GB of RSS during the mark phase.
This is the primary motivation for the `memory.limits` guidance in the
Kubernetes CronJob example (§2.2).

### 9.3 Current behavior when memory is exhausted

GC does not implement streaming pagination of mark records. If the process
runs out of memory during candidate accumulation, it will OOM-kill. The mark
record will not be written (PutIfAbsent at the end of the mark phase will not
execute), so the repository is left in a consistent state — the next GC run
starts fresh.

### 9.4 Future work

A streaming mark writer that emits the mark record in sharded `.json.zst`
chunks rather than accumulating all candidates in memory is planned for a
future milestone. This will allow GC to handle repositories of arbitrary scale
without proportional memory growth.

In the interim, if you operate a repository at this scale:

- Set a memory limit in your CronJob or process supervisor that is at least
  2× the estimated peak (to account for Go runtime overhead and garbage
  collection pressure).
- Monitor GC OOM events. If GC is OOMing, consider increasing the memory
  limit rather than splitting the repository — a split is an application-level
  concern and may not be appropriate.
- You can run `bucketvcs gc --mark-only` to test whether a mark phase
  completes within your memory budget before scheduling sweep.
