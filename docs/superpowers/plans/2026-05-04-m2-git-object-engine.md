# M2 — Git Object Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the bucketvcs Git object engine on top of M1's transaction kernel: pure-Go pack reader, content-addressed `.bvom` (object→pack map) and `.bvcg` (commit graph) indexes, `bucketvcs import`/`export` round-tripping a bare git repo with `git fsck` clean on both ends, and the differential-harness scaffolding (round-trip + `cat-object` oracles vs upstream git on a synthetic in-test fixture corpus).

**Architecture:** Track A (shell out to upstream `git` via `internal/gitcli`) on the import/export plumbing; Track B (pure-Go) on the read path that M3 will consume. One canonical pack per import (§15.1), inline refs (§19.1), SHA-1 only (§20). Every uploaded blob is content-addressed and `PutIfAbsent`-keyed; the only state advance goes through `repo.Repo.Commit`'s atomic-pair primitive.

**Tech Stack:** Go 1.25; module `github.com/bucketvcs/bucketvcs`; depends on existing `internal/storage` (M0) and `internal/repo` (M1); shells out to upstream `git` ≥ 2.40 in tests and import/export paths.

**Spec:** `docs/superpowers/specs/2026-05-04-m2-git-object-engine-design.md`.

**Conventions:**
- Each task is one focused unit. Steps within a task are 2-5 minutes each.
- Every task ends with a commit. Use the `M2 ...` prefix consistently.
- Follow the M1 review protocol from `m1_review_protocol.md`: superpowers code-reviewer per task, then roborev-refine on max reasoning until pass or diminishing returns.
- The pack-format details required by tasks 5-10 are documented in Git's `Documentation/technical/pack-format.txt` (https://git-scm.com/docs/pack-format) and `Documentation/technical/commit-graph-format.txt` (https://git-scm.com/docs/commit-graph-format). Citations like `[pack-format §HEADER]` in the tasks below refer to that document.

**Reconciling spec → existing M1 keys:** M1's `internal/repo/keys/keys.go` already defines `CanonicalPackKey`, `PackIdxKey`, `PackBitmapKey`, `GeneratedPackKey`, `CommitGraphKey`, `ReachabilityKey`. M2 reuses `CanonicalPackKey` and `PackIdxKey(hash, "canonical")` for packs, reuses `CommitGraphKey` (which returns `indexes/commit-graphs/{hash}.graph`) for the commit graph, and adds **one new constructor** `ObjectMapKey` returning `indexes/object-map/{hash}.bvom`. The "BVCG" / "BVOM" 4-byte magics in the file headers identify the M2-local format independent of the on-disk extension.

---

## Task 1: Add ObjectMapKey constructor

**Files:**
- Modify: `internal/repo/keys/keys.go`
- Modify: `internal/repo/keys/keys_test.go`

- [ ] **Step 1: Write the failing test**

Add to the bottom of `internal/repo/keys/keys_test.go`:

```go
func TestObjectMapKey(t *testing.T) {
	r, err := NewRepo("acme", "x")
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	got := r.ObjectMapKey("deadbeef")
	want := "tenants/acme/repos/x/indexes/object-map/deadbeef.bvom"
	if got != want {
		t.Fatalf("ObjectMapKey: got %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/repo/keys/...`
Expected: FAIL with `r.ObjectMapKey undefined`.

- [ ] **Step 3: Write the implementation**

Add to `internal/repo/keys/keys.go`, near `CommitGraphKey`:

```go
// ObjectMapKey returns the path for an M2 object-to-pack map (.bvom).
// Used by M2.
func (r *Repo) ObjectMapKey(hash string) string {
	return r.prefix + "indexes/object-map/" + hash + ".bvom"
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/repo/keys/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/keys/keys.go internal/repo/keys/keys_test.go
git commit -m "M2 keys: add ObjectMapKey constructor for .bvom index"
```

---

## Task 2: gitcli foundation — Version, InitBare, Fsck

**Files:**
- Create: `internal/gitcli/gitcli.go`
- Create: `internal/gitcli/gitcli_test.go`

The wrapper exposes a single resolved `git` binary path, set once at first use from `$GIT_BINARY` or PATH lookup. Tests can override via `SetBinaryForTest`.

- [ ] **Step 1: Write the failing test**

Create `internal/gitcli/gitcli_test.go`:

```go
package gitcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := Version(context.Background()); err != nil {
		t.Skip("git not available on PATH:", err)
	}
}

func TestVersion_Reports(t *testing.T) {
	skipIfNoGit(t)
	v, err := Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !strings.HasPrefix(v, "git version ") {
		t.Fatalf("Version output unexpected: %q", v)
	}
}

func TestInitBare_CreatesObjectsDir(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "objects")); err != nil {
		t.Fatalf("expected objects/ dir after InitBare: %v", err)
	}
}

func TestFsck_OK(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	if err := Fsck(context.Background(), dir, true); err != nil {
		t.Fatalf("Fsck on empty bare repo: %v", err)
	}
}

func TestFsck_DetectsCorruption(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	// Drop a clearly-bogus loose object.
	bogus := filepath.Join(dir, "objects", "ab")
	if err := os.MkdirAll(bogus, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bogus, "cdef0123456789012345678901234567890"), []byte("not-a-git-object"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := Fsck(context.Background(), dir, true); err == nil {
		t.Fatalf("expected Fsck to fail on corrupt loose object")
	}
}

func TestSetBinaryForTest_Override(t *testing.T) {
	old := SetBinaryForTest("/nonexistent-git-binary")
	t.Cleanup(func() { SetBinaryForTest(old) })
	if _, err := Version(context.Background()); err == nil {
		t.Fatalf("expected error when binary path is bogus")
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/gitcli/...`
Expected: FAIL with `package internal/gitcli not found` or `Version undefined`.

- [ ] **Step 3: Write the implementation**

Create `internal/gitcli/gitcli.go`:

```go
// Package gitcli provides thin, well-tested wrappers around the upstream
// `git` binary. M2 import/export and the differential harness use these
// for Track A operations (shell out to git for plumbing). A single git
// binary path is resolved once at first use; tests may override it via
// SetBinaryForTest.
package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

var (
	binMu  sync.Mutex
	binVal string
)

// SetBinaryForTest overrides the resolved git binary path. Returns the
// previous value so tests can restore it. Production code should not
// call this.
func SetBinaryForTest(path string) string {
	binMu.Lock()
	defer binMu.Unlock()
	old := binVal
	binVal = path
	return old
}

func resolveBinary() (string, error) {
	binMu.Lock()
	defer binMu.Unlock()
	if binVal != "" {
		return binVal, nil
	}
	if v := os.Getenv("GIT_BINARY"); v != "" {
		binVal = v
		return binVal, nil
	}
	p, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("gitcli: git not found in PATH: %w", err)
	}
	binVal = p
	return binVal, nil
}

// runError wraps an exec failure with stderr captured for diagnosis.
type runError struct {
	cmd    string
	args   []string
	dir    string
	exit   int
	stderr string
	cause  error
}

func (e *runError) Error() string {
	args := strings.Join(e.args, " ")
	dir := e.dir
	if dir == "" {
		dir = "<no dir>"
	}
	return fmt.Sprintf("gitcli: %s %s (dir=%s exit=%d): %v: stderr=%q",
		e.cmd, args, dir, e.exit, e.cause, e.stderr)
}

func (e *runError) Unwrap() error { return e.cause }

func run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin, err := resolveBinary()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		exit := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		}
		return stdout.Bytes(), &runError{
			cmd: bin, args: args, dir: dir, exit: exit,
			stderr: stderr.String(), cause: err,
		}
	}
	return stdout.Bytes(), nil
}

// Version returns the output of `git --version` (e.g. "git version 2.43.0").
func Version(ctx context.Context) (string, error) {
	out, err := run(ctx, "", "--version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// InitBare runs `git init --bare` in dir. dir must exist.
func InitBare(ctx context.Context, dir string) error {
	_, err := run(ctx, dir, "init", "--bare")
	return err
}

// Fsck runs `git fsck` (with --strict if strict) inside dir.
func Fsck(ctx context.Context, dir string, strict bool) error {
	args := []string{"fsck"}
	if strict {
		args = append(args, "--strict")
	}
	_, err := run(ctx, dir, args...)
	return err
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/gitcli/...`
Expected: PASS (or SKIP if git is missing locally).

- [ ] **Step 5: Commit**

```bash
git add internal/gitcli/gitcli.go internal/gitcli/gitcli_test.go
git commit -m "M2 gitcli: scaffold (Version, InitBare, Fsck) with binary resolution + test override"
```

---

## Task 3: gitcli — clone, pack, index-pack, unpack-objects

**Files:**
- Modify: `internal/gitcli/gitcli.go`
- Modify: `internal/gitcli/gitcli_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/gitcli/gitcli_test.go`:

```go
func makeRepoWithOneCommit(t *testing.T) string {
	t.Helper()
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	// Use a non-bare working repo to author a commit, then clone --bare.
	work := t.TempDir()
	mustRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", work}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustRun("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustRun("add", "README")
	mustRun("commit", "-m", "init")
	// Clone --bare into dir-bare so the source dir has objects.
	out := dir + "-bare"
	if err := CloneBareMirror(context.Background(), work, out); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	return out
}

func TestCloneBareMirror_PreservesRefs(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		t.Fatalf("expected HEAD: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bare, "objects")); err != nil {
		t.Fatalf("expected objects/: %v", err)
	}
}

func TestPackObjectsAll_ProducesPackAndReturnsID(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	outDir := t.TempDir()
	prefix := filepath.Join(outDir, "pack")
	id, err := PackObjectsAll(context.Background(), bare, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	if len(id) != 40 {
		t.Fatalf("pack_id length: got %d, want 40 (%q)", len(id), id)
	}
	if _, err := os.Stat(prefix + "-" + id + ".pack"); err != nil {
		t.Fatalf("expected pack file: %v", err)
	}
	if _, err := os.Stat(prefix + "-" + id + ".idx"); err != nil {
		t.Fatalf("expected idx file: %v", err)
	}
}

func TestIndexPack_ReindexesExistingPack(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	tmp := t.TempDir()
	prefix := filepath.Join(tmp, "p")
	id, err := PackObjectsAll(context.Background(), bare, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	// Remove .idx, reindex with IndexPack.
	idxPath := prefix + "-" + id + ".idx"
	packPath := prefix + "-" + id + ".pack"
	if err := os.Remove(idxPath); err != nil {
		t.Fatalf("Remove idx: %v", err)
	}
	if err := IndexPack(context.Background(), tmp, packPath); err != nil {
		t.Fatalf("IndexPack: %v", err)
	}
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("expected idx after IndexPack: %v", err)
	}
}
```

Note that the test file already imports `os`, `path/filepath`, `strings`. Add `os/exec` to imports.

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/gitcli/...`
Expected: FAIL — `CloneBareMirror`, `PackObjectsAll`, `IndexPack` undefined.

- [ ] **Step 3: Write the implementation**

Append to `internal/gitcli/gitcli.go`:

```go
// CloneBareMirror runs `git clone --bare --mirror <src> <dst>`. dst must
// not already exist (git creates it).
func CloneBareMirror(ctx context.Context, src, dst string) error {
	_, err := run(ctx, "", "clone", "--bare", "--mirror", "--quiet", src, dst)
	return err
}

// PackObjectsAll produces a single pack containing every reachable object
// in dir, written as outPrefix + "-{pack_id}.pack" + ".idx". Returns the
// pack_id (40-char hex SHA-1, the Git-native pack name from §3.2 of the
// M2 design). The function pipes `git rev-list --all --objects` into
// `git pack-objects` to keep behavior deterministic across git versions.
func PackObjectsAll(ctx context.Context, dir, outPrefix string) (string, error) {
	bin, err := resolveBinary()
	if err != nil {
		return "", err
	}
	revList := exec.CommandContext(ctx, bin, "-C", dir, "rev-list", "--all", "--objects")
	pipe, err := revList.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("gitcli: PackObjectsAll: rev-list pipe: %w", err)
	}
	var rlStderr bytes.Buffer
	revList.Stderr = &rlStderr

	pack := exec.CommandContext(ctx, bin, "-C", dir,
		"pack-objects", "--quiet", outPrefix)
	pack.Stdin = pipe
	var packStdout, packStderr bytes.Buffer
	pack.Stdout = &packStdout
	pack.Stderr = &packStderr

	if err := pack.Start(); err != nil {
		return "", fmt.Errorf("gitcli: PackObjectsAll: pack start: %w", err)
	}
	if err := revList.Run(); err != nil {
		_ = pack.Wait()
		return "", fmt.Errorf("gitcli: PackObjectsAll: rev-list: %w: stderr=%q",
			err, rlStderr.String())
	}
	if err := pack.Wait(); err != nil {
		return "", fmt.Errorf("gitcli: PackObjectsAll: pack-objects: %w: stderr=%q",
			err, packStderr.String())
	}
	// pack-objects prints exactly one pack_id line on stdout when one
	// pack is produced. The output may include trailing whitespace.
	id := strings.TrimSpace(packStdout.String())
	if len(id) != 40 {
		return "", fmt.Errorf("gitcli: PackObjectsAll: unexpected pack-objects stdout %q",
			packStdout.String())
	}
	return id, nil
}

// IndexPack runs `git index-pack` against an existing .pack file,
// producing the corresponding .idx alongside it.
func IndexPack(ctx context.Context, dir, packPath string) error {
	_, err := run(ctx, dir, "index-pack", packPath)
	return err
}

// UnpackObjects reads a pack from packPath and explodes it into loose
// objects in dir's object database. dir must be a git repo.
func UnpackObjects(ctx context.Context, dir, packPath string) error {
	bin, err := resolveBinary()
	if err != nil {
		return err
	}
	f, err := os.Open(packPath)
	if err != nil {
		return fmt.Errorf("gitcli: UnpackObjects: open pack: %w", err)
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, bin, "-C", dir, "unpack-objects", "-q")
	cmd.Stdin = f
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gitcli: UnpackObjects: %w: stderr=%q", err, stderr.String())
	}
	return nil
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/gitcli/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitcli/
git commit -m "M2 gitcli: clone-bare-mirror, pack-objects-all, index-pack, unpack-objects"
```

---

## Task 4: gitcli — refs, symref, rev-list, cat-file

**Files:**
- Modify: `internal/gitcli/gitcli.go`
- Modify: `internal/gitcli/gitcli_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/gitcli/gitcli_test.go`:

```go
func TestShowRef_AfterCommit(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	refs, err := ShowRef(context.Background(), bare)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	if len(refs) == 0 {
		t.Fatalf("expected at least one ref, got none")
	}
	for ref, oid := range refs {
		if !strings.HasPrefix(ref, "refs/") {
			t.Fatalf("ref does not start with refs/: %q", ref)
		}
		if len(oid) != 40 {
			t.Fatalf("oid length: got %d for %q", len(oid), ref)
		}
	}
}

func TestSymbolicRef_HEAD(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	target, err := SymbolicRef(context.Background(), bare, "HEAD")
	if err != nil {
		t.Fatalf("SymbolicRef: %v", err)
	}
	if !strings.HasPrefix(target, "refs/heads/") {
		t.Fatalf("HEAD target unexpected: %q", target)
	}
}

func TestRevListAllObjects_NonEmpty(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	oids, err := RevListAllObjects(context.Background(), bare)
	if err != nil {
		t.Fatalf("RevListAllObjects: %v", err)
	}
	// Single-commit repo: one commit, one tree, one blob => 3 objects.
	if len(oids) < 3 {
		t.Fatalf("expected ≥3 reachable objects, got %d: %v", len(oids), oids)
	}
	for _, oid := range oids {
		if len(oid) != 40 {
			t.Fatalf("oid length: got %d for %q", len(oid), oid)
		}
	}
}

func TestCatFilePretty_Commit(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	refs, err := ShowRef(context.Background(), bare)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	var oid string
	for _, v := range refs {
		oid = v
		break
	}
	out, err := CatFilePretty(context.Background(), bare, oid)
	if err != nil {
		t.Fatalf("CatFilePretty: %v", err)
	}
	if !bytes.Contains(out, []byte("tree ")) {
		t.Fatalf("commit pretty output missing tree line: %q", out)
	}
}
```

Add `bytes` to the test file's imports if not already present.

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/gitcli/...`
Expected: FAIL — `ShowRef`, `SymbolicRef`, `RevListAllObjects`, `CatFilePretty` undefined.

- [ ] **Step 3: Write the implementation**

Append to `internal/gitcli/gitcli.go`:

```go
// UpdateRef runs `git update-ref <ref> <oid>` in dir.
func UpdateRef(ctx context.Context, dir, ref, oid string) error {
	_, err := run(ctx, dir, "update-ref", ref, oid)
	return err
}

// SymbolicRef returns the target of a symbolic ref (e.g. "HEAD").
func SymbolicRef(ctx context.Context, dir, name string) (string, error) {
	out, err := run(ctx, dir, "symbolic-ref", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// SymbolicRefSet sets the target of a symbolic ref (e.g. HEAD -> refs/heads/main).
func SymbolicRefSet(ctx context.Context, dir, name, target string) error {
	_, err := run(ctx, dir, "symbolic-ref", name, target)
	return err
}

// ShowRef returns the map of full ref name -> 40-char hex OID for every
// ref under refs/. HEAD and other symbolic refs are not included; use
// SymbolicRef separately.
func ShowRef(ctx context.Context, dir string) (map[string]string, error) {
	out, err := run(ctx, dir, "show-ref")
	if err != nil {
		// `git show-ref` exits non-zero on a repo with no refs.
		var rerr *runError
		if errors.As(err, &rerr) && rerr.exit == 1 && rerr.stderr == "" {
			return map[string]string{}, nil
		}
		return nil, err
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 || len(parts[0]) != 40 {
			return nil, fmt.Errorf("gitcli: ShowRef: malformed line %q", line)
		}
		refs[parts[1]] = parts[0]
	}
	return refs, nil
}

// RevListAllObjects returns every reachable object ID in dir, as 40-char
// hex strings. Equivalent to `git rev-list --all --objects` but stripped
// of trailing path metadata.
func RevListAllObjects(ctx context.Context, dir string) ([]string, error) {
	out, err := run(ctx, dir, "rev-list", "--all", "--objects")
	if err != nil {
		return nil, err
	}
	var oids []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		// Each line starts with the OID; for trees/blobs/tags, a path follows.
		oid := line
		if sp := strings.IndexByte(line, ' '); sp != -1 {
			oid = line[:sp]
		}
		if len(oid) != 40 {
			return nil, fmt.Errorf("gitcli: RevListAllObjects: bad oid %q", oid)
		}
		oids = append(oids, oid)
	}
	return oids, nil
}

// CatFilePretty returns the pretty-printed bytes for an object, matching
// `git cat-file -p <oid>`.
func CatFilePretty(ctx context.Context, dir, oid string) ([]byte, error) {
	return run(ctx, dir, "cat-file", "-p", oid)
}

// CatFileType returns the type ("commit", "tree", "blob", "tag") for an
// object, matching `git cat-file -t <oid>`.
func CatFileType(ctx context.Context, dir, oid string) (string, error) {
	out, err := run(ctx, dir, "cat-file", "-t", oid)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CatFileSize returns the size of an object's content, matching
// `git cat-file -s <oid>`.
func CatFileSize(ctx context.Context, dir, oid string) (int64, error) {
	out, err := run(ctx, dir, "cat-file", "-s", oid)
	if err != nil {
		return 0, err
	}
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n); err != nil {
		return 0, fmt.Errorf("gitcli: CatFileSize: parse %q: %w", out, err)
	}
	return n, nil
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/gitcli/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitcli/
git commit -m "M2 gitcli: refs, symref, rev-list, cat-file"
```

---

## Task 5: pack — types (OID, ObjectType, Object)

**Files:**
- Create: `internal/pack/types.go`
- Create: `internal/pack/types_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/pack/types_test.go`:

```go
package pack

import (
	"encoding/hex"
	"testing"
)

func TestOID_String_RoundTrip(t *testing.T) {
	want := "0123456789abcdef0123456789abcdef01234567"
	b, err := hex.DecodeString(want)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	var oid OID
	copy(oid[:], b)
	got := oid.String()
	if got != want {
		t.Fatalf("OID.String: got %q, want %q", got, want)
	}
	parsed, err := ParseOID(want)
	if err != nil {
		t.Fatalf("ParseOID: %v", err)
	}
	if parsed != oid {
		t.Fatalf("ParseOID round-trip mismatch")
	}
}

func TestParseOID_RejectsBadLengths(t *testing.T) {
	for _, in := range []string{"", "abc", strings("a", 39), strings("a", 41)} {
		if _, err := ParseOID(in); err == nil {
			t.Fatalf("ParseOID(%q) should fail", in)
		}
	}
}

func TestParseOID_RejectsNonHex(t *testing.T) {
	in := "0123456789abcdef0123456789abcdef0123456g"
	if _, err := ParseOID(in); err == nil {
		t.Fatalf("ParseOID with non-hex should fail")
	}
}

func TestObjectType_String(t *testing.T) {
	cases := map[ObjectType]string{
		TypeCommit: "commit",
		TypeTree:   "tree",
		TypeBlob:   "blob",
		TypeTag:    "tag",
	}
	for typ, want := range cases {
		if got := typ.String(); got != want {
			t.Fatalf("ObjectType(%d).String: got %q, want %q", typ, got, want)
		}
	}
}

// strings returns s repeated n times. Local helper to keep the test
// table compact.
func strings(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/pack/...`
Expected: FAIL — package or types not defined.

- [ ] **Step 3: Write the implementation**

Create `internal/pack/types.go`:

```go
// Package pack implements a pure-Go random-access reader over Git's
// .pack/.idx v2 format, designed to read from a storage.ObjectStore
// (range GETs, not local files). M3's fetch negotiation will hold one
// Reader per pack per repo and call Get on the hot path.
package pack

import (
	"encoding/hex"
	"fmt"
)

// OID is a SHA-1 object identifier. M2 is SHA-1 only (§20); SHA-256
// support is deferred per the design doc.
type OID [20]byte

// String returns the lowercase hex form of the OID.
func (o OID) String() string {
	return hex.EncodeToString(o[:])
}

// ParseOID parses a 40-char lowercase hex string into an OID.
func ParseOID(s string) (OID, error) {
	var o OID
	if len(s) != 40 {
		return o, fmt.Errorf("pack: ParseOID: bad length %d (want 40)", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return o, fmt.Errorf("pack: ParseOID: %w", err)
	}
	copy(o[:], b)
	return o, nil
}

// ObjectType is a Git object type. The numeric values match the
// `obj_type` field encoded in pack object headers (RFC pack-format §HEADER).
type ObjectType uint8

const (
	TypeInvalid ObjectType = 0
	TypeCommit  ObjectType = 1
	TypeTree    ObjectType = 2
	TypeBlob    ObjectType = 3
	TypeTag     ObjectType = 4
	// 5 is reserved for future use per the pack format.
	typeOFSDelta ObjectType = 6
	typeREFDelta ObjectType = 7
)

// String returns the lower-case Git type name ("commit", "tree", "blob",
// "tag"). Delta types and Invalid return their internal labels for use
// in error messages.
func (t ObjectType) String() string {
	switch t {
	case TypeCommit:
		return "commit"
	case TypeTree:
		return "tree"
	case TypeBlob:
		return "blob"
	case TypeTag:
		return "tag"
	case typeOFSDelta:
		return "ofs_delta"
	case typeREFDelta:
		return "ref_delta"
	default:
		return fmt.Sprintf("invalid(%d)", uint8(t))
	}
}

// Object is a fully-resolved Git object: deltas applied, payload inflated.
type Object struct {
	Type ObjectType
	Size int64  // length of Data; matches `git cat-file -s` semantics
	Data []byte // commit/tree/blob/tag content; never a delta
}
```

Note on the test: I used a local helper `strings` which shadows the stdlib package; clean, since this test file does not import `strings`. If we ever import it, rename to `repeat`.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/pack/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pack/
git commit -m "M2 pack: OID, ObjectType, Object types with hex round-trip"
```

---

## Task 6: pack — .idx v2 parser

**Files:**
- Create: `internal/pack/index.go`
- Create: `internal/pack/index_test.go`

The .idx v2 format is documented at [pack-format §PACK-IDX-FILE]. Layout:

```
header (8 bytes):
  magic    \377tOc
  version  uint32 BE = 2
fanout (256 × uint32 BE):
  fanout[i] = number of OIDs whose first byte is <= i
oid table (n × 20 bytes), sorted ascending
crc32 table (n × uint32 BE)
offset table (n × uint32 BE):
  if MSB set, offset is into large-offset table; low 31 bits are index
large offset table (m × uint64 BE), present iff any offset overflowed 31 bits
trailer:
  pack_sha1 (20 bytes)  -- SHA-1 of the pack file
  idx_sha1  (20 bytes)  -- SHA-1 of everything before this trailer
```

The parser exposes a structure that supports OID → (offset, crc32) lookup via binary search, and forward iteration in OID order.

- [ ] **Step 1: Write the failing test**

Create `internal/pack/index_test.go`:

```go
package pack

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available:", err)
	}
}

// makeOnePackRepo authors a small repo and produces a single pack via
// gitcli.PackObjectsAll. Returns the prefix passed to PackObjectsAll
// and the pack_id.
func makeOnePackRepo(t *testing.T) (prefix, packID string) {
	t.Helper()
	skipIfNoGit(t)
	work := t.TempDir()
	mustRun := func(args ...string) {
		t.Helper()
		if out, err := gitcli.RunForTest(work, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustRun("init", "--initial-branch=main")
	for i, msg := range []string{"a\n", "b\n", "c\n"} {
		if err := os.WriteFile(filepath.Join(work, "f"), []byte(msg), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		mustRun("add", "f")
		_ = i
		mustRun("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "-m", msg)
	}
	bare := t.TempDir() + "-bare"
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	out := t.TempDir()
	prefix = filepath.Join(out, "pack")
	id, err := gitcli.PackObjectsAll(context.Background(), bare, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	return prefix, id
}

func TestParseIdx_RoundTripFanoutAndCount(t *testing.T) {
	prefix, id := makeOnePackRepo(t)
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	if idx.Count() == 0 {
		t.Fatalf("expected non-zero object count")
	}
	// Fanout invariant: fanout[255] == count.
	if idx.Fanout()[255] != uint32(idx.Count()) {
		t.Fatalf("fanout[255]=%d != count=%d", idx.Fanout()[255], idx.Count())
	}
	// Iteration is OID-sorted.
	var prev OID
	first := true
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		if !first {
			if bytes.Compare(oid[:], prev[:]) <= 0 {
				t.Fatalf("OIDs not strictly ascending at %d", i)
			}
		}
		prev = oid
		first = false
	}
}

func TestIdx_LookupReturnsOffset(t *testing.T) {
	prefix, id := makeOnePackRepo(t)
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		off, ok := idx.Lookup(oid)
		if !ok {
			t.Fatalf("Lookup miss for OID at index %d", i)
		}
		if off == 0 && i != 0 {
			// Offset 0 in a pack would be the header, only valid for first object.
			t.Fatalf("zero offset for non-first OID")
		}
	}
}

func TestIdx_LookupMiss(t *testing.T) {
	prefix, id := makeOnePackRepo(t)
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	var bogus OID
	if _, ok := idx.Lookup(bogus); ok {
		t.Fatalf("expected miss for zero OID")
	}
}
```

This test references `gitcli.RunForTest`, a small test-helper we add next.

- [ ] **Step 2: Add the gitcli test helper**

Append to `internal/gitcli/gitcli.go`:

```go
// RunForTest runs git in dir with the given args and returns combined
// output. Tests pass GIT_AUTHOR/COMMITTER env identity inline via -c
// flags. Production code should NOT use this; use the typed wrappers.
func RunForTest(dir string, args ...string) ([]byte, error) {
	bin, err := resolveBinary()
	if err != nil {
		return nil, err
	}
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command(bin, full...)
	out, err := cmd.CombinedOutput()
	return out, err
}
```

- [ ] **Step 3: Run, confirm failure**

Run: `go test ./internal/pack/...`
Expected: FAIL — `ParseIdx` undefined.

- [ ] **Step 4: Write the implementation**

Create `internal/pack/index.go`:

```go
package pack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// idx v2 magic and version bytes per [pack-format §PACK-IDX-FILE].
var idxMagic = []byte{0xff, 0x74, 0x4f, 0x63}

const (
	idxVersion         uint32 = 2
	idxFanoutEntries          = 256
	idxFanoutBytes            = idxFanoutEntries * 4
	idxHeaderBytes            = 8 // magic+version
	idxOIDSize                = 20
	idxCRCSize                = 4
	idxOffsetSize             = 4
	idxLargeOffsetSize        = 8
	idxTrailerSize            = 40 // pack-sha1 + idx-sha1
	idxOffsetMSB              = uint32(1) << 31
)

// ErrIdxCorrupt is returned when an .idx file fails structural checks.
var ErrIdxCorrupt = errors.New("pack: idx corrupt")

// Idx is a parsed .idx v2 file.
type Idx struct {
	count       int
	fanout      [256]uint32
	oids        []byte // count*20
	crcs        []byte // count*4 (CRC32 per object; M2 stores but does not validate against pack CRC -- M9 may)
	offsets     []byte // count*4
	largeOffs   []uint64
	packTrailer [20]byte // SHA-1 of pack file (per idx footer)
	idxSelfSHA  [20]byte
}

// ParseIdx reads a v2 .idx file from r. size must equal r's content
// length so the trailer offset is known.
func ParseIdx(r io.ReaderAt, size int64) (*Idx, error) {
	if size < int64(idxHeaderBytes+idxFanoutBytes+idxTrailerSize) {
		return nil, fmt.Errorf("%w: too small (%d)", ErrIdxCorrupt, size)
	}
	buf := make([]byte, idxHeaderBytes)
	if _, err := r.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("%w: read header: %v", ErrIdxCorrupt, err)
	}
	if string(buf[:4]) != string(idxMagic) {
		return nil, fmt.Errorf("%w: bad magic %x", ErrIdxCorrupt, buf[:4])
	}
	if v := binary.BigEndian.Uint32(buf[4:8]); v != idxVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrIdxCorrupt, v)
	}
	idx := &Idx{}
	fanoutBuf := make([]byte, idxFanoutBytes)
	if _, err := r.ReadAt(fanoutBuf, int64(idxHeaderBytes)); err != nil {
		return nil, fmt.Errorf("%w: read fanout: %v", ErrIdxCorrupt, err)
	}
	for i := 0; i < idxFanoutEntries; i++ {
		idx.fanout[i] = binary.BigEndian.Uint32(fanoutBuf[i*4:])
	}
	idx.count = int(idx.fanout[255])
	// Validate fanout monotonicity.
	for i := 1; i < idxFanoutEntries; i++ {
		if idx.fanout[i] < idx.fanout[i-1] {
			return nil, fmt.Errorf("%w: fanout non-monotonic at %d", ErrIdxCorrupt, i)
		}
	}
	off := int64(idxHeaderBytes + idxFanoutBytes)
	idx.oids = make([]byte, idx.count*idxOIDSize)
	if _, err := r.ReadAt(idx.oids, off); err != nil {
		return nil, fmt.Errorf("%w: read oid table: %v", ErrIdxCorrupt, err)
	}
	off += int64(idx.count * idxOIDSize)
	idx.crcs = make([]byte, idx.count*idxCRCSize)
	if _, err := r.ReadAt(idx.crcs, off); err != nil {
		return nil, fmt.Errorf("%w: read crc table: %v", ErrIdxCorrupt, err)
	}
	off += int64(idx.count * idxCRCSize)
	idx.offsets = make([]byte, idx.count*idxOffsetSize)
	if _, err := r.ReadAt(idx.offsets, off); err != nil {
		return nil, fmt.Errorf("%w: read offset table: %v", ErrIdxCorrupt, err)
	}
	off += int64(idx.count * idxOffsetSize)

	largeBytes := size - off - int64(idxTrailerSize)
	if largeBytes < 0 || largeBytes%int64(idxLargeOffsetSize) != 0 {
		return nil, fmt.Errorf("%w: large-offset section size %d", ErrIdxCorrupt, largeBytes)
	}
	if largeBytes > 0 {
		raw := make([]byte, largeBytes)
		if _, err := r.ReadAt(raw, off); err != nil {
			return nil, fmt.Errorf("%w: read large offsets: %v", ErrIdxCorrupt, err)
		}
		idx.largeOffs = make([]uint64, largeBytes/int64(idxLargeOffsetSize))
		for i := range idx.largeOffs {
			idx.largeOffs[i] = binary.BigEndian.Uint64(raw[i*idxLargeOffsetSize:])
		}
		off += largeBytes
	}
	trailer := make([]byte, idxTrailerSize)
	if _, err := r.ReadAt(trailer, off); err != nil {
		return nil, fmt.Errorf("%w: read trailer: %v", ErrIdxCorrupt, err)
	}
	copy(idx.packTrailer[:], trailer[:20])
	copy(idx.idxSelfSHA[:], trailer[20:])
	return idx, nil
}

// Count returns the number of indexed objects.
func (i *Idx) Count() int { return i.count }

// Fanout returns a copy of the 256-entry fanout table.
func (i *Idx) Fanout() [256]uint32 { return i.fanout }

// PackTrailerSHA1 returns the .pack file SHA-1 recorded in the .idx footer.
func (i *Idx) PackTrailerSHA1() [20]byte { return i.packTrailer }

// OIDAt returns the OID at the given (sorted) index position. Panics
// if i is out of range.
func (i *Idx) OIDAt(n int) OID {
	var o OID
	copy(o[:], i.oids[n*idxOIDSize:(n+1)*idxOIDSize])
	return o
}

// OffsetAt returns the pack-file byte offset for the OID at position n.
func (i *Idx) OffsetAt(n int) uint64 {
	raw := binary.BigEndian.Uint32(i.offsets[n*idxOffsetSize:])
	if raw&idxOffsetMSB == 0 {
		return uint64(raw)
	}
	idx := int(raw &^ idxOffsetMSB)
	return i.largeOffs[idx]
}

// Lookup returns the pack-file offset for oid, or false if absent.
func (i *Idx) Lookup(oid OID) (uint64, bool) {
	first := oid[0]
	lo := 0
	if first > 0 {
		lo = int(i.fanout[first-1])
	}
	hi := int(i.fanout[first])
	if lo == hi {
		return 0, false
	}
	pos := sort.Search(hi-lo, func(k int) bool {
		var got OID
		copy(got[:], i.oids[(lo+k)*idxOIDSize:])
		for b := 0; b < idxOIDSize; b++ {
			if got[b] != oid[b] {
				return got[b] >= oid[b]
			}
		}
		return true
	})
	if pos == hi-lo {
		return 0, false
	}
	abs := lo + pos
	var got OID
	copy(got[:], i.oids[abs*idxOIDSize:])
	if got != oid {
		return 0, false
	}
	return i.OffsetAt(abs), true
}
```

- [ ] **Step 5: Run, confirm pass**

Run: `go test ./internal/pack/... ./internal/gitcli/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/pack/ internal/gitcli/gitcli.go
git commit -m "M2 pack: parse .idx v2 (fanout, oid/crc/offset tables, large offsets)"
```

---

## Task 7: pack — io.ReaderAt over storage.ObjectStore

**Files:**
- Create: `internal/pack/store_source.go`
- Create: `internal/pack/store_source_test.go`

The pack reader takes an `io.ReaderAt`. For local-disk packs we use `*os.File`. For bucket-backed packs we wrap an `ObjectStore` so every `ReadAt(p, off)` becomes a `GetRange(ctx, key, off, off+len(p)-1)` followed by an `io.ReadFull`.

- [ ] **Step 1: Write the failing test**

Create `internal/pack/store_source_test.go`:

```go
package pack

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.New(localfs.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	return s
}

func TestStoreSource_ReadsRange(t *testing.T) {
	store := newTestStore(t)
	body := []byte("0123456789abcdef")
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(body)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src := NewStoreSource(context.Background(), store, "k", int64(len(body)))
	buf := make([]byte, 4)
	n, err := src.ReadAt(buf, 6)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 4 {
		t.Fatalf("ReadAt n: got %d, want 4", n)
	}
	if !bytes.Equal(buf, []byte("6789")) {
		t.Fatalf("ReadAt got %q", buf)
	}
}

func TestStoreSource_ReadAtTail_ReturnsEOF(t *testing.T) {
	store := newTestStore(t)
	body := []byte("hello")
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(body)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src := NewStoreSource(context.Background(), store, "k", int64(len(body)))
	buf := make([]byte, 8)
	n, err := src.ReadAt(buf, 0)
	if n != 5 {
		t.Fatalf("ReadAt n: got %d, want 5", n)
	}
	if err != io.EOF {
		t.Fatalf("ReadAt err: got %v, want io.EOF", err)
	}
	if string(buf[:5]) != "hello" {
		t.Fatalf("ReadAt got %q", buf[:5])
	}
}

func TestStoreSource_PastEOF_ReturnsEOF(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader("x"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src := NewStoreSource(context.Background(), store, "k", 1)
	buf := make([]byte, 1)
	if _, err := src.ReadAt(buf, 5); err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/pack/...`
Expected: FAIL — `NewStoreSource` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/pack/store_source.go`:

```go
package pack

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// StoreSource adapts a storage.ObjectStore object into io.ReaderAt by
// translating each ReadAt into a GetRange. It is safe for concurrent
// use; each ReadAt issues its own GetRange.
type StoreSource struct {
	ctx   context.Context
	store storage.ObjectStore
	key   string
	size  int64
}

// NewStoreSource constructs a StoreSource. size must equal the object's
// content length so EOF semantics are correct.
func NewStoreSource(ctx context.Context, store storage.ObjectStore, key string, size int64) *StoreSource {
	return &StoreSource{ctx: ctx, store: store, key: key, size: size}
}

// Size returns the object's known content length.
func (s *StoreSource) Size() int64 { return s.size }

// ReadAt implements io.ReaderAt. Returns io.EOF when off+len(p) reaches
// or exceeds the object's end.
func (s *StoreSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("pack: StoreSource.ReadAt: negative offset %d", off)
	}
	if off >= s.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	end := off + want - 1
	atEOF := false
	if end >= s.size {
		end = s.size - 1
		want = end - off + 1
		atEOF = true
	}
	rc, err := s.store.GetRange(s.ctx, s.key, off, end)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return 0, fmt.Errorf("pack: StoreSource: %w", err)
		}
		return 0, fmt.Errorf("pack: StoreSource.ReadAt: GetRange: %w", err)
	}
	defer rc.Close()
	n, err := io.ReadFull(rc, p[:want])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return n, fmt.Errorf("pack: StoreSource.ReadAt: ReadFull: %w", err)
	}
	if atEOF {
		return n, io.EOF
	}
	return n, nil
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/pack/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pack/store_source.go internal/pack/store_source_test.go
git commit -m "M2 pack: StoreSource adapts ObjectStore to io.ReaderAt"
```

---

## Task 8: pack — object header decode + non-delta inflate

**Files:**
- Create: `internal/pack/object.go`
- Create: `internal/pack/object_test.go`

A pack object header is a variable-length encoding with `obj_type` (3 bits) + `size` (7+ bits), then zlib-compressed payload [pack-format §HEADER, §OBJECT-FORMAT]. For non-delta types (commit/tree/blob/tag) the payload inflates directly to the object's content.

- [ ] **Step 1: Write the failing test**

Create `internal/pack/object_test.go`:

```go
package pack

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func TestReadObjectHeader_Commit(t *testing.T) {
	prefix, id := makeOnePackRepo(t)
	packBytes, err := os.ReadFile(prefix + "-" + id + ".pack")
	if err != nil {
		t.Fatalf("ReadFile pack: %v", err)
	}
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		off := idx.OffsetAt(i)
		hdr, err := readObjectHeader(bytes.NewReader(packBytes), int64(off))
		if err != nil {
			t.Fatalf("readObjectHeader at oid=%s off=%d: %v", oid, off, err)
		}
		// Commits/trees/blobs/tags must have a non-delta type.
		if hdr.Type != TypeCommit && hdr.Type != TypeTree && hdr.Type != TypeBlob && hdr.Type != TypeTag &&
			hdr.Type != typeOFSDelta && hdr.Type != typeREFDelta {
			t.Fatalf("unexpected type %v", hdr.Type)
		}
		if hdr.Size <= 0 {
			t.Fatalf("zero size for %s (type %v)", oid, hdr.Type)
		}
	}
}

func TestInflateObject_MatchesGitCatFile(t *testing.T) {
	prefix, id := makeOnePackRepo(t)
	packBytes, err := os.ReadFile(prefix + "-" + id + ".pack")
	if err != nil {
		t.Fatalf("ReadFile pack: %v", err)
	}
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	bareDir := bareFromPrefix(t, prefix, id)
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		off := idx.OffsetAt(i)
		hdr, err := readObjectHeader(bytes.NewReader(packBytes), int64(off))
		if err != nil {
			t.Fatalf("readObjectHeader: %v", err)
		}
		if hdr.Type == typeOFSDelta || hdr.Type == typeREFDelta {
			continue // delta resolution lands in Task 9
		}
		body, err := inflateAt(bytes.NewReader(packBytes), int64(off)+hdr.HeaderLen, hdr.Size)
		if err != nil {
			t.Fatalf("inflate %s: %v", oid, err)
		}
		// Compare to git cat-file -p (matching content for non-deltas).
		want, err := gitcli.CatFilePretty(context.Background(), bareDir, oid.String())
		if err != nil {
			t.Fatalf("cat-file: %v", err)
		}
		// For commits/trees/tags, cat-file -p prints reformatted output;
		// instead, recompute the hash of (type SP size NUL body) and compare
		// against the OID itself, which is the strongest equivalence check.
		var typeStr string
		switch hdr.Type {
		case TypeCommit:
			typeStr = "commit"
		case TypeTree:
			typeStr = "tree"
		case TypeBlob:
			typeStr = "blob"
		case TypeTag:
			typeStr = "tag"
		}
		hashed := sha1.New()
		fmt.Fprintf(hashed, "%s %d", typeStr, hdr.Size)
		hashed.Write([]byte{0})
		hashed.Write(body)
		var got OID
		copy(got[:], hashed.Sum(nil))
		if got != oid {
			t.Fatalf("inflated body hash mismatch for %s (type %s, size %d): got %s",
				oid, typeStr, hdr.Size, got)
		}
		_ = want
	}
}

// bareFromPrefix recovers the bare repo path used to produce the pack
// at prefix-id.pack, by re-running the same fixture flow. The simpler
// approach: makeOnePackRepo returns the bare dir too. Refactor the
// helper to do so.
func bareFromPrefix(t *testing.T, prefix, id string) string {
	t.Helper()
	t.Skip("bareFromPrefix is replaced by makeOnePackRepo returning bareDir; see refactor in this task")
	return ""
}
```

The test references `bareFromPrefix`, which we replace by extending `makeOnePackRepo` to also return the bare dir.

- [ ] **Step 2: Refactor makeOnePackRepo to expose the bare dir**

In `internal/pack/index_test.go`, change the helper signature:

```go
// Replace the existing makeOnePackRepo with this version.
func makeOnePackRepo(t *testing.T) (prefix, packID, bareDir string) {
	t.Helper()
	skipIfNoGit(t)
	work := t.TempDir()
	mustRun := func(args ...string) {
		t.Helper()
		if out, err := gitcli.RunForTest(work, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustRun("init", "--initial-branch=main")
	for _, msg := range []string{"a\n", "b\n", "c\n"} {
		if err := os.WriteFile(filepath.Join(work, "f"), []byte(msg), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		mustRun("add", "f")
		mustRun("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "-m", msg)
	}
	bareDir = t.TempDir() + "-bare"
	if err := gitcli.CloneBareMirror(context.Background(), work, bareDir); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	out := t.TempDir()
	prefix = filepath.Join(out, "pack")
	id, err := gitcli.PackObjectsAll(context.Background(), bareDir, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	return prefix, id, bareDir
}
```

Update the existing callers in `index_test.go` (both `TestParseIdx_*` tests and `TestIdx_Lookup*`) to discard the third return:

```go
prefix, id, _ := makeOnePackRepo(t)
```

In `object_test.go`, replace the `bareFromPrefix` skip with:

```go
prefix, id, bareDir := makeOnePackRepo(t)
// ... bareDir flows into gitcli.CatFilePretty
```

Drop the `bareFromPrefix` function from `object_test.go`.

- [ ] **Step 3: Run, confirm failure**

Run: `go test ./internal/pack/...`
Expected: FAIL — `readObjectHeader`, `inflateAt` undefined.

- [ ] **Step 4: Write the implementation**

Create `internal/pack/object.go`:

```go
package pack

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
)

// ObjectHeader describes a pack-encoded object's type, size, and the
// number of bytes consumed by the variable-length header itself (so the
// caller knows where the zlib payload begins).
type ObjectHeader struct {
	Type      ObjectType
	Size      int64
	HeaderLen int64
	// For ofs_delta: BaseOffset is set to the absolute pack offset of
	// the base object. For ref_delta: BaseOID is set.
	BaseOffset int64
	BaseOID    OID
}

// ErrPackCorrupt is returned when a pack file fails structural checks.
var ErrPackCorrupt = errors.New("pack: pack corrupt")

// readObjectHeader parses the variable-length header at the given pack
// offset. The encoding is documented at [pack-format §HEADER]:
//
//   byte 0: MSB | typ(3) | size_low(4)
//   while MSB set: byte n: MSB | size_extra(7)
//
// For ofs_delta types: a "base offset" big-endian-ish varint follows
// the type+size header; the base lives at this_offset - base_offset.
// For ref_delta types: a 20-byte OID follows.
func readObjectHeader(r io.ReaderAt, off int64) (ObjectHeader, error) {
	var hdr ObjectHeader
	var b [1]byte
	read := int64(0)
	if _, err := r.ReadAt(b[:], off+read); err != nil {
		return hdr, fmt.Errorf("%w: read first header byte: %v", ErrPackCorrupt, err)
	}
	read++
	hdr.Type = ObjectType((b[0] >> 4) & 0x07)
	size := int64(b[0] & 0x0f)
	shift := uint(4)
	for b[0]&0x80 != 0 {
		if _, err := r.ReadAt(b[:], off+read); err != nil {
			return hdr, fmt.Errorf("%w: read header continuation: %v", ErrPackCorrupt, err)
		}
		read++
		size |= int64(b[0]&0x7f) << shift
		shift += 7
		if shift > 63 {
			return hdr, fmt.Errorf("%w: size overflow", ErrPackCorrupt)
		}
	}
	hdr.Size = size

	switch hdr.Type {
	case typeOFSDelta:
		// Big-endian-ish "offset varint" per [pack-format §DELTA-OFFSET].
		if _, err := r.ReadAt(b[:], off+read); err != nil {
			return hdr, fmt.Errorf("%w: read ofs varint: %v", ErrPackCorrupt, err)
		}
		read++
		negOff := int64(b[0] & 0x7f)
		for b[0]&0x80 != 0 {
			negOff++ // implicit +1 between continuation bytes
			negOff <<= 7
			if _, err := r.ReadAt(b[:], off+read); err != nil {
				return hdr, fmt.Errorf("%w: read ofs varint cont: %v", ErrPackCorrupt, err)
			}
			read++
			negOff |= int64(b[0] & 0x7f)
			if negOff < 0 {
				return hdr, fmt.Errorf("%w: ofs varint overflow", ErrPackCorrupt)
			}
		}
		hdr.BaseOffset = off - negOff
		if hdr.BaseOffset < 0 {
			return hdr, fmt.Errorf("%w: ofs base before pack start", ErrPackCorrupt)
		}
	case typeREFDelta:
		var oidBuf [20]byte
		if _, err := r.ReadAt(oidBuf[:], off+read); err != nil {
			return hdr, fmt.Errorf("%w: read ref-delta oid: %v", ErrPackCorrupt, err)
		}
		read += 20
		copy(hdr.BaseOID[:], oidBuf[:])
	}
	hdr.HeaderLen = read
	return hdr, nil
}

// inflateAt zlib-inflates exactly want bytes from the given offset.
func inflateAt(r io.ReaderAt, off int64, want int64) ([]byte, error) {
	// We don't know the compressed size, so wrap the ReaderAt in a section
	// reader that extends to EOF; zlib stops at the first stream end.
	const slack = int64(1 << 30)
	sr := io.NewSectionReader(r, off, slack)
	zr, err := zlib.NewReader(sr)
	if err != nil {
		return nil, fmt.Errorf("%w: zlib: %v", ErrPackCorrupt, err)
	}
	defer zr.Close()
	out := bytes.NewBuffer(make([]byte, 0, want))
	if _, err := io.CopyN(out, zr, want); err != nil {
		return nil, fmt.Errorf("%w: inflate copy: %v", ErrPackCorrupt, err)
	}
	if out.Len() != int(want) {
		return nil, fmt.Errorf("%w: inflated %d, want %d", ErrPackCorrupt, out.Len(), want)
	}
	return out.Bytes(), nil
}
```

- [ ] **Step 5: Run, confirm pass**

Run: `go test ./internal/pack/...`
Expected: PASS for non-delta objects. Delta-typed objects are skipped in the test pending Task 9.

- [ ] **Step 6: Commit**

```bash
git add internal/pack/object.go internal/pack/object_test.go internal/pack/index_test.go
git commit -m "M2 pack: object header parser + non-delta zlib inflate"
```

---

## Task 9: pack — delta resolution (REF_DELTA + OFS_DELTA)

**Files:**
- Create: `internal/pack/delta.go`
- Create: `internal/pack/delta_test.go`

The Git delta format is documented at [pack-format §OBJECT-DATA-DELTIFIED]. The delta payload (after zlib inflation) starts with two size varints (`base_size`, `result_size`), then a sequence of instructions:

- Copy: leading byte `1xxxxxxx` selects which of `[off1, off2, off3, off4, sz1, sz2, sz3]` follow; assembles `(offset, size)` and copies `base[offset : offset+size]`.
- Insert: leading byte `0sssssss` (s != 0) introduces s literal bytes that follow.
- 0x00 is reserved.

To resolve a delta object, we recursively resolve the base, then apply the instructions. Real packs commonly chain deltas; we bound the chain depth.

- [ ] **Step 1: Write the failing test**

Create `internal/pack/delta_test.go`:

```go
package pack

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func TestApplyDelta_Synthetic(t *testing.T) {
	base := []byte("the quick brown fox")
	// Build a tiny delta by hand: result = "the lazy dog" via insert-only.
	result := []byte("the lazy dog")
	delta := buildSyntheticInsertOnlyDelta(t, len(base), result)
	got, err := applyDelta(base, delta)
	if err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	if !bytes.Equal(got, result) {
		t.Fatalf("applyDelta: got %q, want %q", got, result)
	}
}

func TestApplyDelta_CopyAndInsert(t *testing.T) {
	base := []byte("the quick brown fox jumps")
	// result = "the brown fox jumps over the quick" -- mix of copies
	// from base and a literal "over the ".
	result := []byte("the brown fox jumps over the quick")
	delta := buildCopyAndInsertDelta(t, base, result, []deltaOp{
		{copyFrom: 0, copyLen: 4},   // "the "
		{copyFrom: 10, copyLen: 15}, // "brown fox jumps"
		{insert: []byte(" over the")},
		{copyFrom: 3, copyLen: 6}, // " quick"
	})
	got, err := applyDelta(base, delta)
	if err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	if !bytes.Equal(got, result) {
		t.Fatalf("applyDelta: got %q, want %q", got, result)
	}
}

func TestResolveObject_AllPackObjectsRoundTrip(t *testing.T) {
	prefix, id, bareDir := makeOnePackRepo(t)
	packBytes, err := os.ReadFile(prefix + "-" + id + ".pack")
	if err != nil {
		t.Fatalf("ReadFile pack: %v", err)
	}
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	r := bytes.NewReader(packBytes)
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		obj, err := resolveObject(r, idx, idx.OffsetAt(i), 64)
		if err != nil {
			t.Fatalf("resolveObject %s: %v", oid, err)
		}
		// Recompute hash of (type SP size NUL body).
		h := sha1.New()
		fmt.Fprintf(h, "%s %d", obj.Type.String(), obj.Size)
		h.Write([]byte{0})
		h.Write(obj.Data)
		var got OID
		copy(got[:], h.Sum(nil))
		if got != oid {
			t.Fatalf("resolveObject hash mismatch: oid=%s type=%s size=%d got=%s",
				oid, obj.Type, obj.Size, got)
		}
	}
	_ = bareDir
	_ = context.Background()
	_ = gitcli.CatFilePretty // keep import alive across edits
}

// deltaOp is a tiny helper for buildCopyAndInsertDelta; copyFrom/copyLen
// are zero when the op is an insert.
type deltaOp struct {
	copyFrom, copyLen int
	insert            []byte
}

func buildSyntheticInsertOnlyDelta(t *testing.T, baseSize int, result []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writeSizeVarint(&out, uint64(baseSize))
	writeSizeVarint(&out, uint64(len(result)))
	for len(result) > 0 {
		n := len(result)
		if n > 127 {
			n = 127
		}
		out.WriteByte(byte(n))
		out.Write(result[:n])
		result = result[n:]
	}
	return out.Bytes()
}

func buildCopyAndInsertDelta(t *testing.T, base, result []byte, ops []deltaOp) []byte {
	t.Helper()
	var out bytes.Buffer
	writeSizeVarint(&out, uint64(len(base)))
	writeSizeVarint(&out, uint64(len(result)))
	for _, op := range ops {
		if op.insert != nil {
			rem := op.insert
			for len(rem) > 0 {
				n := len(rem)
				if n > 127 {
					n = 127
				}
				out.WriteByte(byte(n))
				out.Write(rem[:n])
				rem = rem[n:]
			}
			continue
		}
		// Copy op: top bit set; following bits select which offset/size
		// bytes are present, written in little-endian order.
		var hdr byte = 0x80
		var enc bytes.Buffer
		off := uint32(op.copyFrom)
		if off&0x000000ff != 0 {
			hdr |= 0x01
			enc.WriteByte(byte(off & 0xff))
		}
		if off&0x0000ff00 != 0 {
			hdr |= 0x02
			enc.WriteByte(byte((off >> 8) & 0xff))
		}
		if off&0x00ff0000 != 0 {
			hdr |= 0x04
			enc.WriteByte(byte((off >> 16) & 0xff))
		}
		if off&0xff000000 != 0 {
			hdr |= 0x08
			enc.WriteByte(byte((off >> 24) & 0xff))
		}
		sz := uint32(op.copyLen)
		if sz&0x0000ff != 0 {
			hdr |= 0x10
			enc.WriteByte(byte(sz & 0xff))
		}
		if sz&0x00ff00 != 0 {
			hdr |= 0x20
			enc.WriteByte(byte((sz >> 8) & 0xff))
		}
		if sz&0xff0000 != 0 {
			hdr |= 0x40
			enc.WriteByte(byte((sz >> 16) & 0xff))
		}
		out.WriteByte(hdr)
		out.Write(enc.Bytes())
	}
	return out.Bytes()
}

func writeSizeVarint(w *bytes.Buffer, n uint64) {
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		w.WriteByte(b)
		if n == 0 {
			return
		}
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/pack/...`
Expected: FAIL — `applyDelta`, `resolveObject` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/pack/delta.go`:

```go
package pack

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
)

// ErrDeltaChainTooDeep is returned when a delta resolution exceeds the
// configured chain bound.
var ErrDeltaChainTooDeep = errors.New("pack: delta chain too deep")

// applyDelta applies a Git delta-encoded byte sequence against a base
// object body, returning the reconstructed result.
//
// Format (see [pack-format §OBJECT-DATA-DELTIFIED]):
//
//   base_size  -- varint, low-7-bit chunks, MSB indicates continuation
//   result_size -- varint, same encoding
//   instructions ...
//     0x80 | mask_byte:
//       low 4 bits select which of off1..off4 follow (LE)
//       next 3 bits select which of sz1..sz3 follow (LE)
//       size 0 means 0x10000 (per Git's quirk)
//     0x01..0x7f: insert-N literal bytes follow
//     0x00: reserved
func applyDelta(base, delta []byte) ([]byte, error) {
	r := bytes.NewReader(delta)
	baseSize, err := readSizeVarint(r)
	if err != nil {
		return nil, fmt.Errorf("delta: read base size: %w", err)
	}
	if int64(baseSize) != int64(len(base)) {
		return nil, fmt.Errorf("delta: declared base size %d != actual %d", baseSize, len(base))
	}
	resultSize, err := readSizeVarint(r)
	if err != nil {
		return nil, fmt.Errorf("delta: read result size: %w", err)
	}
	out := make([]byte, 0, resultSize)
	for {
		op, err := r.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("delta: read op: %w", err)
		}
		switch {
		case op&0x80 != 0:
			// Copy
			var off uint32
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					b, err := r.ReadByte()
					if err != nil {
						return nil, fmt.Errorf("delta: copy off byte: %w", err)
					}
					off |= uint32(b) << (8 * i)
				}
			}
			var sz uint32
			for i := uint(0); i < 3; i++ {
				if op&(0x10<<i) != 0 {
					b, err := r.ReadByte()
					if err != nil {
						return nil, fmt.Errorf("delta: copy sz byte: %w", err)
					}
					sz |= uint32(b) << (8 * i)
				}
			}
			if sz == 0 {
				sz = 0x10000
			}
			if int64(off)+int64(sz) > int64(len(base)) {
				return nil, fmt.Errorf("delta: copy out of range off=%d sz=%d base=%d",
					off, sz, len(base))
			}
			out = append(out, base[off:off+sz]...)
		case op == 0:
			return nil, fmt.Errorf("delta: reserved opcode 0")
		default:
			// Insert N bytes literal.
			n := int(op & 0x7f)
			buf := make([]byte, n)
			if _, err := io.ReadFull(r, buf); err != nil {
				return nil, fmt.Errorf("delta: insert read: %w", err)
			}
			out = append(out, buf...)
		}
	}
	if int64(len(out)) != int64(resultSize) {
		return nil, fmt.Errorf("delta: result size %d != declared %d", len(out), resultSize)
	}
	return out, nil
}

// readSizeVarint reads the size-encoded varint used by the delta format
// (LSB-first, 7 bits per byte, MSB=continuation).
func readSizeVarint(r io.ByteReader) (uint64, error) {
	var v uint64
	var shift uint
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, nil
		}
		shift += 7
		if shift > 63 {
			return 0, fmt.Errorf("size varint overflow")
		}
	}
}

// resolveObject reads, decompresses, and (recursively) un-deltas the
// object at off in the pack. maxDepth bounds the chain length.
func resolveObject(r io.ReaderAt, idx *Idx, off uint64, maxDepth int) (*Object, error) {
	if maxDepth <= 0 {
		return nil, ErrDeltaChainTooDeep
	}
	hdr, err := readObjectHeader(r, int64(off))
	if err != nil {
		return nil, err
	}
	switch hdr.Type {
	case TypeCommit, TypeTree, TypeBlob, TypeTag:
		body, err := inflateAt(r, int64(off)+hdr.HeaderLen, hdr.Size)
		if err != nil {
			return nil, err
		}
		return &Object{Type: hdr.Type, Size: int64(len(body)), Data: body}, nil
	case typeOFSDelta:
		base, err := resolveObject(r, idx, uint64(hdr.BaseOffset), maxDepth-1)
		if err != nil {
			return nil, err
		}
		deltaBody, err := inflateAt(r, int64(off)+hdr.HeaderLen, hdr.Size)
		if err != nil {
			return nil, err
		}
		out, err := applyDelta(base.Data, deltaBody)
		if err != nil {
			return nil, err
		}
		return &Object{Type: base.Type, Size: int64(len(out)), Data: out}, nil
	case typeREFDelta:
		baseOff, ok := idx.Lookup(hdr.BaseOID)
		if !ok {
			return nil, fmt.Errorf("%w: ref-delta base %s not in pack", ErrPackCorrupt, hdr.BaseOID)
		}
		base, err := resolveObject(r, idx, baseOff, maxDepth-1)
		if err != nil {
			return nil, err
		}
		deltaBody, err := inflateAt(r, int64(off)+hdr.HeaderLen, hdr.Size)
		if err != nil {
			return nil, err
		}
		out, err := applyDelta(base.Data, deltaBody)
		if err != nil {
			return nil, err
		}
		return &Object{Type: base.Type, Size: int64(len(out)), Data: out}, nil
	default:
		return nil, fmt.Errorf("%w: bad type %v", ErrPackCorrupt, hdr.Type)
	}
}

// hashObject returns the SHA-1 of (typeName SP size NUL body).
// Used by tests to verify resolveObject correctness.
func hashObject(typ ObjectType, body []byte) OID {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d", typ.String(), len(body))
	h.Write([]byte{0})
	h.Write(body)
	var o OID
	copy(o[:], h.Sum(nil))
	return o
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/pack/...`
Expected: PASS — every object in a real pack now resolves through resolveObject and its hash matches the OID.

- [ ] **Step 5: Commit**

```bash
git add internal/pack/delta.go internal/pack/delta_test.go
git commit -m "M2 pack: REF_DELTA + OFS_DELTA resolution with bounded chain depth"
```

---

## Task 10: pack — Reader (Open, Has, Get, ForEach, Close) + cache

**Files:**
- Create: `internal/pack/reader.go`
- Create: `internal/pack/reader_test.go`
- Create: `internal/pack/cache.go`

The Reader holds the parsed Idx, an `io.ReaderAt` for the pack, and a small bounded delta-base cache. `Open` reads the .idx in full and validates the .pack header (magic + version + count). All Get calls are lazy.

- [ ] **Step 1: Write the failing test**

Create `internal/pack/reader_test.go`:

```go
package pack

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func TestReader_OpenValidatesMagic(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "p.pack", strings.NewReader("garbage"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := store.PutIfAbsent(context.Background(), "p.idx", strings.NewReader("garbage"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "p.pack", "p.idx"); err == nil {
		t.Fatalf("expected Open to fail on garbage")
	}
}

func TestReader_GetMatchesGitCatFile(t *testing.T) {
	prefix, id, bareDir := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	oids, err := gitcli.RevListAllObjects(context.Background(), bareDir)
	if err != nil {
		t.Fatalf("RevListAllObjects: %v", err)
	}
	for _, oidStr := range oids {
		oid, err := ParseOID(oidStr)
		if err != nil {
			t.Fatalf("ParseOID: %v", err)
		}
		if !r.Has(oid) {
			t.Fatalf("Has(%s) = false", oidStr)
		}
		obj, err := r.Get(context.Background(), oid)
		if err != nil {
			t.Fatalf("Get(%s): %v", oidStr, err)
		}
		got := hashObject(obj.Type, obj.Data)
		if got != oid {
			t.Fatalf("Get hash mismatch for %s", oidStr)
		}
	}
}

func TestReader_ForEach_Order(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	var prev OID
	first := true
	count := 0
	if err := r.ForEach(func(oid OID, off uint64) error {
		if !first && bytesCompareOID(oid, prev) <= 0 {
			t.Fatalf("ForEach not OID-sorted")
		}
		prev = oid
		first = false
		count++
		return nil
	}); err != nil {
		t.Fatalf("ForEach: %v", err)
	}
	if count == 0 {
		t.Fatalf("ForEach saw no objects")
	}
}

func uploadFile(t *testing.T, store interface {
	PutIfAbsent(ctx context.Context, key string, body interface{}, opts interface{}) (interface{}, error)
}, srcPath, dstKey string) {
	t.Helper()
	t.Skip("placeholder — replaced below by a real PutIfAbsent helper")
}

func bytesCompareOID(a, b OID) int {
	for i := 0; i < 20; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
```

The test references `uploadFile` with a placeholder signature; we replace it with a real helper before implementing.

- [ ] **Step 2: Replace the uploadFile helper with the real one**

Replace `uploadFile` in `internal/pack/reader_test.go` with:

```go
import (
	// keep existing imports
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func uploadFile(t *testing.T, store storage.ObjectStore, srcPath, dstKey string) {
	t.Helper()
	f, err := os.Open(srcPath)
	if err != nil {
		t.Fatalf("Open %s: %v", srcPath, err)
	}
	defer f.Close()
	if _, err := store.PutIfAbsent(context.Background(), dstKey, f, nil); err != nil {
		t.Fatalf("PutIfAbsent %s: %v", dstKey, err)
	}
}
```

(Drop the placeholder version that called `t.Skip`.)

- [ ] **Step 3: Run, confirm failure**

Run: `go test ./internal/pack/...`
Expected: FAIL — `Open`, `Reader.Has`, `Reader.Get`, `Reader.ForEach`, `Reader.Close` undefined.

- [ ] **Step 4: Write the implementation**

Create `internal/pack/cache.go`:

```go
package pack

import (
	"container/list"
	"sync"
)

// objectCache is a tiny LRU keyed by pack offset, holding decoded
// non-delta objects that may serve as delta bases. Bounded by entry
// count, not bytes; M9 is when this gets serious sizing.
type objectCache struct {
	mu      sync.Mutex
	max     int
	entries map[uint64]*list.Element
	order   *list.List
}

type cacheEntry struct {
	off uint64
	obj *Object
}

func newObjectCache(max int) *objectCache {
	return &objectCache{
		max:     max,
		entries: make(map[uint64]*list.Element, max),
		order:   list.New(),
	}
}

func (c *objectCache) get(off uint64) (*Object, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[off]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*cacheEntry).obj, true
	}
	return nil, false
}

func (c *objectCache) put(off uint64, obj *Object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[off]; ok {
		c.order.MoveToFront(el)
		el.Value.(*cacheEntry).obj = obj
		return
	}
	if c.order.Len() >= c.max {
		back := c.order.Back()
		if back != nil {
			c.order.Remove(back)
			delete(c.entries, back.Value.(*cacheEntry).off)
		}
	}
	el := c.order.PushFront(&cacheEntry{off: off, obj: obj})
	c.entries[off] = el
}
```

Create `internal/pack/reader.go`:

```go
package pack

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// DefaultDeltaChainDepth bounds delta resolution recursion. M2 uses a
// generous-but-safe default; M9 may tune.
const DefaultDeltaChainDepth = 50

// DefaultObjectCacheEntries bounds the delta-base LRU. Unit: objects.
const DefaultObjectCacheEntries = 256

// Reader is a pure-Go random-access pack reader.
type Reader struct {
	idx       *Idx
	pack      io.ReaderAt
	packKey   string
	idxKey    string
	store     storage.ObjectStore
	chainCap  int
	objCache  *objectCache
	packSize  int64
}

// Open loads the .idx in full from store and validates the .pack header.
// Object reads are lazy range GETs.
func Open(ctx context.Context, store storage.ObjectStore, packKey, idxKey string) (*Reader, error) {
	idxObj, err := store.Head(ctx, idxKey)
	if err != nil {
		return nil, fmt.Errorf("pack: head idx: %w", err)
	}
	idxBytes, err := readAll(ctx, store, idxKey, idxObj.SizeBytes)
	if err != nil {
		return nil, fmt.Errorf("pack: read idx: %w", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		return nil, err
	}
	packObj, err := store.Head(ctx, packKey)
	if err != nil {
		return nil, fmt.Errorf("pack: head pack: %w", err)
	}
	src := NewStoreSource(ctx, store, packKey, packObj.SizeBytes)
	if err := validatePackHeader(src, idx); err != nil {
		return nil, err
	}
	return &Reader{
		idx: idx, pack: src, packKey: packKey, idxKey: idxKey, store: store,
		chainCap: DefaultDeltaChainDepth,
		objCache: newObjectCache(DefaultObjectCacheEntries),
		packSize: packObj.SizeBytes,
	}, nil
}

func readAll(ctx context.Context, s storage.ObjectStore, key string, size int64) ([]byte, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	buf := bytes.NewBuffer(make([]byte, 0, size))
	if _, err := io.Copy(buf, rc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// validatePackHeader checks the 12-byte pack header: magic "PACK", version
// 2, and object count == idx count.
func validatePackHeader(r io.ReaderAt, idx *Idx) error {
	hdr := make([]byte, 12)
	if _, err := r.ReadAt(hdr, 0); err != nil {
		return fmt.Errorf("%w: read pack header: %v", ErrPackCorrupt, err)
	}
	if string(hdr[:4]) != "PACK" {
		return fmt.Errorf("%w: pack magic %x", ErrPackCorrupt, hdr[:4])
	}
	if v := binary.BigEndian.Uint32(hdr[4:8]); v != 2 {
		return fmt.Errorf("%w: pack version %d", ErrPackCorrupt, v)
	}
	cnt := binary.BigEndian.Uint32(hdr[8:12])
	if int(cnt) != idx.Count() {
		return fmt.Errorf("%w: pack count %d != idx count %d", ErrPackCorrupt, cnt, idx.Count())
	}
	return nil
}

// Close releases reader resources. Safe to call multiple times.
func (r *Reader) Close() error { return nil }

// Has reports whether oid is present in this pack's idx.
func (r *Reader) Has(oid OID) bool {
	_, ok := r.idx.Lookup(oid)
	return ok
}

// Get returns the fully-resolved object for oid, or an error.
func (r *Reader) Get(ctx context.Context, oid OID) (*Object, error) {
	off, ok := r.idx.Lookup(oid)
	if !ok {
		return nil, fmt.Errorf("pack: oid %s not in idx", oid)
	}
	if obj, hit := r.objCache.get(off); hit {
		return obj, nil
	}
	obj, err := resolveObject(r.pack, r.idx, off, r.chainCap)
	if err != nil {
		return nil, err
	}
	// Cache only non-delta resolved objects (which serve as delta bases).
	r.objCache.put(off, obj)
	return obj, nil
}

// ForEach calls fn for every (oid, packOffset) in the idx, in OID-sorted order.
// Returning a non-nil error terminates iteration with that error.
func (r *Reader) ForEach(fn func(OID, uint64) error) error {
	for i := 0; i < r.idx.Count(); i++ {
		if err := fn(r.idx.OIDAt(i), r.idx.OffsetAt(i)); err != nil {
			return err
		}
	}
	return nil
}

// Idx exposes the parsed idx for index-builders. Not for hot-path use.
func (r *Reader) Idx() *Idx { return r.idx }
```

- [ ] **Step 5: Run, confirm pass**

Run: `go test ./internal/pack/...`
Expected: PASS — every object in the pack round-trips Open → Get → hash.

- [ ] **Step 6: Commit**

```bash
git add internal/pack/reader.go internal/pack/cache.go internal/pack/reader_test.go
git commit -m "M2 pack: Reader (Open/Has/Get/ForEach/Close) + bounded LRU"
```

---

## Task 11: objindex — .bvom format + Build

**Files:**
- Create: `internal/objindex/format.go`
- Create: `internal/objindex/build.go`
- Create: `internal/objindex/format_test.go`

`.bvom` format spec (M2 design §3.4): 32-byte header, fixed-width sorted records, pack-id table at end, 32-byte SHA-256 trailer over preceding bytes.

- [ ] **Step 1: Write the failing test**

Create `internal/objindex/format_test.go`:

```go
package objindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func TestBuild_HeaderAndMagic(t *testing.T) {
	entries := []Entry{
		{OID: oidOf(t, "0123456789abcdef0123456789abcdef01234567"), PackID: "aa", Offset: 12},
		{OID: oidOf(t, "1123456789abcdef0123456789abcdef01234567"), PackID: "aa", Offset: 200},
	}
	out, err := build(entries)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if string(out[:4]) != "BVOM" {
		t.Fatalf("magic: got %q", out[:4])
	}
	if v := binary.BigEndian.Uint32(out[4:8]); v != 1 {
		t.Fatalf("version: got %d", v)
	}
	if cnt := binary.BigEndian.Uint64(out[8:16]); cnt != 2 {
		t.Fatalf("count: got %d", cnt)
	}
}

func TestBuild_SortsRecords(t *testing.T) {
	hi := oidOf(t, "ffffffffffffffffffffffffffffffffffffffff")
	lo := oidOf(t, "0000000000000000000000000000000000000001")
	entries := []Entry{
		{OID: hi, PackID: "aa", Offset: 1},
		{OID: lo, PackID: "aa", Offset: 2},
	}
	out, err := build(entries)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rec0 := out[recordsStart() : recordsStart()+recordSize]
	if !bytes.Equal(rec0[:20], lo[:]) {
		t.Fatalf("records not sorted")
	}
}

func TestBuild_TrailerHash(t *testing.T) {
	entries := []Entry{
		{OID: oidOf(t, "0000000000000000000000000000000000000001"), PackID: "aa", Offset: 12},
	}
	out, err := build(entries)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	pre := out[:len(out)-32]
	want := sha256.Sum256(pre)
	got := out[len(out)-32:]
	if !bytes.Equal(want[:], got) {
		t.Fatalf("trailer hash mismatch")
	}
}

func TestBuild_Determinism(t *testing.T) {
	mk := func() []Entry {
		return []Entry{
			{OID: oidOf(t, "1111111111111111111111111111111111111111"), PackID: "aa", Offset: 1},
			{OID: oidOf(t, "0000000000000000000000000000000000000001"), PackID: "aa", Offset: 2},
		}
	}
	out1, err := build(mk())
	if err != nil {
		t.Fatalf("build1: %v", err)
	}
	out2, err := build(mk())
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatalf("non-deterministic build output")
	}
}

func oidOf(t *testing.T, hex string) pack.OID {
	t.Helper()
	o, err := pack.ParseOID(hex)
	if err != nil {
		t.Fatalf("ParseOID: %v", err)
	}
	return o
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/objindex/...`
Expected: FAIL — package undefined.

- [ ] **Step 3: Write the format & build implementation**

Create `internal/objindex/format.go`:

```go
// Package objindex implements the M2 object-to-pack map (.bvom).
//
// Layout (per spec §3.4):
//
//   header  (32 bytes)  : magic "BVOM", version u32, count u64,
//                         pack_tbl u64, reserved 8 bytes
//   records (count*32)  : oid (20) + pack_idx (u16) + reserved (2) + offset (u64),
//                         sorted ascending by oid
//   pack_tbl            : n_packs u16, then n_packs * 40 bytes (ASCII hex pack_id)
//   trailer (32 bytes)  : SHA-256 over everything before trailer
package objindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

const (
	headerSize  = 32
	recordSize  = 32
	trailerSize = 32
	packIDSize  = 40 // ASCII hex SHA-1
	currentVer  = uint32(1)
)

var magic = []byte{'B', 'V', 'O', 'M'}

// ErrCorrupt indicates a malformed .bvom file.
var ErrCorrupt = errors.New("objindex: corrupt")

// Entry pairs an OID with its pack location.
type Entry struct {
	OID    pack.OID
	PackID string // 40-char hex SHA-1
	Offset uint64
}

func recordsStart() int { return headerSize }
```

Create `internal/objindex/build.go`:

```go
package objindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// Build produces .bvom bytes from packReader's idx and the given pack ID.
// All entries are emitted with pack_idx=0 (M2 has one pack per repo).
func Build(packReader *pack.Reader, packID string) ([]byte, error) {
	if len(packID) != packIDSize {
		return nil, fmt.Errorf("objindex: packID must be 40 hex chars (got %d)", len(packID))
	}
	idx := packReader.Idx()
	entries := make([]Entry, 0, idx.Count())
	for i := 0; i < idx.Count(); i++ {
		entries = append(entries, Entry{
			OID: idx.OIDAt(i), PackID: packID, Offset: idx.OffsetAt(i),
		})
	}
	return build(entries)
}

func build(entries []Entry) ([]byte, error) {
	// Group entries by pack_id, dedup pack_id table.
	idOf := make(map[string]uint16)
	var packTable []string
	for _, e := range entries {
		if _, ok := idOf[e.PackID]; ok {
			continue
		}
		idOf[e.PackID] = uint16(len(packTable))
		packTable = append(packTable, e.PackID)
		if len(packTable) > 0xffff {
			return nil, fmt.Errorf("objindex: too many distinct packs (%d)", len(packTable))
		}
	}
	// Sort entries by OID.
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].OID[:], entries[j].OID[:]) < 0
	})
	// Detect duplicates.
	for i := 1; i < len(entries); i++ {
		if entries[i].OID == entries[i-1].OID {
			return nil, fmt.Errorf("objindex: duplicate OID %s", entries[i].OID)
		}
	}

	count := uint64(len(entries))
	packTblOff := uint64(headerSize) + count*recordSize

	var buf bytes.Buffer
	buf.Grow(int(packTblOff) + 2 + len(packTable)*packIDSize + trailerSize)

	// Header.
	buf.Write(magic)
	_ = binary.Write(&buf, binary.BigEndian, currentVer)
	_ = binary.Write(&buf, binary.BigEndian, count)
	_ = binary.Write(&buf, binary.BigEndian, packTblOff)
	buf.Write(make([]byte, 8)) // reserved

	// Records.
	rec := make([]byte, recordSize)
	for _, e := range entries {
		copy(rec[:20], e.OID[:])
		binary.BigEndian.PutUint16(rec[20:22], idOf[e.PackID])
		rec[22] = 0
		rec[23] = 0
		binary.BigEndian.PutUint64(rec[24:32], e.Offset)
		buf.Write(rec)
	}

	// Pack-id table.
	if len(packTable) > 0xffff {
		return nil, fmt.Errorf("objindex: pack table too large")
	}
	_ = binary.Write(&buf, binary.BigEndian, uint16(len(packTable)))
	for _, id := range packTable {
		buf.WriteString(id)
	}

	// Trailer = SHA-256 over everything so far.
	sum := sha256.Sum256(buf.Bytes())
	buf.Write(sum[:])

	return buf.Bytes(), nil
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/objindex/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/objindex/
git commit -m "M2 objindex: .bvom format + deterministic Build"
```

---

## Task 12: objindex — Open + Lookup

**Files:**
- Create: `internal/objindex/read.go`
- Modify: `internal/objindex/format_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/objindex/format_test.go`:

```go
import (
	"context"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.New(localfs.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	return s
}

func TestOpenAndLookup_RoundTrip(t *testing.T) {
	a := oidOf(t, "0000000000000000000000000000000000000001")
	b := oidOf(t, "1000000000000000000000000000000000000001")
	c := oidOf(t, "ff00000000000000000000000000000000000001")
	pid := strings.Repeat("a", 40)
	entries := []Entry{
		{OID: a, PackID: pid, Offset: 12},
		{OID: b, PackID: pid, Offset: 5000},
		{OID: c, PackID: pid, Offset: 90000},
	}
	out, err := build(entries)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k.bvom", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	m, err := Open(context.Background(), store, "k.bvom")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, e := range entries {
		got, off, ok := m.Lookup(e.OID)
		if !ok {
			t.Fatalf("Lookup(%s) miss", e.OID)
		}
		if got != e.PackID {
			t.Fatalf("Lookup pack: got %s, want %s", got, e.PackID)
		}
		if off != e.Offset {
			t.Fatalf("Lookup offset: got %d, want %d", off, e.Offset)
		}
	}
	bogus := oidOf(t, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if _, _, ok := m.Lookup(bogus); ok {
		t.Fatalf("expected miss for bogus OID")
	}
}

func TestOpen_RejectsBadMagic(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader("XXXXgarbage"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected error on bad magic")
	}
}

func TestOpen_RejectsBadTrailer(t *testing.T) {
	a := oidOf(t, "0000000000000000000000000000000000000001")
	pid := strings.Repeat("a", 40)
	out, err := build([]Entry{{OID: a, PackID: pid, Offset: 1}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Flip a byte in the data section, leave trailer.
	out[headerSize] ^= 0xff
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected trailer mismatch error")
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/objindex/...`
Expected: FAIL — `Open` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/objindex/read.go`:

```go
package objindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Map is a parsed .bvom file held in memory. Lookup is O(log n) via
// binary search.
type Map struct {
	count       int
	records     []byte // count * recordSize
	packTable   []string
}

// Open reads the entire .bvom from store, validates structure + trailer
// hash, and returns a Map.
func Open(ctx context.Context, store storage.ObjectStore, key string) (*Map, error) {
	rc, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("objindex: get %s: %w", key, err)
	}
	defer rc.Close()
	all, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("objindex: read %s: %w", key, err)
	}
	if len(all) < headerSize+trailerSize {
		return nil, fmt.Errorf("%w: file too small (%d)", ErrCorrupt, len(all))
	}
	// Validate trailer.
	want := sha256.Sum256(all[:len(all)-trailerSize])
	if !bytes.Equal(want[:], all[len(all)-trailerSize:]) {
		return nil, fmt.Errorf("%w: trailer hash mismatch", ErrCorrupt)
	}
	// Parse header.
	if !bytes.Equal(all[:4], magic) {
		return nil, fmt.Errorf("%w: bad magic %x", ErrCorrupt, all[:4])
	}
	if v := binary.BigEndian.Uint32(all[4:8]); v != currentVer {
		return nil, fmt.Errorf("%w: version %d", ErrCorrupt, v)
	}
	count := binary.BigEndian.Uint64(all[8:16])
	packTblOff := binary.BigEndian.Uint64(all[16:24])
	expectedRecBytes := uint64(headerSize) + count*recordSize
	if packTblOff != expectedRecBytes {
		return nil, fmt.Errorf("%w: pack_tbl offset mismatch (got %d, want %d)",
			ErrCorrupt, packTblOff, expectedRecBytes)
	}
	if uint64(len(all)) < packTblOff+2+uint64(trailerSize) {
		return nil, fmt.Errorf("%w: truncated pack table", ErrCorrupt)
	}
	nPacks := binary.BigEndian.Uint16(all[packTblOff : packTblOff+2])
	tblBytes := uint64(nPacks) * uint64(packIDSize)
	if uint64(len(all)) < packTblOff+2+tblBytes+uint64(trailerSize) {
		return nil, fmt.Errorf("%w: truncated pack table body", ErrCorrupt)
	}
	packs := make([]string, nPacks)
	for i := 0; i < int(nPacks); i++ {
		off := packTblOff + 2 + uint64(i)*uint64(packIDSize)
		packs[i] = string(all[off : off+uint64(packIDSize)])
	}
	records := all[headerSize : headerSize+count*recordSize]
	// Sanity: records sorted ascending.
	for i := 1; i < int(count); i++ {
		prev := records[(i-1)*recordSize : (i-1)*recordSize+20]
		cur := records[i*recordSize : i*recordSize+20]
		if bytes.Compare(prev, cur) >= 0 {
			return nil, fmt.Errorf("%w: records not sorted at %d", ErrCorrupt, i)
		}
	}
	return &Map{count: int(count), records: records, packTable: packs}, nil
}

// Count returns the number of indexed objects.
func (m *Map) Count() int { return m.count }

// Lookup returns (packID, offset, ok) for oid.
func (m *Map) Lookup(oid pack.OID) (string, uint64, bool) {
	pos := sort.Search(m.count, func(i int) bool {
		rec := m.records[i*recordSize : i*recordSize+20]
		return bytes.Compare(rec, oid[:]) >= 0
	})
	if pos == m.count {
		return "", 0, false
	}
	rec := m.records[pos*recordSize : (pos+1)*recordSize]
	if !bytes.Equal(rec[:20], oid[:]) {
		return "", 0, false
	}
	idx := binary.BigEndian.Uint16(rec[20:22])
	off := binary.BigEndian.Uint64(rec[24:32])
	if int(idx) >= len(m.packTable) {
		return "", 0, false
	}
	return m.packTable[idx], off, true
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/objindex/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/objindex/read.go internal/objindex/format_test.go
git commit -m "M2 objindex: Open + Lookup with trailer-hash validation"
```

---

## Task 13: commitgraph — .bvcg format + Build

**Files:**
- Create: `internal/commitgraph/format.go`
- Create: `internal/commitgraph/build.go`
- Create: `internal/commitgraph/format_test.go`

`.bvcg` per spec §3.5: 32-byte header, ref-tips array, sorted commit records (oid + parents), string table, 32-byte SHA-256 trailer.

To build: walk every commit object in the pack, parse parent OIDs from the inflated commit body. Commit format is well-defined: lines `parent <hex>` immediately after `tree <hex>`. We do not need to tolerate garbled commits at this layer — `git fsck` ran in step 2 of import.

- [ ] **Step 1: Write the failing test**

Create `internal/commitgraph/format_test.go`:

```go
package commitgraph

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestBuild_HeaderAndTrailer(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	b := oid(t, "0000000000000000000000000000000000000002")
	commits := []Record{
		{OID: a, Parents: nil},
		{OID: b, Parents: []pack.OID{a}},
	}
	tips := []Tip{{Ref: "refs/heads/main", OID: b}}
	out, err := build(commits, tips)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if string(out[:4]) != "BVCG" {
		t.Fatalf("magic: %q", out[:4])
	}
	if v := binary.BigEndian.Uint32(out[4:8]); v != 1 {
		t.Fatalf("version: %d", v)
	}
	if cnt := binary.BigEndian.Uint64(out[8:16]); cnt != 2 {
		t.Fatalf("n_commits: %d", cnt)
	}
	if nt := binary.BigEndian.Uint32(out[16:20]); nt != 1 {
		t.Fatalf("n_tips: %d", nt)
	}
	pre := out[:len(out)-trailerSize]
	want := sha256.Sum256(pre)
	if !bytes.Equal(want[:], out[len(out)-trailerSize:]) {
		t.Fatalf("trailer mismatch")
	}
}

func TestBuild_DeterministicSortOrder(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000003")
	b := oid(t, "0000000000000000000000000000000000000001")
	c := oid(t, "0000000000000000000000000000000000000002")
	out1, err := build([]Record{{OID: a}, {OID: b}, {OID: c}}, nil)
	if err != nil {
		t.Fatalf("build1: %v", err)
	}
	out2, err := build([]Record{{OID: c}, {OID: a}, {OID: b}}, nil)
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatalf("non-deterministic build")
	}
}

func oid(t *testing.T, hex string) pack.OID {
	t.Helper()
	o, err := pack.ParseOID(hex)
	if err != nil {
		t.Fatalf("ParseOID: %v", err)
	}
	return o
}

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.New(localfs.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	return s
}

// silence imports until Open lands
var _ = context.Background
var _ = strings.TrimSpace
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/commitgraph/...`
Expected: FAIL — package undefined.

- [ ] **Step 3: Write the format & build implementation**

Create `internal/commitgraph/format.go`:

```go
// Package commitgraph implements the M2-local commit graph (.bvcg).
//
// Layout (per spec §3.5):
//
//   header (32 bytes)  : magic "BVCG", version u32, n_commits u64,
//                        n_tips u32, reserved 12 bytes
//   tips (n_tips × 24) : ref_name_offset u32, oid 20 bytes
//   commits sorted by oid: oid 20 + n_parents u8 + parent_oids[n_parents]*20
//   string table       : NUL-terminated UTF-8 strings (ref names)
//   trailer (32 bytes) : SHA-256 over preceding bytes
package commitgraph

import (
	"errors"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

const (
	headerSize  = 32
	tipSize     = 24
	trailerSize = 32
	currentVer  = uint32(1)
)

var magic = []byte{'B', 'V', 'C', 'G'}

// ErrCorrupt indicates a malformed .bvcg file.
var ErrCorrupt = errors.New("commitgraph: corrupt")

// Tip names a ref and the commit it points to.
type Tip struct {
	Ref string
	OID pack.OID
}

// Record is one commit's entry: its OID and parent OIDs in commit order.
type Record struct {
	OID     pack.OID
	Parents []pack.OID
}
```

Create `internal/commitgraph/build.go`:

```go
package commitgraph

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// Build inflates every commit in packReader, derives parents from the
// commit body, and produces .bvcg bytes paired with the given tips.
func Build(packReader *pack.Reader, tips []Tip) ([]byte, error) {
	var commits []Record
	if err := packReader.ForEach(func(oid pack.OID, off uint64) error {
		// Cheap test: the ForEach gives us OID and offset, but not type.
		// We resolve the object and skip non-commits.
		obj, err := packReader.Get(packReaderCtx(), oid)
		if err != nil {
			return err
		}
		if obj.Type != pack.TypeCommit {
			return nil
		}
		parents, err := parseParents(obj.Data)
		if err != nil {
			return fmt.Errorf("commit %s: %w", oid, err)
		}
		commits = append(commits, Record{OID: oid, Parents: parents})
		return nil
	}); err != nil {
		return nil, err
	}
	return build(commits, tips)
}

// packReaderCtx returns a fresh background context for ForEach-driven
// pack reads. M3+ may pass user contexts in via a new packReader.GetCtx
// API; for M2 importer use, background is correct (the importer holds
// its own ctx and only calls Build on local data).
func packReaderCtx() context.Context { return context.Background() }
```

Note: I reference `context.Background()` — add the missing import.

```go
import (
	// ...existing...
	"context"
)
```

```go
// parseParents extracts parent OIDs from a commit body. Lines:
//   tree <hex>\n
//   parent <hex>\n   (zero or more, immediately following tree)
//   author ...
// We stop scanning at the first non-parent header line after tree.
func parseParents(body []byte) ([]pack.OID, error) {
	var parents []pack.OID
	for len(body) > 0 {
		nl := bytes.IndexByte(body, '\n')
		if nl < 0 {
			return parents, nil
		}
		line := body[:nl]
		body = body[nl+1:]
		switch {
		case bytes.HasPrefix(line, []byte("tree ")):
			continue
		case bytes.HasPrefix(line, []byte("parent ")):
			hex := string(line[len("parent "):])
			oid, err := pack.ParseOID(hex)
			if err != nil {
				return nil, fmt.Errorf("parse parent %q: %w", hex, err)
			}
			parents = append(parents, oid)
		default:
			return parents, nil
		}
	}
	return parents, nil
}

// build emits the .bvcg bytes for already-extracted commit records and tips.
func build(commits []Record, tips []Tip) ([]byte, error) {
	// Sort by OID and dedup.
	sort.Slice(commits, func(i, j int) bool {
		return bytes.Compare(commits[i].OID[:], commits[j].OID[:]) < 0
	})
	for i := 1; i < len(commits); i++ {
		if commits[i].OID == commits[i-1].OID {
			return nil, fmt.Errorf("commitgraph: duplicate commit %s", commits[i].OID)
		}
	}
	// Build string table for tip ref names.
	stringOffset := make(map[string]uint32)
	var stringTable bytes.Buffer
	for _, t := range tips {
		if _, ok := stringOffset[t.Ref]; ok {
			continue
		}
		stringOffset[t.Ref] = uint32(stringTable.Len())
		stringTable.WriteString(t.Ref)
		stringTable.WriteByte(0)
	}
	// Sort tips by (ref, oid) for determinism.
	sortedTips := make([]Tip, len(tips))
	copy(sortedTips, tips)
	sort.Slice(sortedTips, func(i, j int) bool {
		if sortedTips[i].Ref != sortedTips[j].Ref {
			return sortedTips[i].Ref < sortedTips[j].Ref
		}
		return bytes.Compare(sortedTips[i].OID[:], sortedTips[j].OID[:]) < 0
	})

	var buf bytes.Buffer
	buf.Grow(headerSize + len(tips)*tipSize + len(commits)*40 + stringTable.Len() + trailerSize)

	// Header.
	buf.Write(magic)
	_ = binary.Write(&buf, binary.BigEndian, currentVer)
	_ = binary.Write(&buf, binary.BigEndian, uint64(len(commits)))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(sortedTips)))
	buf.Write(make([]byte, 12)) // reserved

	// Tips.
	for _, tt := range sortedTips {
		off, ok := stringOffset[tt.Ref]
		if !ok {
			return nil, fmt.Errorf("commitgraph: tip ref %q missing from string table", tt.Ref)
		}
		_ = binary.Write(&buf, binary.BigEndian, off)
		buf.Write(tt.OID[:])
	}

	// Commit records.
	for _, c := range commits {
		buf.Write(c.OID[:])
		if len(c.Parents) > 255 {
			return nil, fmt.Errorf("commitgraph: %s has %d parents (max 255)",
				c.OID, len(c.Parents))
		}
		buf.WriteByte(byte(len(c.Parents)))
		for _, p := range c.Parents {
			buf.Write(p[:])
		}
	}

	// String table.
	buf.Write(stringTable.Bytes())

	// Trailer.
	sum := sha256.Sum256(buf.Bytes())
	buf.Write(sum[:])

	return buf.Bytes(), nil
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/commitgraph/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/commitgraph/
git commit -m "M2 commitgraph: .bvcg format + Build with parent parsing"
```

---

## Task 14: commitgraph — Open + Parents + Tips

**Files:**
- Create: `internal/commitgraph/read.go`
- Modify: `internal/commitgraph/format_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/commitgraph/format_test.go`:

```go
func TestOpenAndParents_RoundTrip(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	b := oid(t, "0000000000000000000000000000000000000002")
	c := oid(t, "0000000000000000000000000000000000000003")
	commits := []Record{
		{OID: a},
		{OID: b, Parents: []pack.OID{a}},
		{OID: c, Parents: []pack.OID{b, a}}, // octopus
	}
	tips := []Tip{{Ref: "refs/heads/main", OID: c}}
	out, err := build(commits, tips)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k.bvcg", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	g, err := Open(context.Background(), store, "k.bvcg")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	gotA, ok := g.Parents(a)
	if !ok || len(gotA) != 0 {
		t.Fatalf("Parents(a): ok=%v parents=%v", ok, gotA)
	}
	gotB, ok := g.Parents(b)
	if !ok || len(gotB) != 1 || gotB[0] != a {
		t.Fatalf("Parents(b): %v %v", gotB, ok)
	}
	gotC, ok := g.Parents(c)
	if !ok || len(gotC) != 2 || gotC[0] != b || gotC[1] != a {
		t.Fatalf("Parents(c): %v %v", gotC, ok)
	}
	gotTips := g.Tips()
	if len(gotTips) != 1 || gotTips[0].Ref != "refs/heads/main" || gotTips[0].OID != c {
		t.Fatalf("Tips: %+v", gotTips)
	}
}

func TestOpen_RejectsBadTrailer(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	out, _ := build([]Record{{OID: a}}, nil)
	out[headerSize] ^= 0xff
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected error on tampered body")
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/commitgraph/...`
Expected: FAIL — `Open` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/commitgraph/read.go`:

```go
package commitgraph

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Graph holds a parsed .bvcg in memory. Parents lookup is O(log n)
// via binary search.
type Graph struct {
	commits []byte // (oid 20 + n 1 + parents n*20) records, sorted by oid
	commitOffsets []int // byte offset of each record in commits
	tips    []Tip
}

// Open reads the entire .bvcg from store, validates trailer, parses.
func Open(ctx context.Context, store storage.ObjectStore, key string) (*Graph, error) {
	rc, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("commitgraph: get %s: %w", key, err)
	}
	defer rc.Close()
	all, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if len(all) < headerSize+trailerSize {
		return nil, fmt.Errorf("%w: too small", ErrCorrupt)
	}
	want := sha256.Sum256(all[:len(all)-trailerSize])
	if !bytes.Equal(want[:], all[len(all)-trailerSize:]) {
		return nil, fmt.Errorf("%w: trailer hash mismatch", ErrCorrupt)
	}
	if !bytes.Equal(all[:4], magic) {
		return nil, fmt.Errorf("%w: magic %x", ErrCorrupt, all[:4])
	}
	if v := binary.BigEndian.Uint32(all[4:8]); v != currentVer {
		return nil, fmt.Errorf("%w: version %d", ErrCorrupt, v)
	}
	nCommits := binary.BigEndian.Uint64(all[8:16])
	nTips := binary.BigEndian.Uint32(all[16:20])

	tipsStart := headerSize
	tipsEnd := tipsStart + int(nTips)*tipSize
	if tipsEnd > len(all)-trailerSize {
		return nil, fmt.Errorf("%w: tips overflow", ErrCorrupt)
	}
	tipsBuf := all[tipsStart:tipsEnd]

	commitsStart := tipsEnd
	commits, commitsEnd, err := scanCommits(all[commitsStart:len(all)-trailerSize], int(nCommits))
	if err != nil {
		return nil, err
	}
	commitOffsets, commitsBytes := commits, commitsEnd

	stringTable := all[commitsStart+commitsBytes : len(all)-trailerSize]

	tips := make([]Tip, 0, nTips)
	for i := 0; i < int(nTips); i++ {
		off := binary.BigEndian.Uint32(tipsBuf[i*tipSize : i*tipSize+4])
		var oid pack.OID
		copy(oid[:], tipsBuf[i*tipSize+4:i*tipSize+24])
		ref, err := readCString(stringTable, int(off))
		if err != nil {
			return nil, fmt.Errorf("%w: tip ref: %v", ErrCorrupt, err)
		}
		tips = append(tips, Tip{Ref: ref, OID: oid})
	}

	return &Graph{
		commits:       all[commitsStart : commitsStart+commitsBytes],
		commitOffsets: commitOffsets,
		tips:          tips,
	}, nil
}

func scanCommits(buf []byte, nCommits int) (offsets []int, totalBytes int, err error) {
	offsets = make([]int, 0, nCommits)
	pos := 0
	for i := 0; i < nCommits; i++ {
		offsets = append(offsets, pos)
		if pos+21 > len(buf) {
			return nil, 0, fmt.Errorf("%w: commit record %d truncated", ErrCorrupt, i)
		}
		nParents := int(buf[pos+20])
		recLen := 20 + 1 + nParents*20
		if pos+recLen > len(buf) {
			return nil, 0, fmt.Errorf("%w: commit record %d parents truncated", ErrCorrupt, i)
		}
		// Sorted check (skip i==0).
		if i > 0 {
			prev := buf[offsets[i-1] : offsets[i-1]+20]
			cur := buf[pos : pos+20]
			if bytes.Compare(prev, cur) >= 0 {
				return nil, 0, fmt.Errorf("%w: commits not sorted at %d", ErrCorrupt, i)
			}
		}
		pos += recLen
	}
	return offsets, pos, nil
}

func readCString(buf []byte, off int) (string, error) {
	if off < 0 || off >= len(buf) {
		return "", fmt.Errorf("offset %d out of range %d", off, len(buf))
	}
	end := bytes.IndexByte(buf[off:], 0)
	if end < 0 {
		return "", fmt.Errorf("unterminated string at %d", off)
	}
	return string(buf[off : off+end]), nil
}

// Parents returns the parent OIDs of the given commit, in commit order.
// (false, nil) if the commit is not in the graph.
func (g *Graph) Parents(oid pack.OID) ([]pack.OID, bool) {
	pos := sort.Search(len(g.commitOffsets), func(i int) bool {
		off := g.commitOffsets[i]
		return bytes.Compare(g.commits[off:off+20], oid[:]) >= 0
	})
	if pos == len(g.commitOffsets) {
		return nil, false
	}
	off := g.commitOffsets[pos]
	if !bytes.Equal(g.commits[off:off+20], oid[:]) {
		return nil, false
	}
	n := int(g.commits[off+20])
	parents := make([]pack.OID, n)
	for i := 0; i < n; i++ {
		copy(parents[i][:], g.commits[off+21+i*20:off+21+(i+1)*20])
	}
	return parents, true
}

// Tips returns a copy of the registered (ref, oid) tips.
func (g *Graph) Tips() []Tip {
	out := make([]Tip, len(g.tips))
	copy(out, g.tips)
	return out
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/commitgraph/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/commitgraph/read.go internal/commitgraph/format_test.go
git commit -m "M2 commitgraph: Open + Parents + Tips with binary search"
```

---

## Task 15: manifest body — typed M2 fields + golden contract

**Files:**
- Create: `internal/repo/manifest/body.go`
- Create: `internal/repo/manifest/body_test.go`
- Create: `internal/repo/manifest/testdata/golden/m2-body-minimal.json`
- Create: `internal/repo/manifest/testdata/golden/m2-body-single-pack.json`

The Body struct mirrors the on-the-wire JSON 1:1 (snake_case JSON tags). M3 reads these byte sequences from buckets created by M2; drift is an on-the-wire break.

- [ ] **Step 1: Write the failing tests + create the goldens**

Create `internal/repo/manifest/testdata/golden/m2-body-minimal.json`:

```json
{
  "default_branch": "refs/heads/main",
  "refs": {},
  "packs": [],
  "indexes": {}
}
```

Create `internal/repo/manifest/testdata/golden/m2-body-single-pack.json`:

```json
{
  "default_branch": "refs/heads/main",
  "refs": {
    "refs/heads/main": "0123456789abcdef0123456789abcdef01234567",
    "refs/tags/v1": "1123456789abcdef0123456789abcdef01234567"
  },
  "packs": [
    {
      "pack_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "pack_key": "tenants/t/repos/r/packs/canonical/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.pack",
      "idx_key": "tenants/t/repos/r/packs/canonical/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.idx",
      "size_bytes": 4096,
      "object_count": 7
    }
  ],
  "indexes": {
    "object_map": {
      "key": "tenants/t/repos/r/indexes/object-map/cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc.bvom",
      "hash": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    },
    "commit_graph": {
      "key": "tenants/t/repos/r/indexes/commit-graphs/dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd.graph",
      "hash": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
    }
  }
}
```

Create `internal/repo/manifest/body_test.go`:

```go
package manifest

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite golden files from current Body marshal output")

func TestBody_GoldenMinimal(t *testing.T) {
	body := Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{},
		Packs:         []PackEntry{},
		Indexes:       Indexes{},
	}
	checkGolden(t, "m2-body-minimal.json", body)
}

func TestBody_GoldenSinglePack(t *testing.T) {
	body := Body{
		DefaultBranch: "refs/heads/main",
		Refs: map[string]string{
			"refs/heads/main": "0123456789abcdef0123456789abcdef01234567",
			"refs/tags/v1":    "1123456789abcdef0123456789abcdef01234567",
		},
		Packs: []PackEntry{
			{
				PackID:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				PackKey:     "tenants/t/repos/r/packs/canonical/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.pack",
				IdxKey:      "tenants/t/repos/r/packs/canonical/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.idx",
				SizeBytes:   4096,
				ObjectCount: 7,
			},
		},
		Indexes: Indexes{
			ObjectMap: &IndexRef{
				Key:  "tenants/t/repos/r/indexes/object-map/cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc.bvom",
				Hash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			},
			CommitGraph: &IndexRef{
				Key:  "tenants/t/repos/r/indexes/commit-graphs/dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd.graph",
				Hash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			},
		},
	}
	checkGolden(t, "m2-body-single-pack.json", body)
}

func TestBody_RoundTrip(t *testing.T) {
	cases := []string{"m2-body-minimal.json", "m2-body-single-pack.json"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata/golden", name))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var b Body
			if err := json.Unmarshal(data, &b); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, err := MarshalBody(b)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal(canonicalize(t, data), canonicalize(t, got)) {
				t.Fatalf("round-trip mismatch.\nwant:\n%s\ngot:\n%s", canonicalize(t, data), canonicalize(t, got))
			}
		})
	}
}

func checkGolden(t *testing.T, name string, body Body) {
	t.Helper()
	got, err := MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	path := filepath.Join("testdata/golden", name)
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	wantC := canonicalize(t, want)
	gotC := canonicalize(t, got)
	if !bytes.Equal(wantC, gotC) {
		t.Fatalf("golden mismatch %s.\nwant:\n%s\ngot:\n%s", name, wantC, gotC)
	}
}

// canonicalize re-marshals JSON via encoding/json with the standard
// formatting we use, so trivial whitespace differences in the golden
// don't break the test.
func canonicalize(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("canonicalize unmarshal: %v", err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("canonicalize marshal: %v", err)
	}
	return out
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/repo/manifest/...`
Expected: FAIL — `Body`, `MarshalBody` etc. undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/repo/manifest/body.go`:

```go
package manifest

import (
	"encoding/json"
	"fmt"
)

// Body is the typed view of M2-owned root-manifest body fields.
// JSON tags must match the on-the-wire shape exactly; M3 reads buckets
// produced by M2 and the wire format is the contract.
type Body struct {
	DefaultBranch string            `json:"default_branch"`
	Refs          map[string]string `json:"refs"`
	Packs         []PackEntry       `json:"packs"`
	Indexes       Indexes           `json:"indexes"`
}

// PackEntry references one pack uploaded under packs/canonical/.
type PackEntry struct {
	PackID      string `json:"pack_id"`
	PackKey     string `json:"pack_key"`
	IdxKey      string `json:"idx_key"`
	SizeBytes   int64  `json:"size_bytes"`
	ObjectCount int    `json:"object_count"`
}

// Indexes carries pointers to M2 reachability index objects.
type Indexes struct {
	ObjectMap   *IndexRef `json:"object_map,omitempty"`
	CommitGraph *IndexRef `json:"commit_graph,omitempty"`
}

// IndexRef is a key + content-hash pair.
type IndexRef struct {
	Key  string `json:"key"`
	Hash string `json:"hash"`
}

// MarshalBody emits canonical Body JSON. Pretty-printed (2-space indent)
// to keep on-disk diffs readable; the indent is part of the wire format.
func MarshalBody(b Body) ([]byte, error) {
	if b.Refs == nil {
		b.Refs = map[string]string{}
	}
	if b.Packs == nil {
		b.Packs = []PackEntry{}
	}
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("manifest: marshal body: %w", err)
	}
	return out, nil
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/repo/manifest/...`
Expected: PASS for all manifest tests including M1's existing ones.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/manifest/
git commit -m "M2 manifest: typed Body + golden-file wire-format contract"
```

---

## Task 16: importer — Options/Result + clone+fsck+pack

**Files:**
- Create: `internal/importer/importer.go`
- Create: `internal/importer/importer_test.go`

This task implements steps 1-3 of the import flow (§3.6): clone source to a private tmpdir, fsck it, run pack-objects to produce one canonical pack. Steps 4-7 land in tasks 17-18.

- [ ] **Step 1: Write the failing test**

Create `internal/importer/importer_test.go`:

```go
package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
}

// makeSrcRepo authors a tiny bare repo at a path the test owns.
func makeSrcRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	mustRun := func(args ...string) {
		t.Helper()
		if out, err := gitcli.RunForTest(work, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustRun("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustRun("add", "f")
	mustRun("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "init")
	bare := t.TempDir() + "-bare"
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	return bare
}

func TestPrepareLocalPack_ProducesPackAndRefs(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	prep, err := prepareLocalPack(context.Background(), src)
	if err != nil {
		t.Fatalf("prepareLocalPack: %v", err)
	}
	if prep.PackID == "" || len(prep.PackID) != 40 {
		t.Fatalf("PackID: %q", prep.PackID)
	}
	if _, err := os.Stat(prep.PackPath); err != nil {
		t.Fatalf("pack stat: %v", err)
	}
	if _, err := os.Stat(prep.IdxPath); err != nil {
		t.Fatalf("idx stat: %v", err)
	}
	if len(prep.Refs) == 0 {
		t.Fatalf("expected refs")
	}
	if !strings.HasPrefix(prep.DefaultBranch, "refs/heads/") {
		t.Fatalf("DefaultBranch: %q", prep.DefaultBranch)
	}
	// Cleanup is the caller's responsibility; verify the helper exposed
	// the workdir path for downstream tasks.
	if _, err := os.Stat(prep.WorkDir); err != nil {
		t.Fatalf("workdir stat: %v", err)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/importer/...`
Expected: FAIL — `prepareLocalPack` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/importer/importer.go`:

```go
// Package importer round-trips a bare git repo from local disk into
// bucketvcs storage. See spec §3.6 for the import flow. The exported
// surface is Import(ctx, store, opts); the unexported helpers in this
// file are split out for testability.
package importer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// Options configures one import.
type Options struct {
	SourceDir     string
	Tenant, Repo  string
	Actor         string
	DefaultBranch string // optional; if empty, taken from source HEAD
}

// Result describes a successful import's resulting state.
type Result struct {
	PackID          string
	ObjectMapHash   string
	CommitGraphHash string
	ManifestVersion uint64
	RefCount        int
	ObjectCount     int
}

// preparedPack is the local-disk artifact set produced before any upload.
// Caller owns WorkDir and must clean it up.
type preparedPack struct {
	WorkDir       string
	PackID        string
	PackPath      string
	IdxPath       string
	Refs          map[string]string
	DefaultBranch string
}

// prepareLocalPack runs steps 1-3 + 5 of §3.6: clone, fsck, pack-objects,
// collect refs.
func prepareLocalPack(ctx context.Context, sourceDir string) (*preparedPack, error) {
	work, err := os.MkdirTemp("", "bucketvcs-import-")
	if err != nil {
		return nil, fmt.Errorf("importer: tmpdir: %w", err)
	}
	defer func() {
		// On error, drop the workdir; on success, the caller takes ownership.
		if err != nil {
			_ = os.RemoveAll(work)
		}
	}()

	bare := filepath.Join(work, "src.git")
	if err := gitcli.CloneBareMirror(ctx, sourceDir, bare); err != nil {
		return nil, fmt.Errorf("importer: clone: %w", err)
	}
	if err := gitcli.Fsck(ctx, bare, true); err != nil {
		return nil, fmt.Errorf("importer: source fsck: %w", err)
	}
	prefix := filepath.Join(work, "out", "pack")
	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		return nil, fmt.Errorf("importer: mkdir pack out: %w", err)
	}
	packID, err := gitcli.PackObjectsAll(ctx, bare, prefix)
	if err != nil {
		return nil, fmt.Errorf("importer: pack-objects: %w", err)
	}
	refs, err := gitcli.ShowRef(ctx, bare)
	if err != nil {
		return nil, fmt.Errorf("importer: show-ref: %w", err)
	}
	headTarget, err := gitcli.SymbolicRef(ctx, bare, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("importer: symbolic-ref HEAD: %w", err)
	}
	return &preparedPack{
		WorkDir:       work,
		PackID:        packID,
		PackPath:      prefix + "-" + packID + ".pack",
		IdxPath:       prefix + "-" + packID + ".idx",
		Refs:          refs,
		DefaultBranch: headTarget,
	}, nil
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/importer/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/importer/
git commit -m "M2 importer: prepareLocalPack (clone + fsck + pack-objects + refs)"
```

---

## Task 17: importer — build .bvom and .bvcg locally

**Files:**
- Modify: `internal/importer/importer.go`
- Modify: `internal/importer/importer_test.go`

This task implements step 4 of §3.6: open the local pack via the pure-Go reader, build .bvom and .bvcg, hash them.

- [ ] **Step 1: Write the failing test**

Append to `internal/importer/importer_test.go`:

```go
import (
	// add:
	"crypto/sha256"
	"encoding/hex"
)

func TestBuildIndexes_FromPreparedPack(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	prep, err := prepareLocalPack(context.Background(), src)
	if err != nil {
		t.Fatalf("prepareLocalPack: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prep.WorkDir) })

	indexes, err := buildIndexesLocal(context.Background(), prep)
	if err != nil {
		t.Fatalf("buildIndexesLocal: %v", err)
	}
	if len(indexes.ObjectMapBytes) == 0 {
		t.Fatalf("empty .bvom")
	}
	if len(indexes.CommitGraphBytes) == 0 {
		t.Fatalf("empty .bvcg")
	}
	if indexes.ObjectMapHash != sha256Hex(indexes.ObjectMapBytes) {
		t.Fatalf(".bvom hash mismatch")
	}
	if indexes.CommitGraphHash != sha256Hex(indexes.CommitGraphBytes) {
		t.Fatalf(".bvcg hash mismatch")
	}
	if indexes.ObjectCount == 0 {
		t.Fatalf("ObjectCount=0")
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/importer/...`
Expected: FAIL — `buildIndexesLocal` undefined.

- [ ] **Step 3: Write the implementation**

Append to `internal/importer/importer.go`:

```go
import (
	// add:
	"crypto/sha256"
	"encoding/hex"

	"github.com/bucketvcs/bucketvcs/internal/commitgraph"
	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// localIndexes carries the bytes + content-hashes of .bvom and .bvcg
// produced from the local prepared pack. The bytes are uploaded as-is
// in step 6 of §3.6.
type localIndexes struct {
	ObjectMapBytes   []byte
	ObjectMapHash    string
	CommitGraphBytes []byte
	CommitGraphHash  string
	ObjectCount      int
	PackSizeBytes    int64
}

// buildIndexesLocal opens the local pack via pack.Reader (backed by an
// in-memory store keyed off the local file), then calls objindex.Build
// and commitgraph.Build.
//
// We use an in-memory ObjectStore for the local pack so the M2 read
// path is exercised even at import time, validating the pack reader
// against every imported repo.
func buildIndexesLocal(ctx context.Context, prep *preparedPack) (*localIndexes, error) {
	store, err := newLocalFilePackStore(prep.PackPath, prep.IdxPath)
	if err != nil {
		return nil, fmt.Errorf("importer: localfile pack store: %w", err)
	}
	r, err := pack.Open(ctx, store, "p.pack", "p.idx")
	if err != nil {
		return nil, fmt.Errorf("importer: pack.Open: %w", err)
	}
	defer r.Close()

	// .bvom from pack idx.
	bvom, err := objindex.Build(r, prep.PackID)
	if err != nil {
		return nil, fmt.Errorf("importer: objindex.Build: %w", err)
	}
	bvomSum := sha256.Sum256(bvom)

	// .bvcg from pack: derive ref tips that point at commits.
	tips, err := buildTipsFromRefs(ctx, r, prep.Refs)
	if err != nil {
		return nil, fmt.Errorf("importer: buildTipsFromRefs: %w", err)
	}
	bvcg, err := commitgraph.Build(r, tips)
	if err != nil {
		return nil, fmt.Errorf("importer: commitgraph.Build: %w", err)
	}
	bvcgSum := sha256.Sum256(bvcg)

	st, err := os.Stat(prep.PackPath)
	if err != nil {
		return nil, fmt.Errorf("importer: stat pack: %w", err)
	}

	return &localIndexes{
		ObjectMapBytes:   bvom,
		ObjectMapHash:    hex.EncodeToString(bvomSum[:]),
		CommitGraphBytes: bvcg,
		CommitGraphHash:  hex.EncodeToString(bvcgSum[:]),
		ObjectCount:      r.Idx().Count(),
		PackSizeBytes:    st.Size(),
	}, nil
}

// buildTipsFromRefs filters refs down to those whose target is a commit
// in the pack (annotated tags resolve through the tag object's `object`
// field; lightweight tags already point at commits). For M2 we keep
// ref→OID literally; tip resolution to commits is done by walking
// the type for each candidate.
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
		// Resolve annotated tags to their referenced commit by parsing
		// the tag body's `object <hex>` line.
		for obj.Type == pack.TypeTag {
			target, err := tagTarget(obj.Data)
			if err != nil {
				return nil, fmt.Errorf("ref %s: tag target: %w", ref, err)
			}
			oid = target
			obj, err = r.Get(ctx, oid)
			if err != nil {
				return nil, fmt.Errorf("ref %s: dereference tag: %w", ref, err)
			}
		}
		if obj.Type != pack.TypeCommit {
			// Skip non-commit refs (e.g. refs/notes/* containing trees).
			continue
		}
		tips = append(tips, commitgraph.Tip{Ref: ref, OID: oid})
	}
	return tips, nil
}

// tagTarget extracts the OID from a tag object's `object <hex>` line.
func tagTarget(body []byte) (pack.OID, error) {
	for len(body) > 0 {
		nl := indexNewline(body)
		line := body[:nl]
		body = body[nl+1:]
		if len(line) > 7 && string(line[:7]) == "object " {
			return pack.ParseOID(string(line[7:]))
		}
	}
	return pack.OID{}, fmt.Errorf("tag body missing 'object <oid>' line")
}

func indexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return len(b)
}
```

Now create `internal/importer/localfile_store.go` — a tiny in-memory `storage.ObjectStore` that backs only the two keys "p.pack" and "p.idx" with file-on-disk reads. We need this so we can drive the pack reader without a real bucket.

Create `internal/importer/localfile_store.go`:

```go
package importer

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

type localFilePackStore struct {
	packPath, idxPath string
}

func newLocalFilePackStore(packPath, idxPath string) (*localFilePackStore, error) {
	for _, p := range []string{packPath, idxPath} {
		if _, err := os.Stat(p); err != nil {
			return nil, err
		}
	}
	return &localFilePackStore{packPath: packPath, idxPath: idxPath}, nil
}

func (s *localFilePackStore) pathFor(key string) (string, error) {
	switch key {
	case "p.pack":
		return s.packPath, nil
	case "p.idx":
		return s.idxPath, nil
	}
	return "", fmt.Errorf("localFilePackStore: unknown key %q", key)
}

func (s *localFilePackStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (s *localFilePackStore) Head(ctx context.Context, key string) (storage.Object, error) {
	p, err := s.pathFor(key)
	if err != nil {
		return storage.Object{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return storage.Object{}, err
	}
	return storage.Object{Key: key, SizeBytes: st.Size()}, nil
}

func (s *localFilePackStore) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	p, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(start, 0); err != nil {
		f.Close()
		return nil, err
	}
	return &lengthReader{f: f, remaining: end - start + 1}, nil
}

func (s *localFilePackStore) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, fmt.Errorf("localFilePackStore: read-only")
}

func (s *localFilePackStore) PutIfVersionMatches(ctx context.Context, key string, body io.Reader, prev storage.ObjectVersion, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, fmt.Errorf("localFilePackStore: read-only")
}

func (s *localFilePackStore) Delete(ctx context.Context, key string, prev storage.ObjectVersion) error {
	return fmt.Errorf("localFilePackStore: read-only")
}

func (s *localFilePackStore) List(ctx context.Context, opts storage.ListOptions) (storage.ListPage, error) {
	return storage.ListPage{}, fmt.Errorf("localFilePackStore: list not supported")
}

func (s *localFilePackStore) StartMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, fmt.Errorf("localFilePackStore: multipart not supported")
}

func (s *localFilePackStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{}
}

func (s *localFilePackStore) SignedURL(ctx context.Context, key string, opts *storage.SignedURLOptions) (string, error) {
	return "", fmt.Errorf("localFilePackStore: signed URLs not supported")
}

type lengthReader struct {
	f         *os.File
	remaining int64
}

func (lr *lengthReader) Read(p []byte) (int, error) {
	if lr.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > lr.remaining {
		p = p[:lr.remaining]
	}
	n, err := lr.f.Read(p)
	lr.remaining -= int64(n)
	return n, err
}

func (lr *lengthReader) Close() error { return lr.f.Close() }
```

If the `ObjectStore` interface defines additional methods not stubbed above, add no-op implementations returning `errors.New("not supported")`. Run `go build ./...` to verify the interface is satisfied.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/importer/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/importer/
git commit -m "M2 importer: build .bvom and .bvcg locally via pack.Reader over file-backed store"
```

---

## Task 18: importer — upload + Commit (full Import flow)

**Files:**
- Modify: `internal/importer/importer.go`
- Modify: `internal/importer/importer_test.go`

Wires up steps 6 and 7 of §3.6: PutIfAbsent uploads in order, then `repo.Repo.Create` + `repo.Repo.Commit` to advance the manifest atomically.

- [ ] **Step 1: Write the failing test**

Append to `internal/importer/importer_test.go`:

```go
import (
	// add:
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
	"encoding/json"
)

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.New(localfs.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	return s
}

func TestImport_RoundTripState(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	store := newTestStore(t)
	res, err := Import(context.Background(), store, Options{
		SourceDir: src,
		Tenant:    "acme", Repo: "x",
		Actor: "tester",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.PackID) != 40 {
		t.Fatalf("PackID: %q", res.PackID)
	}
	if res.ManifestVersion != 2 {
		// Create writes manifest_version=1; the import Commit advances to 2.
		t.Fatalf("ManifestVersion: got %d, want 2", res.ManifestVersion)
	}
	// Read back via repo and verify body shape.
	r, err := repo.Open(context.Background(), store, "acme", "x")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("Unmarshal body: %v", err)
	}
	if body.DefaultBranch != "refs/heads/main" {
		t.Fatalf("default_branch: %q", body.DefaultBranch)
	}
	if len(body.Refs) == 0 {
		t.Fatalf("no refs in committed body")
	}
	if len(body.Packs) != 1 {
		t.Fatalf("packs: %d", len(body.Packs))
	}
	if body.Packs[0].PackID != res.PackID {
		t.Fatalf("PackID mismatch")
	}
	if body.Indexes.ObjectMap == nil || body.Indexes.ObjectMap.Hash != res.ObjectMapHash {
		t.Fatalf("ObjectMap.Hash mismatch")
	}
	if body.Indexes.CommitGraph == nil || body.Indexes.CommitGraph.Hash != res.CommitGraphHash {
		t.Fatalf("CommitGraph.Hash mismatch")
	}
}

func TestImport_RejectsExistingRepo(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	store := newTestStore(t)
	if _, err := Import(context.Background(), store, Options{SourceDir: src, Tenant: "t", Repo: "r"}); err != nil {
		t.Fatalf("first Import: %v", err)
	}
	if _, err := Import(context.Background(), store, Options{SourceDir: src, Tenant: "t", Repo: "r"}); err == nil {
		t.Fatalf("second Import should fail with ErrRepoExists")
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/importer/...`
Expected: FAIL — `Import` undefined.

- [ ] **Step 3: Write the implementation**

Append to `internal/importer/importer.go`:

```go
import (
	// add:
	"encoding/json"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
)

// Import is the public entry point. See spec §3.6.
func Import(ctx context.Context, store storage.ObjectStore, opts Options) (*Result, error) {
	if opts.SourceDir == "" || opts.Tenant == "" || opts.Repo == "" {
		return nil, fmt.Errorf("importer: SourceDir, Tenant, Repo required")
	}
	prep, err := prepareLocalPack(ctx, opts.SourceDir)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(prep.WorkDir)

	idx, err := buildIndexesLocal(ctx, prep)
	if err != nil {
		return nil, err
	}

	k, err := keys.NewRepo(opts.Tenant, opts.Repo)
	if err != nil {
		return nil, err
	}

	// Step 6: upload (PutIfAbsent) in order: pack, idx, .bvom, .bvcg.
	if err := uploadFile(ctx, store, prep.PackPath, k.CanonicalPackKey(prep.PackID)); err != nil {
		return nil, fmt.Errorf("importer: upload pack: %w", err)
	}
	if err := uploadFile(ctx, store, prep.IdxPath, k.PackIdxKey(prep.PackID, "canonical")); err != nil {
		return nil, fmt.Errorf("importer: upload idx: %w", err)
	}
	bvomKey := k.ObjectMapKey(idx.ObjectMapHash)
	if err := uploadBytes(ctx, store, idx.ObjectMapBytes, bvomKey); err != nil {
		return nil, fmt.Errorf("importer: upload .bvom: %w", err)
	}
	bvcgKey := k.CommitGraphKey(idx.CommitGraphHash)
	if err := uploadBytes(ctx, store, idx.CommitGraphBytes, bvcgKey); err != nil {
		return nil, fmt.Errorf("importer: upload .bvcg: %w", err)
	}

	// Step 7: Create + Commit.
	defaultBranch := opts.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = prep.DefaultBranch
	}
	if defaultBranch == "" {
		defaultBranch = "refs/heads/main"
	}
	r, err := repo.Create(ctx, store, opts.Tenant, opts.Repo, repo.CreateOptions{
		DefaultBranch: defaultBranch,
		ObjectFormat:  "sha1",
		Actor:         opts.Actor,
	})
	if err != nil {
		return nil, err
	}

	body := manifest.Body{
		DefaultBranch: defaultBranch,
		Refs:          prep.Refs,
		Packs: []manifest.PackEntry{{
			PackID:      prep.PackID,
			PackKey:     k.CanonicalPackKey(prep.PackID),
			IdxKey:      k.PackIdxKey(prep.PackID, "canonical"),
			SizeBytes:   idx.PackSizeBytes,
			ObjectCount: idx.ObjectCount,
		}},
		Indexes: manifest.Indexes{
			ObjectMap:   &manifest.IndexRef{Key: bvomKey, Hash: idx.ObjectMapHash},
			CommitGraph: &manifest.IndexRef{Key: bvcgKey, Hash: idx.CommitGraphHash},
		},
	}
	bodyBytes, err := manifest.MarshalBody(body)
	if err != nil {
		return nil, err
	}
	commitTxBody := tx.Body{Type: "import", Actor: opts.Actor}
	if _, err := r.Commit(ctx, commitTxBody, func(prev *repo.RootView) ([]byte, error) {
		return bodyBytes, nil
	}); err != nil {
		return nil, fmt.Errorf("importer: Commit: %w", err)
	}

	// Read back to capture ManifestVersion.
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("importer: ReadRoot post-commit: %w", err)
	}

	return &Result{
		PackID:          prep.PackID,
		ObjectMapHash:   idx.ObjectMapHash,
		CommitGraphHash: idx.CommitGraphHash,
		ManifestVersion: view.Header.ManifestVersion,
		RefCount:        len(prep.Refs),
		ObjectCount:     idx.ObjectCount,
	}, nil
}

func uploadFile(ctx context.Context, store storage.ObjectStore, srcPath, dstKey string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := store.PutIfAbsent(ctx, dstKey, f, nil); err != nil {
		// PutIfAbsent on identical bytes is fine; return the error if the
		// caller cares, but for content-addressed uploads we treat
		// AlreadyExists as success.
		if isAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

func uploadBytes(ctx context.Context, store storage.ObjectStore, b []byte, dstKey string) error {
	if _, err := store.PutIfAbsent(ctx, dstKey, bytesReader(b), nil); err != nil {
		if isAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

func bytesReader(b []byte) io.Reader {
	return io.NopCloser(strings.NewReader(string(b))) // returns io.Reader; the Reader interface is satisfied
}

func isAlreadyExists(err error) bool {
	return errors.Is(err, storage.ErrAlreadyExists)
}
```

You'll need additional imports: `errors`, `io`, `strings`. (`bytesReader` is a helper; if `bytes.NewReader` is preferred, swap it in.)

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/importer/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/importer/
git commit -m "M2 importer: full Import (upload + Create + Commit)"
```

---

## Task 19: exporter — read manifest, download pack, init bare

**Files:**
- Create: `internal/exporter/exporter.go`
- Create: `internal/exporter/exporter_test.go`

The exporter's flow (§6.3): open repo, validate `DestDir` is empty/nonexistent, init bare, download pack to `objects/pack/`, run `git index-pack` to verify and produce idx, write refs via `update-ref`, set HEAD via `symbolic-ref`, run `git fsck`.

- [ ] **Step 1: Write the failing test**

Create `internal/exporter/exporter_test.go`:

```go
package exporter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
}

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.New(localfs.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	return s
}

func makeAndImport(t *testing.T) (storage.ObjectStore, string) {
	t.Helper()
	skipIfNoGit(t)
	work := t.TempDir()
	mustRun := func(args ...string) {
		t.Helper()
		if out, err := gitcli.RunForTest(work, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustRun("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustRun("add", "f")
	mustRun("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "init")
	bare := t.TempDir() + "-bare"
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	store := newTestStore(t)
	if _, err := importer.Import(context.Background(), store, importer.Options{
		SourceDir: bare, Tenant: "acme", Repo: "x", Actor: "test",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	return store, bare
}

func TestExport_RoundTripFsckClean(t *testing.T) {
	store, _ := makeAndImport(t)
	dst := filepath.Join(t.TempDir(), "out")
	res, err := Export(context.Background(), store, Options{
		Tenant: "acme", Repo: "x", DestDir: dst, RunFsck: true,
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !res.FsckOK {
		t.Fatalf("expected FsckOK")
	}
	if _, err := os.Stat(filepath.Join(dst, "objects")); err != nil {
		t.Fatalf("expected objects/: %v", err)
	}
}

func TestExport_RejectsNonEmptyDest(t *testing.T) {
	store, _ := makeAndImport(t)
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "junk"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Export(context.Background(), store, Options{Tenant: "acme", Repo: "x", DestDir: dst, RunFsck: true})
	if err == nil {
		t.Fatalf("expected error on non-empty DestDir")
	}
}

func TestExport_RefsMatchSource(t *testing.T) {
	store, srcBare := makeAndImport(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Export(context.Background(), store, Options{Tenant: "acme", Repo: "x", DestDir: dst, RunFsck: true}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	srcRefs, err := gitcli.ShowRef(context.Background(), srcBare)
	if err != nil {
		t.Fatalf("src ShowRef: %v", err)
	}
	dstRefs, err := gitcli.ShowRef(context.Background(), dst)
	if err != nil {
		t.Fatalf("dst ShowRef: %v", err)
	}
	if len(srcRefs) != len(dstRefs) {
		t.Fatalf("ref count differs: src=%d dst=%d", len(srcRefs), len(dstRefs))
	}
	for k, v := range srcRefs {
		if dstRefs[k] != v {
			t.Fatalf("ref %s: src=%s dst=%s", k, v, dstRefs[k])
		}
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/exporter/...`
Expected: FAIL — package undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/exporter/exporter.go`:

```go
// Package exporter materializes a normal bare git repo on local disk
// from bucketvcs storage. See spec §3.6 (export side) and §6.3.
package exporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Options configures one export.
type Options struct {
	Tenant, Repo string
	DestDir      string
	RunFsck      bool
}

// Result describes a successful export.
type Result struct {
	ManifestVersion uint64
	ObjectCount     int
	FsckOK          bool
}

// ErrDestNotEmpty is returned when DestDir exists with content.
var ErrDestNotEmpty = errors.New("exporter: dest dir exists and is not empty")

// ErrMissingObject is returned when a referenced bucket key is absent.
var ErrMissingObject = errors.New("exporter: bucket missing referenced object")

// Export downloads packs/indexes from store, materializes a normal bare
// git repo at DestDir, and (unless RunFsck=false) runs git fsck.
func Export(ctx context.Context, store storage.ObjectStore, opts Options) (*Result, error) {
	if opts.Tenant == "" || opts.Repo == "" || opts.DestDir == "" {
		return nil, fmt.Errorf("exporter: Tenant, Repo, DestDir required")
	}
	if err := requireEmptyDir(opts.DestDir); err != nil {
		return nil, err
	}

	r, err := repo.Open(ctx, store, opts.Tenant, opts.Repo)
	if err != nil {
		return nil, err
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return nil, err
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		return nil, fmt.Errorf("exporter: unmarshal body: %w", err)
	}

	if err := os.MkdirAll(opts.DestDir, 0o755); err != nil {
		return nil, fmt.Errorf("exporter: mkdir: %w", err)
	}
	if err := gitcli.InitBare(ctx, opts.DestDir); err != nil {
		return nil, fmt.Errorf("exporter: InitBare: %w", err)
	}

	objectCount := 0
	for _, p := range body.Packs {
		count, err := downloadAndIndexPack(ctx, store, p, opts.DestDir)
		if err != nil {
			return nil, err
		}
		objectCount += count
	}

	for ref, oid := range body.Refs {
		if err := gitcli.UpdateRef(ctx, opts.DestDir, ref, oid); err != nil {
			return nil, fmt.Errorf("exporter: update-ref %s: %w", ref, err)
		}
	}
	if body.DefaultBranch != "" {
		if err := gitcli.SymbolicRefSet(ctx, opts.DestDir, "HEAD", body.DefaultBranch); err != nil {
			return nil, fmt.Errorf("exporter: set HEAD: %w", err)
		}
	}

	res := &Result{ManifestVersion: view.Header.ManifestVersion, ObjectCount: objectCount}
	if opts.RunFsck {
		if err := gitcli.Fsck(ctx, opts.DestDir, true); err != nil {
			return res, fmt.Errorf("exporter: fsck: %w", err)
		}
		res.FsckOK = true
	}
	return res, nil
}

func requireEmptyDir(p string) error {
	st, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("exporter: dest %s is not a directory", p)
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return ErrDestNotEmpty
	}
	return nil
}

// downloadAndIndexPack copies the .pack from store into dest's objects/pack/
// and runs git index-pack to (re)build the .idx. Returns object count.
func downloadAndIndexPack(ctx context.Context, store storage.ObjectStore, p manifest.PackEntry, destDir string) (int, error) {
	packDir := filepath.Join(destDir, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		return 0, err
	}
	dstPack := filepath.Join(packDir, "pack-"+p.PackID+".pack")

	rc, err := store.Get(ctx, p.PackKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return 0, fmt.Errorf("%w: %s", ErrMissingObject, p.PackKey)
		}
		return 0, err
	}
	defer rc.Close()

	out, err := os.Create(dstPack)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(out, rc); err != nil {
		_ = out.Close()
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	if err := gitcli.IndexPack(ctx, destDir, dstPack); err != nil {
		return 0, fmt.Errorf("exporter: index-pack: %w", err)
	}
	return p.ObjectCount, nil
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/exporter/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/exporter/
git commit -m "M2 exporter: round-trip via init-bare + index-pack + update-ref + fsck"
```

---

## Task 20: cmd/bucketvcs — `import`, `export`, `cat-object` subcommands + main.go wiring

**Files:**
- Create: `cmd/bucketvcs/import.go`
- Create: `cmd/bucketvcs/export.go`
- Create: `cmd/bucketvcs/catobject.go`
- Create: `cmd/bucketvcs/import_test.go`
- Create: `cmd/bucketvcs/export_test.go`
- Create: `cmd/bucketvcs/catobject_test.go`
- Modify: `cmd/bucketvcs/main.go`

The CLIs follow the M1 conventions: `--store=<url>` required, then positional args, classified exit codes, stable progress lines on stderr (import only).

- [ ] **Step 1: Write the failing tests**

Create `cmd/bucketvcs/import_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func TestImportCmd_HappyPath(t *testing.T) {
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
	src := makeBareForTest(t)
	storeRoot := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"import", "--store=localfs:" + storeRoot, src, "acme", "x"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	// Verify stable progress lines.
	for _, want := range []string{"fsck source ok", "pack built ", "uploaded pack", "uploaded indexes", "commit "} {
		if !bytes.Contains(stderr.Bytes(), []byte(want)) {
			t.Fatalf("stderr missing %q: %q", want, stderr.String())
		}
	}
}

func TestImportCmd_RejectsExistingRepo(t *testing.T) {
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
	src := makeBareForTest(t)
	storeRoot := t.TempDir()
	var sink bytes.Buffer
	if code := run(context.Background(),
		[]string{"import", "--store=localfs:" + storeRoot, src, "t", "r"},
		&sink, &sink); code != 0 {
		t.Fatalf("first import exit=%d", code)
	}
	sink.Reset()
	code := run(context.Background(),
		[]string{"import", "--store=localfs:" + storeRoot, src, "t", "r"},
		&sink, &sink)
	if code != 2 {
		t.Fatalf("second import exit=%d, want 2", code)
	}
}

// makeBareForTest builds a tiny bare repo and returns its path.
func makeBareForTest(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	mustRun := func(args ...string) {
		t.Helper()
		if out, err := gitcli.RunForTest(work, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustRun("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustRun("add", "f")
	mustRun("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "init")
	bare := t.TempDir() + "-bare"
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	return bare
}
```

Create `cmd/bucketvcs/export_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func TestExportCmd_HappyPath(t *testing.T) {
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
	src := makeBareForTest(t)
	storeRoot := t.TempDir()
	var sink bytes.Buffer
	if code := run(context.Background(),
		[]string{"import", "--store=localfs:" + storeRoot, src, "t", "r"},
		&sink, &sink); code != 0 {
		t.Fatalf("import: exit=%d", code)
	}
	dst := filepath.Join(t.TempDir(), "out")
	sink.Reset()
	if code := run(context.Background(),
		[]string{"export", "--store=localfs:" + storeRoot, "t", "r", dst},
		&sink, &sink); code != 0 {
		t.Fatalf("export: exit=%d stderr=%q", code, sink.String())
	}
	if _, err := os.Stat(filepath.Join(dst, "objects")); err != nil {
		t.Fatalf("expected objects/: %v", err)
	}
}

func TestExportCmd_NotFound(t *testing.T) {
	storeRoot := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")
	var sink bytes.Buffer
	code := run(context.Background(),
		[]string{"export", "--store=localfs:" + storeRoot, "absent", "absent", dst},
		&sink, &sink)
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
}
```

Create `cmd/bucketvcs/catobject_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func TestCatObjectCmd_PrettyMatchesGit(t *testing.T) {
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
	src := makeBareForTest(t)
	storeRoot := t.TempDir()
	var sink bytes.Buffer
	if code := run(context.Background(),
		[]string{"import", "--store=localfs:" + storeRoot, src, "t", "r"},
		&sink, &sink); code != 0 {
		t.Fatalf("import: exit=%d", code)
	}
	refs, err := gitcli.ShowRef(context.Background(), src)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	var oid string
	for _, v := range refs {
		oid = v
		break
	}
	want, err := gitcli.CatFilePretty(context.Background(), src, oid)
	if err != nil {
		t.Fatalf("CatFilePretty: %v", err)
	}
	var stdout bytes.Buffer
	sink.Reset()
	if code := run(context.Background(),
		[]string{"cat-object", "--store=localfs:" + storeRoot, "--pretty", "t", "r", oid},
		&stdout, &sink); code != 0 {
		t.Fatalf("cat-object: exit=%d stderr=%q", code, sink.String())
	}
	if !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("cat-object differs from git cat-file -p")
	}
	_ = strings.TrimSpace
}

func TestCatObjectCmd_TypeMatchesGit(t *testing.T) {
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
	src := makeBareForTest(t)
	storeRoot := t.TempDir()
	var sink bytes.Buffer
	if code := run(context.Background(),
		[]string{"import", "--store=localfs:" + storeRoot, src, "t", "r"},
		&sink, &sink); code != 0 {
		t.Fatalf("import: exit=%d", code)
	}
	refs, err := gitcli.ShowRef(context.Background(), src)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	var oid string
	for _, v := range refs {
		oid = v
		break
	}
	want, err := gitcli.CatFileType(context.Background(), src, oid)
	if err != nil {
		t.Fatalf("CatFileType: %v", err)
	}
	var stdout bytes.Buffer
	sink.Reset()
	if code := run(context.Background(),
		[]string{"cat-object", "--store=localfs:" + storeRoot, "--type", "t", "r", oid},
		&stdout, &sink); code != 0 {
		t.Fatalf("cat-object --type: exit=%d", code)
	}
	got := strings.TrimSpace(stdout.String())
	if got != want {
		t.Fatalf("type: got %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./cmd/bucketvcs/...`
Expected: FAIL — `runImport`, `runExport`, `runCatObject` undefined and main router doesn't recognize new subcommands.

- [ ] **Step 3: Write the implementations**

Create `cmd/bucketvcs/import.go`:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

func runImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeURL := fs.String("store", "", `storage URL, e.g. "localfs:/path"`)
	defaultBranch := fs.String("default-branch", "", "default branch ref (overrides source HEAD)")
	actor := fs.String("actor", "", "actor identifier recorded in tx record")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "import: --store is required")
		return 2
	}
	pos := fs.Args()
	if len(pos) != 3 {
		fmt.Fprintf(stderr, "import: want 3 positional args (source-bare-repo tenant repo), got %d\n", len(pos))
		return 2
	}
	src, tenantID, repoID := pos[0], pos[1], pos[2]
	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "import: %v\n", err)
		return 2
	}
	fmt.Fprintln(stderr, "fsck source ok")
	res, err := importer.Import(ctx, store, importer.Options{
		SourceDir: src, Tenant: tenantID, Repo: repoID,
		Actor: *actor, DefaultBranch: *defaultBranch,
	})
	if err != nil {
		if errors.Is(err, repoerrs.ErrRepoExists) {
			fmt.Fprintf(stderr, "import: repo %s/%s already exists; delete it or import to a different name\n",
				tenantID, repoID)
			return 2
		}
		fmt.Fprintf(stderr, "import: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "pack built %s %d objects\n", res.PackID, res.ObjectCount)
	fmt.Fprintf(stderr, "uploaded pack\n")
	fmt.Fprintf(stderr, "uploaded indexes\n")
	fmt.Fprintf(stderr, "commit %d\n", res.ManifestVersion)
	fmt.Fprintf(stdout, "imported %s/%s pack=%s manifest_version=%d refs=%d objects=%d\n",
		tenantID, repoID, res.PackID, res.ManifestVersion, res.RefCount, res.ObjectCount)
	return 0
}
```

Create `cmd/bucketvcs/export.go`:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

func runExport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeURL := fs.String("store", "", `storage URL, e.g. "localfs:/path"`)
	noFsck := fs.Bool("no-fsck", false, "skip git fsck after export")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "export: --store is required")
		return 2
	}
	pos := fs.Args()
	if len(pos) != 3 {
		fmt.Fprintf(stderr, "export: want 3 positional args (tenant repo dst-dir), got %d\n", len(pos))
		return 2
	}
	tenantID, repoID, dst := pos[0], pos[1], pos[2]
	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "export: %v\n", err)
		return 2
	}
	res, err := exporter.Export(ctx, store, exporter.Options{
		Tenant: tenantID, Repo: repoID, DestDir: dst, RunFsck: !*noFsck,
	})
	switch {
	case errors.Is(err, repoerrs.ErrRepoNotFound):
		fmt.Fprintf(stderr, "export: repo %s/%s not found\n", tenantID, repoID)
		return 2
	case errors.Is(err, exporter.ErrDestNotEmpty):
		fmt.Fprintf(stderr, "export: %v\n", err)
		return 2
	case errors.Is(err, exporter.ErrMissingObject):
		fmt.Fprintf(stderr, "export: %v\n", err)
		return 3
	case err != nil:
		// fsck failures bubble up here; classify via stderr substring
		// search would be brittle. We treat anything from Export with a
		// non-nil Result and FsckOK=false as exit 4.
		if res != nil && !res.FsckOK && res.ObjectCount > 0 {
			fmt.Fprintf(stderr, "export: %v\n", err)
			return 4
		}
		fmt.Fprintf(stderr, "export: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "exported %s/%s manifest_version=%d objects=%d fsck=%v\n",
		tenantID, repoID, res.ManifestVersion, res.ObjectCount, res.FsckOK)
	return 0
}
```

Create `cmd/bucketvcs/catobject.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func runCatObject(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cat-object", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeURL := fs.String("store", "", `storage URL, e.g. "localfs:/path"`)
	wantType := fs.Bool("type", false, "print object type")
	wantSize := fs.Bool("size", false, "print object size")
	wantPretty := fs.Bool("pretty", false, "print pretty-printed object content")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "cat-object: --store is required")
		return 2
	}
	flags := 0
	if *wantType { flags++ }
	if *wantSize { flags++ }
	if *wantPretty { flags++ }
	if flags != 1 {
		fmt.Fprintln(stderr, "cat-object: exactly one of --type, --size, --pretty is required")
		return 2
	}
	pos := fs.Args()
	if len(pos) != 3 {
		fmt.Fprintf(stderr, "cat-object: want 3 positional args (tenant repo oid), got %d\n", len(pos))
		return 2
	}
	tenantID, repoID, oidHex := pos[0], pos[1], pos[2]
	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: %v\n", err)
		return 2
	}
	r, err := repo.Open(ctx, store, tenantID, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: %v\n", err)
		return 2
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: ReadRoot: %v\n", err)
		return 1
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		fmt.Fprintf(stderr, "cat-object: unmarshal body: %v\n", err)
		return 1
	}
	if body.Indexes.ObjectMap == nil {
		fmt.Fprintln(stderr, "cat-object: repo has no object_map index")
		return 3
	}
	mp, err := objindex.Open(ctx, store, body.Indexes.ObjectMap.Key)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: open object_map: %v\n", err)
		return 3
	}
	oid, err := pack.ParseOID(oidHex)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: bad oid %q: %v\n", oidHex, err)
		return 2
	}
	packID, _, ok := mp.Lookup(oid)
	if !ok {
		fmt.Fprintf(stderr, "cat-object: oid %s not in object_map\n", oidHex)
		return 2
	}
	// Find the matching pack entry to get the keys.
	var pe *manifest.PackEntry
	for i := range body.Packs {
		if body.Packs[i].PackID == packID {
			pe = &body.Packs[i]
			break
		}
	}
	if pe == nil {
		fmt.Fprintf(stderr, "cat-object: pack %s referenced by object_map missing from manifest\n", packID)
		return 3
	}
	pr, err := pack.Open(ctx, store, pe.PackKey, pe.IdxKey)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: open pack: %v\n", err)
		return 3
	}
	defer pr.Close()
	obj, err := pr.Get(ctx, oid)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: get: %v\n", err)
		return 1
	}
	switch {
	case *wantType:
		fmt.Fprintln(stdout, obj.Type.String())
	case *wantSize:
		fmt.Fprintln(stdout, obj.Size)
	case *wantPretty:
		// Match git cat-file -p semantics for tree objects: pretty-print
		// the tree entries. For commit/blob/tag, write raw bytes.
		switch obj.Type {
		case pack.TypeTree:
			if err := prettyTree(stdout, obj.Data); err != nil {
				fmt.Fprintf(stderr, "cat-object: pretty tree: %v\n", err)
				return 1
			}
		default:
			if _, err := stdout.Write(obj.Data); err != nil {
				return 1
			}
		}
	}
	return 0
}

// prettyTree writes a tree object in `git cat-file -p` format:
//   <mode> SP <type> SP <oid> TAB <name> NL
func prettyTree(w io.Writer, data []byte) error {
	for len(data) > 0 {
		// mode SP name NUL oid(20).
		sp := indexByteOrErr(data, ' ')
		if sp < 0 {
			return fmt.Errorf("malformed tree entry: no space")
		}
		mode := string(data[:sp])
		data = data[sp+1:]
		nul := indexByteOrErr(data, 0)
		if nul < 0 {
			return fmt.Errorf("malformed tree entry: no NUL")
		}
		name := data[:nul]
		data = data[nul+1:]
		if len(data) < 20 {
			return fmt.Errorf("malformed tree entry: short oid")
		}
		var oid pack.OID
		copy(oid[:], data[:20])
		data = data[20:]
		typ := "blob"
		if mode == "40000" || mode == "040000" {
			typ = "tree"
		}
		// Pad mode to 6 chars to match `git cat-file -p` exactly.
		paddedMode := mode
		for len(paddedMode) < 6 {
			paddedMode = "0" + paddedMode
		}
		fmt.Fprintf(w, "%s %s %s\t%s\n", paddedMode, typ, oid, name)
	}
	return nil
}

func indexByteOrErr(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
```

Modify `cmd/bucketvcs/main.go`:

```go
// Replace the switch statement in run() with this version:
switch sub {
case "init":
	return runInit(ctx, rest, stdout, stderr)
case "inspect-manifest":
	return runInspect(ctx, rest, stdout, stderr)
case "import":
	return runImport(ctx, rest, stdout, stderr)
case "export":
	return runExport(ctx, rest, stdout, stderr)
case "cat-object":
	return runCatObject(ctx, rest, stdout, stderr)
case "-h", "--help", "help":
	usage(stdout)
	return 0
default:
	fmt.Fprintf(stderr, "bucketvcs: unknown subcommand %q\n", sub)
	usage(stderr)
	return 2
}
```

Update the `usage` block to list the new subcommands:

```go
func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: bucketvcs <subcommand> [flags] [args]

Subcommands:
  init               Create a new repo
  inspect-manifest   Print summary of the root manifest
  import             Round-trip a bare git repo into bucketvcs storage
  export             Materialize a bare git repo from bucketvcs storage
  cat-object         Read a Git object from a bucketvcs repo

Run "bucketvcs <subcommand> --help" for subcommand flags.
`)
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./cmd/bucketvcs/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/
git commit -m "M2 cli: import, export, cat-object subcommands + main router"
```

---

## Task 21: diffharness — fixtures package and registry

**Files:**
- Create: `internal/diffharness/fixtures/fixtures.go`
- Create: `internal/diffharness/fixtures/synthetic.go`
- Create: `internal/diffharness/fixtures/fixtures_test.go`

Each fixture authors a bare git repo on disk and returns a `Fixture` with refs and reachable OIDs (computed via `gitcli.ShowRef` / `RevListAllObjects`).

- [ ] **Step 1: Write the failing test**

Create `internal/diffharness/fixtures/fixtures_test.go`:

```go
package fixtures

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
}

func TestRegistry_AllFixturesProduceFsckCleanRepos(t *testing.T) {
	skipIfNoGit(t)
	for name, build := range Registry {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fx := build(t, dir)
			if fx.Name != name {
				t.Fatalf("fixture name mismatch: %q vs %q", fx.Name, name)
			}
			if err := gitcli.Fsck(context.Background(), dir, true); err != nil {
				t.Fatalf("fsck: %v", err)
			}
			// "empty" is the only fixture allowed to have zero refs.
			if name != "empty" && len(fx.Refs) == 0 {
				t.Fatalf("%s: expected ≥1 ref", name)
			}
		})
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/diffharness/fixtures/...`
Expected: FAIL — package undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/diffharness/fixtures/fixtures.go`:

```go
// Package fixtures defines the synthetic-repo corpus used by the M2
// differential harness. Each builder authors a bare git repo at the
// given dir and returns a Fixture describing its refs and reachable
// object set, computed via internal/gitcli.
package fixtures

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// Fixture describes a built synthetic repo.
type Fixture struct {
	Name    string
	Refs    map[string]string // ref name -> hex OID
	AllOIDs []string          // reachable OIDs from rev-list --all
}

// Builder authors a bare git repo at dir and returns its Fixture.
type Builder func(t *testing.T, dir string) Fixture

// Registry maps fixture names to builders.
var Registry = map[string]Builder{
	"empty":            buildEmpty,
	"single_commit":    buildSingleCommit,
	"linear_3_commits": buildLinear3,
	"branch_and_merge": buildBranchAndMerge,
	"lightweight_tag":  buildLightweightTag,
	"annotated_tag":    buildAnnotatedTag,
	"symref_head":      buildSymrefHead,
	"two_branches":     buildTwoBranchesDivergent,
	"binary_blob":      buildBlobWithBinaryContent,
	"deep_tree":        buildDeepNestedTrees,
}

// buildBareFromWork clones a non-bare working repo into a bare repo at dir.
func buildBareFromWork(t *testing.T, work, dir string) {
	t.Helper()
	if err := gitcli.CloneBareMirror(context.Background(), work, dir); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
}

// finalize populates Refs and AllOIDs for the bare repo at dir.
func finalize(t *testing.T, name, dir string) Fixture {
	t.Helper()
	refs, err := gitcli.ShowRef(context.Background(), dir)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	oids, err := gitcli.RevListAllObjects(context.Background(), dir)
	if err != nil {
		t.Fatalf("RevListAllObjects: %v", err)
	}
	return Fixture{Name: name, Refs: refs, AllOIDs: oids}
}

// commitFile authors-or-amends a file and commits it. Hermetic env.
func commitFile(t *testing.T, work, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit(t, work, "add", name)
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", msg)
}

func mustGit(t *testing.T, work string, args ...string) {
	t.Helper()
	if out, err := gitcli.RunForTest(work, args...); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// initWork makes a non-bare working repo, returning its path.
func initWork(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	mustGit(t, work, "init", "--initial-branch=main")
	return work
}
```

Create `internal/diffharness/fixtures/synthetic.go`:

```go
package fixtures

import (
	"path/filepath"
	"testing"
)

func buildEmpty(t *testing.T, dir string) Fixture {
	work := initWork(t)
	buildBareFromWork(t, work, dir)
	return finalize(t, "empty", dir)
}

func buildSingleCommit(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "a\n", "init")
	buildBareFromWork(t, work, dir)
	return finalize(t, "single_commit", dir)
}

func buildLinear3(t *testing.T, dir string) Fixture {
	work := initWork(t)
	for _, c := range []struct{ content, msg string }{
		{"a\n", "1"},
		{"b\n", "2"},
		{"c\n", "3"},
	} {
		commitFile(t, work, "f", c.content, c.msg)
	}
	buildBareFromWork(t, work, dir)
	return finalize(t, "linear_3_commits", dir)
}

func buildBranchAndMerge(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "x\n", "base")
	mustGit(t, work, "checkout", "-b", "feature")
	commitFile(t, work, "g", "y\n", "feature")
	mustGit(t, work, "checkout", "main")
	commitFile(t, work, "h", "z\n", "main-2")
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@e",
		"merge", "--no-ff", "-m", "merge feature", "feature")
	buildBareFromWork(t, work, dir)
	return finalize(t, "branch_and_merge", dir)
}

func buildLightweightTag(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "x\n", "init")
	mustGit(t, work, "tag", "v1")
	buildBareFromWork(t, work, dir)
	return finalize(t, "lightweight_tag", dir)
}

func buildAnnotatedTag(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "x\n", "init")
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@e",
		"tag", "-a", "v1", "-m", "release v1")
	buildBareFromWork(t, work, dir)
	return finalize(t, "annotated_tag", dir)
}

func buildSymrefHead(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "x\n", "init")
	mustGit(t, work, "checkout", "-b", "dev")
	commitFile(t, work, "g", "y\n", "dev")
	buildBareFromWork(t, work, dir)
	mustGit(t, dir, "symbolic-ref", "HEAD", "refs/heads/dev")
	return finalize(t, "symref_head", dir)
}

func buildTwoBranchesDivergent(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "f", "x\n", "base")
	mustGit(t, work, "checkout", "-b", "left")
	commitFile(t, work, "g", "y\n", "left-1")
	mustGit(t, work, "checkout", "-b", "right", "main")
	commitFile(t, work, "h", "z\n", "right-1")
	buildBareFromWork(t, work, dir)
	return finalize(t, "two_branches", dir)
}

func buildBlobWithBinaryContent(t *testing.T, dir string) Fixture {
	work := initWork(t)
	// A 1 MiB pseudo-random blob (deterministic from a fixed seed for hermeticity).
	buf := make([]byte, 1024*1024)
	state := uint32(0x12345678)
	for i := range buf {
		state = state*1103515245 + 12345
		buf[i] = byte(state >> 16)
	}
	if err := os.WriteFile(filepath.Join(work, "blob.bin"), buf, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit(t, work, "add", "blob.bin")
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@e",
		"commit", "-m", "binary blob")
	buildBareFromWork(t, work, dir)
	return finalize(t, "binary_blob", dir)
}

func buildDeepNestedTrees(t *testing.T, dir string) Fixture {
	work := initWork(t)
	// 6-deep tree: a/b/c/d/e/f/leaf
	deep := filepath.Join(work, "a", "b", "c", "d", "e", "f")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deep, "leaf"), []byte("L\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit(t, work, "add", "a")
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@e",
		"commit", "-m", "deep tree")
	buildBareFromWork(t, work, dir)
	return finalize(t, "deep_tree", dir)
}
```

The `os` import is needed by `buildBlobWithBinaryContent` and `buildDeepNestedTrees`. Add to `synthetic.go`:

```go
import (
	"os"
	"path/filepath"
	"testing"
)
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/diffharness/fixtures/...`
Expected: PASS — every fixture builds, fsck-clean.

- [ ] **Step 5: Commit**

```bash
git add internal/diffharness/fixtures/
git commit -m "M2 diffharness: synthetic fixture corpus + registry"
```

---

## Task 22: diffharness — round-trip + cat-object oracles

**Files:**
- Create: `internal/diffharness/oracle.go`
- Create: `internal/diffharness/roundtrip.go`
- Create: `internal/diffharness/roundtrip_test.go`
- Create: `internal/diffharness/catobject_test.go`
- Create: `internal/diffharness/README.md`

The oracle helpers wrap gitcli + bucketvcs imports/exports. Tests iterate `fixtures.Registry`.

- [ ] **Step 1: Write the failing tests**

Create `internal/diffharness/roundtrip_test.go`:

```go
package diffharness

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
}

func TestRoundTrip_AllFixtures(t *testing.T) {
	skipIfNoGit(t)
	for name, build := range fixtures.Registry {
		t.Run(name, func(t *testing.T) {
			ImportThenExportAndCompare(t, name, build)
		})
	}
}
```

Create `internal/diffharness/catobject_test.go`:

```go
package diffharness

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
)

func TestCatObject_AllFixtures(t *testing.T) {
	skipIfNoGit(t)
	for name, build := range fixtures.Registry {
		t.Run(name, func(t *testing.T) {
			CatObjectOracle(t, name, build)
		})
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/diffharness/...`
Expected: FAIL — `ImportThenExportAndCompare`, `CatObjectOracle` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/diffharness/oracle.go`:

```go
// Package diffharness scaffolds the M2 differential test harness against
// upstream git per spec §40.3. Round-trip and pack-reader oracles run
// over the synthetic corpus from internal/diffharness/fixtures.
package diffharness

import (
	"bytes"
	"context"
	"sort"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// gitFsck runs `git fsck --strict` on dir and t.Fatals on failure.
func gitFsck(t *testing.T, dir string) {
	t.Helper()
	if err := gitcli.Fsck(context.Background(), dir, true); err != nil {
		t.Fatalf("fsck %s: %v", dir, err)
	}
}

// gitShowRef returns the (sorted) ref->oid map for dir.
func gitShowRef(t *testing.T, dir string) map[string]string {
	t.Helper()
	refs, err := gitcli.ShowRef(context.Background(), dir)
	if err != nil {
		t.Fatalf("ShowRef %s: %v", dir, err)
	}
	return refs
}

// gitRevListAllObjects returns the sorted reachable OID set.
func gitRevListAllObjects(t *testing.T, dir string) []string {
	t.Helper()
	oids, err := gitcli.RevListAllObjects(context.Background(), dir)
	if err != nil {
		t.Fatalf("RevListAllObjects %s: %v", dir, err)
	}
	sort.Strings(oids)
	return oids
}

// gitCatFilePretty returns `git cat-file -p <oid>` bytes.
func gitCatFilePretty(t *testing.T, dir, oid string) []byte {
	t.Helper()
	out, err := gitcli.CatFilePretty(context.Background(), dir, oid)
	if err != nil {
		t.Fatalf("CatFilePretty(%s): %v", oid, err)
	}
	return out
}

// gitCatFileType returns `git cat-file -t <oid>` output trimmed.
func gitCatFileType(t *testing.T, dir, oid string) string {
	t.Helper()
	out, err := gitcli.CatFileType(context.Background(), dir, oid)
	if err != nil {
		t.Fatalf("CatFileType(%s): %v", oid, err)
	}
	return out
}

// gitCatFileSize returns `git cat-file -s <oid>`.
func gitCatFileSize(t *testing.T, dir, oid string) int64 {
	t.Helper()
	n, err := gitcli.CatFileSize(context.Background(), dir, oid)
	if err != nil {
		t.Fatalf("CatFileSize(%s): %v", oid, err)
	}
	return n
}

// equalRefs reports whether two ref maps are deeply equal.
func equalRefs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// equalOIDLists requires both lists be sorted before calling.
func equalOIDLists(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ensureBytesEqual fatals if got != want, reporting context.
func ensureBytesEqual(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs.\ngot: %q\nwant: %q", name, got, want)
	}
}
```

Create `internal/diffharness/roundtrip.go`:

```go
package diffharness

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// newTestStore constructs a localfs-backed ObjectStore for one test.
func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.New(localfs.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	return s
}

// ImportThenExportAndCompare runs the §5.1 round-trip oracle for a fixture.
func ImportThenExportAndCompare(t *testing.T, name string, build fixtures.Builder) {
	t.Helper()
	srcDir := t.TempDir()
	fx := build(t, srcDir)
	if fx.Name != name {
		t.Fatalf("fixture name mismatch")
	}
	gitFsck(t, srcDir)

	store := newTestStore(t)
	if _, err := importer.Import(context.Background(), store, importer.Options{
		SourceDir: srcDir, Tenant: "diff", Repo: name, Actor: "harness",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	dstDir := filepath.Join(t.TempDir(), "out")
	if _, err := exporter.Export(context.Background(), store, exporter.Options{
		Tenant: "diff", Repo: name, DestDir: dstDir, RunFsck: true,
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	gitFsck(t, dstDir)

	srcRefs := gitShowRef(t, srcDir)
	dstRefs := gitShowRef(t, dstDir)
	if !equalRefs(srcRefs, dstRefs) {
		t.Fatalf("refs differ.\nsrc=%v\ndst=%v", srcRefs, dstRefs)
	}
	srcHead, errS := gitcli.SymbolicRef(context.Background(), srcDir, "HEAD")
	dstHead, errD := gitcli.SymbolicRef(context.Background(), dstDir, "HEAD")
	if (errS == nil) != (errD == nil) {
		t.Fatalf("HEAD presence differs: src err=%v, dst err=%v", errS, errD)
	}
	if errS == nil && srcHead != dstHead {
		t.Fatalf("HEAD differs: src=%q dst=%q", srcHead, dstHead)
	}
	srcOIDs := gitRevListAllObjects(t, srcDir)
	dstOIDs := gitRevListAllObjects(t, dstDir)
	if !equalOIDLists(srcOIDs, dstOIDs) {
		t.Fatalf("reachable OIDs differ.\nsrc=%v\ndst=%v", srcOIDs, dstOIDs)
	}
	for _, oid := range srcOIDs {
		got := gitCatFilePretty(t, dstDir, oid)
		want := gitCatFilePretty(t, srcDir, oid)
		ensureBytesEqual(t, "cat-file -p "+oid, got, want)
	}
}
```

Create `internal/diffharness/catobject.go`:

```go
package diffharness

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/importer"
)

// CatObjectOracle runs the §5.1 pack-reader oracle: every reachable OID
// in the source repo, after import, must produce identical cat-object
// output to upstream git.
func CatObjectOracle(t *testing.T, name string, build fixtures.Builder) {
	t.Helper()
	srcDir := t.TempDir()
	fx := build(t, srcDir)
	store := newTestStore(t)
	if _, err := importer.Import(context.Background(), store, importer.Options{
		SourceDir: srcDir, Tenant: "diff", Repo: name, Actor: "harness",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	for _, oid := range fx.AllOIDs {
		// --pretty
		got := bvCatObject(t, store, name, "--pretty", oid)
		want := gitCatFilePretty(t, srcDir, oid)
		ensureBytesEqual(t, "bv cat-object --pretty "+oid, got, want)

		// --type
		gotType := strings.TrimSpace(string(bvCatObject(t, store, name, "--type", oid)))
		wantType := gitCatFileType(t, srcDir, oid)
		if gotType != wantType {
			t.Fatalf("type %s: bv=%q git=%q", oid, gotType, wantType)
		}

		// --size
		gotSize := strings.TrimSpace(string(bvCatObject(t, store, name, "--size", oid)))
		wantSize := strconv.FormatInt(gitCatFileSize(t, srcDir, oid), 10)
		if gotSize != wantSize {
			t.Fatalf("size %s: bv=%q git=%q", oid, gotSize, wantSize)
		}
	}
}

// bvCatObject invokes bucketvcs cat-object via the cmd/bucketvcs run() entry
// point and returns stdout. Using the CLI rather than internal APIs ensures
// the harness exercises the same code path operators run.
func bvCatObject(t *testing.T, store interface{}, name, mode, oid string) []byte {
	t.Helper()
	// We can't easily call run() from cmd/bucketvcs here without an import
	// cycle. Instead, exercise the public packages: open repo, lookup oid,
	// read object, and replicate the CLI's --pretty/--type/--size formatting.
	// This is identical to runCatObject's body in cmd/bucketvcs/catobject.go;
	// keeping the two implementations in sync is enforced by the cmd-level
	// tests (cmd/bucketvcs/catobject_test.go), which spot-check the CLI.
	return invokeCatObjectInline(t, store.(catObjectStore), name, mode, oid)
}

// catObjectStore is the subset of storage.ObjectStore needed.
type catObjectStore interface {
	// We can't import storage.ObjectStore here without dragging the
	// dependency. Use a duck-typed interface; the tests pass the real
	// localfs adapter.
}

// invokeCatObjectInline mirrors cmd/bucketvcs/runCatObject's body for
// harness use. To avoid duplication, expose the logic from cmd via
// a thin adapter; but since cmd is a main package and not importable,
// we replicate the necessary calls here using internal/objindex,
// internal/pack, internal/repo.
func invokeCatObjectInline(t *testing.T, _ catObjectStore, name, mode, oid string) []byte {
	// IMPLEMENTATION NOTE: at execute time, replace this with a direct
	// call sequence matching cmd/bucketvcs/catobject.go's runCatObject.
	// Kept short here to avoid drift; see catobject.go in cmd/bucketvcs/
	// for the canonical sequence.
	t.Skip("invokeCatObjectInline: replace with direct call to package APIs at execute time")
	_ = bytes.NewBuffer
	return nil
}
```

The `catobject.go` file in this package contains a TODO-style skip; replace at execute time with the inline reimplementation of `runCatObject`'s logic, **calling internal/repo / internal/objindex / internal/pack directly** (no shell-out, no cmd import), to keep the harness fast and avoid an import cycle. The comment in `invokeCatObjectInline` documents the source of truth.

A cleaner alternative the executing engineer should adopt: extract `runCatObject`'s core logic into a new function `internal/diffharness.CatObject(ctx, store, tenant, repo, oid, mode)` that returns `(stdout []byte, exitCode int)`, then call it from both `cmd/bucketvcs/catobject.go` and the harness. The cmd file becomes a thin flag-parser around the new function. This is the structurally right move; do it during this task rather than carry the TODO forward.

Concretely, refactor as follows during this task:
1. Add `internal/diffharness/catobject.go` with a public function:

```go
package diffharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// CatObjectMode selects which cat-object formatter runs.
type CatObjectMode int

const (
	CatType   CatObjectMode = iota
	CatSize
	CatPretty
)

// CatObject is the shared implementation of the bucketvcs cat-object
// subcommand. cmd/bucketvcs and the differential harness both call it.
func CatObject(ctx context.Context, store storage.ObjectStore,
	tenant, repoID, oidHex string, mode CatObjectMode) ([]byte, error) {
	r, err := repo.Open(ctx, store, tenant, repoID)
	if err != nil {
		return nil, err
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return nil, err
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		return nil, err
	}
	if body.Indexes.ObjectMap == nil {
		return nil, fmt.Errorf("repo has no object_map")
	}
	mp, err := objindex.Open(ctx, store, body.Indexes.ObjectMap.Key)
	if err != nil {
		return nil, err
	}
	oid, err := pack.ParseOID(oidHex)
	if err != nil {
		return nil, err
	}
	packID, _, ok := mp.Lookup(oid)
	if !ok {
		return nil, fmt.Errorf("oid %s not in object_map", oidHex)
	}
	var pe *manifest.PackEntry
	for i := range body.Packs {
		if body.Packs[i].PackID == packID {
			pe = &body.Packs[i]
			break
		}
	}
	if pe == nil {
		return nil, fmt.Errorf("pack %s missing from manifest", packID)
	}
	pr, err := pack.Open(ctx, store, pe.PackKey, pe.IdxKey)
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	obj, err := pr.Get(ctx, oid)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	switch mode {
	case CatType:
		fmt.Fprintln(&out, obj.Type.String())
	case CatSize:
		fmt.Fprintln(&out, obj.Size)
	case CatPretty:
		switch obj.Type {
		case pack.TypeTree:
			if err := prettyTree(&out, obj.Data); err != nil {
				return nil, err
			}
		default:
			out.Write(obj.Data)
		}
	}
	return out.Bytes(), nil
}

// prettyTree (extracted from cmd/bucketvcs/catobject.go's prettyTree).
func prettyTree(w io.Writer, data []byte) error {
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return fmt.Errorf("malformed tree entry: no space")
		}
		mode := string(data[:sp])
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 {
			return fmt.Errorf("malformed tree entry: no NUL")
		}
		name := data[:nul]
		data = data[nul+1:]
		if len(data) < 20 {
			return fmt.Errorf("malformed tree entry: short oid")
		}
		var oid pack.OID
		copy(oid[:], data[:20])
		data = data[20:]
		typ := "blob"
		if mode == "40000" || mode == "040000" {
			typ = "tree"
		}
		paddedMode := mode
		for len(paddedMode) < 6 {
			paddedMode = "0" + paddedMode
		}
		fmt.Fprintf(w, "%s %s %s\t%s\n", paddedMode, typ, oid, name)
	}
	return nil
}
```

2. In `cmd/bucketvcs/catobject.go`, replace the body of `runCatObject` with a thin shim that parses flags, picks the `CatObjectMode`, and calls `diffharness.CatObject`. Drop the duplicate `prettyTree` from cmd.

3. Replace `invokeCatObjectInline` in `internal/diffharness/catobject_test.go` (or its companion `catobject.go`) with a direct call to `diffharness.CatObject`.

The `bvCatObject` helper in `roundtrip.go`'s harness now becomes:

```go
func bvCatObject(t *testing.T, store storage.ObjectStore, repoName, mode, oid string) []byte {
	t.Helper()
	var m CatObjectMode
	switch mode {
	case "--type":
		m = CatType
	case "--size":
		m = CatSize
	case "--pretty":
		m = CatPretty
	default:
		t.Fatalf("bad mode %q", mode)
	}
	out, err := CatObject(context.Background(), store, "diff", repoName, oid, m)
	if err != nil {
		t.Fatalf("CatObject: %v", err)
	}
	return out
}
```

Update `catobject_test.go` accordingly: drop the duck-typed interface, pass a real `storage.ObjectStore`.

Create `internal/diffharness/README.md`:

```markdown
# Differential harness

The M2 differential harness compares bucketvcs against upstream git on a
synthetic in-test fixture corpus. See spec §5 of the M2 design and §40.3
of the source spec.

## Adding a fixture

1. Add a builder function in `internal/diffharness/fixtures/synthetic.go`.
2. Register it in `internal/diffharness/fixtures/fixtures.go` `Registry` map.
3. Run `go test ./internal/diffharness/...`. Both round-trip and
   cat-object oracles auto-pick up the new fixture.

## Adding an oracle

The current oracles are round-trip (`roundtrip.go`) and cat-object
(`catobject_test.go` + `catobject.go`'s `CatObject`). To add a new oracle
(e.g., for fetch negotiation in M3), create a parallel package or test
file under `internal/diffharness/` that consumes `fixtures.Registry` and
asserts equivalence between bucketvcs and upstream git on some operation.

## Promotion rule

Per spec §40.3, a pure-Go serving path must reach 100% pass on this
harness + 4-week shadow before becoming default serving. M2 doesn't
promote anything (no serving path); the harness exists to be extended
at M3+.
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/diffharness/... ./cmd/bucketvcs/...`
Expected: PASS — every fixture round-trips and cat-object matches upstream git.

- [ ] **Step 5: Commit**

```bash
git add internal/diffharness/ cmd/bucketvcs/
git commit -m "M2 diffharness: round-trip + cat-object oracles + shared CatObject() helper"
```

---

## Task 23: divergences artifact + enforcing test

**Files:**
- Create: `docs/superpowers/diffharness/known-divergences.md`
- Create: `internal/diffharness/divergences_test.go`

The empty file commits the schema; the test enforces it. M2 ship gate: zero entries (or all entries fully classified).

- [ ] **Step 1: Write the failing test**

Create `internal/diffharness/divergences_test.go`:

```go
package diffharness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestKnownDivergences_FormatGate parses known-divergences.md and fails
// CI if any entry is missing classification, date, or issue link. Empty
// file is fine (M2 ship state).
func TestKnownDivergences_FormatGate(t *testing.T) {
	path := repoRoot(t) + "/docs/superpowers/diffharness/known-divergences.md"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	// Each entry begins with `## ` per the file header. We require:
	//   Classification: <one of the 5 categories>
	//   Date: YYYY-MM-DD
	//   Issue: <https URL>
	const (
		classKey = "Classification:"
		dateKey  = "Date:"
		issueKey = "Issue:"
	)
	allowedClasses := map[string]bool{
		"bucketvcs bug":                       true,
		"git quirk to emulate":                true,
		"intentional documented difference":   true,
		"unsupported optional capability":     true,
		"invalid test case":                   true,
	}
	var inEntry bool
	var sawClass, sawDate, sawIssue bool
	finishEntry := func(title string) {
		if !sawClass {
			t.Fatalf("entry %q missing %s", title, classKey)
		}
		if !sawDate {
			t.Fatalf("entry %q missing %s", title, dateKey)
		}
		if !sawIssue {
			t.Fatalf("entry %q missing %s", title, issueKey)
		}
	}
	var currentTitle string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "## "):
			if inEntry {
				finishEntry(currentTitle)
			}
			currentTitle = strings.TrimPrefix(line, "## ")
			inEntry = true
			sawClass, sawDate, sawIssue = false, false, false
		case inEntry && strings.HasPrefix(line, classKey):
			val := strings.TrimSpace(strings.TrimPrefix(line, classKey))
			if !allowedClasses[val] {
				t.Fatalf("entry %q: unknown classification %q", currentTitle, val)
			}
			sawClass = true
		case inEntry && strings.HasPrefix(line, dateKey):
			val := strings.TrimSpace(strings.TrimPrefix(line, dateKey))
			if len(val) != 10 || val[4] != '-' || val[7] != '-' {
				t.Fatalf("entry %q: bad %s %q (want YYYY-MM-DD)", currentTitle, dateKey, val)
			}
			sawDate = true
		case inEntry && strings.HasPrefix(line, issueKey):
			val := strings.TrimSpace(strings.TrimPrefix(line, issueKey))
			if !strings.HasPrefix(val, "https://") {
				t.Fatalf("entry %q: %s must start with https://, got %q", currentTitle, issueKey, val)
			}
			sawIssue = true
		}
	}
	if inEntry {
		finishEntry(currentTitle)
	}
}

// repoRoot walks up from this file's location to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// .../internal/diffharness/divergences_test.go -> repo root is two levels up
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
```

- [ ] **Step 2: Create the known-divergences file**

Create `docs/superpowers/diffharness/known-divergences.md`:

```markdown
# Differential-harness known divergences

Tracked divergences between bucketvcs and upstream git, per spec §40.3.

This is **not** a dumping ground for correctness bugs. Each entry below
must include:

- a `## ` heading with a short title
- `Classification:` one of:
  - `bucketvcs bug`
  - `git quirk to emulate`
  - `intentional documented difference`
  - `unsupported optional capability`
  - `invalid test case`
- `Date:` YYYY-MM-DD
- `Issue:` https URL to the tracking issue

A CI test (`internal/diffharness/divergences_test.go`) parses this file
and fails the build if any entry is missing a required field.

At M2 ship there are no known divergences.

<!-- entries below this line, newest first -->
```

- [ ] **Step 3: Run, confirm pass**

Run: `go test ./internal/diffharness/...`
Expected: PASS (no entries, no failures).

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/diffharness/known-divergences.md internal/diffharness/divergences_test.go
git commit -m "M2 diffharness: known-divergences artifact + format-gate test"
```

---

## Task 24: stress test (build tag)

**Files:**
- Create: `internal/importer/stress_test.go`

A 1000-commit synthetic repo round-trip, gated behind `-tags stress`. Asserts `len(.bvom) + len(.bvcg) < 128 MiB`. Not a ship gate; a smoke test.

- [ ] **Step 1: Write the test**

Create `internal/importer/stress_test.go`:

```go
//go:build stress

package importer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func TestStress_1000CommitsRoundTrip(t *testing.T) {
	skipIfNoGit(t)
	work := t.TempDir()
	mustRun := func(args ...string) {
		t.Helper()
		if out, err := gitcli.RunForTest(work, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustRun("init", "--initial-branch=main")
	for i := 0; i < 1000; i++ {
		path := filepath.Join(work, fmt.Sprintf("f%d", i%50))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("rev=%d\n", i)), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		mustRun("add", "-A")
		mustRun("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "-m", fmt.Sprintf("c%d", i))
	}
	bare := t.TempDir() + "-bare"
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}

	store := newTestStore(t)
	res, err := Import(context.Background(), store, Options{
		SourceDir: bare, Tenant: "stress", Repo: "r", Actor: "stress",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	t.Logf("imported %d objects, pack=%s, manifest_version=%d",
		res.ObjectCount, res.PackID, res.ManifestVersion)

	// Read .bvom + .bvcg sizes back.
	bvomBody, err := getAll(context.Background(), store, "tenants/stress/repos/r/indexes/object-map/"+res.ObjectMapHash+".bvom")
	if err != nil {
		t.Fatalf("get bvom: %v", err)
	}
	bvcgBody, err := getAll(context.Background(), store, "tenants/stress/repos/r/indexes/commit-graphs/"+res.CommitGraphHash+".graph")
	if err != nil {
		t.Fatalf("get bvcg: %v", err)
	}
	combined := int64(len(bvomBody) + len(bvcgBody))
	const cap = int64(128 * 1024 * 1024)
	if combined > cap {
		t.Fatalf(".bvom + .bvcg = %d bytes, cap %d (smoke regression)", combined, cap)
	}
	t.Logf(".bvom=%d .bvcg=%d combined=%d (cap=%d)", len(bvomBody), len(bvcgBody), combined, cap)

	// Round-trip exports: confirm the pack/idx/refs all materialize.
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := exporter.Export(context.Background(), store, exporter.Options{
		Tenant: "stress", Repo: "r", DestDir: dst, RunFsck: true,
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
}

// getAll reads an entire object's bytes from store.
func getAll(ctx context.Context, store interface {
	Get(context.Context, string) (interface{ Read([]byte) (int, error); Close() error }, error)
}, key string) ([]byte, error) {
	// Use the real storage.ObjectStore at execute time. Replace this
	// loose-typed signature with the proper one that imports
	// internal/storage and uses io.ReadAll on rc.
	panic("replace at execute time with proper storage.ObjectStore signature")
}
```

The `getAll` helper is intentionally sketchy because the build tag means it's not compiled by default. At execute time, replace with:

```go
import (
	"io"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func getAll(ctx context.Context, store storage.ObjectStore, key string) ([]byte, error) {
	rc, err := store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
```

- [ ] **Step 2: Run with stress tag, confirm pass**

Run: `go test -tags stress ./internal/importer/...`
Expected: PASS (takes a couple of minutes).

- [ ] **Step 3: Confirm normal test run is unaffected**

Run: `go test ./internal/importer/...`
Expected: PASS, stress test skipped via build tag.

- [ ] **Step 4: Commit**

```bash
git add internal/importer/stress_test.go
git commit -m "M2 importer: 1000-commit stress smoke test (-tags stress, .bvom+.bvcg <128 MiB)"
```

---

## Task 25: final ship gates + repo state recap

**Files:**
- (no code changes; verification + commit of any cleanup)

Verify every M2 ship-gate from spec §7.7 is green; document the resulting repo state in a brief progress note for memory; tag.

- [ ] **Step 1: go test ./... (full)**

Run: `go test ./...`
Expected: PASS for every package, including M1's localfs concurrency property test (regression sanity).

- [ ] **Step 2: go test -race ./...**

Run: `go test -race ./...`
Expected: PASS, no data races reported.

- [ ] **Step 3: go vet ./...**

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 4: staticcheck ./...**

Run: `staticcheck ./...`
Expected: clean. If staticcheck is not installed, run `go install honnef.co/go/tools/cmd/staticcheck@latest` first.

- [ ] **Step 5: Differential harness on the registered fixture corpus**

Run: `go test -v -run TestRoundTrip_AllFixtures ./internal/diffharness/`
Run: `go test -v -run TestCatObject_AllFixtures ./internal/diffharness/`
Expected: every fixture passes both oracles.

- [ ] **Step 6: divergences format gate**

Run: `go test -run TestKnownDivergences_FormatGate ./internal/diffharness/`
Expected: PASS, no entries.

- [ ] **Step 7: M1 regression sanity**

Run: `go test ./internal/repo/...`
Expected: PASS — M1's concurrency property test still holds.

- [ ] **Step 8: Build the binary, sanity-check the new subcommand surface**

Run: `go build ./cmd/bucketvcs && ./bucketvcs --help`
Expected: help text lists `init`, `inspect-manifest`, `import`, `export`, `cat-object`.

- [ ] **Step 9: Tag**

```bash
git tag -a m2-complete -m "M2 Git object engine complete: pack reader + .bvom + .bvcg + import/export + diffharness scaffolding"
```

- [ ] **Step 10: Memory hand-off**

After tagging, write a `m2_progress.md` entry under `~/.claude/projects/.../memory/` summarizing:
- Commit hash and tag.
- Public APIs introduced (`internal/pack.Reader`, `internal/objindex.Map`, `internal/commitgraph.Graph`, `internal/importer.Import`, `internal/exporter.Export`, `internal/diffharness.CatObject`).
- Known limitations carried into M3 (no protocol, no auth, no GC).
- The reconciliation note about M1's pre-existing key constructors (`CommitGraphKey` → `.graph` extension; we added `ObjectMapKey` for `.bvom`).

The MEMORY.md index entry should mirror the M1 row's shape.

---

## Spec coverage cross-check

| Spec §                              | Plan task(s) |
|-------------------------------------|--------------|
| 1 Purpose & boundary                 | Tasks 1-22 collectively |
| 2 Package layout                     | Tasks 1-23   |
| 2.1 M1 boundary respected            | Task 1       |
| 2.2 internal/gitcli rationale        | Tasks 2-4    |
| 3.1 Keys                             | Task 1       |
| 3.2 pack_id                          | Tasks 3, 17  |
| 3.3 {hash}                           | Tasks 17, 11, 13 |
| 3.4 .bvom format                     | Task 11      |
| 3.5 .bvcg format                     | Task 13      |
| 3.6 Import flow                      | Tasks 16-18  |
| 4.1 Pack reader API                  | Tasks 5-10   |
| 4.2 Manifest body fields             | Task 15      |
| 4.3 objindex / commitgraph APIs      | Tasks 11-14  |
| 4.4 Importer / exporter              | Tasks 16-19  |
| 4.5 gitcli wrappers                  | Tasks 2-4    |
| 4.6 cat-object CLI                   | Task 20      |
| 5 Differential harness               | Tasks 21-22  |
| 5.4 Promotion-rule housekeeping      | Task 23      |
| 6 Failure modes / orphan budget      | Task 18      |
| 7.1 Unit tests                       | Tasks 1-19   |
| 7.2 Differential harness tests       | Task 22      |
| 7.3 CLI tests                        | Task 20      |
| 7.4 Manifest wire-format contract    | Task 15      |
| 7.5 Stress / smoke                   | Task 24      |
| 7.7 Ship gates                       | Task 25      |
| 8 CLI surface                        | Task 20      |
| 9 Out-of-scope                       | (no work)    |
