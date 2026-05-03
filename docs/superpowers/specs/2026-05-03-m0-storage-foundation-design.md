# M0 — Storage Foundation: ObjectStore Contract, localfs Adapter, Conformance Suite

Date: 2026-05-03
Status: design draft (revision 8)
Milestone: M0 (first critical-path milestone of bucketvcs OSS-core)
Source spec sections: §9, §10, §29 (subset), §35, §40.1, §40.4
Decomposition: see `2026-05-03-bucketvcs-oss-decomposition-design.md`
Implementation plan: see `docs/superpowers/plans/2026-05-03-m0-storage-foundation.md` (35 bite-sized TDD tasks)

Revision history:
- 2026-05-03 r1: initial design.
- 2026-05-03 r2: address roborev design review (job 7684) findings — read-side mutex acquisition for atomicity; `PutIfAbsent` Stat-then-rename made explicit; `PutOptions.Metadata` removed for M0; multipart lifecycle table added; symlink rejection added; filesystem portability assumptions documented; sidecar schema versioning surfaced from plan; part numbering pinned to 1-based.
- 2026-05-03 r3: address roborev design review (job 7686) findings — AD11 narrowed to best-effort final-path symlink rejection with documented limitations (ancestor symlinks, hardlinks, TOCTOU not covered); directory `fsync` added to the atomic write pattern for crash durability; version-conditional operation contract table added; plan stages summarized inline; crash-recovery semantics tabulated.
- 2026-05-03 r4: address roborev design review (job 7688) findings — stale-sidecar detection on read added (size-mismatch fast-path triggers self-heal) plus `bucketvcs doctor` mandated as a precondition for resuming write traffic after unclean shutdown; `fsyncDir` failure behavior specified (error propagated, operation reported as failed); List symlink mechanism pinned to `DirEntry.Type()` from `filepath.WalkDir`; size-mismatch detection surfaced into the stage 3 summary; README verbatim-copy obligations enumerated.
- 2026-05-03 r5: address roborev design review (job 7690) findings — replaced "must run doctor at M16" with M0-shipped `Localfs.Verify(ctx)` method and package-level `localfs.Verify(root)` function; lockfile (`<root>/.lock`) doubles as unclean-shutdown marker (its presence at `Open` time triggers `ErrAlreadyLocked` with the operator instructed to call `Verify` before retrying); read-path sequence rewritten as an explicit 4-step procedure; self-heal failure semantics specified (original read error wins, partial sidecar left for retry); size-mismatch coverage extended to all reads and conditional writes via the shared `headLocked` helper.
- 2026-05-03 r6: address roborev design review (job 7693) findings — fix the regression introduced in r5 where package-level `Verify(root)` could trample a live process holding the bucket open. Lockfile now stores PID + host + start time; `Verify` checks if the recorded PID is alive on the recorded host before proceeding (`kill(pid, 0)` on POSIX); refuses with `ErrLockedByLiveProcess` if alive unless `WithForce(true)` opt is passed. Package-level `Verify` signature changed to `Verify(ctx context.Context, root string, opts ...VerifyOption) error` for cancellation, force-override, and progress reporting. Reconciliation outcome matrix added covering missing/parse-broken/orphan sidecars, symlinks, dirs, unreadable files, ctx cancellation, partial failure. Same-size torn-sidecar test made an explicit exit criterion (the actual case Verify exists for).
- 2026-05-03 r7: address roborev design review (job 7696) findings — fix the contradictory `started_at` semantics introduced in r6. Lockfile field renamed to `acquired_at` and is forensics-only (not used for liveness logic). M0 liveness check is `kill(pid, 0)` on the same host; PID reuse is documented as a known limitation operator resolves via `WithForce`. Add unchanged-lock snapshot/recheck around `Verify`'s lockfile removal so a legitimate process Opening the bucket mid-Verify is not trampled. Specify progress callback as fire-and-forget (caller owns panic handling). Two new tests required: PID-reuse-via-WithForce and lock-changed-during-Verify.
- 2026-05-03 r8: address roborev design review (job 7698) findings — package-level `Verify` on absent lock now returns nil immediately without any reconciliation, eliminating the absent-lock-at-start race where a legitimate Open during reconciliation could see torn state. Periodic maintenance on a clean open bucket is what `Localfs.Verify(ctx)` (instance method) is for; package-level `Verify` is exclusively for unclean-shutdown recovery (i.e., when `Open` returned `ErrAlreadyLocked`). Unreadable-lockfile wording fixed: non-`ENOENT` read errors propagate (fail closed), `ENOENT` means absent (early return), malformed JSON means treat as stale and proceed (recheck still applies). New test `TestVerifyAbsentLockNoOp` exercises the absent-lock early-return.

## Purpose

M0 establishes the storage abstraction every later milestone depends on. It ships:

1. A provider-neutral `ObjectStore` Go interface that captures the spec's §9 contract in idiomatic Go.
2. A `localfs` adapter implementing that interface for a single-process dev/test bucket on a regular filesystem.
3. A reusable conformance test suite covering the 15 §29 correctness tests and the 3 stress tests applicable to localfs.

Once M0 ships, M1 (manifest CAS + transaction records) and every subsequent milestone can build on top of `ObjectStore` without depending on any specific provider.

## What this milestone is *not*

- Not a cloud adapter. AWS S3, GCS, R2, Azure Blob land at M5 (one of them) and M7 (the rest).
- Not the `bucketvcs conformance-test` CLI subcommand. The conformance package is shaped so the subcommand can wrap it trivially when M3 introduces the binary.
- Not the manifest model, transaction records, or any Git-aware code. Those live in M1 and M2.
- Not multi-process safe localfs. The `.lock` startup file is a courtesy. Two `bucketvcs serve` processes against the same root → undefined.
- Not Windows-supported. Targets Linux + macOS for M0.
- Not signed-URL emulation. `SignedGetURL` returns `ErrNotSupported` on localfs in M0.
- Not historical version retention. Each PUT overwrites previous content.
- Not metrics-instrumented. Structured logging only. Spec §32 metrics land at M3 with the metrics framework choice.

## Implementation stages summary

The full implementation plan (`docs/superpowers/plans/2026-05-03-m0-storage-foundation.md`) decomposes M0 into 34 bite-sized TDD tasks across these seven stages. Each task ends in a commit; each stage produces a reviewable increment.

1. **Bootstrap** (Tasks 1–9): Go module + `.gitignore` + `cmd/bucketvcs/main.go` skeleton; sentinel errors; `ObjectVersion`; option/result types; `MultipartUpload` interface; `ObjectStore` interface; localfs stub returning `ErrNotSupported`; conformance package scaffolding; localfs ↔ conformance wiring.
2. **Localfs internals** (Tasks 10–13): key validation; in-process keyed mutex; JSON sidecar with versioned schema; `Open`/`Close` + lock file.
3. **Core operations** (Tasks 14–18): `Put` + `Head` + `Get` with the atomic write pattern (per-key mutex on reads, `Lstat` symlink rejection, **size-mismatch fast-path triggering sidecar self-heal**, directory `fsync`); `GetRange`; `PutIfAbsent` concurrency tests; `PutIfVersionMatches` (with size-mismatch check before token compare); `DeleteIfVersionMatches` (content-first removal order; size-mismatch check before token compare).
4. **List + multipart** (Tasks 19–23): List + pagination + symlink skip; List delimiter + common prefixes; multipart happy path; multipart-vs-existing-key (§29 #8); `Capabilities` + `SignedGetURL` returning `ErrNotSupported`.
5. **§29 conformance fill-in** (Tasks 24–26): error classification + version round-trip + throttling-skip; key namespace floor; suite self-review checking that all 15 §29 items are covered.
6. **Stress tests** (Tasks 27–29): 100 concurrent CAS attempts; 10,000 small object creates; 256 MiB multipart conflict.
7. **Edge cases & polish** (Tasks 30–35): sidecar self-heal localfs unit tests (missing + size-mismatch across all read/conditional ops); `Localfs.Verify(ctx)` and `localfs.Verify(root)` implementation + tests; worked example; README; multipart lifecycle conformance (8 cases); localfs symlink-rejection unit test.

Each task follows the same TDD shape: Step 1 writes the failing test; Step 2 verifies it fails; Step 3 implements the minimum to pass; Step 4 verifies it passes; Step 5 commits. Conformance tests are added per behavior slice as that slice is implemented; cloud adapters at M5/M7 inherit the same suite.

## Architectural decisions (locked)

These were settled during brainstorming and are not re-litigated below.

| # | Decision | Rationale |
|---|----------|-----------|
| AD1 | Own the `ObjectStore` interface; do not wrap `gocloud.dev/blob` or any portable-blob library | The §9 contract is fundamentally about CAS primitives (`PutIfVersionMatches`, `DeleteIfVersionMatches`, `CompleteMultipartIfAbsent`). Portable blob libraries do not expose these uniformly. The §29 conformance suite tests precisely the semantics those libraries hide. |
| AD2 | Cloud adapters use provider SDKs directly (per spec §40.4) when they land later | SDKs handle auth, retries, signing, throttling backoff, multipart abstractions — undifferentiated work. M0 itself ships no cloud adapters. |
| AD3 | localfs concurrency: in-process keyed `sync.Mutex` map serializes BOTH read and write paths per key. Cross-key operations are independent. | Spec §35 frames localfs as "dev/test." All M0–M3 demos run in a single process. Acquiring the per-key mutex on reads (Get/Head/GetRange) prevents the read from observing a torn (content-N, sidecar-N-1) pair during a concurrent write. Read throughput is serialized per key but parallel across keys, which is acceptable for single-process dev/test. Simpler than `flock` (no NFS/Windows trouble); simpler than SQLite (preserves on-disk inspectability). Multi-process can be retrofitted later without changing the interface. |
| AD4 | `ObjectVersion` token on localfs = `sha256(content)` hex string | Deterministic, derivable from content, no separate state. Matches the "content-addressed" theme of the broader spec. Computed on write, cached in JSON sidecar. |
| AD5 | Multipart implemented as a real spec-conforming operation on localfs | The §29 multipart tests need real code paths to exercise. Stubbing leaves a hole that only fills at M5. Cost on localfs is small (~100 lines). |
| AD6 | `SignedGetURL` stays on the contract; localfs returns `ErrNotSupported`; capability flag declared | Real signed-URL emulation only matters when a milestone needs it (likely M11 packfile-uri). Adding a capability flag now lets the conformance suite skip cleanly with a documented reason. |
| AD7 | Conformance suite is a regular Go package (`internal/storage/conformance`) callable from any adapter's `_test.go` AND from a future CLI subcommand | One implementation, two callers. M0 ships only the `go test` entry point; the CLI hook lands at M3. |
| AD8 | §29 correctness test #14 ("network retry does not duplicate committed object") recast on localfs as: "PutIfAbsent twice with the same args returns ErrAlreadyExists cleanly without corrupting state on the second call." | Same invariant, exercised at the local level. Spec mapping table in this doc documents the recast. Cloud adapters at M5/M7 add a transient-retry-mid-call simulation as an additional case. |
| AD9 | M0 omits `PutOptions.Metadata` (user-defined K/V metadata). Only `ContentType` is supported. | The field added complexity (sidecar schema, `ObjectMetadata` exposure, conformance assertions) without a caller for it in M0. Cloud adapters at M5/M7 reintroduce it with explicit semantics keyed to provider-native metadata (S3 `x-amz-meta-*`, GCS object metadata, etc.). The interface stays additive: adding `Metadata` later does not break existing callers. |
| AD10 | Multipart `PartNumber` is 1-based (S3 convention). Reject part numbers < 1. | Cloud adapters all use 1-based numbering (S3, GCS resumable, Azure block IDs); having localfs match avoids a class of porting bugs. |
| AD11 | Localfs read paths perform **best-effort final-path symlink rejection** via `os.Lstat`. Ancestor-directory symlinks, hardlinks, and TOCTOU between `Lstat` and the subsequent `Open` are explicitly NOT detected. | The realistic dev-test footgun is `ln -s /etc/passwd <root>/objects/foo`; the final-path lstat catches it. Stronger sandboxing (every-component validation, `openat2(RESOLVE_BENEATH)` on Linux, `Nlink>1` checks for hardlinks) is out of scope for M0; the README and the "Symlink and hardlink safety" section document the limitations honestly. Cloud adapters at M5/M7 do not have these concerns because they do not see a host filesystem. |
| AD12 | The localfs lockfile `<root>/.lock` records `{pid, host, acquired_at}` JSON. `Open` writes the JSON via `O_CREAT\|O_EXCL`. `Verify` reads `pid` and `host`; if `host` matches the current host and `kill(pid, 0)` reports the process alive, the lock-holder is treated as live and `Verify` refuses with `ErrLockedByLiveProcess` unless `WithForce(true)` is passed. The `acquired_at` field is forensics-only (logging, debugging) and is NOT used for liveness logic. PID reuse is a documented limitation: a stale lockfile whose PID was later assigned to an unrelated live process will look "alive" and require `WithForce(true)` to bypass. | r5 introduced a regression: package-level `Verify(root)` bypassed the lockfile check unconditionally and could trample a live process. r6 added PID + start-time logic but introduced contradictory `started_at` semantics (lock-acquisition time vs process start time, with internally inconsistent comparison rules). r7 simplifies to the safe direction: `kill(pid, 0)` is enough to refuse against a live PID; `WithForce(true)` lets operators recover from PID-reuse false positives. False refusals are recoverable; false permits are not. Cross-host detection remains unchanged: a recorded host that differs from the current host means M0 cannot probe liveness and refuses without `WithForce`. Reading process start times portably (Linux `/proc/<pid>/stat`, macOS `sysctl(KERN_PROC)`) is OS-specific and deferred to a future revision if PID-reuse false positives become a real operational issue. |
| AD13 | Package-level `Verify` snapshots the lockfile bytes at the start of repair and re-reads them just before removing the lockfile. If the bytes have changed, `Verify` returns an error and does NOT remove the lockfile, preserving whatever new lock has appeared. | If a legitimate process Opens the bucket between `Verify`'s liveness check and its `os.Remove(<root>/.lock)`, removing the lock would let yet another process Open and break the single-writer invariant. The snapshot-then-recheck pattern catches the race: any change to the lockfile (new PID, new host, malformed contents, or absent altogether) aborts the cleanup. The pattern is not strictly atomic against a writer that races between recheck and `Remove`, but it shrinks the window from "entire reconciliation" to "two syscalls"; combined with operator workflow (Verify is invoked when the operator believes the bucket is unowned), the residual race is acceptable for a single-process dev/test adapter. |

## ObjectStore interface

```go
// Package storage defines the provider-neutral storage contract.
// Every adapter implements ObjectStore and must pass the conformance
// suite for the specific backend/configuration in use.
package storage

import (
    "context"
    "errors"
    "io"
    "time"
)

type ObjectStore interface {
    Capabilities() Capabilities

    Get(ctx context.Context, key string, opts *GetOptions) (*Object, error)
    Head(ctx context.Context, key string) (*ObjectMetadata, error)
    GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error)

    PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *PutOptions) (ObjectVersion, error)
    PutIfVersionMatches(ctx context.Context, key string, expected ObjectVersion, body io.Reader, opts *PutOptions) (ObjectVersion, error)
    DeleteIfVersionMatches(ctx context.Context, key string, expected ObjectVersion) error

    List(ctx context.Context, prefix string, opts *ListOptions) (*ListPage, error)

    CreateMultipart(ctx context.Context, key string, opts *MultipartOptions) (MultipartUpload, error)
    CompleteMultipartIfAbsent(ctx context.Context, upload MultipartUpload, parts []MultipartPart) (ObjectVersion, error)

    SignedGetURL(ctx context.Context, key string, opts SignedURLOptions) (string, error)
}

type ObjectVersion struct {
    Provider string
    Token    string
    Kind     VersionKind
}

type VersionKind int

const (
    VersionUnknown VersionKind = iota
    VersionEtag
    VersionGeneration
    VersionVersionID
    VersionOpaque
)

type Capabilities struct {
    SignedURLs           bool
    MultipartMinPartSize int64 // 0 means "no minimum"
    MultipartMaxParts    int   // 0 means "no enforced cap at adapter level"
    MaxObjectSize        int64 // 0 means "unbounded by adapter"
    StrongList           bool  // strong read-after-write for list operations
}

type Object struct {
    Body     io.ReadCloser
    Metadata ObjectMetadata
}

type ObjectMetadata struct {
    Key         string
    Version     ObjectVersion
    Size        int64
    ContentType string
    ModifiedAt  time.Time
}

type GetOptions struct {
    IfVersionMatches *ObjectVersion // optional
}

type PutOptions struct {
    ContentType string
}

type ListOptions struct {
    MaxKeys           int
    ContinuationToken string
    Delimiter         string
}

type ListPage struct {
    Objects        []ObjectMetadata
    NextToken      string
    CommonPrefixes []string
}

type MultipartOptions struct {
    ContentType string
}

type MultipartUpload interface {
    UploadID() string
    Key() string
    UploadPart(ctx context.Context, partNumber int, body io.Reader) (MultipartPart, error)
    Abort(ctx context.Context) error
}

type MultipartPart struct {
    PartNumber int
    Token      string // adapter-defined; localfs uses sha256 of part bytes
    Size       int64
}

type SignedURLOptions struct {
    Expires time.Duration
    Method  string // typically "GET"
}
```

### Error taxonomy

Sentinel errors callers compare against with `errors.Is`. Adapter implementations wrap their underlying errors with these to provide normalized classification.

```go
var (
    ErrNotFound        = errors.New("storage: object not found")
    ErrAlreadyExists   = errors.New("storage: object already exists") // PutIfAbsent / CompleteMultipartIfAbsent
    ErrVersionMismatch = errors.New("storage: version mismatch")      // PutIfVersionMatches / DeleteIfVersionMatches
    ErrThrottled       = errors.New("storage: throttled")             // retryable
    ErrTransient       = errors.New("storage: transient error")       // retryable
    ErrInvalidArgument = errors.New("storage: invalid argument")
    ErrAccessDenied    = errors.New("storage: access denied")
    ErrNotSupported    = errors.New("storage: not supported by adapter")
)
```

Localfs additionally exposes:

```go
// In the localfs package:

// ErrAlreadyLocked is returned by Open when <root>/.lock already
// exists. The cause may be that another process has the bucket open
// or that a previous process crashed without cleaning up. Callers
// resolve the ambiguity by invoking localfs.Verify (which performs
// the AD12 liveness check) or by manual inspection.
var ErrAlreadyLocked = errors.New("localfs: root is already locked by another instance")

// ErrLockedByLiveProcess is returned by package-level Verify when
// the lockfile points to a process that is alive on the current host
// (or to a process on a different host where M0 cannot probe
// liveness). Caller can pass WithForce(true) to override; doing so
// against a truly live holder will corrupt that holder's state.
var ErrLockedByLiveProcess = errors.New("localfs: lockfile holder is alive")
```

The conformance suite (§29 #13) verifies CAS conflicts surface as `ErrVersionMismatch` and `ErrAlreadyExists` correctly. §29 #15 verifies throttling classification — N/A for localfs but real for cloud adapters.

### Design notes on the interface

- **Streaming bodies (`io.Reader` / `io.ReadCloser`).** Pack files routinely exceed RAM. A `[]byte`-only API would force buffering. Adapters that prefer fixed-size APIs internally can `io.Copy` to a buffer themselves.
- **`context.Context` on every call.** Standard Go practice; lets callers cancel slow object-store operations.
- **`ObjectVersion` is opaque.** Spec §9 explicit: "core repository logic MUST NOT directly depend on S3 ETags, GCS generations, R2 ETags, Azure ETags, or provider-specific version IDs." Callers compare versions through interface methods only.
- **Multipart is a first-class part of the contract**, not optional. Cloud adapters at M5/M7 will need it for >5 GiB packs (S3) and resumable uploads. Localfs implementing it gives those adapters a tested oracle.
- **`SignedGetURL` is on every adapter** but localfs returns `ErrNotSupported` and declares `Capabilities{SignedURLs: false}`. Conformance suite skips that test path with documented reason.
- **No batch operations in M0.** Adapters can add batch puts/gets later if a milestone proves the round-trip cost matters; the §9 spec does not require batch.
- **`Capabilities()` is the only metadata channel for adapter-specific limits.** Avoids per-method capability checks scattered through caller code.

## localfs adapter

### On-disk layout

Under the bucket root directory:

```text
<root>/
  objects/
    <key>                  raw bytes (key path mirrors object key)
    <key>.meta             JSON sidecar: {version, size, content_type, mtime, sha256}
  uploads/
    <upload_id>/
      manifest.json        {key, opts, created_at}
      parts/
        00001
        00002
        ...
  .lock                    process-wide lockfile (advisory)
```

### Concurrency model

Per AD3, the keyed mutex serializes BOTH reads and writes per key. Concretely:

- Every method that touches an object on disk (`Get`, `Head`, `GetRange`, `PutIfAbsent`, `PutIfVersionMatches`, `DeleteIfVersionMatches`, `CompleteMultipartIfAbsent`) acquires `mutexes.lock(key)` for the whole operation.
- `List` does not lock at the prefix level; it iterates keys and calls the per-key `head()` helper for each, which acquires the per-key mutex. List therefore sees a consistent view of each individual key, but the page is not a strict snapshot across keys.
- Cross-key operations are independent: locking key `a` does not block work on key `b`.
- Multipart `UploadPart` does not lock the target key (parts go into `<root>/uploads/<id>/`, not `<root>/objects/<key>`). Only `CompleteMultipartIfAbsent` acquires the target's mutex.

This rule eliminates the (content-N, sidecar-N-1) torn-read window: a writer holds the mutex for the full content-rename + sidecar-rename sequence, so any concurrent reader either sees both pre-write or both post-write state.

The cost is that read throughput is serialized per key. For a single-process dev/test adapter that is acceptable; cloud adapters at M5/M7 do not have this constraint because their read paths use provider-side strong consistency, not local mutexes.

### Mechanics

- **Atomic write pattern** for any object body. The full sequence (per content file, then again per `.meta` sidecar) is:
  1. Write to `<root>/objects/<key>.tmp.<rand>`.
  2. `fsync` the temp file.
  3. `rename` to the final path.
  4. **`fsync` the parent directory.** POSIX `rename` is atomic in memory but the directory entry change is not durable until the directory itself is fsynced. Without this step, a crash between steps 3 and the next operation can lose the rename even though the file fsync succeeded. The implementation provides a `fsyncDir(dir string) error` helper used by both content and sidecar writes.

  **`fsyncDir` failure semantics**: if `fsyncDir` returns an error after a successful `rename`, the operation has performed the rename but the directory entry is not durably persisted. The operation MUST return that error (wrapped with the operation name); callers MUST treat it as a failure even though some on-disk state may have changed. The next successful write to the same key will durably persist the previous rename via its own `fsyncDir`. This is consistent with how filesystems and databases conventionally surface fsync errors.

  Because the per-key mutex is held for the whole sequence, no concurrent reader observes a torn pair. The directory fsync makes the rename durable across crashes; it is required for the crash-recovery semantics described below.
- **Stale sidecar detection on read.** Before trusting `sidecar.Sha256` as the version token, `Head` / `Get` / `GetRange` / `PutIfVersionMatches` / `DeleteIfVersionMatches` MUST compare `os.Stat(content).Size()` against `sidecar.Size`. If they differ, the sidecar is stale relative to content (the most likely cause is a crash mid-`PutIfVersionMatches` between the content rename and the sidecar rename). The implementation MUST trigger sidecar self-heal: recompute sha256 from content and rewrite a fresh sidecar at the current schema version. The size-mismatch path is cheap (one `Stat`); same-size collisions are NOT detected by this fast-path and are addressed by `bucketvcs doctor` (see "Crash recovery and bucketvcs doctor" below).
- **`PutIfAbsent`** acquires the per-key mutex, then explicitly `os.Lstat`s the target path. If the target exists (regular file or symlink), returns `ErrAlreadyExists` and writes nothing. If absent, runs the atomic write pattern. *Note: POSIX `rename(2)` overwrites existing targets silently, so the absence check must be performed under the same mutex held during rename. Defense-in-depth on Linux: callers may use `unix.Renameat2(..., RENAME_NOREPLACE)`; M0 relies on the Lstat-under-mutex check because localfs is single-process.*
- **`PutIfVersionMatches`** acquires the per-key mutex, reads the sidecar, applies the size-mismatch fast-path (self-heal if `Stat.Size != sidecar.Size`), then compares `expected.Token` against `sidecar.sha256`. If equal, runs the atomic write pattern; otherwise returns `ErrVersionMismatch`. Returns `ErrVersionMismatch` (not `ErrNotFound`) when the key does not exist, matching S3 If-Match semantics. See the "Version-conditional operation contract" table below for the full matrix.
- **`DeleteIfVersionMatches`** acquires the per-key mutex, reads and compares the version (with size-mismatch fast-path), then removes content first, then sidecar; each removal is followed by `fsyncDir` of the parent. Returns `ErrVersionMismatch` on skew, `ErrNotFound` if absent. **Order rationale**: removing content first means a crash mid-delete leaves "no content + orphan sidecar"; subsequent `Head` returns `ErrNotFound` (correct outcome). The reverse order would leave "content present + missing sidecar"; subsequent `Head` would self-heal the sidecar and the deleted object would resurrect.
- **`Get` / `Head` / `GetRange`** acquire the per-key mutex, then perform the following explicit sequence:
  1. `Lstat` the content path. Absent → `ErrNotFound`. Symlink → `ErrInvalidArgument` (AD11).
  2. Read the sidecar file. Missing or fails to parse → triggers self-heal (recompute sha256 from content, write a fresh sidecar at the current schema version).
  3. If the sidecar parsed cleanly, compare `Stat(content).Size()` against `sidecar.Size`. Mismatch → triggers self-heal (same recompute path as step 2). Match → proceed.
  4. Construct `ObjectMetadata` from the (possibly healed) sidecar. For `Get`/`GetRange`, additionally `os.Open` the content file and return it as `Body`. The open file descriptor remains valid after the keyed mutex is released because POSIX guarantees the inode stays reachable through an open fd even if a subsequent writer renames a new file over the path.

  **Self-heal failure semantics**: if step 2 or step 3's recompute/rewrite fails (e.g., I/O error, disk full, permission denied), the **original** error is wrapped and returned as the primary; the operation reports failure. Any partial sidecar state on disk is bounded by the temp+rename pattern in `writeFileAtomic` — the on-disk sidecar is either fully old or fully new, never half-written. A subsequent read attempt retries the heal.

- **`PutIfVersionMatches`** and **`DeleteIfVersionMatches`** also call `headLocked` at the start of their critical section, so they inherit the same Lstat → sidecar-read → size-mismatch → self-heal sequence. The size-mismatch fast-path therefore guards every read and every conditional write.
- **`List`** walks `<root>/objects/<prefix>` lexicographically using `filepath.WalkDir`. **Symlink detection mechanism**: `filepath.WalkDir` calls the visit function with a `fs.DirEntry` whose `Type()` returns `fs.ModeSymlink` for symlinks (POSIX guarantee — `WalkDir` does not follow symlinks). `List` skips entries where `d.Type()&fs.ModeSymlink != 0` and emits a structured warning. No additional `Lstat` is required for the type check. Pagination uses last-returned key as the continuation token; the next call returns keys strictly greater than that token. `Delimiter` support produces `CommonPrefixes` to match cloud-style "directory-like" listing semantics.
- **`CreateMultipart`** generates a UUID, creates `<root>/uploads/<id>/parts/`, writes `manifest.json` recording the target key, content type, and creation time. Does not lock the target key; multipart uploads do not reserve the key.
- **`UploadPart`** streams part bytes via temp + atomic rename to `parts/NNNNN` (zero-padded; `PartNumber` is 1-based per AD10). Returns the part's sha256 as its token. Out-of-order or repeated part numbers are allowed at upload time; repeated `PartNumber` overwrites the prior part's bytes via the same temp+rename. Part numbers < 1 return `ErrInvalidArgument`.
- **`CompleteMultipartIfAbsent`** validates the `parts` slice is non-empty and contiguously numbered (1, 2, 3, ...); acquires the target key's mutex; performs the same Lstat-then-write sequence as `PutIfAbsent`; concatenates parts in order while computing streaming sha256; atomically promotes the assembled content via the atomic write pattern (with directory fsync); removes the upload directory on success. If the target already exists, returns `ErrAlreadyExists` and leaves the upload directory intact (the caller may `Abort` or retry against a new key).
- **`Abort`** removes the upload directory. Idempotent: aborting an already-aborted or already-completed upload is a no-op (silently succeeds).
- **`SignedGetURL`** returns `ErrNotSupported`.
- **`.lock` startup check** opens `<root>/.lock` with `O_CREATE|O_EXCL`. On success, holds it open for process lifetime. On failure, returns `ErrAlreadyLocked`. Stale locks can be removed by `bucketvcs doctor` (M16) or by hand.

### Version-conditional operation contract

Behavior of the version-conditional operations across edge cases. The Put/Delete asymmetry on absent keys is intentional and matches S3 semantics: "Put with If-Match" on a missing target is a precondition failure, while "Delete on a missing target" is naturally not-found.

| Method | Absent key | Stale version | Current version | Malformed expected token |
|--------|------------|---------------|-----------------|--------------------------|
| `PutIfVersionMatches` | `ErrVersionMismatch` (matches S3 If-Match: 412; lets callers retry as `PutIfAbsent`) | `ErrVersionMismatch` | success | `ErrVersionMismatch` (token compares unequal) |
| `DeleteIfVersionMatches` | `ErrNotFound` (no live object to argue about) | `ErrVersionMismatch` | success (object removed) | `ErrVersionMismatch` |
| `Get` with `IfVersionMatches` | `ErrNotFound` | `ErrVersionMismatch` | success | `ErrVersionMismatch` |

The conformance suite asserts the absent-key column at §29 #11 (Delete) and via the `PutIfVersionMatches` recast in tasks 17 and 24.

### Crash recovery semantics

Localfs in M0 commits to crash recovery for the following states (assuming directory `fsync` is implemented per the atomic write pattern). Crash durability is best-effort: states marked "self-heals" require a subsequent successful operation to fully reconcile.

| Post-crash state | Cause | Recovery |
|------------------|-------|----------|
| Content committed, sidecar missing | Crash between content rename and sidecar rename in a fresh `PutIfAbsent` | `Head`/`Get` self-heal: recompute sha256 from content, write a fresh sidecar at the current schema version. |
| Content committed (new), sidecar present (old) | Crash between content rename and sidecar rename in `PutIfVersionMatches` | `Head`/`Get` return the OLD sidecar version against NEW content. Subsequent `PutIfVersionMatches` with the OLD expected token will succeed and rewrite both, healing the inconsistency. Readers in the window observe a stale version-token-vs-content pair. **This is a known M0 limitation; cloud adapters do not have the equivalent issue because their version tokens are server-side and atomic.** |
| Content missing, sidecar present | Crash between content removal and sidecar removal in `DeleteIfVersionMatches` (because Delete removes content first) | `Head`/`Get` return `ErrNotFound`. `bucketvcs doctor` (M16) flags the orphan sidecar for cleanup. |
| Abandoned temp files (`<key>.tmp.<rand>`) | Process killed during atomic write | Not visible to the public API (temp filenames begin with `.` and never satisfy `validateKey`). `bucketvcs doctor` cleans them up. |
| Abandoned upload dirs (`<root>/uploads/<id>/`) | Caller never calls `Complete` or `Abort` | Not visible to the public API. `bucketvcs doctor` cleans them up. |
| Completed object with failed upload-dir cleanup | RemoveAll fails after object commit | Object is committed (success); upload-dir leak is a gc concern only, not a correctness one. |

The "content (new) + sidecar (old)" window is the one M0 limitation that a cloud adapter would not exhibit. The conformance suite does not (and cannot) inject mid-write crashes into localfs without OS-level chaos tooling; `bucketvcs doctor` is responsible for detecting torn states by recomputing sha256 and comparing against sidecar.

### Crash recovery and `Localfs.Verify`

Localfs's stale-sidecar fast-path (size-mismatch detection on read) catches the common case where a crashed `PutIfVersionMatches` left content of a different size than the previous version. It does NOT catch the rarer case where the new and old content happen to share the same size. To preserve CAS correctness across crashes, M0 ships a verifier in the localfs package itself.

**Verifier API:**

```go
// Localfs.Verify walks every object under <root>/objects/, recomputes
// sha256 from content, and reconciles sidecars per the outcome matrix
// below. Caller already holds the bucket open; Verify acquires the
// per-key mutex per object so it can run alongside normal writes.
func (l *Localfs) Verify(ctx context.Context) error

// Verify is the package-level entry point used to recover from unclean
// shutdown. It checks the lockfile for liveness (AD12) before doing
// anything; if the recorded process is alive, returns
// ErrLockedByLiveProcess unless WithForce(true) was passed. On success
// it removes the lockfile so a subsequent Open succeeds.
func Verify(ctx context.Context, root string, opts ...VerifyOption) error

type VerifyOption func(*verifyConfig)

// WithForce overrides the live-lock check. Destructive: only use when
// the operator has independently confirmed no other process is using
// the bucket.
func WithForce(force bool) VerifyOption

// WithProgress installs a callback that is invoked as objects are
// processed. Called in the same goroutine as Verify. The callback is
// fire-and-forget: panics propagate out of Verify (caller responsibility
// to recover); a callback that blocks blocks Verify; the callback has
// no return value and cannot signal cancellation (use ctx for that).
func WithProgress(cb func(processed int)) VerifyOption
```

**Lockfile content (AD12):**

```json
{
  "pid": 12345,
  "host": "hostname.local",
  "acquired_at": "2026-05-03T20:00:00Z"
}
```

`Open` writes this content via `O_CREAT|O_EXCL`. `Close` removes it. `acquired_at` is recorded for forensics (logs, debugging) and is NOT consulted by the liveness check.

**Lock-liveness rule used by package-level `Verify` (AD12 + AD13):**

```text
1. Snapshot <root>/.lock bytes at the start of Verify.
   - ENOENT (absent) → return nil immediately. Package-level Verify
     does NOT reconcile clean buckets; periodic maintenance on a
     healthy open bucket is the job of Localfs.Verify(ctx) (instance
     method), which acquires the per-key mutex per object and is
     safe to run alongside writes. Treating a clean bucket as
     "nothing to do" eliminates the race where a legitimate Open
     during reconciliation could observe torn sidecars.
   - Any other read error (permission denied, transient I/O) → fail
     closed. Return the error wrapped; do NOT proceed. Operators can
     fix the underlying I/O problem and retry, or pass WithForce(true)
     after manual inspection.
   - Read succeeded → preLockBytes = bytes; proceed to step 2.
2. Parse the JSON.
   - Malformed → treat as stale; proceed to reconcile (the AD13
     recheck still applies because preLockBytes are non-nil).
   - Parsed → proceed to step 3.
3. If recorded host != current host:
     - Without WithForce: refuse with ErrLockedByLiveProcess
       (M0 cannot probe liveness on a remote host).
     - With WithForce: proceed.
4. If recorded host == current host:
     a. Probe POSIX kill(pid, 0).
     b. ESRCH (no such process) → lock-holder is dead; proceed.
     c. Any other "alive" indication (success, EPERM): lock-holder is
        treated as live. Without WithForce: refuse with
        ErrLockedByLiveProcess. With WithForce: proceed (operator owns
        the safety call).
5. After reconciliation completes, re-read <root>/.lock (postLockBytes).
   - If absent: another process already removed it; do nothing.
   - If equal to preLockBytes: remove the lockfile so subsequent Open
     succeeds.
   - If different from preLockBytes: another process has acquired the
     lock during repair. Return ErrLockedByLiveProcess and do NOT
     remove the lockfile, preserving the new owner's lock. The
     reconciliation work already performed is harmless: stale sidecars
     are now correct relative to content, which is the same invariant
     normal writers maintain.
6. Note on PID reuse: M0 does NOT probe process start time to defeat
   PID reuse. A stale lockfile whose PID has been reassigned to an
   unrelated live process appears live and requires WithForce(true)
   to bypass. False refusals are recoverable; false permits are not.
   This is documented as a known limitation; future revisions may add
   OS-specific start-time validation if PID-reuse false positives
   become a real operational concern.
```

**Reconciliation outcome matrix:**

| Encountered state | Verify action | Notes |
|-------------------|---------------|-------|
| Content + matching sidecar (sha256 + size both equal) | No-op | Healthy entry. |
| Content + missing sidecar | Recompute, write fresh sidecar | Self-heal. |
| Content + parse-broken sidecar | Recompute, overwrite sidecar | Self-heal. |
| Content + size-mismatched sidecar | Recompute, overwrite sidecar | Stale-sidecar fast-path's territory; Verify also covers it for completeness. |
| Content + sha-mismatched sidecar (same size, different bytes) | Recompute, overwrite sidecar | **The case Verify exists for.** Read-path size-mismatch fast-path cannot catch this; Verify must. |
| Sidecar without content (`<key>.meta` exists, `<key>` missing) | Remove the orphan sidecar | Cleanup. Possible if a Delete crashed between content removal and sidecar removal. |
| Symlink under `objects/` | Skip with structured warning | AD11. Verify does not chase symlinks. |
| Subdirectories | Recurse normally | Standard `WalkDir` behavior. |
| Unreadable file (permission denied, I/O error) | Return wrapped error; partial reconciliation valid | Subsequent Verify can retry; previously reconciled entries stay reconciled. |
| `ctx.Done()` | Return `ctx.Err()` | Partial reconciliation valid; idempotent. |

**Concurrency rule:**

- `Localfs.Verify(ctx)` (instance method): acquires the per-key mutex per object during reconciliation. Safe to call alongside normal writes; throughput is reduced but correctness is preserved.
- `localfs.Verify(ctx, root, opts...)` (package-level): runs only after the live-lock check has confirmed no live process holds the bucket (or the operator has passed `WithForce`). It does not need per-key mutexes because no other process should be writing.

**Operator workflow on unclean shutdown:**

```text
1. Process running localfs is killed (kill -9, OOM, host crash).
2. New process calls localfs.Open(root).
   → returns ErrAlreadyLocked (the dead process's .lock is still there).
3. Operator confirms the prior process is gone (ps, systemctl, etc.).
4. Operator calls localfs.Verify(ctx, root).
   → Verify reads the lockfile, runs the AD12 liveness check.
   → If the recorded PID is dead or its start time predates the
     recorded started_at: Verify reconciles every object and clears
     the lockfile.
   → If the recorded PID is alive (e.g., operator was wrong about
     the crash): Verify returns ErrLockedByLiveProcess and does
     nothing destructive.
5. New process calls localfs.Open(root) → succeeds.
```

**Operator warning**: passing `WithForce(true)` against a bucket whose lockfile points to a live process WILL corrupt that process's view of the bucket. The flag exists for cases where M0's heuristics cannot determine liveness (e.g., cross-host buckets, containers with namespaced PIDs); operators are responsible for the safety judgment.

**Future M16 integration**: `bucketvcs doctor` will wrap `localfs.Verify` (and equivalents for cloud adapters) as a CLI subcommand, but the underlying primitive ships in M0. The "doctor" name is reserved; M0 callers should use `localfs.Verify` directly.

This requirement applies to localfs only. Cloud adapters at M5/M7 do not have an equivalent torn-state because their version tokens are server-side.

### Multipart lifecycle reference

Codifies edge-case behavior so cloud adapters at M5/M7 inherit a consistent contract:

| Scenario | Behavior |
|----------|----------|
| `UploadPart` with `partNumber < 1` | `ErrInvalidArgument` |
| `UploadPart` with non-contiguous numbers (e.g., 1 then 3) | Allowed at upload time; `Complete` validates contiguity |
| `UploadPart` repeats a `partNumber` (e.g., 2 uploaded twice) | Second upload overwrites the first via temp+rename. The token returned by the second call is the token used at `Complete` time. |
| `Complete` with empty `parts` slice | `ErrInvalidArgument` |
| `Complete` with non-contiguous part numbers in `parts` | `ErrInvalidArgument` |
| `Complete` after target key already exists | `ErrAlreadyExists`. Upload directory is preserved so the caller can `Abort` explicitly. |
| `Complete` referencing a part that wasn't uploaded | I/O error on opening the missing part file, surfaced wrapped (no special sentinel). |
| `Complete` part-size mismatch (manifest size ≠ on-disk part size) | `ErrInvalidArgument`. |
| Two concurrent `Complete` calls on the same upload, same target | Per-key mutex on target serializes them; one wins, the other sees `ErrAlreadyExists`. |
| Two concurrent `Complete` calls on different uploads, same target | Same as above: per-key mutex serializes; one wins, others see `ErrAlreadyExists`. |
| `Abort` after `Complete` | No-op (silently succeeds — the upload directory is already gone). |
| `Complete` after `Abort` | Underlying part files are gone; opening fails with a wrapped I/O error. The contract surface is "you may not call Complete after Abort"; an explicit pre-check is not required. |
| Abandoned uploads (caller never calls `Complete` or `Abort`) | Cleanup is the responsibility of `bucketvcs doctor` (M16). M0 does no automatic cleanup. |
| `Complete` from a `MultipartUpload` returned by a different `Localfs` instance | `ErrInvalidArgument`. |

### Symlink and hardlink safety (AD11)

Localfs in M0 implements **best-effort final-path symlink rejection**, not full path-resolution sandboxing. This section documents what is covered and what is not, so implementers and operators can reason about the residual risk.

**What is covered:**

- All read entry points (`Get`, `Head`, `GetRange`, `List`) call `os.Lstat` on the **final** path component before opening. If that entry is a symlink (`Mode().Type() == fs.ModeSymlink`), the operation returns `ErrInvalidArgument` and `List` skips the entry with a structured warning.
- Write paths create files via `os.OpenFile` with `O_CREATE|O_EXCL` on a temp path, then `os.Rename` into place. `os.MkdirAll` does not follow existing leaf symlinks.

**What is NOT covered in M0 (documented limitations):**

- **Ancestor-directory symlinks.** If `<root>/objects/foo/` is itself a symlink to a directory outside the bucket, files under `<root>/objects/foo/bar` actually live outside the bucket. M0 does not validate every path component and does not call `Lstat` on intermediate directories. Mitigation: the operator is responsible for ensuring `<root>/objects/` and its descendants are normal directories.
- **Hardlinks.** Hardlinks within or outside the bucket cannot be detected by inspection of the dirent type alone (`Lstat` reports them as regular files). M0 does not perform `Nlink>1` checks. An attacker with write access to both the bucket and a target file could hardlink them; subsequent `Get`/`Head` would expose the linked content as if it were a bucket object. Mitigation: localfs is single-process and the bucket root is assumed to be operator-trusted.
- **TOCTOU between `Lstat` and `Open`.** An attacker who can write to the bucket can race symlink replacement against subsequent `Open` calls. M0's check is not atomic.

Closing these gaps would require Linux-specific `openat2(RESOLVE_BENEATH)` (no portable equivalent on macOS) or every-component validation (substantial complexity for a dev/test adapter). Both are deferred. The realistic threat in M0 is a casual misconfiguration — `ln -s /etc/passwd <root>/objects/foo` — which the best-effort check catches. The README documents the gaps in the same words as this section.

### Error normalization

Error mapping for the non-obvious cases:

- **Sidecar parse error** (corrupted JSON, unknown schema version): self-heal — recompute sha256 from content and rewrite a fresh sidecar at the current schema version. If heal also fails, return the original error wrapped with the operation name.
- **`fsync` failure** during atomic write: surface the `*os.PathError` wrapped with the operation name. Caller decides retry policy.
- **Permission errors** (`os.IsPermission`): wrap with `ErrAccessDenied`.
- **Disk-full** (`ENOSPC`): surface the underlying error; no normalized sentinel in M0 (callers can `errors.Is(err, syscall.ENOSPC)` if needed).
- **Partial writes**: cannot occur at the public-API level because writes go through temp+rename. A torn temp file left behind by a crash is not visible to readers; `bucketvcs doctor` cleans it up.
- **Cleanup failures** (e.g., `os.Remove` of a temp file): logged structured, do not fail the operation if the primary commit succeeded.

### Filesystem portability assumptions

Localfs in M0 assumes:

- **Case-sensitive POSIX filesystem.** ext4, XFS, btrfs (Linux); APFS configured case-sensitive (macOS — note default APFS is case-INSENSITIVE; users on default-APFS macOS hosts will see CONTENT collisions if they rely on case-distinct keys). Unsupported: HFS+ (Unicode normalization folds NFC/NFD).
- **Atomic same-filesystem rename.** Standard POSIX. Crossing filesystems via rename is an unspecified error.
- **`fsync` flushes both data and metadata.** ext4 default behavior. `noatime` is fine; `data=writeback` is not recommended.
- **Standard file permissions and ownership.** No special handling for setuid, sticky, or extended attributes.

Unsupported (will refuse or behave undefined):

- **Network filesystems (NFS, SMB, FUSE).** `flock`/lock-file behavior across NFS is unreliable; rename atomicity is not guaranteed on all FUSE backends. The M0 startup `.lock` does not detect cross-host conflict.
- **Windows filesystems.** Path separators, case folding, and `O_CREATE|O_EXCL` semantics differ enough that M0 does not target Windows.

The README documents these restrictions verbatim.

### Capabilities reported

```go
storage.Capabilities{
    SignedURLs:           false,
    MultipartMinPartSize: 0,
    MultipartMaxParts:    0,
    MaxObjectSize:        0,
    StrongList:           true,
}
```

### Key validation

Localfs treats the object key as a forward-slash-separated path. Validation rejects:

- Empty keys
- Keys containing `..` segments
- Keys with leading `/` or trailing `/`
- Keys containing null bytes
- Keys with backslashes (defensive against Windows-style separators)
- Keys exceeding 1024 bytes UTF-8 (cloud-adapter floor; conservative for localfs)

Invalid keys return `ErrInvalidArgument` from any operation.

## Conformance suite

### Layout and API

```text
internal/storage/conformance/
  suite.go        public Run(t, factory)
  correctness.go  the 15 §29 correctness tests
  stress.go       3 §29 stress tests applicable to M0
  fixtures.go     deterministic byte/key generators
  testenv.go      adapter factory glue, cleanup, parallel-safety helpers
```

```go
package conformance

type Factory func(t testing.TB) (storage.ObjectStore, func())

func Run(t *testing.T, f Factory) {
    t.Run("correctness", func(t *testing.T) { runCorrectness(t, f) })
    t.Run("stress", func(t *testing.T) {
        if testing.Short() {
            t.Skip("stress tests skipped in -short mode")
        }
        runStress(t, f)
    })
}
```

Adapters wire it up:

```go
// internal/storage/localfs/localfs_conformance_test.go
func TestConformance(t *testing.T) {
    conformance.Run(t, func(t testing.TB) (storage.ObjectStore, func()) {
        dir := t.TempDir()
        s, err := localfs.Open(dir)
        if err != nil { t.Fatal(err) }
        return s, func() { _ = s.Close() }
    })
}
```

### §29 correctness test mapping

| §29 # | Spec wording | Localfs realization | Notes |
|-------|--------------|---------------------|-------|
| 1 | Concurrent `putIfAbsent` same key → exactly one succeeds | Spawn N=64 goroutines racing on the same key; assert exactly one returns success, others get `ErrAlreadyExists` | |
| 2 | Concurrent `putIfVersionMatches` same key → exactly one succeeds | Pre-create key, spawn N=64 goroutines all expecting the same version; one wins, rest get `ErrVersionMismatch` | |
| 3 | Failed conditional write does not alter object | After conflict, re-read content + version; assert byte-identical | |
| 4 | Read after write sees latest object | Sequential RAW with version assertion | |
| 5 | Read after overwrite sees latest object | Sequential ROW with version mutation | |
| 6 | List after write sees new object | Put then list with prefix; expect entry | |
| 7 | List after delete does not show deleted object | Delete then list; expect absent | |
| 8 | Multipart complete cannot silently overwrite existing object | Pre-create key, start multipart targeting same key, complete; assert `ErrAlreadyExists` and original content unchanged | |
| 9 | Range read returns exact bytes | Generate deterministic 1 MiB blob; range-read various windows including off-end (returns truncated to EOF, mirroring HTTP semantics); assert byte-identical | |
| 10 | Signed URL can read but cannot write | Localfs returns `ErrNotSupported`; test verifies that classification and that `Capabilities{SignedURLs: false}` is reported | Skipped if `Capabilities{SignedURLs: false}`; cloud adapters exercise the full assertion |
| 11 | `DeleteIfVersionMatches` fails if object changed | Get version, mutate object, attempt delete with old version; expect `ErrVersionMismatch`; assert object still present | |
| 12 | Metadata/version token round trips | `Put` returns `v1`; `Head` returns `v2`; assert `v1 == v2` and the `ObjectMetadata` matches | |
| 13 | CAS conflict error maps to normalized conflict type | Provoke conflict; assert `errors.Is(err, ErrVersionMismatch)` (or `ErrAlreadyExists` for `PutIfAbsent`) | |
| 14 | Network retry does not duplicate committed object | **Recast on localfs (AD8):** call `PutIfAbsent` twice with the same args — first succeeds, second returns `ErrAlreadyExists`, content + version unchanged. Cloud adapters at M5/M7 add a transient-retry-mid-call case in addition. | Recast documented here, traceable to original-spec.md |
| 15 | Provider throttling errors classified correctly | Localfs has no throttling; `t.Skip` with documented reason. Cloud adapters exercise this with provider-specific fault injection. | Skipped on localfs |

### §29 stress test inclusion

| §29 stress | M0 inclusion | Notes |
|------------|--------------|-------|
| 100 concurrent manifest CAS attempts | Yes | Exercises keyed-mutex correctness and CAS retry semantics |
| 10,000 small object creates | Yes | Exercises listing pagination and keyed-mutex map growth |
| Large multipart pack upload conflict | Yes | ~256 MiB synthetic pack via multipart against pre-existing key; verifies §29 #8 at scale |
| Delete/read/list race during GC simulation | Defer to M8 | GC simulation belongs with the GC milestone |
| Regional gateway read-after-write from distant region | N/A | Localfs has no regions |

### Fuzz/security floor

Beyond the §29 list, the suite includes a small "key namespace" test set that asserts:

- Keys with `..` are rejected
- Keys with leading or trailing `/` are rejected
- Keys with null bytes are rejected
- Keys with backslashes are rejected
- Keys ≤ 1024 bytes UTF-8 succeed
- Keys > 1024 bytes are rejected
- Keys ending in `.meta` are rejected (localfs reserves this suffix for sidecars)

These are not §29-derived but are a baseline safety floor for any adapter. Heavier fuzzing belongs in a security-review pass after M5.

### Multipart lifecycle conformance tests

In addition to §29 #8 (multipart cannot overwrite existing key), the conformance suite includes the following tests that codify the behavior described in the "Multipart lifecycle reference" table above:

- `MultipartHappyPath` — create, upload N parts, complete, get; assert content equals concatenation.
- `MultipartInvalidPartNumber` — `UploadPart` with `partNumber < 1` returns `ErrInvalidArgument`.
- `MultipartRepeatedPartNumber` — uploading the same `partNumber` twice succeeds; `Complete` uses the second upload's bytes.
- `MultipartCompleteEmptyParts` — `Complete` with empty `parts` returns `ErrInvalidArgument`.
- `MultipartCompleteNonContiguous` — `Complete` with `parts` numbered `[1, 3]` returns `ErrInvalidArgument`.
- `MultipartCompleteSizeMismatch` — `Complete` with `parts[i].Size` differing from on-disk part size returns `ErrInvalidArgument`.
- `MultipartConcurrentComplete` — two concurrent `Complete` calls on the same upload+target serialize via the per-key mutex; one wins, the other sees `ErrAlreadyExists`.
- `MultipartAbortIdempotent` — `Abort` after `Complete` is a no-op; `Abort` called twice in a row is a no-op.
- `MultipartCompleteAfterAbort` — `Complete` after `Abort` returns a wrapped I/O error from the missing part.
- `MultipartCrossInstance` — `Complete` with a `MultipartUpload` from a different `Localfs` instance returns `ErrInvalidArgument`.

### Symlink rejection conformance test

A `SymlinkRejection` test asserts:

- After `Put`-ing a key, manually replacing the on-disk content file with a symlink to `/etc/passwd` causes `Get`, `Head`, and `GetRange` to return `ErrInvalidArgument`.
- `List` skips the symlinked entry.

This test is localfs-only by nature (cloud adapters have no concept of symlinks) and lives in `internal/storage/localfs/localfs_test.go`, not in the conformance package.

### Expected divergence list

The suite supports an "expected divergence list" per adapter. Empty for localfs. Cloud adapters that cannot pass certain tests due to provider limitations document divergences here, paralleling the differential-harness divergence list from §40.3. Unknown divergences fail the test; known divergences are tracked and reviewed.

## Package layout

```text
github.com/<org>/bucketvcs/                      (org TBD pending G1–G3)
├── go.mod
├── cmd/
│   └── bucketvcs/                               (skeleton only; real CLI lands at M3)
│       └── main.go
├── internal/
│   └── storage/
│       ├── objectstore.go                       ObjectStore interface, Capabilities, types
│       ├── version.go                           ObjectVersion + VersionKind
│       ├── errors.go                            sentinel errors + classification helpers
│       ├── options.go                           GetOptions, PutOptions, ListOptions, etc.
│       ├── multipart.go                         MultipartUpload interface, MultipartPart
│       ├── README.md                            interface contract, how to add an adapter, how to run conformance
│       ├── conformance/
│       │   ├── suite.go                         public Run(t, factory)
│       │   ├── correctness.go                   15 tests from §29
│       │   ├── stress.go                        3 stress tests applicable to M0
│       │   ├── fixtures.go                      deterministic byte/key generators
│       │   └── testenv.go                       factory glue, parallel safety
│       └── localfs/
│           ├── localfs.go                       Open, Close, ObjectStore impl
│           ├── keyed_mutex.go                   map[string]*sync.Mutex with RWMutex over the map
│           ├── meta.go                          JSON sidecar read/write, self-heal
│           ├── multipart.go                     upload dir layout, part assembly
│           ├── keys.go                          key validation
│           └── localfs_conformance_test.go
└── docs/
    └── superpowers/
        └── specs/
            ├── 2026-05-03-bucketvcs-oss-decomposition-design.md
            └── 2026-05-03-m0-storage-foundation-design.md
```

Notes:
- `internal/storage/conformance` is a regular package, not a `_test` package. Importable from any adapter's tests today and from a future `bucketvcs conformance-test` subcommand at M3.
- No `pkg/` directory in M0. Re-evaluate at M7 once all four cloud adapters have shipped and the contract is battle-tested.
- `cmd/bucketvcs/main.go` is a skeleton so `go build ./...` works and so M3 has a hook to land the HTTP gateway.

## Exit criteria

1. `go test ./internal/storage/...` is green on Linux and macOS.
2. The 15 §29 correctness tests (with the AD8 recast for #14) pass against localfs.
3. The 3 applicable §29 stress tests pass against localfs.
4. The multipart lifecycle conformance tests (10 cases listed above) pass against localfs.
5. The localfs-only `SymlinkRejection` test passes.
6. `Capabilities()` declarations round-trip through the interface correctly.
7. Documented error taxonomy maps to the conformance assertions per the table above; the error-normalization rules in this spec match implementation behavior.
8. Package layout matches §40.1.
9. `internal/storage/README.md` documents: the interface contract; how to add a new adapter; how to run the conformance suite against an arbitrary adapter; the AD8 recast; the AD11 best-effort symlink claim **including the verbatim "What is NOT covered in M0" list from the "Symlink and hardlink safety" section**; the **verbatim "Filesystem portability assumptions" section**; the **verbatim "Crash recovery and `Localfs.Verify`" section including the operator workflow**; and any expected divergences. The README is operator-facing; copying these blocks verbatim ensures the warnings are not diluted as the spec evolves.
10. `Localfs.Verify(ctx)` (instance method) and `localfs.Verify(ctx, root, opts...)` (package-level function) ship in M0 with the following test coverage:
    - **Clean bucket** → instance-method `Verify(ctx)` is a no-op success.
    - **Absent lockfile (no recovery needed)** → package-level `Verify(ctx, root)` returns nil immediately without mutating any sidecar (no reconciliation on clean buckets per r8).
    - **Same-size torn sidecar** → reconciled successfully (the case Verify exists for; size-mismatch fast-path on read cannot catch this).
    - **Different-size torn sidecar** → reconciled successfully (also the size-mismatch fast-path's territory).
    - **Missing sidecar** → reconciled.
    - **Parse-broken sidecar** → reconciled.
    - **Orphan sidecar** (no content) → removed.
    - **Symlink under `objects/`** → skipped with structured warning.
    - **Live-process lockfile** → `Verify(ctx, root)` without `WithForce` returns `ErrLockedByLiveProcess`; with `WithForce` it proceeds.
    - **Cross-host lockfile** → `Verify` without `WithForce` returns `ErrLockedByLiveProcess` (M0 cannot probe a remote host).
    - **PID reuse: simulated stale lockfile whose PID has been reassigned to an unrelated live process** → `Verify` without `WithForce` returns `ErrLockedByLiveProcess` (correct, conservative); with `WithForce` it proceeds (AD12).
    - **Lock changed during Verify** (lockfile bytes differ between snapshot and recheck) → `Verify` returns `ErrLockedByLiveProcess` and leaves the new lockfile intact (AD13). Test mutates the lockfile out of band between Verify's snapshot read and reconciliation completion using a progress callback hook.
    - **Context cancellation** → `Verify` returns `ctx.Err()`; previously reconciled entries stay reconciled (idempotent retry).
    - **Lockfile cleared on success** → after reconciliation against a stale lock, `<root>/.lock` no longer exists; subsequent `Open` succeeds.
11. Public Go-doc comments on every exported symbol in `internal/storage`.
12. A worked example (~50 lines, in `internal/storage/example_test.go` or under `examples/`) exercises Put, Get, PutIfVersionMatches, List, Multipart, Delete on localfs, including the conflict paths.

## Dependencies

- **Unblocks** M1 directly; M2–M8 transitively.
- **Depends on** governance gate G1 (license) only for the first public push. Engineering can proceed in private until G1–G3 are settled.

## Risks and mitigations

| Risk | Mitigation |
|------|------------|
| Interface lock-in: M1+ depends on `ObjectStore`, changes are expensive | Treat M0–M5 as a continuous reality check. Review the interface after the first cloud adapter (M5) lands and after all four cloud adapters land (M7). Treat any contract change before M7 as expected, not a regression. |
| Localfs being too forgiving: accidentally accepts patterns S3 rejects | Conformance suite includes a "key namespace" test set keyed to the most-restrictive provider's rules (S3 in practice). Localfs adheres. |
| Sidecar drift: `.meta` deleted or corrupted out of band | Self-heal on read by recomputing sha256 from content; surface a structured warning via the logger. |
| Time-to-first-cloud: holding M1 hostage to a "perfect" interface | Ship M0 with a pragmatic interface; revise based on what M5 cloud-adapter work surfaces. |
| `O_EXCL` over networked filesystems is unreliable | Out of scope — localfs is for local filesystems only. NFS-backed roots are unsupported and the README documents this. |
| Mutex map growth on long-running localfs serving millions of distinct keys | M0 does not evict idle entries. Acceptable because localfs is dev/test and per-process. M9 (background maintenance) revisits if a real workload surfaces memory pressure; success criterion is "no localfs deployment to date has needed eviction." |
| HFS+ / case-insensitive APFS: keys differing only in case collide silently | README documents the case-sensitive POSIX assumption. macOS users on default-APFS hosts who need bucketvcs functionality at scale should use a case-sensitive volume or wait for cloud adapters at M5. |
| Symlink under bucket root exposes files outside the bucket | AD11: read paths `Lstat` and reject symlinks; `List` skips symlinks with structured warning. Full sandboxing (`openat2(RESOLVE_BENEATH)`) deferred to a later hardening pass. |

## Open questions deferred to implementation

These are implementation-plan decisions, not design-spec decisions:

- Exact JSON sidecar field names (the schema-versioning policy is fixed: every sidecar carries an integer `version` field; readers reject unknown versions; localfs ships with `version=1`).
- Whether to integrate with `golang.org/x/exp/slog` or wait for M3's logging framework decision.
- Specific UUID library choice for upload IDs.
- Whether `Capabilities` is fetched once per adapter instance or per call (cached vs not).
- Concurrency limits inside the keyed-mutex map (when to evict idle entries — see "Mutex map growth" risk below).
- Test parallelism caps (`t.Parallel()` policy across the conformance suite).

## Out-of-scope deferrals (deferred from this milestone, summarized)

- All cloud adapters (M5, M7)
- Real signed-URL implementation (whichever milestone first needs it; likely M11)
- Throttling and retry policy (cloud adapters)
- Encryption at rest, customer-managed keys (cloud adapters / enterprise scope)
- Observability metrics (M3+)
- `bucketvcs conformance-test` CLI subcommand (M3)
- Repo data model on top of storage (M1)
- Historical version retention on localfs
- Multi-process safety on localfs
- Windows support
- Batch put/get operations
