# M5 Smoke-Test Follow-Ups

End-to-end smoke test on 2026-05-07 (real `git push` + `git clone` against a
local MinIO RELEASE.2025-09-07-T16-13-09Z, via the new `s3://` adapter)
surfaced two pre-existing defects that are out of M5 scope but worth fixing
before M8/M9.

The smoke-test setup itself (`docker-compose.minio.yml` + the manual binary
fallback when Docker fails) lives in repo root for reproducibility.

## FU-1: `git pack-objects` cross-device link error in receive-pack

**Observed:** `git push` to a `bucketvcs serve` instance fails with
`internal-storage-error` when the mirror cache and the repack temp dir are
on different filesystems.

**Server-side error (visible only by adding `fmt.Fprintf` to receive-pack):**
```
importer: BuildAndCommit: pack-objects: gitcli: PackObjectsAll:
pack-objects: exit status 128:
stderr="error: unable to write file
/tmp/bucketvcs-repack-NNN/pack-XXX.pack: Invalid cross-device link
fatal: unable to rename temporary file to
'/tmp/bucketvcs-repack-NNN/pack-XXX.pack'"
```

**Root cause:** `internal/importer.BuildAndCommit` calls
`gitcli.PackObjectsAll` which runs `git pack-objects` against the mirror's
bare repo. Git's pack-objects writes to a temp file in (or near) its `--keep`
target and then `rename(2)`s into place. When the mirror's bare repo is on
one filesystem (e.g. `~/.cache/bucketvcs/mirrors/...` on the user's home
mount) and the temp dir is on another (e.g. `/tmp` as tmpfs), the rename
fails with `EXDEV`.

**Why the diffharness doesn't catch it:**
`internal/diffharness/roundtrip_helpers_test.go` builds both source bare and
destination via `t.TempDir()`, which puts everything under
`$TMPDIR` on a single device. Real deployments use distinct dirs; CI often
puts them on the same device, but not always.

**Workaround for operators:** pass `--mirror-dir` colocated with `$TMPDIR`,
or set `GIT_OBJECT_DIRECTORY` consistently. Smoke test confirmed
`--mirror-dir=/tmp/...` makes the push succeed.

**Fix scope (M2/M3 importer):**
- `gitcli.PackObjectsAll` should create its repack temp dir adjacent to the
  bare repo's `objects/pack/` (same filesystem), not unconditionally in
  `$TMPDIR`.
- Or: catch `EXDEV` in the rename-back path and fall back to copy.

**Severity:** Medium. Production breakage on common operator setups (any
deployment with the mirror cache on a non-tmpfs filesystem). Not blocking
M5 ship because the storage adapter itself is correct — the bug surfaces
in the mirror→bucket commit path, not in any s3compat code.

**Suggested milestone:** M9 (background maintenance / repack consolidation
already touches this code path).

## FU-2: No server-side logging in the gateway

**Observed:** When receive-pack fails (e.g. FU-1), the client sees
`internal-storage-error` but the server logs nothing. The `bucketvcs serve`
process produces no structured logs at all today: no slog handler, no
`log.Print`, no error stream.

**Impact:** Operators cannot diagnose push failures without modifying
gateway source. During the smoke test, identifying FU-1 required adding a
temporary `fmt.Fprintf(os.Stderr, ...)` to
`internal/gateway/receive_pack.go`.

**Fix scope (M3 gateway, light):**
- Initialize a default `slog.Handler` in `cmd/bucketvcs/serve.go` (e.g.
  `slog.NewTextHandler(os.Stderr, ...)`) before constructing the gateway.
- Log every failed `BuildAndCommit`, `IndexPackStrict`, and CAS-conflict
  case at `slog.LevelError` with structured fields (`tenant`, `repo`,
  `actor`, `reason`, `err`).
- Optionally: log every receive-pack and upload-pack request at
  `LevelInfo` with timing, byte counts, and outcome.

**Severity:** Low for correctness, High for operability. Without it,
production debugging requires a custom build with `fmt.Fprintf`s.

**Suggested milestone:** M15 (webhooks + audit) is the natural home for
"what happened" event emission, but a minimal slog handler should land
sooner — could be a 50-line standalone PR at any time.

## Smoke-test artifacts

- `docker-compose.minio.yml` — runnable MinIO setup (when Docker host is
  healthy; the test machine had a broken nvidia containerd shim, so the
  test ended up using direct `minio` binary instead).
- `docs/m5-cloud-quickstart.md` — Cloudflare R2 setup walkthrough (T17).

## Smoke-test summary

After workaround for FU-1 (mirror dir on `/tmp`):

```
=== bucket contents after push ===
indexes/commit-graph/...bvcg                  125B
indexes/object-map/...bvom                    266B
manifest/root.json                          1.1KiB
packs/canonical/...idx                      1.2KiB
packs/canonical/...pack                       413B
tx/tx_<initial>.json
tx/tx_<push>.json

=== inspect-manifest ===
manifest_version: 2  (atomic CAS from 1 -> 2)
refs: 1 entries     (refs/heads/main)
packs: 1 entries
indexes: 2 entries  (object-map + commit-graph)
```

Real `git clone` of the repo afterward returned identical bytes for both
files committed via the original push. Round trip verified.
