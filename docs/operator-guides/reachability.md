# Operator Guide: Reachability Index and Delta-Chain Compaction

This guide is for operators who deploy, schedule, and monitor reachability
compaction in production. It covers what `.bvrd` files are, how the base-index
/ delta-chain model works, how to tune the three reachability thresholds, how to
schedule compaction alongside `bucketvcs gc`, how to inspect the delta chain,
how to diagnose fallback warnings, how to use `bucketvcs negotiate` for ad-hoc
debugging, and the known limits of the reachability implementation.

---

## 1. Overview: `.bvrd`, Base Index, and the Cold-Fetch SLO

### 1.1 What a `.bvrd` file is

Each `git push` to a bucketvcs repository produces a small reachability-delta
(`.bvrd`) file. A `.bvrd` file records:

- The new commits that arrived in the push (OIDs, generation numbers, parent
  OIDs).
- Ref-tip diffs: which refs moved from old-OID to new-OID as a result of the
  push.

`.bvrd` files are append-only per-push artifacts. They do not contain pack
objects; they are pure metadata. A typical small push produces a `.bvrd` file
of 5–20 KB. A push with thousands of new commits can produce a larger file.

### 1.2 The base index + delta chain model

The base index is the pair:

- `.bvom` — object map: maps Git OIDs to pack IDs. Tells `upload-pack` which
  pack contains each commit.
- `.bvcg` — commit graph: maps OIDs to generation numbers and parent lists.
  Enables generation-bounded walk for negotiation.

The base index is produced by `bucketvcs maintenance` (a full repack and
index rebuild) or by the first `git push` to a new repo. It covers all commits
reachable from refs at the time it was built.

The delta chain is the list of `.bvrd` files produced by subsequent pushes.
Each delta layers on top of the base. The manifest's
`indexes.reachability.deltas` array lists them in push order.

During `git fetch` negotiation (`upload-pack`), bucketvcs first tries the base
index. If the client's `have` commits are covered by the base, negotiation
completes in O(1). If the client has commits that arrived after the base was
built, negotiation walks the delta chain to find them. If neither covers the
commit, bucketvcs falls back to a full pack walk (the non-indexed path).

### 1.3 The cold-fetch SLO contract

The cold-fetch SLO is:

**A fresh `git clone` or `git fetch` on a repository with a current base index
and a delta chain of ≤ 100 pushes completes negotiation in O(1) plus O(delta
count) with no pack walk.**

This SLO is met as long as:

1. The base index is current (produced from the manifest's current pack set).
2. The delta chain has not grown past the operator-configured thresholds.

When either condition fails, compaction runs. Compaction refreshes the base
index and truncates the delta chain, restoring the SLO.

The compact-only path (triggered by reachability thresholds without pack
fragmentation) still calls `git repack` under the hood; this is a deferred
optimization (see §9). The cold-fetch SLO win is still realized — the base
index covers the full commit set and the delta chain is truncated — but the
compact-only phase is pack-walk-bound until the optimization ships.

---

## 2. Threshold Tuning

### 2.1 The three reachability thresholds

Reachability compaction adds three threshold flags to `bucketvcs maintenance`:

| Flag | Default | What it measures | When compaction fires |
|------|---------|-----------------|----------------------|
| `--reachability-delta-commits` | 1000 | Total commit count across all `.bvrd` deltas (sum of per-delta header NCommits) | Delta chain covers > N commits |
| `--reachability-delta-pushes` | 100 | Number of `.bvrd` delta files in the manifest | More than N pushes since last compaction |
| `--reachability-delta-bytes` | `64M` | Total byte size of all `.bvrd` files listed in the manifest | Chain exceeds N bytes on disk |

Setting any threshold to `0` disables that specific check. All three at `0`
with `--force` not set means reachability compaction never fires (valid if you
want manual-only compaction).

### 2.2 Default rationale

**100 pushes**: A delta chain of 100 `.bvrd` files means negotiation must scan
up to 100 files to resolve commits that arrived after the base. Each file is a
sequential read; 100 × 15 KB = 1.5 MB of metadata reads per fetch. At this
scale the O(delta count) overhead is perceptible on cold fetch paths.

**1000 commits**: 1000 commits spread across a delta chain represents a
significant amount of commit history that is not covered by the base commit
graph. Generation-bounded walk in the delta chain is still correct but slower
than base index lookup.

**64 MiB bytes**: The delta chain's total on-disk byte size rarely drives
compaction before the push or commit count thresholds. It provides a backstop
for repos where individual pushes are large (e.g., bulk imports that arrive
as single large pushes, each producing a large `.bvrd`).

### 2.3 Busy vs idle repos

| Repository type | Suggested thresholds | Notes |
|-----------------|---------------------|-------|
| Idle (<5 pushes/day) | Defaults | Push count will stay well below 100 for weeks. Weekly `--force` compaction is sufficient. |
| Moderate (10–50 pushes/day) | Defaults | Defaults gate compaction at ~2–10 days of push activity. Daily maintenance with thresholds is appropriate. |
| Hot (100+ pushes/day) | `--reachability-delta-pushes=50` | Default 100-push threshold means compaction fires daily. Halving the threshold doubles the frequency; tune based on observed fetch latency. |
| Monorepo (large commits) | `--reachability-delta-commits=500` | Large commits mean fewer pushes but more commits per push. Lower commit threshold triggers compaction earlier. |

### 2.4 The `--force` flag and reachability

`--force` skips all threshold evaluations, including reachability thresholds.
A forced run always performs a full repack and base index rebuild, and always
truncates the delta chain. Use `--force` for:

- Post-import warmup: after `bucketvcs import` produces many small push packs
  and a long delta chain, a single `--force` run consolidates everything.
- Scheduled weekly runs on small or idle repos where you want deterministic
  behavior.
- Manual operator intervention after a known event (large bulk import,
  repository migration).

### 2.5 Threshold evaluation order

Maintenance evaluates thresholds in this order (cheap-first):

1. `--total-pack-threshold` — O(1) check on manifest body.
2. `--manifest-pack-bytes-threshold` — O(1) check on manifest body.
3. `--reachability-delta-bytes` — O(1) check on manifest body (sum of
   `delta.size_bytes` fields).
4. `--reachability-delta-pushes` — O(1) check on manifest body (len(deltas)).
5. `--recent-pack-threshold` — O(N pack heads) via object-store HEAD calls.
6. `--reachability-delta-commits` — O(N delta headers) via per-file header
   reads.

The first threshold that fires wins; subsequent checks do not run. If a pack
threshold fires, the reachability check is skipped because the full repack
path also rebuilds the base index and truncates the delta chain anyway.

---

## 3. Cron Cadence

### 3.1 Recommended schedule

Run `bucketvcs maintenance` (which includes reachability compaction) before
`bucketvcs gc`. Maintenance consolidates packs and refreshes indexes; GC
reclaims the old artifacts.

Hourly with thresholds is appropriate for hot repos. Weekly with `--force` is
appropriate for idle repos.

### 3.2 Example crontab

```cron
# /etc/cron.d/bucketvcs-maintenance
#
# Hot repos: hourly, thresholds gate compaction.
0 * * * * bucketvcs /usr/local/bin/bucketvcs maintenance \
    --all-repos \
    --store=s3://my-bucket \
    --reachability-delta-pushes=50 \
    --output=json \
    >> /var/log/bucketvcs-maintenance.log 2>&1

# Weekly forced compaction to guarantee a clean base index.
0 3 * * 0 bucketvcs /usr/local/bin/bucketvcs maintenance \
    --all-repos \
    --store=s3://my-bucket \
    --force \
    --output=json \
    >> /var/log/bucketvcs-maintenance.log 2>&1

# GC runs 30 minutes after maintenance.
30 3 * * 0 bucketvcs /usr/local/bin/bucketvcs gc \
    --all-repos \
    --store=s3://my-bucket \
    --retention=168h \
    >> /var/log/bucketvcs-gc.log 2>&1
```

For a moderate repo with daily maintenance:

```cron
# Daily maintenance at 02:00, GC at 02:30.
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

### 3.3 Kubernetes CronJob

For the hourly threshold-gated path:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: bucketvcs-maintenance
  namespace: bucketvcs
spec:
  schedule: "7 * * * *"
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 5
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
                - --reachability-delta-pushes=50
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

Scheduling at `7 * * * *` (7 minutes past the hour) avoids the thundering herd
at `:00` common when everyone defaults to `0 * * * *`.

---

## 4. Inspecting the Delta Chain

### 4.1 Using `inspect-manifest --json`

```bash
bucketvcs inspect-manifest \
    --store=s3://my-bucket \
    --json \
    my-tenant my-repo \
  | jq '{reachability: .indexes.reachability, summary: .reachability_summary}'
```

The top-level JSON object has this shape:

```json
{
  "refs": { "refs/heads/main": "<tip-oid>", ... },
  "packs": [ ... ],
  "indexes": {
    "object_map": { ... },
    "commit_graph": { ... },
    "reachability": {
      "base_manifest": "v00000123",
      "deltas": [ ... ]
    }
  },
  "reachability_summary": { ... }
}
```

Note: `reachability` lives under `indexes` (raw manifest field), while
`reachability_summary` is a top-level derived key added by `inspect-manifest`.

`jq .indexes.reachability` — the raw manifest field:

```json
{
  "base_manifest": "v00000123",
  "deltas": [
    {
      "key": "tenants/my-tenant/repos/my-repo/indexes/reachability-delta/a3f82c....bvrd",
      "hash": "a3f82c...",
      "size_bytes": 88032
    },
    {
      "key": "tenants/my-tenant/repos/my-repo/indexes/reachability-delta/9b21d0....bvrd",
      "hash": "9b21d0...",
      "size_bytes": 91200
    }
  ]
}
```

`jq .reachability_summary` — derived counts computed by `inspect-manifest`:

```json
{
  "base_manifest": "v00000123",
  "delta_chain_length": 2,
  "delta_chain_bytes": 179232,
  "delta_files": [
    {
      "key": "tenants/my-tenant/repos/my-repo/indexes/reachability-delta/a3f82c....bvrd",
      "hash": "a3f82c...",
      "size_bytes": 88032
    },
    {
      "key": "tenants/my-tenant/repos/my-repo/indexes/reachability-delta/9b21d0....bvrd",
      "hash": "9b21d0...",
      "size_bytes": 91200
    }
  ]
}
```

For a repo without a reachability index, both the `indexes.reachability` and
`reachability_summary` keys will be absent.

### 4.2 Extracting just the chain length and byte count

```bash
bucketvcs inspect-manifest \
    --store=s3://my-bucket \
    --json \
    my-tenant my-repo \
  | jq '{len: .reachability_summary.delta_chain_length, bytes: .reachability_summary.delta_chain_bytes}'
```

### 4.3 Listing delta keys for manual inspection

```bash
bucketvcs inspect-manifest \
    --store=s3://my-bucket \
    --json \
    my-tenant my-repo \
  | jq -r '.reachability_summary.delta_files[].key'
```

To read a specific delta file from S3:

```bash
aws s3 cp s3://my-bucket/<key> /tmp/delta.bvrd
```

`.bvrd` files are binary (custom format defined by the `internal/reachability/deltaindex`
package). Use `bucketvcs negotiate` for human-readable output rather than reading
the binary directly (see §6).

### 4.4 Checking the base manifest version

The `base_manifest` field records the manifest version at which the base index
was last built. Comparing it against the current manifest version tells you how
many manifest versions have accumulated since the last base rebuild:

```bash
manifest_version=$(bucketvcs inspect-manifest \
    --store=s3://my-bucket --json my-tenant my-repo \
  | jq .manifest_version)
base_manifest=$(bucketvcs inspect-manifest \
    --store=s3://my-bucket --json my-tenant my-repo \
  | jq -r .reachability_summary.base_manifest)
echo "versions since base: $((manifest_version - ${base_manifest#v}))"
```

A large gap indicates the base index is stale. If the gap exceeds your expected
push rate × threshold push count, a manual `--force` compaction run is warranted.

---

## 5. Diagnosing Fallback Warnings

When `upload-pack` cannot resolve a client `have` commit from the base index or
the delta chain, it falls back to a full pack walk (the non-indexed negotiation
path). A structured fallback log line is emitted:

```
event=reachability.fallback reason=<reason> oid=<short-hash>
```

### 5.1 `reason` label reference

| `reason` label | What it means | Remediation |
|----------------|---------------|-------------|
| `no_index` | The repo has no base index (`.bvom` / `.bvcg` absent). The repo has never been through a maintenance run, or was imported before reachability indexing was available. | Run `bucketvcs maintenance --repo=... --force` to build the initial base index. |
| `delta_decode` | A `.bvrd` delta file failed to parse (`deltaindex.ErrMalformed`). Storage corruption or wire-format drift. | Check store connectivity. Verify the delta key in the manifest exists and is intact. Run `bucketvcs maintenance --repo=... --force` to rebuild the base index and drop the corrupt delta from the chain. |
| `unknown` | Any other error (storage read failure, network timeout, unexpected state). | Check store connectivity and server logs. The structured log line includes the underlying error. |

> **Note**: The labels `oid_not_found`, `base_read_error`, `walk_depth_exceeded`, and `delta_read_error` are reserved for future use and are not emitted by the classifier.

### 5.2 High-frequency `unknown` fallbacks

If you see repeated `unknown` fallbacks, check the structured log for the
underlying error. Common causes are transient store read failures (network
timeout, throttling) and unexpected state in the reachability index.

For a `delta_decode` fallback (corrupt `.bvrd`):

1. Run `bucketvcs inspect-manifest --json` to identify which delta key is listed
   in the manifest.
2. Verify the key exists in the object store.
3. Run `bucketvcs maintenance --repo=... --force` to rebuild the base index and
   drop the corrupt delta from the chain.

### 5.3 `delta_decode` — corrupt delta file

A `delta_decode` fallback indicates a `.bvrd` file failed to parse. After
restoring connectivity or confirming the file is intact:

1. Run `bucketvcs inspect-manifest --json` to verify the keys are present in
   the manifest.
2. Use cloud-native CLI tools (`aws s3 ls`, `gsutil ls`, `az storage blob list`)
   to confirm the keys exist in the bucket.
3. If a key is missing, run `bucketvcs maintenance --force` to rebuild the base
   index (this overwrites the manifest's `indexes.object_map` and
   `indexes.commit_graph` pointers with freshly-built files).
4. If a `.bvrd` key is missing, the delta is lost. Running
   `bucketvcs maintenance --force` will drop the lost delta from the chain and
   rebuild from the base.

---

## 6. `bucketvcs negotiate` for Ad-Hoc Debugging

`bucketvcs negotiate` is a debug subcommand that exercises the negotiation
path against a specific repo and client want/have set, without a real git
client.

### 6.1 Example invocation

`--wants` and `--haves` accept comma-separated 40-character hex OIDs only (not
ref names). If you need to negotiate using a ref tip, first resolve it with
`inspect-manifest`:

```bash
main_tip=$(bucketvcs inspect-manifest \
    --store=s3://my-bucket \
    --json \
    my-tenant my-repo \
  | jq -r '.refs."refs/heads/main"')

bucketvcs negotiate \
    --store=s3://my-bucket \
    --repo=my-tenant/my-repo \
    --wants="$main_tip" \
    --haves=<client-have-oid>
```

Default text output:

```
Shipping plan: 3 commit(s)
  <oid-1>
  <oid-2>
  <oid-3>
Refs:
  refs/heads/main -> <tip-oid>
```

With `--output=json`:

```json
{
  "commits": ["<oid-1>", "<oid-2>", "<oid-3>"],
  "refs": {
    "refs/heads/main": "<tip-oid>"
  }
}
```

Exit codes:

| Code | Meaning |
|------|---------|
| `0` | Success |
| `1` | Operational error (store, repo, or reachability index) |
| `2` | Usage / flag error |
| `3` | Unknown want OID (client asked for a commit not in the index) |

### 6.2 Diagnosing a slow fetch with `negotiate`

1. Capture the client's `have` OIDs from the git protocol log (set
   `GIT_TRACE_PACKET=1` on the client).
2. Resolve the ref tip to an OID with `inspect-manifest --json` (see §6.1).
3. Run `bucketvcs negotiate --wants=<tip-oid> --haves=<have-oid>`.
4. If the exit code is `3`, the want OID is not in the reachability index.
   Check whether the manifest's `refs` block contains the ref
   (`jq '.refs'` on the `inspect-manifest` output) and whether maintenance
   has run since the push that introduced the commit.
5. If the shipping plan lists more commits than expected, check the delta chain
   length with `jq .reachability_summary.delta_chain_length` — a long chain
   means the base index is stale and compaction is overdue.

### 6.3 Per-commit generation numbers

The `--verbose` flag is not implemented. Generation-number details are
visible in the structured fallback log lines emitted by `upload-pack`
(see §5) and in the `.bvrd` binary delta files (not human-readable directly).
A `--verbose` flag for `negotiate` is tracked in the backlog.

---

## 7. Expected `.bvrd` Sizes

Empirical observations from development and testing:

| Push type | Typical `.bvrd` size |
|-----------|---------------------|
| Single-commit push | 5–20 KB |
| Small feature branch (5–20 commits) | 20–80 KB |
| Large feature branch (50–200 commits) | 80–400 KB |
| Bulk import (1000+ commits) | 1–5 MB |
| Octopus merge with many parents | 30–100 KB |

These sizes include the file header (32 bytes), the commit records (40 bytes
per commit), the ref-tip diffs (80 bytes per updated ref), and any padding.

The `--reachability-delta-bytes=64M` default accommodates roughly 3000–12000
small single-commit pushes before compaction fires on the byte threshold alone.
For repos receiving large bulk imports as individual pushes, lower the threshold
or rely on the commit-count threshold to fire sooner.

---

## 8. Operational Interactions

### 8.1 Order: maintenance first, then GC

Run `bucketvcs maintenance` before `bucketvcs gc` in the same scheduling window.

**Why**: maintenance compaction refreshes the base index and drops consumed
`.bvrd` delta files from the manifest. The old `.bvrd` files and old `.bvom` /
`.bvcg` files become unreachable from the manifest after the compaction CAS
commit. GC, running afterward, identifies these files as stale indexes and
sweeps them within the retention window.

If you run GC first, it marks the current `.bvrd` files as live (they are still
referenced by the manifest). Maintenance then compacts, making the old files
unreachable. GC on the next scheduled run sweeps them.

Either order is safe for correctness; maintenance-first maximizes what GC
reclaims on the same scheduling cycle.

### 8.2 Concurrent maintenance runs

Two concurrent `bucketvcs maintenance` runs against the same repo are safe: the
CAS-merge model ensures neither corrupts the manifest. The first to win the CAS
commits its new pack + refreshed indexes. The loser's uploaded artifacts become
GC targets. Both runs rebuild the base index; the duplicate work is
wasted IO.

`concurrencyPolicy: Forbid` in Kubernetes CronJobs prevents accidental
concurrent runs at no correctness cost.

### 8.3 Interaction with active pushes

A push arriving while maintenance is running is handled by the CAS-merge: the
push's new `.bvrd` delta is preserved in the merged body. The compacted manifest
includes the new delta alongside the refreshed base index. From the client's
perspective, the push commits normally and the fetch path is available
immediately after.

### 8.4 Interaction with `bucketvcs gc` mark phase

GC's mark phase reads the current manifest to build the live set. Live indexes
include the current `.bvrd` deltas, the current `.bvom`, and the current `.bvcg`.
All other index files (old `.bvrd`, old `.bvom`, old `.bvcg`) become candidates.

If GC's mark phase runs between maintenance's CAS commit and the retention
window expiry, the old `.bvrd` files will be marked as first-seen-unreachable at
mark time. They are not deleted until the retention window elapses.

For hot repos where maintenance runs hourly and GC runs nightly, the effective
retention delay before stale `.bvrd` files are swept is 24–168 hours (depending
on the GC retention setting), not the push interval.

---

## 9. Known Limits

### 9.1 Compact-only is still pack-walk-bound (deferred optimization)

The compact-only compaction path (triggered when only reachability thresholds
fire, not pack thresholds) currently calls `git repack` under the hood. This
means compaction is still pack-walk-bound even when the pack set is already
consolidated. The cold-fetch SLO win is still realized — the base index is
refreshed and the delta chain is truncated — but the computation is heavier
than necessary.

The deferred optimization (a pure-Go index rebuild path that reads the existing
`.bvom` + `.bvcg` and incrementally extends them using the delta chain, without
spawning `git repack`) is tracked in the backlog.

### 9.2 Monolithic only; no warm pool

The reachability set is monolithic: it covers all commits reachable from
all refs in the current manifest pack set. There is no warm pool or tiered
index (e.g., a "recent" sub-index for the last N pushes and a "cold" full
index). This means:

- Cold fetches are O(1) against the base + O(delta) for recent commits.
  No per-tier split; the full commit-graph covers everything in the base.
- The base index must be rebuilt from scratch when compaction runs. There is no
  incremental base-extension path yet.

Warm-pool / tiered index support is deferred to the backlog.

### 9.3 Commits, trees, blobs, and tags not tracked in deltas

`.bvrd` delta files track commit OIDs and ref-tip diffs only. Tree, blob, and
tag objects are not tracked. This is intentional: the negotiation path only
needs commit reachability to determine the minimal pack to send. Tree and blob
membership is resolved by pack index lookup after negotiation.

Tags (annotated tag objects) are also not tracked in `.bvrd`. A push that
creates only a new tag (no new commits) does not produce a `.bvrd` delta.
A future release may extend the format to cover tags if tag-negotiation
performance becomes a bottleneck.

### 9.4 No bitmaps in delta chain

The delta chain format does not include pack bitmaps. Bitmap-accelerated
negotiation is tracked in the backlog and is separate from the
delta-chain approach.

### 9.5 No Bloom filters

Bloom-filter acceleration for "does this commit exist in this index?" is not
implemented. All existence checks are linear scans over the delta file's commit
list. This is acceptable for chains of ≤ 100 deltas × reasonable per-delta
commit counts, but would benefit from Bloom filter optimization for large delta
chains. Deferred to a future release.

### 9.6 No auto-compaction inside `bucketvcs serve`

Compaction does not run automatically during `git push` processing inside
`bucketvcs serve`. It is exclusively an operator-scheduled maintenance
operation. There is no in-process background goroutine that triggers
compaction when thresholds are exceeded. This is by design: the serve binary
is stateless across requests and the compaction step (which calls `git
repack`) is heavyweight enough that it should run outside the serve request
path. The backlog includes a serve-triggered light-weight compaction
suggestion (no actual repack, just index refresh) as a future enhancement.

---

## 10. Flag Reference

The full set of reachability flags on `bucketvcs maintenance`:

```
  --reachability-delta-commits=N   Default 1000 (0 disables)
  --reachability-delta-pushes=N    Default 100 (0 disables)
  --reachability-delta-bytes=SIZE  Default 64M, suffix K/M/G (0 disables)
```

These flags are evaluated in addition to the pack thresholds. See
`docs/maintenance.md` for the full flag reference covering
all maintenance flags.

Exit codes are unchanged from the base maintenance command:

| Code | Meaning |
|------|---------|
| `0` | Success or dry-run completed |
| `1` | At least one repo failed (including CAS exhaustion) |
| `2` | Invalid flags |
