# bucketvcs OSS Core — Sub-Project Decomposition

Date: 2026-05-03
Status: design draft
Scope: OSS self-hosted core only (excludes hosted control plane, multi-tenant orgs, billing, web UI, BYOB UX, enterprise SSO/SCIM, multi-region replicas, warm pools)
Source spec: `docs/original-spec.md`

## What this document is and is not

This is a **planning artifact**, not a technical design for any single subsystem. It decomposes the bucketvcs OSS-core specification into independently buildable sub-projects (milestones), with dependencies, ordering, and rationale.

Each milestone listed here will get its own design → plan → implementation cycle in a later session. Detailed architecture (storage adapter interface shape, manifest schema fields, pkt-line parser internals, CAS retry policies, etc.) is **out of scope for this document** and lives in the per-milestone specs.

## Organizing principle

Sub-projects are sliced as **vertical end-to-end milestones** rather than horizontal layers. Each milestone, once shipped, produces a usable artifact (library, CLI, or running service). This validates the central thesis (manifest CAS + immutable packs over object storage actually serves Git correctly) on real workloads early, and allows stopping or re-ordering at any milestone boundary without leaving an unusable mid-state.

The alternative — full storage layer, then full engine layer, then full protocol layer — is the structure of the original spec but would mean ~6 months before anything clones, deferring the first reality check on the central design.

## Scope boundary

In scope (from spec §3.0 and §35, plus Phase-2 OSS additions from §39.2):
- Git protocol gateway (HTTP + SSH)
- Storage adapters: localfs, AWS S3, GCS, Cloudflare R2, Azure Blob
- Storage conformance suite
- Repo import/export
- Basic token auth + SSH key auth
- Basic GC and background maintenance
- LFS, hooks/policy, webhooks, audit
- Reachability acceleration, bundles, sharded refs (when load demands)
- Repair tooling
- Basic observability

Out of scope (deferred):
- Hosted multi-tenant control plane, web UI, billing
- BYOB onboarding UX
- Enterprise SSO/SAML/OIDC/SCIM
- Multi-region read replicas, warm pools
- Tigris and MinIO adapters (deferred — not in §35 minimum)
- Tigris global multi-writer experiments
- Helm charts, Terraform modules

The OSS service is single-tenant or admin-managed-multi-tenant; per-org/per-user multi-tenancy belongs to the hosted product.

## Pre-work — governance gates (block first public commit)

These do not block engineering work in a private repo, but MUST be settled before pushing to a public remote, because changing them later requires contributor consent (or a CLA already in place) and is socially expensive.

| Gate | Decision | Why it gates a public commit |
|------|----------|------------------------------|
| G1 | License (Apache/MIT vs AGPL vs BUSL-with-delayed-OSS vs dual) | Determines what the project can call "open source," cloud-provider redistribution defensibility, and what every contributor agrees to from the first commit |
| G2 | Contribution model (CLA vs DCO vs neither) | CLA preserves relicensing optionality. DCO does not. Neither makes future relicensing impossible without contacting every contributor |
| G3 | Governance + trademark intent (single-vendor / foundation / BDFL) | Cheap to defer formally, but a stated intent on day one helps prospective contributors decide whether to engage |

Open spec questions §44.1, §44.2, §44.3 map directly to G1, G2, G3.

## Part A — Critical-path MVP (strictly sequential)

After **M3** there is a working OSS Git server backed by local filesystem. After **M8** the §35 OSS-scope minimum is complete and the project is releasable as OSS v1.

| # | Milestone | Spec sections | Ships |
|---|-----------|---------------|-------|
| M0 | Storage adapter contract + localfs adapter + conformance suite skeleton | §9, §10, §29 (subset), §40.4 | `internal/storage` provider-neutral interface; localfs adapter passing all 15 correctness tests + the basic stress tests |
| M1 | Repository state engine: durable key layout, root manifest CAS, immutable transaction records | §6, §7, §8, §43.7 (schema versioning), §40.1 (`internal/repo`) | Library that creates/reads/updates a repo's durable state, demonstrably correct under concurrent CAS contention |
| M2 | Git object engine: pack read/write, basic inline-only reachability index, inline refs (§19.1), import/export, differential harness scaffolding | §14 (basic), §15.1, §19.1, §20 (SHA-1), §21, §34, §40.3 | `bucketvcs import` and `bucketvcs export` round-trip a bare git repo with `git fsck` clean on both ends; first differential tests run against upstream git |
| M3 | HTTP Smart Git gateway: pkt-line, capability negotiation, protocol v2, clone/fetch/push end-to-end, in-process per-repo push serialization | §2, §13 (HTTP only), §16.1, §17, §18 (basic), §41 | `git clone http://localhost/...` works against a localfs-backed repo; differential harness covers clone closure, ref advertisement, and push acceptance/rejection |
| M4 | HTTPS token authentication: HTTP Basic with token-as-password, hashed at rest per §30.5 | §30.1, §30.3, §30.5 | Authenticated clone/fetch/push from real Git clients via `git credential` helpers |
| M5 | First cloud backend: Cloudflare R2 (recommended per §27 default) or AWS S3 | §11.1, §12.1 or §12.3 | End-to-end clone/fetch/push against a real cloud bucket; chosen backend passes full conformance suite |
| M6 | SSH gateway + SSH public-key authentication, sharing the M4 authorization engine | §30.2, §30.3, §40.2 | `git@host:org/repo.git` works; SSH and HTTPS map to the same actor and permission decisions |
| M7 | Remaining canonical cloud backends: AWS S3 (promoted from M5 conformance), GCS, Azure Blob | §11.1, §12.x | Each backend passes conformance; all four §11.1 schemes (s3://, r2://, gcs://, azureblob://) are canonical |
| M8 | Basic GC: mark/sweep with retention window and §43.6 push-vs-sweep correctness rules; immutable GC mark and sweep records | §25, §33.1, §33.5, §43.6 | `bucketvcs gc` reclaims orphaned packs without ever deleting a still-reachable object; long-running deployments stop leaking storage |

After M8: §35 OSS-scope minimum is complete. First OSS release candidate.

### Notes on critical-path decisions

- **Push serialization in M3**: in-process per-repo serialization belongs in M3 even though the spec discusses §18 alongside larger distributed mechanisms, because correctness suffers without any serialization once two concurrent pushes reach the same gateway. Distributed lease/queue mechanisms are deferred to M12.
- **Auth before cloud (M4 before M5)**: M5 requires somewhere to send a credential. Doing M4 first means M5 can be exercised by external clients on day one.
- **Differential harness scaffolded in M2, not its own milestone**: it is a multi-month investment but it is useless without something to test against. Treating it as cross-cutting (running continuously, deepening every milestone) matches how it actually gets built. Promotion gate from §40.3 (100% pass + 4-week shadow) is enforced from M3 onward.
- **First cloud backend choice**: R2 matches the §27 hosted economic default but has fewer existing reference implementations. S3 is the most-tested provider and has the most prior art. Either is defensible; recommend R2 to validate the economic thesis early. Decision deferred to M5 spec.
- **Basic GC at M8, not later**: §35 lists basic GC as part of OSS scope, and §43.6 is one of the trickier correctness scenarios in the spec. Without it any long-running deployment leaks storage forever, so GC lands before OSS v1 ships rather than as a Phase-2 add-on.

## Part B — Post-MVP OSS milestones (parallelizable after M8)

These complete §35 + Phase-2 OSS features (§39.2). Most can proceed in parallel; touch different code paths.

| # | Milestone | Spec sections | Why it lives here |
|---|-----------|---------------|-------------------|
| M9 | Background maintenance: repack canonical packs, generate commit graph, generate bitmaps, retire old generated packs, recent-pack accumulation bounds | §15.3, §21 | M2 produced packs but no consolidation. Without this, recent-pack count grows unbounded against §15.3 thresholds |
| M10 | Reachability compaction: base + delta index model, partitioning for large repos, compaction CAS protocol | §14.1, §14.2, §14.3, §14.4 | M2 had basic reachability only. Compaction is what keeps cold fetch viable at scale |
| M11 | Bundle URI + packfile URI + bundle freshness machinery (current/warm/stale/retired states) | §3.1 (eventual), §16.3, §16.4 | Pure acceleration on top of M9 |
| M12 | Sharded refs + push serialization beyond in-process (durable-object/lease/queue model, ref resharding maintenance) | §18 (full), §19.2, §19.3 | Inline refs (M2) work fine until ~10k refs or heavy multi-gateway contention. Defer until measured need |
| M13 | LFS: batch API, content-addressed layout, LFS auth scope; locking API explicitly gated as separate sub-deliverable | §22 | Independent surface; does not intersect Git pack path |
| M14 | Hooks + policy engine: pre-receive, update, post-receive; policy-native checks (protected branches, force-push, signed commits, file size, path restrictions) before arbitrary user code | §23 | Plugs into M3 receive-pack flow but additive |
| M15 | Webhooks + audit: async retryable webhook delivery; structured audit event emission for §31 list | §24, §31 | Both sit on top of M3/M14; audit can start as logs and harden later |
| M16 | Repair / recovery tooling: `bucketvcs doctor`, manifest reconstruction from transaction chain, partial-multipart cleanup, orphan pack listing, broken-CAS recovery | §33, §34 (recovery side) | M0–M8 produce the data; this milestone exists to fix it when something goes wrong |

### Notes on post-MVP ordering

- **M8 → M9 → M10/M11 maintenance ordering**: GC first so acceleration is not built on top of leaking storage; repack before bundles because bundles want consolidated packs.
- **M16 (repair) should be started concurrently with M8**, not deferred to last. Repair tooling is consistently neglected in young infrastructure projects and then desperately needed during the first real outage. Treat M16's number as topical placement, not scheduling.
- **M12 depends on M9** (avoid designing ref sharding without seeing observed repack patterns first).
- **M13–M15 are independent of each other** once M8 lands.

## Cross-cutting tracks (continuous, not their own milestones)

| Track | Starts at | What it is |
|-------|-----------|------------|
| Differential test harness vs upstream Git | M2 | Scaffolded in M2, deepens every milestone, CI-gating from M3. Promotion rule (§40.3): no pure-Go path becomes default until 100% differential pass + 4-week shadow. The known-divergence list (§40.3) is a maintained project artifact, not a place to hide bugs |
| Storage conformance suite | M0 | Built in M0, expands at M5 and M7. Every new backend must pass before being labeled canonical. Stress tests added as load patterns become understood |
| Observability | M0 | Structured logging from M0; metrics named in §32 added per-milestone as the corresponding code path lands |
| `bucketvcs` CLI | M0 | Subcommands accumulate across milestones (`init`, `serve`, `import`, `export`, `doctor`, `conformance-test`, `gc`, `inspect-manifest`) per §35 |
| Documentation | M0 | Architecture overview, operator guide, contributor docs grow continuously |

## Dependency graph summary

```text
G1, G2, G3  (governance gates — parallel, gate first public commit)
            |
            v
M0  ->  M1  ->  M2  ->  M3
                              \
                               M4 -> M5 -> M6
                                            \
                                             M7 -> M8 -> { M9, M16 }
                                                          |
                                                          v
                                                          M10, M11   (after M9)
                                                          M12        (after M9)
                                                          M13, M14, M15  (parallel after M8)
```

Cross-cutting tracks run in parallel with everything from their starting milestone onward.

## What is explicitly NOT a milestone in this decomposition

- **Multi-tenant orgs / per-user accounts** — hosted concern (§3.0, §36)
- **Web UI** — hosted concern, with §44.10 deferring to project decision
- **Multi-region read replicas / warm pools** — operational layer, gated by hosted control plane (§26)
- **Tigris and MinIO adapters** — not in §35 minimum, conformance-tested candidates per §11.2
- **BYOB onboarding** — hosted product surface; the OSS adapter contract already supports any conformance-passing backend
- **Billing, quotas, usage dashboards** — hosted concern (§27, §38)
- **Enterprise SSO/SCIM/SAML, private networking, customer-managed keys, SIEM export** — enterprise scope (§37)
- **Helm chart, Terraform modules** — operational packaging, deferred until OSS v1 ships

## Next steps

1. User reviews this decomposition.
2. Pick the first milestone to brainstorm in detail. Default recommendation: **M0** (storage adapter contract + localfs + conformance suite skeleton), since every other milestone depends on it and its boundaries are unusually clear.
3. Run the brainstorming → spec → writing-plans → executing-plans cycle for that milestone.
4. Repeat for subsequent milestones; parallelize where the dependency graph allows.

Open questions from spec §44 that this decomposition does not resolve and that should be answered before or during the relevant milestone:
- §44.6 (first hosted backend choice) — answered at M5
- §44.4 (how much serving path is pure Go in v1) — answered iteratively across M2/M3 via the differential harness
- §44.5 (ref sharding in v1 or only after threshold) — this decomposition defers to M12, post-MVP
- §44.13 (default GC retention window) — answered at M8
- §44.16 (HTTPS-token-auth as default onboarding path vs SSH) — answered at M4/M6
- §44.1, §44.2, §44.3 — gates G1, G2, G3 above
