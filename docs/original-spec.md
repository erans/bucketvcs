# Object-Store-Native Git Service Specification

Version: 1.0 design draft
Status: design draft
Project name: bucketvcs

## 0. Executive summary

This specification defines **bucketvcs**, a Git-compatible repository service whose durable source of truth can be object storage.

The service MUST speak normal Git protocols at the network boundary and MUST NOT require a custom Git client, remote helper, wrapper, IDE plugin, or local daemon.

```text
git clone https://host/org/repo.git
git clone git@host:org/repo.git
git fetch
git pull
git push
git ls-remote
```

The storage layer is pluggable. The system SHOULD support object stores with sufficiently strong consistency and conditional-write semantics, including:

```text
Tier 1 canonical backends:
  AWS S3
  Google Cloud Storage / GCS
  Cloudflare R2
  Azure Blob Storage

Tier 1 candidate / explicit conformance required:
  Tigris
  MinIO / MinIO AIStor

Tier 2 / conformance-gated S3-compatible backends:
  Wasabi
  Backblaze B2
  Ceph RGW
  SeaweedFS
  Garage
  other S3-compatible object stores
```

The core design:

```text
standard Git protocol gateway
+ immutable content-addressed packs/bundles in object storage
+ immutable transaction records
+ root manifest compare-and-swap as the repo commit point
+ provider-specific storage adapter
+ reachability indexes for negotiation
+ read-path caches for pack/delta amplification
```

The core rule:

> The gateway may cache. The bucket is truth. The root manifest is the commit point.

This document is a design contract, not a full implementation handbook. Detailed choices such as database schema, exact token format, cache internals, job scheduler implementation, and license/governance model are explicitly called out as implementation or project-governance decisions where appropriate.

## 1. Purpose

The first purpose of bucketvcs is to be an **open-source, self-hosted Git-compatible repository service** whose durable source of truth can live in object storage.

The initial target user is a team, developer platform, infrastructure group, or enterprise that wants to run its own Git-compatible service while storing repository data in its own object storage backend.

The primary open-source deployment model is:

```text
self-hosted bucketvcs gateway
+ user-managed object storage
+ standard Git clients
+ pluggable storage adapters
```

Supported self-hosted storage targets SHOULD include:

```text
AWS S3
Google Cloud Storage / GCS
Cloudflare R2
Azure Blob Storage
Tigris
MinIO / MinIO AIStor
other conformance-tested S3-compatible stores
```

Commercial hosted, BYOB SaaS, and managed enterprise offerings are product layers built on top of the open-source self-hosted core.

The system therefore has three product forms, in order of priority:

1. Open-source self-hosted bucketvcs.
2. Commercial hosted/BYOB service.
3. Managed self-hosted enterprise service.

The project is not a Git-like protocol. It is a Git server with a different storage engine.

## 2. Hard compatibility requirement

“Git protocol compatible” means:

1. A standard Git CLI can clone, fetch, pull, push, and list refs without plugins.
2. Git clients see a normal HTTPS or SSH Git remote.
3. The service implements Git Smart HTTP behavior.
4. The service implements SSH `git-upload-pack` and `git-receive-pack` behavior.
5. The service supports Git protocol v2.
6. The service supports protocol v0/v1 compatibility where required by common clients.
7. Responses use valid pkt-line framing.
8. Packfiles are valid Git packfiles.
9. Object IDs, refs, tags, symrefs, shallow boundaries, forced updates, and receive-pack status retain standard Git semantics.
10. Unsupported optional capabilities MUST NOT be advertised.

This spec intentionally avoids the phrase “every optional Git feature on day one.” Compatibility means existing Git clients work normally against the capabilities the server advertises.

## 3. Goals

### 3.0 Self-hosted open-source goals

The open-source project MUST be useful without the commercial hosted service.

The OSS/self-hosted edition SHOULD include:

```text
single binary or simple deployment model
HTTPS Smart Git support
SSH Git support
pluggable object storage adapters
local filesystem adapter for dev/test
basic user/token/SSH-key auth
repository import/export
storage conformance testing
basic GC and maintenance
basic observability
```

The commercial service MAY add multi-tenant hosted control plane, billing, advanced audit, SSO, managed operations, warm-pool routing, managed maintenance, and enterprise support, but the core Git-compatible self-hosted storage engine SHOULD remain open source.

The self-hosted OSS edition SHOULD optimize for simple deployment and correctness. Advanced operational features such as repo warm pools, global routing, hosted billing, managed bundle/repack fleets, and SLA-backed operations MAY be commercial/hosted layers because they are operational capabilities rather than core repository-format capabilities.

### 3.1 Protocol goals

The service MUST support:

```text
git clone
git fetch
git pull
git push
git ls-remote
git push --delete
git push --force-with-lease
git fetch --tags
git clone --depth=1
annotated tags
symbolic HEAD
```

The service SHOULD eventually support:

```text
Git protocol v2 bundle-uri
packfile URI
partial clone / filter
Git LFS
push options
signed push where useful
```

### 3.2 Storage goals

The service MUST:

1. Store immutable bulk data in object storage.
2. Use a pluggable storage adapter contract.
3. Support bucket-only durable source-of-truth mode.
4. Support hosted storage, bring-your-own-bucket, and self-hosted deployments.
5. Use conditional create and conditional replace for correctness.
6. Treat local disk, memory, Redis, CDN, and workers as caches unless explicitly configured otherwise.

### 3.3 Product goals

The commercial service SHOULD support:

1. Multi-tenant organizations.
2. Per-user pricing.
3. Included hosted storage.
4. Bring-your-own-bucket.
5. Enterprise self-hosted.
6. Managed self-hosted.
7. SSO/SAML/OIDC/SCIM.
8. Audit logs.
9. Webhooks.
10. Pre-receive hooks and policy validation.
11. Storage usage reporting.
12. Egress-aware backend selection.

## 4. Non-goals

The initial project is not:

1. A GitHub clone.
2. A full issue tracker.
3. A CI/CD system.
4. A code review product.
5. A data lake versioning system.
6. A new Git client.
7. A mandatory Git remote helper.

Optional helper tools MAY exist for administration, import/export, and direct bucket recovery.

## 5. High-level architecture

```text
Git client
  |
  | HTTPS Smart Git / SSH Git
  v
Git protocol gateway
  |
  +-- authn/authz
  +-- protocol v2/v1/v0 negotiation
  +-- upload-pack / receive-pack engine
  +-- pack validation/generation
  +-- reachability index
  +-- read-path cache
  +-- push serialization fast path
  |
  +-- repository state engine
        |
        +-- storage adapter interface
              |
              +-- AWS S3
              +-- Google Cloud Storage
              +-- Cloudflare R2
              +-- Azure Blob Storage
              +-- Tigris
              +-- MinIO / AIStor
              +-- local filesystem
              +-- conformance-gated S3-compatible stores
```

The gateway MUST be stateless with respect to durable repository truth.

The gateway MAY keep local hot caches:

```text
manifest cache
ref advertisement cache
commit graph cache
object index cache
pack window cache
delta base cache
bundle metadata cache
```

Cache invalidation MUST be based on the root manifest version or object-store version token. A cache entry derived from manifest version `N` MUST NOT be used to answer a request against manifest version `N+1` unless explicitly safe.

## 6. Durable repository model

Canonical layout:

```text
/tenants/{tenant_id}/repos/{repo_id}/
  manifest/root.json
  manifest/ref-shards/{shard_hash}.json
  tx/{tx_id}.json
  packs/canonical/{pack_hash}.pack
  packs/canonical/{pack_hash}.idx
  packs/canonical/{pack_hash}.bitmap
  packs/generated/{pack_hash}.pack
  packs/generated/{pack_hash}.idx
  indexes/commit-graphs/{graph_hash}.graph
  indexes/reachability/{index_hash}.json
  bundles/{bundle_id}.bundle
  bundles/{bundle_id}.json
  lfs/objects/{sha256}
  hooks/{hook_id}/...
  gc/marks/{mark_id}.json
  gc/sweeps/{sweep_id}.json
```

Optional layouts MAY be enabled by feature-specific sections. For example, §15.2 defines an optional loose-object-like recent area for development, small self-hosted deployments, repair/import tooling, or benchmark-justified special workloads. It is not part of the default hosted storage path.

Repository names MUST NOT be durable object keys. Names can change. Stable internal IDs MUST be used.

Durable repository truth consists of:

1. The current root manifest object.
2. Immutable objects reachable from the root manifest.
3. Immutable transaction records referenced by committed manifests.

Objects not reachable from a committed manifest are garbage candidates.

## 7. Root manifest

The root manifest is the canonical repository state and the only visibility commit point.

Example:

```json
{
  "schema_version": 1,
  "repo_id": "r_123",
  "repo_format": {
    "object_format": "sha1",
    "compatibility": ["sha1"]
  },
  "manifest_version": 18492,
  "default_branch": "refs/heads/main",
  "ref_mode": "inline",
  "refs": {
    "refs/heads/main": {
      "target": "abc123...",
      "type": "commit",
      "updated_at": "2026-05-03T20:00:00Z",
      "updated_by": "u_123",
      "tx": "tx_999"
    },
    "refs/tags/v1.0.0": {
      "target": "def456...",
      "type": "tag",
      "peeled": "999aaa...",
      "tx": "tx_888"
    }
  },
  "packs": [
    {
      "id": "pack_abc",
      "pack_key": "packs/canonical/sha256-abc.pack",
      "idx_key": "packs/canonical/sha256-abc.idx",
      "bitmap_key": "packs/canonical/sha256-abc.bitmap",
      "pack_sha256": "abc...",
      "pack_size": 123456789,
      "object_count": 90210,
      "created_by_tx": "tx_999"
    }
  ],
  "indexes": {
    "commit_graph": {
      "key": "indexes/commit-graphs/sha256-graph.graph",
      "hash": "sha256-graph"
    },
    "reachability": {
      "key": "indexes/reachability/sha256-reachability.json",
      "hash": "sha256-reachability"
    }
  },
  "bundles": [
    {
      "id": "bundle_main_20260503_18492",
      "bundle_key": "bundles/main-20260503-18492.bundle",
      "manifest_key": "bundles/main-20260503-18492.json",
      "covers_manifest_version": 18492,
      "base_ref": "refs/heads/main",
      "freshness": "current"
    }
  ],
  "latest_tx": "tx_999",
  "created_at": "2026-05-03T20:00:00Z",
  "updated_at": "2026-05-03T20:01:00Z"
}
```

The root manifest MUST be updated by compare-and-swap:

```text
GET root manifest -> body + provider object version
PUT new root manifest only if provider object version still matches
```

If CAS fails, the writer MUST reload the manifest and either retry against the new state or reject the operation with normal Git semantics.

## 8. Immutable transaction records

Every repository mutation MUST write an immutable transaction record before attempting to update the root manifest.

Example:

```json
{
  "schema_version": 1,
  "tx_id": "tx_999",
  "repo_id": "r_123",
  "type": "push",
  "actor": "u_123",
  "started_at": "2026-05-03T20:00:00Z",
  "completed_at": "2026-05-03T20:00:05Z",
  "base_manifest_version": 18491,
  "base_manifest_object_version": "provider-version-token",
  "ref_updates": [
    {
      "ref": "refs/heads/main",
      "old": "oldsha...",
      "new": "newsha...",
      "mode": "fast_forward"
    }
  ],
  "new_packs": [
    {
      "pack_key": "packs/canonical/sha256-abc.pack",
      "idx_key": "packs/canonical/sha256-abc.idx",
      "pack_sha256": "abc...",
      "object_count": 1234
    }
  ],
  "validation": {
    "pack_valid": true,
    "objects_valid": true,
    "connectivity_valid": true,
    "policy_valid": true,
    "hooks_valid": true
  },
  "result": "committed"
}
```

Transaction records MUST be written with create-if-absent semantics.

A transaction is committed only if a root manifest references it.

## 9. Storage adapter contract

The core service MUST use a provider-neutral storage interface.

```ts
interface ObjectStore {
  get(key: string, opts?: GetOptions): Promise<ObjectBody | null>;
  head(key: string): Promise<ObjectMetadata | null>;
  putIfAbsent(key: string, body: Body, opts?: PutOptions): Promise<ObjectVersion>;
  putIfVersionMatches(
    key: string,
    expected: ObjectVersion,
    body: Body,
    opts?: PutOptions
  ): Promise<ObjectVersion>;
  deleteIfVersionMatches(key: string, expected: ObjectVersion): Promise<void>;
  list(prefix: string, opts?: ListOptions): Promise<ListPage>;
  createMultipart(key: string, opts?: MultipartOptions): Promise<MultipartUpload>;
  completeMultipartIfAbsent(
    upload: MultipartUpload,
    parts: MultipartPart[]
  ): Promise<ObjectVersion>;
  getRange(key: string, start: number, endInclusive: number): Promise<Bytes>;
  signedGetUrl(key: string, opts: SignedUrlOptions): Promise<string>;
}
```

Provider version tokens are normalized:

```ts
type ObjectVersion = {
  provider: string;
  token: string;
  kind: "etag" | "generation" | "version_id" | "opaque";
};
```

Core repository logic MUST NOT directly depend on S3 ETags, GCS generations, R2 ETags, Azure ETags, or provider-specific version IDs.

## 10. Required storage semantics

A backend can be canonical repository storage only if it supports:

1. Strong read-after-write for object reads.
2. Strong enough object listing for repair and GC.
3. Atomic create-if-absent.
4. Atomic replace-if-version-matches.
5. Failed conditional writes that do not modify objects.
6. Range reads.
7. Large object upload.
8. Multipart or resumable upload.
9. Signed or delegated reads.
10. Durable object metadata or durable version tokens.

Backends that lack these semantics MAY be used as mirrors, backup targets, or caches, but MUST NOT be used as canonical source of truth.

## 11. Storage backend support matrix

All canonical storage backends are conformance-gated.

The tier labels describe expected support maturity, not a waiver from correctness testing. Even AWS S3, GCS, R2, and Azure Blob MUST pass the bucketvcs conformance suite for the specific bucket/account/configuration used by a deployment.

### 11.1 Default-tested canonical backends

These should be first-class supported backends because their public APIs expose the required consistency and conditional-update primitives.

```text
AWS S3
  strong read-after-write/list consistency
  conditional writes with If-None-Match / If-Match
  range reads
  presigned URLs
  multipart upload

Google Cloud Storage / GCS
  strong global consistency for object read/write/list operations
  generation preconditions such as ifGenerationMatch
  range reads
  signed URLs
  resumable upload

Cloudflare R2
  strong global consistency
  S3-compatible conditional PutObject with If-Match / If-None-Match
  Worker binding conditional writes
  range reads
  signed/custom-domain/Worker-mediated access

Azure Blob Storage
  ETag-based optimistic concurrency
  If-Match / If-None-Match conditional headers
  range reads
  SAS URLs
  block blob upload
```

### 11.2 Deployment-tested canonical candidates

```text
Tigris
  globally distributed S3-compatible object storage
  documented strong consistency
  attractive multi-region read-path story
  must pass conditional write and multipart conformance tests

MinIO / MinIO AIStor
  S3-compatible storage
  can fit self-hosted deployments
  must pass conditional write, consistency, and multipart conformance tests for the specific deployment topology
```

### 11.3 Compatibility-tested S3-compatible backends

```text
Wasabi
Backblaze B2
Ceph RGW
SeaweedFS
Garage
RustFS
other S3-compatible storage systems
```

These MAY be supported, but only after the conformance test suite verifies that the specific backend and configuration satisfy the required semantics.

The product MUST avoid claiming “any S3-compatible bucket works” unless the conformance suite passes.

## 12. Provider mappings

### 12.1 AWS S3

```text
putIfAbsent              -> PutObject / CompleteMultipartUpload with If-None-Match: *
putIfVersionMatches     -> PutObject / CompleteMultipartUpload with If-Match: <etag>
manifest version token   -> ETag or version ID if bucket versioning is enabled
range reads              -> Range GET
signedGetUrl             -> presigned GET URL
```

### 12.2 Google Cloud Storage

```text
putIfAbsent              -> ifGenerationMatch=0
putIfVersionMatches     -> ifGenerationMatch=<generation>
manifest version token   -> object generation
range reads              -> Range GET
signedGetUrl             -> signed URL
```

### 12.3 Cloudflare R2

```text
putIfAbsent              -> PutObject with If-None-Match: * or Worker binding conditional put
putIfVersionMatches     -> PutObject with If-Match: <etag> or Worker binding conditional put
manifest version token   -> ETag / provider object token
range reads              -> Range GET / Worker R2 get range
signedGetUrl             -> presigned URL, custom domain, or Worker-mediated signed access
```

### 12.4 Azure Blob Storage

```text
putIfAbsent              -> If-None-Match: *
putIfVersionMatches     -> If-Match: <etag>
manifest version token   -> ETag or blob version ID
range reads              -> range GET
signedGetUrl             -> SAS URL
```

### 12.5 Tigris

```text
putIfAbsent              -> S3-compatible conditional write if supported by tested API path
putIfVersionMatches     -> S3-compatible If-Match if supported by tested API path
manifest version token   -> ETag or provider object token
range reads              -> Range GET
signedGetUrl             -> S3-compatible presigned URL
```

Tigris MAY become a preferred hosted/multi-region backend if conformance and economics are favorable.

### 12.6 MinIO / MinIO AIStor

```text
putIfAbsent              -> S3-compatible conditional write if supported by deployment
putIfVersionMatches     -> S3-compatible If-Match if supported by deployment
manifest version token   -> ETag or provider object token
range reads              -> Range GET
signedGetUrl             -> S3-compatible presigned URL
```

MinIO support MUST be deployment-tested because consistency depends on configuration, topology, and the exact product/version used.

## 13. Git protocol gateway

The protocol gateway MUST expose:

```text
HTTPS:
  GET  /{org}/{repo}.git/info/refs?service=git-upload-pack
  POST /{org}/{repo}.git/git-upload-pack
  GET  /{org}/{repo}.git/info/refs?service=git-receive-pack
  POST /{org}/{repo}.git/git-receive-pack

SSH:
  git-upload-pack '{org}/{repo}.git'
  git-receive-pack '{org}/{repo}.git'
```

The gateway MUST support:

1. pkt-line framing.
2. capability negotiation.
3. side-band and side-band-64k where appropriate.
4. receive-pack status reporting.
5. stateless RPC behavior over HTTP.
6. protocol v2 command flow.

The gateway MUST NOT require HTTP cookies for Git protocol access.

Authentication MAY use:

```text
HTTPS token auth
HTTP Basic with token-as-password
Bearer tokens
mTLS
SSH public keys
OIDC-issued short-lived Git tokens
```

## 14. Fetch negotiation and reachability index

Fetch negotiation is a core hard problem and MUST have a first-class design.

The gateway MUST be able to answer wants/haves without performing large numbers of object-store range reads during the negotiation loop.

Each repository SHOULD maintain a reachability/index set referenced by the root manifest:

```text
commit graph
commit generation numbers
object-to-pack index
pack offset table
bitmap index where available
ref tip index
tag peel index
optional Bloom filters for path-aware history acceleration
```

Required index properties:

1. Indexes are immutable objects.
2. The root manifest points to the current index set.
3. Indexes are derived from a specific manifest version.
4. Gateway caches are keyed by manifest version.
5. A stale index MUST NOT be used for a newer manifest unless it is explicitly marked as a safe base index plus bounded deltas.

### 14.1 Base index and delta index model

The default model SHOULD be:

```text
base reachability index from manifest N
+ delta index for pushes N+1 ... N+k
= effective reachability index for current manifest
```

Delta indexes SHOULD contain enough information to answer reachability without walking deep history from object storage.

A delta index SHOULD include:

```text
new commits
parent edges for new commits
new trees/blobs/tags introduced by the push
new ref tips
affected pack IDs
optional mini-bitmap for the new commit range
```

The delta format MUST be mergeable into the base index without requiring object-store reads proportional to total repository history.

### 14.2 Compaction bounds

The system MUST place a hard bound on un-compacted reachability deltas.

Initial recommended defaults:

```text
force compaction when delta chain exceeds 1,000 commits
force compaction when delta chain exceeds 100 pushes
force compaction when delta index bytes exceed 64 MiB
force compaction when cold fetch index-load time exceeds configured SLO
```

The exact defaults MAY change with benchmarking, but the product MUST expose an operational bound. “Compacted asynchronously” is not sufficient by itself.

### 14.3 Compaction ownership

Compaction MAY be run by:

```text
background workers
repo-owner actor / durable object
maintenance controller
self-hosted scheduled job
```

Compaction MUST produce a new immutable base index and then commit it by root manifest CAS. If the CAS fails, the compaction output becomes an orphaned candidate and MUST NOT be treated as current.

Cold-cache fetch target:

```text
A cold gateway should load a small bounded number of index objects before negotiation.
It should not perform one object-store range GET per commit or delta-base hop.
```

Bundle URI helps cold clone, but it does not solve incremental fetch. Incremental fetch performance depends on the reachability index.

### 14.4 Large base-index loading

Large repositories MUST NOT require every cold gateway to load one monolithic base index before serving fetch negotiation.

For large repos, the base index SHOULD be partitioned.

Acceptable partitioning strategies:

```text
by commit-graph generation range
by packfile / multi-pack group
by ref namespace
by object ID prefix for object-to-pack maps
by hot/default branch versus long-tail history
```

The root manifest SHOULD point to an index manifest rather than a single opaque index file when the repository crosses a configured size threshold.

Example:

```text
indexes/reachability/root-{hash}.json
  -> generations/0000-0999.idx
  -> generations/1000-1999.idx
  -> packs/pack-group-00.idx
  -> objects/prefix-00.idx
```

Cold gateways SHOULD load only the sections needed for negotiation. Hot gateways MAY keep repo-specific index sections warm in memory.

For very large repos, bucketvcs SHOULD support a warm-pool model:

```text
active large repos are assigned one or more warm gateways
warm gateways keep manifest-versioned index sections in memory
cold gateways can proxy or redirect expensive negotiation to warm gateways
```

This preserves the stateless-source-of-truth model: warm gateways cache index material, but the bucket and root manifest remain authoritative.

Warm pools introduce operational state even though they do not introduce durable repository truth. A hosted or large self-hosted deployment that uses warm pools needs:

```text
repo-to-gateway routing
health checks
draining during deploys
cache warmup/eviction policy
fallback to cold-load path
metrics for warm-hit ratio and warm-pool saturation
```

The simple OSS/single-binary path MAY operate without warm pools and cold-load indexes within configured repository-size limits.

## 15. Pack layout and read amplification

Git packs are efficient on local filesystems but can amplify reads on object storage because object resolution may require offset lookups and delta-base traversal.

The service MUST explicitly manage read amplification.

Required mechanisms:

1. Pack index cache.
2. Pack window cache for frequently accessed byte ranges.
3. Delta base cache.
4. Object-to-pack map loaded from reachability/index objects.
5. Range GET coalescing.
6. Local temporary pack materialization for expensive sessions.

### 15.1 Default recent-write strategy

The default strategy SHOULD be small append-style packs, not one loose object per Git object.

```text
push receives pack from client
gateway validates pack
gateway writes one or a small number of immutable recent packs
root manifest references those packs after CAS commit
background maintenance later repacks/promotes them
```

This keeps the push commit path object-count bounded and avoids excessive per-object PUT cost.

### 15.2 Loose-object-like recent area

A loose-object-like area MAY exist, but it SHOULD NOT be the default hosted path.

Acceptable uses:

```text
development mode
small self-hosted deployments
repair/import tooling
special write-heavy workloads where benchmarks justify it
```

If enabled, the loose-object-like area MUST still be represented in the root manifest or in a manifest-referenced index so that export, GC, and fetch correctness remain deterministic.

### 15.3 Background maintenance

Background maintenance SHOULD:

```text
promote recent packs into optimized packs
generate bitmaps
generate commit graph
generate reachability base index
generate bundle candidates
retire old generated/cache packs after retention
```

The system MUST place operational bounds on recent-pack accumulation.

Initial recommended triggers:

```text
force repack when recent canonical pack count exceeds 1,000
force repack when total canonical pack count exceeds 10,000
force repack when root manifest pack metadata exceeds 8 MiB
force repack when object-to-pack lookup latency breaches fetch SLO
force repack when bitmap coverage falls below configured threshold
```

The exact numbers MAY change after benchmarking, but the implementation MUST expose bounded pack-count and manifest-size policies. Unbounded “background repack later” is not acceptable.

Hot large repos SHOULD maintain:

```text
clone bundles
bitmap packs
gateway-local or regional hot pack caches
bounded recent-pack count
```

The object-store representation MUST always be exportable back into a normal bare Git repository.

## 16. Clone and fetch flow

### 16.1 Basic fetch

```text
1. Client connects over HTTPS or SSH.
2. Gateway authenticates actor.
3. Gateway authorizes read access.
4. Gateway loads root manifest.
5. Gateway loads or retrieves reachability/index data for that manifest.
6. Gateway advertises refs and capabilities.
7. Client sends wants/haves.
8. Gateway computes required object set using reachability index.
9. Gateway serves existing packs, generated pack, bundle URI, or packfile URI depending on client capabilities.
10. Gateway records audit event.
```

### 16.2 Dynamic pack generation

The gateway MAY generate a pack dynamically when existing packs and bundles do not match the request efficiently.

Generated packs SHOULD be written back to object storage if they are likely to be reused.

Generated packs MAY be cached locally or regionally.

### 16.3 Bundle URI acceleration

The gateway SHOULD support protocol v2 `bundle-uri` after the base service is stable.

Bundle freshness is a maintenance requirement, not just a protocol feature.

Bundle generation policies:

```text
small/medium repo:
  regenerate default-branch bundle after meaningful push threshold or time threshold

large active repo:
  maintain rolling base bundle plus incremental bundles
  regenerate on schedule and on read-miss pressure

CI-heavy repo:
  precompute bundles for default branch and release branches
```

Bundle freshness states:

```text
current       covers current manifest version
warm          behind current manifest by <= 100 commits or <= 24 hours, configurable
stale         behind current manifest by > 100 commits, > 24 hours, or poor measured hit ratio
retired       no longer advertised
```

Initial recommended regeneration triggers:

```text
regenerate after 100 commits on default branch
regenerate after 24 hours for active repos
regenerate after bundle miss/hit ratio drops below configured threshold
regenerate immediately for release tags if policy requires it
```

The gateway MUST NOT advertise a bundle that is inconsistent with the advertised refs.

### 16.4 Packfile URI acceleration

The gateway SHOULD support packfile URI where compatible with clients.

When using packfile URI, the gateway SHOULD return short-lived signed URLs or CDN URLs for immutable packs.

The gateway MUST NOT expose bucket credentials.

## 17. Push flow

```text
1. Client connects to git-receive-pack.
2. Gateway authenticates actor.
3. Gateway authorizes write access.
4. Gateway enters push serialization path for the repo/ref shard.
5. Gateway loads current root manifest and provider object version.
6. Gateway advertises refs and receive-pack capabilities.
7. Client sends update commands and packfile.
8. Gateway validates packfile integrity.
9. Gateway validates object IDs and object format.
10. Gateway validates connectivity against current manifest indexes/packs.
11. Gateway runs policy checks and pre-receive hooks.
12. Gateway checks fast-forward, force-push, protected branch, and signed-commit policies.
13. Gateway writes new pack/index objects immutably.
14. Gateway writes tx/{tx_id}.json with create-if-absent.
15. Gateway constructs new root manifest.
16. Gateway writes root manifest with CAS.
17. If CAS succeeds, push is committed.
18. If CAS fails, gateway reloads manifest and retries or rejects.
19. Gateway returns normal receive-pack status.
20. Gateway queues maintenance tasks.
21. Gateway emits audit events and webhooks.
```

A ref update MUST NOT be visible until all required packs are durably written, validated, and referenced by the committed manifest.

## 18. Push concurrency and serialization

Pure optimistic CAS is correct but can starve under high contention.

The service SHOULD provide a short-lived per-repo or per-ref-shard serialization fast path.

Acceptable implementations:

```text
in-process queue for single gateway owner
Durable Object / actor per repo
short-lived object-store lease
Redis/etcd/Postgres advisory lock in hosted mode
cloud queue with per-repo ordering
```

The durable commit point remains the root manifest CAS.

The queue/lease is a scheduling optimization only. It has no authority over correctness.

A gateway that holds a lease MUST still:

```text
load the current root manifest
validate against the current root manifest
write packs immutably
write transaction record
commit only through root manifest CAS
handle CAS failure as stale work
```

If a lease expires while a gateway is working, another gateway may begin work. This is safe because only one root manifest CAS can win. Packs written by losing or stale workers become orphaned candidates for GC.

Contention behavior:

```text
low contention:
  optimistic CAS is enough

moderate contention:
  serialize per repo

large monorepo / many refs:
  serialize per ref shard or branch protection domain
```

A lease MUST have an expiry. A dead gateway MUST NOT permanently block pushes.

## 19. Ref semantics and ref scaling

The service MUST support:

1. Branch refs under `refs/heads/`.
2. Tag refs under `refs/tags/`.
3. Symbolic HEAD.
4. Peeled annotated tags.
5. Ref deletion.
6. Forced updates where policy allows.
7. Protected branch rules.
8. Atomic visibility of committed ref updates.

### 19.1 Inline refs

Small and medium repositories MAY store refs directly in the root manifest.

### 19.2 Sharded refs

Large repositories SHOULD use content-addressed ref shards.

```text
manifest/root.json
manifest/ref-shards/{content_hash}.json
```

Rules:

1. Ref shards are immutable.
2. Ref shards are written with create-if-absent.
3. Root manifest contains the shard key and content hash.
4. Root manifest CAS remains the only commit point.
5. Ref shard objects MUST NOT have independent commit authority.

The sharding strategy is a performance contract and SHOULD be configurable.

Possible strategies:

```text
prefix sharding:
  easier prefix listing and human debugging
  can create hot shards for conventional branch/tag names

hash sharding:
  more uniform write distribution
  weaker prefix-locality for listing/debugging

hybrid namespace + hash sharding:
  shard first by namespace such as heads/tags/pull
  then hash within high-cardinality namespaces
```

Default recommendation for large repos:

```text
use hybrid namespace + hash sharding
keep protected/default branches in a small explicit shard
hash high-churn PR/CI/release refs across many shards
```

Example root manifest fragment:

```json
{
  "ref_mode": "sharded",
  "ref_sharding": "namespace_hash_v1",
  "ref_shards": [
    {
      "namespace": "refs/heads",
      "shard": "00",
      "key": "manifest/ref-shards/sha256-aaa.json",
      "hash": "sha256-aaa",
      "ref_count": 42000
    },
    {
      "namespace": "refs/tags",
      "shard": "3f",
      "key": "manifest/ref-shards/sha256-bbb.json",
      "hash": "sha256-bbb",
      "ref_count": 41000
    }
  ]
}
```

A ref update rewrites the affected shard as a new immutable object, then updates the root manifest pointer via CAS.

This preserves single-commit-point semantics while allowing large ref sets and avoiding predictable hot shards.

### 19.3 Ref resharding

Ref resharding is a maintenance operation similar to repack.

A repo that grows from thousands of refs to hundreds of thousands or millions of refs may need a new shard layout.

Resharding flow:

```text
1. Acquire repo/ref-shard maintenance lease.
2. Read current root manifest and ref shards.
3. Write new immutable ref shards with create-if-absent.
4. Build new shard layout metadata.
5. Attempt root manifest CAS to publish the new layout.
6. If CAS fails, discard or retry against the new manifest.
7. Old shards become GC candidates only after retention.
```

Resharding MUST NOT give shard objects independent commit authority. The root manifest CAS remains the only commit point.

During resharding, concurrent pushes may fail CAS and retry. Large resharding operations SHOULD be scheduled, rate-limited, and visible in maintenance metrics.

## 20. Object format support

The service MUST record each repository’s Git object format.

Required for v1:

```text
SHA-1 Git repositories
```

Future:

```text
SHA-256 repositories
```

SHA-256 support SHOULD be tracked but MUST NOT block initial product viability, because SHA-1 remains the practical compatibility baseline for most Git clients and hosting workflows.

## 21. Pack, index, and bitmap handling

The system MUST understand Git packfiles and pack indexes.

The system SHOULD generate:

```text
.pack files
.idx files
.bitmap files for large repos
commit graphs
multi-pack indexes where useful
bundle manifests
reachability indexes
```

Storage layout:

```text
packs/canonical/...
packs/generated/...
packs/cache/...
```

Canonical packs are referenced by root manifest.

Generated packs MAY be promoted to canonical packs by maintenance.

Cache packs MAY be deleted at any time.

## 22. Git LFS support

Git LFS support is not optional for a commercial Git host.

The service SHOULD implement the Git LFS batch API.

LFS layout:

```text
/repos/{repo_id}/lfs/objects/{sha256}
```

LFS flow:

```text
1. Client calls LFS batch API.
2. Gateway authenticates and authorizes object access.
3. Gateway returns upload/download actions.
4. Uploads go to gateway-mediated endpoint or signed object-store URL.
5. Downloads use signed object-store URL or CDN URL.
6. Gateway records LFS audit and usage events.
```

LFS objects MUST be content-addressed by SHA-256.

LFS billing MUST be separated from Git pack storage.

BYOB mode MAY store LFS objects in the customer bucket under the same repo prefix.

The Git LFS locking API SHOULD be supported for enterprise/commercial deployments. If not implemented in v1, the service MUST explicitly report it as unsupported rather than silently ignoring lock operations.

## 23. Hooks, policy, and server-side validation

Enterprise buyers expect server-side enforcement.

The service MUST support a hook/policy phase before manifest commit.

Minimum hook classes:

```text
pre-receive
update
post-receive
```

MVP MAY implement policy-native equivalents before arbitrary user code.

Policy checks SHOULD include:

```text
protected branches
required signed commits
force-push controls
file size limits
secret scanning hook integration
path restrictions
author/email rules
commit message rules
required status/check integration later
```

Pre-receive hooks MUST run before the transaction is committed.

Post-receive hooks and webhooks MUST run after commit.

Hook execution MUST be isolated and time-limited.

## 24. Webhooks

The service SHOULD support webhooks for:

```text
push
branch create/delete
tag create/delete
repo create/delete/rename
LFS upload
storage binding change
```

Webhook delivery MUST be asynchronous and retryable.

Webhook payloads SHOULD include:

```text
org
repo
actor
tx_id
old refs
new refs
commits summary
manifest version
storage backend
```

## 25. Garbage collection

GC MUST be conservative.

The service MUST NOT delete objects immediately after a ref update.

GC phases:

```text
1. Mark objects/packs reachable from current manifest.
2. Mark objects/packs reachable from recent manifests.
3. Mark packs referenced by active sessions.
4. Mark packs referenced by non-expired signed URLs.
5. Mark packs younger than minimum retention window.
6. Write immutable GC mark record.
7. Wait retention window.
8. Re-read current manifest.
9. Sweep only objects still unreachable.
10. Write immutable GC sweep record.
```

Default hosted retention SHOULD be at least 7 days.

Enterprise deployments MAY configure retention.

GC MUST understand ref shards and recent manifests.

## 26. Multi-region model

Multi-region support is a first-class operational concern.

### 26.1 Single-writer region

The simplest correct model:

```text
one canonical bucket region
one write region per repo
read gateways in many regions
read caches near users
all pushes route to write region
```

This keeps root manifest CAS simple.

Downside: users far from the write region pay latency for pushes.

### 26.2 Regional read replicas

Read replicas MAY use:

```text
object-store replication
CDN
regional pack caches
regional bundle caches
regional index caches
```

Read replicas need an explicit freshness model. bucketvcs SHOULD support two modes:

```text
strong-current mode:
  ref advertisement is served from the canonical write region or verified against it
  pack data may still be served from regional cache/object storage
  higher latency, strongest user expectation

bounded-stale mode:
  regional gateway serves the newest manifest version visible in that region
  stale reads are allowed within a configured lag budget
  if lag exceeds budget, the gateway must stop claiming the replica is healthy-current
  useful for CI mirrors, geo-read replicas, and read-heavy deployments
```

Git protocol itself has no standard way to tell a client “this advertisement is stale by X seconds.” Therefore product/API/UI surfaces MUST expose replica lag, but normal Git clients will simply see the refs advertised by the endpoint they contacted.

Default hosted behavior SHOULD be:

```text
interactive developer remotes: strong-current mode
CI/read mirror endpoints: bounded-stale mode, explicitly configured
```

If a bounded-stale regional gateway loses contact with the canonical region or cannot determine replication lag, the default behavior SHOULD be:

```text
serve reads only until the configured lag budget is exceeded
then mark the replica unhealthy for Git ref advertisement
continue serving immutable pack objects only when requested by exact manifest/pack key
surface replica lag/unhealthy state in metrics and product/API/UI surfaces
```

This favors consistency of ref advertisement over indefinite stale availability. Operators MAY configure a more availability-favoring policy for explicit mirror endpoints, but it MUST be visible as bounded-stale behavior.

A regional gateway MUST NOT advertise a manifest version newer than it can actually serve. Ref advertisement, pack access, and index selection MUST all be tied to the same manifest version.

### 26.3 Multi-writer regions

Multi-writer per repo is deferred.

If implemented, it requires one of:

```text
globally linearizable manifest store
repo-level write ownership transfer
consensus-backed manifest service
provider-specific strongly consistent global object store
```

Generic cross-region object replication MUST NOT be assumed to provide safe multi-writer Git semantics.

### 26.4 Tigris and global object stores

Tigris and similar globally distributed object stores MAY improve read fanout, regional latency, and operational simplicity.

They MUST NOT be treated as a shortcut to multi-writer Git unless their conditional writes are proven globally linearizable for the exact deployment mode.

The relevant question for canonical Git storage is not only global read latency. It is whether compare-and-swap on the root manifest is linearizable across all writers.

Default stance:

```text
globally distributed object store for reads: plausible and useful
globally distributed object store for multi-writer refs: not assumed
single-writer region per repo remains the default correctness model
```

Any global object store MUST pass the same CAS, consistency, and regional stress tests before being used as canonical storage.

## 27. Cost and egress economics

Cost is part of the product thesis.

Git traffic has asymmetric economics:

```text
pushes: usually smaller, write-heavy, validation-heavy
fetches/clones: often large, egress-heavy, CI-amplified
```

The service SHOULD model cost per repo as:

```text
storage bytes
object operation count
range GET count
egress bytes
CDN/cache hit ratio
LFS storage bytes
LFS egress bytes
bundle/cache storage overhead
GC retention overhead
```

Backend implications:

```text
AWS S3:
  excellent durability and enterprise trust
  egress can dominate CI-heavy workloads

GCS:
  strong semantics and enterprise trust
  egress can dominate CI-heavy workloads

Cloudflare R2:
  strong semantics and no egress bandwidth charge model
  strong hosted default candidate for clone/fetch-heavy repos

Azure Blob:
  important for Microsoft/Azure enterprises
  egress and private networking need explicit modeling

Tigris:
  interesting for globally distributed low-latency object access
  pricing and semantics require product-specific validation

BYOB:
  customer owns storage bill and egress bill
  product charges platform fee, users, support, and control plane
```

The product SHOULD expose a usage dashboard showing:

```text
repo storage
LFS storage
clone/fetch bytes
push bytes
object operations
estimated backend cost
cache savings
bundle savings
```

For hosted mode, Cloudflare R2 SHOULD be the default backend unless compliance, customer region, enterprise procurement, or measured workload requirements justify a different provider. S3, GCS, Azure Blob, Tigris, MinIO, and other stores remain important for BYOB and self-hosted deployments, but the hosted COGS model should assume R2-style zero-egress economics as the baseline.

Worked example:

```text
repo clone size: 500 MiB
CI clones per day: 10,000
daily egress: ~5 TiB/monthly egress: ~150 TiB

At $0.09/GB egress:
  ~5 TiB/day * 1024 GB/TiB * $0.09 = ~$460/day
  ~$13,800/month

At zero object-storage egress pricing:
  egress bandwidth line item approaches $0
```

This difference can determine whether hosted bucketvcs has healthy gross margin for CI-heavy customers. The exact numbers depend on provider pricing, cache hit ratio, and regional traffic, but the order of magnitude is central to the business case.

Request pricing still matters. R2-style economics can reduce or eliminate the bandwidth-egress line item, but object operation pricing, especially write/list/class-A-style operations, must be modeled alongside storage and egress. The hosted default should assume that egress dominates CI-heavy workloads, while the implementation must still minimize object operation count through pack consolidation, bundle reuse, caching, and range-read coalescing.

Implementation guidance in §14, §15, and §16 directly addresses request-count minimization through reachability-index compaction, pack consolidation, bundle reuse, cache locality, and range-read coalescing.

## 28. Bring-your-own-bucket mode

BYOB allows the customer to provide storage.

Supported setup patterns:

```text
AWS S3:
  assume role into customer AWS account
  optional customer-managed KMS key

GCS:
  service account or workload identity federation
  optional customer-managed encryption key

Cloudflare R2:
  scoped API token, account-level binding, or managed Worker integration

Azure Blob:
  managed identity, app registration, or SAS delegation
  optional customer-managed key

Tigris:
  scoped S3-compatible credentials

MinIO / self-hosted S3:
  access key / secret key or workload identity where available
```

Minimum permissions:

```text
read object
write object
conditional write
multipart upload
list prefix
range read
delete object under repo prefix for GC
create signed read URLs or allow gateway-mediated reads
```

The service MUST run a conformance suite before accepting a BYOB bucket as canonical storage.

## 29. Storage conformance test suite

Every backend MUST pass correctness tests before canonical use.

Required tests:

```text
1. concurrent putIfAbsent same key -> exactly one succeeds
2. concurrent putIfVersionMatches same key -> exactly one succeeds
3. failed conditional write does not alter object
4. read after write sees latest object
5. read after overwrite sees latest object
6. list after write sees new object
7. list after delete does not show deleted object after success
8. multipart complete cannot silently overwrite existing object
9. range read returns exact bytes
10. signed URL can read but cannot write
11. deleteIfVersionMatches fails if object changed
12. metadata/version token round trips
13. CAS conflict error maps to normalized conflict type
14. network retry does not duplicate committed object
15. provider throttling errors are classified correctly
```

Stress tests:

```text
100 concurrent manifest CAS attempts
10,000 small object creates
large multipart pack upload conflict
delete/read/list race during GC simulation
regional gateway read-after-write from distant region
```

Backends that fail any correctness test MUST NOT be used as canonical repository storage.

## 30. Native Git authentication and authorization model

bucketvcs MUST support both HTTPS and SSH Git authentication.

This is required for native Git compatibility and normal developer experience.

```text
HTTPS:
  best for browser flows, CI tokens, automation, corporate proxies, Git LFS, and simple onboarding

SSH:
  best for developer workstations, long-lived deploy keys, GitHub/GitLab-style workflows, and shell-native Git usage
```

Both transports MUST map into the same authorization engine. A user who can push over SSH can push over HTTPS if they present an equivalent credential, unless policy explicitly disables one transport.

### 30.1 HTTPS authentication

HTTPS Git access SHOULD use standard HTTP authentication patterns that native Git clients already support.

Recommended forms:

```text
username + personal access token as password
username + fine-grained repo token as password
machine user + machine token as password
deploy token + token secret as password
```

Example remotes:

```text
https://git.bucketvcs.example/acme/project.git
https://eran@git.bucketvcs.example/acme/project.git
```

The Git client can prompt for the token and store it using normal Git credential helpers.

bucketvcs SHOULD NOT use account passwords for Git-over-HTTPS. It SHOULD use tokens only.

HTTPS auth requirements:

1. MUST support HTTP Basic over TLS with token-as-password.
2. SHOULD support fine-grained personal access tokens.
3. SHOULD support repo-scoped deploy tokens.
4. SHOULD support machine tokens for CI.
5. MAY support short-lived OIDC-exchanged tokens.
6. MAY support bearer tokens for clients configured with `http.extraHeader`, but this MUST NOT be the only supported mode because it is not the default native Git UX.
7. MUST work with Git credential helpers.
8. MUST work with Git LFS authentication when LFS is enabled.

Recommended token scopes:

```text
repo:read
repo:write
repo:admin
lfs:read
lfs:write
webhook:admin
storage:admin
```

### 30.2 SSH authentication

SSH Git access SHOULD use the familiar Git-hosting model:

```text
git@git.bucketvcs.example:org/repo.git
ssh://git@git.bucketvcs.example/org/repo.git
```

The SSH username SHOULD usually be `git`. The actual user identity comes from the public key, SSH certificate, or signed principal, not from the Unix username.

SSH auth requirements:

1. MUST support user SSH public keys.
2. SHOULD support deploy keys.
3. SHOULD support machine keys for CI.
4. MAY support SSH certificates for enterprise deployments.
5. MUST restrict commands to Git operations only.
6. MUST NOT provide a general shell.
7. MUST parse and authorize only allowed commands:

   ```text
   git-upload-pack
   git-receive-pack
   git-upload-archive, optional
   ```
8. MUST normalize repository paths before authorization.
9. MUST prevent command injection and path traversal.

SSH forced-command behavior:

```text
client asks: ssh git@git.bucketvcs.example "git-upload-pack 'org/repo.git'"
server maps key -> actor
server parses command -> upload-pack
server maps path -> repo_id
server authorizes actor for repo read
server starts Git protocol session
```

### 30.3 Authorization model

Authorization decisions MUST be transport-neutral.

Permission checks:

```text
ls-remote / ref advertisement -> repo read
clone/fetch/pull -> repo read
push -> repo write
force push -> repo write + force-push permission or branch policy allow
protected branch update -> protected-branch permission or required policy pass
tag creation/deletion -> tag policy
LFS read/write -> repo read/write plus LFS scope
admin operations -> repo admin or org owner
```

The authorization engine SHOULD evaluate:

```text
actor
organization
repository
transport
credential type
token scopes
source IP / network policy
SSO session state
branch/ref policy
hook/policy result
```

### 30.4 Recommended auth product defaults

For hosted bucketvcs:

```text
enable HTTPS token auth by default
enable SSH keys by default
disable raw passwords for Git operations
require 2FA/SSO for token creation where configured
support deploy keys per repo
support machine tokens per org
```

For enterprise self-hosted bucketvcs:

```text
support SAML/OIDC login for UI
support SCIM provisioning
support SSH certificates optionally
support IP allowlists
support private networking
support audit export for all auth events
```

### 30.5 Token and credential storage requirements

bucketvcs MUST NOT store plaintext bearer tokens.

This applies to:

```text
personal access tokens
fine-grained repo tokens
deploy tokens
machine tokens
CI tokens
LFS tokens
short-lived API tokens where persisted
```

Token secrets MUST be shown only once at creation time.

The spec-level security contract is:

```text
no plaintext tokens at rest
no reversible token encryption as the default storage model
tokens are stored as one-way verifiable secrets
token verification keys, if used, are stored in KMS/HSM/secret manager
token revocation and expiration are checked before authorization
token scopes are enforced on every Git and LFS operation
token creation/use/revocation/failure events are audited
```

The implementation plan owns the exact token format, database schema, prefix strategy, hashing/HMAC choice, key rotation mechanics, and cache design.

SSH public keys MAY be stored in plaintext because they are public keys, but bucketvcs MUST store and display stable fingerprints.

bucketvcs MUST NOT store user SSH private keys.

If bucketvcs later adds managed deploy keys or generated machine keys, private keys MUST be protected by KMS/HSM-backed encryption, strict access controls, audit logging, and explicit product-level opt-in.

All authentication failures SHOULD be rate-limited and audited.

Token and SSH-key authorization results MAY be cached briefly, but caches MUST respect revocation and expiration semantics.

## 31. Audit events

The service SHOULD emit audit events for:

```text
repo.created
repo.deleted
repo.renamed
repo.cloned
repo.fetched
repo.pushed
ref.created
ref.updated
ref.deleted
pack.uploaded
manifest.updated
lfs.uploaded
lfs.downloaded
hook.executed
webhook.delivered
gc.started
gc.completed
storage.binding.created
storage.binding.failed
```

Audit records SHOULD include:

```text
actor
org
repo
source IP
auth method
protocol
old refs
new refs
tx id
manifest version
storage backend
result
latency
bytes in
bytes out
```

## 32. Observability

Metrics:

```text
git_requests_total
git_request_duration_seconds
pushes_total
push_failures_total
cas_conflicts_total
push_queue_wait_seconds
fetch_negotiation_duration_seconds
reachability_index_load_seconds
range_gets_per_fetch
pack_bytes_uploaded_total
pack_bytes_served_total
bundle_uri_advertisements_total
bundle_hit_ratio
storage_operations_total
storage_operation_errors_total
storage_operation_latency_seconds
gc_runs_total
gc_deleted_bytes_total
lfs_bytes_uploaded_total
lfs_bytes_downloaded_total
```

Tracing SHOULD cover:

```text
protocol session
auth check
manifest read
index load
pack negotiation
range GETs
pack generation
storage writes
CAS update
hook execution
webhook enqueue
response write
```

## 33. Failure handling

### 33.1 Crash before manifest update

Packs written but not referenced by a committed manifest are orphaned and GC candidates.

### 33.2 Crash after transaction write but before manifest update

The transaction is uncommitted. Repair tools MAY inspect it. It MUST NOT be treated as committed unless referenced by a root manifest.

### 33.3 CAS conflict

The gateway MUST reload the manifest and decide whether to retry or return a normal Git push rejection.

### 33.4 Push queue owner crash

The lease/queue ownership MUST expire. Another gateway MAY resume.

### 33.5 Partial multipart upload

Uncompleted multipart uploads MUST be aborted by lifecycle policy or cleanup worker.

### 33.6 Manifest corruption

The system SHOULD retain recent manifest versions where provider support allows.

Repair tooling SHOULD reconstruct the latest valid manifest from committed transaction chain and reachable pack data.

## 34. Repository import/export

Import flow:

```text
1. Read normal bare repository.
2. Run git fsck.
3. Generate canonical packs/indexes.
4. Generate reachability indexes.
5. Upload immutable packs/indexes.
6. Create initial transaction.
7. CAS-create root manifest with putIfAbsent.
```

Export flow:

```text
1. Read root manifest.
2. Load refs/ref shards.
3. Download required packs.
4. Materialize standard bare Git repository.
5. Run git fsck.
6. Produce portable repo archive if requested.
```

Customers MUST be able to recover a standard Git repository from the bucket representation.

## 35. Open-source project scope

OSS SHOULD include:

```text
Git protocol gateway
local filesystem adapter
AWS S3 adapter
Google Cloud Storage adapter
Cloudflare R2 adapter
Azure Blob adapter
storage conformance tests
repo import/export
basic token auth
basic SSH auth
basic GC
basic observability
```

Optional CLI:

```text
bucketvcs init
bucketvcs serve
bucketvcs import
bucketvcs export
bucketvcs doctor
bucketvcs conformance-test
bucketvcs gc
bucketvcs inspect-manifest
```

## 36. Commercial product scope

Hosted product SHOULD add:

```text
multi-tenant orgs
web UI
per-user billing
included hosted storage
BYOB bindings
quotas
usage reporting
SSO/SAML/OIDC/SCIM
audit logs
webhooks
pre-receive policy engine
managed GC/repack/bundles
support plans
```

## 37. Enterprise self-hosted scope

Enterprise deployments SHOULD support:

```text
Helm chart
Terraform modules
private networking
customer-managed object storage
customer-managed encryption keys
customer-managed control metadata DB where used
air-gapped mode where possible
managed upgrades
SIEM export
policy-as-code
support bundle generation
```

## 38. Pricing model

Suggested packaging:

```text
OSS:
  self-hosted gateway
  local/S3/GCS/R2/Azure adapters
  conformance suite
  import/export

Team hosted:
  $/user/month
  included storage
  extra storage per GB/month
  generous fetch/clone bandwidth if R2-backed

BYOB team:
  $/user/month platform fee
  customer pays storage and egress directly
  bucket conformance and monitoring included

Enterprise:
  annual contract
  SSO/SCIM/audit/private networking
  managed self-hosted
  support and compliance features
```

Billing dimensions:

```text
users
repositories
logical Git storage
physical packed storage
LFS storage
bundle/cache storage
retention overhead
clone/fetch egress where applicable
object operation volume
support tier
```

## 39. Go implementation strategy

bucketvcs SHOULD be implemented in Go.

The main service, storage adapters, auth layer, protocol gateway, workers, CLI, and conformance suite SHOULD all be Go code.

### 39.1 Phase 1: protocol-compatible MVP

MUST support:

```text
Go HTTP Smart Git gateway
Go SSH Git gateway
clone
fetch
push
ls-remote
S3/GCS/R2/Azure adapters
root manifest CAS
immutable packs
transaction log
basic reachability index
basic read cache
import/export
conformance tests
```

### 39.2 Phase 2: production hardening

Add:

```text
GC
repack
commit graph
bitmap generation
bundle generation
push serialization
multi-tenant auth
BYOB setup
quotas
audit logs
webhooks
pre-receive policy
LFS
```

### 39.3 Phase 3: acceleration

Add:

```text
bundle-uri
packfile-uri
CDN integration
regional hot pack cache
partial clone optimization
multi-pack index
advanced reachability index
```

### 39.4 Phase 4: enterprise

Add:

```text
SAML/OIDC/SCIM
private networking
customer-managed keys
self-hosted deployment
managed upgrades
SIEM export
policy-as-code
```

## 40. Go implementation architecture and prior art

The implementation language is Go.

JGit DFS remains important prior art, but bucketvcs should not depend on the JVM if the project requirement is an all-Go implementation.

### 40.1 Recommended Go repository layout

```text
cmd/bucketvcs/
  main.go

internal/gitproto/
  pktline/
  capabilities/
  httpgit/
  sshgit/
  uploadpack/
  receivepack/

internal/repo/
  manifest/
  refs/
  tx/
  reachability/
  packindex/
  gc/

internal/storage/
  objectstore.go
  s3/
  gcs/
  r2/
  azureblob/
  tigris/
  minio/
  localfs/

internal/auth/
  tokens/
  sshkeys/
  permissions/
  sso/

internal/hooks/
internal/lfs/
internal/webhooks/
internal/billing/
internal/audit/
internal/conformance/
```

### 40.2 Go protocol gateway

HTTP gateway:

```text
net/http handlers for Smart HTTP endpoints
pkt-line parser/encoder in Go
stateless-RPC request handling
Basic token auth middleware
LFS batch API handlers
```

SSH gateway:

```text
Go SSH server
public key authentication
forced-command parsing
only git-upload-pack / git-receive-pack allowed
same authorization engine as HTTPS
```

Implementation options:

```text
golang.org/x/crypto/ssh
or a thin framework such as gliderlabs/ssh
```

### 40.3 Git engine strategy in Go

Track A and Track B are not a single either/or cutover. The realistic path is gradual migration by code path.

#### Track A: upstream Git as oracle and temporary helper

The bucketvcs service is Go, but during early development it MAY invoke the system `git` binary in isolated paths for:

```text
compatibility tests
import/export
fsck
pack validation comparisons
reference behavior checks
admin repair tools
```

Upstream Git SHOULD be treated as the compatibility oracle.

#### Track B: pure-Go serving path

The long-term serving path SHOULD implement protocol handling, negotiation, validation, pack handling, and manifest commits in Go.

Potential building blocks:

```text
go-git for selected plumbing/reference behavior where suitable
custom pkt-line and protocol v2 implementation where needed
custom pack index/reachability layer for object-store-native reads
provider SDKs for storage operations
```

#### Differential harness

A differential test harness is a core deliverable, not a nice-to-have.

For each supported operation, tests SHOULD compare bucketvcs behavior against upstream Git:

```text
same refs advertised
same push acceptance/rejection behavior
same clone/fetch object closure
same fsck result after clone
same tag and symref behavior
same shallow clone behavior where supported
same LFS behavior where supported
same behavior across supported manifest schema versions
```

Promotion rule:

```text
A pure-Go serving path SHOULD NOT be promoted to default serving traffic until it passes 100% of the required differential harness for the supported feature set across the client matrix in §41.

For hosted production, a newly promoted pure-Go path SHOULD run in shadow or canary mode for at least 4 weeks, or an equivalent volume-based threshold, with no correctness divergences before full rollout.
```

Differential harness divergences MUST be triaged. Each divergence must be classified as:

```text
bucketvcs bug to fix
upstream Git/client quirk to emulate
intentional documented difference
unsupported optional capability that must not be advertised
invalid test case
```

Intentional differences MUST have written rationale and compatibility impact. The known-divergence list MUST be a maintained project artifact, reviewed during Track B promotion decisions. It must not become a dumping ground for correctness bugs.

The migration from helper/oracle paths to pure-Go paths SHOULD happen incrementally:

```text
start with upstream Git for validation and test oracle
implement pure-Go read path components
compare continuously
move one serving path at a time
keep upstream Git in CI as compatibility oracle
```

### 40.4 Storage adapters in Go

Recommended SDKs:

```text
AWS S3: aws-sdk-go-v2
GCS: cloud.google.com/go/storage
R2: S3-compatible API via aws-sdk-go-v2, plus optional Cloudflare-specific config
Azure Blob: Azure SDK for Go
Tigris: S3-compatible API via aws-sdk-go-v2
MinIO: MinIO Go SDK or S3-compatible path via aws-sdk-go-v2 where practical
```

Each adapter MUST implement the same `ObjectStore` contract and pass the conformance suite.

### 40.5 Prior art to study

JGit DFS and Gerrit remain the closest prior art for Git-on-non-filesystem storage.

bucketvcs differentiates through:

```text
Go implementation
multi-cloud object-store adapter contract
bucket-only root manifest CAS model
BYOB as a first-class product surface
storage conformance suite
R2/GCS/S3/Azure/Tigris/MinIO portability
commercial hosted + self-hosted packaging
```

### 40.6 Compatibility rule

Because bucketvcs wants native Git compatibility, the Go implementation MUST maintain a test suite that compares behavior against upstream Git.

The project SHOULD treat upstream Git as the compatibility oracle even if the serving path is pure Go.

## 41. Compatibility test matrix

Test against:

```text
Git CLI latest stable
Git CLI versions in common Linux distributions
Git for Windows
macOS Apple Git
libgit2 clients
JGit clients
popular IDEs
popular CI systems
Git LFS client
```

Required command tests:

```text
git clone
git clone --depth=1
git fetch
git fetch --tags
git pull
git push
git push --force-with-lease
git push --delete
git tag -a && git push --tags
git ls-remote
git submodule update where permissions allow
git fsck after clone
git gc after clone
git lfs clone/pull/push if LFS enabled
```

## 42. Security requirements

The service MUST:

1. Never expose bucket credentials to Git clients.
2. Use short-lived signed URLs for direct object reads.
3. Validate all received packs before commit.
4. Enforce authorization before ref advertisement where private refs are hidden.
5. Enforce authorization before object access.
6. Prevent path traversal through repo names, refs, and object keys.
7. Treat all object keys as generated internal paths.
8. Encrypt data in transit.
9. Support provider encryption at rest.
10. Support customer-managed keys where providers allow.
11. Log administrative access.
12. Isolate hook execution.
13. Avoid leaking private repo object existence across tenants.

## 43. What kills this: adversarial scenarios

The design is not ready unless these scenarios have convincing answers.

### 43.1 High-contention monorepo: 50 pushes/minute

Risk:

```text
root manifest CAS retry storm
push starvation
slow pre-receive hooks amplify queue delay
```

Required answer:

```text
per-repo or per-ref-shard serialization
bounded queue wait metrics
fast stale-push rejection
manifest CAS still final commit point
branch protection domains for parallelism
```

### 43.2 Cold-cache fetch with five years of history

Risk:

```text
fetch negotiation causes many object-store range GETs
pack delta chains amplify latency
clone appears correct but slow
```

Required answer:

```text
manifest-referenced reachability index
commit graph with generation numbers
bitmap packs for large repos
pack/index cache
range GET coalescing
bundle URI for cold clone
```

### 43.3 100k refs with concurrent churn during GC

Risk:

```text
root manifest too large
ref shard updates race GC
GC deletes objects reachable from recent ref state
```

Required answer:

```text
immutable content-addressed ref shards
root manifest remains single commit point
GC marks current and recent manifests
minimum retention window
GC rechecks reachability before sweep
```

### 43.4 BYOB bucket with weak S3-compatible semantics

Risk:

```text
lost updates
stale reads
silent overwrite on multipart complete
incorrect list during repair
```

Required answer:

```text
mandatory conformance test
backend denied canonical mode if tests fail
clear tiering of supported storage systems
```

### 43.5 Region far from users

Risk:

```text
push latency dominated by manifest CAS round trip
read path slow without regional cache
replication exposes stale refs
```

Required answer:

```text
single-writer region per repo
regional read caches
manifest-version-aware ref advertisement
explicit multi-writer non-goal until consensus/global CAS exists
```

Under regional partition, replicas favor consistency of ref advertisement and may go unhealthy rather than serve indefinitely stale refs; see §26.2.

### 43.6 Push during GC sweep

Risk:

```text
GC mark phase decides pack X is unreachable
a concurrent push commits a new manifest that references pack X
GC sweep deletes real repository data
```

Required answer:

```text
GC never sweeps immediately after mark
GC observes minimum retention window
GC re-reads current manifest before sweep
GC marks current and recent manifests
newly committed manifests win because root manifest CAS is authoritative
packs younger than retention window are never swept
```

Retention MUST be measured from the time an object became unreachable, not only from object creation time.

A pack created at T0, referenced by a manifest at T1, and made unreachable by a force-push at T2 MUST remain protected until at least T2 + retention_window.

GC mark records SHOULD track:

```text
first_seen_unreachable_at
last_seen_reachable_at
mark_manifest_version
sweep_attempted_at
```

Sweep is allowed only if the object is still unreachable after re-reading current and recent manifests and its `first_seen_unreachable_at` exceeds the retention window.

### 43.7 Manifest schema migration

Risk:

```text
new code expects manifest schema v2
old repos still have schema v1
rewriting every repo at deploy time is unsafe and expensive
```

Required answer:

```text
manifest readers are versioned
new binaries read all supported old versions
writers emit current version
lazy migration occurs on successful write/maintenance CAS
bulk migration is optional and resumable
unknown future versions fail closed
```

Manifest objects SHOULD include an explicit minimum reader version:

```json
{
  "schema_version": 2,
  "min_reader_version": "1.4.0"
}
```

During rolling deploys, readers must be upgraded before writers emit a manifest schema requiring a newer reader. Existing manifests do not need to be rewritten during the reader-upgrade phase; old manifests keep their existing schema and minimum-reader requirement until a later write or maintenance CAS migrates them. A gateway that sees a manifest requiring a newer reader MUST refuse to serve the repository safely rather than ignore unknown fields or return partial/wrong data.

The differential harness MUST include schema-version compatibility tests:

```text
old manifest read by new binary
new manifest rejected by old binary where appropriate
lazy migration on write
repair/export across schema versions
Track A helper/oracle paths and Track B pure-Go paths reading the same manifests
```

## 44. Open questions

1. What license should the first public commit use, given that changing license later can require contributor consent unless a CLA or similar contributor agreement preserves relicensing optionality? Options include Apache/MIT, AGPL, BUSL/source-available with delayed open license, dual licensing, or another model.
2. What governance model should bucketvcs use: single-vendor open source, foundation-backed, benevolent dictator, or formal maintainer council?
3. Should the project require a contributor license agreement, developer certificate of origin, or another contribution model to preserve licensing and governance flexibility?
4. How much of the serving path should be pure Go in v1 versus using upstream Git as a compatibility oracle/helper?
5. Should ref sharding be implemented in v1 or only after threshold?
6. What is the first hosted backend: R2, S3, or customer choice?
7. Should Tigris be treated as a first-class global backend after conformance?
8. What is the minimum LFS support needed for commercial launch?
9. Should arbitrary pre-receive hooks be allowed, or only policy-native hooks initially?
10. How much web UI belongs in OSS?
11. How should forks share packs safely?
12. Should cross-repo dedupe be supported initially?
13. What is the default GC retention window?
14. What compliance guarantees are needed for enterprise source custody?
15. Should SSH certificates be enterprise-only or available in OSS?
16. Should HTTPS token auth be the default recommended onboarding path, with SSH keys as advanced/developer setup?

## 45. Out of scope for this design spec

This design spec intentionally does not define every implementation or project-governance decision.

Deferred to implementation plan:

```text
exact database schema for users/orgs/repos/tokens
token string format and hashing/HMAC implementation
KMS/secret-manager integration details
cache eviction algorithms
job scheduler implementation
exact repack, bundle, and compaction default values after benchmarking
hook sandbox implementation
web UI architecture
billing system implementation
```

Deferred to operations plan:

```text
hosted deployment topology
warm-pool scheduling
regional routing
SLOs and alert thresholds
backup/restore runbooks
incident response procedures
upgrade/rollback strategy
```

Deferred to project/governance plan:

```text
open-source license
trademark policy
governance model
contribution process
commercial feature boundary
cloud-provider redistribution policy, which is strategically coupled to license choice and commercial defensibility
```

## 46. Positioning

Primary:

```text
bucketvcs is Git-compatible hosting where your repositories live in object storage.
```

Technical:

```text
An object-store-native Git server with standard Git protocol compatibility and pluggable storage backends.
```

BYOB:

```text
Bring your own S3, GCS, R2, Azure Blob, Tigris, or MinIO bucket and keep source code in your cloud boundary.
```

Hosted:

```text
Normal Git remotes, object-store economics, and a storage architecture built for clone/fetch-heavy workloads.
```
