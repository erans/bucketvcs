# M0 — Storage Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a provider-neutral `ObjectStore` Go interface, a single-process localfs adapter, and a reusable conformance test suite that verifies the §29 correctness and stress invariants. After M0, M1 (manifest CAS) can be built directly on top of `ObjectStore`.

**Architecture:** Bespoke `ObjectStore` interface (no portable-blob library wrapper). Localfs implements the contract with in-process keyed `sync.Mutex` for both reads and writes (AD3), JSON sidecars for metadata, content sha256 as version token, atomic-rename + directory `fsync` for durable writes (crash recovery semantics tabulated in the spec), real spec-conforming multipart, lstat-based best-effort symlink rejection (AD11). Conformance suite is a regular Go package that any adapter can call from its own test file via `conformance.Run(t, factory)`.

**Tech Stack:** Go 1.22, `github.com/google/uuid` v1.6.0. No other external dependencies in M0. Linux + macOS targets.

**Reference docs:**
- Design spec (r4): `docs/superpowers/specs/2026-05-03-m0-storage-foundation-design.md`
- Original spec sections: §9, §10, §29, §35, §40.1
- Decomposition: `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md`

**Module path placeholder:** uses `github.com/bucketvcs/bucketvcs`. This is a TBD pending governance gate G1. Substitute the real path once G1 is settled. The plan itself is unchanged by the substitution.

**Plan revision r2 changes (2026-05-03):** Updated to match design spec r2 (which addresses roborev design review job 7684). Notable plan-level changes from r1:

- Tasks 14 and 15 now acquire the per-key keyed mutex on read paths (`Get`, `Head`, `GetRange`) per AD3, eliminating the (content-N, sidecar-N-1) torn-read window.
- Tasks 14, 15, and 19 reject symlinks via `lstatNoSymlink` / WalkDir filter per AD11.
- Task 4 drops `PutOptions.Metadata` per AD9.
- Task 5 doc comment updated to allow out-of-order and repeated multipart part numbers (AD10 pins 1-based).
- The `head()` helper splits into `headLocked()` (caller holds mutex) and `head()` (locking wrapper for List).
- Two new tasks: Task 33 (multipart lifecycle edge-case conformance tests) and Task 34 (localfs symlink-rejection test).

**Plan revision r3 changes (2026-05-03):** Updated to match design spec r3 (addresses roborev round 2, job 7686).

- Task 14 adds `fsyncDir(path string) error` helper and calls `fsyncDir(filepath.Dir(...))` after every `os.Rename` in `writeAtomic` and `writeFileAtomic` for crash durability of directory-entry changes.
- Task 18 swaps the Delete order to **content-first then sidecar** so a crash mid-delete leaves an orphan sidecar (subsequent `Head` returns `ErrNotFound`, the correct outcome) rather than the inverse failure mode where self-heal would resurrect the deleted object. Each `os.Remove` is followed by `fsyncDir`.
- Task 21 multipart `CompleteMultipartIfAbsent` calls `fsyncDir` after the assembled-content rename.
- The localfs `lstatNoSymlink` claim is narrowed to "best-effort final-path"; the README and `localfs_test.go` document residual gaps (ancestor-directory symlinks, hardlinks, TOCTOU between `Lstat` and `Open`).

**Plan revision r4 changes (2026-05-03):** Updated to match design spec r4 (addresses roborev round 3, job 7688).

- Task 14 `headLocked` adds a size-mismatch fast-path: if `os.Stat(content).Size() != sidecar.Size`, the sidecar is treated as stale and self-heal triggers (recompute sha256 from content, rewrite sidecar). This catches the post-crash "content (new) + sidecar (old)" window when content size changed; same-size crash windows are addressed by `bucketvcs doctor` at M16. Conditional writes (`PutIfVersionMatches`, `DeleteIfVersionMatches`) inherit the fast-path because they call `headLocked` first.
- Task 30 split into two unit tests: missing-sidecar self-heal and size-mismatch self-heal.
- Task 32 README expanded with three verbatim sections from the design spec: "Symlink and hardlink safety (AD11)", "Filesystem portability assumptions", and "Crash recovery and `bucketvcs doctor`". Operator-facing warnings cannot be silently diluted.

---

## File structure (created across all tasks)

```text
github.com/bucketvcs/bucketvcs/
├── go.mod
├── go.sum
├── .gitignore
├── cmd/bucketvcs/main.go                                  Task 1
├── internal/storage/
│   ├── errors.go                                          Task 2
│   ├── version.go                                         Task 3
│   ├── version_test.go                                    Task 3
│   ├── options.go                                         Task 4
│   ├── multipart.go                                       Task 5
│   ├── objectstore.go                                     Task 6
│   ├── README.md                                          Task 32
│   ├── example_test.go                                    Task 31
│   ├── conformance/
│   │   ├── suite.go                                       Task 8
│   │   ├── correctness.go                                 Tasks 14–26 + 33 (additive)
│   │   ├── stress.go                                      Tasks 27–29 (additive)
│   │   ├── fixtures.go                                    Task 8
│   │   └── testenv.go                                     Task 8
│   └── localfs/
│       ├── keys.go                                        Task 10
│       ├── keys_test.go                                   Task 10
│       ├── keyed_mutex.go                                 Task 11
│       ├── keyed_mutex_test.go                            Task 11
│       ├── meta.go                                        Task 12
│       ├── meta_test.go                                   Task 12
│       ├── localfs.go                                     Task 7 (stub) → Tasks 13–23 (real)
│       ├── localfs_test.go                                Tasks 13, 30, 34 (lock file, self-heal, symlink rejection)
│       ├── multipart.go                                   Task 21
│       └── localfs_conformance_test.go                    Task 9
```

Each file has one focused responsibility. Tests for type-only declarations are skipped when there is no behavior to assert; the package's compile + `go vet ./...` clean covers correctness for those.

---

## Task 1: Bootstrap module

**Files:**
- Create: `go.mod`
- Create: `.gitignore`
- Create: `cmd/bucketvcs/main.go`

- [ ] **Step 1: Initialize the Go module**

Run from the repo root (`/home/eran/work/bucketvcs`):

```bash
go mod init github.com/bucketvcs/bucketvcs
```

Expected: creates `go.mod` containing `module github.com/bucketvcs/bucketvcs` and `go 1.22` (or whichever toolchain is installed; require ≥ 1.22).

If the installed toolchain reports a Go version lower than 1.22, install Go 1.22+ before continuing.

- [ ] **Step 2: Pin minimum Go version in go.mod**

Edit `go.mod` so it reads exactly:

```text
module github.com/bucketvcs/bucketvcs

go 1.22
```

- [ ] **Step 3: Create .gitignore**

Write `.gitignore`:

```text
# Binaries
/bin/
bucketvcs
bucketvcs.exe

# Test artifacts
*.out
coverage.txt
coverage.html

# Local dev state
*.local
.env
.env.*

# Editor noise
.idea/
.vscode/
*.swp
.DS_Store
```

- [ ] **Step 4: Create cmd/bucketvcs/main.go skeleton**

Write `cmd/bucketvcs/main.go`:

```go
// Command bucketvcs is the entry point for the bucketvcs CLI. M0 ships
// this as a placeholder so that go build ./... succeeds. Real subcommands
// land in later milestones (M3 introduces the Git protocol gateway and
// the first useful CLI surface).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "bucketvcs: no subcommands available in M0")
	os.Exit(1)
}
```

- [ ] **Step 5: Verify the build is clean**

Run:

```bash
go build ./...
go vet ./...
```

Expected: both succeed silently (no output, exit code 0).

- [ ] **Step 6: Commit**

```bash
git add go.mod .gitignore cmd/bucketvcs/main.go
git commit -m "Bootstrap Go module skeleton for M0

Adds go.mod (module path placeholder pending governance gate G1),
.gitignore, and a no-op cmd/bucketvcs/main.go so go build ./... has
something to build."
```

---

## Task 2: Sentinel errors

**Files:**
- Create: `internal/storage/errors.go`

- [ ] **Step 1: Create errors.go with the eight sentinels from the design spec**

Write `internal/storage/errors.go`:

```go
// Package storage defines the provider-neutral storage contract used by
// every bucketvcs adapter. Adapters (localfs in M0; AWS S3, GCS, R2, Azure
// Blob in later milestones) implement ObjectStore and must pass the
// conformance suite for the specific backend/configuration in use.
package storage

import "errors"

// Sentinel errors returned by ObjectStore implementations. Callers compare
// against these with errors.Is to make routing decisions.
//
// Adapters wrap their underlying provider errors with these sentinels so
// classification is consistent across providers. The conformance suite
// verifies the mapping per §29 #13 and #15 of the original spec.
var (
	// ErrNotFound: the requested object does not exist.
	ErrNotFound = errors.New("storage: object not found")

	// ErrAlreadyExists: PutIfAbsent or CompleteMultipartIfAbsent failed
	// because the target key is already present.
	ErrAlreadyExists = errors.New("storage: object already exists")

	// ErrVersionMismatch: PutIfVersionMatches or DeleteIfVersionMatches
	// failed because the on-store version differs from the expected
	// version.
	ErrVersionMismatch = errors.New("storage: version mismatch")

	// ErrThrottled: the provider is rate-limiting. Caller may retry with
	// backoff.
	ErrThrottled = errors.New("storage: throttled")

	// ErrTransient: a retryable transient failure (network blip, brief
	// provider unavailability). Caller may retry.
	ErrTransient = errors.New("storage: transient error")

	// ErrInvalidArgument: the caller supplied an argument that violates
	// the contract (malformed key, negative offset, etc.).
	ErrInvalidArgument = errors.New("storage: invalid argument")

	// ErrAccessDenied: authentication or authorization with the provider
	// failed. Not retryable.
	ErrAccessDenied = errors.New("storage: access denied")

	// ErrNotSupported: the operation is not supported by this adapter.
	// Inspect Capabilities() to decide before calling.
	ErrNotSupported = errors.New("storage: not supported by adapter")
)
```

- [ ] **Step 2: Verify the package compiles**

Run:

```bash
go vet ./internal/storage/...
```

Expected: succeeds silently.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/errors.go
git commit -m "storage: add sentinel error taxonomy

Eight sentinels covering not-found, already-exists, version-mismatch,
retryable (throttled/transient), permanent (invalid-argument/access-denied),
and capability (not-supported). Conformance suite §29 #13 and #15 verify
adapter mapping."
```

---

## Task 3: ObjectVersion + VersionKind

**Files:**
- Create: `internal/storage/version.go`
- Create: `internal/storage/version_test.go`

- [ ] **Step 1: Write the failing test for VersionKind.String()**

Write `internal/storage/version_test.go`:

```go
package storage

import "testing"

func TestVersionKindString(t *testing.T) {
	cases := []struct {
		k    VersionKind
		want string
	}{
		{VersionUnknown, "unknown"},
		{VersionEtag, "etag"},
		{VersionGeneration, "generation"},
		{VersionVersionID, "version_id"},
		{VersionOpaque, "opaque"},
		{VersionKind(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("VersionKind(%d).String() = %q, want %q", c.k, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
go test ./internal/storage/...
```

Expected: FAIL — `VersionKind` and constants undefined.

- [ ] **Step 3: Create version.go with the type and method**

Write `internal/storage/version.go`:

```go
package storage

// ObjectVersion is the normalized cross-provider version token. Core
// repository logic compares versions by value and never inspects Provider
// or Kind for routing decisions; those fields exist for diagnostics and
// for adapters that need to round-trip provider metadata.
//
// Token semantics are adapter-defined and opaque to callers: localfs uses
// hex-encoded sha256 of content, S3 uses the ETag, GCS uses the
// generation, and so on.
type ObjectVersion struct {
	Provider string
	Token    string
	Kind     VersionKind
}

// VersionKind is a hint about the provider-native form of an
// ObjectVersion's token. Callers must not switch on Kind for correctness;
// it is informational.
type VersionKind int

const (
	VersionUnknown VersionKind = iota
	VersionEtag
	VersionGeneration
	VersionVersionID
	VersionOpaque
)

// String returns a stable lowercase label for the kind, suitable for logs
// and error messages.
func (k VersionKind) String() string {
	switch k {
	case VersionEtag:
		return "etag"
	case VersionGeneration:
		return "generation"
	case VersionVersionID:
		return "version_id"
	case VersionOpaque:
		return "opaque"
	default:
		return "unknown"
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run:

```bash
go test ./internal/storage/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/version.go internal/storage/version_test.go
git commit -m "storage: add ObjectVersion + VersionKind

Normalized version token usable across S3 ETags, GCS generations,
Azure Blob version IDs, and adapter-defined opaque tokens. Localfs
uses sha256-content as token; cloud adapters fill in their kind at
M5/M7."
```

---

## Task 4: Object, ObjectMetadata, Capabilities, options, ListPage

**Files:**
- Create: `internal/storage/options.go`

- [ ] **Step 1: Create options.go with the request/response value types**

Write `internal/storage/options.go`:

```go
package storage

import (
	"io"
	"time"
)

// Object is the result of a successful Get. The caller must Close Body
// when done.
type Object struct {
	Body     io.ReadCloser
	Metadata ObjectMetadata
}

// ObjectMetadata describes a stored object without its body bytes.
type ObjectMetadata struct {
	Key         string
	Version     ObjectVersion
	Size        int64
	ContentType string
	ModifiedAt  time.Time
}

// Capabilities advertises what an adapter supports. Conformance tests
// gate behavior on these flags so an adapter can declare honestly that
// it does not implement an optional capability.
type Capabilities struct {
	// SignedURLs reports whether SignedGetURL returns a working URL. If
	// false, SignedGetURL returns ErrNotSupported.
	SignedURLs bool

	// MultipartMinPartSize is the minimum allowed part size in bytes for
	// non-final parts. Zero means no minimum.
	MultipartMinPartSize int64

	// MultipartMaxParts is the maximum number of parts the adapter will
	// accept. Zero means no adapter-imposed cap.
	MultipartMaxParts int

	// MaxObjectSize is the maximum size of a single object in bytes.
	// Zero means no adapter-imposed cap.
	MaxObjectSize int64

	// StrongList reports whether List provides strong read-after-write
	// for objects PUT before the call.
	StrongList bool
}

// GetOptions controls Get behavior.
type GetOptions struct {
	// IfVersionMatches, when non-nil, causes Get to return
	// ErrVersionMismatch if the on-store version differs.
	IfVersionMatches *ObjectVersion
}

// PutOptions controls Put-family behavior. M0 ships only ContentType;
// user-defined metadata is intentionally deferred (AD9 in the M0 design
// spec). Cloud adapters at M5/M7 reintroduce metadata mapped to
// provider-native fields (S3 x-amz-meta-*, GCS object metadata, etc.).
type PutOptions struct {
	ContentType string
}

// ListOptions controls List behavior.
type ListOptions struct {
	// MaxKeys caps the page size. Zero means adapter-default.
	MaxKeys int

	// ContinuationToken is the NextToken from a previous ListPage. Empty
	// means start from the beginning of the prefix.
	ContinuationToken string

	// Delimiter, if non-empty, causes the adapter to roll keys ending in
	// Delimiter into CommonPrefixes rather than Objects.
	Delimiter string
}

// ListPage is one page of List results.
type ListPage struct {
	Objects        []ObjectMetadata
	NextToken      string
	CommonPrefixes []string
}

// MultipartOptions controls CreateMultipart.
type MultipartOptions struct {
	ContentType string
}

// SignedURLOptions controls SignedGetURL.
type SignedURLOptions struct {
	Expires time.Duration
	Method  string // typically "GET"
}
```

- [ ] **Step 2: Verify the package compiles**

```bash
go vet ./internal/storage/...
go test ./internal/storage/...
```

Expected: vet silent. Tests still pass (no behavior added).

- [ ] **Step 3: Commit**

```bash
git add internal/storage/options.go
git commit -m "storage: add option/result types

Object, ObjectMetadata, Capabilities, GetOptions, PutOptions,
ListOptions, ListPage, MultipartOptions, SignedURLOptions. Pure
declarations; no behavior."
```

---

## Task 5: MultipartPart + MultipartUpload interface

**Files:**
- Create: `internal/storage/multipart.go`

- [ ] **Step 1: Create multipart.go**

Write `internal/storage/multipart.go`:

```go
package storage

import (
	"context"
	"io"
)

// MultipartPart describes one uploaded part of a multipart upload.
// Adapters define the meaning of Token; localfs uses hex sha256 of part
// bytes.
type MultipartPart struct {
	PartNumber int
	Token      string
	Size       int64
}

// MultipartUpload is a handle to an in-progress multipart upload.
// CreateMultipart returns one of these. The caller uploads parts via
// UploadPart, then completes the upload via
// ObjectStore.CompleteMultipartIfAbsent. If the upload should be
// discarded, the caller calls Abort.
type MultipartUpload interface {
	// UploadID is the adapter-defined identifier for this upload. It
	// must be stable for the life of the upload.
	UploadID() string

	// Key is the target object key the upload will become on completion.
	Key() string

	// UploadPart uploads one part. PartNumber is 1-based (1, 2, 3,
	// ...). Out-of-order and repeated part numbers are allowed at
	// upload time; uploading the same partNumber twice overwrites the
	// prior part. Final ordering and contiguity are validated at
	// CompleteMultipartIfAbsent. Body may exceed MultipartMinPartSize
	// for non-final parts.
	UploadPart(ctx context.Context, partNumber int, body io.Reader) (MultipartPart, error)

	// Abort cancels the upload and removes any temporary state. After
	// Abort, no further calls on this upload are valid.
	Abort(ctx context.Context) error
}
```

- [ ] **Step 2: Verify build is clean**

```bash
go vet ./internal/storage/...
```

Expected: silent.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/multipart.go
git commit -m "storage: add MultipartUpload interface and MultipartPart type"
```

---

## Task 6: ObjectStore interface

**Files:**
- Create: `internal/storage/objectstore.go`

- [ ] **Step 1: Create objectstore.go with the full contract**

Write `internal/storage/objectstore.go`:

```go
package storage

import (
	"context"
	"io"
)

// ObjectStore is the provider-neutral storage contract. Every bucketvcs
// adapter implements every method and must pass the conformance suite at
// internal/storage/conformance for the specific backend/configuration in
// use.
//
// Method semantics:
//
//   - Get/Head/GetRange: read paths; return ErrNotFound if the key is
//     absent.
//   - PutIfAbsent: create-only; returns ErrAlreadyExists if the key is
//     already present.
//   - PutIfVersionMatches: update-only with optimistic concurrency;
//     returns ErrVersionMismatch if the on-store version differs from
//     expected, or ErrVersionMismatch if the key does not exist.
//   - DeleteIfVersionMatches: delete with optimistic concurrency; returns
//     ErrVersionMismatch on version skew, ErrNotFound if absent.
//   - List: prefix listing, paginated via ContinuationToken/NextToken.
//   - CreateMultipart/CompleteMultipartIfAbsent: large-object upload
//     path; CompleteMultipartIfAbsent returns ErrAlreadyExists if the
//     target key is already present (the spec §29 #8 invariant).
//   - SignedGetURL: returns a short-lived URL the caller can hand to a
//     third party for read access. Adapters that do not support this
//     return ErrNotSupported and report Capabilities{SignedURLs: false}.
type ObjectStore interface {
	// Capabilities reports adapter features and limits.
	Capabilities() Capabilities

	// Get reads an object. Caller must Close the returned Body.
	Get(ctx context.Context, key string, opts *GetOptions) (*Object, error)

	// Head reads metadata without the body.
	Head(ctx context.Context, key string) (*ObjectMetadata, error)

	// GetRange reads bytes [start, endInclusive] from an object. If
	// endInclusive exceeds the object size, the returned reader yields
	// only the existing bytes. Negative indices are ErrInvalidArgument.
	GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error)

	// PutIfAbsent stores body at key only if no object exists at key.
	// Returns ErrAlreadyExists otherwise.
	PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *PutOptions) (ObjectVersion, error)

	// PutIfVersionMatches stores body at key only if the on-store
	// version matches expected. Returns ErrVersionMismatch otherwise
	// (including when the key does not exist).
	PutIfVersionMatches(ctx context.Context, key string, expected ObjectVersion, body io.Reader, opts *PutOptions) (ObjectVersion, error)

	// DeleteIfVersionMatches removes the object only if the on-store
	// version matches expected. Returns ErrVersionMismatch on skew or
	// ErrNotFound if absent.
	DeleteIfVersionMatches(ctx context.Context, key string, expected ObjectVersion) error

	// List returns one page of objects under prefix.
	List(ctx context.Context, prefix string, opts *ListOptions) (*ListPage, error)

	// CreateMultipart begins a multipart upload targeting key.
	CreateMultipart(ctx context.Context, key string, opts *MultipartOptions) (MultipartUpload, error)

	// CompleteMultipartIfAbsent assembles parts into the target key only
	// if no object already exists at the target. Returns ErrAlreadyExists
	// otherwise.
	CompleteMultipartIfAbsent(ctx context.Context, upload MultipartUpload, parts []MultipartPart) (ObjectVersion, error)

	// SignedGetURL returns a short-lived URL granting read access.
	// Adapters without signed-URL support return ErrNotSupported.
	SignedGetURL(ctx context.Context, key string, opts SignedURLOptions) (string, error)
}
```

- [ ] **Step 2: Verify the package compiles**

```bash
go vet ./internal/storage/...
go test ./internal/storage/...
```

Expected: silent vet, tests still pass.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/objectstore.go
git commit -m "storage: add ObjectStore interface (provider-neutral contract)

Defines the full §9 contract: Capabilities, Get/Head/GetRange,
PutIfAbsent/PutIfVersionMatches/DeleteIfVersionMatches, List,
multipart, and SignedGetURL. Every adapter implements this interface
and must pass the conformance suite."
```

---

## Task 7: Localfs stub adapter (compile-only)

**Files:**
- Create: `internal/storage/localfs/localfs.go`

- [ ] **Step 1: Write the stub that satisfies ObjectStore**

Write `internal/storage/localfs/localfs.go`:

```go
// Package localfs implements storage.ObjectStore over a regular local
// filesystem. It is intended for development, tests, and small
// self-hosted deployments. Localfs is single-process: holding two open
// Localfs instances against the same root directory in different
// processes is undefined.
package localfs

import (
	"context"
	"errors"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Localfs is the local-filesystem ObjectStore implementation.
type Localfs struct {
	root string
}

// Compile-time assertion that *Localfs satisfies storage.ObjectStore.
var _ storage.ObjectStore = (*Localfs)(nil)

// Open returns a Localfs rooted at the given directory. The directory
// must exist. Real implementations land in later tasks; this stub just
// returns ErrNotSupported on every method so the package compiles and
// the conformance suite has a target to fail against.
func Open(root string) (*Localfs, error) {
	if root == "" {
		return nil, errors.New("localfs: root must be non-empty")
	}
	return &Localfs{root: root}, nil
}

// Close releases any resources held by the Localfs instance.
func (l *Localfs) Close() error {
	return nil
}

func (l *Localfs) Capabilities() storage.Capabilities {
	return storage.Capabilities{}
}

func (l *Localfs) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrNotSupported
}

func (l *Localfs) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrNotSupported
}

func (l *Localfs) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	return storage.ErrNotSupported
}

func (l *Localfs) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) CreateMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) CompleteMultipartIfAbsent(ctx context.Context, upload storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrNotSupported
}

func (l *Localfs) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	return "", storage.ErrNotSupported
}
```

- [ ] **Step 2: Verify the package compiles**

```bash
go vet ./...
go build ./...
```

Expected: silent. Compile-time assertion `var _ storage.ObjectStore = (*Localfs)(nil)` will fail to compile if any signature is wrong.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/localfs/localfs.go
git commit -m "localfs: stub adapter returning ErrNotSupported

Compile-only skeleton with the full ObjectStore method set. Real
implementations land per behavior in later tasks. The compile-time
assertion var _ storage.ObjectStore = (*Localfs)(nil) ensures the
signature stays in sync with the interface."
```

---

## Task 8: Conformance package scaffolding

**Files:**
- Create: `internal/storage/conformance/suite.go`
- Create: `internal/storage/conformance/correctness.go`
- Create: `internal/storage/conformance/stress.go`
- Create: `internal/storage/conformance/fixtures.go`
- Create: `internal/storage/conformance/testenv.go`

- [ ] **Step 1: Create suite.go with Run and Factory**

Write `internal/storage/conformance/suite.go`:

```go
// Package conformance is the storage adapter conformance test suite. It
// is a regular Go package, not a _test package, so it can be imported
// from any adapter's _test.go file and (later) from a
// `bucketvcs conformance-test` CLI subcommand.
//
// The contract being tested is documented at internal/storage as
// ObjectStore. Test names map to the §29 correctness and stress lists in
// the original spec; the comment on each test cites its §29 number.
package conformance

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Factory returns a fresh storage.ObjectStore for one test invocation,
// plus a cleanup function the suite calls when the test finishes.
//
// Each call must return an empty, isolated store: the suite does not
// share state across tests.
type Factory func(t testing.TB) (storage.ObjectStore, func())

// Run executes the full conformance suite (correctness + stress) against
// any adapter. Adapter packages call this from their _test.go files. The
// stress sub-suite is skipped when go test -short is in effect.
func Run(t *testing.T, f Factory) {
	t.Helper()
	t.Run("correctness", func(t *testing.T) {
		runCorrectness(t, f)
	})
	t.Run("stress", func(t *testing.T) {
		if testing.Short() {
			t.Skip("stress tests skipped in -short mode")
		}
		runStress(t, f)
	})
}
```

- [ ] **Step 2: Create correctness.go with the runner shell**

Write `internal/storage/conformance/correctness.go`:

```go
package conformance

import "testing"

// runCorrectness is the entry point for the §29 correctness tests. Each
// test corresponds to one numbered item in §29 and is added in a later
// task. This shell exists from Task 8 so adapter wiring can compile.
func runCorrectness(t *testing.T, f Factory) {
	t.Helper()
	// Tests are appended here as Tasks 14–26 implement them.
}
```

- [ ] **Step 3: Create stress.go with the runner shell**

Write `internal/storage/conformance/stress.go`:

```go
package conformance

import "testing"

// runStress is the entry point for the §29 stress tests applicable to
// localfs in M0: 100 concurrent CAS attempts, 10,000 small object
// creates, large multipart pack upload conflict.
func runStress(t *testing.T, f Factory) {
	t.Helper()
	// Stress tests are appended here as Tasks 27–29 implement them.
}
```

- [ ] **Step 4: Create fixtures.go with deterministic generators**

Write `internal/storage/conformance/fixtures.go`:

```go
package conformance

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
)

// DeterministicBytes returns n bytes generated from seed. Two calls with
// the same (n, seed) return the same bytes. Used for test fixtures so
// fixture content is stable across runs without inflating the repo.
func DeterministicBytes(n int, seed string) []byte {
	out := make([]byte, n)
	h := fnv.New64a()
	h.Write([]byte(seed))
	state := h.Sum64()
	for i := 0; i < n; i += 8 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		end := i + 8
		if end > n {
			end = n
		}
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], state)
		copy(out[i:end], buf[:end-i])
	}
	return out
}

// Key returns a key derived from a base prefix and an integer suffix,
// formatted so lexicographic order matches numeric order across the
// expected range. Tests use this for predictable List ordering.
func Key(prefix string, n int) string {
	return fmt.Sprintf("%s/%010d", prefix, n)
}
```

- [ ] **Step 5: Create testenv.go with adapter-factory helpers**

Write `internal/storage/conformance/testenv.go`:

```go
package conformance

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// newStore wraps the Factory call so test code reads more cleanly.
func newStore(t testing.TB, f Factory) storage.ObjectStore {
	t.Helper()
	s, cleanup := f(t)
	t.Cleanup(cleanup)
	return s
}

// ctx returns a fresh background context for use in tests. Tests that
// need cancellation create their own; this helper is for the common case.
func ctx() context.Context { return context.Background() }
```

- [ ] **Step 6: Verify the conformance package compiles**

```bash
go vet ./...
go test ./...
```

Expected: silent vet, tests still pass (suite is empty so far).

- [ ] **Step 7: Commit**

```bash
git add internal/storage/conformance/
git commit -m "storage: scaffold conformance test suite

Public Run(t, factory) entry point; Factory contract; correctness/stress
runner shells; deterministic byte/key fixtures. Suite is empty until
Tasks 14–29 add behavior tests."
```

---

## Task 9: Wire localfs into the conformance suite

**Files:**
- Create: `internal/storage/localfs/localfs_conformance_test.go`

- [ ] **Step 1: Write the conformance wiring test**

Write `internal/storage/localfs/localfs_conformance_test.go`:

```go
package localfs_test

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/conformance"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t testing.TB) (storage.ObjectStore, func()) {
		dir := t.TempDir()
		s, err := localfs.Open(dir)
		if err != nil {
			t.Fatalf("localfs.Open(%q): %v", dir, err)
		}
		return s, func() { _ = s.Close() }
	})
}
```

- [ ] **Step 2: Run the test to verify it passes (with empty suite)**

```bash
go test ./internal/storage/localfs/...
```

Expected: PASS — the conformance suite is empty so there is nothing to fail.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/localfs/localfs_conformance_test.go
git commit -m "localfs: wire adapter into conformance suite

Imports the conformance package and runs Run(t, factory) against a
fresh localfs in t.TempDir(). Empty suite passes today; tests added in
Tasks 14+ will exercise behavior."
```

---

## Task 10: Localfs key validation

**Files:**
- Create: `internal/storage/localfs/keys.go`
- Create: `internal/storage/localfs/keys_test.go`

- [ ] **Step 1: Write failing tests for key validation**

Write `internal/storage/localfs/keys_test.go`:

```go
package localfs

import (
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	valid := []string{
		"a",
		"a/b",
		"tenants/t1/repos/r1/manifest/root.json",
		strings.Repeat("a", 1024),
	}
	for _, k := range valid {
		if err := validateKey(k); err != nil {
			t.Errorf("validateKey(%q) returned %v, want nil", k, err)
		}
	}

	invalid := []string{
		"",
		"/leading-slash",
		"trailing-slash/",
		"has/../segment",
		"..",
		"with\x00nullbyte",
		"with\\backslash",
		strings.Repeat("a", 1025),
		"foo.meta",
		"a/b.meta",
	}
	for _, k := range invalid {
		if err := validateKey(k); err == nil {
			t.Errorf("validateKey(%q) returned nil, want error", k)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/storage/localfs/...
```

Expected: FAIL — `validateKey` undefined.

- [ ] **Step 3: Create keys.go with the implementation**

Write `internal/storage/localfs/keys.go`:

```go
package localfs

import (
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

const maxKeyBytes = 1024

// validateKey returns ErrInvalidArgument if key violates localfs key
// rules. The rules are conservative — they match the most-restrictive
// floor across cloud providers — so a key valid for localfs is also
// valid for S3, GCS, R2, and Azure Blob. Localfs additionally reserves
// the ".meta" suffix for its own JSON sidecars; keys ending in ".meta"
// are rejected so that the sidecar namespace cannot collide with real
// object keys.
func validateKey(key string) error {
	if key == "" {
		return errKey("empty")
	}
	if len(key) > maxKeyBytes {
		return errKey(fmt.Sprintf("longer than %d bytes", maxKeyBytes))
	}
	if strings.HasPrefix(key, "/") {
		return errKey("has leading /")
	}
	if strings.HasSuffix(key, "/") {
		return errKey("has trailing /")
	}
	if strings.HasSuffix(key, ".meta") {
		return errKey("ends in .meta (reserved for localfs sidecars)")
	}
	if strings.Contains(key, "\\") {
		return errKey("contains backslash")
	}
	if strings.ContainsRune(key, 0) {
		return errKey("contains null byte")
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return errKey(fmt.Sprintf("contains forbidden segment %q", seg))
		}
	}
	return nil
}

func errKey(reason string) error {
	return fmt.Errorf("%w: key %s", storage.ErrInvalidArgument, reason)
}
```

- [ ] **Step 4: Run tests to verify pass**

```bash
go test ./internal/storage/localfs/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/localfs/keys.go internal/storage/localfs/keys_test.go
git commit -m "localfs: validate object keys

Rejects empty, leading/trailing slash, .. segments, null bytes,
backslashes, and keys >1024 bytes UTF-8. Floor matches the most
restrictive of S3/GCS/R2/Azure so localfs cannot accept a key cloud
adapters would later reject."
```

---

## Task 11: Localfs keyed mutex

**Files:**
- Create: `internal/storage/localfs/keyed_mutex.go`
- Create: `internal/storage/localfs/keyed_mutex_test.go`

- [ ] **Step 1: Write failing tests for the keyed mutex**

Write `internal/storage/localfs/keyed_mutex_test.go`:

```go
package localfs

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeyedMutexSerializesPerKey(t *testing.T) {
	km := newKeyedMutex()
	var counter int64
	var wg sync.WaitGroup
	const n = 32
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			km.lock("k1")
			defer km.unlock("k1")
			cur := atomic.AddInt64(&counter, 1)
			if cur != 1 {
				t.Errorf("concurrent holders for the same key: counter=%d", cur)
			}
			atomic.AddInt64(&counter, -1)
		}()
	}
	wg.Wait()
}

func TestKeyedMutexDifferentKeysIndependent(t *testing.T) {
	km := newKeyedMutex()
	km.lock("a")
	defer km.unlock("a")

	done := make(chan struct{})
	go func() {
		km.lock("b")
		km.unlock("b")
		close(done)
	}()
	select {
	case <-done:
		// ok — different keys did not block each other
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lock on different key (would have been a deadlock)")
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

```bash
go test ./internal/storage/localfs/...
```

Expected: FAIL — `newKeyedMutex`, `lock`, `unlock` undefined.

- [ ] **Step 3: Implement the keyed mutex**

Write `internal/storage/localfs/keyed_mutex.go`:

```go
package localfs

import "sync"

// keyedMutex is a map of mutexes keyed by string. lock/unlock provide
// per-key serialization without serializing across keys.
//
// The map itself is protected by an RWMutex: lock/unlock take the read
// lock to look up or create the per-key mutex. Map growth (creating a
// new entry) takes the write lock briefly. Entries are never removed in
// M0; if memory pressure becomes an issue in production we revisit
// eviction in M9.
type keyedMutex struct {
	mu    sync.RWMutex
	locks map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*sync.Mutex)}
}

func (km *keyedMutex) get(key string) *sync.Mutex {
	km.mu.RLock()
	m, ok := km.locks[key]
	km.mu.RUnlock()
	if ok {
		return m
	}
	km.mu.Lock()
	defer km.mu.Unlock()
	if m, ok := km.locks[key]; ok {
		return m
	}
	m = &sync.Mutex{}
	km.locks[key] = m
	return m
}

func (km *keyedMutex) lock(key string) {
	km.get(key).Lock()
}

func (km *keyedMutex) unlock(key string) {
	km.get(key).Unlock()
}
```

- [ ] **Step 4: Run tests with the race detector**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/localfs/keyed_mutex.go internal/storage/localfs/keyed_mutex_test.go
git commit -m "localfs: add per-key serialization via keyed mutex

map[string]*sync.Mutex protected by RWMutex over the map itself.
Different keys lock independently; same key serializes. M0 does not
evict idle entries (revisit at M9 if memory pressure surfaces)."
```

---

## Task 12: Localfs JSON sidecar

**Files:**
- Create: `internal/storage/localfs/meta.go`
- Create: `internal/storage/localfs/meta_test.go`

- [ ] **Step 1: Write failing test for sidecar round-trip**

Write `internal/storage/localfs/meta_test.go`:

```go
package localfs

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSidecarRoundTrip(t *testing.T) {
	in := sidecar{
		Version:     1,
		Sha256:      "deadbeef",
		Size:        1234,
		ContentType: "application/octet-stream",
		ModifiedAt:  time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out sidecar
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\nwant %+v\ngot  %+v", in, out)
	}
}

func TestSidecarRejectsUnknownVersion(t *testing.T) {
	b := []byte(`{"version":99,"sha256":"x","size":1,"content_type":"","modified_at":"2026-05-03T12:00:00Z"}`)
	if _, err := parseSidecar(b); err == nil {
		t.Error("parseSidecar accepted version=99, want error")
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

```bash
go test ./internal/storage/localfs/...
```

Expected: FAIL — `sidecar`, `parseSidecar` undefined.

- [ ] **Step 3: Implement the sidecar type**

Write `internal/storage/localfs/meta.go`:

```go
package localfs

import (
	"encoding/json"
	"fmt"
	"time"
)

// sidecar is the JSON-encoded metadata file written next to every
// localfs object: <root>/objects/<key>.meta. The Version field gates
// schema migrations; an unknown version causes parseSidecar to fail
// rather than guess.
type sidecar struct {
	Version     int       `json:"version"`
	Sha256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	ModifiedAt  time.Time `json:"modified_at"`
}

const sidecarSchemaVersion = 1

func newSidecar(sha256 string, size int64, contentType string, modifiedAt time.Time) sidecar {
	return sidecar{
		Version:     sidecarSchemaVersion,
		Sha256:      sha256,
		Size:        size,
		ContentType: contentType,
		ModifiedAt:  modifiedAt.UTC(),
	}
}

func encodeSidecar(s sidecar) ([]byte, error) {
	return json.Marshal(s)
}

func parseSidecar(data []byte) (sidecar, error) {
	var s sidecar
	if err := json.Unmarshal(data, &s); err != nil {
		return sidecar{}, fmt.Errorf("parseSidecar: %w", err)
	}
	if s.Version != sidecarSchemaVersion {
		return sidecar{}, fmt.Errorf("parseSidecar: unsupported schema version %d", s.Version)
	}
	return s, nil
}
```

- [ ] **Step 4: Run tests to verify pass**

```bash
go test ./internal/storage/localfs/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/localfs/meta.go internal/storage/localfs/meta_test.go
git commit -m "localfs: JSON sidecar type with versioned schema

Sidecar holds version, sha256, size, content-type, modified-at.
parseSidecar rejects unknown versions to keep schema migration honest."
```

---

## Task 13: Localfs Open/Close + lock file

**Files:**
- Modify: `internal/storage/localfs/localfs.go`
- Create: `internal/storage/localfs/localfs_test.go`

- [ ] **Step 1: Write failing test for the lock file**

Write `internal/storage/localfs/localfs_test.go`:

```go
package localfs_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestOpenLockFile(t *testing.T) {
	dir := t.TempDir()

	a, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if _, err := localfs.Open(dir); !errors.Is(err, localfs.ErrAlreadyLocked) {
		t.Errorf("second Open returned %v, want ErrAlreadyLocked", err)
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close (c): %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify failure**

```bash
go test ./internal/storage/localfs/...
```

Expected: FAIL — `ErrAlreadyLocked` undefined; second Open succeeds.

- [ ] **Step 3: Update localfs.go with Open/Close + lock file**

Replace `internal/storage/localfs/localfs.go` with:

```go
// Package localfs implements storage.ObjectStore over a regular local
// filesystem. It is intended for development, tests, and small
// self-hosted deployments. Localfs is single-process: holding two open
// Localfs instances against the same root directory in different
// processes is undefined.
package localfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ErrAlreadyLocked is returned by Open when another Localfs instance
// (in this process) already holds the root.
var ErrAlreadyLocked = errors.New("localfs: root is already locked by another instance")

const (
	objectsDir = "objects"
	uploadsDir = "uploads"
	lockFile   = ".lock"
	metaSuffix = ".meta"
)

// Localfs is the local-filesystem ObjectStore implementation.
type Localfs struct {
	root     string
	lock     *os.File
	mutexes  *keyedMutex
}

// Compile-time assertion that *Localfs satisfies storage.ObjectStore.
var _ storage.ObjectStore = (*Localfs)(nil)

// Open returns a Localfs rooted at the given directory. The directory
// is created if missing. Open holds a process-wide lockfile at
// <root>/.lock; a second Open against the same root returns
// ErrAlreadyLocked.
func Open(root string) (*Localfs, error) {
	if root == "" {
		return nil, errors.New("localfs: root must be non-empty")
	}
	if err := os.MkdirAll(filepath.Join(root, objectsDir), 0o755); err != nil {
		return nil, fmt.Errorf("localfs: mkdir objects: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, uploadsDir), 0o755); err != nil {
		return nil, fmt.Errorf("localfs: mkdir uploads: %w", err)
	}

	lockPath := filepath.Join(root, lockFile)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, ErrAlreadyLocked
		}
		return nil, fmt.Errorf("localfs: create lockfile: %w", err)
	}

	return &Localfs{
		root:    root,
		lock:    f,
		mutexes: newKeyedMutex(),
	}, nil
}

// Close releases the lockfile.
func (l *Localfs) Close() error {
	if l.lock == nil {
		return nil
	}
	closeErr := l.lock.Close()
	l.lock = nil
	rmErr := os.Remove(filepath.Join(l.root, lockFile))
	if closeErr != nil {
		return closeErr
	}
	return rmErr
}

func (l *Localfs) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		SignedURLs:           false,
		MultipartMinPartSize: 0,
		MultipartMaxParts:    0,
		MaxObjectSize:        0,
		StrongList:           true,
	}
}

func (l *Localfs) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrNotSupported
}

func (l *Localfs) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrNotSupported
}

func (l *Localfs) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	return storage.ErrNotSupported
}

func (l *Localfs) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) CreateMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) CompleteMultipartIfAbsent(ctx context.Context, upload storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrNotSupported
}

func (l *Localfs) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	return "", storage.ErrNotSupported
}
```

- [ ] **Step 4: Run tests to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS for `TestOpenLockFile` and existing tests.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/localfs/localfs.go internal/storage/localfs/localfs_test.go
git commit -m "localfs: Open/Close with process lockfile

Open creates objects/ and uploads/ directories and grabs a .lock file
via O_CREATE|O_EXCL. A second Open against the same root returns
ErrAlreadyLocked. Close removes the lockfile so subsequent Opens
succeed."
```

---

## Task 14: Localfs Put + Head + atomic-write pattern (conformance §29 #4)

**Files:**
- Modify: `internal/storage/localfs/localfs.go`
- Modify: `internal/storage/conformance/correctness.go`

- [ ] **Step 1: Write the failing conformance test for Put + Head**

Replace the contents of `internal/storage/conformance/correctness.go` with:

```go
package conformance

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// runCorrectness is the entry point for the §29 correctness tests. Each
// test corresponds to one numbered item in §29.
func runCorrectness(t *testing.T, f Factory) {
	t.Helper()
	t.Run("§29#4_PutThenGet_RAW", func(t *testing.T) { test29_4(t, f) })
}

// §29 #4: Read after write sees latest object.
func test29_4(t *testing.T, f Factory) {
	s := newStore(t, f)
	want := []byte("hello world")
	v, err := s.PutIfAbsent(ctx(), "rk/29-4", bytes.NewReader(want), nil)
	if err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}
	if v.Token == "" {
		t.Fatal("PutIfAbsent returned empty version token")
	}

	md, err := s.Head(ctx(), "rk/29-4")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Size != int64(len(want)) {
		t.Errorf("Head Size = %d, want %d", md.Size, len(want))
	}
	if md.Version != v {
		t.Errorf("Head Version = %+v, want %+v", md.Version, v)
	}

	obj, err := s.Get(ctx(), "rk/29-4", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get content = %q, want %q", got, want)
	}

	if _, err := s.Get(ctx(), "rk/missing", nil); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 2: Run the conformance test against localfs to verify failure**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: FAIL — `PutIfAbsent` returns `ErrNotSupported`.

- [ ] **Step 3: Implement Put + Head + Get on localfs**

Replace the `Get`, `Head`, `PutIfAbsent` methods in `internal/storage/localfs/localfs.go` and add helper functions. The `Open`, `Close`, `Capabilities` and other stubs stay unchanged.

The read paths (`Get`, `Head`, and `GetRange` in Task 15) acquire the per-key mutex per AD3 in the M0 design spec to eliminate the (content-N, sidecar-N-1) torn-read window. Read paths also reject symlinks per AD11.

Replace these three method bodies in `internal/storage/localfs/localfs.go`:

```go
func (l *Localfs) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	if err := lstatNoSymlink(l.objectPath(key)); err != nil {
		return nil, err
	}
	md, err := l.headLocked(key)
	if err != nil {
		return nil, err
	}
	if opts != nil && opts.IfVersionMatches != nil && opts.IfVersionMatches.Token != md.Version.Token {
		return nil, fmt.Errorf("%w: get if-version-matches", storage.ErrVersionMismatch)
	}
	f, err := os.Open(l.objectPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	// Note: f remains valid for the caller to read after we release
	// the keyed mutex on return. POSIX guarantees the inode stays
	// reachable through the open file descriptor even if a subsequent
	// writer renames a new file over the path.
	return &storage.Object{Body: f, Metadata: *md}, nil
}

func (l *Localfs) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	if err := lstatNoSymlink(l.objectPath(key)); err != nil {
		return nil, err
	}
	return l.headLocked(key)
}

func (l *Localfs) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return storage.ObjectVersion{}, err
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	objPath := l.objectPath(key)
	// rename(2) silently overwrites existing targets, so the absence
	// check must be performed under the same mutex held during the
	// atomic write below. Defense-in-depth on Linux would use
	// renameat2(RENAME_NOREPLACE), deferred.
	if _, err := os.Lstat(objPath); err == nil {
		return storage.ObjectVersion{}, storage.ErrAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return storage.ObjectVersion{}, err
	}

	contentType := ""
	if opts != nil {
		contentType = opts.ContentType
	}
	v, err := l.writeAtomic(key, body, contentType)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	return v, nil
}
```

Add helpers below the `Capabilities` method (anywhere outside the existing method bodies works; convention: at the end of the file):

```go
// objectPath returns the filesystem path for an object's content.
func (l *Localfs) objectPath(key string) string {
	return filepath.Join(l.root, objectsDir, filepath.FromSlash(key))
}

// metaPath returns the filesystem path for an object's sidecar.
func (l *Localfs) metaPath(key string) string {
	return l.objectPath(key) + metaSuffix
}

// lstatNoSymlink rejects symlinks under the bucket root per AD11.
// Returns ErrNotFound if the path does not exist, ErrInvalidArgument
// if it does and is a symlink, nil otherwise. Not TOCTOU-safe: an
// attacker who can write to the bucket can race symlink replacement
// against subsequent open calls. For the localfs dev/test threat model
// this is acceptable; full path-resolution sandboxing
// (openat2 RESOLVE_BENEATH) is deferred.
func lstatNoSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return storage.ErrNotFound
		}
		return err
	}
	if info.Mode().Type()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: path is a symlink (not allowed)", storage.ErrInvalidArgument)
	}
	return nil
}

// headLocked reads the sidecar (or self-heals from content if the
// sidecar is missing, unreadable, or stale relative to content) and
// returns metadata. Caller MUST hold l.mutexes for the key. Returns
// ErrNotFound if the object content does not exist.
//
// The "stale relative to content" check is a size-mismatch fast-path
// that catches the post-crash "content (new) + sidecar (old)" window
// when the new content has a different size from the old. Same-size
// post-crash torn states are NOT detected by this fast-path; operators
// must run bucketvcs doctor (M16) after unclean shutdown to fully
// reconcile. See the M0 design spec "Crash recovery and bucketvcs
// doctor" section.
func (l *Localfs) headLocked(key string) (*storage.ObjectMetadata, error) {
	contentInfo, err := os.Stat(l.objectPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}

	var sc sidecar
	scBytes, err := os.ReadFile(l.metaPath(key))
	if err == nil {
		sc, err = parseSidecar(scBytes)
	}
	if err == nil && sc.Size != contentInfo.Size() {
		// Stale sidecar: content size disagrees with sidecar's recorded
		// size. Most likely a crash mid-PutIfVersionMatches between
		// content rename and sidecar rename. Treat as if the sidecar is
		// missing.
		err = fmt.Errorf("sidecar size %d != content size %d (stale)", sc.Size, contentInfo.Size())
	}
	if err != nil {
		// Self-heal: recompute sha256 from content. Sidecar may be
		// missing, truncated, schema-incompatible, or stale relative
		// to content (size-mismatch fast-path).
		sc, err = l.healSidecar(key, contentInfo)
		if err != nil {
			return nil, err
		}
	}

	return &storage.ObjectMetadata{
		Key: key,
		Version: storage.ObjectVersion{
			Provider: "localfs",
			Token:    sc.Sha256,
			Kind:     storage.VersionEtag,
		},
		Size:        sc.Size,
		ContentType: sc.ContentType,
		ModifiedAt:  sc.ModifiedAt,
	}, nil
}

// head is the locking wrapper for headLocked. Used by callers that do
// NOT already hold the per-key mutex (notably List, which walks across
// keys and locks each one individually).
func (l *Localfs) head(key string) (*storage.ObjectMetadata, error) {
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)
	return l.headLocked(key)
}

// healSidecar recomputes a sidecar from content when the on-disk sidecar
// is missing or unreadable. Writes the new sidecar back so subsequent
// reads are fast. Caller holds the keyed mutex.
func (l *Localfs) healSidecar(key string, contentInfo os.FileInfo) (sidecar, error) {
	f, err := os.Open(l.objectPath(key))
	if err != nil {
		return sidecar{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sidecar{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	sc := newSidecar(sum, contentInfo.Size(), "", contentInfo.ModTime())
	scBytes, err := encodeSidecar(sc)
	if err != nil {
		return sidecar{}, err
	}
	if err := writeFileAtomic(l.metaPath(key), scBytes); err != nil {
		return sidecar{}, err
	}
	return sc, nil
}

// writeAtomic streams body to a temp file in the destination directory,
// hashes it as it goes, atomically renames into place, fsyncs the
// directory, then writes the sidecar via the same pattern. Caller holds
// the keyed mutex.
func (l *Localfs) writeAtomic(key string, body io.Reader, contentType string) (storage.ObjectVersion, error) {
	objPath := l.objectPath(key)
	objDir := filepath.Dir(objPath)
	if err := os.MkdirAll(objDir, 0o755); err != nil {
		return storage.ObjectVersion{}, err
	}
	tmp, err := os.CreateTemp(objDir, "."+filepath.Base(objPath)+".tmp.*")
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	h := sha256.New()
	tee := io.TeeReader(body, h)
	n, err := io.Copy(tmp, tee)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := os.Rename(tmpName, objPath); err != nil {
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := fsyncDir(objDir); err != nil {
		return storage.ObjectVersion{}, err
	}

	sum := hex.EncodeToString(h.Sum(nil))
	sc := newSidecar(sum, n, contentType, time.Now().UTC())
	scBytes, err := encodeSidecar(sc)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	if err := writeFileAtomic(l.metaPath(key), scBytes); err != nil {
		return storage.ObjectVersion{}, err
	}

	return storage.ObjectVersion{
		Provider: "localfs",
		Token:    sum,
		Kind:     storage.VersionEtag,
	}, nil
}

// writeFileAtomic writes data to path via temp + rename, then fsyncs
// the parent directory so the rename is durable across crashes.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return fsyncDir(dir)
}

// fsyncDir opens the directory and calls Sync to durably persist any
// rename or unlink that happened in it. POSIX requires this for crash
// durability of directory-entry changes; without it, a rename can be
// lost across a crash even though the file's own fsync succeeded.
func fsyncDir(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
```

Add the necessary imports at the top of `internal/storage/localfs/localfs.go`. Replace the existing import block with:

```go
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)
```

- [ ] **Step 4: Run conformance test to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS for `TestConformance/correctness/§29#4_PutThenGet_RAW`.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/localfs/localfs.go internal/storage/conformance/correctness.go
git commit -m "localfs: implement Put/Head/Get with atomic-write pattern

PutIfAbsent streams body to a temp file, sha256s as it goes, atomic-
renames into place, then writes the JSON sidecar via the same pattern.
Get/Head read the sidecar (or self-heal from content if missing).
Conformance §29 #4 (read-after-write) passes."
```

---

## Task 15: Localfs GetRange (conformance §29 #9)

**Files:**
- Modify: `internal/storage/localfs/localfs.go`
- Modify: `internal/storage/conformance/correctness.go`

- [ ] **Step 1: Append the failing conformance test for §29 #9**

In `internal/storage/conformance/correctness.go`, append to the body of `runCorrectness` after the existing `t.Run`:

```go
	t.Run("§29#9_GetRange", func(t *testing.T) { test29_9(t, f) })
```

And add the test function at the bottom of the file:

```go
// §29 #9: Range read returns exact bytes (and truncates to EOF when
// endInclusive exceeds the object size, mirroring HTTP semantics).
func test29_9(t *testing.T, f Factory) {
	s := newStore(t, f)
	const size = 1 << 20 // 1 MiB
	content := DeterministicBytes(size, "29-9")
	if _, err := s.PutIfAbsent(ctx(), "rk/29-9", bytes.NewReader(content), nil); err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}

	cases := []struct {
		start, end int64
	}{
		{0, 0},
		{0, 1023},
		{1024, 2047},
		{int64(size) - 1, int64(size) - 1},
		{int64(size) - 1024, int64(size) - 1},
	}
	for _, c := range cases {
		rc, err := s.GetRange(ctx(), "rk/29-9", c.start, c.end)
		if err != nil {
			t.Fatalf("GetRange[%d,%d]: %v", c.start, c.end, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("ReadAll[%d,%d]: %v", c.start, c.end, err)
		}
		want := content[c.start : c.end+1]
		if !bytes.Equal(got, want) {
			t.Errorf("GetRange[%d,%d] mismatch: got len=%d want len=%d", c.start, c.end, len(got), len(want))
		}
	}

	// Off-end: end exceeds content size; expect truncation to EOF.
	rc, err := s.GetRange(ctx(), "rk/29-9", int64(size-10), int64(size+1000))
	if err != nil {
		t.Fatalf("GetRange off-end: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if len(got) != 10 {
		t.Errorf("GetRange off-end returned %d bytes, want 10", len(got))
	}

	// Invalid: negative start.
	if _, err := s.GetRange(ctx(), "rk/29-9", -1, 5); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("GetRange(negative start) = %v, want ErrInvalidArgument", err)
	}

	// Missing key.
	if _, err := s.GetRange(ctx(), "rk/29-9-missing", 0, 0); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("GetRange(missing) = %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 2: Verify the test fails**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: FAIL — `GetRange` returns `ErrNotSupported`.

- [ ] **Step 3: Implement GetRange on localfs**

Replace the `GetRange` method in `internal/storage/localfs/localfs.go`. Per AD3 read paths acquire the per-key mutex; per AD11 they reject symlinks via `lstatNoSymlink`.

```go
func (l *Localfs) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if start < 0 || endInclusive < start {
		return nil, fmt.Errorf("%w: invalid range [%d,%d]", storage.ErrInvalidArgument, start, endInclusive)
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	if err := lstatNoSymlink(l.objectPath(key)); err != nil {
		return nil, err
	}
	f, err := os.Open(l.objectPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if start >= info.Size() {
		_ = f.Close()
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	end := endInclusive
	if end >= info.Size() {
		end = info.Size() - 1
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	// As with Get, the open file descriptor remains valid for the
	// caller after we release the keyed mutex on return.
	return &limitedReadCloser{Reader: io.LimitReader(f, end-start+1), Closer: f}, nil
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}
```

Add `"bytes"` to the imports if not already present.

- [ ] **Step 4: Run tests to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS for §29 #9.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/localfs/localfs.go internal/storage/conformance/correctness.go
git commit -m "localfs: implement GetRange with HTTP-style off-end truncation

GetRange returns exact bytes for in-range reads, truncates to EOF when
endInclusive exceeds object size, returns empty reader when start >=
size, and ErrInvalidArgument for negative or inverted ranges.
Conformance §29 #9 passes."
```

---

## Task 16: PutIfAbsent concurrency (conformance §29 #1, #14 recast)

**Files:**
- Modify: `internal/storage/conformance/correctness.go`

PutIfAbsent already implemented in Task 14. This task adds the concurrency tests.

- [ ] **Step 1: Append the §29 #1 and §29 #14 tests**

In `internal/storage/conformance/correctness.go`, add two `t.Run` lines to `runCorrectness`:

```go
	t.Run("§29#1_ConcurrentPutIfAbsent", func(t *testing.T) { test29_1(t, f) })
	t.Run("§29#14_PutIfAbsentIdempotentRetry", func(t *testing.T) { test29_14(t, f) })
```

Add the test functions at the end of the file:

```go
// §29 #1: Concurrent putIfAbsent same key — exactly one succeeds.
func test29_1(t *testing.T, f Factory) {
	s := newStore(t, f)
	const n = 64
	content := []byte("payload-29-1")
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := s.PutIfAbsent(ctx(), "rk/29-1", bytes.NewReader(content), nil)
			results <- err
		}()
	}
	successes, conflicts, others := 0, 0, 0
	for i := 0; i < n; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storage.ErrAlreadyExists):
			conflicts++
		default:
			others++
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1", successes)
	}
	if conflicts != n-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, n-1)
	}
}

// §29 #14 (recast per AD8): PutIfAbsent twice with the same args returns
// ErrAlreadyExists cleanly without corrupting state on the second call.
// See M0 design doc Architectural Decision 8.
func test29_14(t *testing.T, f Factory) {
	s := newStore(t, f)
	content := []byte("payload-29-14")
	v1, err := s.PutIfAbsent(ctx(), "rk/29-14", bytes.NewReader(content), nil)
	if err != nil {
		t.Fatalf("first PutIfAbsent: %v", err)
	}
	if _, err := s.PutIfAbsent(ctx(), "rk/29-14", bytes.NewReader(content), nil); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("second PutIfAbsent = %v, want ErrAlreadyExists", err)
	}

	md, err := s.Head(ctx(), "rk/29-14")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Version != v1 {
		t.Errorf("version mutated by failed second PutIfAbsent: got %+v, want %+v", md.Version, v1)
	}
}
```

- [ ] **Step 2: Run tests to verify they pass against current localfs**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS for both §29 #1 and §29 #14. (The keyed mutex from Task 11 + Stat-then-create semantics from Task 14 are already correct.)

- [ ] **Step 3: Commit**

```bash
git add internal/storage/conformance/correctness.go
git commit -m "conformance: add §29 #1 (concurrent PutIfAbsent) and #14 (idempotent retry, AD8 recast)

64-goroutine race on the same key asserts exactly one success and n-1
ErrAlreadyExists. Idempotent-retry test asserts second PutIfAbsent
with same args returns ErrAlreadyExists and does not mutate version."
```

---

## Task 17: PutIfVersionMatches (conformance §29 #2, #3, #5)

**Files:**
- Modify: `internal/storage/localfs/localfs.go`
- Modify: `internal/storage/conformance/correctness.go`

- [ ] **Step 1: Add failing conformance tests for §29 #2, #3, #5**

In `internal/storage/conformance/correctness.go`, add three `t.Run` lines to `runCorrectness`:

```go
	t.Run("§29#2_ConcurrentPutIfVersionMatches", func(t *testing.T) { test29_2(t, f) })
	t.Run("§29#3_FailedConditionalDoesNotAlter", func(t *testing.T) { test29_3(t, f) })
	t.Run("§29#5_OverwriteThenRead", func(t *testing.T) { test29_5(t, f) })
```

Add at the end of the file:

```go
// §29 #2: Concurrent putIfVersionMatches same key — exactly one succeeds.
func test29_2(t *testing.T, f Factory) {
	s := newStore(t, f)
	v0, err := s.PutIfAbsent(ctx(), "rk/29-2", bytes.NewReader([]byte("v0")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 64
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := s.PutIfVersionMatches(ctx(), "rk/29-2", v0, bytes.NewReader([]byte("v1")), nil)
			results <- err
		}()
	}
	successes, conflicts, others := 0, 0, 0
	for i := 0; i < n; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storage.ErrVersionMismatch):
			conflicts++
		default:
			others++
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1", successes)
	}
	if conflicts != n-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, n-1)
	}
}

// §29 #3: Failed conditional write does not alter object.
func test29_3(t *testing.T, f Factory) {
	s := newStore(t, f)
	want := []byte("original")
	v0, err := s.PutIfAbsent(ctx(), "rk/29-3", bytes.NewReader(want), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	bogus := storage.ObjectVersion{Provider: v0.Provider, Token: "deadbeef", Kind: v0.Kind}
	if _, err := s.PutIfVersionMatches(ctx(), "rk/29-3", bogus, bytes.NewReader([]byte("DROP")), nil); !errors.Is(err, storage.ErrVersionMismatch) {
		t.Errorf("PutIfVersionMatches(bogus) = %v, want ErrVersionMismatch", err)
	}

	obj, err := s.Get(ctx(), "rk/29-3", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if !bytes.Equal(got, want) {
		t.Errorf("content mutated by failed conditional: got %q, want %q", got, want)
	}
	if obj.Metadata.Version != v0 {
		t.Errorf("version mutated by failed conditional: got %+v, want %+v", obj.Metadata.Version, v0)
	}
}

// §29 #5: Read after overwrite sees the latest object.
func test29_5(t *testing.T, f Factory) {
	s := newStore(t, f)
	v0, err := s.PutIfAbsent(ctx(), "rk/29-5", bytes.NewReader([]byte("v0")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	v1, err := s.PutIfVersionMatches(ctx(), "rk/29-5", v0, bytes.NewReader([]byte("v1-content")), nil)
	if err != nil {
		t.Fatalf("PutIfVersionMatches: %v", err)
	}
	if v1 == v0 {
		t.Error("version did not change after overwrite")
	}
	obj, err := s.Get(ctx(), "rk/29-5", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if string(got) != "v1-content" {
		t.Errorf("after overwrite content = %q, want %q", got, "v1-content")
	}
	if obj.Metadata.Version != v1 {
		t.Errorf("Metadata.Version = %+v, want %+v", obj.Metadata.Version, v1)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: FAIL — `PutIfVersionMatches` returns `ErrNotSupported`.

- [ ] **Step 3: Implement PutIfVersionMatches**

Replace the `PutIfVersionMatches` method in `internal/storage/localfs/localfs.go`:

```go
func (l *Localfs) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return storage.ObjectVersion{}, err
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	current, err := l.headLocked(key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return storage.ObjectVersion{}, fmt.Errorf("%w: object absent", storage.ErrVersionMismatch)
		}
		return storage.ObjectVersion{}, err
	}
	if current.Version.Token != expected.Token {
		return storage.ObjectVersion{}, fmt.Errorf("%w: have %s want %s", storage.ErrVersionMismatch, current.Version.Token, expected.Token)
	}

	contentType := ""
	if opts != nil {
		contentType = opts.ContentType
	}
	return l.writeAtomic(key, body, contentType)
}
```

- [ ] **Step 4: Run tests to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS for §29 #2, #3, #5.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/localfs/localfs.go internal/storage/conformance/correctness.go
git commit -m "localfs: implement PutIfVersionMatches under keyed mutex

Reads sidecar under lock, compares expected.Token, proceeds via the
same atomic-write pattern as PutIfAbsent if matched. ErrVersionMismatch
on skew or absent key. Conformance §29 #2, #3, #5 pass."
```

---

## Task 18: DeleteIfVersionMatches (conformance §29 #11)

**Files:**
- Modify: `internal/storage/localfs/localfs.go`
- Modify: `internal/storage/conformance/correctness.go`

- [ ] **Step 1: Add failing conformance test for §29 #11**

In `internal/storage/conformance/correctness.go`, add to `runCorrectness`:

```go
	t.Run("§29#11_DeleteIfVersionMatches", func(t *testing.T) { test29_11(t, f) })
```

Add at the end of the file:

```go
// §29 #11: DeleteIfVersionMatches fails if object changed.
func test29_11(t *testing.T, f Factory) {
	s := newStore(t, f)
	v0, err := s.PutIfAbsent(ctx(), "rk/29-11", bytes.NewReader([]byte("v0")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	v1, err := s.PutIfVersionMatches(ctx(), "rk/29-11", v0, bytes.NewReader([]byte("v1")), nil)
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	if err := s.DeleteIfVersionMatches(ctx(), "rk/29-11", v0); !errors.Is(err, storage.ErrVersionMismatch) {
		t.Errorf("DeleteIfVersionMatches(stale) = %v, want ErrVersionMismatch", err)
	}
	if _, err := s.Head(ctx(), "rk/29-11"); err != nil {
		t.Errorf("after failed delete, Head = %v, want nil", err)
	}

	if err := s.DeleteIfVersionMatches(ctx(), "rk/29-11", v1); err != nil {
		t.Errorf("DeleteIfVersionMatches(current) = %v, want nil", err)
	}
	if _, err := s.Head(ctx(), "rk/29-11"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("after delete, Head = %v, want ErrNotFound", err)
	}
	if err := s.DeleteIfVersionMatches(ctx(), "rk/29-11", v1); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("DeleteIfVersionMatches(absent) = %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: FAIL — `DeleteIfVersionMatches` returns `ErrNotSupported`.

- [ ] **Step 3: Implement DeleteIfVersionMatches**

Replace the `DeleteIfVersionMatches` method in `internal/storage/localfs/localfs.go`:

```go
func (l *Localfs) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	if err := validateKey(key); err != nil {
		return err
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	current, err := l.headLocked(key)
	if err != nil {
		return err // ErrNotFound or fs error
	}
	if current.Version.Token != expected.Token {
		return fmt.Errorf("%w: have %s want %s", storage.ErrVersionMismatch, current.Version.Token, expected.Token)
	}
	// Order: remove content first, then sidecar. A crash between the
	// two leaves "no content + orphan sidecar"; subsequent Head returns
	// ErrNotFound (correct outcome). The reverse order would leave
	// "content present + missing sidecar"; Head's self-heal would
	// regenerate the sidecar and the deleted object would resurrect.
	objPath := l.objectPath(key)
	objDir := filepath.Dir(objPath)
	if err := os.Remove(objPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return storage.ErrNotFound
		}
		return err
	}
	if err := fsyncDir(objDir); err != nil {
		return err
	}
	if err := os.Remove(l.metaPath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := fsyncDir(objDir); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/localfs/localfs.go internal/storage/conformance/correctness.go
git commit -m "localfs: implement DeleteIfVersionMatches with content-first removal

Reads sidecar under lock; ErrVersionMismatch on skew, ErrNotFound on
missing. Removes content first then sidecar so a crash mid-delete
leaves an orphan sidecar (subsequent Head returns ErrNotFound, the
correct outcome) rather than leaving content with no sidecar (which
self-heal would resurrect). fsyncDir after each remove for crash
durability. Conformance §29 #11 passes."
```

---

## Task 19: List with prefix + pagination (conformance §29 #6, #7)

**Files:**
- Modify: `internal/storage/localfs/localfs.go`
- Modify: `internal/storage/conformance/correctness.go`

- [ ] **Step 1: Add failing conformance tests for §29 #6 and #7**

In `internal/storage/conformance/correctness.go`, add to `runCorrectness`:

```go
	t.Run("§29#6_ListAfterWrite", func(t *testing.T) { test29_6(t, f) })
	t.Run("§29#7_ListAfterDelete", func(t *testing.T) { test29_7(t, f) })
	t.Run("ListPagination", func(t *testing.T) { testListPagination(t, f) })
```

Add at the end of the file:

```go
// §29 #6: List after write sees new object.
func test29_6(t *testing.T, f Factory) {
	s := newStore(t, f)
	for i := 0; i < 5; i++ {
		key := Key("p/29-6", i)
		if _, err := s.PutIfAbsent(ctx(), key, bytes.NewReader([]byte("x")), nil); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	page, err := s.List(ctx(), "p/29-6/", &storage.ListOptions{MaxKeys: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != 5 {
		t.Errorf("listed %d objects, want 5", len(page.Objects))
	}
}

// §29 #7: List after delete does not show deleted object.
func test29_7(t *testing.T, f Factory) {
	s := newStore(t, f)
	v, err := s.PutIfAbsent(ctx(), "p/29-7/a", bytes.NewReader([]byte("a")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.DeleteIfVersionMatches(ctx(), "p/29-7/a", v); err != nil {
		t.Fatalf("delete: %v", err)
	}
	page, err := s.List(ctx(), "p/29-7/", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, md := range page.Objects {
		if md.Key == "p/29-7/a" {
			t.Error("listed deleted object")
		}
	}
}

// Pagination: List returns at most MaxKeys; subsequent calls with
// NextToken cover the remainder; concatenation matches the full set.
func testListPagination(t *testing.T, f Factory) {
	s := newStore(t, f)
	const total = 25
	for i := 0; i < total; i++ {
		if _, err := s.PutIfAbsent(ctx(), Key("p/page", i), bytes.NewReader([]byte{byte(i)}), nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	got := map[string]bool{}
	token := ""
	for iter := 0; iter < 100; iter++ {
		page, err := s.List(ctx(), "p/page/", &storage.ListOptions{MaxKeys: 7, ContinuationToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, md := range page.Objects {
			if got[md.Key] {
				t.Errorf("duplicate key in pagination: %s", md.Key)
			}
			got[md.Key] = true
		}
		if page.NextToken == "" {
			break
		}
		if len(page.Objects) > 7 {
			t.Errorf("page returned %d objects, want <= 7", len(page.Objects))
		}
		token = page.NextToken
	}
	if len(got) != total {
		t.Errorf("paginated total = %d, want %d", len(got), total)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: FAIL — `List` returns `ErrNotSupported`.

- [ ] **Step 3: Implement List**

Replace the `List` method in `internal/storage/localfs/localfs.go`:

```go
func (l *Localfs) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	maxKeys := 1000
	delimiter := ""
	cont := ""
	if opts != nil {
		if opts.MaxKeys > 0 {
			maxKeys = opts.MaxKeys
		}
		delimiter = opts.Delimiter
		cont = opts.ContinuationToken
	}

	keys, err := l.collectKeys(prefix)
	if err != nil {
		return nil, err
	}
	// Filter to keys strictly greater than the continuation token, if any.
	if cont != "" {
		idx := sort.SearchStrings(keys, cont)
		// Skip the cont key itself (token is the last key returned).
		for idx < len(keys) && keys[idx] <= cont {
			idx++
		}
		keys = keys[idx:]
	}

	page := &storage.ListPage{}
	commonSeen := map[string]bool{}
	for _, k := range keys {
		if delimiter != "" {
			rest := strings.TrimPrefix(k, prefix)
			if i := strings.Index(rest, delimiter); i >= 0 {
				cp := prefix + rest[:i+len(delimiter)]
				if !commonSeen[cp] {
					commonSeen[cp] = true
					page.CommonPrefixes = append(page.CommonPrefixes, cp)
				}
				continue
			}
		}
		md, err := l.head(k)
		if err != nil {
			return nil, err
		}
		page.Objects = append(page.Objects, *md)
		if len(page.Objects)+len(page.CommonPrefixes) >= maxKeys {
			page.NextToken = k
			return page, nil
		}
	}
	return page, nil
}

// collectKeys walks the objects directory under prefix and returns
// matching keys in lexicographic order. Sidecar files are excluded.
func (l *Localfs) collectKeys(prefix string) ([]string, error) {
	root := filepath.Join(l.root, objectsDir)
	prefixFs := filepath.FromSlash(prefix)
	walkRoot := root
	if prefixFs != "" {
		walkRoot = filepath.Join(root, prefixFs)
		// If walkRoot is a file (the prefix happens to be a complete key),
		// list the parent directory and filter by prefix.
		info, err := os.Stat(walkRoot)
		if err != nil || !info.IsDir() {
			walkRoot = root
		}
	}

	var keys []string
	err := filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip symlinks per AD11. filepath.WalkDir does not follow
		// symlinks; d.Type() reports ModeSymlink for them.
		if d.Type()&fs.ModeSymlink != 0 {
			// TODO: structured warning log when M3 logging framework lands.
			return nil
		}
		if strings.HasSuffix(path, metaSuffix) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}
```

Add `"sort"` and `"strings"` to the imports.

- [ ] **Step 4: Run tests to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS for §29 #6, §29 #7, and pagination.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/localfs/localfs.go internal/storage/conformance/correctness.go
git commit -m "localfs: implement List with prefix + pagination + delimiter

filepath.WalkDir filtered by prefix, sorted lexicographically. Excludes
.meta sidecars. ContinuationToken is the last-returned key; pagination
covers the full set without duplicates. Delimiter rolls common prefixes.
Conformance §29 #6, #7 and a pagination test pass."
```

---

## Task 20: List delimiter / common-prefixes test

**Files:**
- Modify: `internal/storage/conformance/correctness.go`

Implementation already done in Task 19; add a dedicated delimiter test.

- [ ] **Step 1: Add the delimiter test**

In `runCorrectness`, add:

```go
	t.Run("ListDelimiter", func(t *testing.T) { testListDelimiter(t, f) })
```

Add at the end of the file:

```go
func testListDelimiter(t *testing.T, f Factory) {
	s := newStore(t, f)
	keys := []string{
		"d/a/1",
		"d/a/2",
		"d/b/1",
		"d/c",
	}
	for _, k := range keys {
		if _, err := s.PutIfAbsent(ctx(), k, bytes.NewReader([]byte("x")), nil); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	page, err := s.List(ctx(), "d/", &storage.ListOptions{Delimiter: "/", MaxKeys: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantPrefixes := map[string]bool{"d/a/": true, "d/b/": true}
	gotPrefixes := map[string]bool{}
	for _, p := range page.CommonPrefixes {
		gotPrefixes[p] = true
	}
	for p := range wantPrefixes {
		if !gotPrefixes[p] {
			t.Errorf("missing common prefix %q (got %v)", p, page.CommonPrefixes)
		}
	}
	wantObjs := map[string]bool{"d/c": true}
	gotObjs := map[string]bool{}
	for _, md := range page.Objects {
		gotObjs[md.Key] = true
	}
	for k := range wantObjs {
		if !gotObjs[k] {
			t.Errorf("missing direct object %q (got %v)", k, page.Objects)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/conformance/correctness.go
git commit -m "conformance: list-delimiter test verifies common-prefix rollup"
```

---

## Task 21: Multipart upload happy path

**Files:**
- Create: `internal/storage/localfs/multipart.go`
- Modify: `internal/storage/localfs/localfs.go`
- Modify: `internal/storage/conformance/correctness.go`

- [ ] **Step 1: Add the failing multipart conformance test**

In `runCorrectness`, add:

```go
	t.Run("MultipartHappyPath", func(t *testing.T) { testMultipartHappyPath(t, f) })
```

Add at the end of the file:

```go
func testMultipartHappyPath(t *testing.T, f Factory) {
	s := newStore(t, f)
	const partSize = 1 << 16 // 64 KiB
	const numParts = 4
	full := DeterministicBytes(partSize*numParts, "multi-happy")

	mp, err := s.CreateMultipart(ctx(), "rk/multi-happy", &storage.MultipartOptions{ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if mp.UploadID() == "" {
		t.Error("UploadID empty")
	}
	if mp.Key() != "rk/multi-happy" {
		t.Errorf("Key = %q, want %q", mp.Key(), "rk/multi-happy")
	}

	parts := make([]storage.MultipartPart, 0, numParts)
	for i := 0; i < numParts; i++ {
		chunk := full[i*partSize : (i+1)*partSize]
		p, err := mp.UploadPart(ctx(), i+1, bytes.NewReader(chunk))
		if err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
		parts = append(parts, p)
	}

	v, err := s.CompleteMultipartIfAbsent(ctx(), mp, parts)
	if err != nil {
		t.Fatalf("CompleteMultipartIfAbsent: %v", err)
	}
	if v.Token == "" {
		t.Error("complete returned empty version token")
	}

	obj, err := s.Get(ctx(), "rk/multi-happy", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if !bytes.Equal(got, full) {
		t.Errorf("multipart content mismatch: got len=%d want len=%d", len(got), len(full))
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: FAIL — `CreateMultipart` returns `ErrNotSupported`.

- [ ] **Step 3: Add the UUID dependency**

Run:

```bash
go get github.com/google/uuid@v1.6.0
go mod tidy
```

Expected: `go.mod` lists `github.com/google/uuid v1.6.0` and `go.sum` is populated.

- [ ] **Step 4: Implement multipart**

Write `internal/storage/localfs/multipart.go`:

```go
package localfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/google/uuid"
)

// uploadManifest is the JSON record persisted in the upload directory so
// CompleteMultipartIfAbsent can validate the target key was the same one
// the caller used at CreateMultipart time.
type uploadManifest struct {
	Version     int       `json:"version"`
	UploadID    string    `json:"upload_id"`
	Key         string    `json:"key"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
}

const uploadManifestVersion = 1

// localfsUpload is the MultipartUpload returned by Localfs.CreateMultipart.
type localfsUpload struct {
	parent      *Localfs
	uploadID    string
	key         string
	contentType string
	dir         string // <root>/uploads/<id>
}

func (u *localfsUpload) UploadID() string { return u.uploadID }
func (u *localfsUpload) Key() string      { return u.key }

func (u *localfsUpload) UploadPart(ctx context.Context, partNumber int, body io.Reader) (storage.MultipartPart, error) {
	if partNumber < 1 {
		return storage.MultipartPart{}, fmt.Errorf("%w: partNumber must be >= 1", storage.ErrInvalidArgument)
	}
	partsDir := filepath.Join(u.dir, "parts")
	if err := os.MkdirAll(partsDir, 0o755); err != nil {
		return storage.MultipartPart{}, err
	}
	partPath := filepath.Join(partsDir, fmt.Sprintf("%05d", partNumber))
	tmp, err := os.CreateTemp(partsDir, fmt.Sprintf(".%05d.tmp.*", partNumber))
	if err != nil {
		return storage.MultipartPart{}, err
	}
	tmpName := tmp.Name()
	h := sha256.New()
	tee := io.TeeReader(body, h)
	n, err := io.Copy(tmp, tee)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return storage.MultipartPart{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return storage.MultipartPart{}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return storage.MultipartPart{}, err
	}
	if err := os.Rename(tmpName, partPath); err != nil {
		_ = os.Remove(tmpName)
		return storage.MultipartPart{}, err
	}
	return storage.MultipartPart{
		PartNumber: partNumber,
		Token:      hex.EncodeToString(h.Sum(nil)),
		Size:       n,
	}, nil
}

func (u *localfsUpload) Abort(ctx context.Context) error {
	return os.RemoveAll(u.dir)
}

// CreateMultipart begins a multipart upload, creating the upload
// directory and writing its manifest.
func (l *Localfs) CreateMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	dir := filepath.Join(l.root, uploadsDir, id)
	if err := os.MkdirAll(filepath.Join(dir, "parts"), 0o755); err != nil {
		return nil, err
	}
	contentType := ""
	if opts != nil {
		contentType = opts.ContentType
	}
	manifest := uploadManifest{
		Version:     uploadManifestVersion,
		UploadID:    id,
		Key:         key,
		ContentType: contentType,
		CreatedAt:   time.Now().UTC(),
	}
	mb, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, "manifest.json"), mb); err != nil {
		return nil, err
	}
	return &localfsUpload{
		parent:      l,
		uploadID:    id,
		key:         key,
		contentType: contentType,
		dir:         dir,
	}, nil
}

// CompleteMultipartIfAbsent assembles parts in order and atomically
// promotes them to the target key, only if the target does not already
// exist. Returns ErrAlreadyExists otherwise.
func (l *Localfs) CompleteMultipartIfAbsent(ctx context.Context, upload storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	u, ok := upload.(*localfsUpload)
	if !ok {
		return storage.ObjectVersion{}, fmt.Errorf("%w: upload not from this adapter", storage.ErrInvalidArgument)
	}
	if u.parent != l {
		return storage.ObjectVersion{}, fmt.Errorf("%w: upload not from this Localfs instance", storage.ErrInvalidArgument)
	}
	if len(parts) == 0 {
		return storage.ObjectVersion{}, fmt.Errorf("%w: no parts", storage.ErrInvalidArgument)
	}
	for i, p := range parts {
		if p.PartNumber != i+1 {
			return storage.ObjectVersion{}, fmt.Errorf("%w: parts not contiguously numbered (parts[%d].PartNumber=%d)", storage.ErrInvalidArgument, i, p.PartNumber)
		}
	}

	l.mutexes.lock(u.key)
	defer l.mutexes.unlock(u.key)

	if _, err := os.Stat(l.objectPath(u.key)); err == nil {
		return storage.ObjectVersion{}, storage.ErrAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return storage.ObjectVersion{}, err
	}

	objPath := l.objectPath(u.key)
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		return storage.ObjectVersion{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(objPath), "."+filepath.Base(objPath)+".tmp.*")
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	h := sha256.New()
	var total int64
	partsDir := filepath.Join(u.dir, "parts")
	for _, p := range parts {
		partPath := filepath.Join(partsDir, fmt.Sprintf("%05d", p.PartNumber))
		f, err := os.Open(partPath)
		if err != nil {
			_ = tmp.Close()
			cleanup()
			return storage.ObjectVersion{}, err
		}
		tee := io.TeeReader(f, h)
		n, err := io.Copy(tmp, tee)
		_ = f.Close()
		if err != nil {
			_ = tmp.Close()
			cleanup()
			return storage.ObjectVersion{}, err
		}
		if n != p.Size {
			_ = tmp.Close()
			cleanup()
			return storage.ObjectVersion{}, fmt.Errorf("%w: part %d size mismatch (manifest=%d, on-disk=%d)", storage.ErrInvalidArgument, p.PartNumber, p.Size, n)
		}
		total += n
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := os.Rename(tmpName, objPath); err != nil {
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := fsyncDir(filepath.Dir(objPath)); err != nil {
		return storage.ObjectVersion{}, err
	}

	sum := hex.EncodeToString(h.Sum(nil))
	sc := newSidecar(sum, total, u.contentType, time.Now().UTC())
	scBytes, err := encodeSidecar(sc)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	if err := writeFileAtomic(l.metaPath(u.key), scBytes); err != nil {
		return storage.ObjectVersion{}, err
	}

	if err := os.RemoveAll(u.dir); err != nil {
		// Non-fatal: the object is committed; the upload dir leak is a
		// gc concern, not a correctness one.
		_ = err
	}

	return storage.ObjectVersion{
		Provider: "localfs",
		Token:    sum,
		Kind:     storage.VersionEtag,
	}, nil
}
```

- [ ] **Step 5: Remove the old stub methods**

Delete the existing `CreateMultipart` and `CompleteMultipartIfAbsent` stub methods from `internal/storage/localfs/localfs.go` (the multipart.go file now provides them).

- [ ] **Step 6: Run tests to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS for `MultipartHappyPath`.

- [ ] **Step 7: Commit**

```bash
git add internal/storage/localfs/multipart.go internal/storage/localfs/localfs.go internal/storage/conformance/correctness.go go.mod go.sum
git commit -m "localfs: implement multipart upload (create, upload-part, complete)

CreateMultipart writes an upload manifest under <root>/uploads/<id>/.
UploadPart streams part bytes to parts/NNNNN under temp+rename and
returns the part sha256 as its token. CompleteMultipartIfAbsent
concatenates parts in order under the keyed mutex, atomically promotes
the result to the target key, and refuses with ErrAlreadyExists if the
key already exists. Adds google/uuid dep for upload IDs."
```

---

## Task 22: Multipart-vs-existing-key conflict (conformance §29 #8)

**Files:**
- Modify: `internal/storage/conformance/correctness.go`

Behavior already implemented in Task 21. This task adds the conformance test.

- [ ] **Step 1: Add the §29 #8 test**

In `runCorrectness`, add:

```go
	t.Run("§29#8_MultipartCannotOverwrite", func(t *testing.T) { test29_8(t, f) })
```

Add at the end of the file:

```go
// §29 #8: Multipart complete cannot silently overwrite existing object.
func test29_8(t *testing.T, f Factory) {
	s := newStore(t, f)
	original := []byte("original")
	v0, err := s.PutIfAbsent(ctx(), "rk/29-8", bytes.NewReader(original), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	mp, err := s.CreateMultipart(ctx(), "rk/29-8", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("DROP")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p}); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("Complete on existing key = %v, want ErrAlreadyExists", err)
	}

	obj, err := s.Get(ctx(), "rk/29-8", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if !bytes.Equal(got, original) {
		t.Errorf("original mutated: got %q, want %q", got, original)
	}
	if obj.Metadata.Version != v0 {
		t.Errorf("version mutated: got %+v, want %+v", obj.Metadata.Version, v0)
	}
}
```

- [ ] **Step 2: Run tests to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/conformance/correctness.go
git commit -m "conformance: §29 #8 — multipart complete refuses to overwrite

Pre-creates target key via PutIfAbsent, runs a single-part multipart
targeting same key, asserts CompleteMultipartIfAbsent returns
ErrAlreadyExists and original content + version unchanged."
```

---

## Task 23: Capabilities + SignedGetURL ErrNotSupported (conformance §29 #10)

**Files:**
- Modify: `internal/storage/conformance/correctness.go`

`Capabilities()` already returns the right values from Task 13. `SignedGetURL` already returns `ErrNotSupported`. This task adds the §29 #10 conformance test.

- [ ] **Step 1: Add the §29 #10 test**

In `runCorrectness`, add:

```go
	t.Run("§29#10_SignedURL", func(t *testing.T) { test29_10(t, f) })
```

Add at the end of the file:

```go
// §29 #10: Signed URL can read but cannot write. Adapters that do not
// support signed URLs declare so via Capabilities and return
// ErrNotSupported from SignedGetURL.
func test29_10(t *testing.T, f Factory) {
	s := newStore(t, f)
	caps := s.Capabilities()
	if !caps.SignedURLs {
		_, err := s.SignedGetURL(ctx(), "rk/29-10", storage.SignedURLOptions{Expires: 0, Method: "GET"})
		if !errors.Is(err, storage.ErrNotSupported) {
			t.Errorf("Capabilities.SignedURLs=false but SignedGetURL = %v, want ErrNotSupported", err)
		}
		return
	}
	t.Skip("adapter declares SignedURLs=true; full URL semantics tested by adapter-specific suite")
}
```

- [ ] **Step 2: Run tests to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/conformance/correctness.go
git commit -m "conformance: §29 #10 — Capabilities/SignedGetURL contract

If Capabilities.SignedURLs is false, SignedGetURL must return
ErrNotSupported. Cloud adapters declaring true exercise URL semantics
in their adapter-specific test suite."
```

---

## Task 24: Conformance tests §29 #12, #13, #15

**Files:**
- Modify: `internal/storage/conformance/correctness.go`

§29 #12 (version round-trip), #13 (CAS conflict classification), #15 (throttling — skip on localfs).

- [ ] **Step 1: Add the three tests**

In `runCorrectness`, add:

```go
	t.Run("§29#12_VersionRoundTrip", func(t *testing.T) { test29_12(t, f) })
	t.Run("§29#13_CASConflictClassification", func(t *testing.T) { test29_13(t, f) })
	t.Run("§29#15_ThrottlingClassification", func(t *testing.T) { test29_15(t, f) })
```

Add at the end of the file:

```go
// §29 #12: Metadata/version token round-trips Put → Head.
func test29_12(t *testing.T, f Factory) {
	s := newStore(t, f)
	v, err := s.PutIfAbsent(ctx(), "rk/29-12", bytes.NewReader([]byte("payload")), &storage.PutOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	md, err := s.Head(ctx(), "rk/29-12")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Version != v {
		t.Errorf("Head Version = %+v, want %+v", md.Version, v)
	}
	if md.Size != int64(len("payload")) {
		t.Errorf("Size = %d, want %d", md.Size, len("payload"))
	}
	if md.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want %q", md.ContentType, "text/plain")
	}
	if md.Key != "rk/29-12" {
		t.Errorf("Key = %q, want %q", md.Key, "rk/29-12")
	}
}

// §29 #13: CAS conflict error maps to normalized conflict type.
func test29_13(t *testing.T, f Factory) {
	s := newStore(t, f)
	if _, err := s.PutIfAbsent(ctx(), "rk/29-13", bytes.NewReader([]byte("a")), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := s.PutIfAbsent(ctx(), "rk/29-13", bytes.NewReader([]byte("b")), nil)
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("PutIfAbsent on existing = %v, want ErrAlreadyExists", err)
	}

	_, err = s.PutIfVersionMatches(ctx(), "rk/29-13", storage.ObjectVersion{Token: "deadbeef"}, bytes.NewReader([]byte("c")), nil)
	if !errors.Is(err, storage.ErrVersionMismatch) {
		t.Errorf("PutIfVersionMatches stale = %v, want ErrVersionMismatch", err)
	}
}

// §29 #15: Provider throttling errors are classified correctly.
// Localfs does not throttle. Cloud adapters override this skip.
func test29_15(t *testing.T, f Factory) {
	t.Skip("localfs has no throttling; cloud adapters at M5/M7 inject and assert ErrThrottled")
}
```

- [ ] **Step 2: Run tests to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS for §29 #12 and #13; SKIP for §29 #15.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/conformance/correctness.go
git commit -m "conformance: §29 #12 (version round-trip), #13 (CAS classification), #15 (throttling skip)

Round-trip asserts Put-returned ObjectVersion equals Head-returned
ObjectVersion plus matching size/content-type/key. Conflict
classification asserts errors.Is matches the documented sentinel.
§29 #15 skips on localfs (no throttling); cloud adapters inject and
override at M5/M7."
```

---

## Task 25: Conformance key namespace tests

**Files:**
- Modify: `internal/storage/conformance/correctness.go`

The key namespace tests are not §29-derived but are a baseline safety floor for any adapter.

- [ ] **Step 1: Add the key namespace test**

In `runCorrectness`, add:

```go
	t.Run("KeyNamespace", func(t *testing.T) { testKeyNamespace(t, f) })
```

Add at the end of the file:

```go
// Key namespace floor: invalid keys must be rejected with
// ErrInvalidArgument across the contract.
func testKeyNamespace(t *testing.T, f Factory) {
	s := newStore(t, f)
	invalid := []string{
		"",
		"/leading-slash",
		"trailing-slash/",
		"contains/../segment",
		"with\x00null",
		"with\\backslash",
	}
	for _, k := range invalid {
		if _, err := s.PutIfAbsent(ctx(), k, bytes.NewReader([]byte("x")), nil); !errors.Is(err, storage.ErrInvalidArgument) {
			t.Errorf("PutIfAbsent(%q) = %v, want ErrInvalidArgument", k, err)
		}
		if _, err := s.Head(ctx(), k); !errors.Is(err, storage.ErrInvalidArgument) {
			t.Errorf("Head(%q) = %v, want ErrInvalidArgument", k, err)
		}
		if err := s.DeleteIfVersionMatches(ctx(), k, storage.ObjectVersion{}); !errors.Is(err, storage.ErrInvalidArgument) {
			t.Errorf("DeleteIfVersionMatches(%q) = %v, want ErrInvalidArgument", k, err)
		}
	}

	// Valid keys at the floor should succeed.
	for _, k := range []string{"a", "a/b", "tenants/t1/repos/r1/manifest/root.json"} {
		if _, err := s.PutIfAbsent(ctx(), k, bytes.NewReader([]byte("x")), nil); err != nil {
			t.Errorf("PutIfAbsent(%q) returned %v, want nil", k, err)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify pass**

```bash
go test -race ./internal/storage/localfs/...
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/conformance/correctness.go
git commit -m "conformance: key namespace floor

Asserts every adapter rejects empty, leading/trailing slash, ..,
null bytes, and backslashes via ErrInvalidArgument across Put, Head,
and Delete. Valid-key smoke tests also exercise multi-segment paths
and Git-style key shapes."
```

---

## Task 26: Conformance correctness self-review

**Files:**
- (no code changes; verification only)

Verify all 15 §29 correctness items are covered.

- [ ] **Step 1: List the §29 items the suite covers**

The conformance suite must cover §29 items 1–15. Inventory:

| §29 # | Test name | Status |
|-------|-----------|--------|
| 1 | `§29#1_ConcurrentPutIfAbsent` | Task 16 |
| 2 | `§29#2_ConcurrentPutIfVersionMatches` | Task 17 |
| 3 | `§29#3_FailedConditionalDoesNotAlter` | Task 17 |
| 4 | `§29#4_PutThenGet_RAW` | Task 14 |
| 5 | `§29#5_OverwriteThenRead` | Task 17 |
| 6 | `§29#6_ListAfterWrite` | Task 19 |
| 7 | `§29#7_ListAfterDelete` | Task 19 |
| 8 | `§29#8_MultipartCannotOverwrite` | Task 22 |
| 9 | `§29#9_GetRange` | Task 15 |
| 10 | `§29#10_SignedURL` | Task 23 |
| 11 | `§29#11_DeleteIfVersionMatches` | Task 18 |
| 12 | `§29#12_VersionRoundTrip` | Task 24 |
| 13 | `§29#13_CASConflictClassification` | Task 24 |
| 14 | `§29#14_PutIfAbsentIdempotentRetry` (AD8 recast) | Task 16 |
| 15 | `§29#15_ThrottlingClassification` (skipped on localfs) | Task 24 |

- [ ] **Step 2: Run the full conformance suite to verify everything passes**

```bash
go test -race -v ./internal/storage/localfs/... -run TestConformance
```

Expected: every `§29#N_*` subtest reports PASS or SKIP. Inventory match the table above. If any §29 item is missing, return to its task and fix.

---

## Task 27: Stress test — 100 concurrent CAS attempts

**Files:**
- Modify: `internal/storage/conformance/stress.go`

- [ ] **Step 1: Add the stress test**

Replace `internal/storage/conformance/stress.go`:

```go
package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// runStress is the entry point for the §29 stress tests applicable to
// localfs in M0.
func runStress(t *testing.T, f Factory) {
	t.Helper()
	t.Run("Stress100ConcurrentCAS", func(t *testing.T) { stress100ConcurrentCAS(t, f) })
}

// §29 stress: 100 concurrent manifest CAS attempts.
//
// Models the manifest commit hot path. Many goroutines all read the
// current version then race to PutIfVersionMatches. Exactly one wins
// per round; the rest retry with the new version. After many rounds,
// every winner's CAS must have produced a strictly later version, and
// the final state matches the last winner.
func stress100ConcurrentCAS(t *testing.T, f Factory) {
	s := newStore(t, f)
	const writers = 100
	const rounds = 5
	const key = "stress/cas"

	v0, err := s.PutIfAbsent(ctx(), key, bytes.NewReader([]byte("init")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	current := v0

	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		results := make(chan struct {
			v   storage.ObjectVersion
			err error
		}, writers)
		expected := current
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				body := bytes.NewReader([]byte(fmt.Sprintf("round-%d-writer-%d", r, i)))
				v, err := s.PutIfVersionMatches(ctx(), key, expected, body, nil)
				results <- struct {
					v   storage.ObjectVersion
					err error
				}{v, err}
			}(i)
		}
		wg.Wait()
		close(results)

		successes, conflicts := 0, 0
		var winner storage.ObjectVersion
		for r := range results {
			if r.err == nil {
				successes++
				winner = r.v
			} else if errors.Is(r.err, storage.ErrVersionMismatch) {
				conflicts++
			} else {
				t.Errorf("unexpected error: %v", r.err)
			}
		}
		if successes != 1 {
			t.Fatalf("round %d: successes = %d, want 1", r, successes)
		}
		if conflicts != writers-1 {
			t.Fatalf("round %d: conflicts = %d, want %d", r, conflicts, writers-1)
		}
		current = winner
	}

	md, err := s.Head(ctx(), key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Version != current {
		t.Errorf("final Version = %+v, want %+v", md.Version, current)
	}
}
```

- [ ] **Step 2: Run the stress test**

```bash
go test -race -v ./internal/storage/localfs/... -run "TestConformance/stress/Stress100ConcurrentCAS"
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/conformance/stress.go
git commit -m "conformance: stress — 100 concurrent CAS attempts over multiple rounds

Models the manifest commit hot path. Five rounds × 100 writers each;
asserts exactly one winner per round and final state matches the last
winner."
```

---

## Task 28: Stress test — 10,000 small object creates

**Files:**
- Modify: `internal/storage/conformance/stress.go`

- [ ] **Step 1: Append the second stress test**

In `internal/storage/conformance/stress.go`, add to `runStress`:

```go
	t.Run("Stress10kCreates", func(t *testing.T) { stress10kCreates(t, f) })
```

Append at the end of the file:

```go
// §29 stress: 10,000 small object creates.
//
// Exercises listing pagination, keyed-mutex map growth, sidecar I/O
// rate. Each object is 16 bytes. After all creates, list every object
// across many pages and assert the count.
func stress10kCreates(t *testing.T, f Factory) {
	s := newStore(t, f)
	const total = 10_000
	const concurrency = 32

	work := make(chan int, concurrency)
	errs := make(chan error, total)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				body := DeterministicBytes(16, fmt.Sprintf("seed-%d", i))
				if _, err := s.PutIfAbsent(ctx(), Key("stress/10k", i), bytes.NewReader(body), nil); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for i := 0; i < total; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("PutIfAbsent: %v", err)
	}

	got := 0
	token := ""
	for iter := 0; iter < 10_000; iter++ {
		page, err := s.List(ctx(), "stress/10k/", &storage.ListOptions{MaxKeys: 1024, ContinuationToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		got += len(page.Objects)
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	if got != total {
		t.Errorf("listed %d objects, want %d", got, total)
	}
}
```

- [ ] **Step 2: Run the stress test**

```bash
go test -race -v ./internal/storage/localfs/... -run "TestConformance/stress/Stress10kCreates"
```

Expected: PASS. (May take 30 s — 60 s on slow disks.)

- [ ] **Step 3: Commit**

```bash
git add internal/storage/conformance/stress.go
git commit -m "conformance: stress — 10,000 small object creates

32 worker goroutines create 10,000 16-byte objects; full prefix
pagination then lists them and asserts the count."
```

---

## Task 29: Stress test — large multipart pack conflict

**Files:**
- Modify: `internal/storage/conformance/stress.go`

- [ ] **Step 1: Append the third stress test**

In `internal/storage/conformance/stress.go`, add to `runStress`:

```go
	t.Run("StressLargeMultipartConflict", func(t *testing.T) { stressLargeMultipartConflict(t, f) })
```

Append at the end of the file:

```go
// §29 stress: large multipart pack upload conflict.
//
// Pre-creates the target key, then runs a 256 MiB multipart upload
// targeting same key. Asserts CompleteMultipartIfAbsent returns
// ErrAlreadyExists and the original content + version are unchanged.
func stressLargeMultipartConflict(t *testing.T, f Factory) {
	s := newStore(t, f)
	const partSize = 32 * 1024 * 1024 // 32 MiB
	const numParts = 8                // 256 MiB total
	const key = "stress/large-multi"

	v0, err := s.PutIfAbsent(ctx(), key, bytes.NewReader([]byte("original-pack")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	mp, err := s.CreateMultipart(ctx(), key, nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	parts := make([]storage.MultipartPart, 0, numParts)
	for i := 0; i < numParts; i++ {
		chunk := DeterministicBytes(partSize, fmt.Sprintf("large-%d", i))
		p, err := mp.UploadPart(ctx(), i+1, bytes.NewReader(chunk))
		if err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
		parts = append(parts, p)
	}

	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, parts); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("Complete on existing key = %v, want ErrAlreadyExists", err)
	}

	md, err := s.Head(ctx(), key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Version != v0 {
		t.Errorf("version mutated by failed Complete: got %+v, want %+v", md.Version, v0)
	}
	if md.Size != int64(len("original-pack")) {
		t.Errorf("size mutated by failed Complete: got %d, want %d", md.Size, len("original-pack"))
	}
}
```

- [ ] **Step 2: Run the stress test**

```bash
go test -race -v ./internal/storage/localfs/... -run "TestConformance/stress/StressLargeMultipartConflict"
```

Expected: PASS. (May take 30 s — 90 s; allocates 256 MiB on disk.)

- [ ] **Step 3: Commit**

```bash
git add internal/storage/conformance/stress.go
git commit -m "conformance: stress — large multipart pack conflict

256 MiB across 8 × 32 MiB parts targeting a pre-existing key. Asserts
CompleteMultipartIfAbsent returns ErrAlreadyExists and the original
content + version are unchanged."
```

---

## Task 30: Sidecar self-heal localfs unit test

**Files:**
- Modify: `internal/storage/localfs/localfs_test.go`

Self-heal already implemented in Task 14's `headLocked` (sidecar missing/corrupt → recompute) and the size-mismatch fast-path added in plan r4 (sidecar size disagrees with content size → recompute). This task adds two localfs-specific unit tests for both self-heal triggers.

- [ ] **Step 1: Add the failing tests**

In `internal/storage/localfs/localfs_test.go`, append:

```go
func TestSidecarSelfHealMissing(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	want := []byte("self-heal-missing")
	v, err := s.PutIfAbsent(context.Background(), "rk/self-heal", bytes.NewReader(want), nil)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Delete the sidecar out of band.
	metaPath := filepath.Join(dir, "objects", "rk", "self-heal.meta")
	if err := os.Remove(metaPath); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	md, err := s.Head(context.Background(), "rk/self-heal")
	if err != nil {
		t.Fatalf("Head after sidecar removal: %v", err)
	}
	if md.Version.Token != v.Token {
		t.Errorf("self-heal recovered version = %s, want %s", md.Version.Token, v.Token)
	}
	if md.Size != int64(len(want)) {
		t.Errorf("self-heal recovered size = %d, want %d", md.Size, len(want))
	}

	// Sidecar should now exist again.
	if _, err := os.Stat(metaPath); err != nil {
		t.Errorf("sidecar not recreated: %v", err)
	}
}

// TestSidecarSelfHealSizeMismatch simulates the post-crash "content
// (new) + sidecar (old)" window: rewrite content out of band so its
// size differs from what the sidecar records, then call Head and
// assert the size-mismatch fast-path detects the staleness and
// regenerates the sidecar with sha256 of the new content.
func TestSidecarSelfHealSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.PutIfAbsent(context.Background(), "rk/torn", bytes.NewReader([]byte("aaa")), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Out-of-band rewrite content with a different size; sidecar still
	// records the original size. This simulates a crash mid-rewrite.
	objPath := filepath.Join(dir, "objects", "rk", "torn")
	newContent := []byte("BBBBBBBBBB")
	if err := os.WriteFile(objPath, newContent, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	md, err := s.Head(context.Background(), "rk/torn")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Size != int64(len(newContent)) {
		t.Errorf("size after self-heal = %d, want %d", md.Size, len(newContent))
	}
	// Token should be the sha256 of the NEW content, not the old.
	expectedHash := sha256.Sum256(newContent)
	want := hex.EncodeToString(expectedHash[:])
	if md.Version.Token != want {
		t.Errorf("token after self-heal = %s, want %s (sha256 of new content)", md.Version.Token, want)
	}
}
```

Add the imports `"bytes"`, `"context"`, `"crypto/sha256"`, `"encoding/hex"`, `"os"`, `"path/filepath"` to the existing imports if missing.

- [ ] **Step 2: Run the tests to verify pass**

```bash
go test -race ./internal/storage/localfs/... -run "TestSidecarSelfHeal"
```

Expected: PASS for both `TestSidecarSelfHealMissing` and `TestSidecarSelfHealSizeMismatch` — `headLocked()` self-heals on either trigger via Task 14's r4 implementation.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/localfs/localfs_test.go
git commit -m "localfs: unit tests for sidecar self-heal (missing + size-mismatch)

Missing-sidecar test removes the .meta file out of band and asserts
Head returns the correct version (recomputed from content sha256)
and rewrites the sidecar.

Size-mismatch test rewrites content out of band so the sidecar's
recorded size disagrees with the on-disk content; asserts the
size-mismatch fast-path triggers self-heal and the recovered token
is sha256 of the NEW content. This is the post-crash content-vs-
sidecar torn-state recovery path described in M0 design spec r4."
```

---

## Task 31: Worked example

**Files:**
- Create: `internal/storage/example_test.go`

A Go example demonstrating end-to-end localfs use. `Example_*()` functions are documented and executable.

- [ ] **Step 1: Create the example file**

Write `internal/storage/example_test.go`:

```go
package storage_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// Example_localfsLifecycle demonstrates the full ObjectStore contract on
// localfs: Put, Get, conditional update, conflict detection, multipart,
// list, and delete.
func Example_localfsLifecycle() {
	dir, err := os.MkdirTemp("", "bucketvcs-example-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	s, err := localfs.Open(filepath.Join(dir, "bucket"))
	if err != nil {
		panic(err)
	}
	defer s.Close()

	ctx := context.Background()
	key := "tenants/t1/repos/r1/manifest/root.json"

	// 1. Create-only PUT.
	v0, err := s.PutIfAbsent(ctx, key, bytes.NewReader([]byte(`{"version":1}`)), nil)
	if err != nil {
		panic(err)
	}
	fmt.Println("created v0")

	// 2. Read-after-write.
	obj, err := s.Get(ctx, key, nil)
	if err != nil {
		panic(err)
	}
	body, _ := io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	fmt.Printf("read: %s\n", body)

	// 3. Conditional update against current version.
	v1, err := s.PutIfVersionMatches(ctx, key, v0, bytes.NewReader([]byte(`{"version":2}`)), nil)
	if err != nil {
		panic(err)
	}
	fmt.Println("updated v0 -> v1")

	// 4. Stale CAS rejected.
	_, err = s.PutIfVersionMatches(ctx, key, v0, bytes.NewReader([]byte(`{"version":3}`)), nil)
	if errors.Is(err, storage.ErrVersionMismatch) {
		fmt.Println("stale CAS rejected with ErrVersionMismatch")
	}

	// 5. Multipart upload to a different key.
	mp, err := s.CreateMultipart(ctx, "tenants/t1/repos/r1/packs/canonical/sha256-pack.pack", nil)
	if err != nil {
		panic(err)
	}
	p1, _ := mp.UploadPart(ctx, 1, bytes.NewReader([]byte("part-1")))
	p2, _ := mp.UploadPart(ctx, 2, bytes.NewReader([]byte("part-2")))
	if _, err := s.CompleteMultipartIfAbsent(ctx, mp, []storage.MultipartPart{p1, p2}); err != nil {
		panic(err)
	}
	fmt.Println("multipart pack assembled")

	// 6. Listing.
	page, err := s.List(ctx, "tenants/t1/repos/r1/", &storage.ListOptions{MaxKeys: 100})
	if err != nil {
		panic(err)
	}
	fmt.Printf("listed %d objects\n", len(page.Objects))

	// 7. Conditional delete.
	if err := s.DeleteIfVersionMatches(ctx, key, v1); err != nil {
		panic(err)
	}
	fmt.Println("deleted manifest at v1")

	// Output:
	// created v0
	// read: {"version":1}
	// updated v0 -> v1
	// stale CAS rejected with ErrVersionMismatch
	// multipart pack assembled
	// listed 2 objects
	// deleted manifest at v1
}
```

- [ ] **Step 2: Run the example**

```bash
go test -run Example_localfsLifecycle ./internal/storage/...
```

Expected: PASS — the `// Output:` block matches actual stdout.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/example_test.go
git commit -m "storage: worked example demonstrating full ObjectStore contract on localfs

Put, Get, conditional update, stale-CAS rejection, multipart assembly,
prefix listing, conditional delete. Doubles as an executable Go doc
example."
```

---

## Task 32: README

**Files:**
- Create: `internal/storage/README.md`

- [ ] **Step 1: Write the README**

Write `internal/storage/README.md`:

````markdown
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

## Crash recovery and `bucketvcs doctor` — verbatim from M0 design spec r4

Localfs's stale-sidecar fast-path (size-mismatch detection on read)
catches the common case where a crashed `PutIfVersionMatches` left
content of a different size than the previous version. It does NOT
catch the rarer case where the new and old content happen to share
the same size. To preserve CAS correctness across crashes:

> **After unclean shutdown (`kill -9`, OOM, host crash, etc.),
> localfs MUST be re-validated by `bucketvcs doctor` (M16) before
> write traffic resumes.** Doctor walks every object under
> `<root>/objects/`, recomputes sha256 from content, and rewrites
> stale sidecars. After doctor completes successfully, the (content,
> sidecar) pair is guaranteed consistent and CAS semantics are fully
> restored.

Operational guidance:

- **Clean shutdown**: a graceful `Close` on the `Localfs` instance
  does not require doctor. Subsequent opens are safe.
- **Unclean shutdown without doctor**: read traffic is allowed
  (size-mismatch fast-path catches most stale sidecars).
  Conditional writes against an undetected stale sidecar may
  operate on the wrong version, breaking CAS. Operators MUST treat
  this as data loss risk for any keys touched during the crashed
  write.
- **Doctor is M16 work, not M0**: until M16 ships, localfs M0
  deployments must be considered ephemeral; production-style
  operators should keep an off-bucket replica as recourse.

This requirement applies to localfs only. Cloud adapters at M5/M7
do not have an equivalent torn-state because their version tokens
are server-side.

## Module path placeholder

`github.com/bucketvcs/bucketvcs` is a placeholder pending governance
gate G1 (license + repo host). Substitute the real path once G1 is
settled; the contract and behavior are unchanged.
````

- [ ] **Step 2: Verify the README renders cleanly**

Open the file in any markdown viewer or paste into GitHub Issues preview to check tables and code fences. (No automated check.)

- [ ] **Step 3: Commit**

```bash
git add internal/storage/README.md
git commit -m "storage: README documenting interface, adapter contract, conformance suite

Covers contract summary, how to add a new adapter, how to run the
conformance suite, the §29 test mapping table, AD8 recast rationale,
localfs caveats, and the module path placeholder pending governance
gate G1."
```

---

## Task 33: Multipart lifecycle edge-case conformance tests

**Files:**
- Modify: `internal/storage/conformance/correctness.go`

Codifies the multipart lifecycle reference table from the M0 design spec into executable tests. Localfs already implements the behavior from Tasks 21 and 22; these tests pin it down for cloud adapters at M5/M7.

- [ ] **Step 1: Add the lifecycle test runs to `runCorrectness`**

In `internal/storage/conformance/correctness.go`, add to `runCorrectness`:

```go
	t.Run("MultipartInvalidPartNumber", func(t *testing.T) { testMultipartInvalidPartNumber(t, f) })
	t.Run("MultipartRepeatedPartNumber", func(t *testing.T) { testMultipartRepeatedPartNumber(t, f) })
	t.Run("MultipartCompleteEmptyParts", func(t *testing.T) { testMultipartCompleteEmptyParts(t, f) })
	t.Run("MultipartCompleteNonContiguous", func(t *testing.T) { testMultipartCompleteNonContiguous(t, f) })
	t.Run("MultipartCompleteSizeMismatch", func(t *testing.T) { testMultipartCompleteSizeMismatch(t, f) })
	t.Run("MultipartConcurrentComplete", func(t *testing.T) { testMultipartConcurrentComplete(t, f) })
	t.Run("MultipartAbortIdempotent", func(t *testing.T) { testMultipartAbortIdempotent(t, f) })
	t.Run("MultipartCompleteAfterAbort", func(t *testing.T) { testMultipartCompleteAfterAbort(t, f) })
```

Add the test functions at the end of the file:

```go
// MultipartInvalidPartNumber: UploadPart with partNumber < 1 returns
// ErrInvalidArgument.
func testMultipartInvalidPartNumber(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-invalid", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if _, err := mp.UploadPart(ctx(), 0, bytes.NewReader([]byte("x"))); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("UploadPart(0) = %v, want ErrInvalidArgument", err)
	}
	if _, err := mp.UploadPart(ctx(), -1, bytes.NewReader([]byte("x"))); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("UploadPart(-1) = %v, want ErrInvalidArgument", err)
	}
	_ = mp.Abort(ctx())
}

// MultipartRepeatedPartNumber: uploading the same partNumber twice
// succeeds; Complete uses the second upload's bytes.
func testMultipartRepeatedPartNumber(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-repeat", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if _, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("first"))); err != nil {
		t.Fatalf("UploadPart 1 first: %v", err)
	}
	p1, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("SECOND")))
	if err != nil {
		t.Fatalf("UploadPart 1 second: %v", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p1}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	obj, err := s.Get(ctx(), "rk/multi-repeat", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if string(got) != "SECOND" {
		t.Errorf("content = %q, want %q (second upload should win)", got, "SECOND")
	}
}

// MultipartCompleteEmptyParts: Complete with empty parts slice returns
// ErrInvalidArgument.
func testMultipartCompleteEmptyParts(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-empty", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Complete(nil parts) = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Complete(empty parts) = %v, want ErrInvalidArgument", err)
	}
	_ = mp.Abort(ctx())
}

// MultipartCompleteNonContiguous: Complete with non-contiguous part
// numbers returns ErrInvalidArgument.
func testMultipartCompleteNonContiguous(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-noncontig", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p1, _ := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("a")))
	p3, _ := mp.UploadPart(ctx(), 3, bytes.NewReader([]byte("c")))
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p1, p3}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Complete([1,3]) = %v, want ErrInvalidArgument", err)
	}
	_ = mp.Abort(ctx())
}

// MultipartCompleteSizeMismatch: Complete with a parts entry whose Size
// differs from the on-disk part size returns ErrInvalidArgument.
func testMultipartCompleteSizeMismatch(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-sizemis", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p1, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("five.")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	p1.Size = p1.Size + 1 // claim wrong size
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p1}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Complete(size mismatch) = %v, want ErrInvalidArgument", err)
	}
	_ = mp.Abort(ctx())
}

// MultipartConcurrentComplete: two concurrent Complete calls on the
// same upload+target serialize via the per-key mutex; one wins, the
// other sees ErrAlreadyExists.
func testMultipartConcurrentComplete(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-conc", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p1, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}

	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p1})
			results <- err
		}()
	}
	successes, conflicts, others := 0, 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storage.ErrAlreadyExists):
			conflicts++
		default:
			others++
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1", successes)
	}
	if conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", conflicts)
	}
}

// MultipartAbortIdempotent: Abort after Complete is a no-op; Abort
// twice in a row is a no-op.
func testMultipartAbortIdempotent(t *testing.T, f Factory) {
	s := newStore(t, f)
	// Abort twice on a fresh upload.
	mp, err := s.CreateMultipart(ctx(), "rk/multi-abort1", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if err := mp.Abort(ctx()); err != nil {
		t.Errorf("Abort: %v", err)
	}
	if err := mp.Abort(ctx()); err != nil {
		t.Errorf("second Abort: %v", err)
	}

	// Abort after Complete.
	mp2, err := s.CreateMultipart(ctx(), "rk/multi-abort2", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p1, err := mp2.UploadPart(ctx(), 1, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp2, []storage.MultipartPart{p1}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := mp2.Abort(ctx()); err != nil {
		t.Errorf("Abort after Complete: %v", err)
	}
}

// MultipartCompleteAfterAbort: Complete after Abort fails (the part
// files are gone). The contract surface is "you may not call Complete
// after Abort"; the precise error is the wrapped underlying I/O error.
func testMultipartCompleteAfterAbort(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-cafterA", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p1, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if err := mp.Abort(ctx()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p1}); err == nil {
		t.Error("Complete after Abort returned nil, want non-nil error")
	}
}
```

- [ ] **Step 2: Run tests to verify pass**

```bash
go test -race -v ./internal/storage/localfs/... -run TestConformance/correctness
```

Expected: every new lifecycle test reports PASS. (Localfs already implements the behavior; these tests just make it explicit.)

- [ ] **Step 3: Commit**

```bash
git add internal/storage/conformance/correctness.go
git commit -m "conformance: multipart lifecycle edge-case tests

Codifies the M0 design spec multipart lifecycle reference table:
invalid part numbers, repeated/non-contiguous numbering,
empty/size-mismatch parts, concurrent Complete (per-key mutex
serializes), idempotent Abort, Complete after Abort. Cloud adapters
at M5/M7 will inherit this contract."
```

---

## Task 34: Localfs symlink-rejection test

**Files:**
- Modify: `internal/storage/localfs/localfs_test.go`

Localfs-specific (cloud adapters have no concept of symlinks). Asserts that symlinks placed under the bucket root are rejected on read per AD11 and skipped on List.

- [ ] **Step 1: Append the failing test**

In `internal/storage/localfs/localfs_test.go`, append:

```go
func TestSymlinkRejection(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Seed a normal object so the bucket has at least one valid entry.
	if _, err := s.PutIfAbsent(context.Background(), "rk/normal", bytes.NewReader([]byte("ok")), nil); err != nil {
		t.Fatalf("seed normal: %v", err)
	}

	// Place a symlink at <root>/objects/rk/symlinked pointing to /etc/hosts.
	target := "/etc/hosts"
	if _, err := os.Stat(target); err != nil {
		t.Skipf("test target %s not present: %v", target, err)
	}
	linkPath := filepath.Join(dir, "objects", "rk", "symlinked")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// Get/Head/GetRange must reject the symlinked key.
	if _, err := s.Get(context.Background(), "rk/symlinked", nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Get(symlink) = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.Head(context.Background(), "rk/symlinked"); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Head(symlink) = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.GetRange(context.Background(), "rk/symlinked", 0, 0); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("GetRange(symlink) = %v, want ErrInvalidArgument", err)
	}

	// List must skip the symlinked entry but still return the normal one.
	page, err := s.List(context.Background(), "rk/", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, md := range page.Objects {
		if md.Key == "rk/symlinked" {
			t.Error("List returned a symlinked key; expected it to be skipped")
		}
	}
	foundNormal := false
	for _, md := range page.Objects {
		if md.Key == "rk/normal" {
			foundNormal = true
		}
	}
	if !foundNormal {
		t.Error("List did not return the normal entry alongside the skipped symlink")
	}
}
```

Add the imports `"errors"` and `"github.com/bucketvcs/bucketvcs/internal/storage"` to the existing import block of `localfs_test.go` if missing. (`bytes`, `context`, `os`, `path/filepath`, `testing` should already be present from earlier tasks.)

- [ ] **Step 2: Run the test to verify pass**

```bash
go test -race ./internal/storage/localfs/... -run TestSymlinkRejection
```

Expected: PASS — `lstatNoSymlink` from Task 14 and the WalkDir symlink-skip from Task 19 already implement the behavior.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/localfs/localfs_test.go
git commit -m "localfs: symlink-rejection test (AD11)

Drops a symlink to /etc/hosts under <root>/objects/rk/ and asserts
Get/Head/GetRange return ErrInvalidArgument while List skips the
entry without disrupting the listing of the normal sibling."
```

---

## Final verification

- [ ] **Step 1: Run the full test suite**

```bash
go test -race ./...
```

Expected: every test passes. Stress tests may take 1–3 minutes total.

- [ ] **Step 2: Run go vet**

```bash
go vet ./...
```

Expected: silent.

- [ ] **Step 3: Run gofmt to verify formatting**

```bash
gofmt -l . | grep -v '^$' && echo "FORMATTING ISSUES" || echo "clean"
```

Expected: `clean`.

- [ ] **Step 4: Verify the exit-criteria checklist**

Confirm against the design spec r4 exit criteria:

1. `go test ./internal/storage/...` is green on Linux/macOS — verified Step 1
2. 15 §29 correctness tests pass — verified by Task 26 inventory
3. 3 applicable §29 stress tests pass — verified Tasks 27/28/29
4. Multipart lifecycle conformance tests (10 cases) pass — verified Tasks 21, 22, 33
5. Localfs symlink-rejection test passes — verified Task 34
6. Sidecar self-heal triggers (missing + size-mismatch) covered — verified Task 30
7. `Capabilities()` declarations round-trip — verified Task 23
8. Error taxonomy maps to conformance assertions — verified across §29 #13, #15, #10, key namespace; error normalization rules and `fsyncDir` failure semantics from spec r3/r4 implemented in Task 14 helpers
9. Package layout matches §40.1 — verified by file structure at top of plan
10. README documents contract, adding adapters, running conformance, AD8 recast, and includes the verbatim "Symlink and hardlink safety", "Filesystem portability assumptions", and "Crash recovery and `bucketvcs doctor`" sections — Task 32
11. Public Go-doc comments on every exported symbol in `internal/storage` — verified across Tasks 2–6
12. Worked example exercises Put/Get/PutIfVersionMatches/List/Multipart/Delete with success and conflict paths — Task 31

- [ ] **Step 5: Tag M0 milestone (optional)**

```bash
git tag -a m0-complete -m "M0 storage foundation complete: ObjectStore + localfs + conformance"
```

Do not push the tag without coordinating with whoever owns the public release path (governance gates G1–G3).
