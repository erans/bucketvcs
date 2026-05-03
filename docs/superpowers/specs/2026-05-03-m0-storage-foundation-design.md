# M0 — Storage Foundation: ObjectStore Contract, localfs Adapter, Conformance Suite

Date: 2026-05-03
Status: design draft
Milestone: M0 (first critical-path milestone of bucketvcs OSS-core)
Source spec sections: §9, §10, §29 (subset), §35, §40.1, §40.4
Decomposition: see `2026-05-03-bucketvcs-oss-decomposition-design.md`

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
| AD3 | localfs CAS implemented via in-process keyed `sync.Mutex` map | Spec §35 frames localfs as "dev/test." All M0–M3 demos run in a single process. Simpler than `flock` (no NFS/Windows trouble); simpler than SQLite (preserves on-disk inspectability). Multi-process can be retrofitted later without changing the interface. |
| AD4 | `ObjectVersion` token on localfs = `sha256(content)` hex string | Deterministic, derivable from content, no separate state. Matches the "content-addressed" theme of the broader spec. Computed on write, cached in JSON sidecar. |
| AD5 | Multipart implemented as a real spec-conforming operation on localfs | The §29 multipart tests need real code paths to exercise. Stubbing leaves a hole that only fills at M5. Cost on localfs is small (~100 lines). |
| AD6 | `SignedGetURL` stays on the contract; localfs returns `ErrNotSupported`; capability flag declared | Real signed-URL emulation only matters when a milestone needs it (likely M11 packfile-uri). Adding a capability flag now lets the conformance suite skip cleanly with a documented reason. |
| AD7 | Conformance suite is a regular Go package (`internal/storage/conformance`) callable from any adapter's `_test.go` AND from a future CLI subcommand | One implementation, two callers. M0 ships only the `go test` entry point; the CLI hook lands at M3. |
| AD8 | §29 correctness test #14 ("network retry does not duplicate committed object") recast on localfs as: "PutIfAbsent twice with the same args returns ErrAlreadyExists cleanly without corrupting state on the second call." | Same invariant, exercised at the local level. Spec mapping table in this doc documents the recast. Cloud adapters at M5/M7 add a transient-retry-mid-call simulation as an additional case. |

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
    Metadata    map[string]string
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

### Mechanics

- **CAS critical sections** are guarded by a keyed mutex map: `map[string]*sync.Mutex` accessed under a `sync.RWMutex` for the map itself. Acquired around the read-modify-write sequence: lock, read sidecar, compare expected version, write temp file, atomic rename, write new sidecar, release.
- **Atomic write pattern** for any object: write to `<root>/objects/<key>.tmp.<rand>`, fsync the file, rename to `<root>/objects/<key>`, write `<root>/objects/<key>.meta.tmp.<rand>`, fsync, rename. POSIX rename within the same filesystem is atomic. There is a small window where content is committed but sidecar lags; `Get`/`Head` self-heal by recomputing sha256 from content if sidecar is missing or stale.
- **`PutIfAbsent`** uses `os.OpenFile(path, O_CREATE|O_EXCL|O_WRONLY, ...)` on a temp path, then performs the atomic rename under the keyed mutex. If the target object already exists at rename time, returns `ErrAlreadyExists` and removes the temp file.
- **`PutIfVersionMatches`** acquires the keyed mutex, reads the sidecar, compares `expected.Token` against `sidecar.sha256`, and proceeds only if equal; otherwise returns `ErrVersionMismatch`.
- **`DeleteIfVersionMatches`** mirrors `PutIfVersionMatches`: acquire mutex, compare version, then `os.Remove` content + sidecar. Removal of an absent object returns `ErrNotFound`.
- **`List`** walks `<root>/objects/<prefix>` lexicographically. Pagination uses last-returned key as a continuation token. `Delimiter` support produces `CommonPrefixes` to match the cloud-style "directory-like" listing semantics.
- **`CreateMultipart`** generates a UUID, creates `<root>/uploads/<id>/parts/`, writes `manifest.json` with target key. Returns a `MultipartUpload` whose `UploadPart` writes part bytes to `parts/NNNNN` and returns the part's sha256 as its token.
- **`CompleteMultipartIfAbsent`** validates parts (numbering contiguous, tokens match), concatenates them in order while computing a streaming sha256, atomically promotes the assembled content to `<root>/objects/<key>` under the keyed mutex via the same atomic-write pattern, writes the sidecar, and removes the upload directory. If the target key already exists, returns `ErrAlreadyExists` and discards the assembled bytes.
- **`SignedGetURL`** returns `ErrNotSupported`.
- **`.lock` startup check** opens `<root>/.lock` with `O_CREATE|O_EXCL`. On success, holds it open for process lifetime. On failure, returns an error indicating another process may be using this root. Stale locks can be removed by `bucketvcs doctor` (M16) or by hand.

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

These are not §29-derived but are a baseline safety floor for any adapter. Heavier fuzzing belongs in a security-review pass after M5.

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
4. `Capabilities()` declarations round-trip through the interface correctly.
5. Documented error taxonomy maps to the conformance assertions per the table above.
6. Package layout matches §40.1.
7. `internal/storage/README.md` documents: the interface contract, how to add a new adapter, how to run the conformance suite against an arbitrary adapter, the AD8 recast and any expected divergences.
8. Public Go-doc comments on every exported symbol in `internal/storage`.
9. A worked example (~50 lines, in `internal/storage/example_test.go` or under `examples/`) exercises Put, Get, PutIfVersionMatches, List, Multipart, Delete on localfs, including the conflict paths.

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

## Open questions deferred to implementation

These are implementation-plan decisions, not design-spec decisions:

- Exact JSON sidecar schema (field names, version field migration policy)
- Whether to integrate with `golang.org/x/exp/slog` or wait for M3's logging framework decision
- Specific UUID library choice for upload IDs
- Whether part numbering is 1-based (S3 convention) or 0-based (cleaner Go)
- Whether `Capabilities` is fetched once per adapter instance or per call (cached vs not)
- Concurrency limits inside the keyed-mutex map (when to evict idle entries)
- Test parallelism caps (`t.Parallel()` policy across the conformance suite)

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
