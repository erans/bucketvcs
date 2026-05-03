# `internal/storage`

Provider-neutral storage layer for bucketvcs. Defines the `ObjectStore`
Go interface and the conformance test suite that every adapter must
pass.

## Status

M0 ships:

- `ObjectStore` interface (`objectstore.go`) and supporting types
- `localfs` adapter (single-process; dev/test use)
- Conformance suite covering the 15 §29 correctness items and 3
  applicable §29 stress items

Cloud adapters (AWS S3, GCS, Cloudflare R2, Azure Blob) ship at M5 and
M7. Each implements the same `ObjectStore` interface and must pass the
conformance suite for the specific backend/configuration in use.

## Contract

All adapters implement [`ObjectStore`](objectstore.go). The contract is
documented inline; key points:

- **Compare-and-swap is the durability primitive.** `PutIfAbsent` and
  `PutIfVersionMatches` are the only ways to write committing changes.
  Provider version tokens are normalized via `ObjectVersion`.
- **Versions are opaque.** Callers compare `ObjectVersion` values for
  equality only. Adapters define the `Token` format (localfs: hex
  sha256 of content; S3: ETag; GCS: generation; etc.).
- **Multipart uploads are required.** Cloud adapters need them for
  large packs (>5 GiB on S3). Localfs implements them so the
  conformance suite can exercise multipart paths against a known-good
  reference.
- **Signed URLs are optional.** Adapters declare support via
  `Capabilities.SignedURLs`; opt-out adapters return
  `ErrNotSupported` from `SignedGetURL`.

## Adding a new adapter

1. Create a sub-package under `internal/storage/<your-adapter>/`.
2. Implement every method of `storage.ObjectStore`.
3. Add a compile-time interface assertion: `var _ storage.ObjectStore = (*Yours)(nil)`.
4. Add a conformance test:

   ```go
   // internal/storage/<your-adapter>/<your_adapter>_conformance_test.go
   package youradapter_test

   import (
       "testing"

       "github.com/bucketvcs/bucketvcs/internal/storage"
       "github.com/bucketvcs/bucketvcs/internal/storage/conformance"
       "github.com/bucketvcs/bucketvcs/internal/storage/<your-adapter>"
   )

   func TestConformance(t *testing.T) {
       conformance.Run(t, func(t testing.TB) (storage.ObjectStore, func()) {
           s, cleanup := setupYours(t)
           return s, cleanup
       })
   }
   ```
5. Run `go test -race ./internal/storage/<your-adapter>/...`. Every
   §29 correctness item must PASS or SKIP-with-documented-reason.
6. Document any expected divergence in your adapter's README.

## Running the conformance suite against an arbitrary adapter

The conformance package is importable from any `_test.go` file. A
future `bucketvcs conformance-test` CLI subcommand (M3) wraps the same
package for runtime execution against a backend URL.

To run against localfs only:

```bash
go test -race ./internal/storage/localfs/...
```

To skip stress tests (faster iteration):

```bash
go test -race -short ./internal/storage/localfs/...
```

## §29 test mapping

| §29 # | Test | File |
|-------|------|------|
| 1 | `§29#1_ConcurrentPutIfAbsent` | `conformance/correctness.go` |
| 2 | `§29#2_ConcurrentPutIfVersionMatches` | `conformance/correctness.go` |
| 3 | `§29#3_FailedConditionalDoesNotAlter` | `conformance/correctness.go` |
| 4 | `§29#4_PutThenGet_RAW` | `conformance/correctness.go` |
| 5 | `§29#5_OverwriteThenRead` | `conformance/correctness.go` |
| 6 | `§29#6_ListAfterWrite` | `conformance/correctness.go` |
| 7 | `§29#7_ListAfterDelete` | `conformance/correctness.go` |
| 8 | `§29#8_MultipartCannotOverwrite` | `conformance/correctness.go` |
| 9 | `§29#9_GetRange` | `conformance/correctness.go` |
| 10 | `§29#10_SignedURL` | `conformance/correctness.go` |
| 11 | `§29#11_DeleteIfVersionMatches` | `conformance/correctness.go` |
| 12 | `§29#12_VersionRoundTrip` | `conformance/correctness.go` |
| 13 | `§29#13_CASConflictClassification` | `conformance/correctness.go` |
| 14 | `§29#14_PutIfAbsentIdempotentRetry` (AD8 recast) | `conformance/correctness.go` |
| 15 | `§29#15_ThrottlingClassification` (skipped on localfs) | `conformance/correctness.go` |

§29 stress items: 100 concurrent CAS, 10k creates, large multipart
conflict — all in `conformance/stress.go`. The remaining two §29
stress items (GC simulation, regional read-after-write) are out of
scope for M0 and live with later milestones.

## §29 #14 recast (AD8)

The original §29 #14 reads "network retry does not duplicate
committed object." Localfs has no network. Per AD8 in the M0 design
doc, the test is recast on localfs as: "PutIfAbsent twice with the
same args returns `ErrAlreadyExists` cleanly without corrupting state
on the second call." Cloud adapters at M5/M7 add a transient-retry-
mid-call case in addition to the localfs-equivalent assertion.

## Localfs caveats

- Single-process. A second `Open` against the same root returns
  `ErrAlreadyLocked`. Multi-process use is unsupported.
- Linux + macOS only. Windows is out of scope for M0.
- Signed URLs: `SignedGetURL` returns `ErrNotSupported`.
- No historical version retention. Each PUT overwrites previous
  content.
- Read paths (`Get`, `Head`, `GetRange`) acquire the per-key keyed
  mutex per AD3, so concurrent reads of the same key serialize.
  Cross-key parallelism is unaffected.
- The keyed-mutex map does not evict idle entries in M0. Long-running
  processes serving millions of distinct keys may want this revisited.

## Localfs key-namespace caveat (prefix overlap)

Localfs maps each object key directly onto `<root>/objects/<key>`,
which means **a key cannot coexist with another key that is a strict
prefix-with-`/`-boundary of it**. Concretely: if `a` exists as an
object, `a/b` cannot be created (the path `<root>/objects/a` is
already a regular file, not a directory). The reverse is symmetric: if
`a/b` exists then path `<root>/objects/a` is a directory, so creating
key `a` fails.

Bucketvcs's structured key namespace
(`tenants/.../repos/.../refs/heads/main`, `blobs/<hash>/<part>`,
manifest paths, etc.) is layered such that this collision never
occurs. A third-party caller using localfs as a generic
S3-compatible-key store with arbitrary keys could hit it. Cloud
adapters at M5/M7 do not have this constraint — S3, GCS, R2, and
Azure Blob have flat key namespaces — so the limitation is localfs-
specific, not part of the `ObjectStore` contract.

The conformance suite explicitly does not exercise prefix-overlap; an
adapter that relies on a flat backend will pass the suite even if
it allows overlap, and localfs passes the suite because its tests
choose non-overlapping keys.

## Localfs reserved key segments

The localfs adapter additionally reserves two filename patterns under
`<root>/objects/` for its own use. Keys violating these reservations
are rejected at the API boundary with `ErrInvalidArgument`:

- **Segments ending in `.meta`.** Each object's JSON sidecar lives at
  `<root>/objects/<key>.meta`, so a key like `foo.meta` would collide
  with the sidecar of object `foo`.
- **Segments starting with `.`.** Atomic-write temp files are named
  with a leading dot (e.g., `.foo.tmp.NNN`); without the reservation,
  in-flight or crashed-leftover temp files would be indistinguishable
  from real keys in a directory listing. As a consequence,
  dot-prefixed keys (`.gitkeep`, `.dotfile`) cannot exist in localfs.

Cloud adapters at M5/M7 do not impose either restriction. Bucketvcs's
internal key namespace does not produce such segments.

## ErrClosed semantics

`Close` is retryable: if either the lockfile-handle close or the
on-disk lockfile removal fails, the receiver remembers the failed
step and a subsequent `Close` retries it. Operators should keep
calling `Close` until it returns `nil`; otherwise a stranded
`<root>/.lock` blocks future `Open` calls.

Once a `Close` succeeds (lockfile released), any subsequent operation
on that `Localfs` instance returns `localfs.ErrClosed` (a localfs-
specific sentinel, not a `storage.Err...`). The instance refuses
service so it cannot scribble on a bucket whose lock another process
may now hold.

Recovery from a hard process crash where no `Close` ever ran is the
job of the package-level `Verify` (see "Crash recovery and
`Localfs.Verify`" below) with `WithForce(true)`.

## Sidecar schema is asymmetric (forward-incompatible by design)

The `.meta` sidecar carries an explicit `Version` integer. The current
binary's `Version` is the highest schema it understands.

- **Sidecar `Version > current`** — the sidecar was written by a newer
  binary. The current binary fails closed with
  `ErrUnsupportedSidecarSchema` and **does not self-heal**. Self-heal
  would silently overwrite the future-schema sidecar with a current-
  schema one, downgrading the on-disk format.
- **Sidecar missing, zero, negative, or otherwise corrupt** — current
  binary recomputes the sidecar from content (sha256, size, mtime)
  and writes it back. This is the standard self-heal path.

**Operator implication.** Do not run an older bucketvcs binary against
a bucket touched by a newer binary. The older binary will refuse all
reads of every object the newer binary has written until either the
newer binary regenerates the sidecars or the bucket is restored.

## Symlink and hardlink safety (AD11) — verbatim from M0 design spec r4

Localfs in M0 implements **best-effort final-path symlink rejection**,
not full path-resolution sandboxing.

**What is covered:**

- All read entry points (`Get`, `Head`, `GetRange`, `List`) call
  `os.Lstat` on the **final** path component before opening. If that
  entry is a symlink, the operation returns `ErrInvalidArgument` and
  `List` skips the entry with a structured warning.
- Write paths create files via `os.OpenFile` with `O_CREATE|O_EXCL`
  on a temp path, then `os.Rename` into place.

**What is NOT covered in M0:**

- **Ancestor-directory symlinks.** If `<root>/objects/foo/` is itself
  a symlink to a directory outside the bucket, files under
  `<root>/objects/foo/bar` actually live outside the bucket. M0 does
  not validate every path component.
- **Hardlinks.** Hardlinks within or outside the bucket cannot be
  detected by `Lstat` alone. M0 does not perform `Nlink>1` checks.
- **TOCTOU between `Lstat` and `Open`.** An attacker who can write
  to the bucket can race symlink replacement against subsequent
  `Open` calls.

The realistic threat in M0 is a casual misconfiguration —
`ln -s /etc/passwd <root>/objects/foo` — which the best-effort check
catches. Operators are responsible for ensuring `<root>/objects/` and
its descendants are normal directories under operator-trusted control.

## Filesystem portability assumptions — verbatim from M0 design spec r4

Localfs in M0 assumes:

- **Case-sensitive POSIX filesystem.** ext4, XFS, btrfs (Linux);
  APFS configured case-sensitive (macOS — note default APFS is
  case-INSENSITIVE; users on default-APFS macOS hosts will see
  CONTENT collisions if they rely on case-distinct keys).
  Unsupported: HFS+ (Unicode normalization folds NFC/NFD).
- **Atomic same-filesystem rename.** Standard POSIX. Crossing
  filesystems via rename is an unspecified error.
- **`fsync` flushes both data and metadata.** ext4 default behavior.
  `noatime` is fine; `data=writeback` is not recommended.
- **Standard file permissions and ownership.** No special handling
  for setuid, sticky, or extended attributes.

Unsupported (will refuse or behave undefined):

- **Network filesystems (NFS, SMB, FUSE).** `flock`/lock-file
  behavior across NFS is unreliable; rename atomicity is not
  guaranteed on all FUSE backends.
- **Windows filesystems.** Path separators, case folding, and
  `O_CREATE|O_EXCL` semantics differ enough that M0 does not target
  Windows.

## Crash recovery and `Localfs.Verify` — verbatim from M0 design spec r5

Localfs's stale-sidecar fast-path (size-mismatch detection on read)
catches the common case where a crashed `PutIfVersionMatches` left
content of a different size than the previous version. It does NOT
catch the rarer case where the new and old content happen to share
the same size. To preserve CAS correctness across crashes, M0 ships
a verifier in the localfs package itself.

**Verifier API:**

- `Localfs.Verify(ctx) error` (instance method): walks every object,
  recomputes sha256, rewrites stale sidecars. Safe to call as
  periodic maintenance on a healthy open bucket.
- `localfs.Verify(root) error` (package-level function): for
  recovery from unclean shutdown. Opens the bucket in repair mode
  (bypassing the lockfile check), reconciles, then clears
  `<root>/.lock` so a subsequent `Open` succeeds.

**Operator workflow on unclean shutdown:**

1. Process running localfs is killed (kill -9, OOM, host crash).
2. New process calls `localfs.Open(root)` →
   returns `ErrAlreadyLocked` because the dead process's `.lock`
   is still present.
3. Operator confirms the prior process is gone (ps, systemctl, etc.).
4. Operator calls `localfs.Verify(ctx, root)`. Verify reads the
   lockfile JSON, performs a POSIX `kill(pid, 0)` liveness check
   on the recorded PID, and if the holder is dead reconciles every
   object and clears `.lock`.
5. New process calls `localfs.Open(root)` → succeeds.

**Operator warning**: passing `localfs.WithForce(true)` to `Verify`
against a bucket whose lockfile points to a live process WILL
corrupt that process's view of the bucket. `WithForce` exists for
cases where M0's heuristics cannot determine liveness (e.g.,
cross-host buckets, containers with namespaced PIDs); operators are
responsible for the safety judgment.

**Future M16 integration**: `bucketvcs doctor` will wrap
`localfs.Verify` (and equivalents for cloud adapters) as a CLI
subcommand. The underlying primitive ships in M0; the "doctor" name
is reserved for the M16 wrapper.

This requirement applies to localfs only. Cloud adapters at M5/M7
do not have an equivalent torn-state because their version tokens
are server-side.

## Module path placeholder

`github.com/bucketvcs/bucketvcs` is a placeholder pending governance
gate G1 (license + repo host). Substitute the real path once G1 is
settled; the contract and behavior are unchanged.
