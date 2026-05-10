# M9 — Background Maintenance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `bucketvcs maintenance`: an operator-driven CLI that runs a single full repack per repo, refreshes the commit-graph (`.bvcg`) and object-map (`.bvom`) indexes against the new pack, and CAS-merges the result into the root manifest while preserving concurrent-push packs.

**Architecture:** New top-level package `internal/maintenance/` (sibling of `internal/gc`, `internal/repo`). Pack production reuses `gitcli.PackObjectsAll` against a per-run temp bare repo materialized from current canonical packs (the importer's pattern from M2). Index building stays pure Go via `objindex.Build` and `commitgraph.Build`. The CAS-merge is delegated to `repo.Repo.Commit`'s built-in retry loop with a callback that produces a body merging `[new_pack] ++ (prev.Packs - P0)`. CLI subcommand `cmd/bucketvcs/maintenance.go` follows the existing M3+ subcommand pattern.

**Tech Stack:** Go 1.25, existing `internal/storage` ObjectStore contract from M0, existing `internal/repo` transaction kernel from M1, `internal/pack` reader from M2, `internal/objindex` and `internal/commitgraph` builders from M2, `internal/gitcli` shell-out helpers from M2, `log/slog` for structured logging (M3+ convention), `github.com/oklog/ulid/v2` for tx IDs (already in repo), no new external dependencies.

**Spec:** `docs/superpowers/specs/2026-05-10-m9-background-maintenance-design.md`

---

## File Structure

**New files:**

```
internal/maintenance/
  doc.go                       // package overview
  errs.go                      // sentinel errors (ErrInvalidFlags, ErrNoOp, ErrCASExhausted, ...)
  errs_test.go
  options.go                   // RunOptions, Thresholds, Defaults, Report
  options_test.go
  localpack.go                 // file-backed ObjectStore for a single local pack pair
  localpack_test.go
  refsfile.go                  // write packed-refs / HEAD / minimal config from manifest state
  refsfile_test.go
  materialize.go               // download P0 packs into <tmp>/bare.git; orchestrate refsfile + fsck
  materialize_test.go
  repack.go                    // gitcli.PackObjectsAll wrapper; tmpdir lifecycle
  repack_test.go
  indexes.go                   // buildIndexesFromPack (.bvom + .bvcg) — duplicated from importer
  indexes_test.go
  upload.go                    // PutIfAbsent of pack/idx/.bvom/.bvcg with importer's collision rules
  upload_test.go
  thresholds.go                // §15.3 trigger evaluation against a manifest body + pack mtimes
  thresholds_test.go
  casmerge.go                  // BuildBody callback and merge logic (pure function over manifest.Body)
  casmerge_test.go
  pipeline.go                  // orchestrates phases 0-7
  pipeline_test.go
  multirepo.go                 // DiscoverRepos + RunAll wrappers (mirrors internal/gc/multirepo.go)
  multirepo_test.go
  log.go                       // slog event builders for maintenance.started / .completed
  log_test.go
  metrics.go                   // §32 metric name constants + slog field helpers
  metrics_test.go
  run.go                       // top-level Run(ctx, store, k, opts) entry point
  run_test.go
  README.md                    // short package readme

internal/maintenance/conformance/
  safety.go                    // RunPropertyMaintenanceSafety(t, factory) — 4 interleavings
  safety_test.go               // localfs run of the property suite

internal/maintenance/mtest/
  fixtures.go                  // shared test fixtures: synthesizeRepo, materializedManifest, ObjectStoreRecorder

cmd/bucketvcs/
  maintenance.go               // CLI subcommand
  maintenance_test.go          // flag validation, exit codes, --dry-run, --force, --output=json

docs/
  m9-maintenance-operator-guide.md
```

**Modified files:**

```
cmd/bucketvcs/main.go          // wire "maintenance" subcommand into the dispatcher
docs/m5-cloud-quickstart.md    // one-line pointer to the M9 guide alongside the M8 multipart-cleanup section
docs/m7-cloud-quickstart.md    // same
README.md                      // add `maintenance` to the CLI surface table
internal/diffharness/diffharness_test.go  // add Import→Maintenance→Export equivalence test
```

**Note on §32 metrics:** the spec lists Prometheus-style metric names. The codebase still has no metrics framework wired (M8's verdict carries forward). M9 emits structured `slog` events that carry the metric name and value as fields (`metric_name=maintenance_runs_total outcome=success value=1 repo_id=...`). When a future metrics-scaffolding milestone lands, it can lift these fields out without touching M9 call sites.

---

## Phase 0 — Foundation: package skeleton, errors, options

This phase creates the empty `internal/maintenance/` package with sentinel errors, doc.go, and the data types used everywhere else. Nothing functional yet; subsequent phases fill in one file at a time.

### Task 0.1: Create package directory and doc.go

**Files:**
- Create: `internal/maintenance/doc.go`

- [ ] **Step 1: Create the package**

Create `internal/maintenance/doc.go`:

```go
// Package maintenance implements bucketvcs's M9 background-maintenance
// pipeline: a single full repack of canonical packs, fresh commit-graph
// (.bvcg) and object-map (.bvom) indexes against the new pack, and a
// CAS-merge that preserves concurrent push packs (§43.6 / §17).
//
// The pipeline is invoked from one-shot operator processes
// (cmd/bucketvcs/maintenance.go) — the package has no scheduler,
// daemon, or background goroutines of its own. Pack production uses
// gitcli.PackObjectsAll against a per-run temp bare repo materialized
// from current canonical packs; index building is pure Go.
//
// Composition with M8: M9 produces, M8 reclaims. Old canonical packs
// and stale indexes drop out of manifest.Packs / manifest.Indexes on
// CAS-merge; M8 GC sweeps them after retention.
package maintenance
```

- [ ] **Step 2: Verify the package compiles**

Run: `go build ./internal/maintenance/...`
Expected: no error (empty package compiles).

- [ ] **Step 3: Commit**

```bash
git add internal/maintenance/doc.go
git commit -m "M9 task 0.1: create internal/maintenance package skeleton"
```

### Task 0.2: Add sentinel errors

**Files:**
- Create: `internal/maintenance/errs.go`
- Create: `internal/maintenance/errs_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/errs_test.go`:

```go
package maintenance_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
)

func TestSentinelErrors_Distinct(t *testing.T) {
	all := []error{
		maintenance.ErrInvalidFlags,
		maintenance.ErrCASExhausted,
		maintenance.ErrPackCollision,
		maintenance.ErrCorruptInput,
		maintenance.ErrNoRefs,
	}
	seen := map[error]struct{}{}
	for _, e := range all {
		if e == nil {
			t.Fatalf("nil sentinel in list")
		}
		if _, dup := seen[e]; dup {
			t.Fatalf("duplicate sentinel: %v", e)
		}
		seen[e] = struct{}{}
	}
	// Sanity: errors.Is over the same value works.
	if !errors.Is(maintenance.ErrCASExhausted, maintenance.ErrCASExhausted) {
		t.Fatalf("errors.Is identity broken")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestSentinelErrors_Distinct -v`
Expected: FAIL with `undefined: maintenance.ErrInvalidFlags` (and friends).

- [ ] **Step 3: Implement sentinel errors**

Create `internal/maintenance/errs.go`:

```go
package maintenance

import "errors"

// ErrInvalidFlags is returned when RunOptions has mutually-exclusive or
// otherwise invalid combinations (e.g. repo and all-repos both set).
var ErrInvalidFlags = errors.New("maintenance: invalid flags")

// ErrCASExhausted is returned when the manifest CAS-merge loop exhausts
// its retry budget. The uploaded pack and indexes remain in the bucket
// and become orphan candidates for the next M8 GC run.
var ErrCASExhausted = errors.New("maintenance: cas retry budget exhausted")

// ErrPackCollision is returned when PutIfAbsent on the new canonical
// pack key fails because pre-existing bytes already occupy the key.
// The local .bvom encodes offsets for OUR pack bytes; committing a
// manifest whose .bvom expects our offsets but whose pack key resolves
// to different bytes would corrupt object lookup. Aborting is correct.
var ErrPackCollision = errors.New("maintenance: pack key collision against pre-existing bytes")

// ErrCorruptInput is returned when the materialized bare repo fails
// fsck — the source canonical packs (or the manifest's ref tips) are
// inconsistent with each other and the run cannot proceed safely.
var ErrCorruptInput = errors.New("maintenance: corrupt input (fsck failed)")

// ErrNoRefs is returned when the manifest has no refs at run start;
// there is nothing to repack. Treated as a no-op success at the CLI.
var ErrNoRefs = errors.New("maintenance: manifest has no refs")
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run TestSentinelErrors_Distinct -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/errs.go internal/maintenance/errs_test.go
git commit -m "M9 task 0.2: add sentinel errors for invalid flags / CAS / collision / corrupt / no-refs"
```

### Task 0.3: Add `RunOptions`, `Thresholds`, defaults, and `Report` types

**Files:**
- Create: `internal/maintenance/options.go`
- Create: `internal/maintenance/options_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/options_test.go`:

```go
package maintenance_test

import (
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
)

func TestThresholds_Defaults(t *testing.T) {
	d := maintenance.DefaultThresholds()
	if d.RecentPackCount != 1000 {
		t.Errorf("RecentPackCount default = %d, want 1000", d.RecentPackCount)
	}
	if d.TotalPackCount != 10000 {
		t.Errorf("TotalPackCount default = %d, want 10000", d.TotalPackCount)
	}
	if d.ManifestPackBytes != 8<<20 {
		t.Errorf("ManifestPackBytes default = %d, want %d", d.ManifestPackBytes, 8<<20)
	}
}

func TestRunOptions_NormalizeApplyDefaults(t *testing.T) {
	o := maintenance.RunOptions{}
	o.Normalize()
	if o.CASRetry != maintenance.DefaultCASRetry {
		t.Errorf("CASRetry default = %d, want %d", o.CASRetry, maintenance.DefaultCASRetry)
	}
	if o.RecentWindow != maintenance.DefaultRecentWindow {
		t.Errorf("RecentWindow default = %v, want %v", o.RecentWindow, maintenance.DefaultRecentWindow)
	}
	if o.Logger == nil {
		t.Errorf("Logger default = nil, want slog.Default()")
	}
	if o.Now == nil {
		t.Errorf("Now default = nil, want time.Now")
	}
}

func TestRunOptions_NormalizePreservesCallerValues(t *testing.T) {
	o := maintenance.RunOptions{
		CASRetry:     12,
		RecentWindow: 7 * time.Hour,
		Actor:        "u_test",
	}
	o.Normalize()
	if o.CASRetry != 12 {
		t.Errorf("CASRetry = %d, want 12 (caller value preserved)", o.CASRetry)
	}
	if o.RecentWindow != 7*time.Hour {
		t.Errorf("RecentWindow = %v, want 7h (caller value preserved)", o.RecentWindow)
	}
}

func TestRunOptions_ValidateRejectsSubHourWindow(t *testing.T) {
	o := maintenance.RunOptions{RecentWindow: 30 * time.Minute}
	o.Normalize()
	if err := o.Validate(); err == nil {
		t.Fatal("Validate accepted sub-1h RecentWindow; want error")
	}
}

func TestRunOptions_ValidateRejectsZeroCASRetry(t *testing.T) {
	o := maintenance.RunOptions{CASRetry: 0}
	o.Normalize()
	// Normalize bumps CASRetry to default, so this should now pass.
	if err := o.Validate(); err != nil {
		t.Fatalf("Validate after Normalize: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestThresholds_Defaults -v`
Expected: FAIL with `undefined: maintenance.DefaultThresholds`.

- [ ] **Step 3: Implement options.go**

Create `internal/maintenance/options.go`:

```go
package maintenance

import (
	"fmt"
	"log/slog"
	"time"
)

// Defaults for RunOptions / Thresholds. Exposed for tests and the CLI.
const (
	DefaultCASRetry     = 5
	DefaultRecentWindow = 24 * time.Hour
)

// Thresholds are the §15.3 force-repack triggers. A zero value disables
// that specific trigger; setting all to zero with !Force makes Run a
// no-op. Bitmap-coverage and lookup-latency triggers are intentionally
// omitted from M9 — they ship in their successor milestones.
type Thresholds struct {
	// RecentPackCount triggers when the count of canonical packs whose
	// object-store creation_time is within RecentWindow exceeds this.
	RecentPackCount int

	// TotalPackCount triggers when len(manifest.Packs) exceeds this.
	TotalPackCount int

	// ManifestPackBytes triggers when the JSON byte size of
	// manifest.Packs exceeds this.
	ManifestPackBytes int64
}

// DefaultThresholds returns the spec §15.3 recommended values.
func DefaultThresholds() Thresholds {
	return Thresholds{
		RecentPackCount:   1000,
		TotalPackCount:    10000,
		ManifestPackBytes: 8 << 20, // 8 MiB
	}
}

// RunOptions configures one Run invocation against one repo.
type RunOptions struct {
	Thresholds   Thresholds
	RecentWindow time.Duration // window for "recent" pack classification
	CASRetry     int           // bound on Phase 6 CAS-merge retries
	Force        bool          // skip threshold evaluation; always proceed
	DryRun       bool          // walk + plan + report; write nothing
	Actor        string        // tx record actor; "u_op" if empty
	Logger       *slog.Logger  // defaults to slog.Default()
	Now          func() time.Time
}

// Normalize fills in defaults for unset fields. Idempotent.
func (o *RunOptions) Normalize() {
	if o.Thresholds == (Thresholds{}) {
		o.Thresholds = DefaultThresholds()
	}
	if o.CASRetry <= 0 {
		o.CASRetry = DefaultCASRetry
	}
	if o.RecentWindow <= 0 {
		o.RecentWindow = DefaultRecentWindow
	}
	if o.Actor == "" {
		o.Actor = "u_op"
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Validate returns an error wrapped in ErrInvalidFlags if RunOptions is
// inconsistent. Call after Normalize.
func (o RunOptions) Validate() error {
	if o.RecentWindow < time.Hour {
		return fmt.Errorf("%w: RecentWindow=%s is below the 1h minimum",
			ErrInvalidFlags, o.RecentWindow)
	}
	if o.CASRetry < 1 {
		return fmt.Errorf("%w: CASRetry=%d must be >= 1",
			ErrInvalidFlags, o.CASRetry)
	}
	return nil
}

// Report summarizes one Run for the caller (CLI, future scheduler).
type Report struct {
	RepoID            string        `json:"repo_id"`
	Outcome           string        `json:"outcome"` // success|noop|failed_*
	DryRun            bool          `json:"dry_run"`
	ManifestVersionAt uint64        `json:"manifest_version_at_start"`
	ManifestVersionTo uint64        `json:"manifest_version_after,omitempty"`
	TriggerEval       TriggerReport `json:"trigger_eval"`
	BeforePackCount   int           `json:"before_pack_count"`
	AfterPackCount    int           `json:"after_pack_count"`
	BeforeManifestPB  int64         `json:"before_manifest_pack_bytes"`
	AfterManifestPB   int64         `json:"after_manifest_pack_bytes"`
	NewPackKey        string        `json:"new_pack_key,omitempty"`
	NewPackObjects    int           `json:"new_pack_objects,omitempty"`
	NewPackBytes      int64         `json:"new_pack_bytes,omitempty"`
	NewObjectMapKey   string        `json:"new_object_map_key,omitempty"`
	NewCommitGraphKey string        `json:"new_commit_graph_key,omitempty"`
	RepackedPackKeys  []string      `json:"repacked_pack_keys"`
	CASAttempts       int           `json:"cas_attempts"`
	DurationMS        int64         `json:"duration_ms"`
}

// TriggerReport records what Phase 0 saw, regardless of outcome.
type TriggerReport struct {
	Triggered         bool    `json:"triggered"`
	Reason            string  `json:"reason,omitempty"` // first trigger that fired
	RecentPackCount   int     `json:"recent_pack_count"`
	TotalPackCount    int     `json:"total_pack_count"`
	ManifestPackBytes int64   `json:"manifest_pack_bytes"`
	Thresholds        Thresholds `json:"thresholds"`
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run "TestThresholds|TestRunOptions" -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/options.go internal/maintenance/options_test.go
git commit -m "M9 task 0.3: add RunOptions, Thresholds, Report types with defaults + Validate"
```

---

## Phase 1 — Local pack store helper

`pack.Open` requires a `storage.ObjectStore`. To open a pack from the local filesystem (post-repack output, pre-upload), we need a tiny in-memory adapter. `internal/importer` has an unexported `newLocalFilePackStore` that does exactly this; M9 duplicates it (≈30 lines) rather than expose importer internals.

### Task 1.1: Implement `localFilePackStore`

**Files:**
- Create: `internal/maintenance/localpack.go`
- Create: `internal/maintenance/localpack_test.go`

- [ ] **Step 1: Read the importer's existing helper**

Run: `grep -n "newLocalFilePackStore\|localFilePackStore" /home/eran/work/bucketvcs/internal/importer/*.go`

Read the implementation (likely in `internal/importer/buildcommit.go` or a sibling) — the goal is to copy its shape, not reuse via a public re-export. Note its file path and the line range; M9's copy will live at `internal/maintenance/localpack.go`.

If the importer's adapter does more than the maintenance package needs (e.g., supports keys beyond two), trim to the two-key minimum: `p.pack` and `p.idx`.

- [ ] **Step 2: Write the failing test**

Create `internal/maintenance/localpack_test.go`:

```go
package maintenance

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalFilePackStore_GetReturnsFileBytes(t *testing.T) {
	tmp := t.TempDir()
	packPath := filepath.Join(tmp, "p.pack")
	idxPath := filepath.Join(tmp, "p.idx")
	if err := os.WriteFile(packPath, []byte("PACK-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, []byte("IDX-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := newLocalFilePackStore(packPath, idxPath)
	if err != nil {
		t.Fatalf("newLocalFilePackStore: %v", err)
	}
	ctx := context.Background()

	for _, tc := range []struct {
		key, want string
	}{
		{"p.pack", "PACK-bytes"},
		{"p.idx", "IDX-bytes"},
	} {
		obj, err := s.Get(ctx, tc.key, nil)
		if err != nil {
			t.Fatalf("Get(%s): %v", tc.key, err)
		}
		body, err := io.ReadAll(obj.Body)
		obj.Body.Close()
		if err != nil {
			t.Fatalf("ReadAll(%s): %v", tc.key, err)
		}
		if string(body) != tc.want {
			t.Errorf("Get(%s) = %q, want %q", tc.key, body, tc.want)
		}
		if int(obj.Metadata.Size) != len(tc.want) {
			t.Errorf("Get(%s).Metadata.Size = %d, want %d", tc.key, obj.Metadata.Size, len(tc.want))
		}
	}
}

func TestLocalFilePackStore_HeadReturnsSize(t *testing.T) {
	tmp := t.TempDir()
	packPath := filepath.Join(tmp, "p.pack")
	if err := os.WriteFile(packPath, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := newLocalFilePackStore(packPath, packPath) // idxPath same is fine for Head test
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	md, err := s.Head(ctx, "p.pack")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Size != 10 {
		t.Errorf("Head.Size = %d, want 10", md.Size)
	}
}

func TestLocalFilePackStore_RejectsUnknownKey(t *testing.T) {
	tmp := t.TempDir()
	packPath := filepath.Join(tmp, "p.pack")
	if err := os.WriteFile(packPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := newLocalFilePackStore(packPath, packPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), "p.nope", nil); err == nil {
		t.Fatal("Get(unknown key) succeeded; want error")
	}
}
```

(Note: tests live in package `maintenance` (not `_test`) to access the unexported `newLocalFilePackStore`. Subsequent test files for unexported symbols follow the same convention.)

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestLocalFilePackStore -v`
Expected: FAIL with `undefined: newLocalFilePackStore`.

- [ ] **Step 4: Implement localpack.go**

Create `internal/maintenance/localpack.go`:

```go
package maintenance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// localFilePackStore is a read-only, two-key storage.ObjectStore backed
// by a single local pack/idx pair on disk. It exists so pack.Open can
// read a freshly-produced repack output without needing to upload first.
//
// Mirrors internal/importer's unexported helper of the same name. The
// only methods exercised on the maintenance hot path are Get and Head;
// every other ObjectStore method returns ErrNotImplemented.
type localFilePackStore struct {
	packPath, idxPath string
	packBytes         []byte
	idxBytes          []byte
}

func newLocalFilePackStore(packPath, idxPath string) (*localFilePackStore, error) {
	pb, err := os.ReadFile(packPath)
	if err != nil {
		return nil, fmt.Errorf("localpack: read pack: %w", err)
	}
	ib, err := os.ReadFile(idxPath)
	if err != nil {
		return nil, fmt.Errorf("localpack: read idx: %w", err)
	}
	return &localFilePackStore{
		packPath: packPath, idxPath: idxPath,
		packBytes: pb, idxBytes: ib,
	}, nil
}

func (s *localFilePackStore) Get(_ context.Context, key string, _ *storage.GetOptions) (*storage.Object, error) {
	switch key {
	case "p.pack":
		return &storage.Object{
			Body:     io.NopCloser(bytes.NewReader(s.packBytes)),
			Metadata: storage.ObjectMetadata{Key: key, Size: int64(len(s.packBytes))},
		}, nil
	case "p.idx":
		return &storage.Object{
			Body:     io.NopCloser(bytes.NewReader(s.idxBytes)),
			Metadata: storage.ObjectMetadata{Key: key, Size: int64(len(s.idxBytes))},
		}, nil
	default:
		return nil, fmt.Errorf("localpack: unknown key %q", key)
	}
}

func (s *localFilePackStore) Head(_ context.Context, key string) (*storage.ObjectMetadata, error) {
	switch key {
	case "p.pack":
		return &storage.ObjectMetadata{Key: key, Size: int64(len(s.packBytes))}, nil
	case "p.idx":
		return &storage.ObjectMetadata{Key: key, Size: int64(len(s.idxBytes))}, nil
	default:
		return nil, fmt.Errorf("localpack: unknown key %q", key)
	}
}

// Unused ObjectStore methods. Returning errors here keeps the surface
// honest — pack.Open never calls them.
func (s *localFilePackStore) Put(context.Context, string, io.Reader, *storage.PutOptions) (*storage.ObjectMetadata, error) {
	return nil, fmt.Errorf("localpack: Put unsupported")
}
func (s *localFilePackStore) PutIfAbsent(context.Context, string, io.Reader, *storage.PutOptions) (*storage.ObjectMetadata, error) {
	return nil, fmt.Errorf("localpack: PutIfAbsent unsupported")
}
func (s *localFilePackStore) Delete(context.Context, string) error {
	return fmt.Errorf("localpack: Delete unsupported")
}
func (s *localFilePackStore) DeleteIfVersionMatches(context.Context, string, string) error {
	return fmt.Errorf("localpack: DeleteIfVersionMatches unsupported")
}
func (s *localFilePackStore) List(context.Context, *storage.ListOptions) (storage.ListResult, error) {
	return storage.ListResult{}, fmt.Errorf("localpack: List unsupported")
}
func (s *localFilePackStore) Copy(context.Context, string, string) error {
	return fmt.Errorf("localpack: Copy unsupported")
}
func (s *localFilePackStore) StartMultipart(context.Context, string) (storage.MultipartHandle, error) {
	return nil, fmt.Errorf("localpack: StartMultipart unsupported")
}
```

If the actual `storage.ObjectStore` interface in this tree includes additional methods, follow `grep -n "type ObjectStore interface" internal/storage/`'s output and stub each one with the same "unsupported" pattern. Verify against `internal/importer`'s adapter as the canonical model.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run TestLocalFilePackStore -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Commit**

```bash
git add internal/maintenance/localpack.go internal/maintenance/localpack_test.go
git commit -m "M9 task 1.1: add localFilePackStore for opening freshly-repacked pack pre-upload"
```

---

## Phase 2 — Materialize bare repo

This phase produces a usable `<tmp>/bare.git/` from the manifest's pack list and ref tips. Three files: `refsfile.go` (the deterministic packed-refs / HEAD writers), `materialize.go` (download orchestrator + fsck), and a fixtures helper in `mtest/`.

### Task 2.1: Write `packed-refs`, `HEAD`, and `config` helpers

**Files:**
- Create: `internal/maintenance/refsfile.go`
- Create: `internal/maintenance/refsfile_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/refsfile_test.go`:

```go
package maintenance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritePackedRefs_DeterministicSortByRefName(t *testing.T) {
	dir := t.TempDir()
	refs := map[string]string{
		"refs/heads/main":   "1111111111111111111111111111111111111111",
		"refs/heads/dev":    "2222222222222222222222222222222222222222",
		"refs/tags/v1.0.0":  "3333333333333333333333333333333333333333",
	}
	if err := writePackedRefs(dir, refs); err != nil {
		t.Fatalf("writePackedRefs: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "packed-refs"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"# pack-refs with: peeled fully-peeled sorted",
		"2222222222222222222222222222222222222222 refs/heads/dev",
		"1111111111111111111111111111111111111111 refs/heads/main",
		"3333333333333333333333333333333333333333 refs/tags/v1.0.0",
		"",
	}, "\n")
	if string(got) != want {
		t.Errorf("packed-refs:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func TestWriteHEAD_SymbolicRef(t *testing.T) {
	dir := t.TempDir()
	if err := writeHEAD(dir, "main"); err != nil {
		t.Fatalf("writeHEAD: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ref: refs/heads/main\n" {
		t.Errorf("HEAD = %q", got)
	}
}

func TestWriteHEAD_RejectsEmptyDefaultBranch(t *testing.T) {
	dir := t.TempDir()
	if err := writeHEAD(dir, ""); err == nil {
		t.Fatal("writeHEAD(empty) succeeded; want error")
	}
}

func TestWriteMinimalConfig_Bare(t *testing.T) {
	dir := t.TempDir()
	if err := writeMinimalConfig(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"
	if string(got) != want {
		t.Errorf("config = %q", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run "TestWritePackedRefs|TestWriteHEAD|TestWriteMinimalConfig" -v`
Expected: FAIL with undefined symbols.

- [ ] **Step 3: Implement refsfile.go**

Create `internal/maintenance/refsfile.go`:

```go
package maintenance

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// writePackedRefs writes <bareDir>/packed-refs from the manifest's ref
// map. Lines are sorted by ref name for determinism (so two materialize
// runs over the same manifest produce byte-identical output, which
// makes test fixtures stable). Standard packed-refs header.
func writePackedRefs(bareDir string, refs map[string]string) error {
	names := make([]string, 0, len(refs))
	for k := range refs {
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# pack-refs with: peeled fully-peeled sorted\n")
	for _, n := range names {
		fmt.Fprintf(&b, "%s %s\n", refs[n], n)
	}
	return os.WriteFile(filepath.Join(bareDir, "packed-refs"), []byte(b.String()), 0o644)
}

// writeHEAD writes <bareDir>/HEAD as a symbolic ref to
// refs/heads/<defaultBranch>. M9 only repacks repos that have refs;
// callers that hit the no-refs path skip materialize entirely.
func writeHEAD(bareDir, defaultBranch string) error {
	if defaultBranch == "" {
		return fmt.Errorf("writeHEAD: defaultBranch is empty")
	}
	body := fmt.Sprintf("ref: refs/heads/%s\n", defaultBranch)
	return os.WriteFile(filepath.Join(bareDir, "HEAD"), []byte(body), 0o644)
}

// writeMinimalConfig writes the smallest [core] block git needs to
// recognize <bareDir> as a bare repo for fsck and pack-objects.
func writeMinimalConfig(bareDir string) error {
	body := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"
	return os.WriteFile(filepath.Join(bareDir, "config"), []byte(body), 0o644)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run "TestWritePackedRefs|TestWriteHEAD|TestWriteMinimalConfig" -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/refsfile.go internal/maintenance/refsfile_test.go
git commit -m "M9 task 2.1: write packed-refs, HEAD, minimal config for the materialized bare repo"
```

### Task 2.2: Implement pack download

**Files:**
- Create: `internal/maintenance/materialize.go` (initial version: only `downloadPack`)
- Create: `internal/maintenance/materialize_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/materialize_test.go`:

```go
package maintenance

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestDownloadPack_StreamsPackAndIdxToBareDir(t *testing.T) {
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	packKey := "tenants/acme/repos/site/packs/canonical/abc.pack"
	idxKey := "tenants/acme/repos/site/packs/canonical/abc.idx"
	if _, err := store.Put(ctx, packKey, bytes.NewReader([]byte("PACKBYTES")), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, idxKey, bytes.NewReader([]byte("IDXBYTES")), nil); err != nil {
		t.Fatal(err)
	}

	bareDir := filepath.Join(t.TempDir(), "bare.git", "objects", "pack")
	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gotPack, gotIdx, err := downloadPack(ctx, store, packKey, idxKey, bareDir)
	if err != nil {
		t.Fatalf("downloadPack: %v", err)
	}
	pb, err := os.ReadFile(gotPack)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := os.ReadFile(gotIdx)
	if err != nil {
		t.Fatal(err)
	}
	if string(pb) != "PACKBYTES" {
		t.Errorf("pack bytes = %q", pb)
	}
	if string(ib) != "IDXBYTES" {
		t.Errorf("idx bytes = %q", ib)
	}
	// Both files must share the same basename root (git's pack-N convention).
	if filepath.Base(gotPack)[:len(filepath.Base(gotPack))-len(".pack")] !=
		filepath.Base(gotIdx)[:len(filepath.Base(gotIdx))-len(".idx")] {
		t.Errorf("pack/idx basenames don't match: %s vs %s", gotPack, gotIdx)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestDownloadPack -v`
Expected: FAIL with `undefined: downloadPack`.

- [ ] **Step 3: Implement downloadPack**

Create `internal/maintenance/materialize.go`:

```go
package maintenance

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// downloadPack streams (packKey, idxKey) into <bareDir>/pack-<basename>.{pack,idx}.
// The basename is derived from packKey's content hash where possible
// (last /-separated segment, sans extension); if that fails we fall
// back to a SHA-1 of packKey to keep names unique within bareDir.
//
// Returns the local paths of the written files.
func downloadPack(ctx context.Context, s storage.ObjectStore, packKey, idxKey, bareDir string) (string, string, error) {
	base := basenameFromKey(packKey)
	if base == "" {
		sum := sha1.Sum([]byte(packKey))
		base = "synth-" + hex.EncodeToString(sum[:8])
	}
	packPath := filepath.Join(bareDir, "pack-"+base+".pack")
	idxPath := filepath.Join(bareDir, "pack-"+base+".idx")

	if err := streamToFile(ctx, s, packKey, packPath); err != nil {
		return "", "", fmt.Errorf("downloadPack: pack: %w", err)
	}
	if err := streamToFile(ctx, s, idxKey, idxPath); err != nil {
		return "", "", fmt.Errorf("downloadPack: idx: %w", err)
	}
	return packPath, idxPath, nil
}

// streamToFile streams an ObjectStore key to a local file, creating
// parent directories as needed. Closes both the response body and the
// destination file before returning.
func streamToFile(ctx context.Context, s storage.ObjectStore, key, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	obj, err := s.Get(ctx, key, nil)
	if err != nil {
		return err
	}
	defer obj.Body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, obj.Body); err != nil {
		return err
	}
	return nil
}

// basenameFromKey extracts the last /-separated segment of key, then
// strips any trailing .pack / .idx. Returns "" if the result is empty.
func basenameFromKey(key string) string {
	last := key
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '/' {
			last = key[i+1:]
			break
		}
	}
	for _, ext := range []string{".pack", ".idx"} {
		if len(last) > len(ext) && last[len(last)-len(ext):] == ext {
			last = last[:len(last)-len(ext)]
		}
	}
	return last
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run TestDownloadPack -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/materialize.go internal/maintenance/materialize_test.go
git commit -m "M9 task 2.2: download pack/idx pair from object store into bareDir"
```

### Task 2.3: Implement `Materialize` orchestrator

**Files:**
- Modify: `internal/maintenance/materialize.go`
- Modify: `internal/maintenance/materialize_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/maintenance/materialize_test.go`:

```go
import (
	"os/exec"
)

func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func TestMaterialize_BuildsBareRepoThatFscks(t *testing.T) {
	gitAvailable(t)

	// Build a real repo with `git`, pack-objects-all it, upload the
	// pack to a localfs store under a canonical key, then call
	// Materialize and assert the resulting bare repo passes fsck.
	src := t.TempDir()
	mustGit(t, src, "init", "--bare")
	mustGit(t, src, "config", "user.email", "test@example.com")
	mustGit(t, src, "config", "user.name", "T")
	wt := t.TempDir()
	mustGit(t, wt, "init")
	mustGit(t, wt, "config", "user.email", "test@example.com")
	mustGit(t, wt, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(wt, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "add", ".")
	mustGit(t, wt, "commit", "-m", "init")
	mustGit(t, wt, "remote", "add", "origin", src)
	mustGit(t, wt, "push", "origin", "HEAD:refs/heads/main")

	// Pack-objects-all the source bare → tmp/out/pack-<id>.{pack,idx}
	prefix := filepath.Join(t.TempDir(), "out", "pack")
	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		t.Fatal(err)
	}
	packID, err := gitcli.PackObjectsAll(context.Background(), src, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	srcPack := prefix + "-" + packID + ".pack"
	srcIdx := prefix + "-" + packID + ".idx"

	// Build a localfs store and upload the pack/idx under a canonical key.
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	packKey := "tenants/acme/repos/site/packs/canonical/" + packID + ".pack"
	idxKey := "tenants/acme/repos/site/packs/canonical/" + packID + ".idx"
	uploadFileForTest(t, store, srcPack, packKey)
	uploadFileForTest(t, store, srcIdx, idxKey)

	// Resolve the head OID.
	out := mustGitOutput(t, src, "rev-parse", "HEAD")
	headOID := strings.TrimSpace(out)

	bareDir := t.TempDir()
	err = Materialize(context.Background(), store, MaterializeInput{
		BareDir: bareDir,
		Packs: []PackRef{{
			PackKey: packKey,
			IdxKey:  idxKey,
		}},
		Refs:          map[string]string{"refs/heads/main": headOID},
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// Verify fsck-clean.
	cmd := exec.Command("git", "--git-dir="+filepath.Join(bareDir, "bare.git"), "fsck", "--full")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git fsck failed: %v\n%s", err, out)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func mustGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func uploadFileForTest(t *testing.T, s storage.ObjectStore, srcPath, dstKey string) {
	t.Helper()
	f, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := s.Put(context.Background(), dstKey, f, nil); err != nil {
		t.Fatal(err)
	}
}
```

Add the necessary imports at the top of the test file: `"strings"`, `"github.com/bucketvcs/bucketvcs/internal/gitcli"`, `"github.com/bucketvcs/bucketvcs/internal/storage"`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestMaterialize_BuildsBareRepoThatFscks -v`
Expected: FAIL with `undefined: Materialize`.

- [ ] **Step 3: Implement `Materialize`**

Append to `internal/maintenance/materialize.go`:

```go
import (
	// (added imports for the new code below)
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// PackRef identifies one canonical pack to materialize. Mirrors the
// minimum subset of manifest.PackEntry that materialize needs.
type PackRef struct {
	PackKey string
	IdxKey  string
}

// MaterializeInput drives one Materialize call.
type MaterializeInput struct {
	BareDir       string            // parent dir; "bare.git/" is created inside
	Packs         []PackRef         // every canonical pack that must end up locally
	Refs          map[string]string // ref → commit oid
	DefaultBranch string            // for HEAD; must be non-empty when Refs is non-empty
}

// Materialize creates <BareDir>/bare.git/objects/pack/, downloads every
// pack pair, writes packed-refs, HEAD, and a minimal config, and runs
// `git fsck --full` to validate the result.
func Materialize(ctx context.Context, s storage.ObjectStore, in MaterializeInput) error {
	bare := filepath.Join(in.BareDir, "bare.git")
	packDir := filepath.Join(bare, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		return fmt.Errorf("materialize: mkdir bare: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(bare, "refs"), 0o755); err != nil {
		return fmt.Errorf("materialize: mkdir refs: %w", err)
	}
	if err := writeMinimalConfig(bare); err != nil {
		return fmt.Errorf("materialize: write config: %w", err)
	}
	if err := writeHEAD(bare, in.DefaultBranch); err != nil {
		return fmt.Errorf("materialize: write HEAD: %w", err)
	}
	if err := writePackedRefs(bare, in.Refs); err != nil {
		return fmt.Errorf("materialize: write packed-refs: %w", err)
	}
	for _, p := range in.Packs {
		if _, _, err := downloadPack(ctx, s, p.PackKey, p.IdxKey, packDir); err != nil {
			return fmt.Errorf("materialize: download pack: %w", err)
		}
	}
	if err := gitcli.Fsck(ctx, bare, true); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptInput, err)
	}
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run TestMaterialize_BuildsBareRepoThatFscks -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/materialize.go internal/maintenance/materialize_test.go
git commit -m "M9 task 2.3: Materialize orchestrator builds fsck-clean bare repo from canonical packs"
```

---

## Phase 3 — Repack via gitcli.PackObjectsAll

### Task 3.1: Implement `Repack`

**Files:**
- Create: `internal/maintenance/repack.go`
- Create: `internal/maintenance/repack_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/repack_test.go`:

```go
package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRepack_ProducesSinglePackPair(t *testing.T) {
	gitAvailable(t)
	bareDir := setupSyntheticBareRepo(t) // helper from materialize_test.go (move to mtest later)

	out, err := Repack(context.Background(), bareDir)
	if err != nil {
		t.Fatalf("Repack: %v", err)
	}
	if out.PackID == "" {
		t.Fatal("PackID empty")
	}
	if _, err := os.Stat(out.PackPath); err != nil {
		t.Fatalf("pack file missing: %v", err)
	}
	if _, err := os.Stat(out.IdxPath); err != nil {
		t.Fatalf("idx file missing: %v", err)
	}
	if out.SizeBytes <= 0 {
		t.Fatalf("SizeBytes = %d, want > 0", out.SizeBytes)
	}
	if filepath.Dir(out.PackPath) != filepath.Dir(out.IdxPath) {
		t.Errorf("pack and idx in different dirs: %s vs %s", out.PackPath, out.IdxPath)
	}
}
```

The helper `setupSyntheticBareRepo` should be extracted from the materialize_test.go inline `mustGit`/PackObjectsAll dance into a small helper that returns the bare-repo path, ready for repack. Moving it into `internal/maintenance/mtest/fixtures.go` is the right home — see Task 3.2.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestRepack_ProducesSinglePackPair -v`
Expected: FAIL with `undefined: Repack` and/or `undefined: setupSyntheticBareRepo`.

- [ ] **Step 3: Implement `Repack`**

Create `internal/maintenance/repack.go`:

```go
package maintenance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// RepackOutput describes the local artifacts produced by Repack.
type RepackOutput struct {
	PackID    string // git's trailing SHA-1 over the pack bytes
	PackPath  string // <bareDir>/out/pack-<PackID>.pack
	IdxPath   string // <bareDir>/out/pack-<PackID>.idx
	SizeBytes int64  // size of the .pack file
}

// Repack invokes gitcli.PackObjectsAll against <bareDir>/bare.git and
// writes the consolidated pack pair into <bareDir>/out/. Returns the
// pack ID, paths, and pack file size.
//
// Caller is responsible for cleaning up <bareDir> when done.
func Repack(ctx context.Context, bareDir string) (*RepackOutput, error) {
	outDir := filepath.Join(bareDir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("repack: mkdir: %w", err)
	}
	prefix := filepath.Join(outDir, "pack")

	bare := filepath.Join(bareDir, "bare.git")
	packID, err := gitcli.PackObjectsAll(ctx, bare, prefix)
	if err != nil {
		return nil, fmt.Errorf("repack: pack-objects: %w", err)
	}
	packPath := prefix + "-" + packID + ".pack"
	idxPath := prefix + "-" + packID + ".idx"
	st, err := os.Stat(packPath)
	if err != nil {
		return nil, fmt.Errorf("repack: stat pack: %w", err)
	}
	return &RepackOutput{
		PackID:    packID,
		PackPath:  packPath,
		IdxPath:   idxPath,
		SizeBytes: st.Size(),
	}, nil
}
```

- [ ] **Step 4: Run the test to verify it passes** (after Task 3.2 lands the helper)

Move `setupSyntheticBareRepo` into the `mtest` package (Task 3.2). Once that lands, run: `go test ./internal/maintenance/ -run TestRepack_ProducesSinglePackPair -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/repack.go internal/maintenance/repack_test.go
git commit -m "M9 task 3.1: Repack wraps gitcli.PackObjectsAll into a typed RepackOutput"
```

### Task 3.2: Move test helpers into `internal/maintenance/mtest`

**Files:**
- Create: `internal/maintenance/mtest/fixtures.go`
- Modify: `internal/maintenance/materialize_test.go`
- Modify: `internal/maintenance/repack_test.go`

- [ ] **Step 1: Extract `mustGit`, `mustGitOutput`, `gitAvailable`, `uploadFileForTest`, `setupSyntheticBareRepo`**

Create `internal/maintenance/mtest/fixtures.go` with the extracted helpers, exported (`MustGit`, `GitAvailable`, `SetupSyntheticBareRepo`, `UploadFile`, `MustGitOutput`). Implementations are the same as the inline test helpers from earlier tasks; lift them verbatim.

`SetupSyntheticBareRepo(t *testing.T) (bareDir string, packKey, idxKey, headOID string, store storage.ObjectStore)` returns enough state for any downstream test to call `Materialize` immediately.

- [ ] **Step 2: Update existing test files to use mtest**

In `internal/maintenance/materialize_test.go` and `internal/maintenance/repack_test.go`, replace inline helpers with `mtest.*` references. Tests live in package `maintenance` (they touch unexported helpers); the imports change to:

```go
import "github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
```

- [ ] **Step 3: Run all maintenance tests**

Run: `go test ./internal/maintenance/...`
Expected: PASS (all prior tests still green; no behavior change).

- [ ] **Step 4: Commit**

```bash
git add internal/maintenance/mtest/ internal/maintenance/materialize_test.go internal/maintenance/repack_test.go
git commit -m "M9 task 3.2: extract test fixtures into internal/maintenance/mtest"
```

---

## Phase 4 — Build .bvom and .bvcg from local pack

This phase duplicates `internal/importer.buildIndexesFromPack` (≈50 lines) into `internal/maintenance/indexes.go`. Per the spec rationale we duplicate rather than refactor importer to expose it.

### Task 4.1: Implement `buildIndexesFromLocalPack`

**Files:**
- Create: `internal/maintenance/indexes.go`
- Create: `internal/maintenance/indexes_test.go`

- [ ] **Step 1: Read importer's buildIndexesFromPack**

Run: `sed -n '/func buildIndexesFromPack/,/^}/p' /home/eran/work/bucketvcs/internal/importer/importer.go`

Note its inputs (packPath, idxPath, packID, refs), outputs (.bvom + .bvcg bytes + hashes + object count + pack size), and the helpers it calls (`pack.Open`, `objindex.Build`, `buildTipsFromRefs`, `commitgraph.Build`).

- [ ] **Step 2: Write the failing test**

Create `internal/maintenance/indexes_test.go`:

```go
package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
)

func TestBuildIndexesFromLocalPack_HashesAreContentAddressed(t *testing.T) {
	mtest.GitAvailable(t)
	out := mtest.SetupRepackedPack(t) // returns packPath, idxPath, packID, refs

	res, err := buildIndexesFromLocalPack(context.Background(),
		out.PackPath, out.IdxPath, out.PackID, out.Refs)
	if err != nil {
		t.Fatalf("buildIndexesFromLocalPack: %v", err)
	}
	if len(res.ObjectMapBytes) == 0 {
		t.Fatalf("ObjectMapBytes empty")
	}
	bvomSum := sha256.Sum256(res.ObjectMapBytes)
	if res.ObjectMapHash != hex.EncodeToString(bvomSum[:]) {
		t.Errorf("ObjectMapHash != sha256(bytes)")
	}
	if len(res.CommitGraphBytes) == 0 {
		t.Fatalf("CommitGraphBytes empty")
	}
	bvcgSum := sha256.Sum256(res.CommitGraphBytes)
	if res.CommitGraphHash != hex.EncodeToString(bvcgSum[:]) {
		t.Errorf("CommitGraphHash != sha256(bytes)")
	}
	if res.ObjectCount <= 0 {
		t.Errorf("ObjectCount = %d", res.ObjectCount)
	}
	if res.PackSizeBytes <= 0 {
		t.Errorf("PackSizeBytes = %d", res.PackSizeBytes)
	}
}
```

`mtest.SetupRepackedPack` is a new helper: build a synthetic bare repo (per Task 3.2), run `Repack`, and return `{PackPath, IdxPath, PackID, Refs}`. Add it to `mtest/fixtures.go` in the same step.

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestBuildIndexesFromLocalPack -v`
Expected: FAIL with `undefined: buildIndexesFromLocalPack`.

- [ ] **Step 4: Implement indexes.go**

Create `internal/maintenance/indexes.go`:

```go
package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/commitgraph"
	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// LocalIndexes is the result of buildIndexesFromLocalPack — exactly the
// shape the maintenance pipeline hands to the upload + CAS-merge phases.
type LocalIndexes struct {
	ObjectMapBytes   []byte
	ObjectMapHash    string // hex-encoded SHA-256
	CommitGraphBytes []byte
	CommitGraphHash  string // hex-encoded SHA-256
	ObjectCount      int
	PackSizeBytes    int64
}

// buildIndexesFromLocalPack opens (packPath, idxPath) via the local
// file pack store, builds .bvom (objindex) and .bvcg (commit-graph),
// and returns the bytes + their content hashes.
//
// Mirrors internal/importer.buildIndexesFromPack. Caller is responsible
// for ensuring the pack is reachability-complete relative to refs
// (Phase 1+2 of the maintenance pipeline guarantee this).
func buildIndexesFromLocalPack(ctx context.Context, packPath, idxPath, packID string, refs map[string]string) (*LocalIndexes, error) {
	store, err := newLocalFilePackStore(packPath, idxPath)
	if err != nil {
		return nil, fmt.Errorf("indexes: localpack: %w", err)
	}
	r, err := pack.Open(ctx, store, "p.pack", "p.idx")
	if err != nil {
		return nil, fmt.Errorf("indexes: pack.Open: %w", err)
	}
	defer r.Close()

	bvom, err := objindex.Build(r, packID)
	if err != nil {
		return nil, fmt.Errorf("indexes: objindex.Build: %w", err)
	}
	bvomSum := sha256.Sum256(bvom)

	tips, err := buildTipsFromRefs(ctx, r, refs)
	if err != nil {
		return nil, fmt.Errorf("indexes: buildTipsFromRefs: %w", err)
	}
	bvcg, err := commitgraph.Build(ctx, r, tips)
	if err != nil {
		return nil, fmt.Errorf("indexes: commitgraph.Build: %w", err)
	}
	bvcgSum := sha256.Sum256(bvcg)

	st, err := os.Stat(packPath)
	if err != nil {
		return nil, fmt.Errorf("indexes: stat pack: %w", err)
	}
	return &LocalIndexes{
		ObjectMapBytes:   bvom,
		ObjectMapHash:    hex.EncodeToString(bvomSum[:]),
		CommitGraphBytes: bvcg,
		CommitGraphHash:  hex.EncodeToString(bvcgSum[:]),
		ObjectCount:      r.Idx().Count(),
		PackSizeBytes:    st.Size(),
	}, nil
}

// buildTipsFromRefs filters refs to those whose target is a commit in
// the pack (annotated tags are dereferenced via the tag's `object`
// line, capped at depth 16). Mirrors internal/importer's helper.
func buildTipsFromRefs(ctx context.Context, r *pack.Reader, refs map[string]string) ([]commitgraph.Tip, error) {
	tips := make([]commitgraph.Tip, 0, len(refs))
	for ref, oidStr := range refs {
		oid, err := pack.ParseOID(oidStr)
		if err != nil {
			return nil, fmt.Errorf("ref %s: parse oid: %w", ref, err)
		}
		obj, err := r.Get(ctx, oid)
		if err != nil {
			return nil, fmt.Errorf("ref %s: get %s: %w", ref, oid, err)
		}
		const maxTagDepth = 16
		depth := 0
		for obj.Type == pack.TypeTag {
			depth++
			if depth > maxTagDepth {
				return nil, fmt.Errorf("ref %s: tag chain exceeds depth %d", ref, maxTagDepth)
			}
			target, err := tagTargetOID(obj.Body)
			if err != nil {
				return nil, fmt.Errorf("ref %s: tag parse: %w", ref, err)
			}
			obj, err = r.Get(ctx, target)
			if err != nil {
				return nil, fmt.Errorf("ref %s: tag target %s: %w", ref, target, err)
			}
		}
		if obj.Type != pack.TypeCommit {
			continue // skip non-commit refs (e.g. blob ref) silently
		}
		tips = append(tips, commitgraph.Tip{Ref: ref, OID: oid})
	}
	return tips, nil
}

// tagTargetOID parses the `object <oid>` line of a tag body. Mirrors
// internal/importer.tagTarget; copied to avoid an importer dependency.
func tagTargetOID(body []byte) (pack.OID, error) {
	// Tag body format: header lines ending at the first blank line.
	// First header MUST be "object <40-hex>\n".
	if len(body) < len("object ")+40+1 {
		return pack.OID{}, fmt.Errorf("tag body too short")
	}
	if string(body[:len("object ")]) != "object " {
		return pack.OID{}, fmt.Errorf("tag body missing 'object' header")
	}
	return pack.ParseOID(string(body[len("object "):][:40]))
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run TestBuildIndexesFromLocalPack -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/maintenance/indexes.go internal/maintenance/indexes_test.go internal/maintenance/mtest/fixtures.go
git commit -m "M9 task 4.1: build .bvom + .bvcg from local repack output (duplicated from importer)"
```

---

## Phase 5 — Upload artifacts (PutIfAbsent)

### Task 5.1: Implement `UploadArtifacts`

**Files:**
- Create: `internal/maintenance/upload.go`
- Create: `internal/maintenance/upload_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/upload_test.go`:

```go
package maintenance

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestUploadArtifacts_SuccessWritesAllFour(t *testing.T) {
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	in := uploadInput{
		PackID:           "abc123",
		PackBytes:        []byte("PACKBYTES"),
		IdxBytes:         []byte("IDXBYTES"),
		ObjectMapHash:    "deadbeef",
		ObjectMapBytes:   []byte("BVOM"),
		CommitGraphHash:  "cafef00d",
		CommitGraphBytes: []byte("BVCG"),
	}
	out, err := uploadArtifacts(ctx, s, k, in)
	if err != nil {
		t.Fatalf("uploadArtifacts: %v", err)
	}
	if out.PackKey == "" || out.IdxKey == "" || out.ObjectMapKey == "" || out.CommitGraphKey == "" {
		t.Fatalf("uploadResult has empty keys: %+v", out)
	}
	for _, key := range []string{out.PackKey, out.IdxKey, out.ObjectMapKey, out.CommitGraphKey} {
		if _, err := s.Head(ctx, key); err != nil {
			t.Errorf("Head(%s): %v", key, err)
		}
	}
}

func TestUploadArtifacts_PackCollisionReturnsErrPackCollision(t *testing.T) {
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Pre-populate the canonical pack key with different bytes.
	preexistingKey := k.CanonicalPackKey("xyz")
	if _, err := s.Put(ctx, preexistingKey, bytes.NewReader([]byte("DIFFERENT")), nil); err != nil {
		t.Fatal(err)
	}
	in := uploadInput{
		PackID:    "xyz",
		PackBytes: []byte("OURS"),
		IdxBytes:  []byte("IDX"),
	}
	if _, err := uploadArtifacts(ctx, s, k, in); !errors.Is(err, ErrPackCollision) {
		t.Fatalf("err = %v, want ErrPackCollision", err)
	}
}

func TestUploadArtifacts_IndexAlreadyExistsIsBenign(t *testing.T) {
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Pre-populate the .bvom key with the SAME bytes we're about to upload.
	hash := "deadbeef"
	bvomKey := k.ObjectMapKey(hash)
	bvomBytes := []byte("SAMEBVOM")
	if _, err := s.Put(ctx, bvomKey, bytes.NewReader(bvomBytes), nil); err != nil {
		t.Fatal(err)
	}

	in := uploadInput{
		PackID:           "p1",
		PackBytes:        []byte("P"),
		IdxBytes:         []byte("I"),
		ObjectMapHash:    hash,
		ObjectMapBytes:   bvomBytes,
		CommitGraphHash:  "cafe",
		CommitGraphBytes: []byte("CG"),
	}
	if _, err := uploadArtifacts(ctx, s, k, in); err != nil {
		t.Fatalf("uploadArtifacts (benign collision): %v", err)
	}
}

var _ = storage.ErrAlreadyExists // touch import for clarity in helpers below
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestUploadArtifacts -v`
Expected: FAIL with undefined symbols.

- [ ] **Step 3: Implement upload.go**

Create `internal/maintenance/upload.go`:

```go
package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

type uploadInput struct {
	PackID           string
	PackBytes        []byte // ignored when PackPath is set
	PackPath         string // optional; when set, streamed from disk
	IdxBytes         []byte
	IdxPath          string
	ObjectMapHash    string
	ObjectMapBytes   []byte
	CommitGraphHash  string
	CommitGraphBytes []byte
}

type uploadResult struct {
	PackKey        string
	IdxKey         string
	ObjectMapKey   string
	CommitGraphKey string
}

func uploadArtifacts(ctx context.Context, s storage.ObjectStore, k *keys.Repo, in uploadInput) (uploadResult, error) {
	res := uploadResult{
		PackKey:        k.CanonicalPackKey(in.PackID),
		IdxKey:         k.PackIdxKey(in.PackID, "canonical"),
		ObjectMapKey:   k.ObjectMapKey(in.ObjectMapHash),
		CommitGraphKey: k.CommitGraphKey(in.CommitGraphHash),
	}

	if err := putIfAbsentBytesOrFile(ctx, s, res.PackKey, in.PackBytes, in.PackPath); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			return res, fmt.Errorf("%w: key=%s", ErrPackCollision, res.PackKey)
		}
		return res, fmt.Errorf("upload: pack: %w", err)
	}
	if err := putIfAbsentBytesOrFile(ctx, s, res.IdxKey, in.IdxBytes, in.IdxPath); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			return res, fmt.Errorf("%w: key=%s (idx)", ErrPackCollision, res.IdxKey)
		}
		return res, fmt.Errorf("upload: idx: %w", err)
	}
	// .bvom and .bvcg are content-addressed (sha256 of bytes); ErrAlreadyExists means same bytes -> benign.
	if err := putIfAbsentBytes(ctx, s, res.ObjectMapKey, in.ObjectMapBytes); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
		return res, fmt.Errorf("upload: bvom: %w", err)
	}
	if err := putIfAbsentBytes(ctx, s, res.CommitGraphKey, in.CommitGraphBytes); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
		return res, fmt.Errorf("upload: bvcg: %w", err)
	}
	return res, nil
}

func putIfAbsentBytes(ctx context.Context, s storage.ObjectStore, key string, body []byte) error {
	_, err := s.PutIfAbsent(ctx, key, bytes.NewReader(body), nil)
	return err
}

func putIfAbsentBytesOrFile(ctx context.Context, s storage.ObjectStore, key string, body []byte, path string) error {
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = s.PutIfAbsent(ctx, key, f, nil)
		return err
	}
	return putIfAbsentBytes(ctx, s, key, body)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run TestUploadArtifacts -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/upload.go internal/maintenance/upload_test.go
git commit -m "M9 task 5.1: upload pack/idx/.bvom/.bvcg with PutIfAbsent + collision handling"
```

---

## Phase 6 — CAS-merge body builder

The CAS retry loop is `repo.Repo.Commit`'s built-in machinery. M9 supplies a `buildBody` callback that, given the just-read prev `*RootView`, returns the merged manifest body bytes.

### Task 6.1: Implement `buildMergedBody`

**Files:**
- Create: `internal/maintenance/casmerge.go`
- Create: `internal/maintenance/casmerge_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/casmerge_test.go`:

```go
package maintenance

import (
	"encoding/json"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestBuildMergedBody_NoConcurrentChange(t *testing.T) {
	prev := manifest.Body{
		DefaultBranch: "main",
		Refs:          map[string]string{"refs/heads/main": "deadbeef"},
		Packs: []manifest.PackEntry{
			{PackID: "old1", PackKey: "K1", IdxKey: "I1"},
			{PackID: "old2", PackKey: "K2", IdxKey: "I2"},
		},
		Indexes: manifest.Indexes{
			ObjectMap:   &manifest.IndexRef{Key: "old-bvom", Hash: "h1"},
			CommitGraph: &manifest.IndexRef{Key: "old-bvcg", Hash: "h2"},
		},
	}
	in := mergeInput{
		P0Keys: []string{"K1", "K2"},
		NewPack: manifest.PackEntry{
			PackID: "new1", PackKey: "Knew", IdxKey: "Inew",
			SizeBytes: 100, ObjectCount: 5,
		},
		NewObjectMap:   manifest.IndexRef{Key: "new-bvom", Hash: "h3"},
		NewCommitGraph: manifest.IndexRef{Key: "new-bvcg", Hash: "h4"},
	}
	got := buildMergedBody(prev, in)
	if len(got.Packs) != 1 || got.Packs[0].PackID != "new1" {
		t.Fatalf("Packs = %+v, want exactly [new1]", got.Packs)
	}
	if got.Indexes.ObjectMap == nil || got.Indexes.ObjectMap.Key != "new-bvom" {
		t.Errorf("ObjectMap not updated: %+v", got.Indexes.ObjectMap)
	}
	if got.Refs["refs/heads/main"] != "deadbeef" {
		t.Errorf("Refs not preserved")
	}
	if got.DefaultBranch != "main" {
		t.Errorf("DefaultBranch not preserved")
	}
}

func TestBuildMergedBody_KeepsLatePushPacks(t *testing.T) {
	prev := manifest.Body{
		DefaultBranch: "main",
		Refs:          map[string]string{"refs/heads/main": "newtip"},
		Packs: []manifest.PackEntry{
			{PackID: "old1", PackKey: "K1", IdxKey: "I1"},
			{PackID: "old2", PackKey: "K2", IdxKey: "I2"},
			{PackID: "late", PackKey: "Klate", IdxKey: "Ilate"}, // landed during run
		},
	}
	in := mergeInput{
		P0Keys:  []string{"K1", "K2"}, // we only repacked K1 and K2; Klate is new
		NewPack: manifest.PackEntry{PackID: "new1", PackKey: "Knew", IdxKey: "Inew"},
	}
	got := buildMergedBody(prev, in)
	if len(got.Packs) != 2 {
		t.Fatalf("got %d packs, want 2", len(got.Packs))
	}
	if got.Packs[0].PackID != "new1" {
		t.Errorf("Packs[0] = %s, want new1", got.Packs[0].PackID)
	}
	if got.Packs[1].PackID != "late" {
		t.Errorf("Packs[1] = %s, want late", got.Packs[1].PackID)
	}
}

func TestBuildMergedBody_RoundTrips(t *testing.T) {
	in := mergeInput{
		P0Keys:         []string{"K"},
		NewPack:        manifest.PackEntry{PackID: "n", PackKey: "Knew", IdxKey: "Inew"},
		NewObjectMap:   manifest.IndexRef{Key: "BV", Hash: "H"},
		NewCommitGraph: manifest.IndexRef{Key: "CG", Hash: "H2"},
	}
	prev := manifest.Body{
		DefaultBranch: "main",
		Refs:          map[string]string{"r": "o"},
		Packs:         []manifest.PackEntry{{PackKey: "K"}},
	}
	body := buildMergedBody(prev, in)
	bytes, err := manifest.MarshalBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var rt manifest.Body
	if err := json.Unmarshal(bytes, &rt); err != nil {
		t.Fatal(err)
	}
	if rt.Packs[0].PackID != "n" {
		t.Errorf("round trip lost new pack")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestBuildMergedBody -v`
Expected: FAIL with `undefined: buildMergedBody`.

- [ ] **Step 3: Implement casmerge.go**

Create `internal/maintenance/casmerge.go`:

```go
package maintenance

import "github.com/bucketvcs/bucketvcs/internal/repo/manifest"

// mergeInput is the per-run state that buildMergedBody needs.
type mergeInput struct {
	P0Keys         []string             // PackKey set we repacked at run start
	NewPack        manifest.PackEntry   // the consolidated repack output
	NewObjectMap   manifest.IndexRef
	NewCommitGraph manifest.IndexRef
}

// buildMergedBody constructs the manifest body that maintenance wants
// to commit, given prev (the just-read manifest) and our run state.
//
//   Packs         = [NewPack] ++ (prev.Packs filtered by PackKey ∉ P0Keys)
//   Indexes       = { ObjectMap: NewObjectMap, CommitGraph: NewCommitGraph }
//   Refs          = prev.Refs        (preserved verbatim)
//   DefaultBranch = prev.DefaultBranch
//   Bundles       = prev.Bundles
//
// This is a pure function over its inputs — fully testable without an
// ObjectStore. The retry loop in repo.Repo.Commit re-runs this on each
// CAS attempt with a fresh prev.
func buildMergedBody(prev manifest.Body, in mergeInput) manifest.Body {
	p0 := make(map[string]struct{}, len(in.P0Keys))
	for _, k := range in.P0Keys {
		p0[k] = struct{}{}
	}
	out := manifest.Body{
		DefaultBranch: prev.DefaultBranch,
		Refs:          prev.Refs,
		Bundles:       prev.Bundles,
	}
	out.Packs = append(out.Packs, in.NewPack)
	for _, p := range prev.Packs {
		if _, repacked := p0[p.PackKey]; repacked {
			continue
		}
		out.Packs = append(out.Packs, p)
	}
	bvom := in.NewObjectMap
	bvcg := in.NewCommitGraph
	out.Indexes = manifest.Indexes{
		ObjectMap:   &bvom,
		CommitGraph: &bvcg,
	}
	return out
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run TestBuildMergedBody -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/casmerge.go internal/maintenance/casmerge_test.go
git commit -m "M9 task 6.1: buildMergedBody — pure CAS-merge body construction"
```

---

## Phase 7 — Threshold evaluation

### Task 7.1: Implement `Evaluate`

**Files:**
- Create: `internal/maintenance/thresholds.go`
- Create: `internal/maintenance/thresholds_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/maintenance/thresholds_test.go`:

```go
package maintenance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestEvaluate_TotalPackTrigger(t *testing.T) {
	body := manifest.Body{}
	for i := 0; i < 5; i++ {
		body.Packs = append(body.Packs, manifest.PackEntry{PackKey: "K" + string(rune('a'+i))})
	}
	thresh := Thresholds{TotalPackCount: 3}
	rep, err := evaluatePure(body, nil, thresh)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Triggered {
		t.Fatalf("expected triggered; got %+v", rep)
	}
	if rep.Reason != "total_pack_count" {
		t.Errorf("Reason = %q, want total_pack_count", rep.Reason)
	}
}

func TestEvaluate_ManifestPackBytesTrigger(t *testing.T) {
	body := manifest.Body{
		Packs: []manifest.PackEntry{{PackKey: "K", PackID: "ABCDEFG"}},
	}
	// Encode size of body.Packs alone.
	pb, _ := json.Marshal(body.Packs)
	thresh := Thresholds{ManifestPackBytes: int64(len(pb)) - 1}
	rep, err := evaluatePure(body, nil, thresh)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Triggered || rep.Reason != "manifest_pack_bytes" {
		t.Errorf("Reason = %q triggered = %v, want manifest_pack_bytes", rep.Reason, rep.Triggered)
	}
}

func TestEvaluate_NoTriggerIsZeroTrigger(t *testing.T) {
	rep, err := evaluatePure(manifest.Body{}, nil, Thresholds{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Triggered {
		t.Errorf("zero thresholds + empty body should not trigger; got %+v", rep)
	}
}

func TestEvaluate_RecentPackCountUsesObjectStoreMTime(t *testing.T) {
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := manifest.Body{
		Packs: []manifest.PackEntry{
			{PackKey: "tenants/acme/repos/site/packs/canonical/p1.pack"},
			{PackKey: "tenants/acme/repos/site/packs/canonical/p2.pack"},
		},
	}
	for _, p := range body.Packs {
		if _, err := s.Put(ctx, p.PackKey, bytesReader("x"), nil); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Add(time.Hour) // every Put will be older than now
	thresh := Thresholds{RecentPackCount: 1}
	rep, err := Evaluate(ctx, s, body, thresh, time.Hour, now) // 1h window
	if err != nil {
		t.Fatal(err)
	}
	if rep.RecentPackCount != 2 {
		t.Errorf("RecentPackCount = %d, want 2", rep.RecentPackCount)
	}
	if !rep.Triggered || rep.Reason != "recent_pack_count" {
		t.Errorf("Reason = %q, want recent_pack_count", rep.Reason)
	}
}

func bytesReader(s string) *bytesReaderT { return &bytesReaderT{s: s} }

type bytesReaderT struct {
	s string
	i int
}

func (b *bytesReaderT) Read(p []byte) (int, error) {
	if b.i >= len(b.s) {
		return 0, io.EOF
	}
	n := copy(p, b.s[b.i:])
	b.i += n
	return n, nil
}

// (add `import "io"` at top of file)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestEvaluate -v`
Expected: FAIL with undefined symbols.

- [ ] **Step 3: Implement thresholds.go**

Create `internal/maintenance/thresholds.go`:

```go
package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Evaluate computes a TriggerReport for body against thresh, with
// "recent" pack classification using the object store's reported
// creation_time relative to (now - recentWindow).
func Evaluate(ctx context.Context, s storage.ObjectStore, body manifest.Body, thresh Thresholds, recentWindow time.Duration, now time.Time) (TriggerReport, error) {
	cutoff := now.Add(-recentWindow)
	recent := 0
	for _, p := range body.Packs {
		md, err := s.Head(ctx, p.PackKey)
		if err != nil {
			return TriggerReport{}, fmt.Errorf("evaluate: head %s: %w", p.PackKey, err)
		}
		// Some adapters report CreationTime, others LastModified — for
		// canonical packs the two are interchangeable since canonical
		// packs are immutable. Prefer CreationTime; fall back to
		// LastModified if CreationTime is zero.
		t := md.CreationTime
		if t.IsZero() {
			t = md.LastModified
		}
		if t.After(cutoff) {
			recent++
		}
	}
	rep, err := evaluatePure(body, &recent, thresh)
	if err != nil {
		return TriggerReport{}, err
	}
	return rep, nil
}

// evaluatePure is the substrate that pure unit tests exercise. Pass
// recentOverride non-nil to inject a recent-pack count without a
// real object store.
func evaluatePure(body manifest.Body, recentOverride *int, thresh Thresholds) (TriggerReport, error) {
	pb, err := json.Marshal(body.Packs)
	if err != nil {
		return TriggerReport{}, fmt.Errorf("evaluate: marshal packs: %w", err)
	}
	rep := TriggerReport{
		TotalPackCount:    len(body.Packs),
		ManifestPackBytes: int64(len(pb)),
		Thresholds:        thresh,
	}
	if recentOverride != nil {
		rep.RecentPackCount = *recentOverride
	}
	switch {
	case thresh.RecentPackCount > 0 && rep.RecentPackCount > thresh.RecentPackCount:
		rep.Triggered, rep.Reason = true, "recent_pack_count"
	case thresh.TotalPackCount > 0 && rep.TotalPackCount > thresh.TotalPackCount:
		rep.Triggered, rep.Reason = true, "total_pack_count"
	case thresh.ManifestPackBytes > 0 && rep.ManifestPackBytes > thresh.ManifestPackBytes:
		rep.Triggered, rep.Reason = true, "manifest_pack_bytes"
	}
	return rep, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run TestEvaluate -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/thresholds.go internal/maintenance/thresholds_test.go
git commit -m "M9 task 7.1: §15.3 trigger evaluation (recent/total/manifest-pack-bytes)"
```

---

## Phase 8 — Pipeline orchestration

### Task 8.1: Implement `Run` (top-level entry point)

**Files:**
- Create: `internal/maintenance/run.go`
- Create: `internal/maintenance/run_test.go`
- Create: `internal/maintenance/pipeline.go`
- Create: `internal/maintenance/pipeline_test.go`

- [ ] **Step 1: Write the failing test (integration)**

Create `internal/maintenance/run_test.go`:

```go
package maintenance_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestRun_HappyPathConvergesToOnePack(t *testing.T) {
	mtest.GitAvailable(t)
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	// Use mtest.SeedRepoWithPushes to create a repo at "acme/site" with
	// N committed pushes, leaving N canonical packs in the manifest.
	rep := mtest.SeedRepoWithPushes(t, s, "acme", "site", 3)
	_ = rep // for clarity

	r, err := repo.Open(context.Background(), s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	opts := maintenance.RunOptions{Force: true}
	opts.Normalize()
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", report.Outcome)
	}
	if report.AfterPackCount != 1 {
		t.Errorf("AfterPackCount = %d, want 1", report.AfterPackCount)
	}
	if report.NewPackKey == "" {
		t.Errorf("NewPackKey empty")
	}

	// Re-read manifest; assert exactly one pack and Indexes set.
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Packs) != 1 {
		t.Errorf("post-Run manifest has %d packs, want 1", len(body.Packs))
	}
	if body.Indexes.ObjectMap == nil || body.Indexes.CommitGraph == nil {
		t.Errorf("post-Run manifest missing indexes")
	}
}

func TestRun_NoOpWhenThresholdsNotTriggered(t *testing.T) {
	mtest.GitAvailable(t)
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	mtest.SeedRepoWithPushes(t, s, "acme", "site", 1)

	r, err := repo.Open(context.Background(), s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	opts := maintenance.RunOptions{} // no Force; defaults
	opts.Normalize()
	report, err := maintenance.Run(context.Background(), s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "noop" {
		t.Errorf("Outcome = %q, want noop", report.Outcome)
	}
}
```

`mtest.SeedRepoWithPushes(t, store, tenant, repo, n)` is a fixture that initializes a repo (via `repo.Create`) and runs `n` synthetic push-equivalents through the importer's `BuildAndCommit` path so the manifest accumulates `n` canonical packs. Build it from the existing `mtest` patterns in this milestone; reuse importer's local-clone helpers.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run "TestRun_HappyPath|TestRun_NoOp" -v`
Expected: FAIL with `undefined: maintenance.Run`.

- [ ] **Step 3: Implement pipeline.go and run.go**

Create `internal/maintenance/pipeline.go`:

```go
package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"

	"github.com/oklog/ulid/v2"
)

// runPipeline executes phases 0–7 of the maintenance pipeline against
// one repo. Returns a populated Report or an error. Phase numbers in
// log fields match §4 of the spec.
func runPipeline(ctx context.Context, s storage.ObjectStore, r *repo.Repo, k *keys.Repo, opts RunOptions) (Report, error) {
	started := opts.Now()
	report := Report{
		RepoID: r.TenantID() + "/" + r.RepoID(),
		DryRun: opts.DryRun,
	}

	// Phase 0 — Load & gate
	view, err := r.ReadRoot(ctx)
	if err != nil {
		report.Outcome = "failed_other"
		return report, fmt.Errorf("phase0: read root: %w", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		report.Outcome = "failed_other"
		return report, fmt.Errorf("phase0: unmarshal body: %w", err)
	}
	report.ManifestVersionAt = view.Header.ManifestVersion
	report.BeforePackCount = len(body.Packs)

	pb, _ := json.Marshal(body.Packs)
	report.BeforeManifestPB = int64(len(pb))

	trig, err := Evaluate(ctx, s, body, opts.Thresholds, opts.RecentWindow, opts.Now())
	if err != nil {
		report.Outcome = "failed_other"
		return report, fmt.Errorf("phase0: evaluate: %w", err)
	}
	report.TriggerEval = trig

	if !opts.Force && !trig.Triggered {
		report.Outcome = "noop"
		report.AfterPackCount = report.BeforePackCount
		report.AfterManifestPB = report.BeforeManifestPB
		report.DurationMS = opts.Now().Sub(started).Milliseconds()
		return report, nil
	}
	if len(body.Refs) == 0 || len(body.Packs) == 0 {
		report.Outcome = "noop"
		report.AfterPackCount = report.BeforePackCount
		report.AfterManifestPB = report.BeforeManifestPB
		report.DurationMS = opts.Now().Sub(started).Milliseconds()
		return report, nil
	}

	t0Refs := body.Refs
	defaultBranch := body.DefaultBranch
	p0 := make([]PackRef, 0, len(body.Packs))
	p0Keys := make([]string, 0, len(body.Packs))
	for _, p := range body.Packs {
		p0 = append(p0, PackRef{PackKey: p.PackKey, IdxKey: p.IdxKey})
		p0Keys = append(p0Keys, p.PackKey)
	}
	sort.Strings(p0Keys)

	// Phase 1 — Materialize bare repo
	tmp, err := os.MkdirTemp("", "bucketvcs-maint-")
	if err != nil {
		report.Outcome = "failed_other"
		return report, fmt.Errorf("phase1: tmpdir: %w", err)
	}
	defer os.RemoveAll(tmp)

	if !opts.DryRun {
		if err := Materialize(ctx, s, MaterializeInput{
			BareDir:       tmp,
			Packs:         p0,
			Refs:          t0Refs,
			DefaultBranch: defaultBranch,
		}); err != nil {
			report.Outcome = classifyMaterializeErr(err)
			return report, fmt.Errorf("phase1: %w", err)
		}
	}

	// In dry-run mode: stop here. Report would_repack=true (because we
	// got past Phase 0 force/trigger gate), no writes, exit 0.
	if opts.DryRun {
		report.Outcome = "success"
		report.AfterPackCount = report.BeforePackCount
		report.AfterManifestPB = report.BeforeManifestPB
		report.DurationMS = opts.Now().Sub(started).Milliseconds()
		return report, nil
	}

	// Phase 2 — Repack
	repackOut, err := Repack(ctx, tmp)
	if err != nil {
		report.Outcome = "failed_pack_write"
		return report, fmt.Errorf("phase2: %w", err)
	}

	// Phase 3 — Index rebuild
	idx, err := buildIndexesFromLocalPack(ctx, repackOut.PackPath, repackOut.IdxPath, repackOut.PackID, t0Refs)
	if err != nil {
		report.Outcome = "failed_other"
		return report, fmt.Errorf("phase3: %w", err)
	}

	// Phase 4 — Upload artifacts
	uploaded, err := uploadArtifacts(ctx, s, k, uploadInput{
		PackID:           repackOut.PackID,
		PackPath:         repackOut.PackPath,
		IdxPath:          repackOut.IdxPath,
		ObjectMapHash:    idx.ObjectMapHash,
		ObjectMapBytes:   idx.ObjectMapBytes,
		CommitGraphHash:  idx.CommitGraphHash,
		CommitGraphBytes: idx.CommitGraphBytes,
	})
	if err != nil {
		report.Outcome = "failed_other"
		return report, fmt.Errorf("phase4: %w", err)
	}
	report.NewPackKey = uploaded.PackKey
	report.NewObjectMapKey = uploaded.ObjectMapKey
	report.NewCommitGraphKey = uploaded.CommitGraphKey
	report.NewPackBytes = repackOut.SizeBytes
	report.NewPackObjects = idx.ObjectCount
	report.RepackedPackKeys = p0Keys

	// Phase 5+6 — Tx record + CAS-merge (delegated to repo.Repo.Commit).
	// repo.Commit's callback receives a fresh prev on each retry, so
	// our buildBody runs the merge against the latest manifest.
	mergeIn := mergeInput{
		P0Keys: p0Keys,
		NewPack: manifest.PackEntry{
			PackID:      repackOut.PackID,
			PackKey:     uploaded.PackKey,
			IdxKey:      uploaded.IdxKey,
			SizeBytes:   repackOut.SizeBytes,
			ObjectCount: idx.ObjectCount,
		},
		NewObjectMap:   manifest.IndexRef{Key: uploaded.ObjectMapKey, Hash: idx.ObjectMapHash},
		NewCommitGraph: manifest.IndexRef{Key: uploaded.CommitGraphKey, Hash: idx.CommitGraphHash},
	}
	extraBytes, err := buildTxExtra(report, repackOut, idx, p0Keys, t0Refs)
	if err != nil {
		report.Outcome = "failed_other"
		return report, fmt.Errorf("phase5: build tx extra: %w", err)
	}
	txBody := tx.Body{Type: "maintenance", Actor: opts.Actor, Extra: extraBytes}

	attempts := 0
	_, err = r.Commit(ctx, txBody, func(prev *repo.RootView) ([]byte, error) {
		attempts++
		var prevBody manifest.Body
		if perr := json.Unmarshal(prev.Body, &prevBody); perr != nil {
			return nil, perr
		}
		merged := buildMergedBody(prevBody, mergeIn)
		return manifest.MarshalBody(merged)
	}, repo.WithCommitPolicy(repo.CommitPolicy{MaxRetries: opts.CASRetry}))
	report.CASAttempts = attempts
	if err != nil {
		report.Outcome = "failed_cas"
		return report, fmt.Errorf("%w: %v", ErrCASExhausted, err)
	}

	// Phase 7 — Refresh report from post-commit manifest.
	postView, err := r.ReadRoot(ctx)
	if err == nil {
		report.ManifestVersionTo = postView.Header.ManifestVersion
		var postBody manifest.Body
		if perr := json.Unmarshal(postView.Body, &postBody); perr == nil {
			report.AfterPackCount = len(postBody.Packs)
			postPB, _ := json.Marshal(postBody.Packs)
			report.AfterManifestPB = int64(len(postPB))
		}
	}
	report.Outcome = "success"
	report.DurationMS = opts.Now().Sub(started).Milliseconds()
	return report, nil
}

func classifyMaterializeErr(err error) string {
	if err == nil {
		return "success"
	}
	if errIs(err, ErrCorruptInput) {
		return "failed_walk" // fsck corruption maps to "walk" in metric labels
	}
	return "failed_other"
}

// errIs avoids an explicit errors import here when the only use is one
// errors.Is — keeps imports tight for tooling that complains.
func errIs(err, target error) bool {
	type isser interface{ Is(error) bool }
	for e := err; e != nil; {
		if e == target {
			return true
		}
		if x, ok := e.(isser); ok && x.Is(target) {
			return true
		}
		type unwrap interface{ Unwrap() error }
		if u, ok := e.(unwrap); ok {
			e = u.Unwrap()
			continue
		}
		break
	}
	return false
}

// buildTxExtra serializes the audit/extra block for the tx record per
// §4.6 of the spec (m0_version, ref tip snapshot, repacked pack keys,
// new artifact metadata).
func buildTxExtra(rep Report, repackOut *RepackOutput, idx *LocalIndexes, p0Keys []string, t0Refs map[string]string) ([]byte, error) {
	type indexRefAudit struct{ Key, Hash string }
	type extra struct {
		M0Version           uint64            `json:"m0_version"`
		RefTipSnapshot      map[string]string `json:"ref_tip_snapshot"`
		RepackedPackKeys    []string          `json:"repacked_pack_keys"`
		NewPackKey          string            `json:"new_pack_key"`
		NewPackHash         string            `json:"new_pack_hash"`
		NewPackSizeBytes    int64             `json:"new_pack_size_bytes"`
		NewPackObjectCount  int               `json:"new_pack_object_count"`
		NewObjectMap        indexRefAudit     `json:"new_object_map"`
		NewCommitGraph      indexRefAudit     `json:"new_commit_graph"`
		RunStartedAtUnixSec int64             `json:"run_started_at"`
	}
	e := extra{
		M0Version:           rep.ManifestVersionAt,
		RefTipSnapshot:      t0Refs,
		RepackedPackKeys:    p0Keys,
		NewPackKey:          rep.NewPackKey,
		NewPackHash:         repackOut.PackID,
		NewPackSizeBytes:    repackOut.SizeBytes,
		NewPackObjectCount:  idx.ObjectCount,
		NewObjectMap:        indexRefAudit{Key: rep.NewObjectMapKey, Hash: idx.ObjectMapHash},
		NewCommitGraph:      indexRefAudit{Key: rep.NewCommitGraphKey, Hash: idx.CommitGraphHash},
		RunStartedAtUnixSec: time.Now().Unix(),
	}
	return json.Marshal(e)
}

var _ = ulid.Make // touch import; ULID for tx_id is minted by repo.Commit
```

Create `internal/maintenance/run.go`:

```go
package maintenance

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Run executes one maintenance pass against one open repo. Caller is
// responsible for opening the repo (repo.Open) and constructing the
// keys.Repo handle (keys.NewRepo).
//
// Run normalizes opts, validates them, and delegates to runPipeline.
// The returned Report is populated even on failure (Outcome will name
// the failure class), so the caller can render diagnostics uniformly.
func Run(ctx context.Context, s storage.ObjectStore, r *repo.Repo, k *keys.Repo, opts RunOptions) (Report, error) {
	opts.Normalize()
	if err := opts.Validate(); err != nil {
		return Report{
			RepoID:  r.TenantID() + "/" + r.RepoID(),
			Outcome: "failed_other",
		}, err
	}
	return runPipeline(ctx, s, r, k, opts)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run "TestRun_HappyPath|TestRun_NoOp" -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/run.go internal/maintenance/run_test.go internal/maintenance/pipeline.go internal/maintenance/mtest/
git commit -m "M9 task 8.1: Run + runPipeline orchestrating phases 0-7 against one repo"
```

### Task 8.2: Test the CAS-merge under concurrent push

**Files:**
- Modify: `internal/maintenance/run_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/maintenance/run_test.go`:

```go
func TestRun_PushDuringPipelinePreservesLatePack(t *testing.T) {
	mtest.GitAvailable(t)
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	mtest.SeedRepoWithPushes(t, s, "acme", "site", 2)

	r, err := repo.Open(context.Background(), s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Inject a hook: BetweenRepackAndCAS executes a fresh push before
	// runPipeline reaches Phase 5+6. Implementation choice: the hook
	// is a function field on RunOptions, default nil. We only set it
	// for this test.
	opts := maintenance.RunOptions{
		Force: true,
		BetweenRepackAndCAS: func() {
			mtest.SeedOneAdditionalPush(t, s, "acme", "site")
		},
	}
	opts.Normalize()
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", report.Outcome)
	}
	if report.AfterPackCount != 2 {
		t.Errorf("AfterPackCount = %d, want 2 (1 repacked + 1 late push)", report.AfterPackCount)
	}
	if report.CASAttempts < 2 {
		t.Errorf("CASAttempts = %d, want >= 2 (first attempt should hit a version mismatch)", report.CASAttempts)
	}
}
```

`mtest.SeedOneAdditionalPush` is a fresh push that lands one new canonical pack on the existing repo without intersecting the previous pushes' object set.

- [ ] **Step 2: Add `BetweenRepackAndCAS` test hook to RunOptions**

Modify `internal/maintenance/options.go`: add an unexported-but-test-accessible function field. Use `//nolint:godox` style or just keep it exported with a `// Test hook only` comment. The cleanest pattern is to add a non-exported field and an `internal_test.go` shim, but the simpler approach is exported with a clear doc comment:

```go
// BetweenRepackAndCAS is a test hook invoked at the start of Phase 5
// (after repack, before tx record + CAS-merge). Production callers
// leave it nil.
BetweenRepackAndCAS func() `json:"-"`
```

Add a corresponding call in pipeline.go just before constructing the `mergeIn` block.

- [ ] **Step 3: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run TestRun_PushDuringPipeline -v`
Expected: PASS. CAS attempts should be ≥ 2 because the late push bumped the version after Phase 0 read.

- [ ] **Step 4: Commit**

```bash
git add internal/maintenance/run_test.go internal/maintenance/options.go internal/maintenance/pipeline.go
git commit -m "M9 task 8.2: CAS-merge test confirms late push pack is preserved across maintenance"
```

---

## Phase 9 — CLI subcommand

### Task 9.1: Implement `bucketvcs maintenance` flag parser

**Files:**
- Create: `cmd/bucketvcs/maintenance.go`
- Create: `cmd/bucketvcs/maintenance_test.go`

- [ ] **Step 1: Read cmd/bucketvcs/gc.go end-to-end as reference**

Run: `cat /home/eran/work/bucketvcs/cmd/bucketvcs/gc.go`

The maintenance subcommand mirrors gc's structure: flag set, store URL handling, single-repo vs all-repos discriminator, JSON/text output, exit codes 0/1/2.

- [ ] **Step 2: Write the failing test (flag validation)**

Create `cmd/bucketvcs/maintenance_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunMaintenance_RejectsMissingStoreFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(), []string{"--repo=acme/site"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "store") {
		t.Errorf("stderr does not mention --store: %q", stderr.String())
	}
}

func TestRunMaintenance_RejectsBothRepoAndAllRepos(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(),
		[]string{"--store=mem://", "--repo=acme/site", "--all-repos"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRunMaintenance_RejectsSubHourRecentWindow(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(),
		[]string{"--store=mem://", "--repo=acme/site", "--recent-window=30m"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "1h") {
		t.Errorf("stderr does not mention 1h minimum: %q", stderr.String())
	}
}

func TestRunMaintenance_HelpFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "maintenance") {
		t.Errorf("usage missing 'maintenance': %q", stdout.String())
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./cmd/bucketvcs/ -run TestRunMaintenance -v`
Expected: FAIL with `undefined: runMaintenance`.

- [ ] **Step 4: Implement maintenance.go**

Create `cmd/bucketvcs/maintenance.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

const maintenanceUsage = `usage: bucketvcs maintenance --store=<URL> {--repo=<t>/<r> | --all-repos} [flags]

Run a single full repack against one repo (or every repo discovered
under tenants/*/repos/*). Default thresholds match spec §15.3
recommendations; --force runs unconditionally.

Flags:
  --store=URL                       Storage URL (required)
  --repo=<tenant>/<repo>            Single repo (mutex with --all-repos)
  --all-repos                       Process every discovered repo
  --force                           Skip threshold check
  --dry-run                         Walk + plan only; no writes
  --recent-pack-threshold=N         Default 1000 (0 disables)
  --total-pack-threshold=N          Default 10000 (0 disables)
  --manifest-pack-bytes-threshold=N Default 8388608 (0 disables)
  --recent-window=DURATION          Default 24h, minimum 1h
  --cas-retry=N                     Default 5
  --output=text|json                Default text
  --help                            Show this help

Exit codes:
  0 success or dry-run completed
  1 at least one repo failed (incl. CAS exhaustion)
  2 invalid flags
`

func runMaintenance(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("maintenance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stdout, maintenanceUsage) }

	storeURL := fs.String("store", "", "Storage URL (required)")
	repoFlag := fs.String("repo", "", "<tenant>/<repo>")
	allRepos := fs.Bool("all-repos", false, "Process every repo discovered under tenants/*/repos/*")
	force := fs.Bool("force", false, "Skip threshold check")
	dryRun := fs.Bool("dry-run", false, "Walk + plan; no writes")
	recentPackT := fs.Int("recent-pack-threshold", 1000, "")
	totalPackT := fs.Int("total-pack-threshold", 10000, "")
	manifestPackBytesT := fs.Int64("manifest-pack-bytes-threshold", 8<<20, "")
	recentWindow := fs.Duration("recent-window", 24*time.Hour, "")
	casRetry := fs.Int("cas-retry", maintenance.DefaultCASRetry, "")
	output := fs.String("output", "text", "text|json")
	help := fs.Bool("help", false, "")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *help {
		fmt.Fprint(stdout, maintenanceUsage)
		return 0
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "maintenance: --store is required")
		return 2
	}
	if *repoFlag == "" && !*allRepos {
		fmt.Fprintln(stderr, "maintenance: one of --repo or --all-repos is required")
		return 2
	}
	if *repoFlag != "" && *allRepos {
		fmt.Fprintln(stderr, "maintenance: --repo and --all-repos are mutually exclusive")
		return 2
	}
	if *recentWindow < time.Hour {
		fmt.Fprintf(stderr, "maintenance: --recent-window=%s is below the 1h minimum\n", *recentWindow)
		return 2
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintf(stderr, "maintenance: --output=%q must be text|json\n", *output)
		return 2
	}

	store, err := openStoreFromURL(*storeURL) // shared helper; see cmd/bucketvcs/store.go
	if err != nil {
		fmt.Fprintf(stderr, "maintenance: open store: %v\n", err)
		return 1
	}

	opts := maintenance.RunOptions{
		Thresholds: maintenance.Thresholds{
			RecentPackCount:   *recentPackT,
			TotalPackCount:    *totalPackT,
			ManifestPackBytes: *manifestPackBytesT,
		},
		RecentWindow: *recentWindow,
		CASRetry:     *casRetry,
		Force:        *force,
		DryRun:       *dryRun,
		Logger:       slog.Default(),
	}
	opts.Normalize()

	if *repoFlag != "" {
		t, r, ok := splitRepoFlag(*repoFlag)
		if !ok {
			fmt.Fprintf(stderr, "maintenance: --repo=%q must be <tenant>/<repo>\n", *repoFlag)
			return 2
		}
		return runMaintenanceOne(ctx, store, t, r, opts, stdout, stderr, *output)
	}

	return runMaintenanceAll(ctx, store, opts, stdout, stderr, *output)
}

func runMaintenanceOne(ctx context.Context, store storage.ObjectStore, tenantID, repoID string, opts maintenance.RunOptions, stdout, stderr io.Writer, output string) int {
	r, err := repo.Open(ctx, store, tenantID, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "maintenance: open repo %s/%s: %v\n", tenantID, repoID, err)
		return 1
	}
	k, err := keys.NewRepo(tenantID, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "maintenance: keys: %v\n", err)
		return 1
	}
	rep, err := maintenance.Run(ctx, store, r, k, opts)
	if err != nil {
		fmt.Fprintf(stderr, "maintenance: %s/%s: %v\n", tenantID, repoID, err)
		emitReport(stdout, []maintenance.Report{rep}, output)
		return 1
	}
	emitReport(stdout, []maintenance.Report{rep}, output)
	return 0
}

func runMaintenanceAll(ctx context.Context, store storage.ObjectStore, opts maintenance.RunOptions, stdout, stderr io.Writer, output string) int {
	repos, err := maintenance.DiscoverRepos(ctx, store)
	if err != nil {
		fmt.Fprintf(stderr, "maintenance: discover repos: %v\n", err)
		return 1
	}
	exit := 0
	reports := make([]maintenance.Report, 0, len(repos))
	for _, ref := range repos {
		r, err := repo.Open(ctx, store, ref.TenantID, ref.RepoID)
		if err != nil {
			fmt.Fprintf(stderr, "maintenance: open %s/%s: %v\n", ref.TenantID, ref.RepoID, err)
			exit = 1
			reports = append(reports, maintenance.Report{
				RepoID: ref.TenantID + "/" + ref.RepoID, Outcome: "failed_other",
			})
			continue
		}
		k, err := keys.NewRepo(ref.TenantID, ref.RepoID)
		if err != nil {
			fmt.Fprintf(stderr, "maintenance: keys %s/%s: %v\n", ref.TenantID, ref.RepoID, err)
			exit = 1
			continue
		}
		rep, err := maintenance.Run(ctx, store, r, k, opts)
		if err != nil {
			fmt.Fprintf(stderr, "maintenance: %s/%s: %v\n", ref.TenantID, ref.RepoID, err)
			exit = 1
		}
		reports = append(reports, rep)
	}
	emitReport(stdout, reports, output)
	return exit
}

func emitReport(w io.Writer, reports []maintenance.Report, output string) {
	if output == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(reports)
		return
	}
	for _, r := range reports {
		marker := ""
		if r.DryRun {
			marker = "[DRY RUN] "
		}
		fmt.Fprintf(w, "%s%s: outcome=%s pack_count=%d→%d manifest_pack_bytes=%d→%d cas_attempts=%d duration=%dms",
			marker, r.RepoID, r.Outcome, r.BeforePackCount, r.AfterPackCount,
			r.BeforeManifestPB, r.AfterManifestPB, r.CASAttempts, r.DurationMS)
		if r.TriggerEval.Reason != "" {
			fmt.Fprintf(w, " trigger=%s", r.TriggerEval.Reason)
		}
		fmt.Fprintln(w)
	}
}

func splitRepoFlag(s string) (string, string, bool) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

var _ = os.Stdout // keep import for future use
```

`openStoreFromURL` already exists in `cmd/bucketvcs/store.go` (used by `gc`); reuse it.

- [ ] **Step 5: Wire the dispatcher**

Modify `cmd/bucketvcs/main.go`: locate the dispatch table that maps subcommand names to functions (the same place where `gc` is wired), and add `"maintenance": runMaintenance`. Identifying line: `grep -n '"gc"' /home/eran/work/bucketvcs/cmd/bucketvcs/main.go`.

- [ ] **Step 6: Run the tests**

Run: `go test ./cmd/bucketvcs/ -run TestRunMaintenance -v`
Expected: PASS (4 tests).

- [ ] **Step 7: Commit**

```bash
git add cmd/bucketvcs/maintenance.go cmd/bucketvcs/maintenance_test.go cmd/bucketvcs/main.go
git commit -m "M9 task 9.1: bucketvcs maintenance CLI subcommand with flag validation + JSON/text output"
```

### Task 9.2: End-to-end CLI test against localfs

**Files:**
- Modify: `cmd/bucketvcs/maintenance_test.go`

- [ ] **Step 1: Add the failing test**

Append to `cmd/bucketvcs/maintenance_test.go`:

```go
func TestRunMaintenance_E2E_ConvergesToOnePack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	storeDir := t.TempDir()
	storeURL := "file://" + storeDir
	// Seed three pushes via the same helpers maintenance/run_test.go
	// uses; expose them through a small shim if needed.
	mtest.SeedRepoWithPushesAtURL(t, storeURL, "acme", "site", 3)

	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(),
		[]string{"--store=" + storeURL, "--repo=acme/site", "--force", "--output=json"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d (stderr=%s)", code, stderr.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json: %v\nstdout=%s", err, stdout.String())
	}
	if len(out) != 1 {
		t.Fatalf("got %d reports, want 1", len(out))
	}
	if out[0]["outcome"] != "success" {
		t.Errorf("outcome = %v, want success", out[0]["outcome"])
	}
	if int(out[0]["after_pack_count"].(float64)) != 1 {
		t.Errorf("after_pack_count = %v, want 1", out[0]["after_pack_count"])
	}
}
```

(Add `"os/exec"`, `"encoding/json"`, `"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"` imports.)

- [ ] **Step 2: Run the test**

Run: `go test ./cmd/bucketvcs/ -run TestRunMaintenance_E2E -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add cmd/bucketvcs/maintenance_test.go internal/maintenance/mtest/fixtures.go
git commit -m "M9 task 9.2: e2e CLI test (3 pushes -> single canonical pack)"
```

---

## Phase 10 — Multi-repo discovery (`--all-repos`)

### Task 10.1: Port `internal/gc/multirepo.go` patterns

**Files:**
- Create: `internal/maintenance/multirepo.go`
- Create: `internal/maintenance/multirepo_test.go`

- [ ] **Step 1: Read internal/gc/multirepo.go**

Run: `cat /home/eran/work/bucketvcs/internal/gc/multirepo.go`

The same `DiscoverRepos` + `RepoRef` types apply. Decision: duplicate them inside `internal/maintenance/` (mirroring the M8 spec rationale that each milestone owns its multi-repo discovery, so multi-repo bugs in one don't accidentally affect the other). The duplication is small (~30 lines).

- [ ] **Step 2: Write the failing test**

Create `internal/maintenance/multirepo_test.go`:

```go
package maintenance

import (
	"bytes"
	"context"
	"sort"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestDiscoverRepos_FindsAllUnderTenants(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{
		"tenants/acme/repos/site/manifest/root.json",
		"tenants/acme/repos/api/manifest/root.json",
		"tenants/contoso/repos/web/manifest/root.json",
		"random/object", // should not be discovered
	} {
		if _, err := s.Put(ctx, key, bytes.NewReader([]byte("{}")), nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DiscoverRepos(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].TenantID != got[j].TenantID {
			return got[i].TenantID < got[j].TenantID
		}
		return got[i].RepoID < got[j].RepoID
	})
	want := []RepoRef{
		{TenantID: "acme", RepoID: "api"},
		{TenantID: "acme", RepoID: "site"},
		{TenantID: "contoso", RepoID: "web"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, r := range want {
		if got[i] != r {
			t.Errorf("repo[%d] = %+v, want %+v", i, got[i], r)
		}
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run TestDiscoverRepos -v`
Expected: FAIL.

- [ ] **Step 4: Implement multirepo.go**

Create `internal/maintenance/multirepo.go` with the same content as `internal/gc/multirepo.go` (the `RepoRef` type, `DiscoverRepos`, and the `listCommonPrefixes` helper — copy verbatim, change package name).

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/maintenance/ -run TestDiscoverRepos -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/maintenance/multirepo.go internal/maintenance/multirepo_test.go
git commit -m "M9 task 10.1: DiscoverRepos for --all-repos (mirrors internal/gc/multirepo.go)"
```

---

## Phase 11 — Audit + metrics emission

### Task 11.1: slog event builders

**Files:**
- Create: `internal/maintenance/log.go`
- Create: `internal/maintenance/log_test.go`
- Modify: `internal/maintenance/pipeline.go` (call into log.go at start + end)

- [ ] **Step 1: Read internal/gc/log.go**

Run: `cat /home/eran/work/bucketvcs/internal/gc/log.go`

Maintenance mirrors gc's audit/metric emission shape: `audit=true`-tagged events for the durable trail, `metric_name=...` tagged events for §32 metrics. Defining helpers as `emitStarted`, `emitCompleted`, `emitMetric` keeps call sites short.

- [ ] **Step 2: Write the failing test**

Create `internal/maintenance/log_test.go`:

```go
package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestEmitStarted_AuditTagged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitStarted(context.Background(), logger, Report{RepoID: "a/b", ManifestVersionAt: 7}, false)

	line := buf.String()
	if !strings.Contains(line, `"audit":true`) {
		t.Errorf("missing audit=true tag: %s", line)
	}
	if !strings.Contains(line, `"event":"maintenance.started"`) {
		t.Errorf("missing event tag: %s", line)
	}
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if entry["repo_id"] != "a/b" {
		t.Errorf("repo_id = %v, want a/b", entry["repo_id"])
	}
}

func TestEmitMetric_HasMetricNameField(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitMetric(context.Background(), logger, "maintenance_runs_total", 1, "outcome", "success")
	line := buf.String()
	if !strings.Contains(line, `"metric_name":"maintenance_runs_total"`) {
		t.Errorf("missing metric_name: %s", line)
	}
	if !strings.Contains(line, `"value":1`) {
		t.Errorf("missing value: %s", line)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/maintenance/ -run "TestEmit" -v`
Expected: FAIL.

- [ ] **Step 4: Implement log.go**

Create `internal/maintenance/log.go`:

```go
package maintenance

import (
	"context"
	"log/slog"
)

func emitStarted(ctx context.Context, logger *slog.Logger, r Report, dryRun bool) {
	logger.LogAttrs(ctx, slog.LevelInfo, "maintenance.started",
		slog.Bool("audit", true),
		slog.String("event", "maintenance.started"),
		slog.String("repo_id", r.RepoID),
		slog.Uint64("manifest_version_at_start", r.ManifestVersionAt),
		slog.Bool("dry_run", dryRun),
		slog.Any("threshold_eval", r.TriggerEval),
	)
}

func emitCompleted(ctx context.Context, logger *slog.Logger, r Report) {
	logger.LogAttrs(ctx, slog.LevelInfo, "maintenance.completed",
		slog.Bool("audit", true),
		slog.String("event", "maintenance.completed"),
		slog.String("repo_id", r.RepoID),
		slog.String("outcome", r.Outcome),
		slog.Int("before_pack_count", r.BeforePackCount),
		slog.Int("after_pack_count", r.AfterPackCount),
		slog.Int64("before_manifest_pack_bytes", r.BeforeManifestPB),
		slog.Int64("after_manifest_pack_bytes", r.AfterManifestPB),
		slog.String("new_pack_key", r.NewPackKey),
		slog.Int("new_pack_objects", r.NewPackObjects),
		slog.String("new_object_map_key", r.NewObjectMapKey),
		slog.String("new_commit_graph_key", r.NewCommitGraphKey),
		slog.Any("repacked_pack_keys", r.RepackedPackKeys),
		slog.Int("cas_attempts", r.CASAttempts),
		slog.Int64("duration_ms", r.DurationMS),
		slog.Bool("dry_run", r.DryRun),
	)
}

func emitMetric(ctx context.Context, logger *slog.Logger, name string, value int64, kvs ...any) {
	attrs := []slog.Attr{
		slog.String("metric_name", name),
		slog.Int64("value", value),
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		k, _ := kvs[i].(string)
		attrs = append(attrs, slog.Any(k, kvs[i+1]))
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric", attrs...)
}
```

- [ ] **Step 5: Wire into pipeline.go**

Modify `internal/maintenance/pipeline.go` `runPipeline`:

- After Phase 0's read+evaluate, call `emitStarted(ctx, opts.Logger, report, opts.DryRun)`.
- Just before each `return report, ...` add `emitCompleted(ctx, opts.Logger, report)` and per-outcome metric emissions:

```go
emitMetric(ctx, opts.Logger, "maintenance_runs_total", 1, "outcome", report.Outcome)
emitMetric(ctx, opts.Logger, "maintenance_run_duration_seconds", report.DurationMS/1000, "outcome", report.Outcome)
emitMetric(ctx, opts.Logger, "maintenance_threshold_recent_pack_count", int64(report.TriggerEval.RecentPackCount))
emitMetric(ctx, opts.Logger, "maintenance_threshold_total_pack_count", int64(report.TriggerEval.TotalPackCount))
emitMetric(ctx, opts.Logger, "maintenance_threshold_manifest_pack_bytes", report.TriggerEval.ManifestPackBytes)
if report.NewPackBytes > 0 {
    emitMetric(ctx, opts.Logger, "maintenance_pack_bytes_out", report.NewPackBytes)
    emitMetric(ctx, opts.Logger, "maintenance_objects_packed_total", int64(report.NewPackObjects))
}
if report.CASAttempts > 0 {
    emitMetric(ctx, opts.Logger, "maintenance_cas_attempts", int64(report.CASAttempts))
}
```

Ensure `report.DurationMS` is set on every exit path.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/maintenance/...`
Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add internal/maintenance/log.go internal/maintenance/log_test.go internal/maintenance/pipeline.go
git commit -m "M9 task 11.1: audit + metric emission via slog (started, completed, runs_total, ...)"
```

---

## Phase 12 — Conformance: `RunPropertyMaintenanceSafety`

### Task 12.1: Property test scaffold

**Files:**
- Create: `internal/maintenance/conformance/safety.go`
- Create: `internal/maintenance/conformance/safety_test.go`

- [ ] **Step 1: Read internal/gc/conformance/ for the factory pattern**

Run: `ls /home/eran/work/bucketvcs/internal/gc/conformance/ && cat /home/eran/work/bucketvcs/internal/gc/conformance/*.go | head -80`

The same factory shape applies: `RunProperty<X>(t *testing.T, factory func(*testing.T) storage.ObjectStore)`. Localfs and each cloud adapter then call this via their own conformance test files.

- [ ] **Step 2: Write the failing test (drives the API)**

Create `internal/maintenance/conformance/safety_test.go`:

```go
package conformance_test

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance/conformance"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestRunPropertyMaintenanceSafety_LocalFS(t *testing.T) {
	conformance.RunPropertyMaintenanceSafety(t, func(t *testing.T) storage.ObjectStore {
		dir := t.TempDir()
		s, err := localfs.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
```

- [ ] **Step 3: Implement safety.go with all four interleavings**

Create `internal/maintenance/conformance/safety.go`. The function signature:

```go
func RunPropertyMaintenanceSafety(t *testing.T, factory func(*testing.T) storage.ObjectStore)
```

The body runs four sub-tests via `t.Run`. Each sub-test:

1. Creates a fresh store via `factory(t)`.
2. Seeds a synthetic repo via `mtest.SeedRepoWithPushes` (variant: with N pushes specific to the scenario).
3. Runs the scenario (solo, push-during-walk, gc-during-retention, two-maintenances-racing).
4. Asserts the invariant: every commit referenced by `manifest.Refs` is reachable through `manifest.Packs`.

The "reachable through manifest.Packs" check downloads the manifest's referenced packs into a temp bare repo, runs `git fsck --full`, and runs `git rev-list --objects <ref-tip>` for each ref to confirm full-closure presence.

```go
package conformance

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// RunPropertyMaintenanceSafety verifies the §43.6-style invariant
// against a caller-provided ObjectStore factory. Four scenarios:
//
//   solo                — single maintenance run; manifest converges.
//   push_during_walk    — push lands between repack and CAS-merge.
//   gc_during_retention — gc runs while old packs in retention.
//   two_maintenances    — two maintenances race the CAS.
//
// Each scenario seeds a fresh repo via mtest.SeedRepoWithPushes(...)
// then runs the workflow and asserts every ref is fsck-reachable
// through the post-workflow manifest's pack set.
func RunPropertyMaintenanceSafety(t *testing.T, factory func(*testing.T) storage.ObjectStore) {
	t.Run("solo", func(t *testing.T) { runSolo(t, factory(t)) })
	t.Run("push_during_walk", func(t *testing.T) { runPushDuringWalk(t, factory(t)) })
	t.Run("gc_during_retention", func(t *testing.T) { runGCDuringRetention(t, factory(t)) })
	t.Run("two_maintenances", func(t *testing.T) { runTwoMaintenances(t, factory(t)) })
}

// (Implementations of runSolo, runPushDuringWalk, runGCDuringRetention,
// runTwoMaintenances follow. Each is ~30-50 lines.)
```

Implement each scenario inline. The shared "assert reachability" helper materializes the post-workflow manifest into a bare repo and runs fsck.

- [ ] **Step 4: Run localfs conformance**

Run: `go test ./internal/maintenance/conformance/...`
Expected: PASS for all four sub-tests.

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/conformance/
git commit -m "M9 task 12.1: RunPropertyMaintenanceSafety + 4 interleaving scenarios on localfs"
```

### Task 12.2: Wire into the cross-adapter aggregator

**Files:**
- Modify: `internal/storage/conformance/suite.go` (or the canonical aggregator file — confirm with `grep`)
- Modify: `internal/storage/s3compat/*_test.go`, `internal/storage/gcs/*_test.go`, `internal/storage/azureblob/*_test.go` (one call site each)

- [ ] **Step 1: Locate the aggregator pattern**

Run: `grep -rn "RunPropertyGCSafety" /home/eran/work/bucketvcs/internal/storage/ --include="*.go" | head`

The pattern from M8 — each adapter's existing test file calls into a function that runs all the property suites against the adapter's ObjectStore factory. M9 adds one more entry point.

- [ ] **Step 2: Add the wiring**

In each adapter's existing `*_test.go` (e.g. `internal/storage/localfs/conformance_test.go`), add a sibling test:

```go
func TestMaintenanceSafety(t *testing.T) {
	maintconformance.RunPropertyMaintenanceSafety(t, func(t *testing.T) storage.ObjectStore {
		// existing factory body
	})
}
```

(Import alias `maintconformance "github.com/bucketvcs/bucketvcs/internal/maintenance/conformance"`.)

For cloud adapters: the property suite runs only when the adapter test runs (i.e. when the corresponding emulator or real-cloud secrets are available). Same gating posture as M8's RunPropertyGCSafety.

- [ ] **Step 3: Run all adapter conformance suites**

Run: `go test ./internal/storage/...`
Expected: PASS for localfs and any adapter whose emulator is available.

- [ ] **Step 4: Commit**

```bash
git add internal/storage/
git commit -m "M9 task 12.2: wire RunPropertyMaintenanceSafety into each adapter test"
```

---

## Phase 13 — Differential harness

### Task 13.1: Add `Import → Maintenance → Export` equivalence test

**Files:**
- Modify: `internal/diffharness/diffharness_test.go`

- [ ] **Step 1: Read the existing harness shape**

Run: `head -120 /home/eran/work/bucketvcs/internal/diffharness/diffharness_test.go`

Identify the existing import→export round-trip test; the new test mirrors its setup but adds a `maintenance.Run` step in the middle.

- [ ] **Step 2: Add the failing test**

Append to `internal/diffharness/diffharness_test.go`:

```go
func TestImport_Maintenance_Export_PreservesObjectInventory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Existing import setup: builds a synthetic source repo, calls
	// importer.Import, returns (store, tenantID, repoID, srcDir).
	src := buildSyntheticSourceRepo(t, /* n_commits */ 25)

	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := importer.Import(ctx, store, importer.Options{
		TenantID: "acme", RepoID: "site", SourceDir: src, Actor: "u_test",
	}); err != nil {
		t.Fatal(err)
	}

	// Maintenance.
	r, err := repo.Open(ctx, store, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	opts := maintenance.RunOptions{Force: true}
	opts.Normalize()
	if _, err := maintenance.Run(ctx, store, r, k, opts); err != nil {
		t.Fatal(err)
	}

	// Export.
	dst := t.TempDir()
	if err := exporter.Export(ctx, store, exporter.Options{
		TenantID: "acme", RepoID: "site", DestDir: dst,
	}); err != nil {
		t.Fatal(err)
	}

	// Equivalence: rev-list --all --objects on src vs. dst yields
	// identical object inventories.
	srcInv := mustRevListObjects(t, src)
	dstInv := mustRevListObjects(t, dst)
	if !equalObjectInventories(srcInv, dstInv) {
		t.Errorf("object inventories diverged:\nsrc=%v\ndst=%v", srcInv, dstInv)
	}

	// Bonus: dst is fsck-clean.
	cmd := exec.Command("git", "--git-dir="+dst, "fsck", "--full")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dst fsck: %v\n%s", err, out)
	}
}
```

`equalObjectInventories` and `mustRevListObjects` are small helpers that parse `git rev-list --all --objects` output into a sorted set of OIDs (ignoring path strings, which `pack-objects` may reorder).

- [ ] **Step 3: Run the test**

Run: `go test ./internal/diffharness/ -run TestImport_Maintenance_Export -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/diffharness/
git commit -m "M9 task 13.1: differential harness test — import->maintenance->export preserves inventory"
```

---

## Phase 14 — Documentation

### Task 14.1: Write `docs/m9-maintenance-operator-guide.md`

**Files:**
- Create: `docs/m9-maintenance-operator-guide.md`

- [ ] **Step 1: Read docs/m8-gc-operator-guide.md**

Run: `wc -l /home/eran/work/bucketvcs/docs/m8-gc-operator-guide.md && head -60 /home/eran/work/bucketvcs/docs/m8-gc-operator-guide.md`

Mirror its structure: short prose intro, scheduling recipes (cron/CronJob/systemd), threshold tuning section with worked examples, before/after walkthrough, JSON output schema, failure-mode runbook.

- [ ] **Step 2: Draft the guide**

Create `docs/m9-maintenance-operator-guide.md` with these sections:

1. **What `bucketvcs maintenance` does** — one-paragraph summary mirroring spec §0.
2. **When to run it** — manual cadence guidance: small repo (weekly), medium (daily), hot-large (every 6h or post-bulk-import).
3. **Scheduling**: cron, Kubernetes CronJob, systemd timer recipes. Include the order with `bucketvcs gc` (run `maintenance` first, then `gc`).
4. **Thresholds**: explain each, give worked numbers for small/medium/hot-large profiles.
5. **What changes after a successful run** — manifest pack list collapses to 1 entry, indexes refresh, old packs become unreachable, M8 GC reclaims after retention.
6. **JSON output schema**: field-by-field, suitable for log aggregation.
7. **Failure runbook**: CAS exhaustion (re-run; the previous attempt's artifacts are orphan candidates for the next gc), fsck failure (manifest references corrupt packs — see m8-gc-operator-guide.md §X), partial upload (gc reclaims).
8. **Interaction with `bucketvcs gc`**: explicit before/after mental model and the "concurrent runs are safe but waste IO" note.

Aim for ~250 lines of documentation. Cover the ops-team mental model as concretely as the M8 guide does.

- [ ] **Step 3: Commit**

```bash
git add docs/m9-maintenance-operator-guide.md
git commit -m "M9 task 14.1: operator guide with scheduling recipes, threshold tuning, runbook"
```

### Task 14.2: Update quickstarts and README

**Files:**
- Modify: `docs/m5-cloud-quickstart.md`
- Modify: `docs/m7-cloud-quickstart.md`
- Modify: `README.md`

- [ ] **Step 1: Append a one-line pointer to m5/m7 quickstarts**

In each, find the M8 multipart-cleanup section and add:

```markdown
> See also `docs/m9-maintenance-operator-guide.md` for the recommended
> `bucketvcs maintenance` scheduling alongside `bucketvcs gc`.
```

- [ ] **Step 2: Update README**

Add a row to the CLI surface table for `maintenance`. Use the same wording style as the existing `gc` row.

- [ ] **Step 3: Commit**

```bash
git add docs/m5-cloud-quickstart.md docs/m7-cloud-quickstart.md README.md
git commit -m "M9 task 14.2: link maintenance guide from quickstarts; add to README CLI table"
```

---

## Phase 15 — Final wiring + memory

### Task 15.1: Run the full test suite

- [ ] **Step 1: Run `go test ./...`**

Run: `go test ./...`
Expected: PASS across the whole repository. Address any flaky/cross-package failures inline (typical issues: leftover tmpdirs in test isolation, conformance suites that need git on PATH gated with `GitAvailable`).

- [ ] **Step 2: Run `go vet ./...`**

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 3: Run `golangci-lint run ./internal/maintenance/... ./cmd/bucketvcs/`** (if `golangci-lint` is configured for the repo)

Address any new lints introduced by the M9 code.

- [ ] **Step 4: If anything fails**

Fix inline and re-run. Do not commit a broken main.

### Task 15.2: Write `m9_progress.md` for memory

**Files:**
- Create: `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m9_progress.md`
- Modify: `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md`

- [ ] **Step 1: Write m9_progress.md**

Mirror the shape of `m8_progress.md`. Fields: name, description, type, originSessionId, narrative covering merge commit hash, tag, dated, notable details (cross-milestone touches, deferred items), follow-ups.

- [ ] **Step 2: Add the MEMORY.md pointer**

Insert a one-liner under the M8 entry:

```markdown
- [M9 background maintenance merged to main](m9_progress.md) — commit <sha>, tag m9-complete (2026-05-XX); bucketvcs maintenance + gitcli.PackObjectsAll repack pipeline + .bvom/.bvcg refresh + CAS-merge that preserves concurrent push packs
```

- [ ] **Step 3: Commit**

```bash
git add ".claude/projects/-home-eran-work-bucketvcs/memory/m9_progress.md" \
        ".claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md"
git commit -m "M9 progress: tag and record m9-complete in auto-memory"
```

(If memory files live outside the repo, run the equivalent `Write` operations against the absolute paths under `/home/eran/.claude/projects/...` and skip the git commit for those files.)

### Task 15.3: Tag and merge

- [ ] **Step 1: Push the worktree branch**

Run: `git push -u origin <branch-name>`

- [ ] **Step 2: Open the PR via roborev-review-branch**

Use the `roborev-review-branch` skill (per the M1+ review protocol) to get an automated branch review on max reasoning. Address findings via `roborev-refine` until pass / diminishing returns (per project review protocol).

- [ ] **Step 3: Squash-merge to main**

After review passes, squash-merge with a commit message summarizing the milestone deliverables.

- [ ] **Step 4: Tag**

Run: `git tag m9-complete && git push origin m9-complete`

---

## Out of scope (explicit reminders)

- **Pure-Go pack writer** — its own future milestone per §40.3 promotion rule.
- **Bitmap (`.bitmap`) generation** — M9.5.
- **Generated/cache pack retention** — paired with whoever first emits dynamic packs.
- **Geometric / tiered repack** — only when full-repack runtime becomes unaffordable.
- **In-serve background scheduler** — wraps `maintenance.Run` later.
- **Object-to-pack lookup-latency trigger** — needs latency substrate that doesn't exist yet.

If during implementation you discover a real cross-milestone need (e.g., a tx-record schema field), surface it as a Phase 0 patch task, get it reviewed independently, and ship it as a backward-compatible patch — same posture M8 took with `tx.WriteCommitMarker`.
