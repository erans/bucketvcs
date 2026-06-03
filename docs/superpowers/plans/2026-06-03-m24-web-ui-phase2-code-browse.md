# M24 Web UI Phase 2 — Code Browse Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add read-only, GitHub-style git-content browsing (repo home, file tree, blob/raw, commit log, single commit + diff, branch/tag switcher, README render, syntax highlighting) to the M24 web UI.

**Architecture:** A new storage-touching package `internal/gitbrowse` exposes a `Service` that reads git content via the **hybrid** strategy from the design: `refstore` (manifest path, no mirror) for refs/default-branch, and the shared `mirror.Manager` + `git` shell-outs (via new `gitcli` helpers) for tree/blob/log/commit/diff. View-model DTOs and sentinel errors live in a dependency-free leaf package `internal/browsemodel`, imported by both `gitbrowse` and `internal/web`. The web layer declares a `ContentStore` interface (returning `browsemodel` types) that `gitbrowse.Service` satisfies structurally, so it is wired directly at the composition root with **no conversion adapter**. This preserves the Phase 1 rule that `internal/web` never imports the storage/mirror layer — it imports only the leaf DTO package and consumes the interface.

**Tech Stack:** Go 1.26; `html/template` + vendored htmx (existing); `git` CLI via `internal/gitcli`; new deps `github.com/yuin/goldmark` (Markdown), `github.com/microcosm-cc/bluemonday` (HTML sanitization), `github.com/alecthomas/chroma/v2` (syntax highlighting). Module path `github.com/bucketvcs/bucketvcs`. Work happens on the existing branch `m24-web-ui-phase2`.

**Key API facts (verified against the tree):**
- `repo.Open(ctx, store, tenant, repoID) (*repo.Repo, error)`; sentinel `repo.ErrRepoNotFound`.
- `(*repo.Repo).ReadRoot(ctx) (*repo.RootView, error)`; `RootView.Body` is `json.RawMessage`.
- `manifest.UnmarshalBody(raw []byte) (manifest.Body, error)`; `Body.DefaultBranch string`, `Body.Refs map[string]string`, `Body.RefShards`.
- `keys.NewRepo(tenant, repo string) (*keys.Repo, error)`.
- `refstore.New(ctx, store storage.ObjectStore, k *keys.Repo, body *manifest.Body) (refstore.RefStore, error)`; `(refstore.RefStore).List(ctx) (map[string]string, error)`.
- `mirror.NewManager(rootDir string, store storage.ObjectStore) (*mirror.Manager, error)`; `(*mirror.Manager).Open(ctx, tenant, repo) (*mirror.Mirror, error)`; `(*mirror.Mirror).RLock()/RUnlock()/BareDir()`.
- `gitcli` has unexported `run(ctx, dir string, args ...string) ([]byte, error)` and guard `validRefOrOID(s string) bool`; existing `CatFileType`/`CatFileSize` accept a `<oid>:<path>` rev string.
- web: `NewHandler(Deps) http.Handler`; `Deps{Store DataStore; Logger; Limiter; UIDir; SessionTTL; TrustProxy; OIDC}`; `server` struct; `(*server).render.render(w, page string, data any)`; `(*server).renderError(w,r,code,msg)`; `SessionFromContext(ctx)`; `actorFromSession(*auth.Session) *auth.Actor`; `issueCSRF(w, secure) string`; `requestIsTLS(r, trustProxy) bool`; `EmitRequestMetric(ctx, logger, route, status)`; render-data structs embed `base{Session,CSRF}`; each page template ends with `{{template "base" .}}` and is rendered via `ExecuteTemplate(w, page, data)`.
- Test fixture pattern (from `internal/mirror/mirror_test.go`): build a bare repo with the `git` CLI, then `importer.Import(ctx, store, importer.Options{SourceDir, Tenant, Repo, Actor, DefaultBranch})` into a `localfs` store.

**Conventions for every task:** run `go test ./<pkg>/...` for the touched package; `go vet ./...` before each commit; commit messages use the repo's prefix style (`feat(web): …`, `feat(gitbrowse): …`, `chore(deps): …`) and end with the `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>` trailer. Tests that shell out to `git` start with `if testing.Short() { t.Skip("requires git binary") }`.

---

### Task 1: Add dependencies + `internal/browsemodel` leaf package

**Files:**
- Modify: `go.mod`, `go.sum` (via `go get`)
- Create: `internal/browsemodel/model.go`
- Create: `internal/browsemodel/model_test.go`

- [ ] **Step 1: Add the three dependencies**

Run:
```bash
go get github.com/yuin/goldmark@latest
go get github.com/microcosm-cc/bluemonday@latest
go get github.com/alecthomas/chroma/v2@latest
```
Expected: `go.mod`/`go.sum` updated; no build errors. (These are pulled into real use in Tasks 14 & 16; adding them now keeps the dependency change isolated and reviewable.)

- [ ] **Step 2: Write the failing test**

Create `internal/browsemodel/model_test.go`:
```go
package browsemodel

import (
	"errors"
	"fmt"
	"testing"
)

func TestSentinelsAreDistinct(t *testing.T) {
	if errors.Is(ErrNotFound, ErrWarming) || errors.Is(ErrWarming, ErrNotFound) {
		t.Fatal("ErrNotFound and ErrWarming must be distinct sentinels")
	}
	wrapped := fmt.Errorf("read tree: %w", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Fatal("wrapped ErrNotFound must satisfy errors.Is")
	}
}

func TestRefsZeroValueIsSafe(t *testing.T) {
	var r Refs
	if len(r.Branches) != 0 || len(r.Tags) != 0 || r.Default != "" {
		t.Fatal("zero Refs should be empty")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/browsemodel/...`
Expected: FAIL — package/identifiers undefined.

- [ ] **Step 4: Write the model**

Create `internal/browsemodel/model.go`:
```go
// Package browsemodel holds the view-model DTOs and sentinel errors shared by
// internal/gitbrowse (the storage-touching producer) and internal/web (the
// consumer). It deliberately imports nothing beyond the standard library so the
// web layer can depend on it without pulling in the storage/mirror packages,
// preserving the Phase 1 decoupling rule.
package browsemodel

import "errors"

// Sentinel errors crossing the ContentStore boundary.
var (
	// ErrNotFound means a repo, ref, path, or object does not exist. The web
	// layer maps it to HTTP 404.
	ErrNotFound = errors.New("browsemodel: not found")
	// ErrWarming means the on-disk mirror is still materializing and exceeded
	// the browse timeout. The web layer maps it to HTTP 503.
	ErrWarming = errors.New("browsemodel: repository warming up")
)

// RefInfo is a single branch or tag with its resolved commit OID.
type RefInfo struct {
	Name string // short name, e.g. "main" or "feature/foo" (no refs/heads/ prefix)
	OID  string // 40-hex commit OID
}

// Refs is the set of branches and tags plus the repo's default branch name.
type Refs struct {
	Default  string // short default-branch name, e.g. "main"; "" for an empty repo
	Branches []RefInfo
	Tags     []RefInfo
}

// Resolved is the outcome of splitting a URL remainder into a ref (or raw OID)
// and a path. Ref is the display name echoed in links/switcher; OID is the
// stable 40-hex handle used for content reads.
type Resolved struct {
	Ref  string // display ref name, or "" when the URL used a raw OID
	OID  string // 40-hex commit OID the ref/OID resolved to
	Path string // repo-relative path after the ref segment (no leading slash)
}

// TreeEntry is one row in a directory listing.
type TreeEntry struct {
	Name string // basename
	Path string // full repo-relative path
	Mode string // git mode, e.g. "100644"
	Type string // "tree" | "blob" | "commit" (submodule/gitlink)
	Size int64  // blob size in bytes; 0 for trees/commits
	OID  string // 40-hex object OID
}

// Blob is a file's content plus rendering hints.
type Blob struct {
	Path     string
	OID      string
	Size     int64
	Binary   bool   // contains a NUL byte in the first 8 KiB
	TooLarge bool   // size exceeds the hard read cap; Bytes is nil
	Bytes    []byte // nil when Binary or TooLarge
}

// CommitMeta is the summary form used in logs and as the header of a commit view.
type CommitMeta struct {
	OID         string
	ShortOID    string // first 12 hex chars
	Summary     string // first line of the message
	AuthorName  string
	AuthorEmail string
	AuthorTime  int64 // unix seconds
}

// DiffLine is one line within a hunk. Kind is ' ' (context), '+' (added), or
// '-' (removed).
type DiffLine struct {
	Kind byte
	Text string // line content without the leading +/-/space
}

// Hunk is a contiguous @@ ... @@ block.
type Hunk struct {
	Header string // the literal "@@ -a,b +c,d @@ ..." line
	Lines  []DiffLine
}

// FileDiff is the diff for a single file within a commit.
type FileDiff struct {
	OldPath   string
	NewPath   string
	Status    string // "A"|"M"|"D"|"R"|"C"
	Binary    bool
	TooLarge  bool // exceeded the per-file line cap; Hunks is nil
	Additions int
	Deletions int
	Hunks     []Hunk
}

// CommitDetail is a single commit with metadata, message, parents, and diff.
type CommitDetail struct {
	Meta      CommitMeta
	Message   string
	Parents   []string
	Files     []FileDiff
	Truncated bool // diff exceeded the file cap; Files is partial
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/browsemodel/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/browsemodel/
git commit -m "feat(browsemodel): shared DTOs + sentinels for code browse; add goldmark/bluemonday/chroma deps"
```

---

### Task 2: `gitcli` helpers for browse reads

**Files:**
- Modify: `internal/gitcli/gitcli.go`
- Create: `internal/gitcli/browse_test.go`

These follow the existing `gitcli` style: guard caller-supplied strings with `validRefOrOID`, call the unexported `run(ctx, dir, args...)`, and prepend `--no-replace-objects` for object reads (matching `CatFilePretty`).

- [ ] **Step 1: Write the failing test**

Create `internal/gitcli/browse_test.go`:
```go
package gitcli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeBrowseBare builds a bare repo with one commit (a.txt + sub/b.txt) on main
// and returns (bareDir, commitOID).
func makeBrowseBare(t *testing.T) (string, string) {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "r.git")
	mustRun := func(dir string, args ...string) {
		c := exec.Command("git", args...)
		if dir != "" {
			c.Dir = dir
		}
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	mustRun("", "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "sub", "b.txt"), []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(work, "-C", work, "add", ".")
	mustRun(work, "-C", work, "commit", "-q", "-m", "init")
	mustRun("", "clone", "-q", "--bare", work, bare)
	out, err := exec.Command("git", "-C", bare, "rev-parse", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	return bare, strings.TrimSpace(string(out))
}

func TestLsTree_RootAndSub(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, oid := makeBrowseBare(t)
	root, err := LsTree(context.Background(), bare, oid)
	if err != nil {
		t.Fatalf("LsTree root: %v", err)
	}
	if !strings.Contains(string(root), "a.txt") || !strings.Contains(string(root), "sub") {
		t.Fatalf("root listing missing entries: %q", root)
	}
	sub, err := LsTree(context.Background(), bare, oid+":sub")
	if err != nil {
		t.Fatalf("LsTree sub: %v", err)
	}
	if !strings.Contains(string(sub), "b.txt") {
		t.Fatalf("sub listing missing b.txt: %q", sub)
	}
}

func TestCatBlob(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, oid := makeBrowseBare(t)
	b, err := CatBlob(context.Background(), bare, oid+":a.txt")
	if err != nil {
		t.Fatalf("CatBlob: %v", err)
	}
	if string(b) != "hello\n" {
		t.Fatalf("got %q", b)
	}
}

func TestLogRaw_And_CommitObject_And_DiffTreePatch(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, oid := makeBrowseBare(t)
	lg, err := LogRaw(context.Background(), bare, oid, 0, 10)
	if err != nil || !strings.Contains(string(lg), "init") {
		t.Fatalf("LogRaw: %v %q", err, lg)
	}
	co, err := CatFileCommit(context.Background(), bare, oid)
	if err != nil || !strings.Contains(string(co), "author") {
		t.Fatalf("CatFileCommit: %v %q", err, co)
	}
	d, err := DiffTreePatch(context.Background(), bare, oid)
	if err != nil || !strings.Contains(string(d), "a.txt") {
		t.Fatalf("DiffTreePatch: %v %q", err, d)
	}
}

func TestBrowseHelpers_RejectFlagLikeArgs(t *testing.T) {
	_, err := LsTree(context.Background(), "/tmp", "--upload-pack=evil")
	if err == nil {
		t.Fatal("expected rejection of flag-like treeish")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitcli/ -run 'LsTree|CatBlob|LogRaw|CommitObject|DiffTreePatch|BrowseHelpers'`
Expected: FAIL — `LsTree`/`CatBlob`/`LogRaw`/`CatFileCommit`/`DiffTreePatch` undefined.

- [ ] **Step 3: Add the helpers**

Append to `internal/gitcli/gitcli.go` (after `DiffTreeChangedPaths`):
```go
// LsTree returns the raw `git ls-tree --long -z <treeish>` output for a tree-ish.
// treeish is typically "<commitOID>" for the root tree or "<commitOID>:<dir>" for
// a subdirectory. Output is NUL-terminated records, each:
//   "<mode> SP <type> SP <oid> SP <size|-> TAB <name>" \0
func LsTree(ctx context.Context, dir, treeish string) ([]byte, error) {
	if !validRefOrOID(treeish) {
		return nil, fmt.Errorf("gitcli: LsTree: invalid treeish %q", treeish)
	}
	return run(ctx, dir, "--no-replace-objects", "ls-tree", "--long", "-z", treeish)
}

// CatBlob returns raw blob bytes for a rev, matching `git cat-file blob <rev>`.
// rev is typically "<commitOID>:<path>".
func CatBlob(ctx context.Context, dir, rev string) ([]byte, error) {
	if !validRefOrOID(rev) {
		return nil, fmt.Errorf("gitcli: CatBlob: invalid rev %q", rev)
	}
	return run(ctx, dir, "--no-replace-objects", "cat-file", "blob", rev)
}

// LogRaw returns commit-log records for rev, paginated by skip/max. Each record
// is unit-separated (0x1f) fields terminated by a record separator (0x1e):
//   <full-oid> 0x1f <author-name> 0x1f <author-email> 0x1f <author-unixtime> 0x1f <subject> 0x1e
func LogRaw(ctx context.Context, dir, rev string, skip, max int) ([]byte, error) {
	if !validRefOrOID(rev) {
		return nil, fmt.Errorf("gitcli: LogRaw: invalid rev %q", rev)
	}
	if skip < 0 || max <= 0 {
		return nil, fmt.Errorf("gitcli: LogRaw: bad skip/max %d/%d", skip, max)
	}
	const format = "--pretty=format:%H%x1f%an%x1f%ae%x1f%at%x1f%s%x1e"
	return run(ctx, dir, "--no-replace-objects", "log", rev,
		fmt.Sprintf("--skip=%d", skip), fmt.Sprintf("--max-count=%d", max),
		"--no-color", format)
}

// CatFileCommit returns the raw commit object bytes, matching
// `git cat-file commit <oid>` (headers: tree/parent/author/committer, blank line,
// then the message).
func CatFileCommit(ctx context.Context, dir, oid string) ([]byte, error) {
	if !validRefOrOID(oid) {
		return nil, fmt.Errorf("gitcli: CatFileCommit: invalid oid %q", oid)
	}
	return run(ctx, dir, "--no-replace-objects", "cat-file", "commit", oid)
}

// DiffTreePatch returns the unified patch for a commit against its first parent
// (or the empty tree for a root commit, via --root), with rename detection (-M).
func DiffTreePatch(ctx context.Context, dir, oid string) ([]byte, error) {
	if !validRefOrOID(oid) {
		return nil, fmt.Errorf("gitcli: DiffTreePatch: invalid oid %q", oid)
	}
	return run(ctx, dir, "--no-replace-objects", "diff-tree", "-p", "-M",
		"--root", "--no-color", oid)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gitcli/ -run 'LsTree|CatBlob|LogRaw|CommitObject|DiffTreePatch|BrowseHelpers'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitcli/gitcli.go internal/gitcli/browse_test.go
git commit -m "feat(gitcli): add LsTree/CatBlob/LogRaw/CatFileCommit/DiffTreePatch browse helpers"
```

---

### Task 3: `gitbrowse.Service` skeleton + `openMirror` + shared test fixture

**Files:**
- Create: `internal/gitbrowse/service.go`
- Create: `internal/gitbrowse/fixture_test.go`
- Create: `internal/gitbrowse/service_test.go`

- [ ] **Step 1: Write the shared fixture helper (test support)**

Create `internal/gitbrowse/fixture_test.go`:
```go
package gitbrowse

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// fixture imports a synthetic repo into a localfs store and returns a ready
// Service plus (tenant, repo) and a map of useful OIDs. The repo has:
//   - branch main: a.txt, README.md, sub/b.txt, bin.dat (binary)
//   - branch feature/foo: adds c.txt (tests slash-ref disambiguation)
//   - tag v1.0 on main's first commit
//   - two commits on main (so log/diff have content)
func fixture(t *testing.T) (svc *Service, tenant, repo string, oids map[string]string) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires git binary")
	}
	work := t.TempDir()
	srcBare := filepath.Join(t.TempDir(), "src.git")
	oids = map[string]string{}

	git := func(dir string, args ...string) string {
		c := exec.Command("git", args...)
		if dir != "" {
			c.Dir = dir
		}
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ann", "GIT_AUTHOR_EMAIL=ann@x",
			"GIT_COMMITTER_NAME=Ann", "GIT_COMMITTER_EMAIL=ann@x")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, content string) {
		p := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("", "init", "-q", "-b", "main", work)
	write("a.txt", "hello\n")
	write("README.md", "# Demo\n\nHello *world* & <b>safe</b>.\n")
	write("sub/b.txt", "world\n")
	if err := os.WriteFile(filepath.Join(work, "bin.dat"), []byte{0x00, 0x01, 0x02, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	git(work, "-C", work, "add", ".")
	git(work, "-C", work, "commit", "-q", "-m", "init")
	oids["c1"] = git(work, "-C", work, "rev-parse", "HEAD")
	git(work, "-C", work, "tag", "v1.0")

	write("a.txt", "hello again\n")
	git(work, "-C", work, "add", ".")
	git(work, "-C", work, "commit", "-q", "-m", "update a")
	oids["c2"] = git(work, "-C", work, "rev-parse", "HEAD")

	git(work, "-C", work, "checkout", "-q", "-b", "feature/foo")
	write("c.txt", "branch file\n")
	git(work, "-C", work, "add", ".")
	git(work, "-C", work, "commit", "-q", "-m", "add c on branch")
	oids["feat"] = git(work, "-C", work, "rev-parse", "HEAD")
	git(work, "-C", work, "checkout", "-q", "main")

	git("", "clone", "-q", "--bare", work, srcBare)

	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := importer.Import(context.Background(), store, importer.Options{
		SourceDir:     srcBare,
		Tenant:        "acme",
		Repo:          "demo",
		Actor:         "test",
		DefaultBranch: "refs/heads/main",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	mgr, err := mirror.NewManager(t.TempDir(), store)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	return NewService(store, mgr, 0), "acme", "demo", oids
}
```

- [ ] **Step 2: Write the failing test**

Create `internal/gitbrowse/service_test.go`:
```go
package gitbrowse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

func TestOpenMirror_ReturnsBareDir(t *testing.T) {
	svc, tenant, repo, _ := fixture(t)
	m, release, err := svc.openMirror(context.Background(), tenant, repo)
	if err != nil {
		t.Fatalf("openMirror: %v", err)
	}
	defer release()
	if m.BareDir() == "" {
		t.Fatal("empty bare dir")
	}
}

func TestOpenMirror_TimeoutIsWarming(t *testing.T) {
	svc, tenant, repo, _ := fixture(t)
	svc.timeout = 1 * time.Nanosecond // force the cold-open deadline to blow
	_, _, err := svc.openMirror(context.Background(), tenant, repo)
	if !errors.Is(err, browsemodel.ErrWarming) {
		t.Fatalf("want ErrWarming, got %v", err)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/gitbrowse/ -run OpenMirror`
Expected: FAIL — `Service`/`NewService`/`openMirror` undefined.

- [ ] **Step 4: Write the service**

Create `internal/gitbrowse/service.go`:
```go
// Package gitbrowse implements read-only git-content browsing for the web UI,
// using the hybrid strategy: refstore (manifest path, no mirror) for refs, and
// the shared mirror.Manager + git shell-outs for tree/blob/log/commit/diff.
// It returns browsemodel DTOs so it structurally satisfies web.ContentStore.
package gitbrowse

import (
	"context"
	"errors"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// DefaultTimeout bounds synchronous cold-mirror materialization per request.
const DefaultTimeout = 20 * time.Second

// Service reads git content for a tenant/repo.
type Service struct {
	store   storage.ObjectStore
	mgr     *mirror.Manager
	timeout time.Duration
}

// NewService constructs a Service. timeout <= 0 uses DefaultTimeout.
func NewService(store storage.ObjectStore, mgr *mirror.Manager, timeout time.Duration) *Service {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Service{store: store, mgr: mgr, timeout: timeout}
}

// openMirror opens (materializing if cold) the bare mirror and takes its read
// lock. The returned release func MUST be called to drop the lock. A
// materialization that exceeds s.timeout is reported as browsemodel.ErrWarming.
func (s *Service) openMirror(ctx context.Context, tenant, repo string) (*mirror.Mirror, func(), error) {
	octx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	m, err := s.mgr.Open(octx, tenant, repo)
	if err != nil {
		if errors.Is(octx.Err(), context.DeadlineExceeded) {
			return nil, nil, browsemodel.ErrWarming
		}
		return nil, nil, err
	}
	m.RLock()
	return m, m.RUnlock, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/gitbrowse/ -run OpenMirror`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/gitbrowse/
git commit -m "feat(gitbrowse): Service skeleton + bounded openMirror (ErrWarming on timeout) + test fixture"
```

---

### Task 4: `gitbrowse.ListRefs` (manifest/refstore path, no mirror)

**Files:**
- Create: `internal/gitbrowse/refs.go`
- Modify: `internal/gitbrowse/service_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/gitbrowse/service_test.go`:
```go
func TestListRefs(t *testing.T) {
	svc, tenant, repo, _ := fixture(t)
	refs, err := svc.ListRefs(context.Background(), tenant, repo)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if refs.Default != "main" {
		t.Fatalf("default = %q, want main", refs.Default)
	}
	names := map[string]bool{}
	for _, b := range refs.Branches {
		names[b.Name] = true
		if len(b.OID) != 40 {
			t.Fatalf("branch %q bad oid %q", b.Name, b.OID)
		}
	}
	if !names["main"] || !names["feature/foo"] {
		t.Fatalf("missing branches: %+v", refs.Branches)
	}
	tagNames := map[string]bool{}
	for _, tg := range refs.Tags {
		tagNames[tg.Name] = true
	}
	if !tagNames["v1.0"] {
		t.Fatalf("missing tag v1.0: %+v", refs.Tags)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitbrowse/ -run TestListRefs`
Expected: FAIL — `ListRefs` undefined.

- [ ] **Step 3: Implement ListRefs**

Create `internal/gitbrowse/refs.go`:
```go
package gitbrowse

import (
	"context"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
)

// loadRefs resolves the full ref map and default-branch name via the manifest
// path (no mirror). Returned map is refname->40-hex OID (e.g. "refs/heads/main").
func (s *Service) loadRefs(ctx context.Context, tenant, repoID string) (refMap map[string]string, defaultBranch string, err error) {
	r, err := repo.Open(ctx, s.store, tenant, repoID)
	if err != nil {
		return nil, "", err
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return nil, "", err
	}
	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		return nil, "", err
	}
	k, err := keys.NewRepo(tenant, repoID)
	if err != nil {
		return nil, "", err
	}
	rs, err := refstore.New(ctx, s.store, k, &body)
	if err != nil {
		return nil, "", err
	}
	m, err := rs.List(ctx)
	if err != nil {
		return nil, "", err
	}
	return m, body.DefaultBranch, nil
}

// ListRefs returns branches, tags, and the default-branch short name.
func (s *Service) ListRefs(ctx context.Context, tenant, repoID string) (browsemodel.Refs, error) {
	m, def, err := s.loadRefs(ctx, tenant, repoID)
	if err != nil {
		return browsemodel.Refs{}, err
	}
	var out browsemodel.Refs
	for name, oid := range m {
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			out.Branches = append(out.Branches, browsemodel.RefInfo{
				Name: strings.TrimPrefix(name, "refs/heads/"), OID: oid,
			})
		case strings.HasPrefix(name, "refs/tags/"):
			out.Tags = append(out.Tags, browsemodel.RefInfo{
				Name: strings.TrimPrefix(name, "refs/tags/"), OID: oid,
			})
		}
	}
	sort.Slice(out.Branches, func(i, j int) bool { return out.Branches[i].Name < out.Branches[j].Name })
	sort.Slice(out.Tags, func(i, j int) bool { return out.Tags[i].Name < out.Tags[j].Name })
	out.Default = defaultBranchName(def, out.Branches)
	return out, nil
}

// defaultBranchName picks the display default branch: the manifest's
// DefaultBranch (stripped) if it is a real branch; else main; else master; else
// the first sorted branch; else "" (empty repo).
func defaultBranchName(manifestDefault string, branches []browsemodel.RefInfo) string {
	has := func(n string) bool {
		for _, b := range branches {
			if b.Name == n {
				return true
			}
		}
		return false
	}
	d := strings.TrimPrefix(manifestDefault, "refs/heads/")
	if d != "" && has(d) {
		return d
	}
	if has("main") {
		return "main"
	}
	if has("master") {
		return "master"
	}
	if len(branches) > 0 {
		return branches[0].Name
	}
	return ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gitbrowse/ -run TestListRefs`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitbrowse/refs.go internal/gitbrowse/service_test.go
git commit -m "feat(gitbrowse): ListRefs via refstore + default-branch detection"
```

---

### Task 5: `gitbrowse.Resolve` (OID + longest-ref-prefix disambiguation)

**Files:**
- Create: `internal/gitbrowse/resolve.go`
- Modify: `internal/gitbrowse/service_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/gitbrowse/service_test.go`:
```go
func TestResolve_SlashRefVsPath(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	ctx := context.Background()

	// "feature/foo/c.txt" must split ref="feature/foo", path="c.txt".
	r, err := svc.Resolve(ctx, tenant, repo, "feature/foo/c.txt")
	if err != nil {
		t.Fatalf("Resolve slash ref: %v", err)
	}
	if r.Ref != "feature/foo" || r.Path != "c.txt" || r.OID != oids["feat"] {
		t.Fatalf("got %+v", r)
	}

	// "main" alone resolves to a ref with empty path.
	r, err = svc.Resolve(ctx, tenant, repo, "main")
	if err != nil || r.Ref != "main" || r.Path != "" || r.OID != oids["c2"] {
		t.Fatalf("Resolve main: %v %+v", err, r)
	}

	// A raw 40-hex OID resolves with empty ref.
	r, err = svc.Resolve(ctx, tenant, repo, oids["c1"]+"/a.txt")
	if err != nil || r.Ref != "" || r.OID != oids["c1"] || r.Path != "a.txt" {
		t.Fatalf("Resolve oid: %v %+v", err, r)
	}

	// Unknown ref → ErrNotFound.
	if _, err := svc.Resolve(ctx, tenant, repo, "nope/x.txt"); err == nil {
		t.Fatal("expected ErrNotFound for unknown ref")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitbrowse/ -run TestResolve`
Expected: FAIL — `Resolve` undefined.

- [ ] **Step 3: Implement Resolve**

Create `internal/gitbrowse/resolve.go`:
```go
package gitbrowse

import (
	"context"
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

// isHex40 reports whether s is exactly 40 lowercase/uppercase hex chars.
func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// Resolve splits a URL remainder ("ref/maybe/path" or "<40hex>/path") into a
// resolved {Ref, OID, Path}. It prefers a leading 40-hex OID; otherwise it picks
// the longest known branch/tag that is a slash-delimited prefix of rest.
func (s *Service) Resolve(ctx context.Context, tenant, repoID, rest string) (browsemodel.Resolved, error) {
	rest = strings.Trim(rest, "/")

	// Raw-OID form: <40hex> optionally followed by "/<path>".
	head := rest
	tail := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		head, tail = rest[:i], rest[i+1:]
	}
	if isHex40(head) {
		return browsemodel.Resolved{Ref: "", OID: head, Path: tail}, nil
	}

	refs, err := s.ListRefs(ctx, tenant, repoID)
	if err != nil {
		return browsemodel.Resolved{}, err
	}
	type cand struct {
		name string
		oid  string
	}
	all := make([]cand, 0, len(refs.Branches)+len(refs.Tags))
	for _, b := range refs.Branches {
		all = append(all, cand{b.Name, b.OID})
	}
	for _, tg := range refs.Tags {
		all = append(all, cand{tg.Name, tg.OID})
	}

	best := cand{}
	for _, c := range all {
		if rest == c.name || strings.HasPrefix(rest, c.name+"/") {
			if len(c.name) > len(best.name) {
				best = c
			}
		}
	}
	if best.name == "" {
		return browsemodel.Resolved{}, fmt.Errorf("resolve %q: %w", rest, browsemodel.ErrNotFound)
	}
	path := strings.TrimPrefix(rest, best.name)
	path = strings.TrimPrefix(path, "/")
	return browsemodel.Resolved{Ref: best.name, OID: best.oid, Path: path}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gitbrowse/ -run TestResolve`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitbrowse/resolve.go internal/gitbrowse/service_test.go
git commit -m "feat(gitbrowse): Resolve ref/path with longest-prefix + raw-OID disambiguation"
```

---

### Task 6: `gitbrowse.ReadTree` (+ `parseLsTree`)

**Files:**
- Create: `internal/gitbrowse/tree.go`
- Create: `internal/gitbrowse/tree_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/gitbrowse/tree_test.go`:
```go
package gitbrowse

import (
	"context"
	"testing"
)

func TestParseLsTree(t *testing.T) {
	// Two NUL-terminated --long records: a blob and a tree.
	raw := "100644 blob aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa      6\ta.txt\x00" +
		"040000 tree bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb       -\tsub\x00"
	entries, err := parseLsTree([]byte(raw), "")
	if err != nil {
		t.Fatalf("parseLsTree: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	// Dirs sort first.
	if entries[0].Name != "sub" || entries[0].Type != "tree" {
		t.Fatalf("entry0 = %+v", entries[0])
	}
	if entries[1].Name != "a.txt" || entries[1].Type != "blob" || entries[1].Size != 6 {
		t.Fatalf("entry1 = %+v", entries[1])
	}
	if entries[1].Path != "a.txt" {
		t.Fatalf("path = %q", entries[1].Path)
	}
}

func TestReadTree_RootAndSub(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	ctx := context.Background()
	root, err := svc.ReadTree(ctx, tenant, repo, oids["c2"], "")
	if err != nil {
		t.Fatalf("ReadTree root: %v", err)
	}
	names := map[string]string{}
	for _, e := range root {
		names[e.Name] = e.Type
	}
	if names["sub"] != "tree" || names["a.txt"] != "blob" || names["bin.dat"] != "blob" {
		t.Fatalf("root entries = %+v", root)
	}
	sub, err := svc.ReadTree(ctx, tenant, repo, oids["c2"], "sub")
	if err != nil {
		t.Fatalf("ReadTree sub: %v", err)
	}
	if len(sub) != 1 || sub[0].Name != "b.txt" || sub[0].Path != "sub/b.txt" {
		t.Fatalf("sub entries = %+v", sub)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitbrowse/ -run 'ParseLsTree|ReadTree'`
Expected: FAIL — `parseLsTree`/`ReadTree` undefined.

- [ ] **Step 3: Implement ReadTree + parseLsTree**

Create `internal/gitbrowse/tree.go`:
```go
package gitbrowse

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// ReadTree lists one directory level at (oid, path). path "" is the root tree.
func (s *Service) ReadTree(ctx context.Context, tenant, repoID, oid, path string) ([]browsemodel.TreeEntry, error) {
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return nil, err
	}
	defer release()

	treeish := oid
	clean := strings.Trim(path, "/")
	if clean != "" {
		treeish = oid + ":" + clean
	}
	out, err := gitcli.LsTree(ctx, m.BareDir(), treeish)
	if err != nil {
		// git exits non-zero for a missing path/oid; treat as not found.
		return nil, fmt.Errorf("ls-tree %q: %w", treeish, browsemodel.ErrNotFound)
	}
	return parseLsTree(out, clean)
}

// parseLsTree parses `git ls-tree --long -z` output. parentPath is the
// directory the listing is relative to ("" for root) and is prefixed onto each
// entry's Path. Entries are sorted directories-first then by name.
func parseLsTree(raw []byte, parentPath string) ([]browsemodel.TreeEntry, error) {
	var entries []browsemodel.TreeEntry
	records := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	for _, rec := range records {
		if rec == "" {
			continue
		}
		// "<mode> <type> <oid> <size|->\t<name>"
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("gitbrowse: malformed ls-tree record %q", rec)
		}
		meta := strings.Fields(rec[:tab])
		name := rec[tab+1:]
		if len(meta) != 4 {
			return nil, fmt.Errorf("gitbrowse: malformed ls-tree meta %q", rec[:tab])
		}
		var size int64
		if meta[3] != "-" {
			n, err := strconv.ParseInt(meta[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("gitbrowse: ls-tree size %q: %w", meta[3], err)
			}
			size = n
		}
		full := name
		if parentPath != "" {
			full = parentPath + "/" + name
		}
		entries = append(entries, browsemodel.TreeEntry{
			Name: name, Path: full, Mode: meta[0], Type: meta[1], OID: meta[2], Size: size,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		di, dj := entries[i].Type == "tree", entries[j].Type == "tree"
		if di != dj {
			return di // trees first
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gitbrowse/ -run 'ParseLsTree|ReadTree'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitbrowse/tree.go internal/gitbrowse/tree_test.go
git commit -m "feat(gitbrowse): ReadTree + ls-tree parser (dirs-first ordering)"
```

---

### Task 7: `gitbrowse.ReadBlob` (binary detection + size caps)

**Files:**
- Create: `internal/gitbrowse/blob.go`
- Create: `internal/gitbrowse/blob_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/gitbrowse/blob_test.go`:
```go
package gitbrowse

import (
	"context"
	"testing"
)

func TestReadBlob_Text(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	b, err := svc.ReadBlob(context.Background(), tenant, repo, oids["c1"], "a.txt")
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if b.Binary || b.TooLarge || string(b.Bytes) != "hello\n" || b.Size != 6 {
		t.Fatalf("got %+v", b)
	}
}

func TestReadBlob_Binary(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	b, err := svc.ReadBlob(context.Background(), tenant, repo, oids["c2"], "bin.dat")
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if !b.Binary || b.Bytes != nil {
		t.Fatalf("want binary with nil Bytes, got %+v", b)
	}
}

func TestReadBlob_Missing(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	if _, err := svc.ReadBlob(context.Background(), tenant, repo, oids["c2"], "nope.txt"); err == nil {
		t.Fatal("expected error for missing blob")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitbrowse/ -run TestReadBlob`
Expected: FAIL — `ReadBlob` undefined.

- [ ] **Step 3: Implement ReadBlob**

Create `internal/gitbrowse/blob.go`:
```go
package gitbrowse

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

const (
	// maxBlobBytes is the hard cap on blob bytes read for the blob/raw views.
	maxBlobBytes = 10 << 20 // 10 MiB
	// binarySniffWindow is how many leading bytes are scanned for a NUL.
	binarySniffWindow = 8 << 10 // 8 KiB
)

// ReadBlob returns the blob at (oid, path). It confirms the path is a blob,
// records the size, applies the hard size cap, and detects binary content.
func (s *Service) ReadBlob(ctx context.Context, tenant, repoID, oid, path string) (browsemodel.Blob, error) {
	clean := strings.Trim(path, "/")
	if clean == "" {
		return browsemodel.Blob{}, fmt.Errorf("read blob: empty path: %w", browsemodel.ErrNotFound)
	}
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return browsemodel.Blob{}, err
	}
	defer release()

	rev := oid + ":" + clean
	typ, err := gitcli.CatFileType(ctx, m.BareDir(), rev)
	if err != nil || typ != "blob" {
		return browsemodel.Blob{}, fmt.Errorf("read blob %q: %w", rev, browsemodel.ErrNotFound)
	}
	size, err := gitcli.CatFileSize(ctx, m.BareDir(), rev)
	if err != nil {
		return browsemodel.Blob{}, err
	}
	out := browsemodel.Blob{Path: clean, Size: size}
	if size > maxBlobBytes {
		out.TooLarge = true
		return out, nil
	}
	data, err := gitcli.CatBlob(ctx, m.BareDir(), rev)
	if err != nil {
		return browsemodel.Blob{}, err
	}
	window := data
	if len(window) > binarySniffWindow {
		window = window[:binarySniffWindow]
	}
	if bytes.IndexByte(window, 0x00) >= 0 {
		out.Binary = true
		return out, nil
	}
	out.Bytes = data
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gitbrowse/ -run TestReadBlob`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitbrowse/blob.go internal/gitbrowse/blob_test.go
git commit -m "feat(gitbrowse): ReadBlob with binary detection + 10 MiB hard cap"
```

---

### Task 8: `gitbrowse.Log` (paginated)

**Files:**
- Create: `internal/gitbrowse/log.go`
- Create: `internal/gitbrowse/log_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/gitbrowse/log_test.go`:
```go
package gitbrowse

import (
	"context"
	"testing"
)

func TestParseLog(t *testing.T) {
	raw := "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1\x1fAnn\x1fann@x\x1f1700000000\x1fsecond\x1e" +
		"e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0\x1fAnn\x1fann@x\x1f1699999999\x1ffirst\x1e"
	metas, err := parseLog([]byte(raw))
	if err != nil {
		t.Fatalf("parseLog: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("want 2, got %d", len(metas))
	}
	if metas[0].Summary != "second" || metas[0].AuthorName != "Ann" || metas[0].AuthorTime != 1700000000 {
		t.Fatalf("meta0 = %+v", metas[0])
	}
	if metas[0].ShortOID != "f1f1f1f1f1f1" {
		t.Fatalf("shortoid = %q", metas[0].ShortOID)
	}
}

func TestLog_Pagination(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	ctx := context.Background()
	// main has 2 commits; page size 1 → first page has more=true.
	page, more, err := svc.Log(ctx, tenant, repo, oids["c2"], 0, 1)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(page) != 1 || !more {
		t.Fatalf("page0: len=%d more=%v", len(page), more)
	}
	page2, more2, err := svc.Log(ctx, tenant, repo, oids["c2"], 1, 1)
	if err != nil {
		t.Fatalf("Log p2: %v", err)
	}
	if len(page2) != 1 || more2 {
		t.Fatalf("page1: len=%d more=%v", len(page2), more2)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitbrowse/ -run 'ParseLog|TestLog'`
Expected: FAIL — `parseLog`/`Log` undefined.

- [ ] **Step 3: Implement Log + parseLog**

Create `internal/gitbrowse/log.go`:
```go
package gitbrowse

import (
	"context"
	"strconv"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// MaxLogLimit caps a single log page.
const MaxLogLimit = 100

// Log returns one page of commits reachable from oid. It requests limit+1 rows
// to compute `more` without a second query. limit<=0 defaults to 50; limit is
// capped at MaxLogLimit; offset<0 is clamped to 0.
func (s *Service) Log(ctx context.Context, tenant, repoID, oid string, offset, limit int) ([]browsemodel.CommitMeta, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > MaxLogLimit {
		limit = MaxLogLimit
	}
	if offset < 0 {
		offset = 0
	}
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return nil, false, err
	}
	defer release()

	raw, err := gitcli.LogRaw(ctx, m.BareDir(), oid, offset, limit+1)
	if err != nil {
		return nil, false, err
	}
	metas, err := parseLog(raw)
	if err != nil {
		return nil, false, err
	}
	more := false
	if len(metas) > limit {
		more = true
		metas = metas[:limit]
	}
	return metas, more, nil
}

// parseLog parses the 0x1e-record / 0x1f-field format emitted by gitcli.LogRaw.
func parseLog(raw []byte) ([]browsemodel.CommitMeta, error) {
	var out []browsemodel.CommitMeta
	for _, rec := range strings.Split(string(raw), "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "\x1f")
		if len(f) != 5 {
			continue
		}
		var at int64
		if n, err := strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64); err == nil {
			at = n
		}
		oid := f[0]
		short := oid
		if len(short) > 12 {
			short = short[:12]
		}
		out = append(out, browsemodel.CommitMeta{
			OID: oid, ShortOID: short, AuthorName: f[1], AuthorEmail: f[2],
			AuthorTime: at, Summary: f[4],
		})
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gitbrowse/ -run 'ParseLog|TestLog'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitbrowse/log.go internal/gitbrowse/log_test.go
git commit -m "feat(gitbrowse): paginated Log (limit+1 lookahead) + record parser"
```

---

### Task 9: `gitbrowse.Commit` (+ commit-object parse + unified-diff parse)

**Files:**
- Create: `internal/gitbrowse/commit.go`
- Create: `internal/gitbrowse/commit_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/gitbrowse/commit_test.go`:
```go
package gitbrowse

import (
	"context"
	"testing"
)

func TestParseCommitObject(t *testing.T) {
	raw := "tree 1111111111111111111111111111111111111111\n" +
		"parent 2222222222222222222222222222222222222222\n" +
		"author Ann <ann@x> 1700000000 +0000\n" +
		"committer Ann <ann@x> 1700000000 +0000\n" +
		"\n" +
		"update a\n\nbody line\n"
	meta, parents, msg, err := parseCommitObject([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if meta.AuthorName != "Ann" || meta.AuthorEmail != "ann@x" || meta.AuthorTime != 1700000000 {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.Summary != "update a" {
		t.Fatalf("summary = %q", meta.Summary)
	}
	if len(parents) != 1 || parents[0] != "2222222222222222222222222222222222222222" {
		t.Fatalf("parents = %v", parents)
	}
	if msg != "update a\n\nbody line\n" {
		t.Fatalf("msg = %q", msg)
	}
}

func TestParseUnifiedDiff(t *testing.T) {
	patch := "diff --git a/a.txt b/a.txt\n" +
		"index e0e..f1f 100644\n" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1 +1 @@\n" +
		"-hello\n" +
		"+hello again\n" +
		"diff --git a/bin.dat b/bin.dat\n" +
		"new file mode 100644\n" +
		"index 000..abc\n" +
		"Binary files /dev/null and b/bin.dat differ\n"
	files, truncated := parseUnifiedDiff([]byte(patch))
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	if files[0].NewPath != "a.txt" || files[0].Additions != 1 || files[0].Deletions != 1 {
		t.Fatalf("file0 = %+v", files[0])
	}
	if len(files[0].Hunks) != 1 || files[0].Hunks[0].Header != "@@ -1 +1 @@" {
		t.Fatalf("file0 hunks = %+v", files[0].Hunks)
	}
	if !files[1].Binary || files[1].NewPath != "bin.dat" {
		t.Fatalf("file1 = %+v", files[1])
	}
}

func TestCommit_EndToEnd(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	cd, err := svc.Commit(context.Background(), tenant, repo, oids["c2"])
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if cd.Meta.OID != oids["c2"] || cd.Meta.Summary != "update a" {
		t.Fatalf("meta = %+v", cd.Meta)
	}
	if len(cd.Parents) != 1 || cd.Parents[0] != oids["c1"] {
		t.Fatalf("parents = %v", cd.Parents)
	}
	var sawA bool
	for _, f := range cd.Files {
		if f.NewPath == "a.txt" {
			sawA = true
		}
	}
	if !sawA {
		t.Fatalf("expected a.txt in diff, files = %+v", cd.Files)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitbrowse/ -run 'ParseCommitObject|ParseUnifiedDiff|TestCommit'`
Expected: FAIL — `parseCommitObject`/`parseUnifiedDiff`/`Commit` undefined.

- [ ] **Step 3: Implement Commit + parsers**

Create `internal/gitbrowse/commit.go`:
```go
package gitbrowse

import (
	"context"
	"strconv"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

const (
	maxDiffFiles        = 300
	maxDiffLinesPerFile = 3000
)

// Commit returns the commit metadata, message, parents, and parsed diff.
func (s *Service) Commit(ctx context.Context, tenant, repoID, oid string) (browsemodel.CommitDetail, error) {
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return browsemodel.CommitDetail{}, err
	}
	defer release()

	rawCommit, err := gitcli.CatFileCommit(ctx, m.BareDir(), oid)
	if err != nil {
		return browsemodel.CommitDetail{}, browsemodel.ErrNotFound
	}
	meta, parents, msg, err := parseCommitObject(rawCommit)
	if err != nil {
		return browsemodel.CommitDetail{}, err
	}
	meta.OID = oid
	meta.ShortOID = oid
	if len(oid) > 12 {
		meta.ShortOID = oid[:12]
	}

	rawDiff, err := gitcli.DiffTreePatch(ctx, m.BareDir(), oid)
	if err != nil {
		return browsemodel.CommitDetail{}, err
	}
	files, truncated := parseUnifiedDiff(rawDiff)
	return browsemodel.CommitDetail{
		Meta: meta, Message: msg, Parents: parents, Files: files, Truncated: truncated,
	}, nil
}

// parseCommitObject parses a raw `git cat-file commit` object.
func parseCommitObject(raw []byte) (browsemodel.CommitMeta, []string, string, error) {
	s := string(raw)
	var parents []string
	var meta browsemodel.CommitMeta
	idx := strings.Index(s, "\n\n")
	header := s
	msg := ""
	if idx >= 0 {
		header = s[:idx]
		msg = s[idx+2:]
	}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "parent "):
			parents = append(parents, strings.TrimSpace(line[len("parent "):]))
		case strings.HasPrefix(line, "author "):
			name, email, when := parseIdentity(line[len("author "):])
			meta.AuthorName, meta.AuthorEmail, meta.AuthorTime = name, email, when
		}
	}
	meta.Summary = msg
	if nl := strings.IndexByte(msg, '\n'); nl >= 0 {
		meta.Summary = msg[:nl]
	}
	return meta, parents, msg, nil
}

// parseIdentity parses "Name <email> <unixtime> <tz>" into its parts.
func parseIdentity(s string) (name, email string, when int64) {
	lt := strings.IndexByte(s, '<')
	gt := strings.IndexByte(s, '>')
	if lt >= 0 && gt > lt {
		name = strings.TrimSpace(s[:lt])
		email = s[lt+1 : gt]
		rest := strings.Fields(strings.TrimSpace(s[gt+1:]))
		if len(rest) >= 1 {
			if n, err := strconv.ParseInt(rest[0], 10, 64); err == nil {
				when = n
			}
		}
	}
	return name, email, when
}

// parseUnifiedDiff parses `git diff-tree -p` output into per-file diffs. It
// enforces maxDiffFiles (commit-level Truncated) and maxDiffLinesPerFile
// (per-file TooLarge). Binary files are flagged with no hunks.
func parseUnifiedDiff(raw []byte) ([]browsemodel.FileDiff, bool) {
	lines := strings.Split(string(raw), "\n")
	var files []browsemodel.FileDiff
	var cur *browsemodel.FileDiff
	var curHunk *browsemodel.Hunk
	truncated := false

	flushHunk := func() {
		if cur != nil && curHunk != nil {
			cur.Hunks = append(cur.Hunks, *curHunk)
			curHunk = nil
		}
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}

	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			flushFile()
			if len(files) >= maxDiffFiles {
				truncated = true
				return files, truncated
			}
			cur = &browsemodel.FileDiff{Status: "M"}
		case cur == nil:
			// preamble before first file header; ignore
		case strings.HasPrefix(ln, "new file"):
			cur.Status = "A"
		case strings.HasPrefix(ln, "deleted file"):
			cur.Status = "D"
		case strings.HasPrefix(ln, "rename from "):
			cur.Status = "R"
			cur.OldPath = strings.TrimPrefix(ln, "rename from ")
		case strings.HasPrefix(ln, "rename to "):
			cur.NewPath = strings.TrimPrefix(ln, "rename to ")
		case strings.HasPrefix(ln, "copy from "):
			cur.Status = "C"
			cur.OldPath = strings.TrimPrefix(ln, "copy from ")
		case strings.HasPrefix(ln, "copy to "):
			cur.NewPath = strings.TrimPrefix(ln, "copy to ")
		case strings.HasPrefix(ln, "Binary files "):
			cur.Binary = true
		case strings.HasPrefix(ln, "--- "):
			p := strings.TrimPrefix(ln, "--- ")
			if p != "/dev/null" {
				cur.OldPath = strings.TrimPrefix(p, "a/")
			}
		case strings.HasPrefix(ln, "+++ "):
			p := strings.TrimPrefix(ln, "+++ ")
			if p != "/dev/null" {
				cur.NewPath = strings.TrimPrefix(p, "b/")
			}
		case strings.HasPrefix(ln, "@@"):
			flushHunk()
			if cur.TooLarge {
				continue
			}
			curHunk = &browsemodel.Hunk{Header: ln}
		case curHunk != nil && (strings.HasPrefix(ln, "+") || strings.HasPrefix(ln, "-") || strings.HasPrefix(ln, " ")):
			if cur.Additions+cur.Deletions >= maxDiffLinesPerFile {
				cur.TooLarge = true
				cur.Hunks = nil
				curHunk = nil
				continue
			}
			kind := ln[0]
			text := ln[1:]
			switch kind {
			case '+':
				cur.Additions++
			case '-':
				cur.Deletions++
			}
			curHunk.Lines = append(curHunk.Lines, browsemodel.DiffLine{Kind: kind, Text: text})
		}
	}
	flushFile()

	// Ensure NewPath is populated for non-renamed files (fall back to OldPath).
	for i := range files {
		if files[i].NewPath == "" {
			files[i].NewPath = files[i].OldPath
		}
	}
	return files, truncated
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gitbrowse/ -run 'ParseCommitObject|ParseUnifiedDiff|TestCommit'`
Expected: PASS.

- [ ] **Step 5: Full package test + commit**

Run: `go test ./internal/gitbrowse/...`
Expected: PASS (all tasks 3–9).
```bash
git add internal/gitbrowse/commit.go internal/gitbrowse/commit_test.go
git commit -m "feat(gitbrowse): Commit view — commit-object + unified-diff parsers with caps"
```

---

### Task 10: `web.ContentStore` interface + `Deps.Content` wiring field

**Files:**
- Create: `internal/web/contentstore.go`
- Modify: `internal/web/handler.go`

- [ ] **Step 1: Add the interface and the Deps/server fields**

Create `internal/web/contentstore.go`:
```go
package web

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

// ContentStore is the read surface the browse pages need. It is satisfied
// directly by *gitbrowse.Service (which returns browsemodel types), wired at the
// composition root. internal/web depends only on this interface and the leaf
// browsemodel package — never on the storage/mirror layer.
type ContentStore interface {
	ListRefs(ctx context.Context, tenant, repo string) (browsemodel.Refs, error)
	Resolve(ctx context.Context, tenant, repo, rest string) (browsemodel.Resolved, error)
	ReadTree(ctx context.Context, tenant, repo, oid, path string) ([]browsemodel.TreeEntry, error)
	ReadBlob(ctx context.Context, tenant, repo, oid, path string) (browsemodel.Blob, error)
	Log(ctx context.Context, tenant, repo, oid string, offset, limit int) ([]browsemodel.CommitMeta, bool, error)
	Commit(ctx context.Context, tenant, repo, oid string) (browsemodel.CommitDetail, error)
}
```

- [ ] **Step 2: Add `Content` to `Deps` and the `server` struct**

In `internal/web/handler.go`, add to the `Deps` struct (after `OIDC`):
```go
	Content    ContentStore       // nil => code browse disabled (routes 404)
```
Add to the `server` struct (after `oidc`):
```go
	content    ContentStore
```
In `NewHandler`, after `trustProxy: d.TrustProxy,` in the `s := &server{...}` literal, add:
```go
		content:    d.Content,
```

- [ ] **Step 3: Verify it compiles**

Run: `go build ./internal/web/...`
Expected: success (no behavior yet; `content` is unused but assigned — Go allows unused struct fields).

- [ ] **Step 4: Commit**

```bash
git add internal/web/contentstore.go internal/web/handler.go
git commit -m "feat(web): ContentStore interface + Deps.Content wiring field"
```

---

### Task 11: `DataStore.GetVisibleRepo` + sqlitestore impl + adapter

**Files:**
- Modify: `internal/web/datastore.go`
- Create: `internal/auth/sqlitestore/visiblerepo.go`
- Create: `internal/auth/sqlitestore/visiblerepo_test.go`
- Modify: `cmd/bucketvcs/webadapter.go`

- [ ] **Step 1: Write the failing test**

Create `internal/auth/sqlitestore/visiblerepo_test.go`:
```go
package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestGetVisibleRepo(t *testing.T) {
	s := newTestStore(t) // existing helper in this package's tests
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "pub"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterRepo(ctx, "acme", "priv"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoPublic(ctx, "acme", "pub", true); err != nil {
		t.Fatal(err)
	}

	// Anonymous: sees public, not private.
	if _, err := s.GetVisibleRepo(ctx, nil, "acme", "pub"); err != nil {
		t.Fatalf("anon pub: %v", err)
	}
	if _, err := s.GetVisibleRepo(ctx, nil, "acme", "priv"); !errors.Is(err, ErrRepoNotVisible) {
		t.Fatalf("anon priv: want ErrRepoNotVisible, got %v", err)
	}
	// Missing repo: not visible.
	if _, err := s.GetVisibleRepo(ctx, nil, "acme", "ghost"); !errors.Is(err, ErrRepoNotVisible) {
		t.Fatalf("missing: want ErrRepoNotVisible, got %v", err)
	}
	// Admin: sees private.
	admin := &auth.Actor{UserID: "u1", IsAdmin: true}
	if _, err := s.GetVisibleRepo(ctx, admin, "acme", "priv"); err != nil {
		t.Fatalf("admin priv: %v", err)
	}
}
```
> Note: confirm the helper name for an in-memory store and the public-toggle method by reading `internal/auth/sqlitestore/accessrepos_test.go` and `store.go`. If the store helper is named differently (e.g. `openTestStore`), match it; if the public setter differs (e.g. `SetPublicRead`), match it. Adjust the two calls accordingly — the rest of the test is store-agnostic.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run TestGetVisibleRepo`
Expected: FAIL — `GetVisibleRepo`/`ErrRepoNotVisible` undefined.

- [ ] **Step 3: Implement GetVisibleRepo**

Create `internal/auth/sqlitestore/visiblerepo.go`:
```go
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// ErrRepoNotVisible means the repo does not exist or the actor may not see it.
// Callers MUST NOT distinguish the two (anti-enumeration).
var ErrRepoNotVisible = errors.New("sqlitestore: repo not visible")

// GetVisibleRepo returns the repo if the actor may see it under the same rules
// as ListAccessibleRepos (anon → public only; user → public + granted; admin →
// all). Both "absent" and "not authorized" return ErrRepoNotVisible.
func (s *Store) GetVisibleRepo(ctx context.Context, actor *auth.Actor, tenant, name string) (*Repo, error) {
	r := &Repo{}
	var pub int
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant, name, public_read, created_at FROM repos WHERE tenant = ? AND name = ?`,
		tenant, name).Scan(&r.Tenant, &r.Name, &pub, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRepoNotVisible
	}
	if err != nil {
		return nil, err
	}
	r.PublicRead = pub != 0

	if actor != nil && actor.IsAdmin {
		return r, nil
	}
	if r.PublicRead {
		return r, nil
	}
	if actor == nil {
		return nil, ErrRepoNotVisible
	}
	var one int
	err = s.db.QueryRowContext(ctx,
		`SELECT 1 FROM repo_permissions WHERE tenant = ? AND repo = ? AND user_id = ? LIMIT 1`,
		tenant, name, actor.UserID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRepoNotVisible
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}
```
> Note: confirm the `repo_permissions` column names (`tenant`, `repo`, `user_id`) against `accessrepos.go` — they are reused verbatim from `ListAccessibleRepos`'s JOIN, so they are known-correct.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/auth/sqlitestore/ -run TestGetVisibleRepo`
Expected: PASS.

- [ ] **Step 5: Add to the web DataStore interface + adapter**

In `internal/web/datastore.go`, add to the `DataStore` interface (after `ListAccessibleRepos`):
```go
	// GetVisibleRepo returns the repo if the actor may view it, or an error the
	// adapter maps from the store's not-visible sentinel. The web layer treats
	// any error as "404" (anti-enumeration), so it need not be a typed sentinel
	// at this layer.
	GetVisibleRepo(ctx context.Context, actor *auth.Actor, tenant, name string) (*Repo, error)
```

In `cmd/bucketvcs/webadapter.go`, add a method:
```go
func (a *webAdapter) GetVisibleRepo(ctx context.Context, actor *auth.Actor, tenant, name string) (*web.Repo, error) {
	r, err := a.s.GetVisibleRepo(ctx, actor, tenant, name)
	if err != nil {
		return nil, err
	}
	return &web.Repo{Tenant: r.Tenant, Name: r.Name, PublicRead: r.PublicRead, CreatedAt: r.CreatedAt}, nil
}
```

- [ ] **Step 6: Verify build + commit**

Run: `go build ./... && go test ./internal/auth/sqlitestore/ -run TestGetVisibleRepo`
Expected: build OK; test PASS.
```bash
git add internal/web/datastore.go internal/auth/sqlitestore/visiblerepo.go internal/auth/sqlitestore/visiblerepo_test.go cmd/bucketvcs/webadapter.go
git commit -m "feat(web,sqlitestore): GetVisibleRepo with uniform not-visible sentinel"
```

---

### Task 12: Repo router — path parse, authorization, dispatch skeleton

**Files:**
- Create: `internal/web/browse.go`
- Create: `internal/web/browse_test.go`
- Modify: `internal/web/handler.go`

This task wires routing + authorization and returns placeholder 200/404/503 so the route contract is testable before the real pages exist. A `fakeContentStore` keeps web tests independent of `gitbrowse`.

- [ ] **Step 1: Write the failing test**

Create `internal/web/browse_test.go`:
```go
package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

// fakeContent is a minimal ContentStore for routing tests.
type fakeContent struct {
	refs browsemodel.Refs
	res  browsemodel.Resolved
	warm bool
}

func (f *fakeContent) ListRefs(ctx context.Context, t, r string) (browsemodel.Refs, error) {
	if f.warm {
		return browsemodel.Refs{}, browsemodel.ErrWarming
	}
	return f.refs, nil
}
func (f *fakeContent) Resolve(ctx context.Context, t, r, rest string) (browsemodel.Resolved, error) {
	if f.warm {
		return browsemodel.Resolved{}, browsemodel.ErrWarming
	}
	return f.res, nil
}
func (f *fakeContent) ReadTree(ctx context.Context, t, r, oid, p string) ([]browsemodel.TreeEntry, error) {
	return nil, nil
}
func (f *fakeContent) ReadBlob(ctx context.Context, t, r, oid, p string) (browsemodel.Blob, error) {
	return browsemodel.Blob{}, browsemodel.ErrNotFound
}
func (f *fakeContent) Log(ctx context.Context, t, r, oid string, off, lim int) ([]browsemodel.CommitMeta, bool, error) {
	return nil, false, nil
}
func (f *fakeContent) Commit(ctx context.Context, t, r, oid string) (browsemodel.CommitDetail, error) {
	return browsemodel.CommitDetail{}, browsemodel.ErrNotFound
}

// visRepoFake extends the existing test DataStore fake. The plan assumes the
// web package already has a fake DataStore used by landing/login tests; add a
// GetVisibleRepo method to it. Here we use a focused fake for browse routing.
type browseStore struct {
	DataStore
	visible map[string]bool // "tenant/name" -> visible
}

func (b *browseStore) GetVisibleRepo(ctx context.Context, a *auth.Actor, tenant, name string) (*Repo, error) {
	if b.visible[tenant+"/"+name] {
		return &Repo{Tenant: tenant, Name: name}, nil
	}
	return nil, errNotVisibleForTest
}

func newBrowseServer(t *testing.T, content ContentStore, visible map[string]bool) http.Handler {
	t.Helper()
	return NewHandler(Deps{
		Store:   &browseStore{DataStore: stubDataStore{}, visible: visible},
		Content: content,
	})
}

func TestBrowse_Routing(t *testing.T) {
	content := &fakeContent{res: browsemodel.Resolved{Ref: "main", OID: "abc", Path: ""}}
	h := newBrowseServer(t, content, map[string]bool{"acme/demo": true})

	cases := []struct {
		path string
		want int
	}{
		{"/acme/demo", 200},
		{"/acme/demo/tree/main/sub", 200},
		{"/acme/demo/commits/main", 200},
		{"/acme/demo/bogus/main", http.StatusNotFound}, // unknown verb
		{"/acme", http.StatusNotFound},                  // single segment
		{"/acme/secret", http.StatusNotFound},           // not visible → 404
		{"/acme/demo/blob/main/..%2f", http.StatusNotFound}, // invalid name handled upstream; path stays in-repo
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d want %d", c.path, rec.Code, c.want)
		}
	}
}

func TestBrowse_NotVisibleIs404NotForbidden(t *testing.T) {
	h := newBrowseServer(t, &fakeContent{}, map[string]bool{}) // nothing visible
	req := httptest.NewRequest("GET", "/acme/demo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestBrowse_WarmingIs503(t *testing.T) {
	h := newBrowseServer(t, &fakeContent{warm: true}, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
}

func TestBrowse_DisabledWhenContentNil(t *testing.T) {
	h := NewHandler(Deps{Store: &browseStore{DataStore: stubDataStore{}, visible: map[string]bool{"acme/demo": true}}})
	req := httptest.NewRequest("GET", "/acme/demo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("content nil should 404, got %d", rec.Code)
	}
}
```
> Note: `stubDataStore` and `errNotVisibleForTest` are test helpers. Reuse the existing web-package test fake for `DataStore` if one exists (check `handler_test.go`); if its name differs, embed that instead of `stubDataStore`. Add near the top of `browse_test.go`:
> ```go
> var errNotVisibleForTest = errorsNew("not visible")
> ```
> and a tiny `errorsNew` shim, or simply `import "errors"` and use `errors.New`. Keep `stubDataStore` as a zero-value fake whose methods return zero values (only `GetVisibleRepo` is overridden by `browseStore`). If the existing fake already implements all `DataStore` methods, embed it directly and drop `stubDataStore`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/web/ -run TestBrowse`
Expected: FAIL — browse routing not implemented (repo paths currently 404 via `handleLanding`).

- [ ] **Step 3: Implement the router**

Create `internal/web/browse.go`:
```go
package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gateway/routenames"
)

// browseRoute is the parsed shape of a repo browse URL.
type browseRoute struct {
	tenant string
	repo   string
	verb   string // "", "tree", "blob", "raw", "commits", "commit"
	rest   string // remainder after the verb (ref/path or oid), no leading slash
}

// parseBrowsePath parses "/{tenant}/{repo}[/{verb}/{rest...}]". ok=false means
// "not a browse path" (caller should 404). It validates tenant/repo names.
func parseBrowsePath(p string) (browseRoute, bool) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return browseRoute{}, false
	}
	seg := strings.SplitN(p, "/", 4) // tenant, repo, verb, rest
	if len(seg) < 2 || seg[0] == "" || seg[1] == "" {
		return browseRoute{}, false
	}
	if !routenames.ValidateName(seg[0]) || !routenames.ValidateName(seg[1]) {
		return browseRoute{}, false
	}
	br := browseRoute{tenant: seg[0], repo: seg[1]}
	if len(seg) == 2 {
		return br, true // repo home
	}
	br.verb = seg[2]
	switch br.verb {
	case "tree", "blob", "raw", "commits", "commit":
	default:
		return browseRoute{}, false
	}
	if len(seg) == 4 {
		br.rest = seg[3]
	}
	return br, true
}

// handleBrowse is the catch-all entry for repo paths. It authorizes the repo
// (uniform 404 on not-visible) then dispatches by verb.
func (s *server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if s.content == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	br, ok := parseBrowsePath(r.URL.Path)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	sess := SessionFromContext(r.Context())
	if _, err := s.store.GetVisibleRepo(r.Context(), actorFromSession(sess), br.tenant, br.repo); err != nil {
		// Uniform 404 for both not-found and not-authorized (anti-enumeration).
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}

	switch br.verb {
	case "":
		s.handleRepoHome(w, r, br)
	case "tree":
		s.handleTree(w, r, br)
	case "blob":
		s.handleBlob(w, r, br)
	case "raw":
		s.handleRaw(w, r, br)
	case "commits":
		s.handleCommits(w, r, br)
	case "commit":
		s.handleCommit(w, r, br)
	default:
		s.renderError(w, r, http.StatusNotFound, "not found")
	}
}

// browseError maps a ContentStore error to a rendered status page.
func (s *server) browseError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, browsemodel.ErrWarming):
		s.renderError(w, r, http.StatusServiceUnavailable, "repository is warming up — please retry shortly")
	case errors.Is(err, browsemodel.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "not found")
	default:
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
	}
}

// queryPage parses ?page= as a non-negative int (default 0).
func queryPage(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
```

Add **placeholder** handlers at the end of `browse.go` (replaced with real pages in Tasks 13–16, but present now so routing tests pass and the package compiles):
```go
// --- placeholder page handlers (replaced in Tasks 13–16) ---

func (s *server) handleRepoHome(w http.ResponseWriter, r *http.Request, br browseRoute) {
	if _, err := s.content.ListRefs(r.Context(), br.tenant, br.repo); err != nil {
		s.browseError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
func (s *server) handleTree(w http.ResponseWriter, r *http.Request, br browseRoute) {
	if _, err := s.content.Resolve(r.Context(), br.tenant, br.repo, br.rest); err != nil {
		s.browseError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
func (s *server) handleBlob(w http.ResponseWriter, r *http.Request, br browseRoute)    { w.WriteHeader(http.StatusOK) }
func (s *server) handleRaw(w http.ResponseWriter, r *http.Request, br browseRoute)     { w.WriteHeader(http.StatusOK) }
func (s *server) handleCommits(w http.ResponseWriter, r *http.Request, br browseRoute) { w.WriteHeader(http.StatusOK) }
func (s *server) handleCommit(w http.ResponseWriter, r *http.Request, br browseRoute)  { w.WriteHeader(http.StatusOK) }
```

- [ ] **Step 4: Route repo paths to `handleBrowse`**

In `internal/web/landing.go`, change `handleLanding` so non-"/" paths fall through to browse instead of 404:
```go
func (s *server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.handleBrowse(w, r) // repo browse (Phase 2); 404s for non-repo paths
		return
	}
	// ... existing landing body unchanged ...
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/web/ -run TestBrowse`
Expected: PASS.

- [ ] **Step 6: Full web package test (no regressions) + commit**

Run: `go test ./internal/web/...`
Expected: PASS.
```bash
git add internal/web/browse.go internal/web/browse_test.go internal/web/landing.go
git commit -m "feat(web): repo browse router — parse, authorize (uniform 404), warming 503, dispatch"
```

---

### Task 13: Repo home + tree pages (templates + htmx ref switch)

**Files:**
- Modify: `internal/web/browse.go` (replace `handleRepoHome`, `handleTree`)
- Create: `internal/web/templates/repo.html`
- Create: `internal/web/templates/tree.html`
- Create: `internal/web/templates/_tree.html`
- Modify: `internal/web/render.go` (add view-model structs + register new pages)
- Modify: `internal/web/browse_test.go` (assert rendered content)

- [ ] **Step 1: Add view-models + register templates**

In `internal/web/render.go`, add structs:
```go
type browseHeader struct {
	base
	Tenant  string
	Repo    string
	Ref     string // current ref display name
	Refs    browsemodel.Refs
}

type repoHomeData struct {
	browseHeader
	Entries    []browsemodel.TreeEntry
	ReadmeHTML template.HTML // sanitized; "" when no README
}

type treeData struct {
	browseHeader
	Path    string
	Entries []browsemodel.TreeEntry
}
```
Add the import `"github.com/bucketvcs/bucketvcs/internal/browsemodel"` to `render.go`. In `newRenderer`, extend the embedded-page list:
```go
		for _, page := range []string{"landing.html", "login.html", "error.html",
			"repo.html", "tree.html", "_tree.html", "blob.html", "commits.html", "commit.html"} {
```
> Note: `blob.html`, `commits.html`, `commit.html` are created in Tasks 14–15; create empty-but-valid stubs now (each containing just `{{define "..."}}...{{template "base" .}}`) OR add them to the list only as each task lands. Simplest: add all to the list in this task and create minimal valid stubs for the three not-yet-built pages so `newRenderer` doesn't fail. Stub example for `blob.html`: a file containing `{{template "base" .}}`. Replace in later tasks.

- [ ] **Step 2: Write the failing test**

Append to `internal/web/browse_test.go`:
```go
func TestRepoHome_RendersTree(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abc"}}},
		res:  browsemodel.Resolved{Ref: "main", OID: "abc"},
	}
	content.tree = []browsemodel.TreeEntry{{Name: "a.txt", Path: "a.txt", Type: "blob", Size: 6, OID: "x"}}
	h := newBrowseServer(t, content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "a.txt") {
		t.Fatalf("home missing tree entry: %s", rec.Body.String())
	}
}

func TestTree_HtmxReturnsPartial(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abc"}}},
		res:  browsemodel.Resolved{Ref: "main", OID: "abc", Path: "sub"},
	}
	content.tree = []browsemodel.TreeEntry{{Name: "b.txt", Path: "sub/b.txt", Type: "blob", OID: "y"}}
	h := newBrowseServer(t, content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/tree/main/sub", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Fatalf("htmx request should return a partial, not a full page: %s", body)
	}
	if !strings.Contains(body, "b.txt") {
		t.Fatalf("partial missing entry: %s", body)
	}
}
```
Extend `fakeContent` with a `tree` field and return it from `ReadTree`:
```go
// add field: tree []browsemodel.TreeEntry
func (f *fakeContent) ReadTree(ctx context.Context, t, r, oid, p string) ([]browsemodel.TreeEntry, error) {
	if f.warm {
		return nil, browsemodel.ErrWarming
	}
	return f.tree, nil
}
```
(Replace the earlier `ReadTree` stub.)

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/web/ -run 'TestRepoHome|TestTree_Htmx'`
Expected: FAIL — placeholder handlers write empty bodies.

- [ ] **Step 4: Implement the handlers**

In `internal/web/browse.go`, replace `handleRepoHome` and `handleTree`:
```go
func (s *server) handleRepoHome(w http.ResponseWriter, r *http.Request, br browseRoute) {
	refs, err := s.content.ListRefs(r.Context(), br.tenant, br.repo)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	if refs.Default == "" {
		// Empty repo: render header with no tree.
		s.renderBrowse(w, r, "repo.html", repoHomeData{
			browseHeader: s.header(r, br, refs, ""),
		})
		return
	}
	res, err := s.content.Resolve(r.Context(), br.tenant, br.repo, refs.Default)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	entries, err := s.content.ReadTree(r.Context(), br.tenant, br.repo, res.OID, "")
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	readme := s.renderReadme(r.Context(), br, res.OID, entries) // implemented in Task 16; returns "" for now
	s.renderBrowse(w, r, "repo.html", repoHomeData{
		browseHeader: s.header(r, br, refs, refs.Default),
		Entries:      entries,
		ReadmeHTML:   readme,
	})
}

func (s *server) handleTree(w http.ResponseWriter, r *http.Request, br browseRoute) {
	refs, err := s.content.ListRefs(r.Context(), br.tenant, br.repo)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	res, err := s.content.Resolve(r.Context(), br.tenant, br.repo, br.rest)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	entries, err := s.content.ReadTree(r.Context(), br.tenant, br.repo, res.OID, res.Path)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	data := treeData{
		browseHeader: s.header(r, br, refs, res.Ref),
		Path:         res.Path,
		Entries:      entries,
	}
	page := "tree.html"
	if r.Header.Get("HX-Request") == "true" {
		page = "_tree.html" // partial swap
	}
	s.renderBrowse(w, r, page, data)
}
```

Add helpers to `browse.go`:
```go
// header builds the common browse header view-model.
func (s *server) header(r *http.Request, br browseRoute, refs browsemodel.Refs, ref string) browseHeader {
	return browseHeader{
		base:   base{Session: SessionFromContext(r.Context())},
		Tenant: br.tenant, Repo: br.repo, Ref: ref, Refs: refs,
	}
}

// renderBrowse renders a browse page and records the request metric.
func (s *server) renderBrowse(w http.ResponseWriter, r *http.Request, page string, data any) {
	if err := s.render.render(w, page, data); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, strings.TrimSuffix(strings.TrimPrefix(page, "_"), ".html"), http.StatusOK)
}

// renderReadme is implemented in Task 16; this stub returns no README.
func (s *server) renderReadme(ctx context.Context, br browseRoute, oid string, entries []browsemodel.TreeEntry) template.HTML {
	return ""
}
```
Add imports to `browse.go`: `"context"`, `"html/template"`.

- [ ] **Step 5: Create the templates**

Create `internal/web/templates/repo.html`:
```html
{{define "title"}}{{.Tenant}}/{{.Repo}}{{end}}
{{define "content"}}
<div class="browse">
  <div class="repohdr">
    <strong>{{.Tenant}}/{{.Repo}}</strong>
    {{template "refswitcher" .}}
  </div>
  {{if .Entries}}
    <div id="tree">{{template "_treeRows" .}}</div>
  {{else}}
    <p>empty repository</p>
  {{end}}
  {{if .ReadmeHTML}}
    <div class="readme">── README ──{{.ReadmeHTML}}</div>
  {{end}}
</div>
{{end}}
{{template "base" .}}
```

Create `internal/web/templates/tree.html`:
```html
{{define "title"}}{{.Tenant}}/{{.Repo}}: {{.Path}}{{end}}
{{define "content"}}
<div class="browse">
  <div class="repohdr">
    <strong>{{.Tenant}}/{{.Repo}}</strong>
    {{template "refswitcher" .}}
  </div>
  <div class="path">{{.Path}}</div>
  <div id="tree">{{template "_treeRows" .}}</div>
</div>
{{end}}
{{template "base" .}}
```

Create `internal/web/templates/_tree.html` (the htmx partial — just the rows block, no layout):
```html
{{define "_treeRows"}}
<table class="tree">
  {{range .Entries}}
  <tr>
    {{if eq .Type "tree"}}
      <td>[dir]</td><td><a href="/{{$.Tenant}}/{{$.Repo}}/tree/{{$.Ref}}/{{.Path}}">{{.Name}}/</a></td><td></td>
    {{else if eq .Type "blob"}}
      <td>[file]</td><td><a href="/{{$.Tenant}}/{{$.Repo}}/blob/{{$.Ref}}/{{.Path}}">{{.Name}}</a></td><td>{{.Size}}</td>
    {{else}}
      <td>[mod]</td><td>{{.Name}}</td><td></td>
    {{end}}
  </tr>
  {{end}}
</table>
{{end}}
{{template "_treeRows" .}}
```
> Note: `_tree.html` defines `_treeRows` (reused by repo.html/tree.html via `{{template "_treeRows" .}}`) and, when rendered directly as the page (htmx partial), emits just the rows. Verify against `base.html`'s block names: if the layout uses `{{block "content" .}}`/`{{block "title" .}}`, the `content`/`title` defines above are correct; if it uses different names, match them by reading `internal/web/templates/base.html` and `landing.html` first.

Create the `refswitcher` partial. Add it to `_tree.html` or a new `internal/web/templates/_refswitcher.html`; simplest is to define it in `_tree.html` too:
```html
{{define "refswitcher"}}
<form class="refswitch" method="get" action="/{{.Tenant}}/{{.Repo}}/tree/" hx-boost="false">
  <select name="ref" onchange="location.href='/{{.Tenant}}/{{.Repo}}/tree/'+this.value">
    {{range .Refs.Branches}}<option value="{{.Name}}" {{if eq .Name $.Ref}}selected{{end}}>{{.Name}}</option>{{end}}
    {{range .Refs.Tags}}<option value="{{.Name}}" {{if eq .Name $.Ref}}selected{{end}}>tag:{{.Name}}</option>{{end}}
  </select>
</form>
{{end}}
```
> Note: the inline `onchange` is a no-JS-friendly enhancement fallback target; full htmx wiring (swap `#tree`) is a visual-polish detail handled with the frontend-design skill at the end. The functional requirement (switch ref → navigate) is met by `onchange`.

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/web/ -run 'TestRepoHome|TestTree'`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/web/browse.go internal/web/render.go internal/web/templates/ internal/web/browse_test.go
git commit -m "feat(web): repo home + tree pages with ref switcher and htmx partial"
```

---

### Task 14: Blob view + raw endpoint (syntax highlighting + XSS safety)

**Files:**
- Modify: `internal/web/browse.go` (replace `handleBlob`, `handleRaw`)
- Create: `internal/web/highlight.go`
- Create: `internal/web/templates/blob.html` (replace stub)
- Modify: `internal/web/render.go` (add `blobData`)
- Modify: `internal/web/browse_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/web/browse_test.go`:
```go
func TestRaw_ForcesSafeContentType(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main"},
		res:  browsemodel.Resolved{Ref: "main", OID: "abc", Path: "evil.html"},
		blob: browsemodel.Blob{Path: "evil.html", Size: 20, Bytes: []byte("<script>x()</script>")},
	}
	h := newBrowseServer(t, content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/raw/main/evil.html", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	if rec.Body.String() != "<script>x()</script>" {
		t.Fatalf("raw body altered: %q", rec.Body.String())
	}
}

func TestRaw_BinaryIsOctetStreamAttachment(t *testing.T) {
	content := &fakeContent{
		res:  browsemodel.Resolved{Ref: "main", OID: "abc", Path: "bin.dat"},
		blob: browsemodel.Blob{Path: "bin.dat", Size: 4, Binary: true},
	}
	h := newBrowseServer(t, content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/raw/main/bin.dat", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("disposition = %q", cd)
	}
}

func TestBlob_HighlightedAndEscaped(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main"},
		res:  browsemodel.Resolved{Ref: "main", OID: "abc", Path: "main.go"},
		blob: browsemodel.Blob{Path: "main.go", Size: 30, Bytes: []byte("package main // <x>\n")},
	}
	h := newBrowseServer(t, content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/blob/main/main.go", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "<x>") {
		t.Fatalf("blob content must be HTML-escaped, found raw <x>: %s", body)
	}
	if !strings.Contains(body, "main.go") {
		t.Fatalf("blob view missing filename")
	}
}
```
Extend `fakeContent` with a `blob` field; replace its `ReadBlob`:
```go
// add field: blob browsemodel.Blob
func (f *fakeContent) ReadBlob(ctx context.Context, t, r, oid, p string) (browsemodel.Blob, error) {
	if f.warm {
		return browsemodel.Blob{}, browsemodel.ErrWarming
	}
	return f.blob, nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/web/ -run 'TestRaw|TestBlob'`
Expected: FAIL — placeholder handlers don't set headers/body.

- [ ] **Step 3: Implement highlight helper**

Create `internal/web/highlight.go`:
```go
package web

import (
	"bytes"
	"html"
	"html/template"
	"path/filepath"

	"github.com/alecthomas/chroma/v2/quick"
)

// maxHighlightBytes caps source size eligible for syntax highlighting; larger
// text blobs render as plain escaped <pre>.
const maxHighlightBytes = 1 << 20 // 1 MiB

// highlight returns sanitized, highlighted HTML for a text blob. For oversized
// input (or on any highlighter error) it falls back to an HTML-escaped <pre>.
// The output is always derived from HTML-escaped source, so it is safe to mark
// template.HTML.
func highlight(filename string, src []byte) template.HTML {
	if len(src) > maxHighlightBytes {
		return template.HTML("<pre class=\"blob\">" + html.EscapeString(string(src)) + "</pre>")
	}
	var buf bytes.Buffer
	lexer := filepath.Base(filename)
	// chroma's quick.Highlight escapes the source into <span>-wrapped HTML using
	// the "bw" (black & white) style, which suits the retro aesthetic. On error
	// we fall back to a plain escaped <pre>.
	if err := quick.Highlight(&buf, string(src), lexer, "html", "bw"); err != nil {
		return template.HTML("<pre class=\"blob\">" + html.EscapeString(string(src)) + "</pre>")
	}
	return template.HTML(buf.String())
}
```
> Note: `quick.Highlight(w, source, lexer, formatter, style)` — the lexer arg is matched by filename/alias; passing the base filename lets chroma auto-detect by extension, falling back to plaintext. Confirm the import path `github.com/alecthomas/chroma/v2/quick` resolves after Task 1's `go get`.

- [ ] **Step 4: Add `blobData` + implement handlers**

In `internal/web/render.go` add:
```go
type blobData struct {
	browseHeader
	Path     string
	Blob     browsemodel.Blob
	Code     template.HTML // highlighted HTML; empty for binary/too-large
}
```

In `internal/web/browse.go`, replace `handleBlob` and `handleRaw`:
```go
func (s *server) handleBlob(w http.ResponseWriter, r *http.Request, br browseRoute) {
	refs, err := s.content.ListRefs(r.Context(), br.tenant, br.repo)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	res, err := s.content.Resolve(r.Context(), br.tenant, br.repo, br.rest)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	b, err := s.content.ReadBlob(r.Context(), br.tenant, br.repo, res.OID, res.Path)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	var code template.HTML
	if !b.Binary && !b.TooLarge {
		code = highlight(res.Path, b.Bytes)
	}
	s.renderBrowse(w, r, "blob.html", blobData{
		browseHeader: s.header(r, br, refs, res.Ref),
		Path:         res.Path, Blob: b, Code: code,
	})
}

func (s *server) handleRaw(w http.ResponseWriter, r *http.Request, br browseRoute) {
	res, err := s.content.Resolve(r.Context(), br.tenant, br.repo, br.rest)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	b, err := s.content.ReadBlob(r.Context(), br.tenant, br.repo, res.OID, res.Path)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	// Force a safe content-type so attacker-controlled repo content cannot
	// execute inline in the UI origin.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if b.Binary || b.TooLarge {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(res.Path)+"\"")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "inline")
	}
	EmitRequestMetric(r.Context(), s.logger, "raw", http.StatusOK)
	if b.Binary || b.TooLarge {
		// For binary/too-large the body bytes were not loaded (nil); the raw
		// endpoint still must serve bytes. Re-read without caps is out of scope;
		// for Phase 2, binary/too-large blobs under the hard cap are served from
		// b.Bytes when present, else an empty 200 with the attachment headers.
		_, _ = w.Write(b.Bytes)
		return
	}
	_, _ = w.Write(b.Bytes)
}
```
Add import `"path/filepath"` to `browse.go`.
> Note: `ReadBlob` returns nil `Bytes` for binary blobs (Task 7 sets `out.Binary=true` and returns without bytes). For a functional raw download of binary files, adjust `gitbrowse.ReadBlob` to ALSO populate `Bytes` for binary content under the hard cap (binary detection should set the flag but still return the bytes). Update Task 7's implementation: in the binary branch, set `out.Binary = true; out.Bytes = data` before returning. Update Task 7's `TestReadBlob_Binary` accordingly to expect non-nil Bytes. (This is the one cross-task correction — apply it when implementing Task 7, or amend here.)

- [ ] **Step 5: Create `blob.html` (replace stub)**

Create/replace `internal/web/templates/blob.html`:
```html
{{define "title"}}{{.Path}}{{end}}
{{define "content"}}
<div class="browse">
  <div class="repohdr"><strong>{{.Tenant}}/{{.Repo}}</strong>{{template "refswitcher" .}}</div>
  <div class="path">{{.Path}}
    &nbsp;<a href="/{{.Tenant}}/{{.Repo}}/raw/{{.Ref}}/{{.Path}}">[raw]</a>
  </div>
  {{if .Blob.TooLarge}}
    <p>file too large to display ({{.Blob.Size}} bytes) — <a href="/{{.Tenant}}/{{.Repo}}/raw/{{.Ref}}/{{.Path}}">download</a></p>
  {{else if .Blob.Binary}}
    <p>binary file ({{.Blob.Size}} bytes) — <a href="/{{.Tenant}}/{{.Repo}}/raw/{{.Ref}}/{{.Path}}">download</a></p>
  {{else}}
    <div class="code">{{.Code}}</div>
  {{end}}
</div>
{{end}}
{{template "base" .}}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/web/ -run 'TestRaw|TestBlob'`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/web/browse.go internal/web/highlight.go internal/web/render.go internal/web/templates/blob.html internal/web/browse_test.go
git commit -m "feat(web): blob view (chroma highlight) + raw endpoint (forced safe content-type + nosniff + CSP)"
```

---

### Task 15: Commit log + single commit + diff pages

**Files:**
- Modify: `internal/web/browse.go` (replace `handleCommits`, `handleCommit`)
- Create: `internal/web/templates/commits.html` (replace stub)
- Create: `internal/web/templates/commit.html` (replace stub)
- Modify: `internal/web/render.go` (add `commitsData`, `commitData`)
- Modify: `internal/web/browse_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/web/browse_test.go`:
```go
func TestCommits_ListAndPaging(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main"},
		res:  browsemodel.Resolved{Ref: "main", OID: "abc"},
		log:  []browsemodel.CommitMeta{{OID: "c2", ShortOID: "c2", Summary: "update a", AuthorName: "Ann"}},
		more: true,
	}
	h := newBrowseServer(t, content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/commits/main", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "update a") {
		t.Fatalf("commit log missing summary: %s", body)
	}
	if !strings.Contains(body, "page=1") {
		t.Fatalf("expected next-page link when more=true: %s", body)
	}
}

func TestCommit_RendersDiff(t *testing.T) {
	content := &fakeContent{
		commit: browsemodel.CommitDetail{
			Meta:    browsemodel.CommitMeta{OID: "c2", ShortOID: "c2", Summary: "update a", AuthorName: "Ann"},
			Message: "update a\n",
			Parents: []string{"c1"},
			Files: []browsemodel.FileDiff{{
				NewPath: "a.txt", Status: "M", Additions: 1, Deletions: 1,
				Hunks: []browsemodel.Hunk{{Header: "@@ -1 +1 @@", Lines: []browsemodel.DiffLine{
					{Kind: '-', Text: "hello"}, {Kind: '+', Text: "hello again"},
				}}},
			}},
		},
	}
	h := newBrowseServer(t, content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/commit/c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "a.txt") || !strings.Contains(body, "hello again") {
		t.Fatalf("commit view missing diff: %s", body)
	}
}
```
Extend `fakeContent` with `log []browsemodel.CommitMeta`, `more bool`, `commit browsemodel.CommitDetail`; replace `Log` and `Commit`:
```go
func (f *fakeContent) Log(ctx context.Context, t, r, oid string, off, lim int) ([]browsemodel.CommitMeta, bool, error) {
	if f.warm {
		return nil, false, browsemodel.ErrWarming
	}
	return f.log, f.more, nil
}
func (f *fakeContent) Commit(ctx context.Context, t, r, oid string) (browsemodel.CommitDetail, error) {
	if f.warm {
		return browsemodel.CommitDetail{}, browsemodel.ErrWarming
	}
	return f.commit, nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/web/ -run 'TestCommits|TestCommit_'`
Expected: FAIL — placeholder handlers write empty bodies.

- [ ] **Step 3: Add view-models**

In `internal/web/render.go`:
```go
type commitsData struct {
	browseHeader
	Commits  []browsemodel.CommitMeta
	Page     int
	HasMore  bool
}

type commitData struct {
	browseHeader
	Detail browsemodel.CommitDetail
}
```

- [ ] **Step 4: Implement handlers**

In `internal/web/browse.go`, replace `handleCommits` and `handleCommit`:
```go
func (s *server) handleCommits(w http.ResponseWriter, r *http.Request, br browseRoute) {
	refs, err := s.content.ListRefs(r.Context(), br.tenant, br.repo)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	res, err := s.content.Resolve(r.Context(), br.tenant, br.repo, br.rest)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	const pageSize = 50
	page := queryPage(r)
	commits, more, err := s.content.Log(r.Context(), br.tenant, br.repo, res.OID, page*pageSize, pageSize)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	s.renderBrowse(w, r, "commits.html", commitsData{
		browseHeader: s.header(r, br, refs, res.Ref),
		Commits:      commits, Page: page, HasMore: more,
	})
}

func (s *server) handleCommit(w http.ResponseWriter, r *http.Request, br browseRoute) {
	refs, err := s.content.ListRefs(r.Context(), br.tenant, br.repo)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	// br.rest is the raw commit OID for /commit/{oid}.
	oid := strings.Trim(br.rest, "/")
	detail, err := s.content.Commit(r.Context(), br.tenant, br.repo, oid)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	s.renderBrowse(w, r, "commit.html", commitData{
		browseHeader: s.header(r, br, refs, ""),
		Detail:       detail,
	})
}
```

- [ ] **Step 5: Create templates**

Create/replace `internal/web/templates/commits.html`:
```html
{{define "title"}}commits: {{.Tenant}}/{{.Repo}}{{end}}
{{define "content"}}
<div class="browse">
  <div class="repohdr"><strong>{{.Tenant}}/{{.Repo}}</strong>{{template "refswitcher" .}}</div>
  <table class="commits">
    {{range .Commits}}
    <tr>
      <td><a href="/{{$.Tenant}}/{{$.Repo}}/commit/{{.OID}}">{{.ShortOID}}</a></td>
      <td>{{.Summary}}</td>
      <td>{{.AuthorName}}</td>
    </tr>
    {{end}}
  </table>
  <div class="pager">
    {{if gt .Page 0}}<a href="/{{.Tenant}}/{{.Repo}}/commits/{{.Ref}}?page={{minus1 .Page}}">prev</a>{{end}}
    {{if .HasMore}}<a href="/{{.Tenant}}/{{.Repo}}/commits/{{.Ref}}?page={{plus1 .Page}}">next</a>{{end}}
  </div>
</div>
{{end}}
{{template "base" .}}
```
Register two template funcs. In `render.go`'s `parsePage`, attach a FuncMap before `ParseFS`:
```go
func parsePage(fsys fs.FS, dir, page string) (*template.Template, error) {
	base, pg := "base.html", page
	if dir != "" && dir != "." {
		base = dir + "/base.html"
		pg = dir + "/" + page
	}
	funcs := template.FuncMap{
		"plus1":  func(n int) int { return n + 1 },
		"minus1": func(n int) int { if n <= 0 { return 0 }; return n - 1 },
	}
	return template.New("").Funcs(funcs).ParseFS(fsys, base, pg)
}
```
> Note: this modifies the existing `parsePage` (used by all pages). The added funcs are harmless to existing templates. Keep the rest of `parsePage` identical.

Create/replace `internal/web/templates/commit.html`:
```html
{{define "title"}}{{.Detail.Meta.ShortOID}} — {{.Tenant}}/{{.Repo}}{{end}}
{{define "content"}}
<div class="browse">
  <div class="repohdr"><strong>{{.Tenant}}/{{.Repo}}</strong></div>
  <div class="commitmeta">
    <div><strong>{{.Detail.Meta.Summary}}</strong></div>
    <div>{{.Detail.Meta.ShortOID}} · {{.Detail.Meta.AuthorName}} &lt;{{.Detail.Meta.AuthorEmail}}&gt;</div>
    <pre class="message">{{.Detail.Message}}</pre>
  </div>
  {{range .Detail.Files}}
  <div class="filediff">
    <div class="fhdr">{{.Status}} {{if .NewPath}}{{.NewPath}}{{else}}{{.OldPath}}{{end}} (+{{.Additions}} −{{.Deletions}})</div>
    {{if .Binary}}<div>binary file</div>{{else if .TooLarge}}<div>diff too large; view raw</div>{{else}}
      {{range .Hunks}}
        <div class="hunk">{{.Header}}</div>
        {{range .Lines}}<div class="dl k{{printf "%c" .Kind}}">{{printf "%c" .Kind}}{{.Text}}</div>{{end}}
      {{end}}
    {{end}}
  </div>
  {{end}}
  {{if .Detail.Truncated}}<p>diff truncated (too many files)</p>{{end}}
</div>
{{end}}
{{template "base" .}}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/web/ -run 'TestCommits|TestCommit_'`
Expected: PASS.

- [ ] **Step 7: Full web test + commit**

Run: `go test ./internal/web/...`
Expected: PASS.
```bash
git add internal/web/browse.go internal/web/render.go internal/web/templates/commits.html internal/web/templates/commit.html internal/web/browse_test.go
git commit -m "feat(web): commit log (paginated) + single-commit diff views"
```

---

### Task 16: README rendering (goldmark + bluemonday)

**Files:**
- Create: `internal/web/markdown.go`
- Modify: `internal/web/browse.go` (replace `renderReadme` stub)
- Create: `internal/web/markdown_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/web/markdown_test.go`:
```go
package web

import (
	"strings"
	"testing"
)

func TestRenderMarkdown_SanitizesScript(t *testing.T) {
	out := renderMarkdown([]byte("# Hi\n\n<script>alert(1)</script>\n\n**bold**"))
	s := string(out)
	if strings.Contains(s, "<script") {
		t.Fatalf("script not sanitized: %s", s)
	}
	if !strings.Contains(s, "<strong>") && !strings.Contains(s, "<h1") {
		t.Fatalf("markdown not rendered: %s", s)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/web/ -run TestRenderMarkdown`
Expected: FAIL — `renderMarkdown` undefined.

- [ ] **Step 3: Implement markdown render + README selection**

Create `internal/web/markdown.go`:
```go
package web

import (
	"bytes"
	"context"
	"html/template"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

// renderMarkdown converts Markdown to sanitized HTML safe to embed.
func renderMarkdown(src []byte) template.HTML {
	var buf bytes.Buffer
	if err := goldmark.Convert(src, &buf); err != nil {
		return ""
	}
	clean := bluemonday.UGCPolicy().SanitizeBytes(buf.Bytes())
	return template.HTML(clean)
}

// renderReadme finds a root README among entries and renders it. Markdown files
// are rendered + sanitized; a plain README (no extension) is shown escaped via
// the template. Returns "" when no README is present.
func (s *server) renderReadme(ctx context.Context, br browseRoute, oid string, entries []browsemodel.TreeEntry) template.HTML {
	var name string
	for _, e := range entries {
		if e.Type != "blob" {
			continue
		}
		switch strings.ToLower(e.Name) {
		case "readme.md", "readme.markdown":
			name = e.Path
		}
		if name != "" {
			break
		}
	}
	if name == "" {
		return ""
	}
	b, err := s.content.ReadBlob(ctx, br.tenant, br.repo, oid, name)
	if err != nil || b.Binary || b.TooLarge {
		return ""
	}
	return renderMarkdown(b.Bytes)
}
```
Remove the placeholder `renderReadme` stub from `browse.go` (added in Task 13) to avoid a duplicate definition.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/web/ -run TestRenderMarkdown`
Expected: PASS.

- [ ] **Step 5: Full web build/test + commit**

Run: `go build ./internal/web/... && go test ./internal/web/...`
Expected: build OK; tests PASS.
```bash
git add internal/web/markdown.go internal/web/browse.go internal/web/markdown_test.go
git commit -m "feat(web): README rendering via goldmark + bluemonday sanitization"
```

---

### Task 17: Browse metrics

**Files:**
- Modify: `internal/web/metrics.go`
- Modify: `internal/web/browse.go` (emit `web_browse_total`)
- Modify: `internal/gitbrowse/service.go` (emit mirror-wait histogram)
- Modify: `internal/web/browse_test.go` (assert emission via a captured logger — optional)

- [ ] **Step 1: Add the metric emitters**

Append to `internal/web/metrics.go`:
```go
// EmitBrowseMetric records a served browse view. view ∈
// {repo,tree,blob,raw,commits,commit}.
func EmitBrowseMetric(ctx context.Context, logger *slog.Logger, view string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "web_browse_total"),
		slog.String("view", view),
		slog.Int("value", 1),
	)
}
```

- [ ] **Step 2: Emit it from the browse dispatcher**

In `internal/web/browse.go`, at the end of `handleBrowse`'s `switch` (call sites already render), add a single emission point. Simplest: emit in `renderBrowse` is already counting `web_requests_total`; add a `view` counter in `handleBrowse` right after successful authorization:
```go
	// (inside handleBrowse, just before the switch)
	view := br.verb
	if view == "" {
		view = "repo"
	}
	EmitBrowseMetric(r.Context(), s.logger, view)
```

- [ ] **Step 3: Emit the mirror-wait histogram in gitbrowse**

`internal/gitbrowse` has no logger today. Add an optional logger to `Service` to emit the wait metric without coupling to `slog` semantics elsewhere. Modify `service.go`:
```go
// add field to Service:
//   logger *slog.Logger
// add import "log/slog" and "time" (time already imported)
```
Change `NewService` signature to accept a logger (nil → slog.Default()):
```go
func NewService(store storage.ObjectStore, mgr *mirror.Manager, timeout time.Duration, logger *slog.Logger) *Service {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, mgr: mgr, timeout: timeout, logger: logger}
}
```
In `openMirror`, measure the wait. Since `Date.now`-style wall clock is fine in production code (this is not a workflow script), use `time.Now()`:
```go
func (s *Service) openMirror(ctx context.Context, tenant, repo string) (*mirror.Mirror, func(), error) {
	start := time.Now()
	octx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	m, err := s.mgr.Open(octx, tenant, repo)
	s.logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "web_browse_mirror_wait_seconds"),
		slog.Float64("value", time.Since(start).Seconds()))
	if err != nil {
		if errors.Is(octx.Err(), context.DeadlineExceeded) {
			return nil, nil, browsemodel.ErrWarming
		}
		return nil, nil, err
	}
	m.RLock()
	return m, m.RUnlock, nil
}
```
Update the fixture in `fixture_test.go` to pass `nil` for the new logger arg: `NewService(store, mgr, 0, nil)`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/web/... ./internal/gitbrowse/...`
Expected: PASS (signature change propagated to the fixture).

- [ ] **Step 5: Commit**

```bash
git add internal/web/metrics.go internal/web/browse.go internal/gitbrowse/service.go internal/gitbrowse/fixture_test.go
git commit -m "feat(web,gitbrowse): web_browse_total + web_browse_mirror_wait_seconds metrics"
```

---

### Task 18: Wire it up — gateway accessor, serve flag, composition root

**Files:**
- Modify: `internal/gateway/server.go` (add `MirrorManager()` accessor)
- Modify: `cmd/bucketvcs/serve.go` (add `--ui-browse-timeout`, build `gitbrowse.Service`, set `Deps.Content`)

- [ ] **Step 1: Add the gateway accessor**

In `internal/gateway/server.go`, add a method near the other `*Server` methods:
```go
// MirrorManager returns the server's mirror manager so co-located components
// (e.g. the web UI's code-browse reader) can reuse the same warm cache and the
// same process-wide mirror flock instead of opening a second manager.
func (s *Server) MirrorManager() *mirror.Manager { return s.mgr }
```
> Note: confirm `mirror` is already imported in `server.go` (it is — `mgr *mirror.Manager` is declared there).

- [ ] **Step 2: Add the serve flag**

In `cmd/bucketvcs/serve.go`, near the other UI flags (`uiEnabled`, `uiAddr`, `uiDir`, `uiSessionTTL`), add:
```go
	uiBrowseTimeout := fs.Duration("ui-browse-timeout", 20*time.Second,
		"Max wait for cold mirror materialization on a browse request before returning a 503 warming page")
```
> Note: confirm `time` is imported in `serve.go` (it is — `uiSessionTTL` uses a duration).

- [ ] **Step 3: Build and wire the content service**

In `cmd/bucketvcs/serve.go`, inside the `if *uiEnabled {` block, BEFORE the `uiHandler = web.NewHandler(web.Deps{...})` call, construct the browse service from the gateway's mirror manager and the object store:
```go
			browseSvc := gitbrowse.NewService(store, srv.MirrorManager(), *uiBrowseTimeout, logger)
```
Then add `Content: browseSvc,` to the `web.Deps{...}` literal:
```go
				uiHandler = web.NewHandler(web.Deps{
					Store:      newWebAdapter(authS),
					Logger:     logger,
					Limiter:    rateLimiter,
					UIDir:      *uiDir,
					SessionTTL: *uiSessionTTL,
					TrustProxy: *trustProxyHeaders,
					OIDC:       oidcProvider,
					Content:    browseSvc,
				})
```
Add the import `"github.com/bucketvcs/bucketvcs/internal/gitbrowse"` to `serve.go`.
> Note: confirm `store` (the `storage.ObjectStore`) and `srv` (the `*gateway.Server`) are both in scope at this point in `serve.go` — they are (`srv` is created just above at `gateway.NewServer(store, ...)`).

- [ ] **Step 4: Build + smoke vet**

Run: `go build ./... && go vet ./...`
Expected: success.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/server.go cmd/bucketvcs/serve.go
git commit -m "feat(serve): wire gitbrowse code-browse into the web UI; add --ui-browse-timeout"
```

---

### Task 19: Operator guide + progress memory

**Files:**
- Modify: `docs/operator-guides/web-ui.md`
- Create: `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m24_phase2_progress.md`
- Modify: `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md`

- [ ] **Step 1: Update the operator guide**

In `docs/operator-guides/web-ui.md`: in the Production-readiness table, flip "Code browse" to ✅ shipped; add a new section "## Code browse (Phase 2)" documenting the routes (`/{tenant}/{repo}`, `tree`, `blob`, `raw`, `commits`, `commit`), the branch/tag switcher, README rendering, syntax highlighting, the `--ui-browse-timeout` flag + warming 503 behavior, the uniform-404 visibility rule, the raw-endpoint safety headers, and the new metrics (`web_browse_total{view}`, `web_browse_mirror_wait_seconds`). Update §7 "Deferred work" to mark Phase 2 shipped and list the remaining deferrals (path-filtered log, blame, search, compare, cursor pagination, per-read audit, web clone/zip).

- [ ] **Step 2: Write the progress memory file**

Create `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m24_phase2_progress.md` summarizing: packages added (`internal/browsemodel`, `internal/gitbrowse`), the hybrid reader (refstore for refs, mirror+git for content/diff), gitcli helpers added (LsTree/CatBlob/LogRaw/CatFileCommit/DiffTreePatch), the ContentStore interface + GetVisibleRepo, routes + ref/path longest-prefix disambiguation, raw-endpoint XSS hardening, caps (10 MiB blob / 1 MiB highlight / 300 files × 3000 lines diff), deps (goldmark/bluemonday/chroma), and deferrals.

- [ ] **Step 3: Add the one-line MEMORY.md index entry**

Append one line to `MEMORY.md` under the existing list (keep it under ~200 chars):
```
- [M24 Web UI Phase 2 — code browse](m24_phase2_progress.md) — internal/gitbrowse + internal/browsemodel; hybrid reader (refstore + mirror/git); tree/blob/raw/log/commit+diff; ref switcher; README (goldmark+bluemonday); chroma highlight; raw nosniff/CSP; uniform-404; --ui-browse-timeout
```

- [ ] **Step 4: Commit**

```bash
git add docs/operator-guides/web-ui.md
git commit -m "docs(web-ui): document Phase 2 code browse"
```
(Memory files under `~/.claude` are outside the repo and are not committed.)

---

### Task 20: Full verification + branch finalize

**Files:** none (verification only)

- [ ] **Step 1: Run the full test suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS. If any pre-existing flaky importer/pack test fails (documented in memory), re-run that package once to confirm it is the known flake and not a regression.

- [ ] **Step 2: Manual smoke (optional but recommended)**

Run a local server against a localfs store with an imported repo and curl the routes:
```bash
# Build
go build -o /tmp/bucketvcs ./cmd/bucketvcs
# (Use an existing localfs store + auth-db with a public repo, or import one.)
/tmp/bucketvcs serve --addr 127.0.0.1:8088 --store localfs:/path/to/store \
    --auth-db /path/to/auth.db --mirror-dir /tmp/bvcs-mirror --lfs=false &
curl -s 127.0.0.1:8088/acme/demo | head
curl -s 127.0.0.1:8088/acme/demo/tree/main/sub | head
curl -s 127.0.0.1:8088/acme/demo/blob/main/a.txt | head
curl -si 127.0.0.1:8088/acme/demo/raw/main/a.txt | grep -i content-type
curl -s 127.0.0.1:8088/acme/demo/commits/main | head
```
Expected: 200s with rendered content; raw shows `text/plain; charset=utf-8`.

- [ ] **Step 3: Request code review**

Use superpowers:requesting-code-review (or the roborev-review-branch skill) on the branch before merge.

- [ ] **Step 4: Finalize**

Use superpowers:finishing-a-development-branch to open the PR (matching the prior milestone PR style, e.g. "M24 Web UI — Phase 2: code browse").

---

## Self-Review

**1. Spec coverage** (each design section → task):
- §2.1 `internal/gitbrowse` structure → Tasks 3–9. ✅
- §2.2 `ContentStore` interface + sentinels (`ErrNotFound`/`ErrWarming`) → Task 1 (sentinels in `browsemodel`), Task 10 (interface). ✅ (Refinement noted in plan header: DTOs live in leaf `internal/browsemodel`, so `gitbrowse.Service` satisfies the interface directly — no conversion adapter. Faithful to the spec's "interface is the contract".)
- §2.3 wiring in composition root → Task 18 (serve.go) + Task 11 (adapter `GetVisibleRepo`). ✅
- §2.4 request flow (session → router → authz → content → render) → Task 12. ✅
- §3.1 routes (home/tree/blob/raw/commits/commit) → Tasks 12–15. ✅
- §3.2 router placement (1-seg→404, fixed routes unshadowed, ValidateName) → Task 12. ✅
- §3.3 ref↔path disambiguation (OID + longest-prefix) → Task 5. ✅
- §4 access control + uniform 404 → Task 11 + Task 12. ✅
- §5.1 bounded mirror open + RLock + ErrWarming → Task 3. ✅
- §5.2 ListRefs + default-branch order → Task 4. ✅
- §5.3 ReadTree (ls-tree, dirs-first, submodule rows) → Task 6. ✅
- §5.4 ReadBlob (binary, 10 MiB cap) → Task 7. ✅
- §5.5 Log (skip/max, limit+1 more, 50/100) → Task 8. ✅
- §5.6 Commit + diff (rename -M, root, file/line caps) → Task 9. ✅
- §6.1 templates + retro layout + htmx ref switch → Tasks 13–15. ✅
- §6.2 README goldmark+bluemonday → Task 16. ✅
- §6.3 chroma highlight + size cap + raw nosniff/CSP/content-type → Task 14. ✅
- §6.4 warming 503 page → Task 12 (`browseError`). ✅
- §7 `--ui-browse-timeout` → Task 18. ✅
- §8 metrics (`web_browse_total`, `web_browse_mirror_wait_seconds`, route label) → Task 17. ✅
- §8 audit: none — matches spec. ✅
- §9 testing items → distributed across tasks; routing/anti-enumeration/raw-safety/markdown-sanitize/warming all covered. ✅

**2. Placeholder scan:** Task 12 intentionally ships *placeholder handlers* that are explicitly replaced in Tasks 13–16 — this is staged implementation, not a plan placeholder (each has complete code). The Task 13 note to create minimal valid stubs for `blob.html`/`commits.html`/`commit.html` before they're filled is concrete (the stub content is given). No "TBD"/"add error handling"-style gaps remain.

**3. Type consistency:**
- `browsemodel` types are defined once (Task 1) and referenced verbatim everywhere. ✅
- `ContentStore` method set (Task 10) exactly matches `gitbrowse.Service`'s methods (Tasks 3–9): `ListRefs`, `Resolve`, `ReadTree`, `ReadBlob`, `Log`, `Commit`. ✅
- **`NewService` signature change in Task 17** (adds `logger *slog.Logger`) — Task 18's call site uses the 4-arg form, and the Task 3/17 fixture uses `nil`. Flagged in Task 17 Step 3. ✅
- **`ReadBlob` binary `Bytes`**: Task 7 initially returns nil Bytes for binary; Task 14's raw endpoint needs the bytes. The cross-task correction is called out explicitly in Task 14 Step 4's Note (set `out.Bytes = data` in the binary branch and adjust `TestReadBlob_Binary`). Apply when implementing Task 7. ✅
- `GetVisibleRepo` exists on both the store (Task 11, returns `*sqlitestore.Repo`) and the web `DataStore`/adapter (Task 11, returns `*web.Repo`) — names match, return types differ by layer as intended. ✅
- Template func names `plus1`/`minus1` defined in `parsePage` (Task 15) and used only in `commits.html`. ✅

All consistent; corrections are explicitly flagged at point of use. Plan is ready to execute.
