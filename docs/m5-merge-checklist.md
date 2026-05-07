# M5 Merge Checklist

Apply these steps after M5 merges to main:

## Tag

```bash
git tag -a m5-complete -m "M5 first cloud backend (s3compat for R2 + S3)"
git push --tags
```

## Auto-memory update

Add to `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md`:

```markdown
- [M5 first cloud backend merged to main](m5_progress.md) — commit <hash>, tag m5-complete (2026-05-XX); s3compat adapter for R2 (canonical) + AWS S3 (M7 promotion in progress)
```

Create `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m5_progress.md`:

```markdown
---
name: M5 first cloud backend merged to main
description: M5 progress entry — s3compat adapter for R2 (canonical) + AWS S3, conformance-gated
type: project
---

M5 first cloud backend merged to main.

- Commit: <fill in after merge>
- Tag: m5-complete (2026-05-XX)
- Package: internal/storage/s3compat (single S3-compatible adapter via aws-sdk-go-v2)
- Schemes wired in cmd/bucketvcs: s3:// (AWS S3 + MinIO) and r2:// (Cloudflare R2)
- Conformance: full §29 suite (15 correctness + stress) passes against R2 (canonical at M5) and S3 (formally promoted to canonical at M7)
- Diffharness: clone/import/export oracles pass against live R2 via BUCKETVCS_DIFFHARNESS_STORE
- Ship gate: scripts/conformance-cloud.sh
- CI: .github/workflows/conformance-cloud.yml (forward-looking; activates when project CI lands)

What's NOT in M5:
- GCS (M7)
- Azure Blob (M7)
- R2 Worker bindings (post-MVP, deferred)
- localfs->cloud migration tool (manual aws s3 sync documented in docs/m5-cloud-quickstart.md)

Open residuals (flagged for T13 conformance / future polish):
- UploadPart's failure-path race with concurrent terminal ops can leak ErrNotFound from a NoSuchUpload SDK response (narrow window between checkActive and SDK call). Closed roborev as diminishing returns.
- Mock multipart Complete handler doesn't parse the SDK's CompletedPart XML — accepted divergence; live conformance is the authority.
- SignedGetURL semantics (signature validity, write-rejection on non-GET) verified only by URL form in unit tests; conformance #10 against live R2/S3 is the authority.
```
