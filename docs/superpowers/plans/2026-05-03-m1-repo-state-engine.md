# M1 — Repository State Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the M1 thin transaction kernel — `internal/repo` library + `bucketvcs init` / `bucketvcs inspect-manifest` CLI — that creates/reads/updates a repo's durable state on top of M0's `ObjectStore`, demonstrably correct under concurrent CAS contention.

**Architecture:** Header/body split for both root manifest and tx records: M1 owns header fields (`schema_version`, `manifest_version`, `latest_tx`, `repo_id`, `repo_format`, `created_at`, `updated_at`) and enforces invariants; callers supply opaque body bytes. The only state-mutation entry point is `Repo.Commit(ctx, txBody, buildBody)`, an atomic-pair primitive that writes a fresh-ULID tx record then CAS-swaps the root, retrying with a re-invoked builder on conflict (per-attempt fresh tx_id; orphans left for M8 GC). Schema gate is asymmetric and fail-closed on `schema_version > 1` or `min_reader_version > supported`.

**Tech Stack:** Go 1.22, `github.com/bucketvcs/bucketvcs/internal/storage` (M0 ObjectStore), `github.com/oklog/ulid/v2` (tx_id minting), `golang.org/x/mod/semver` (schema gate). Stdlib `flag` for CLI (no cobra — keeps M1 lean; if M3 needs richer CLI we revisit).

**Spec:** `docs/superpowers/specs/2026-05-03-m1-repo-state-engine-design.md`

---

## Pre-flight

- Working from clean `main` at or after commit `718c0f4` (M0 merged, tag `m0-complete`).
- Recommended: work in a fresh git worktree (`git worktree add .claude/worktrees/m1-repo-state -b worktree-m1-repo-state main`) to mirror the M0 workflow.
- Run `go build ./... && go test -race ./...` before starting; should pass on a clean tree.

---

## Task 1: Add module dependencies and scaffold package directories

**Files:**
- Modify: `go.mod`, `go.sum`
- Create (empty placeholder): `internal/repo/keys/`, `internal/repo/manifest/`, `internal/repo/tx/`, `internal/repo/internal/`

- [ ] **Step 1: Add ulid and semver dependencies**

```bash
cd /home/eran/work/bucketvcs
go get github.com/oklog/ulid/v2
go get golang.org/x/mod/semver
```

- [ ] **Step 2: Verify go.mod**

Run: `cat go.mod`
Expected: `require` block includes `github.com/oklog/ulid/v2` and `golang.org/x/mod`.

- [ ] **Step 3: Create directory skeleton**

```bash
mkdir -p internal/repo/keys internal/repo/manifest internal/repo/tx internal/repo/internal
```

- [ ] **Step 4: Verify directories exist and are empty**

Run: `find internal/repo -type d -o -type f`
Expected: only the four directories listed; no files.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum
git commit -m "M1: add ulid + semver deps; scaffold internal/repo dirs"
```

---

## Task 2: Errors module

**Files:**
- Create: `internal/repo/errors.go`
- Test: `internal/repo/errors_test.go`

- [ ] **Step 1: Write the failing test**

`internal/repo/errors_test.go`:
```go
package repo_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestSentinelErrorsAreDistinct(t *testing.T) {
	all := []error{
		repo.ErrRepoExists,
		repo.ErrRepoNotFound,
		repo.ErrUnsupportedSchema,
		repo.ErrCallbackFailed,
		repo.ErrInvalidTenantID,
		repo.ErrInvalidRepoID,
	}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("sentinel %d == %d but should be distinct: %v", i, j, a)
			}
		}
	}
}

func TestCommitGaveUpErrorUnwrap(t *testing.T) {
	got := &repo.CommitGaveUpError{
		Attempts:    8,
		OrphanTxIDs: []string{"tx_a", "tx_b"},
		LastErr:     storage.ErrVersionMismatch,
	}
	if !errors.Is(got, storage.ErrVersionMismatch) {
		t.Fatalf("CommitGaveUpError must Unwrap to LastErr; got %v", got)
	}
	if msg := got.Error(); msg == "" {
		t.Fatalf("CommitGaveUpError.Error() must produce a message")
	}
}
```

- [ ] **Step 2: Run the test, confirm it fails**

Run: `go test ./internal/repo/...`
Expected: build failure — `repo.ErrRepoExists` etc. undefined.

- [ ] **Step 3: Write the implementation**

`internal/repo/errors.go`:
```go
// Package repo is the M1 transaction kernel: the only place in the
// codebase that atomically advances a repo from one durable state to the
// next. Sits between internal/storage (M0) and the future Git object
// engine (M2).
package repo

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrRepoExists: Create called against an existing repo
	// (manifest/root.json already present).
	ErrRepoExists = errors.New("repo: root manifest already exists")

	// ErrRepoNotFound: Open or ReadRoot called against a repo whose
	// manifest/root.json does not exist.
	ErrRepoNotFound = errors.New("repo: root manifest not found")

	// ErrUnsupportedSchema: the on-disk manifest's schema_version
	// exceeds the maximum this build supports, OR its min_reader_version
	// exceeds this build's reader version. Per spec §43.7 the gate is
	// asymmetric and fail-closed: refuse rather than ignore unknown
	// fields.
	ErrUnsupportedSchema = errors.New("repo: schema or min_reader_version exceeds supported")

	// ErrCallbackFailed: the buildBody callback supplied to Commit
	// returned an error. Wrap with errors.Unwrap to retrieve the
	// caller's original error.
	ErrCallbackFailed = errors.New("repo: buildBody callback returned error")

	// ErrInvalidTenantID: tenant_id failed Validate (charset, length,
	// or path-traversal check).
	ErrInvalidTenantID = errors.New("repo: tenant_id invalid")

	// ErrInvalidRepoID: repo_id failed Validate.
	ErrInvalidRepoID = errors.New("repo: repo_id invalid")
)

// CommitGaveUpError is returned by Repo.Commit when the retry budget is
// exhausted by repeated CAS conflicts. OrphanTxIDs lists the tx records
// written across all attempts; they remain on disk and become M8 GC
// candidates per §43.6.
type CommitGaveUpError struct {
	Attempts    int
	OrphanTxIDs []string
	LastErr     error
}

func (e *CommitGaveUpError) Error() string {
	return fmt.Sprintf(
		"repo: commit gave up after %d attempts (orphans: %s): %v",
		e.Attempts, strings.Join(e.OrphanTxIDs, ","), e.LastErr,
	)
}

func (e *CommitGaveUpError) Unwrap() error { return e.LastErr }
```

- [ ] **Step 4: Run the test, confirm it passes**

Run: `go test -race ./internal/repo/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/errors.go internal/repo/errors_test.go
git commit -m "M1 repo: sentinel errors + CommitGaveUpError"
```

---

## Task 3: Keys package — ID validation

**Files:**
- Create: `internal/repo/keys/keys.go`
- Test: `internal/repo/keys/keys_test.go`

- [ ] **Step 1: Write the failing test**

`internal/repo/keys/keys_test.go`:
```go
package keys_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
)

func TestNewRepo_ValidIDs(t *testing.T) {
	cases := []string{"a", "abc", "acme-prod_1", "A1Z9_-", strings("x", 128)}
	for _, id := range cases {
		if _, err := keys.NewRepo(id, id); err != nil {
			t.Errorf("expected NewRepo(%q,%q) ok, got %v", id, id, err)
		}
	}
}

func TestNewRepo_InvalidIDs(t *testing.T) {
	cases := []struct {
		tenant, repo string
		wantErr      error
	}{
		{"", "ok", repo.ErrInvalidTenantID},
		{"ok", "", repo.ErrInvalidRepoID},
		{"a/b", "ok", repo.ErrInvalidTenantID},
		{"ok", "a..b", repo.ErrInvalidRepoID},
		{"ok", "a b", repo.ErrInvalidRepoID},
		{strings("x", 129), "ok", repo.ErrInvalidTenantID},
		{"ok", strings("x", 129), repo.ErrInvalidRepoID},
		{"ok", ".", repo.ErrInvalidRepoID},
		{"ok", "..", repo.ErrInvalidRepoID},
	}
	for _, c := range cases {
		_, err := keys.NewRepo(c.tenant, c.repo)
		if !errors.Is(err, c.wantErr) {
			t.Errorf("NewRepo(%q,%q): want %v, got %v", c.tenant, c.repo, c.wantErr, err)
		}
	}
}

func TestRepoPrefix(t *testing.T) {
	r, err := keys.NewRepo("acme", "my-repo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Prefix(), "tenants/acme/repos/my-repo/"; got != want {
		t.Errorf("Prefix: want %q, got %q", want, got)
	}
}

func strings(c string, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = c[0]
	}
	return string(out)
}
```

- [ ] **Step 2: Run the test, confirm it fails**

Run: `go test ./internal/repo/keys/...`
Expected: build failure — `keys.NewRepo` undefined.

- [ ] **Step 3: Write the implementation**

`internal/repo/keys/keys.go`:
```go
// Package keys owns the §6 durable-key naming contract for bucketvcs
// repositories. Every path inside /tenants/{tid}/repos/{rid}/ is
// constructed here; M2/M3/M8 do not invent paths.
package keys

import (
	"regexp"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/repo"
)

// Repo holds the pre-computed key prefix for one (tenant, repo) pair.
// Construct via NewRepo, which validates IDs.
type Repo struct {
	tenantID, repoID string
	prefix           string
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// NewRepo validates IDs and returns a Repo bound to the corresponding
// key prefix.
func NewRepo(tenantID, repoID string) (*Repo, error) {
	if !validID(tenantID) {
		return nil, repo.ErrInvalidTenantID
	}
	if !validID(repoID) {
		return nil, repo.ErrInvalidRepoID
	}
	return &Repo{
		tenantID: tenantID,
		repoID:   repoID,
		prefix:   "tenants/" + tenantID + "/repos/" + repoID + "/",
	}, nil
}

// Prefix returns the durable key prefix for this repo, with trailing
// slash. All keys returned by this package's constructors begin with
// this prefix.
func (r *Repo) Prefix() string { return r.prefix }

// TenantID returns the tenant identifier this Repo was constructed with.
func (r *Repo) TenantID() string { return r.tenantID }

// RepoID returns the repo identifier this Repo was constructed with.
func (r *Repo) RepoID() string { return r.repoID }

func validID(s string) bool {
	if !idPattern.MatchString(s) {
		return false
	}
	// Reject path-traversal and dot-only segments even within the
	// allowed charset.
	if s == "." || s == ".." || strings.Contains(s, "..") {
		return false
	}
	return true
}
```

- [ ] **Step 4: Run the test, confirm it passes**

Run: `go test -race ./internal/repo/keys/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/keys/keys.go internal/repo/keys/keys_test.go
git commit -m "M1 keys: Repo + ID validation"
```

---

## Task 4: Keys — manifest and tx constructors (M1's only writes)

**Files:**
- Modify: `internal/repo/keys/keys.go`
- Modify: `internal/repo/keys/keys_test.go`

- [ ] **Step 1: Add the failing tests**

Append to `internal/repo/keys/keys_test.go`:
```go
func TestRootManifestKey(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	if got, want := r.RootManifestKey(), "tenants/acme/repos/my-repo/manifest/root.json"; got != want {
		t.Errorf("RootManifestKey: want %q, got %q", want, got)
	}
}

func TestTxRecordKey(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	id := "01HW7JSXEMABCDEF0123456789"
	want := "tenants/acme/repos/my-repo/tx/" + id + ".json"
	if got := r.TxRecordKey(id); got != want {
		t.Errorf("TxRecordKey: want %q, got %q", want, got)
	}
}

func TestTxPrefix(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	if got, want := r.TxPrefix(), "tenants/acme/repos/my-repo/tx/"; got != want {
		t.Errorf("TxPrefix: want %q, got %q", want, got)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/repo/keys/...`
Expected: build failure — `RootManifestKey`, `TxRecordKey`, `TxPrefix` undefined.

- [ ] **Step 3: Add the implementation**

Append to `internal/repo/keys/keys.go`:
```go
// RootManifestKey returns the durable key for the §7 root manifest.
func (r *Repo) RootManifestKey() string {
	return r.prefix + "manifest/root.json"
}

// TxRecordKey returns the durable key for one §8 immutable transaction
// record identified by txID (a ULID minted by Commit).
func (r *Repo) TxRecordKey(txID string) string {
	return r.prefix + "tx/" + txID + ".json"
}

// TxPrefix returns the prefix for listing all tx records in this repo.
// Used by M8 GC for orphan sweeps; not used by M1 itself.
func (r *Repo) TxPrefix() string {
	return r.prefix + "tx/"
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./internal/repo/keys/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/keys/keys.go internal/repo/keys/keys_test.go
git commit -m "M1 keys: RootManifestKey + TxRecordKey + TxPrefix"
```

---

## Task 5: Keys — pack/index/bundle/lfs/hooks/gc constructors (M2+ surface)

**Files:**
- Modify: `internal/repo/keys/keys.go`
- Modify: `internal/repo/keys/keys_test.go`

- [ ] **Step 1: Add the failing tests**

Append to `internal/repo/keys/keys_test.go`:
```go
func TestPackKeys(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	hash := "sha256-abc"
	cases := []struct {
		got, want string
	}{
		{r.CanonicalPackKey(hash), "tenants/acme/repos/my-repo/packs/canonical/" + hash + ".pack"},
		{r.GeneratedPackKey(hash), "tenants/acme/repos/my-repo/packs/generated/" + hash + ".pack"},
		{r.PackIdxKey(hash, "canonical"), "tenants/acme/repos/my-repo/packs/canonical/" + hash + ".idx"},
		{r.PackBitmapKey(hash, "generated"), "tenants/acme/repos/my-repo/packs/generated/" + hash + ".bitmap"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("want %q, got %q", c.want, c.got)
		}
	}
}

func TestIndexAndBundleKeys(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	cases := []struct{ got, want string }{
		{r.CommitGraphKey("g1"), "tenants/acme/repos/my-repo/indexes/commit-graphs/g1.graph"},
		{r.ReachabilityKey("i1"), "tenants/acme/repos/my-repo/indexes/reachability/i1.json"},
		{r.BundleKey("b1"), "tenants/acme/repos/my-repo/bundles/b1.bundle"},
		{r.BundleManifestKey("b1"), "tenants/acme/repos/my-repo/bundles/b1.json"},
		{r.LFSObjectKey("sha"), "tenants/acme/repos/my-repo/lfs/objects/sha"},
		{r.HookKey("h1", "pre-receive"), "tenants/acme/repos/my-repo/hooks/h1/pre-receive"},
		{r.GCMarkKey("m1"), "tenants/acme/repos/my-repo/gc/marks/m1.json"},
		{r.GCSweepKey("s1"), "tenants/acme/repos/my-repo/gc/sweeps/s1.json"},
		{r.RefShardKey("rs1"), "tenants/acme/repos/my-repo/manifest/ref-shards/rs1.json"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("want %q, got %q", c.want, c.got)
		}
	}
}

func TestPackIdxKey_RejectsBadArea(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic for bad area")
		}
	}()
	_ = r.PackIdxKey("h", "loose")
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/repo/keys/...`
Expected: build failure — methods undefined.

- [ ] **Step 3: Add the implementation**

Append to `internal/repo/keys/keys.go`:
```go
// CanonicalPackKey returns the path for a canonical (.pack) object in the
// canonical area. Used by M2.
func (r *Repo) CanonicalPackKey(packHash string) string {
	return r.prefix + "packs/canonical/" + packHash + ".pack"
}

// GeneratedPackKey returns the path for an on-the-fly generated pack.
// Used by M2.
func (r *Repo) GeneratedPackKey(packHash string) string {
	return r.prefix + "packs/generated/" + packHash + ".pack"
}

// PackIdxKey returns the .idx path for a pack in the named area
// ("canonical" or "generated"). Panics on unknown area to catch typos
// at test time. Used by M2.
func (r *Repo) PackIdxKey(packHash, area string) string {
	checkPackArea(area)
	return r.prefix + "packs/" + area + "/" + packHash + ".idx"
}

// PackBitmapKey returns the .bitmap path for a pack in the named area.
// Used by M2.
func (r *Repo) PackBitmapKey(packHash, area string) string {
	checkPackArea(area)
	return r.prefix + "packs/" + area + "/" + packHash + ".bitmap"
}

func checkPackArea(area string) {
	if area != "canonical" && area != "generated" {
		panic("keys: unknown pack area " + area + ` (want "canonical" or "generated")`)
	}
}

// CommitGraphKey returns the path for a commit-graph index. Used by M2.
func (r *Repo) CommitGraphKey(graphHash string) string {
	return r.prefix + "indexes/commit-graphs/" + graphHash + ".graph"
}

// ReachabilityKey returns the path for a reachability index. Used by M2.
func (r *Repo) ReachabilityKey(indexHash string) string {
	return r.prefix + "indexes/reachability/" + indexHash + ".json"
}

// BundleKey returns the path for a bundle file. Used by M11.
func (r *Repo) BundleKey(bundleID string) string {
	return r.prefix + "bundles/" + bundleID + ".bundle"
}

// BundleManifestKey returns the path for a bundle's sidecar manifest.
// Used by M11.
func (r *Repo) BundleManifestKey(bundleID string) string {
	return r.prefix + "bundles/" + bundleID + ".json"
}

// LFSObjectKey returns the path for an LFS object. Used by M13.
func (r *Repo) LFSObjectKey(sha256 string) string {
	return r.prefix + "lfs/objects/" + sha256
}

// HookKey returns the path for a server-side hook payload. Used by M14.
func (r *Repo) HookKey(hookID, name string) string {
	return r.prefix + "hooks/" + hookID + "/" + name
}

// GCMarkKey returns the path for a GC mark record. Used by M8.
func (r *Repo) GCMarkKey(markID string) string {
	return r.prefix + "gc/marks/" + markID + ".json"
}

// GCSweepKey returns the path for a GC sweep record. Used by M8.
func (r *Repo) GCSweepKey(sweepID string) string {
	return r.prefix + "gc/sweeps/" + sweepID + ".json"
}

// RefShardKey returns the path for a sharded-refs shard. Used by M12;
// never written by M1.
func (r *Repo) RefShardKey(shardHash string) string {
	return r.prefix + "manifest/ref-shards/" + shardHash + ".json"
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./internal/repo/keys/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/keys/keys.go internal/repo/keys/keys_test.go
git commit -m "M1 keys: pack/index/bundle/lfs/hook/gc/refshard constructors"
```

---

## Task 6: Manifest header struct + JSON round-trip

**Files:**
- Create: `internal/repo/manifest/header.go`
- Test: `internal/repo/manifest/header_test.go`

- [ ] **Step 1: Write the failing test**

`internal/repo/manifest/header_test.go`:
```go
package manifest_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestRootHeader_JSONRoundTrip(t *testing.T) {
	h := manifest.RootHeader{
		SchemaVersion:    1,
		MinReaderVersion: "0.1.0",
		RepoID:           "r_123",
		RepoFormat: manifest.Format{
			ObjectFormat:  "sha1",
			Compatibility: []string{"sha1"},
		},
		ManifestVersion: 42,
		LatestTx:        "tx_01HW7",
		CreatedAt:       time.Date(2026, 5, 3, 20, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 5, 3, 20, 1, 0, 0, time.UTC),
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var got manifest.RootHeader
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Errorf("round trip mismatch:\nwant %+v\ngot  %+v", h, got)
	}
}

func TestRootHeader_TopLevelKeys(t *testing.T) {
	h := manifest.RootHeader{
		SchemaVersion:    1,
		MinReaderVersion: "0.1.0",
		RepoID:           "r",
		RepoFormat:       manifest.Format{ObjectFormat: "sha1"},
		ManifestVersion:  1,
		LatestTx:         "tx_x",
	}
	b, _ := json.Marshal(h)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"schema_version", "min_reader_version", "repo_id",
		"repo_format", "manifest_version", "latest_tx",
		"created_at", "updated_at",
	} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, b)
		}
	}
}
```

Note: `RootHeader` must be value-comparable (no slices/maps in equality path) — `Compatibility` is a slice, so swap the equality check to `reflect.DeepEqual` if needed; the test above uses `if got != h` which will fail to compile because of the slice. Use the version below instead:

```go
import "reflect"
// ...
if !reflect.DeepEqual(got, h) {
    t.Errorf("round trip mismatch:\nwant %+v\ngot  %+v", h, got)
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/repo/manifest/...`
Expected: build failure — `manifest.RootHeader` undefined.

- [ ] **Step 3: Write the implementation**

`internal/repo/manifest/header.go`:
```go
// Package manifest defines the M1-owned root-manifest header struct and
// the §43.7 schema gate. Body fields (refs, packs, indexes, bundles,
// default_branch) are M2's concern and are passed through this package
// as opaque json.RawMessage.
package manifest

import "time"

// RootHeader is the M1-owned subset of the §7 root manifest. Every
// field in this struct is set or validated by M1 on every commit.
type RootHeader struct {
	SchemaVersion    int       `json:"schema_version"`
	MinReaderVersion string    `json:"min_reader_version"`
	RepoID           string    `json:"repo_id"`
	RepoFormat       Format    `json:"repo_format"`
	ManifestVersion  uint64    `json:"manifest_version"`
	LatestTx         string    `json:"latest_tx"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Format describes the on-disk Git object format for this repository.
// M1 ships only "sha1"; "sha256" is reserved for a future milestone.
type Format struct {
	ObjectFormat  string   `json:"object_format"`
	Compatibility []string `json:"compatibility,omitempty"`
}

// HeaderKeys lists the JSON field names M1 owns at the top level of the
// root manifest. Used by the body-merge helper to ensure callers do not
// emit duplicate or conflicting header fields in their body bytes.
var HeaderKeys = []string{
	"schema_version", "min_reader_version", "repo_id", "repo_format",
	"manifest_version", "latest_tx", "created_at", "updated_at",
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./internal/repo/manifest/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/manifest/header.go internal/repo/manifest/header_test.go
git commit -m "M1 manifest: RootHeader + Format + JSON round-trip"
```

---

## Task 7: Schema gate (§43.7)

**Files:**
- Create: `internal/repo/manifest/schema.go`
- Test: `internal/repo/manifest/schema_test.go`

- [ ] **Step 1: Write the failing test**

`internal/repo/manifest/schema_test.go`:
```go
package manifest_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestSchemaGate_Accepts(t *testing.T) {
	cases := []manifest.RootHeader{
		{SchemaVersion: 1, MinReaderVersion: "0.0.0"},
		{SchemaVersion: 1, MinReaderVersion: manifest.SupportedReaderVersion},
		{SchemaVersion: 1, MinReaderVersion: ""}, // empty == accept
	}
	for _, h := range cases {
		if err := manifest.SchemaGate(h); err != nil {
			t.Errorf("SchemaGate(%+v) want nil, got %v", h, err)
		}
	}
}

func TestSchemaGate_RejectsFutureSchemaVersion(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 2, MinReaderVersion: "0.1.0"}
	if err := manifest.SchemaGate(h); !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}

func TestSchemaGate_RejectsFutureMinReader(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 1, MinReaderVersion: "999.0.0"}
	if err := manifest.SchemaGate(h); !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}

func TestSchemaGate_RejectsZeroSchemaVersion(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 0, MinReaderVersion: "0.1.0"}
	if err := manifest.SchemaGate(h); !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/repo/manifest/...`
Expected: failure — `manifest.SchemaGate` and `manifest.SupportedReaderVersion` undefined.

- [ ] **Step 3: Write the implementation**

`internal/repo/manifest/schema.go`:
```go
package manifest

import (
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"golang.org/x/mod/semver"
)

const (
	// CurrentSchemaVersion is the schema_version M1 writers emit and
	// the highest schema_version M1 readers accept. Per §43.7 the gate
	// is asymmetric: future versions fail closed.
	CurrentSchemaVersion = 1

	// SupportedReaderVersion is the minimum reader version this build
	// satisfies. Manifests with min_reader_version > this value are
	// rejected at read time. The version string follows semver and
	// includes the "v" prefix expected by golang.org/x/mod/semver.
	SupportedReaderVersion = "0.1.0"
)

// SchemaGate enforces the §43.7 fail-closed compatibility check. Returns
// repo.ErrUnsupportedSchema (wrapped with detail) if the header would
// require a newer reader; nil if this build can read the manifest.
func SchemaGate(h RootHeader) error {
	if h.SchemaVersion < 1 || h.SchemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("%w: schema_version=%d (supported max=%d)",
			repo.ErrUnsupportedSchema, h.SchemaVersion, CurrentSchemaVersion)
	}
	if h.MinReaderVersion == "" {
		return nil
	}
	mr := vPrefix(h.MinReaderVersion)
	supported := vPrefix(SupportedReaderVersion)
	if !semver.IsValid(mr) {
		return fmt.Errorf("%w: min_reader_version=%q is not valid semver",
			repo.ErrUnsupportedSchema, h.MinReaderVersion)
	}
	if semver.Compare(mr, supported) > 0 {
		return fmt.Errorf("%w: min_reader_version=%q exceeds supported=%q",
			repo.ErrUnsupportedSchema, h.MinReaderVersion, SupportedReaderVersion)
	}
	return nil
}

func vPrefix(s string) string {
	if len(s) > 0 && s[0] == 'v' {
		return s
	}
	return "v" + s
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./internal/repo/manifest/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/manifest/schema.go internal/repo/manifest/schema_test.go
git commit -m "M1 manifest: §43.7 fail-closed schema gate"
```

---

## Task 8: Manifest CAS helpers (readRoot / casRoot / wrapHeader)

**Files:**
- Create: `internal/repo/manifest/cas.go`
- Test: `internal/repo/manifest/cas_test.go`

- [ ] **Step 1: Write the failing test**

`internal/repo/manifest/cas_test.go`:
```go
package manifest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadRoot_NotFound(t *testing.T) {
	s := newStore(t)
	_, _, _, err := manifest.ReadRoot(context.Background(), s, "tenants/a/repos/b/manifest/root.json")
	if !errors.Is(err, repo.ErrRepoNotFound) {
		t.Errorf("want ErrRepoNotFound, got %v", err)
	}
}

func TestReadRootAndCASRoot_RoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := "tenants/a/repos/b/manifest/root.json"

	header := manifest.RootHeader{
		SchemaVersion:    1,
		MinReaderVersion: "0.1.0",
		RepoID:           "b",
		RepoFormat:       manifest.Format{ObjectFormat: "sha1"},
		ManifestVersion:  1,
		LatestTx:         "tx_init",
		CreatedAt:        time.Now().UTC().Truncate(time.Second),
		UpdatedAt:        time.Now().UTC().Truncate(time.Second),
	}
	body := json.RawMessage(`{"refs":{},"packs":[],"default_branch":"refs/heads/main"}`)
	wrapped, err := manifest.WrapHeaderInBody(header, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfAbsent(ctx, key, strings.NewReader(string(wrapped)), nil); err != nil {
		t.Fatal(err)
	}

	gotHeader, gotBody, ver, err := manifest.ReadRoot(ctx, s, key)
	if err != nil {
		t.Fatal(err)
	}
	if gotHeader.RepoID != "b" || gotHeader.ManifestVersion != 1 {
		t.Errorf("header round-trip wrong: %+v", gotHeader)
	}
	if !json.Valid(gotBody) {
		t.Errorf("body not valid JSON: %s", gotBody)
	}

	header.ManifestVersion = 2
	header.LatestTx = "tx_2"
	header.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	wrapped2, _ := manifest.WrapHeaderInBody(header, body)
	if _, err := manifest.CASRoot(ctx, s, key, wrapped2, ver); err != nil {
		t.Fatal(err)
	}
}

func TestCASRoot_VersionMismatch(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := "tenants/a/repos/b/manifest/root.json"

	header := manifest.RootHeader{
		SchemaVersion: 1, RepoID: "b",
		RepoFormat: manifest.Format{ObjectFormat: "sha1"},
		ManifestVersion: 1,
	}
	wrapped, _ := manifest.WrapHeaderInBody(header, json.RawMessage(`{}`))
	if _, err := s.PutIfAbsent(ctx, key, strings.NewReader(string(wrapped)), nil); err != nil {
		t.Fatal(err)
	}

	stale := storage.ObjectVersion{Provider: "localfs", Token: "deadbeef"}
	_, err := manifest.CASRoot(ctx, s, key, wrapped, stale)
	if !errors.Is(err, storage.ErrVersionMismatch) {
		t.Errorf("want ErrVersionMismatch, got %v", err)
	}
}

func TestWrapHeaderInBody_RejectsHeaderKeysInBody(t *testing.T) {
	header := manifest.RootHeader{SchemaVersion: 1}
	body := json.RawMessage(`{"refs":{},"manifest_version":99}`)
	if _, err := manifest.WrapHeaderInBody(header, body); err == nil {
		t.Fatal("expected error when body contains a reserved header key")
	}
}
```

If `localfs.Open` returns a non-`storage.ObjectStore` concrete type, the test should still compile because `*Localfs` implements the interface. Verify with: `grep -n "func Open" internal/storage/localfs/localfs.go`. The conformance suite at `internal/storage/conformance` uses the same constructor — match its style. (No separate helper file needed; the `newStore` helper inside `cas_test.go` is sufficient for this package's tests.)

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/repo/manifest/...`
Expected: build failure — `manifest.ReadRoot`, `manifest.CASRoot`, `manifest.WrapHeaderInBody` undefined.

- [ ] **Step 3: Write the implementation**

`internal/repo/manifest/cas.go`:
```go
package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ReadRoot fetches the root manifest at key and parses out the
// M1-owned header. Returns the parsed header, the opaque body bytes
// (everything except header keys), and the storage version token for a
// later CAS. Returns repo.ErrRepoNotFound if the object does not exist.
func ReadRoot(ctx context.Context, s storage.ObjectStore, key string) (
	RootHeader, json.RawMessage, storage.ObjectVersion, error,
) {
	obj, err := s.Get(ctx, key, nil)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return RootHeader{}, nil, storage.ObjectVersion{}, repo.ErrRepoNotFound
		}
		return RootHeader{}, nil, storage.ObjectVersion{}, fmt.Errorf("repo: read root: %w", err)
	}
	defer obj.Body.Close()

	raw, err := io.ReadAll(obj.Body)
	if err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, fmt.Errorf("repo: read root body: %w", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, fmt.Errorf("repo: parse root manifest: %w", err)
	}

	headerJSON, err := pickHeader(top)
	if err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, err
	}
	var header RootHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, fmt.Errorf("repo: parse root header: %w", err)
	}

	for _, k := range HeaderKeys {
		delete(top, k)
	}
	body, err := json.Marshal(top)
	if err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, fmt.Errorf("repo: re-marshal body: %w", err)
	}
	return header, body, obj.Metadata.Version, nil
}

// CASRoot performs a PutIfVersionMatches against key with the given
// bytes. Storage-layer errors are wrapped so callers can errors.Is
// against storage sentinels.
func CASRoot(ctx context.Context, s storage.ObjectStore, key string, body []byte, prev storage.ObjectVersion) (storage.ObjectVersion, error) {
	v, err := s.PutIfVersionMatches(ctx, key, prev, strings.NewReader(string(body)), nil)
	if err != nil {
		return storage.ObjectVersion{}, fmt.Errorf("repo: cas root: %w", err)
	}
	return v, nil
}

// WrapHeaderInBody splices the M1-owned header keys into a body JSON
// object and returns the full root manifest bytes. Errors if the body
// already contains any of the reserved header keys (the caller would
// be claiming to own a field M1 owns).
func WrapHeaderInBody(h RootHeader, body json.RawMessage) ([]byte, error) {
	var top map[string]json.RawMessage
	if len(body) == 0 {
		top = map[string]json.RawMessage{}
	} else {
		if err := json.Unmarshal(body, &top); err != nil {
			return nil, fmt.Errorf("repo: body must be a JSON object: %w", err)
		}
	}
	for _, k := range HeaderKeys {
		if _, ok := top[k]; ok {
			return nil, fmt.Errorf("repo: body must not contain reserved header key %q", k)
		}
	}
	headerJSON, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("repo: marshal header: %w", err)
	}
	var headerMap map[string]json.RawMessage
	if err := json.Unmarshal(headerJSON, &headerMap); err != nil {
		return nil, fmt.Errorf("repo: re-parse header: %w", err)
	}
	for k, v := range headerMap {
		top[k] = v
	}
	return json.Marshal(top)
}

func pickHeader(top map[string]json.RawMessage) (json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for _, k := range HeaderKeys {
		v, ok := top[k]
		if !ok {
			continue
		}
		out[k] = v
	}
	return json.Marshal(out)
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./internal/repo/manifest/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/manifest/cas.go internal/repo/manifest/cas_test.go
git commit -m "M1 manifest: ReadRoot + CASRoot + WrapHeaderInBody"
```

---

## Task 9: Tx record types and write

**Files:**
- Create: `internal/repo/tx/record.go`
- Create: `internal/repo/tx/write.go`
- Test: `internal/repo/tx/record_test.go`

- [ ] **Step 1: Write the failing test**

`internal/repo/tx/record_test.go`:
```go
package tx_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMarshal_HeaderKeysAtTopLevel(t *testing.T) {
	header := tx.Header{
		SchemaVersion:             1,
		TxID:                      "tx_01HW7",
		RepoID:                    "r_1",
		BaseManifestVersion:       42,
		BaseManifestObjectVersion: "abcd",
		StartedAt:                 time.Date(2026, 5, 3, 20, 0, 0, 0, time.UTC),
	}
	body := tx.Body{
		Type:  "push",
		Actor: "u_1",
		RefUpdates: json.RawMessage(`[{"ref":"refs/heads/main"}]`),
		NewPacks:   json.RawMessage(`[{"pack_key":"x"}]`),
	}
	out, err := tx.Marshal(header, body)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"schema_version", "tx_id", "repo_id",
		"base_manifest_version", "base_manifest_object_version",
		"started_at", "type", "actor", "ref_updates", "new_packs",
	} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, out)
		}
	}
}

func TestMarshal_RejectsHeaderKeyInBody(t *testing.T) {
	header := tx.Header{SchemaVersion: 1, TxID: "x", RepoID: "r"}
	body := tx.Body{Type: "push", Actor: "u", Extra: json.RawMessage(`{"tx_id":"hijack"}`)}
	if _, err := tx.Marshal(header, body); err == nil {
		t.Fatal("expected error when body Extra contains reserved header key")
	}
}

func TestWrite_PutIfAbsentSemantics(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := "tenants/a/repos/b/tx/tx_01HW7.json"

	header := tx.Header{
		SchemaVersion: 1, TxID: "tx_01HW7", RepoID: "b", StartedAt: time.Now().UTC(),
	}
	body := tx.Body{Type: "create", Actor: "u_1"}

	if err := tx.Write(ctx, s, key, header, body); err != nil {
		t.Fatal(err)
	}
	if err := tx.Write(ctx, s, key, header, body); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("want ErrAlreadyExists on second write, got %v", err)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/repo/tx/...`
Expected: build failure — `tx.Header`, `tx.Body`, `tx.Marshal`, `tx.Write` undefined.

- [ ] **Step 3: Write the implementations**

`internal/repo/tx/record.go`:
```go
// Package tx defines the M1-owned tx-record header struct, the
// caller-supplied body struct, and helpers to marshal a complete
// immutable transaction record per spec §8.
package tx

import (
	"encoding/json"
	"fmt"
	"time"
)

// Header is the M1-owned subset of a tx record. M1 mints these fields
// at commit time; callers cannot supply them.
type Header struct {
	SchemaVersion             int       `json:"schema_version"`
	TxID                      string    `json:"tx_id"`
	RepoID                    string    `json:"repo_id"`
	BaseManifestVersion       uint64    `json:"base_manifest_version"`
	BaseManifestObjectVersion string    `json:"base_manifest_object_version"`
	StartedAt                 time.Time `json:"started_at"`
}

// Body is the caller-supplied subset of a tx record. M1 splices these
// fields into the top level of the JSON document at write time.
type Body struct {
	// Type is the high-level operation classification: "create",
	// "push", "gc", or future values defined by M2/M3/M8.
	Type string `json:"type"`

	// Actor identifies the principal performing the operation. M4 will
	// supply real identity strings; M1 / unit tests may use placeholder
	// values like "u_test".
	Actor string `json:"actor"`

	// RefUpdates, NewPacks, Validation are opaque to M1; their schemas
	// are defined by the milestones that produce them.
	RefUpdates json.RawMessage `json:"ref_updates,omitempty"`
	NewPacks   json.RawMessage `json:"new_packs,omitempty"`
	Validation json.RawMessage `json:"validation,omitempty"`

	// Extra carries forward-compatible additional fields. Must be a
	// JSON object whose keys do not collide with any reserved header or
	// known body key. Marshal returns an error on collision.
	Extra json.RawMessage `json:"-"`
}

// HeaderKeys lists the JSON field names M1 owns at the top level of a
// tx record. Used by Marshal to reject body content that would shadow
// these fields.
var HeaderKeys = []string{
	"schema_version", "tx_id", "repo_id",
	"base_manifest_version", "base_manifest_object_version", "started_at",
}

// BodyKnownKeys lists the JSON field names the Body struct emits. Used
// by Marshal to reject Extra content that would shadow these fields.
var BodyKnownKeys = []string{"type", "actor", "ref_updates", "new_packs", "validation"}

// Marshal returns the canonical JSON bytes of a complete tx record:
// header keys + body fields + Extra, all flattened to a single
// top-level object.
func Marshal(h Header, b Body) ([]byte, error) {
	top := map[string]json.RawMessage{}

	hb, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("tx: marshal header: %w", err)
	}
	if err := json.Unmarshal(hb, &top); err != nil {
		return nil, fmt.Errorf("tx: re-parse header: %w", err)
	}

	bb, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("tx: marshal body: %w", err)
	}
	var bodyMap map[string]json.RawMessage
	if err := json.Unmarshal(bb, &bodyMap); err != nil {
		return nil, fmt.Errorf("tx: re-parse body: %w", err)
	}
	for k, v := range bodyMap {
		if _, dup := top[k]; dup {
			return nil, fmt.Errorf("tx: body key %q collides with header", k)
		}
		top[k] = v
	}

	if len(b.Extra) > 0 {
		var extraMap map[string]json.RawMessage
		if err := json.Unmarshal(b.Extra, &extraMap); err != nil {
			return nil, fmt.Errorf("tx: Extra must be a JSON object: %w", err)
		}
		for k, v := range extraMap {
			if _, dup := top[k]; dup {
				return nil, fmt.Errorf("tx: extra key %q collides with header or known body key", k)
			}
			top[k] = v
		}
	}
	return json.Marshal(top)
}
```

`internal/repo/tx/write.go`:
```go
package tx

import (
	"context"
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Write marshals the record and stores it at key with PutIfAbsent
// (create-only). Returns storage.ErrAlreadyExists if the key already
// has an object — under normal operation this is impossible because
// txID is a fresh ULID, but the test suite can provoke it.
func Write(ctx context.Context, s storage.ObjectStore, key string, h Header, b Body) error {
	bytes, err := Marshal(h, b)
	if err != nil {
		return err
	}
	if _, err := s.PutIfAbsent(ctx, key, strings.NewReader(string(bytes)), nil); err != nil {
		return fmt.Errorf("tx: write %s: %w", key, err)
	}
	return nil
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./internal/repo/tx/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/tx/
git commit -m "M1 tx: Header + Body + Marshal + Write (PutIfAbsent)"
```

---

## Task 10: Repo handle skeleton + Open

**Files:**
- Create: `internal/repo/repo.go`
- Test: `internal/repo/repo_test.go`

- [ ] **Step 1: Write the failing test**

`internal/repo/repo_test.go`:
```go
package repo_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newLocalFS(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpen_NotFound(t *testing.T) {
	s := newLocalFS(t)
	_, err := repo.Open(context.Background(), s, "acme", "missing")
	if !errors.Is(err, repo.ErrRepoNotFound) {
		t.Errorf("want ErrRepoNotFound, got %v", err)
	}
}

func TestOpen_BadIDs(t *testing.T) {
	s := newLocalFS(t)
	_, err := repo.Open(context.Background(), s, "", "x")
	if !errors.Is(err, repo.ErrInvalidTenantID) {
		t.Errorf("want ErrInvalidTenantID, got %v", err)
	}
}

func TestOpen_FutureSchemaRejected(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	header := manifest.RootHeader{
		SchemaVersion:    999,
		MinReaderVersion: "0.1.0",
		RepoID:           "b",
		RepoFormat:       manifest.Format{ObjectFormat: "sha1"},
		ManifestVersion:  1,
		LatestTx:         "tx_x",
		CreatedAt:        time.Now().UTC().Truncate(time.Second),
		UpdatedAt:        time.Now().UTC().Truncate(time.Second),
	}
	wrapped, err := manifest.WrapHeaderInBody(header, json.RawMessage(`{"refs":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfAbsent(ctx, "tenants/acme/repos/b/manifest/root.json",
		strings.NewReader(string(wrapped)), nil); err != nil {
		t.Fatal(err)
	}
	_, err = repo.Open(ctx, s, "acme", "b")
	if !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/repo/...`
Expected: failure — `repo.Open` undefined.

- [ ] **Step 3: Write the implementation**

`internal/repo/repo.go`:
```go
package repo

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Repo is a handle to one (tenant, repo) pair backed by an ObjectStore.
// Construct via Open or Create. Repo is safe to share between
// goroutines: it holds no per-call mutable state.
type Repo struct {
	store storage.ObjectStore
	keys  *keys.Repo
}

// Open returns a handle for an existing repo. Returns ErrRepoNotFound
// if no root manifest exists at the expected key, ErrUnsupportedSchema
// if the root's header fails the §43.7 gate, and a wrapped error for
// any other read failure.
func Open(ctx context.Context, store storage.ObjectStore, tenantID, repoID string) (*Repo, error) {
	k, err := keys.NewRepo(tenantID, repoID)
	if err != nil {
		return nil, err
	}
	header, _, _, err := manifest.ReadRoot(ctx, store, k.RootManifestKey())
	if err != nil {
		return nil, err
	}
	if err := manifest.SchemaGate(header); err != nil {
		return nil, err
	}
	return &Repo{store: store, keys: k}, nil
}

// TenantID returns the tenant identifier this Repo was opened with.
func (r *Repo) TenantID() string { return r.keys.TenantID() }

// RepoID returns the repo identifier this Repo was opened with.
func (r *Repo) RepoID() string { return r.keys.RepoID() }
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./internal/repo/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/repo.go internal/repo/repo_test.go
git commit -m "M1 repo: Open with schema gate"
```

---

## Task 11: Repo Create with §4.3 carve-out

**Files:**
- Modify: `internal/repo/repo.go`
- Modify: `internal/repo/repo_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/repo/repo_test.go`:
```go
func TestCreate_HappyPath(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, err := repo.Create(ctx, s, "acme", "my-repo", repo.CreateOptions{
		DefaultBranch: "refs/heads/main",
		ObjectFormat:  "sha1",
		Actor:         "u_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.TenantID() != "acme" || r.RepoID() != "my-repo" {
		t.Errorf("unexpected handle: %s/%s", r.TenantID(), r.RepoID())
	}
	// Verify root.json exists with manifest_version=1.
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Header.ManifestVersion != 1 {
		t.Errorf("want manifest_version=1, got %d", view.Header.ManifestVersion)
	}
	if view.Header.RepoID != "my-repo" {
		t.Errorf("want repo_id=my-repo, got %q", view.Header.RepoID)
	}
	if view.Header.SchemaVersion != 1 {
		t.Errorf("want schema_version=1, got %d", view.Header.SchemaVersion)
	}
	if view.Header.LatestTx == "" {
		t.Errorf("LatestTx should reference the create tx")
	}
}

func TestCreate_AlreadyExists(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	if _, err := repo.Create(ctx, s, "acme", "my-repo", repo.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := repo.Create(ctx, s, "acme", "my-repo", repo.CreateOptions{})
	if !errors.Is(err, repo.ErrRepoExists) {
		t.Errorf("want ErrRepoExists, got %v", err)
	}
	// Verify no orphan tx record from the failed create. List the tx prefix.
	page, err := s.List(ctx, "tenants/acme/repos/my-repo/tx/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 1 {
		t.Errorf("want 1 tx record (only the original create), got %d", len(page.Objects))
	}
}

func TestReadRoot_AfterCreate(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, _ := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{Actor: "u"})
	v, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v.Version.Token == "" {
		t.Errorf("expected non-empty version token")
	}
	if v.SizeBytes == 0 {
		t.Errorf("expected non-zero size")
	}
	if !json.Valid(v.Body) {
		t.Errorf("body must be valid JSON: %s", v.Body)
	}
}
```

Note: the List signature above assumes `s.List(ctx, prefix, opts)` returns a page with `.Items`. Verify with: `grep -n "func.*List" internal/storage/objectstore.go internal/storage/localfs/localfs.go`. Adapt the call (and the import of any options type) to whatever the M0 signature actually is.

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/repo/...`
Expected: failure — `repo.Create`, `repo.CreateOptions`, `(*Repo).ReadRoot`, `RootView` undefined.

- [ ] **Step 3: Write the implementation**

Append to `internal/repo/repo.go`:
```go
import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/oklog/ulid/v2"
	mathrand "math/rand"
)

// CreateOptions controls Create-time choices.
type CreateOptions struct {
	// DefaultBranch is the body-level default_branch field. Defaults
	// to "refs/heads/main" when empty.
	DefaultBranch string

	// ObjectFormat is the Git object format. M1 supports "sha1" only;
	// "sha256" is reserved. Defaults to "sha1".
	ObjectFormat string

	// Actor is recorded in the create tx record. Defaults to "unknown".
	Actor string
}

// Create writes the initial tx record + root manifest for a new repo.
// Returns ErrRepoExists if the root manifest already exists. Per spec
// §4.3 of the M1 design, Create is the only operation that violates the
// "tx record before CAS" ordering: it PutIfAbsent's the root first,
// then writes the create-tx record. The reason: there is no prior
// root to CAS against, and writing a tx record for a duplicate Create
// would generate a useless orphan on every accidental re-init.
func Create(ctx context.Context, store storage.ObjectStore, tenantID, repoID string, opts CreateOptions) (*Repo, error) {
	k, err := keys.NewRepo(tenantID, repoID)
	if err != nil {
		return nil, err
	}
	if opts.DefaultBranch == "" {
		opts.DefaultBranch = "refs/heads/main"
	}
	if opts.ObjectFormat == "" {
		opts.ObjectFormat = "sha1"
	}
	if opts.ObjectFormat != "sha1" {
		return nil, fmt.Errorf("repo: object_format %q not supported in M1 (only sha1)", opts.ObjectFormat)
	}
	if opts.Actor == "" {
		opts.Actor = "unknown"
	}

	now := time.Now().UTC().Truncate(time.Second)
	txID := newTxID()

	header := manifest.RootHeader{
		SchemaVersion:    manifest.CurrentSchemaVersion,
		MinReaderVersion: manifest.SupportedReaderVersion,
		RepoID:           repoID,
		RepoFormat: manifest.Format{
			ObjectFormat:  opts.ObjectFormat,
			Compatibility: []string{opts.ObjectFormat},
		},
		ManifestVersion: 1,
		LatestTx:        txID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	body := json.RawMessage(fmt.Sprintf(
		`{"refs":{},"packs":[],"indexes":{},"bundles":[],"default_branch":%q}`,
		opts.DefaultBranch,
	))
	rootBytes, err := manifest.WrapHeaderInBody(header, body)
	if err != nil {
		return nil, err
	}

	if _, err := store.PutIfAbsent(ctx, k.RootManifestKey(),
		strings.NewReader(string(rootBytes)), nil); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			return nil, ErrRepoExists
		}
		return nil, fmt.Errorf("repo: create root: %w", err)
	}

	txHeader := tx.Header{
		SchemaVersion: 1, TxID: txID, RepoID: repoID,
		BaseManifestVersion: 0, BaseManifestObjectVersion: "",
		StartedAt: now,
	}
	txBody := tx.Body{Type: "create", Actor: opts.Actor}
	if err := tx.Write(ctx, store, k.TxRecordKey(txID), txHeader, txBody); err != nil {
		// The root is already on disk pointing at this tx_id. Surfacing
		// the error lets the caller know a follow-up doctor command will
		// be needed; M16 will provide it. M8 GC will not sweep this
		// referenced-but-missing tx because root.latest_tx pins it.
		return nil, fmt.Errorf("repo: create tx record (root already committed): %w", err)
	}
	return &Repo{store: store, keys: k}, nil
}

// RootView is a snapshot of the root manifest as returned by ReadRoot.
type RootView struct {
	Header    manifest.RootHeader
	Body      json.RawMessage
	Version   storage.ObjectVersion
	SizeBytes int64
}

// ReadRoot returns the current root manifest header + body bytes +
// version token. ErrRepoNotFound if the root is missing.
func (r *Repo) ReadRoot(ctx context.Context) (*RootView, error) {
	header, body, ver, err := manifest.ReadRoot(ctx, r.store, r.keys.RootManifestKey())
	if err != nil {
		return nil, err
	}
	if err := manifest.SchemaGate(header); err != nil {
		return nil, err
	}
	return &RootView{
		Header:    header,
		Body:      body,
		Version:   ver,
		SizeBytes: int64(len(body)),
	}, nil
}

// txEntropy is a goroutine-safe entropy source for ulid.MustNew. ulid's
// monotonic entropy reader keeps IDs lexicographically sortable within
// a millisecond and avoids ID collisions across goroutines.
var txEntropy = ulid.Monotonic(mathrand.New(mathrand.NewSource(time.Now().UnixNano())), 0)

func newTxID() string {
	return "tx_" + ulid.MustNew(ulid.Timestamp(time.Now()), txEntropy).String()
}
```

Note: combine the two `import` blocks in `repo.go` into a single block. Re-check imports compile.

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./internal/repo/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/repo.go internal/repo/repo_test.go
git commit -m "M1 repo: Create + ReadRoot + tx_id minting"
```

---

## Task 12: Repo Commit (retry loop, per-attempt fresh tx_id)

**Files:**
- Modify: `internal/repo/repo.go`
- Modify: `internal/repo/repo_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/repo/repo_test.go`:
```go
func TestCommit_HappyPath(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, _ := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{Actor: "u"})

	txID, err := r.Commit(ctx,
		tx.Body{Type: "push", Actor: "u_pusher"},
		func(prev *repo.RootView) ([]byte, error) {
			// Mutate body: add one ref.
			var top map[string]json.RawMessage
			if err := json.Unmarshal(prev.Body, &top); err != nil {
				return nil, err
			}
			top["refs"] = json.RawMessage(`{"refs/heads/main":{"target":"abc"}}`)
			return json.Marshal(top)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if txID == "" || !strings.HasPrefix(txID, "tx_") {
		t.Errorf("bad tx id: %q", txID)
	}

	v, _ := r.ReadRoot(ctx)
	if v.Header.ManifestVersion != 2 {
		t.Errorf("want manifest_version=2 after one Commit, got %d", v.Header.ManifestVersion)
	}
	if v.Header.LatestTx != txID {
		t.Errorf("LatestTx mismatch: want %s, got %s", txID, v.Header.LatestTx)
	}
	if !strings.Contains(string(v.Body), "refs/heads/main") {
		t.Errorf("body did not record the ref: %s", v.Body)
	}
}

func TestCommit_CallbackError(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, _ := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{})
	sentinel := errors.New("callback ran but returned this")

	_, err := r.Commit(ctx, tx.Body{Type: "push", Actor: "u"}, func(*repo.RootView) ([]byte, error) {
		return nil, sentinel
	})
	if !errors.Is(err, repo.ErrCallbackFailed) {
		t.Errorf("want ErrCallbackFailed, got %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err should also unwrap to caller's sentinel, got %v", err)
	}
	// No new tx record should have been written.
	page, _ := s.List(ctx, "tenants/acme/repos/x/tx/", nil)
	if len(page.Objects) != 1 {
		t.Errorf("want 1 tx record (the create), got %d", len(page.Objects))
	}
	// manifest_version unchanged.
	v, _ := r.ReadRoot(ctx)
	if v.Header.ManifestVersion != 1 {
		t.Errorf("want manifest_version=1, got %d", v.Header.ManifestVersion)
	}
}

func TestCommit_PerAttemptFreshTxID(t *testing.T) {
	// Force a CAS conflict on the first attempt by interposing a
	// helper that bumps the root between callback and CAS.
	s := newLocalFS(t)
	ctx := context.Background()
	r, _ := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{Actor: "u"})

	calls := 0
	txID, err := r.Commit(ctx, tx.Body{Type: "push", Actor: "u"}, func(prev *repo.RootView) ([]byte, error) {
		calls++
		if calls == 1 {
			// Race: do a side-channel commit to invalidate prev.Version
			_, err := r.Commit(ctx, tx.Body{Type: "push", Actor: "u_other"}, func(p2 *repo.RootView) ([]byte, error) {
				return p2.Body, nil
			})
			if err != nil {
				return nil, err
			}
		}
		return prev.Body, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Errorf("expected callback re-invoked on conflict; called %d times", calls)
	}
	// Two committed tx records (the side-channel and the eventual winner)
	// + one orphan from the first CAS attempt + one from create = 4.
	page, _ := s.List(ctx, "tenants/acme/repos/x/tx/", nil)
	if len(page.Objects) != 4 {
		t.Errorf("want 4 tx records (1 create + 1 orphan + 2 committed); got %d", len(page.Objects))
	}
	v, _ := r.ReadRoot(ctx)
	if v.Header.LatestTx != txID {
		t.Errorf("LatestTx mismatch")
	}
	if v.Header.ManifestVersion != 3 {
		t.Errorf("want manifest_version=3, got %d", v.Header.ManifestVersion)
	}
}

func TestCommit_RetryBudgetExhausted(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, _ := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{Actor: "u"})

	// Always race a side-channel commit so the main commit can never win.
	_, err := r.Commit(ctx, tx.Body{Type: "push", Actor: "u"},
		func(prev *repo.RootView) ([]byte, error) {
			_, _ = r.Commit(ctx, tx.Body{Type: "push", Actor: "u_other"},
				func(p2 *repo.RootView) ([]byte, error) { return p2.Body, nil })
			return prev.Body, nil
		},
		repo.WithCommitPolicy(repo.CommitPolicy{MaxRetries: 3, BackoffBase: 0}),
	)
	var gaveUp *repo.CommitGaveUpError
	if !errors.As(err, &gaveUp) {
		t.Fatalf("want *CommitGaveUpError, got %v", err)
	}
	if gaveUp.Attempts != 3 {
		t.Errorf("want Attempts=3, got %d", gaveUp.Attempts)
	}
	if len(gaveUp.OrphanTxIDs) != 3 {
		t.Errorf("want 3 orphan tx ids, got %d", len(gaveUp.OrphanTxIDs))
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/repo/...`
Expected: failure — `(*Repo).Commit`, `repo.WithCommitPolicy`, `repo.CommitPolicy` undefined.

- [ ] **Step 3: Write the implementation**

Append to `internal/repo/repo.go`:
```go
// CommitPolicy controls retry behavior for a single Commit invocation.
type CommitPolicy struct {
	// MaxRetries is the maximum number of CAS attempts before
	// returning *CommitGaveUpError. Default 8.
	MaxRetries int

	// BackoffBase is the base delay between retries. Actual delay is
	// jittered uniformly in [0, BackoffBase * 2^attempt). Default 5ms.
	// Set to 0 to disable backoff (useful in tests).
	BackoffBase time.Duration
}

// CommitOption configures one Commit invocation.
type CommitOption func(*CommitPolicy)

// WithCommitPolicy overrides the default CommitPolicy for one Commit.
func WithCommitPolicy(p CommitPolicy) CommitOption {
	return func(out *CommitPolicy) { *out = p }
}

const (
	defaultMaxRetries  = 8
	defaultBackoffBase = 5 * time.Millisecond
)

// Commit performs the §8 atomic-pair (write tx record, then CAS root)
// with bounded retry on CAS conflict. Each attempt mints a fresh tx_id
// so every tx record on disk has accurate base_manifest_* fields. The
// returned tx_id is the *winning* one (referenced by the new root).
// Lost attempts leave orphan tx records on disk for M8 GC.
func (r *Repo) Commit(
	ctx context.Context,
	txBody tx.Body,
	buildBody func(prev *RootView) (newBody []byte, err error),
	opts ...CommitOption,
) (string, error) {
	policy := CommitPolicy{MaxRetries: defaultMaxRetries, BackoffBase: defaultBackoffBase}
	for _, o := range opts {
		o(&policy)
	}

	var (
		orphans []string
		lastErr error
	)
	for attempt := 1; attempt <= policy.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		view, err := r.ReadRoot(ctx)
		if err != nil {
			return "", err
		}
		newBody, err := buildBody(view)
		if err != nil {
			return "", fmt.Errorf("%w: %s", ErrCallbackFailed, err.Error())
		}

		txID := newTxID()
		txHeader := tx.Header{
			SchemaVersion:             1,
			TxID:                      txID,
			RepoID:                    r.RepoID(),
			BaseManifestVersion:       view.Header.ManifestVersion,
			BaseManifestObjectVersion: view.Version.Token,
			StartedAt:                 time.Now().UTC().Truncate(time.Second),
		}
		if err := tx.Write(ctx, r.store, r.keys.TxRecordKey(txID), txHeader, txBody); err != nil {
			return "", err
		}

		nextHeader := view.Header
		nextHeader.ManifestVersion++
		nextHeader.LatestTx = txID
		nextHeader.UpdatedAt = time.Now().UTC().Truncate(time.Second)

		nextBytes, err := manifest.WrapHeaderInBody(nextHeader, newBody)
		if err != nil {
			orphans = append(orphans, txID)
			return "", err
		}

		if _, err := manifest.CASRoot(ctx, r.store, r.keys.RootManifestKey(), nextBytes, view.Version); err != nil {
			lastErr = err
			orphans = append(orphans, txID)
			if errors.Is(err, storage.ErrVersionMismatch) {
				if policy.BackoffBase > 0 {
					sleepBackoff(ctx, policy.BackoffBase, attempt)
				}
				continue
			}
			return "", err
		}
		return txID, nil
	}
	return "", &CommitGaveUpError{
		Attempts: policy.MaxRetries, OrphanTxIDs: orphans, LastErr: lastErr,
	}
}

func sleepBackoff(ctx context.Context, base time.Duration, attempt int) {
	mult := int64(1) << attempt
	if mult > 1<<10 {
		mult = 1 << 10
	}
	jitter := time.Duration(mathrand.Int63n(int64(base) * mult))
	t := time.NewTimer(jitter)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./internal/repo/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/repo.go internal/repo/repo_test.go
git commit -m "M1 repo: Commit retry loop with per-attempt fresh tx_id"
```

---

## Task 13: CLI shared --store flag parser

**Files:**
- Create: `cmd/bucketvcs/store.go`
- Test: `cmd/bucketvcs/store_test.go`

- [ ] **Step 1: Write the failing test**

`cmd/bucketvcs/store_test.go`:
```go
package main

import (
	"strings"
	"testing"
)

func TestParseStoreURL_LocalFS(t *testing.T) {
	cases := []struct {
		url, wantPath string
	}{
		{"localfs:/tmp/x", "/tmp/x"},
		{"localfs:./relative", "./relative"},
		{"localfs:" + strings.Repeat("a", 200), strings.Repeat("a", 200)},
	}
	for _, c := range cases {
		scheme, path, err := parseStoreURL(c.url)
		if err != nil {
			t.Errorf("%q: %v", c.url, err)
			continue
		}
		if scheme != "localfs" || path != c.wantPath {
			t.Errorf("%q: got (%q,%q), want (localfs, %q)", c.url, scheme, path, c.wantPath)
		}
	}
}

func TestParseStoreURL_Errors(t *testing.T) {
	cases := []string{"", "localfs", "s3://bucket/key", "http://x", "localfs:"}
	for _, in := range cases {
		if _, _, err := parseStoreURL(in); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./cmd/bucketvcs/...`
Expected: failure — `parseStoreURL` undefined.

- [ ] **Step 3: Write the implementation**

`cmd/bucketvcs/store.go`:
```go
package main

import (
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// parseStoreURL parses a --store value into (scheme, scheme-specific
// path). M1 supports only "localfs:<path>"; cloud schemes ("s3:", "gcs:",
// "r2:", "azureblob:") are recognized but rejected with an explanatory
// error pointing at the milestone that will add them.
func parseStoreURL(s string) (scheme, path string, err error) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 {
		return "", "", fmt.Errorf(`--store: missing scheme; want "localfs:<path>"`)
	}
	scheme = s[:colon]
	path = s[colon+1:]
	if path == "" {
		return "", "", fmt.Errorf(`--store: empty path after %q:`, scheme)
	}
	switch scheme {
	case "localfs":
		return scheme, path, nil
	case "s3", "gcs", "r2", "azureblob":
		return "", "", fmt.Errorf(`--store: scheme %q is reserved; cloud adapters land at M5/M7`, scheme)
	default:
		return "", "", fmt.Errorf(`--store: unknown scheme %q; want "localfs:<path>"`, scheme)
	}
}

// openStore parses the --store URL and returns a constructed
// ObjectStore. Caller is responsible for any cleanup (localfs has none).
func openStore(url string) (storage.ObjectStore, error) {
	scheme, path, err := parseStoreURL(url)
	if err != nil {
		return nil, err
	}
	switch scheme {
	case "localfs":
		s, err := localfs.Open(path)
		if err != nil {
			return nil, fmt.Errorf("localfs: %w", err)
		}
		return s, nil
	default:
		return nil, fmt.Errorf("unreachable: scheme %q passed parseStoreURL but openStore has no constructor", scheme)
	}
}
```

Note: `localfs.Open(path)` is the assumed constructor. Verify with: `grep -n "^func New" internal/storage/localfs/localfs.go`. If the actual signature differs (e.g. takes options), adapt.

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./cmd/bucketvcs/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/store.go cmd/bucketvcs/store_test.go
git commit -m "M1 cli: --store parser (localfs only in M1)"
```

---

## Task 14: CLI bucketvcs init

**Files:**
- Create: `cmd/bucketvcs/init.go`
- Test: `cmd/bucketvcs/init_test.go`

- [ ] **Step 1: Write the failing test**

`cmd/bucketvcs/init_test.go`:
```go
package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
)

func TestRunInit_HappyPath(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runInit(context.Background(), []string{
		"--store=localfs:" + dir,
		"--actor=u_test",
		"acme", "my-repo",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "created acme/my-repo") {
		t.Errorf("missing success line in stdout: %q", stdout.String())
	}
	rootPath := filepath.Join(dir, "tenants/acme/repos/my-repo/manifest/root.json")
	if _, err := os.Stat(rootPath); err != nil {
		t.Errorf("expected root manifest at %s, got %v", rootPath, err)
	}
}

func TestRunInit_AlreadyExists(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--store=localfs:" + dir, "acme", "my-repo"}

	var sink bytes.Buffer
	if code := runInit(context.Background(), args, &sink, &sink); code != 0 {
		t.Fatalf("first init failed: exit %d", code)
	}
	var stdout, stderr bytes.Buffer
	code := runInit(context.Background(), args, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit on duplicate init")
	}
	if !errors.Is(asError(stderr.String(), repo.ErrRepoExists), repo.ErrRepoExists) {
		// Soft check: just look for the substring since stderr is text.
		if !strings.Contains(stderr.String(), "already exists") {
			t.Errorf("stderr should mention 'already exists', got %q", stderr.String())
		}
	}
}

// asError is a placeholder for the linter; never matches.
func asError(_ string, _ error) error { return nil }

func TestRunInit_BadFlags(t *testing.T) {
	cases := [][]string{
		{}, // missing positional args
		{"--store=localfs:/tmp/x"},                  // missing positional
		{"--store=", "a", "b"},                      // empty store
		{"--store=localfs:/tmp/x", "a"},             // missing repo
		{"--store=localfs:/tmp/x", "a", "b", "c"},   // too many
	}
	for i, c := range cases {
		var stdout, stderr bytes.Buffer
		if code := runInit(context.Background(), c, &stdout, &stderr); code == 0 {
			t.Errorf("case %d: expected non-zero exit, got 0; stderr=%s", i, stderr.String())
		}
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./cmd/bucketvcs/...`
Expected: failure — `runInit` undefined.

- [ ] **Step 3: Write the implementation**

`cmd/bucketvcs/init.go`:
```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/repo"
)

// runInit is the body of `bucketvcs init`. Returns the process exit
// code. stdout/stderr are injected for testability.
func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeURL := fs.String("store", "", `Store URL (e.g. "localfs:/var/lib/bucketvcs")`)
	actor := fs.String("actor", defaultActor(), "Actor recorded in the create tx record")
	branch := fs.String("default-branch", "refs/heads/main", "Default branch ref")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "init: --store is required")
		return 2
	}
	pos := fs.Args()
	if len(pos) != 2 {
		fmt.Fprintf(stderr, "init: want exactly 2 positional args (tenant repo), got %d\n", len(pos))
		return 2
	}
	tenantID, repoID := pos[0], pos[1]

	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	_, err = repo.Create(ctx, store, tenantID, repoID, repo.CreateOptions{
		DefaultBranch: *branch,
		Actor:         *actor,
	})
	if err != nil {
		if errors.Is(err, repo.ErrRepoExists) {
			fmt.Fprintf(stderr, "init: repo %s/%s already exists\n", tenantID, repoID)
			return 1
		}
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	fmt.Fprintf(stdout, "created %s/%s\n", tenantID, repoID)
	return 0
}

func defaultActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./cmd/bucketvcs/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/init.go cmd/bucketvcs/init_test.go
git commit -m "M1 cli: bucketvcs init"
```

---

## Task 15: CLI bucketvcs inspect-manifest

**Files:**
- Create: `cmd/bucketvcs/inspect.go`
- Test: `cmd/bucketvcs/inspect_test.go`

- [ ] **Step 1: Write the failing test**

`cmd/bucketvcs/inspect_test.go`:
```go
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunInspect_HumanFormat(t *testing.T) {
	dir := t.TempDir()
	var sink bytes.Buffer
	if code := runInit(context.Background(),
		[]string{"--store=localfs:" + dir, "acme", "my-repo"},
		&sink, &sink); code != 0 {
		t.Fatalf("init failed: exit %d, %s", code, sink.String())
	}

	var stdout, stderr bytes.Buffer
	code := runInspect(context.Background(),
		[]string{"--store=localfs:" + dir, "acme", "my-repo"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"schema_version", "1",
		"repo_id", "my-repo",
		"manifest_version", "1",
		"latest_tx", "tx_",
		"refs", "0",
		"packs", "0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRunInspect_JSONFormat(t *testing.T) {
	dir := t.TempDir()
	var sink bytes.Buffer
	runInit(context.Background(),
		[]string{"--store=localfs:" + dir, "acme", "x"}, &sink, &sink)

	var stdout, stderr bytes.Buffer
	code := runInspect(context.Background(),
		[]string{"--store=localfs:" + dir, "--json", "acme", "x"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, stderr.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout.String()), "{") {
		t.Errorf("--json output should be a JSON object, got: %s", stdout.String())
	}
}

func TestRunInspect_NotFound(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runInspect(context.Background(),
		[]string{"--store=localfs:" + dir, "acme", "missing"},
		&stdout, &stderr)
	if code != 2 {
		t.Errorf("want exit 2 (not found), got %d", code)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./cmd/bucketvcs/...`
Expected: failure — `runInspect` undefined.

- [ ] **Step 3: Write the implementation**

`cmd/bucketvcs/inspect.go`:
```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/bucketvcs/bucketvcs/internal/repo"
)

// runInspect is the body of `bucketvcs inspect-manifest`.
func runInspect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspect-manifest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeURL := fs.String("store", "", `Store URL (e.g. "localfs:/var/lib/bucketvcs")`)
	asJSON := fs.Bool("json", false, "Print the raw root manifest as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "inspect-manifest: --store is required")
		return 2
	}
	pos := fs.Args()
	if len(pos) != 2 {
		fmt.Fprintf(stderr, "inspect-manifest: want exactly 2 positional args (tenant repo), got %d\n", len(pos))
		return 2
	}
	tenantID, repoID := pos[0], pos[1]

	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintln(stderr, "inspect-manifest:", err)
		return 1
	}
	r, err := repo.Open(ctx, store, tenantID, repoID)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrRepoNotFound):
			fmt.Fprintf(stderr, "inspect-manifest: repo %s/%s not found\n", tenantID, repoID)
			return 2
		case errors.Is(err, repo.ErrUnsupportedSchema):
			fmt.Fprintln(stderr, "inspect-manifest:", err)
			return 3
		default:
			fmt.Fprintln(stderr, "inspect-manifest:", err)
			return 1
		}
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "inspect-manifest:", err)
		return 1
	}

	if *asJSON {
		// Re-emit the wrapped (header + body) manifest as JSON.
		var bodyMap map[string]json.RawMessage
		if err := json.Unmarshal(view.Body, &bodyMap); err != nil {
			fmt.Fprintln(stderr, "inspect-manifest: parse body:", err)
			return 1
		}
		headerJSON, _ := json.Marshal(view.Header)
		var headerMap map[string]json.RawMessage
		_ = json.Unmarshal(headerJSON, &headerMap)
		for k, v := range headerMap {
			bodyMap[k] = v
		}
		out, _ := json.MarshalIndent(bodyMap, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return 0
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "schema_version\t%d\n", view.Header.SchemaVersion)
	fmt.Fprintf(w, "min_reader_version\t%s\n", view.Header.MinReaderVersion)
	fmt.Fprintf(w, "repo_id\t%s\n", view.Header.RepoID)
	fmt.Fprintf(w, "object_format\t%s\n", view.Header.RepoFormat.ObjectFormat)
	fmt.Fprintf(w, "manifest_version\t%d\n", view.Header.ManifestVersion)
	fmt.Fprintf(w, "latest_tx\t%s\n", view.Header.LatestTx)
	fmt.Fprintf(w, "created_at\t%s\n", view.Header.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Fprintf(w, "updated_at\t%s\n", view.Header.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"))

	counts, _ := bodyCounts(view.Body)
	fmt.Fprintf(w, "refs\t%d entries\n", counts["refs"])
	fmt.Fprintf(w, "packs\t%d entries\n", counts["packs"])
	fmt.Fprintf(w, "indexes\t%d entries\n", counts["indexes"])
	fmt.Fprintf(w, "bundles\t%d entries\n", counts["bundles"])
	w.Flush()
	return 0
}

// bodyCounts returns the cardinality of well-known body collections.
// For object-typed fields ("refs", "indexes") the count is len(map); for
// array-typed fields ("packs", "bundles") it is len(slice). Unknown
// fields are skipped — M1 deliberately doesn't enforce body schema.
func bodyCounts(body json.RawMessage) (map[string]int, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	out := map[string]int{}
	for k, v := range m {
		switch k {
		case "refs", "indexes":
			var obj map[string]json.RawMessage
			if json.Unmarshal(v, &obj) == nil {
				out[k] = len(obj)
			}
		case "packs", "bundles":
			var arr []json.RawMessage
			if json.Unmarshal(v, &arr) == nil {
				out[k] = len(arr)
			}
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./cmd/bucketvcs/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/inspect.go cmd/bucketvcs/inspect_test.go
git commit -m "M1 cli: bucketvcs inspect-manifest"
```

---

## Task 16: CLI subcommand router

**Files:**
- Modify: `cmd/bucketvcs/main.go`
- Test: `cmd/bucketvcs/main_test.go`

- [ ] **Step 1: Write the failing test**

`cmd/bucketvcs/main_test.go`:
```go
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRun_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, &stdout, &stderr)
	if code == 0 {
		t.Errorf("want non-zero exit when no subcommand")
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr should print usage; got %q", stderr.String())
	}
}

func TestRun_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"frobulate"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("want non-zero exit on unknown subcommand")
	}
}

func TestRun_DispatchInit(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"init", "--store=localfs:" + dir, "acme", "x"},
		&stdout, &stderr)
	if code != 0 {
		t.Errorf("init dispatch: want 0, got %d; stderr=%s", code, stderr.String())
	}
}

func TestRun_DispatchInspect(t *testing.T) {
	dir := t.TempDir()
	var sink bytes.Buffer
	run(context.Background(), []string{"init", "--store=localfs:" + dir, "acme", "x"}, &sink, &sink)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"inspect-manifest", "--store=localfs:" + dir, "acme", "x"},
		&stdout, &stderr)
	if code != 0 {
		t.Errorf("inspect-manifest dispatch: want 0, got %d; stderr=%s", code, stderr.String())
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./cmd/bucketvcs/...`
Expected: failure — `run` undefined (or stale main.go).

- [ ] **Step 3: Replace `cmd/bucketvcs/main.go`**

```go
// Command bucketvcs is the bucketvcs CLI entry point. M1 wires two
// subcommands: `init` and `inspect-manifest`. Subcommand surface
// expands per-milestone (M3 adds the protocol gateway, M8 adds gc, etc.).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to a subcommand. Exit codes:
//
//	0  success
//	1  general error (store, IO, ...)
//	2  usage / not found
//	3  schema-gate refusal
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return runInit(ctx, rest, stdout, stderr)
	case "inspect-manifest":
		return runInspect(ctx, rest, stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "bucketvcs: unknown subcommand %q\n", sub)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: bucketvcs <subcommand> [flags] [args]

Subcommands:
  init               Create a new repo
  inspect-manifest   Print summary of the root manifest

Run "bucketvcs <subcommand> --help" for subcommand flags.
`)
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test -race ./cmd/bucketvcs/... && go build ./...`
Expected: tests PASS; build succeeds.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/main.go cmd/bucketvcs/main_test.go
git commit -m "M1 cli: subcommand router (init + inspect-manifest)"
```

---

## Task 17: Concurrency property test (M1 ship gate)

**Files:**
- Create: `internal/repo/internal/repo_concurrency_test.go`

- [ ] **Step 1: Write the test**

`internal/repo/internal/repo_concurrency_test.go`:
```go
// Package repointernal hosts concurrency tests that exercise the public
// internal/repo API surface against a real localfs store. These tests
// are the M1 ship gate per the design doc §8.3.
package repointernal_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	tx "github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestCommit_PropertyManifestVersionMonotonic(t *testing.T) {
	const (
		writers          = 8
		commitsPerWriter = 200
	)
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := repo.Create(ctx, store, "acme", "stress", repo.CreateOptions{Actor: "u_init"})
	if err != nil {
		t.Fatal(err)
	}

	var (
		wg          sync.WaitGroup
		seq         atomic.Int64 // unique per commit (writer*1e6 + i)
		committedTx sync.Map     // tx_id -> bool
	)
	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < commitsPerWriter; i++ {
				key := "k_" + strconv.Itoa(w*commitsPerWriter+i)
				txID, err := r.Commit(ctx,
					tx.Body{Type: "push", Actor: "u_" + strconv.Itoa(w)},
					func(prev *repo.RootView) ([]byte, error) {
						var top map[string]json.RawMessage
						if err := json.Unmarshal(prev.Body, &top); err != nil {
							return nil, err
						}
						top[key] = json.RawMessage("true")
						_ = seq.Add(1)
						return json.Marshal(top)
					},
				)
				if err != nil {
					t.Errorf("Commit failed: %v", err)
					return
				}
				committedTx.Store(txID, true)
			}
		}()
	}
	wg.Wait()

	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantManifestVersion := uint64(1 + writers*commitsPerWriter)
	if view.Header.ManifestVersion != wantManifestVersion {
		t.Errorf("ManifestVersion: want %d, got %d", wantManifestVersion, view.Header.ManifestVersion)
	}

	if _, ok := committedTx.Load(view.Header.LatestTx); !ok {
		t.Errorf("latest_tx %q not in committed set", view.Header.LatestTx)
	}

	// All commit-flagged keys must be present in body.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(view.Body, &top); err != nil {
		t.Fatal(err)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < commitsPerWriter; i++ {
			k := "k_" + strconv.Itoa(w*commitsPerWriter+i)
			if _, ok := top[k]; !ok {
				t.Errorf("body missing key %q", k)
			}
		}
	}
}
```

- [ ] **Step 2: Run, confirm pass**

Run: `go test -race ./internal/repo/internal/... -run TestCommit_Property -timeout 60s`
Expected: PASS within ~60s.

- [ ] **Step 3: Commit**

```bash
git add internal/repo/internal/repo_concurrency_test.go
git commit -m "M1 concurrency: property test (manifest_version monotonic)"
```

---

## Task 18: Concurrency scenario tests

**Files:**
- Modify: `internal/repo/internal/repo_concurrency_test.go`

- [ ] **Step 1: Append the scenario tests**

Append to `internal/repo/internal/repo_concurrency_test.go`:
```go
import (
	"errors"
	"time"
)

func TestCommit_Scenario_TwoWritersOneWins(t *testing.T) {
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "race", repo.CreateOptions{Actor: "u"})

	type result struct {
		txID string
		err  error
	}
	gate := make(chan struct{})
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			<-gate
			id, err := r.Commit(ctx,
				tx.Body{Type: "push", Actor: "u_" + strconv.Itoa(i)},
				func(prev *repo.RootView) ([]byte, error) { return prev.Body, nil },
			)
			results <- result{id, err}
		}()
	}
	close(gate)
	r1, r2 := <-results, <-results
	if r1.err != nil || r2.err != nil {
		t.Fatalf("both commits should eventually succeed: %v / %v", r1.err, r2.err)
	}
	if r1.txID == r2.txID {
		t.Fatalf("tx ids should differ: %q == %q", r1.txID, r2.txID)
	}
	view, _ := r.ReadRoot(ctx)
	if view.Header.ManifestVersion != 3 {
		t.Errorf("want manifest_version=3 (1 create + 2 commits), got %d", view.Header.ManifestVersion)
	}
}

func TestCommit_Scenario_CtxCancelMidCommit(t *testing.T) {
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := repo.Create(context.Background(), store, "acme", "x", repo.CreateOptions{Actor: "u"})

	_, err := r.Commit(ctx, tx.Body{Type: "push", Actor: "u"},
		func(prev *repo.RootView) ([]byte, error) {
			cancel() // cancel after callback runs but before CAS
			return prev.Body, nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
	view, _ := r.ReadRoot(context.Background())
	if view.Header.ManifestVersion != 1 {
		t.Errorf("manifest_version should remain 1, got %d", view.Header.ManifestVersion)
	}
}

func TestCommit_Scenario_CallbackErrorAborts(t *testing.T) {
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "x", repo.CreateOptions{Actor: "u"})

	sentinel := errors.New("nope")
	_, err := r.Commit(ctx, tx.Body{Type: "push", Actor: "u"},
		func(*repo.RootView) ([]byte, error) { return nil, sentinel })
	if !errors.Is(err, repo.ErrCallbackFailed) || !errors.Is(err, sentinel) {
		t.Errorf("want ErrCallbackFailed wrapping sentinel, got %v", err)
	}

	page, _ := store.List(ctx, "tenants/acme/repos/x/tx/", nil)
	if len(page.Objects) != 1 {
		t.Errorf("want 1 tx record (only the create), got %d", len(page.Objects))
	}
}

func TestCommit_Scenario_ReadDuringWrite(t *testing.T) {
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "x", repo.CreateOptions{Actor: "u"})

	stop := make(chan struct{})
	var (
		readerErrs atomic.Int64
		readerOps  atomic.Int64
	)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			v, err := r.ReadRoot(ctx)
			readerOps.Add(1)
			if err != nil {
				readerErrs.Add(1)
				return
			}
			if v.Header.SchemaVersion != 1 {
				readerErrs.Add(1)
				return
			}
		}
	}()

	deadline := time.After(2 * time.Second)
	for w := 0; w < 4; w++ {
		go func(w int) {
			for i := 0; ; i++ {
				select {
				case <-deadline:
					return
				default:
				}
				_, _ = r.Commit(ctx, tx.Body{Type: "push", Actor: "u"},
					func(prev *repo.RootView) ([]byte, error) { return prev.Body, nil })
			}
		}(w)
	}
	<-deadline
	close(stop)
	if readerErrs.Load() != 0 {
		t.Errorf("reader saw %d invalid snapshots over %d ops", readerErrs.Load(), readerOps.Load())
	}
}
```

- [ ] **Step 2: Run, confirm pass**

Run: `go test -race ./internal/repo/internal/... -timeout 90s`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/repo/internal/repo_concurrency_test.go
git commit -m "M1 concurrency: scenario tests (race, cancel, callback err, read-during-write)"
```

---

## Task 19: Stress test behind build tag

**Files:**
- Create: `internal/repo/internal/repo_stress_test.go`

- [ ] **Step 1: Write the test**

`internal/repo/internal/repo_stress_test.go`:
```go
//go:build stress

package repointernal_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	tx "github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestCommit_Stress(t *testing.T) {
	const (
		writers          = 100
		commitsPerWriter = 1000
	)
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	r, _ := repo.Create(ctx, store, "acme", "stress", repo.CreateOptions{Actor: "u"})

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < commitsPerWriter; i++ {
				key := "k_" + strconv.Itoa(w*commitsPerWriter+i)
				_, err := r.Commit(ctx,
					tx.Body{Type: "push", Actor: "u_" + strconv.Itoa(w)},
					func(prev *repo.RootView) ([]byte, error) {
						var top map[string]json.RawMessage
						if err := json.Unmarshal(prev.Body, &top); err != nil {
							return nil, err
						}
						top[key] = json.RawMessage("true")
						return json.Marshal(top)
					},
				)
				if err != nil {
					t.Errorf("commit failed: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	v, _ := r.ReadRoot(ctx)
	if want := uint64(1 + writers*commitsPerWriter); v.Header.ManifestVersion != want {
		t.Errorf("manifest_version: want %d, got %d", want, v.Header.ManifestVersion)
	}
}
```

- [ ] **Step 2: Run with the build tag**

Run: `go test -race -tags stress -count=1 ./internal/repo/internal/... -run TestCommit_Stress -timeout 5m`
Expected: PASS within ~60s on a laptop.

- [ ] **Step 3: Commit**

```bash
git add internal/repo/internal/repo_stress_test.go
git commit -m "M1 concurrency: stress test (100x1000 commits, build tag stress)"
```

---

## Task 20: Synthetic future-schema fixture test

**Files:**
- Modify: `internal/repo/repo_test.go`

- [ ] **Step 1: Confirm Task 10 already covers this** — `TestOpen_FutureSchemaRejected` is in `repo_test.go` from Task 10. Verify it still passes:

Run: `go test -race ./internal/repo/... -run TestOpen_FutureSchemaRejected`
Expected: PASS. **No new task work; this exists for explicit traceability against the spec's "synthetic v999 fixture" requirement.**

- [ ] **Step 2: Mark satisfied** — no commit; this is a verification step.

---

## Task 21: README + cross-package documentation

**Files:**
- Create: `internal/repo/README.md`
- Modify: `internal/storage/README.md` (add a forward-pointer to internal/repo)

- [ ] **Step 1: Write `internal/repo/README.md`**

`internal/repo/README.md`:
````markdown
# `internal/repo`

The M1 thin transaction kernel: the only place in the codebase that
atomically advances a repo from one durable state to the next. Sits
between [`internal/storage`](../storage) (M0) and the future Git object
engine (M2).

## Status

M1 ships:

- `Repo` handle with `Open`, `Create`, `ReadRoot`, `Commit`
- `internal/repo/keys` — constructors for the entire §6 path layout
- `internal/repo/manifest` — `RootHeader` struct + §43.7 schema gate +
  CAS helpers
- `internal/repo/tx` — header/body split + `PutIfAbsent` writer
- `bucketvcs init` and `bucketvcs inspect-manifest` CLI subcommands

## What this package owns

1. **The §6 key naming contract.** Every path inside
   `/tenants/{tenant_id}/repos/{repo_id}/` is constructed via `keys`.
   M2/M3/M8 do not invent paths.
2. **The §7 root-manifest CAS.** `Commit` reads `manifest/root.json`,
   invokes the caller's `buildBody` callback, splices in M1-owned
   header fields, and atomically swaps the root via
   `ObjectStore.PutIfVersionMatches`.
3. **The §8 transaction-record-then-CAS ordering.** Each `Commit`
   attempt mints a fresh ULID, writes the immutable tx record, then
   attempts the CAS. On conflict, retry with a fresh tx_id; on
   exhaustion, return `*CommitGaveUpError` carrying the orphan IDs.

## What this package does NOT own

- Refs, pack content, reachability indexes, bundles — M2.
- Sharded refs (`manifest/ref-shards/`) — M12.
- Garbage collection of orphan tx records — M8.
- Git protocol surface — M3.
- Authentication / tenants — M4 / commercial scope.

## Concurrency-test conformance bar

The concurrency suite in `internal/repo/internal/` is the M1 ship
gate. **Cloud adapters at M5 (R2 or S3) and M7 (the others) MUST run
this same suite against their backend before claiming conformance.**
The kernel is identical; only the underlying `ObjectStore` changes.

## Adding a Commit caller

```go
import "github.com/bucketvcs/bucketvcs/internal/repo"
import tx "github.com/bucketvcs/bucketvcs/internal/repo/tx"

txID, err := repo.Commit(ctx, tx.Body{Type: "push", Actor: actor},
    func(prev *repo.RootView) ([]byte, error) {
        // Caller mutates the body — refs, packs, etc. — and returns
        // the new body bytes. M1 splices in header fields; the caller
        // must NOT include header keys in the returned bytes.
        return mutate(prev.Body)
    },
)
```

The callback may be invoked multiple times (once per CAS attempt).
Make the callback idempotent against `prev`; do not let it depend on
state captured before the first call.

## Schema gate

`schema_version 1` is the only value M1 reads or writes. Manifests
with `schema_version > 1` or `min_reader_version > 0.1.0` are rejected
with `repo.ErrUnsupportedSchema`. When a real schema bump lands at M2+,
extend `manifest.CurrentSchemaVersion` and update the gate.
````

- [ ] **Step 2: Add forward-pointer to storage README**

Append to `internal/storage/README.md`:
```markdown

## Consumers

- [`internal/repo`](../repo) — M1 transaction kernel; the only
  consumer of `PutIfVersionMatches` / `PutIfAbsent` for repository
  state. New consumers of `ObjectStore` should not write directly to
  `manifest/root.json` or `tx/*.json`; go through `internal/repo`.
```

- [ ] **Step 3: Commit**

```bash
git add internal/repo/README.md internal/storage/README.md
git commit -m "M1 docs: internal/repo README + storage forward-pointer"
```

---

## Task 22: Final verification + summary commit

**Files:** none (verification only)

- [ ] **Step 1: Run the full test suite**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 2: Run the stress test**

Run: `go test -race -tags stress -count=1 ./internal/repo/internal/... -run TestCommit_Stress -timeout 5m`
Expected: PASS.

- [ ] **Step 3: Build the CLI and smoke-test end-to-end**

```bash
go build -o /tmp/bucketvcs ./cmd/bucketvcs
mkdir -p /tmp/m1-smoke
/tmp/bucketvcs init --store=localfs:/tmp/m1-smoke acme my-repo
/tmp/bucketvcs inspect-manifest --store=localfs:/tmp/m1-smoke acme my-repo
/tmp/bucketvcs inspect-manifest --store=localfs:/tmp/m1-smoke --json acme my-repo
```

Expected:
- `init` prints `created acme/my-repo` and exits 0.
- First `inspect-manifest` prints a human table with `manifest_version 1`.
- `--json` invocation prints the full root manifest as JSON.

- [ ] **Step 4: `git log --oneline` review**

Run: `git log --oneline main..HEAD`
Expected: ~21 focused commits, one per task. Verify each commit message is descriptive.

- [ ] **Step 5: If working in a worktree, prepare for merge**

If using a worktree, follow the same merge pattern as M0: `git checkout main && git merge --no-ff <branch> -m "M1 transaction kernel: internal/repo + bucketvcs init/inspect-manifest"` then tag `git tag -a m1-complete -m "M1 transaction kernel"`.

If working directly on `main` or another branch, defer the merge/tag step to the user.

---

## Spec coverage map

For traceability, every spec section maps to at least one task:

| Spec section | Task(s) |
|---|---|
| §1 Purpose / boundary | Task 21 (README) |
| §2 Package layout | Tasks 1, 21 |
| §3.1 Repo handle API | Tasks 10, 11, 12 |
| §3.2 Manifest header | Task 6 |
| §3.3 Tx record body | Task 9 |
| §3.4 Errors | Task 2 |
| §4 Commit data flow + invariants | Task 12 |
| §4.2 Orphan tx records | Task 12 (covered by `TestCommit_RetryBudgetExhausted` + `TestCommit_PerAttemptFreshTxID`) |
| §4.3 Create carve-out | Task 11 (covered by `TestCreate_AlreadyExists` orphan check) |
| §5 Schema versioning | Tasks 7, 20 |
| §6 Keys package | Tasks 3, 4, 5 |
| §7.1 `bucketvcs init` | Task 14 |
| §7.2 `bucketvcs inspect-manifest` | Task 15 |
| §8.1 Unit tests | Tasks 2, 3, 4, 5, 6, 7, 8, 9 |
| §8.2 Integration tests | Tasks 10, 11, 12, 14, 15, 16 |
| §8.3 Concurrency suite (ship gate) | Tasks 17, 18, 19 |
| §9.1 Non-goals | Task 21 (README) |
| §9.2 Contracts M2 inherits | Task 21 (README) |
| §9.3 Carry-forward open questions | Spec §9.3 + Task 21 |

## Notes for the executing engineer

- **Run `go test -race ./...` after every task.** The M1 invariants are subtle; the race detector catches them quickly.
- **If a step fails:** investigate before moving on. The localfs adapter's behavior (especially around `PutIfVersionMatches` and version tokens) is documented in `internal/storage/README.md`. Check there before assuming a logic bug in `internal/repo`.
- **`localfs.Open(dir)` is the constructor** (verified: `internal/storage/localfs/localfs.go:60`). All `localfs.Open(t.TempDir())` calls in this plan match.
- **`ObjectStore.List(ctx, prefix, *ListOptions)` returns `*storage.ListPage` with field `Objects []ObjectMetadata`** (verified: `internal/storage/options.go:84`). The plan uses `page.Objects`.
- **`ObjectStore.PutIfVersionMatches(ctx, key, expected, body, opts)`** — `expected` precedes `body` (verified: `internal/storage/localfs/localfs.go:350`). The plan's `manifest.CASRoot` matches.
- **Don't add `cobra` or any CLI framework.** Keep the M1 CLI on stdlib `flag` per the design's YAGNI stance.
- **Don't extend the schema gate to permit `schema_version: 0` "for compatibility".** It is intentionally fail-closed below as well as above.
- **Frequent commits.** Each task ends in a commit. Don't batch.
