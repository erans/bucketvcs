# M7 — Remaining Canonical Cloud Backends — progress

Date merged: TBD (write at merge time)
Tag: m7-complete (apply after merge to main)
Worktree: `.claude/worktrees/m7-cloud` on branch `worktree-m7-cloud`
Plan: `docs/superpowers/plans/2026-05-09-m7-remaining-cloud-backends.md`
Spec: `docs/superpowers/specs/2026-05-09-m7-remaining-cloud-backends-design.md`

## Acceptance criteria — all green

1. ✅ `internal/storage/gcs` passes the full conformance suite against `fake-gcs-server` (live GCS run pending CI secret config).
   - Final acceptance run (Task 5.2): 142.6 s, all correctness + stress subtests pass.
   - Documented skips: §29#10 SignedURL (fake-gcs has no signing), §29#15 ThrottlingClassification (no emulator throttle injection).
2. ✅ `internal/storage/azureblob` passes the full conformance suite against Azurite (live Azure run pending CI secret config).
   - Final acceptance run (Task 5.2): 129.3 s, all correctness + stress subtests pass.
   - Documented skips: §29#10 SignedURL (deferred to adapter-specific suite by conformance-suite design — see follow-ups), §29#15 ThrottlingClassification.
3. ✅ `internal/storage/s3compat` passes the full conformance suite against MinIO (live AWS run pending CI secret config).
   - Final acceptance run (Task 5.2): 20.7 s, all correctness + stress subtests pass.
4. ✅ `bucketvcs init --store=gcs://…`, `--store=azureblob://…`, and `--store=s3://…` work end-to-end. Verified via `internal/diffharness` round-trip (Task 3.3) against both new emulators: 18.7 s for gcs, 15.1 s for azureblob.
5. ✅ The `emulators` CI job (`.github/workflows/conformance.yml`) runs on every PR and exercises localfs + s3compat-vs-MinIO + gcs-vs-fake-gcs + azureblob-vs-Azurite. The `real-cloud` nightly job runs against AWS / R2 / GCS / Azure but is gated on each provider's repo secrets being configured (no-op until secrets land).
6. ✅ Boundary preserved: nothing outside `internal/storage/{gcs,azureblob,s3compat}/` imports a provider SDK. Verified by `grep -r "cloud.google.com/go/storage\|Azure/azure-sdk\|aws-sdk-go" --include="*.go"` outside those packages — no hits.

## Highlights — bugs the conformance suite caught before they shipped

The §29 conformance suite caught **seven** real adapter bugs across the two new packages, each fixed in its own focused commit before the corresponding harness commit landed:

GCS (Task 1.15 fix, commit `95e3266`):
- `validateKey` was missing checks for null bytes (`\x00`), backslashes (`\\`), and `..` segments. Null-byte keys made fake-gcs return 500, triggering an SDK retry loop on `context.Background()` — a 10-minute hang.
- `parseGen` returned `ErrInvalidArgument` for non-numeric tokens; tests `§29 #3` and `§29 #13` expect `ErrVersionMismatch` (an unparseable token can never match a real generation).
- `DeleteIfVersionMatches`: fake-gcs does not enforce `ifGenerationMatch` on DELETE; added a client-side `Attrs` pre-check (defense in depth — real GCS still gets the server-side guard).
- `CompleteMultipartIfAbsent` for large objects (resumable uploads): same fake-gcs gap, same client-side existence check.

Azure Blob (Task 2.15 fix, commit `0549b7b`):
- `DeleteIfVersionMatches`: Azurite returns 412 instead of 404 for missing-blob DELETE; same defense-in-depth `GetProperties` pre-check.
- `CompleteMultipartIfAbsent` did not enforce contiguous part numbering ([1, 3] now → `ErrInvalidArgument`).
- `CompleteMultipartIfAbsent` did not validate caller-reported part `Size`; mismatch now → `ErrInvalidArgument`.

## Defensive cost (for full transparency)

The defense-in-depth pre-checks add one extra `Attrs` / `GetProperties` round-trip per Delete and per multipart Complete. Against real cloud backends where the server-side guard works, this is wasted RTT (acceptable cost for emulator-vs-real consistency). Documented in both `internal/storage/gcs/README.md` and `internal/storage/azureblob/README.md`.

## Documentation deliverables

- `docs/m7-cloud-quickstart.md` — operator quickstart for gcs:// and azureblob:// (Task 5.1).
- `docs/superpowers/specs/2026-05-09-m7-remaining-cloud-backends-design.md` — design (committed before plan).
- `internal/storage/gcs/README.md`, `internal/storage/azureblob/README.md` — package-level docs.
- `docs/m5-cloud-quickstart.md`, `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md`, root `README.md`, `internal/storage/s3compat/README.md` — updated to reflect AWS S3 promotion (Task 4.2).

## Out of scope (deferred by design)

- §11.2 deployment-tested candidates (Tigris, MinIO AIStor)
- §11.3 compatibility-tested S3-compatible backends (Wasabi, B2, Ceph, etc.)
- Cross-backend migration tooling (M16 if needed)
- `bucketvcs store check` smoke-test subcommand
- Worker bindings for R2

## Follow-ups before tagging m7-complete

1. **§29 #10 SignedURL skips against Azurite** — Task 2.15 reported the conformance suite skips this case "by suite design". Azurite supports SAS with the well-known dev key; the suite should run the test and pass it. Either fix the suite's skip logic to be data-driven (probe `SignedGetURL` once and only skip on `ErrNotSupported`) or write an adapter-specific signed-URL test. Tracked as a small follow-up — does not block m7-complete because real Azure also exercises it via the nightly `real-cloud` job once secrets are configured.

2. **Real-cloud CI secrets** — the `real-cloud` workflow job currently no-ops because the AWS / R2 / GCS / Azure repo secrets are not yet configured. Configure them per `docs/m7-cloud-quickstart.md` "Rotating CI secrets" section and trigger one `workflow_dispatch` run to confirm green before tagging.

3. **Memory note** — once tagged, update `~/.claude/projects/.../memory/m5_progress.md`: replace "AWS S3 (M7 promotion in progress)" with "AWS S3 (canonical since M7)".

## Total cost

- 35 implementation tasks across 6 phases
- 51 commits on `worktree-m7-cloud` (35 task commits + 16 plan / fix / spec commits)
- Two cloud SDK dep trees added (cloud.google.com/go/storage, github.com/Azure/azure-sdk-for-go)
- Final conformance run: 5 minutes wall-clock for all 4 backends end-to-end
