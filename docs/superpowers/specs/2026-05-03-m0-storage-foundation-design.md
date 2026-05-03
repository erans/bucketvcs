# M0 — Storage Foundation: ObjectStore Contract, localfs Adapter, Conformance Suite

Date: 2026-05-03
Status: design draft (revision 2)
Milestone: M0 (first critical-path milestone of bucketvcs OSS-core)
Source spec sections: §9, §10, §29 (subset), §35, §40.1, §40.4
Decomposition: see `2026-05-03-bucketvcs-oss-decomposition-design.md`
Implementation plan: see `docs/superpowers/plans/2026-05-03-m0-storage-foundation.md` (32 bite-sized TDD tasks)

Revision history:
- 2026-05-03 r1: initial design.
- 2026-05-03 r2: address roborev design review (job 7684) findings — read-side mutex acquisition for atomicity; `PutIfAbsent` Stat-then-rename made explicit; `PutOptions.Metadata` removed for M0; multipart lifecycle table added; symlink rejection added; filesystem portability assumptions documented; sidecar schema versioning surfaced from plan; part numbering pinned to 1-based.

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
| AD11 | Localfs read paths reject symlinks under the bucket root via `os.Lstat` checks at `Get`/`Head`/`GetRange`/`List`. | Defense against the dev-test footgun of an operator dropping a symlink into `<root>/objects/` and exposing files outside the bucket. Full path-resolution sandboxing (`renameat2(RESOLVE_BENEATH)` on Linux) is out of scope for M0; symlink rejection covers the realistic risk. |

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

- **Atomic write pattern** for any object body: write to `<root>/objects/<key>.tmp.<rand>`, fsync the file, rename to `<root>/objects/<key>`, write `<root>/objects/<key>.meta.tmp.<rand>`, fsync, rename. POSIX rename within the same filesystem is atomic. Because the per-key mutex is held for the whole sequence, no concurrent reader observes a torn pair.
- **`PutIfAbsent`** acquires the per-key mutex, then explicitly `os.Stat`s the target path. If the target exists, returns `ErrAlreadyExists` and writes nothing. If absent, runs the atomic write pattern. *Note: POSIX `rename(2)` overwrites existing targets silently, so the absence check must be performed under the same mutex held during rename. Defense-in-depth on Linux: callers may use `unix.Renameat2(..., RENAME_NOREPLACE)`; M0 relies on the Stat-under-mutex check because localfs is single-process.*
- **`PutIfVersionMatches`** acquires the per-key mutex, reads the sidecar, compares `expected.Token` against `sidecar.sha256`. If equal, runs the atomic write pattern; otherwise returns `ErrVersionMismatch`. Returns `ErrVersionMismatch` (not `ErrNotFound`) when the key does not exist, matching S3 If-Match semantics.
- **`DeleteIfVersionMatches`** mirrors `PutIfVersionMatches`: acquire mutex, read and compare version, then `os.Remove` content + sidecar. Returns `ErrVersionMismatch` on skew, `ErrNotFound` if absent.
- **`Get` / `Head` / `GetRange`** acquire the per-key mutex, perform an `os.Lstat` on the content path (rejects symlinks under AD11), read sidecar (or self-heal), then read content. Sidecar self-heal recomputes sha256 from content and rewrites a fresh sidecar; if heal fails (e.g., I/O error), the original error is wrapped and returned.
- **`List`** walks `<root>/objects/<prefix>` lexicographically using `filepath.WalkDir`. Pagination uses last-returned key as the continuation token; the next call returns keys strictly greater than that token. `Delimiter` support produces `CommonPrefixes` to match cloud-style "directory-like" listing semantics. List skips entries whose `Lstat` reports a symlink, with a structured warning log.
- **`CreateMultipart`** generates a UUID, creates `<root>/uploads/<id>/parts/`, writes `manifest.json` recording the target key, content type, and creation time. Does not lock the target key; multipart uploads do not reserve the key.
- **`UploadPart`** streams part bytes via temp + atomic rename to `parts/NNNNN` (zero-padded; `PartNumber` is 1-based per AD10). Returns the part's sha256 as its token. Out-of-order or repeated part numbers are allowed at upload time; repeated `PartNumber` overwrites the prior part's bytes via the same temp+rename. Part numbers < 1 return `ErrInvalidArgument`.
- **`CompleteMultipartIfAbsent`** validates the `parts` slice is non-empty and contiguously numbered (1, 2, 3, ...); acquires the target key's mutex; performs the same Stat-then-write sequence as `PutIfAbsent`; concatenates parts in order while computing streaming sha256; atomically promotes the assembled content via the atomic write pattern; removes the upload directory on success. If the target already exists, returns `ErrAlreadyExists` and leaves the upload directory intact (the caller may `Abort` or retry against a new key).
- **`Abort`** removes the upload directory. Idempotent: aborting an already-aborted or already-completed upload is a no-op (silently succeeds).
- **`SignedGetURL`** returns `ErrNotSupported`.
- **`.lock` startup check** opens `<root>/.lock` with `O_CREATE|O_EXCL`. On success, holds it open for process lifetime. On failure, returns `ErrAlreadyLocked`. Stale locks can be removed by `bucketvcs doctor` (M16) or by hand.

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

Localfs must not expose files outside the bucket root via symlinks placed within `<root>/objects/`. The protection is:

- All read entry points (`Get`, `Head`, `GetRange`, `List`) call `os.Lstat` on the target path before opening. If the entry is a symlink (`Mode().Type() == fs.ModeSymlink`), the operation returns `ErrInvalidArgument` and `List` skips the entry with a structured warning.
- Write paths create files via `os.OpenFile` with `O_CREATE|O_EXCL` on a temp path under `filepath.Dir(target)`. The dest directory is created with `os.MkdirAll`, which does not follow existing symlinks for the final component.
- Full path-resolution sandboxing (e.g., `openat2(RESOLVE_BENEATH)` on Linux) is out of scope for M0. The defense above covers the realistic dev/test footgun.

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
9. `internal/storage/README.md` documents: the interface contract, how to add a new adapter, how to run the conformance suite against an arbitrary adapter, the AD8 recast, the filesystem portability assumptions, and any expected divergences.
10. Public Go-doc comments on every exported symbol in `internal/storage`.
11. A worked example (~50 lines, in `internal/storage/example_test.go` or under `examples/`) exercises Put, Get, PutIfVersionMatches, List, Multipart, Delete on localfs, including the conflict paths.

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
