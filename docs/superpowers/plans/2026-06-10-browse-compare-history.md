# Browse depth: Compare + Per-file history — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a two-dot Compare view (diff between two refs/commits) and per-file/dir history to the web code browser, reusing the existing diff and commits-list rendering.

**Architecture:** Both features follow the established read path: `web.ContentStore` → `gitbrowse.Service` → `gitcli` shell-out → `browsemodel` types, reusing existing templates. New gitcli helpers mirror `DiffTreePatch`/`LogRaw`; `Compare` reuses `parseUnifiedDiff`; `LogPath` reuses `parseLog`. The per-file diff markup is extracted from `commit.html` into a shared `{{define "filediff"}}` partial so `commit.html` and the new `compare.html` share one renderer.

**Tech Stack:** Go, `git` plumbing via `internal/gitcli`, `html/template`, the existing `internal/web` browse chassis.

**Spec:** `docs/superpowers/specs/2026-06-10-browse-compare-history-design.md`

**Reference reading before starting:**
- `internal/gitcli/gitcli.go` — `DiffTreePatch`, `LogRaw`, `LogNameStatus` (pathspec pattern), `runCapped`/`run`, `validRefOrOID`/`validRevPath`, `maxDiffPatchBytes`, `ErrOutputCapped`. And `internal/gitcli/gitcli_test.go` for the repo-fixture test helpers (init repo, make commits) — reuse whatever the existing diff/log tests use.
- `internal/gitbrowse/commit.go` (`parseUnifiedDiff`, `Commit`) and `log.go` (`parseLog`, `Log`, `MaxLogLimit`, `openMirror`).
- `internal/browsemodel/model.go` (`FileDiff`, `CommitMeta`, `Resolved`, `Refs`, `ResolveRest`, `IsHex40`, `ErrNotFound`).
- `internal/web/browse.go` (`parseBrowsePath`, `handleBrowse` switch, `handleCommits`, `handleCommit`, `handleTree`, `handleBlob`, `browseError`, `header`, `queryPage`, `renderBrowse`, the `commitsData`/`commitData`/`treeData`/`blobData`/`browseHeader` view-models).
- `internal/web/contentstore.go` (the interface), `internal/web/templates/commit.html`, `commits.html`, `tree.html`, `blob.html`, `_partials.html`, `internal/web/render.go` (template registration list + funcs `urlpath`/`minus1`/`plus1`/`reltime`/`abstime`/`diffclass`).

---

## Task 1: `gitcli.DiffRefsPatch` (two-dot patch)

**Files:**
- Modify: `internal/gitcli/gitcli.go`
- Test: `internal/gitcli/gitcli_test.go`

- [ ] **Step 1: Write the failing test**

Read `gitcli_test.go` first to find the existing repo-fixture helper (how `TestDiffTreePatch*`/`TestLogRaw*` build a temp repo + commits). Mirror it. Add:

```go
func TestDiffRefsPatch_TwoDot(t *testing.T) {
	dir := t.TempDir()
	// Use the same init+commit helper the existing diff tests use. Create two
	// commits: c1 adds file "a.txt"=="one\n"; c2 modifies it to "two\n".
	c1 := gitInitAndCommit(t, dir, map[string]string{"a.txt": "one\n"}, "c1") // adapt helper name
	c2 := gitCommit(t, dir, map[string]string{"a.txt": "two\n"}, "c2")        // adapt helper name

	patch, err := DiffRefsPatch(context.Background(), dir, c1, c2)
	if err != nil {
		t.Fatalf("DiffRefsPatch: %v", err)
	}
	s := string(patch)
	if !strings.Contains(s, "a/a.txt") || !strings.Contains(s, "-one") || !strings.Contains(s, "+two") {
		t.Fatalf("patch missing base->head changes:\n%s", s)
	}
}

func TestDiffRefsPatch_InvalidRef(t *testing.T) {
	if _, err := DiffRefsPatch(context.Background(), t.TempDir(), "-x", "main"); err == nil {
		t.Fatal("want error for leading-dash base")
	}
}
```

Adapt `gitInitAndCommit`/`gitCommit` to the real helper names/signatures in the file (the existing diff tests already create commits and capture their OIDs — reuse that exactly).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitcli/ -run TestDiffRefsPatch -v`
Expected: FAIL — `DiffRefsPatch` undefined.

- [ ] **Step 3: Implement `DiffRefsPatch`** (add next to `DiffTreePatch`)

```go
// DiffRefsPatch returns the two-dot unified patch from base to head
// (git diff-tree -p -M base head): additions in head are '+', removals '-'.
// Both must be valid refs/OIDs. Filenames are emitted verbatim (quotePath off).
// Output is capped at maxDiffPatchBytes; overflow returns the captured prefix
// and ErrOutputCapped (callers parse the prefix and mark the result truncated).
func DiffRefsPatch(ctx context.Context, dir, base, head string) ([]byte, error) {
	if !validRefOrOID(base) {
		return nil, fmt.Errorf("gitcli: DiffRefsPatch: invalid base %q", base)
	}
	if !validRefOrOID(head) {
		return nil, fmt.Errorf("gitcli: DiffRefsPatch: invalid head %q", head)
	}
	return runCapped(ctx, dir, maxDiffPatchBytes, "-c", "core.quotePath=false", "--no-replace-objects",
		"diff-tree", "-p", "-M", "--no-color", base, head)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gitcli/ -run TestDiffRefsPatch -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitcli/gitcli.go internal/gitcli/gitcli_test.go
git commit -m "feat(gitcli): DiffRefsPatch for two-dot ref-to-ref diffs"
```

---

## Task 2: `gitcli.LogRawPath` + `gitcli.PathKind`

**Files:**
- Modify: `internal/gitcli/gitcli.go`
- Test: `internal/gitcli/gitcli_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestLogRawPath_ScopesToPath(t *testing.T) {
	dir := t.TempDir()
	// c1: add a.txt + b.txt; c2: modify a.txt only.
	gitInitAndCommit(t, dir, map[string]string{"a.txt": "1\n", "b.txt": "x\n"}, "c1")
	gitCommit(t, dir, map[string]string{"a.txt": "2\n"}, "c2 touch a")

	raw, err := LogRawPath(context.Background(), dir, "HEAD", "a.txt", false, 0, 10)
	if err != nil {
		t.Fatalf("LogRawPath: %v", err)
	}
	recs := strings.Count(string(raw), "\x1e")
	if recs != 2 { // both commits touched a.txt
		t.Fatalf("want 2 records for a.txt, got %d:\n%q", recs, raw)
	}
	rawB, err := LogRawPath(context.Background(), dir, "HEAD", "b.txt", false, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(rawB), "\x1e"); got != 1 { // only c1 touched b.txt
		t.Fatalf("want 1 record for b.txt, got %d", got)
	}
}

func TestLogRawPath_InvalidPath(t *testing.T) {
	if _, err := LogRawPath(context.Background(), t.TempDir(), "HEAD", "-flag", false, 0, 10); err == nil {
		t.Fatal("want error for leading-dash path")
	}
}

func TestPathKind(t *testing.T) {
	dir := t.TempDir()
	gitInitAndCommit(t, dir, map[string]string{"dir/a.txt": "1\n"}, "c1")
	if k, err := PathKind(context.Background(), dir, "HEAD", "dir/a.txt"); err != nil || k != "blob" {
		t.Fatalf("file kind = %q, %v; want blob", k, err)
	}
	if k, err := PathKind(context.Background(), dir, "HEAD", "dir"); err != nil || k != "tree" {
		t.Fatalf("dir kind = %q, %v; want tree", k, err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/gitcli/ -run 'TestLogRawPath|TestPathKind' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement both** (add near `LogRaw`)

```go
// LogRawPath is LogRaw scoped to a single pathspec (git log [--follow] <rev> -- <path>).
// Same record format as LogRaw. follow tracks renames and is valid only for a
// single file (git rejects --follow with a directory); callers must pass
// follow=false for directories. path is validated as a rev-path (rejects a
// leading '-' and NUL/CR/LF; spaces allowed).
func LogRawPath(ctx context.Context, dir, rev, path string, follow bool, skip, max int) ([]byte, error) {
	if !validRefOrOID(rev) {
		return nil, fmt.Errorf("gitcli: LogRawPath: invalid rev %q", rev)
	}
	if !validRevPath(path) {
		return nil, fmt.Errorf("gitcli: LogRawPath: invalid path %q", path)
	}
	if skip < 0 || max <= 0 {
		return nil, fmt.Errorf("gitcli: LogRawPath: bad skip/max %d/%d", skip, max)
	}
	const format = "--pretty=format:%H%x1f%an%x1f%ae%x1f%at%x1f%s%x1e"
	args := []string{"-c", "core.quotePath=false", "--no-replace-objects", "log", rev,
		fmt.Sprintf("--skip=%d", skip), fmt.Sprintf("--max-count=%d", max), "--no-color", format}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, "--", path)
	return run(ctx, dir, args...)
}

// PathKind reports the object kind ("blob"|"tree") of path at rev via
// `git cat-file -t <rev>:<path>`. A lookup failure (path absent at rev, etc.)
// returns "" plus the error; callers that only need it to choose --follow
// should treat any error as "not a blob" and proceed without following.
func PathKind(ctx context.Context, dir, rev, path string) (string, error) {
	spec := rev + ":" + path
	if !validRevPath(spec) {
		return "", fmt.Errorf("gitcli: PathKind: invalid spec %q", spec)
	}
	out, err := run(ctx, dir, "--no-replace-objects", "cat-file", "-t", spec)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
```

(`strings` and `fmt` are already imported in gitcli.go.)

- [ ] **Step 4: Run tests**

Run: `go test ./internal/gitcli/ -run 'TestLogRawPath|TestPathKind' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitcli/gitcli.go internal/gitcli/gitcli_test.go
git commit -m "feat(gitcli): LogRawPath (path-scoped log) + PathKind"
```

---

## Task 3: `browsemodel.Comparison` + `gitbrowse.Service.Compare`

**Files:**
- Modify: `internal/browsemodel/model.go`
- Create: `internal/gitbrowse/compare.go`
- Test: `internal/gitbrowse/compare_test.go`

- [ ] **Step 1: Add the `Comparison` type to `model.go`** (after `CommitDetail`)

```go
// Comparison is a two-dot diff (base..head) between two commits. Files reuses
// the per-commit FileDiff shape; Additions/Deletions are repo-wide totals.
type Comparison struct {
	Files     []FileDiff
	Additions int
	Deletions int
	Truncated bool // diff exceeded the byte cap; Files is partial
}
```

- [ ] **Step 2: Write the failing test**

Read `internal/gitbrowse/*_test.go` for the service-fixture helper (how `Commit`/`Log` tests build a repo + service). Mirror it. Create `compare_test.go`:

```go
package gitbrowse

import (
	"context"
	"testing"
)

func TestCompare_ModifiedAndAdded(t *testing.T) {
	// Build a fixture service+repo with two commits via the existing helper:
	//   base: a.txt="one\n"
	//   head: a.txt="two\n" (modified) + c.txt="new\n" (added)
	svc, tenant, repo, baseOID, headOID := newCompareFixture(t) // implement using existing service-test harness
	cmp, err := svc.Compare(context.Background(), tenant, repo, baseOID, headOID)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(cmp.Files) != 2 {
		t.Fatalf("want 2 files, got %d: %+v", len(cmp.Files), cmp.Files)
	}
	var sawMod, sawAdd bool
	for _, f := range cmp.Files {
		if f.NewPath == "a.txt" && f.Status == "M" {
			sawMod = true
		}
		if f.NewPath == "c.txt" && f.Status == "A" {
			sawAdd = true
		}
	}
	if !sawMod || !sawAdd {
		t.Fatalf("missing M a.txt / A c.txt: %+v", cmp.Files)
	}
	if cmp.Additions == 0 {
		t.Fatalf("additions should be > 0")
	}
}
```

Build `newCompareFixture` from the existing gitbrowse test harness (the `Commit`/`Log` tests already construct a `*Service` over a fixture mirror — reuse that construction and its commit helper; return the two commit OIDs).

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/gitbrowse/ -run TestCompare -v`
Expected: FAIL — `svc.Compare` undefined.

- [ ] **Step 4: Implement `Compare`** in new `internal/gitbrowse/compare.go`

```go
package gitbrowse

import (
	"context"
	"errors"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// Compare returns the two-dot diff (baseOID..headOID) parsed into per-file
// diffs, reusing parseUnifiedDiff (same caps/truncation as Commit).
func (s *Service) Compare(ctx context.Context, tenant, repoID, baseOID, headOID string) (browsemodel.Comparison, error) {
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return browsemodel.Comparison{}, err
	}
	defer release()

	rawDiff, err := gitcli.DiffRefsPatch(ctx, m.BareDir(), baseOID, headOID)
	capped := errors.Is(err, gitcli.ErrOutputCapped)
	if err != nil && !capped {
		return browsemodel.Comparison{}, err
	}
	files, truncated := parseUnifiedDiff(rawDiff)
	if capped {
		if len(files) > 0 {
			files = files[:len(files)-1]
		}
		truncated = true
	}
	add, del := 0, 0
	for i := range files {
		add += files[i].Additions
		del += files[i].Deletions
	}
	return browsemodel.Comparison{Files: files, Additions: add, Deletions: del, Truncated: truncated}, nil
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/gitbrowse/ -run TestCompare -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/browsemodel/model.go internal/gitbrowse/compare.go internal/gitbrowse/compare_test.go
git commit -m "feat(gitbrowse): Compare (two-dot ref diff) + browsemodel.Comparison"
```

---

## Task 4: `gitbrowse.Service.LogPath`

**Files:**
- Modify: `internal/gitbrowse/log.go`
- Test: `internal/gitbrowse/log_test.go` (or the existing log test file)

- [ ] **Step 1: Write the failing test**

```go
func TestLogPath_FileVsDir(t *testing.T) {
	// Fixture: commit1 adds dir/a.txt + dir/b.txt; commit2 modifies dir/a.txt.
	svc, tenant, repo, headOID := newLogPathFixture(t) // build from existing harness
	ctx := context.Background()

	fileCommits, more, err := svc.LogPath(ctx, tenant, repo, headOID, "dir/a.txt", 0, 50)
	if err != nil {
		t.Fatalf("LogPath file: %v", err)
	}
	if len(fileCommits) != 2 || more {
		t.Fatalf("want 2 commits for dir/a.txt, got %d more=%v", len(fileCommits), more)
	}
	dirCommits, _, err := svc.LogPath(ctx, tenant, repo, headOID, "dir", 0, 50)
	if err != nil {
		t.Fatalf("LogPath dir: %v", err)
	}
	if len(dirCommits) != 2 { // both commits touched dir/
		t.Fatalf("want 2 commits for dir/, got %d", len(dirCommits))
	}
	none, _, err := svc.LogPath(ctx, tenant, repo, headOID, "nope.txt", 0, 50)
	if err != nil {
		t.Fatalf("LogPath missing: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("want 0 commits for missing path, got %d", len(none))
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/gitbrowse/ -run TestLogPath -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement `LogPath`** (add to `log.go`)

```go
// LogPath returns one page of commits touching path, reachable from oid. It
// mirrors Log's pagination. Rename-following (git --follow) is enabled only
// when path resolves to a single blob; for directories or an undeterminable
// kind it is omitted (git --follow is single-file only).
func (s *Service) LogPath(ctx context.Context, tenant, repoID, oid, path string, offset, limit int) ([]browsemodel.CommitMeta, bool, error) {
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

	follow := false
	if kind, kerr := gitcli.PathKind(ctx, m.BareDir(), oid, path); kerr == nil && kind == "blob" {
		follow = true
	}
	raw, err := gitcli.LogRawPath(ctx, m.BareDir(), oid, path, follow, offset, limit+1)
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
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/gitbrowse/ -run TestLogPath -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitbrowse/log.go internal/gitbrowse/log_test.go
git commit -m "feat(gitbrowse): LogPath (path-scoped commit history, rename-follow for files)"
```

---

## Task 5: Extend `ContentStore` interface

**Files:**
- Modify: `internal/web/contentstore.go`
- Test: `internal/web/contentstore_test.go` (create or extend)

- [ ] **Step 1: Add the two methods to the interface**

In `internal/web/contentstore.go`, add to the `ContentStore` interface:

```go
	Compare(ctx context.Context, tenant, repo, baseOID, headOID string) (browsemodel.Comparison, error)
	LogPath(ctx context.Context, tenant, repo, oid, path string, offset, limit int) ([]browsemodel.CommitMeta, bool, error)
```

- [ ] **Step 2: Add a compile-time conformance assertion test**

Create `internal/web/contentstore_test.go` (or add to an existing web test file):

```go
package web

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitbrowse"
)

func TestGitbrowseSatisfiesContentStore(t *testing.T) {
	var _ ContentStore = (*gitbrowse.Service)(nil)
}
```

- [ ] **Step 3: Run to verify it compiles + passes**

Run: `go test ./internal/web/ -run TestGitbrowseSatisfiesContentStore -v`
Expected: PASS. If it FAILS to compile, a signature in Task 3/4 drifted from the interface — reconcile (the interface is the spec; fix the concrete method signature to match, or vice-versa, so they're identical).

- [ ] **Step 4: Build the whole tree**

Run: `go build ./...`
Expected: clean (the fake `ContentStore` used in `browse_test.go` will now fail to compile until Task 6 — that is expected and handled in Task 6; if `browse_test.go` has a fake that must satisfy `ContentStore`, add the two new methods to that fake here as a minimal stub so the package builds: a `Compare` returning `browsemodel.Comparison{}` and a `LogPath` returning `nil, false, nil`, then Tasks 6/8 give them real test behavior). Check `browse_test.go` for such a fake and stub it if present.

- [ ] **Step 5: Commit**

```bash
git add internal/web/contentstore.go internal/web/contentstore_test.go internal/web/browse_test.go
git commit -m "feat(web): ContentStore gains Compare + LogPath"
```

---

## Task 6: Per-file history (handler + templates + entry points)

**Files:**
- Modify: `internal/web/browse.go` (`handleCommits` + `commitsData`)
- Modify: `internal/web/templates/commits.html`, `blob.html`, `tree.html`
- Test: `internal/web/browse_test.go`

- [ ] **Step 1: Write failing tests**

Read `browse_test.go` for the fake `ContentStore` and the browse-request harness (how a GET to a browse path is issued and the rendered body asserted). Extend the fake's `LogPath` to return canned commits and add:

```go
func TestCommits_PerFileHistory(t *testing.T) {
	// fake.LogPath returns 2 commits for path "src/a.go"; fake.Log must NOT be
	// called when a path is present. Use the existing fake + harness.
	body, code := getBrowse(t, fakeWithFileHistory(t), "/acme/demo/commits/main/src/a.go") // adapt harness
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(body, "history: src/a.go") {
		t.Fatalf("missing history heading: %s", body)
	}
	// pager link (if HasMore) carries the path
	if !strings.Contains(body, "/acme/demo/commits/main/src/a.go?page=1") {
		t.Fatalf("pager link missing path: %s", body)
	}
}

func TestBlob_HasHistoryLink(t *testing.T) {
	body, _ := getBrowse(t, fakeBlob(t), "/acme/demo/blob/main/src/a.go")
	if !strings.Contains(body, "/acme/demo/commits/main/src/a.go") {
		t.Fatalf("blob missing history link: %s", body)
	}
}
```

Match the real fake/harness names from `browse_test.go`. Make `fakeWithFileHistory.LogPath` return 2 metas + `more=true` so the pager renders; have its `Log` call `t.Fatal` if invoked (asserts the path path is taken).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/web/ -run 'TestCommits_PerFileHistory|TestBlob_HasHistoryLink' -v`
Expected: FAIL (currently `res.Path != "" → 404`; no history links).

- [ ] **Step 3: Update `handleCommits`** — replace the `if res.Path != "" { … 404 }` block with path-aware logging

```go
	const pageSize = 50
	page := queryPage(r)
	var commits []browsemodel.CommitMeta
	var more bool
	if res.Path != "" {
		commits, more, err = s.content.LogPath(r.Context(), br.tenant, br.repo, res.OID, res.Path, page*pageSize, pageSize)
	} else {
		commits, more, err = s.content.Log(r.Context(), br.tenant, br.repo, res.OID, page*pageSize, pageSize)
	}
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	s.renderBrowse(w, r, "commits.html", commitsData{
		browseHeader: s.header(w, r, br, refs, res.Ref, res.OID),
		Commits:      commits,
		Page:         page,
		HasMore:      more,
		Path:         res.Path,
	})
```

Add a `Path string` field to the `commitsData` struct definition (find it in `browse.go`).

- [ ] **Step 4: Update `commits.html`** — add the path heading, empty state, and path-aware pager

Replace the `commits.html` content body with (keeping the existing `define`/`base` wrappers):

```html
{{define "content"}}
<div class="browse">
  <div class="repohdr"><span class="path">{{.Tenant}}/{{.Repo}}</span>{{if .CanAdmin}}&nbsp;<a href="/{{.Tenant}}/{{.Repo}}/settings">[settings]</a>{{end}}</div>
  {{if .Path}}<h2>history: {{.Path}} @ {{.RefOrOID}}</h2>{{end}}
  {{if .Commits}}
  <table class="commits">
    {{range .Commits}}
    <tr>
      <td class="oid"><a href="/{{$.Tenant}}/{{$.Repo}}/commit/{{.OID}}">{{.ShortOID}}</a></td>
      <td class="summary">{{.Summary}}</td>
      <td class="who">{{.AuthorName}}</td>
      <td class="age" title="{{abstime .AuthorTime}}">{{reltime .AuthorTime}}</td>
    </tr>
    {{end}}
  </table>
  <div class="pager">
    {{if gt .Page 0}}<a href="/{{.Tenant}}/{{.Repo}}/commits/{{urlpath .RefOrOID}}{{if .Path}}/{{urlpath .Path}}{{end}}?page={{minus1 .Page}}">prev</a>{{end}}
    {{if .HasMore}}<a href="/{{.Tenant}}/{{.Repo}}/commits/{{urlpath .RefOrOID}}{{if .Path}}/{{urlpath .Path}}{{end}}?page={{plus1 .Page}}">next</a>{{end}}
  </div>
  {{else}}
  <p class="empty">{{if .Path}}no history for this path.{{else}}no commits.{{end}}</p>
  {{end}}
</div>
{{end}}
```

(Confirm `urlpath`, `minus1`, `plus1`, `reltime`, `abstime` are registered funcs — they are, used by the current `commits.html`.)

- [ ] **Step 5: Add `[history]` entry points**

In `blob.html`, near the existing path/header area, add a history link (read the file to place it next to the existing `[raw]`/header links; `.RefOrOID` and `.Path` are available on the blob view-model):
```html
<a href="/{{.Tenant}}/{{.Repo}}/commits/{{urlpath .RefOrOID}}/{{urlpath .Path}}">[history]</a>
```

In `tree.html`, near the header/path breadcrumb, add a directory-history link (`.Path` is the current tree path, "" at root → links to whole-repo history):
```html
<a href="/{{.Tenant}}/{{.Repo}}/commits/{{urlpath .RefOrOID}}{{if .Path}}/{{urlpath .Path}}{{end}}">[history]</a>
```

- [ ] **Step 6: Run tests**

Run: `go test ./internal/web/ -run 'TestCommits|TestBlob|TestTree' -v`
Expected: PASS (new tests + existing browse tests still green). Run `go build ./...`.

- [ ] **Step 7: Commit**

```bash
git add internal/web/browse.go internal/web/templates/commits.html internal/web/templates/blob.html internal/web/templates/tree.html internal/web/browse_test.go
git commit -m "feat(web): per-file/dir history via path-scoped commits view + [history] links"
```

---

## Task 7: Extract shared `filediff` partial

**Files:**
- Modify: `internal/web/templates/_partials.html`, `internal/web/templates/commit.html`
- Test: `internal/web/render_test.go` (or `browse_test.go`)

- [ ] **Step 1: Write a failing render test**

Add a test that renders `commit.html` for a fixture `commitData` containing one modified file with one hunk, and asserts the diff markup is present (this guards the partial extraction leaves output equivalent):

```go
func TestCommitHTML_RendersFileDiff(t *testing.T) {
	d := commitData{ /* build browseHeader + Detail with one FileDiff:
		Status:"M", NewPath:"a.txt", Additions:1, Deletions:1,
		Hunks:[{Header:"@@ -1 +1 @@", Lines:[{Kind:'-',Text:"old"},{Kind:'+',Text:"new"}]}] */ }
	body := renderToString(t, "commit.html", d) // use the existing render helper
	for _, want := range []string{`class="filediff"`, "M a.txt (+1 -1)", `class="hunk"`, "-old", "+new"} {
		if !strings.Contains(body, want) {
			t.Fatalf("commit.html missing %q:\n%s", want, body)
		}
	}
}
```

Build `commitData`/`browseHeader`/`browsemodel.CommitDetail` with real fields (read the structs). Use the existing render harness (how `render_test.go` renders a named template to a string).

- [ ] **Step 2: Run — it should PASS already** (commit.html renders this today)

Run: `go test ./internal/web/ -run TestCommitHTML_RendersFileDiff -v`
Expected: PASS (pre-extraction). This test is the *guard*: it must stay green across the refactor.

- [ ] **Step 3: Extract the partial** — add to `_partials.html`

```html
{{define "filediff"}}
<div class="filediff">
  <div class="fhdr">{{.Status}} {{if .NewPath}}{{.NewPath}}{{else}}{{.OldPath}}{{end}} (+{{.Additions}} -{{.Deletions}})</div>
  {{if .Binary}}<div class="empty">binary file</div>
  {{else if .TooLarge}}<div class="empty">diff too large; view raw</div>
  {{else}}
    {{range .Hunks}}
      <div class="hunk">{{.Header}}</div>
      {{range .Lines}}<div class="dl {{diffclass .Kind}}">{{printf "%c" .Kind}}{{.Text}}</div>{{end}}
    {{end}}
  {{end}}
</div>
{{end}}
```

- [ ] **Step 4: Update `commit.html`** — replace the inline per-file block (currently lines ~10–22) with:

```html
  {{range .Detail.Files}}{{template "filediff" .}}{{end}}
  {{if .Detail.Truncated}}<p class="empty">diff truncated (too many files)</p>{{end}}
```

(Leave the `commitmeta` block and wrappers unchanged.)

- [ ] **Step 5: Run the guard test (must still pass)**

Run: `go test ./internal/web/ -run TestCommitHTML_RendersFileDiff -v`
Expected: PASS (output equivalent after extraction). Run the full web suite: `go test ./internal/web/ 2>&1 | tail -3`.

- [ ] **Step 6: Commit**

```bash
git add internal/web/templates/_partials.html internal/web/templates/commit.html internal/web/render_test.go
git commit -m "refactor(web): extract shared filediff partial (commit + compare reuse)"
```

---

## Task 8: Compare view (route, handler, template)

**Files:**
- Modify: `internal/web/browse.go` (`parseBrowsePath`, `handleBrowse`, new `handleCompare` + `compareData`)
- Create: `internal/web/templates/compare.html`
- Modify: `internal/web/render.go` (register `compare.html`)
- Test: `internal/web/browse_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestComparePicker_RendersRefs(t *testing.T) {
	// fake.ListRefs returns Default "main", Branches [main, feature], Tags [v1]
	body, code := getBrowse(t, fakeWithRefs(t), "/acme/demo/compare")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{"feature", "v1", `name="base"`, `name="head"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("picker missing %q: %s", want, body)
		}
	}
}

func TestCompareResult_RendersDiff(t *testing.T) {
	// fake.ListRefs resolves main->oidA, feature->oidB; fake.Compare returns a
	// Comparison with one A file "c.txt" (+3 -0).
	body, code := getBrowse(t, fakeCompare(t), "/acme/demo/compare/main..feature")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{"main..feature", "1 file", "c.txt", `class="filediff"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("compare result missing %q: %s", want, body)
		}
	}
}

func TestCompare_BadRef404(t *testing.T) {
	_, code := getBrowse(t, fakeWithRefs(t), "/acme/demo/compare/main..nope")
	if code != 404 {
		t.Fatalf("want 404 for unknown head, got %d", code)
	}
}

func TestCompare_MissingSeparator404(t *testing.T) {
	_, code := getBrowse(t, fakeWithRefs(t), "/acme/demo/compare/mainfeature")
	if code != 404 {
		t.Fatalf("want 404 for missing '..', got %d", code)
	}
}
```

Extend the fake `ContentStore`: `Compare` returns a canned `Comparison`; `ListRefs` returns the named refs; resolution uses `browsemodel.ResolveRest` against those refs inside the handler (so the fake only needs `ListRefs` + `Compare`).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/web/ -run TestCompare -v`
Expected: FAIL — `compare` route 404s (not in whitelist) / `handleCompare` undefined.

- [ ] **Step 3: Add `compare` to the route whitelist + dispatch**

In `parseBrowsePath`, add `"compare"` to the verb `switch`:
```go
	case "tree", "blob", "raw", "commits", "commit", "compare":
```
In `handleBrowse`'s verb switch, add:
```go
	case "compare":
		s.handleCompare(w, r, br)
```

- [ ] **Step 4: Add `compareData` + `handleCompare`** to `browse.go`

```go
type compareData struct {
	browseHeader
	Base      string // base ref/OID display string (from the URL or picker default)
	Head      string
	HasResult bool
	Cmp       browsemodel.Comparison
}

func (s *server) handleCompare(w http.ResponseWriter, r *http.Request, br browseRoute) {
	refs, err := s.content.ListRefs(r.Context(), br.tenant, br.repo)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	rest := strings.Trim(br.rest, "/")

	// No spec → picker page (or redirect from the picker's ?base=&head= GET).
	if rest == "" {
		base := r.URL.Query().Get("base")
		head := r.URL.Query().Get("head")
		if base != "" && head != "" {
			http.Redirect(w, r, "/"+br.tenant+"/"+br.repo+"/compare/"+base+".."+head, http.StatusSeeOther)
			return
		}
		if base == "" {
			base = refs.Default
		}
		if head == "" {
			head = refs.Default
		}
		s.renderBrowse(w, r, "compare.html", compareData{
			browseHeader: s.header(w, r, br, refs, refs.Default, ""),
			Base:         base,
			Head:         head,
		})
		return
	}

	// Result page: split on the first "..".
	i := strings.Index(rest, "..")
	if i < 0 {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	baseSpec, headSpec := rest[:i], rest[i+2:]
	if baseSpec == "" || headSpec == "" {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	resBase, err := browsemodel.ResolveRest(refs, baseSpec)
	if err != nil || resBase.Path != "" {
		if err != nil {
			s.browseError(w, r, err)
		} else {
			s.renderError(w, r, http.StatusNotFound, "not found")
		}
		return
	}
	resHead, err := browsemodel.ResolveRest(refs, headSpec)
	if err != nil || resHead.Path != "" {
		if err != nil {
			s.browseError(w, r, err)
		} else {
			s.renderError(w, r, http.StatusNotFound, "not found")
		}
		return
	}
	cmp, err := s.content.Compare(r.Context(), br.tenant, br.repo, resBase.OID, resHead.OID)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	s.renderBrowse(w, r, "compare.html", compareData{
		browseHeader: s.header(w, r, br, refs, "", ""),
		Base:         baseSpec,
		Head:         headSpec,
		HasResult:    true,
		Cmp:          cmp,
	})
}
```

- [ ] **Step 5: Create `compare.html`** and register it

`internal/web/templates/compare.html`:

```html
{{define "title"}}compare · {{.Tenant}}/{{.Repo}}{{end}}
{{define "content"}}
<div class="browse">
  <div class="repohdr"><span class="path">{{.Tenant}}/{{.Repo}}</span>{{if .CanAdmin}}&nbsp;<a href="/{{.Tenant}}/{{.Repo}}/settings">[settings]</a>{{end}}</div>
  <h2>compare</h2>
  <form class="refswitch" method="get" action="/{{.Tenant}}/{{.Repo}}/compare">
    <select name="base">
      {{range .Refs.Branches}}<option value="{{.Name}}"{{if eq .Name $.Base}} selected{{end}}>{{.Name}}</option>{{end}}
      {{range .Refs.Tags}}<option value="{{.Name}}"{{if eq .Name $.Base}} selected{{end}}>{{.Name}}</option>{{end}}
    </select>
    <span>..</span>
    <select name="head">
      {{range .Refs.Branches}}<option value="{{.Name}}"{{if eq .Name $.Head}} selected{{end}}>{{.Name}}</option>{{end}}
      {{range .Refs.Tags}}<option value="{{.Name}}"{{if eq .Name $.Head}} selected{{end}}>{{.Name}}</option>{{end}}
    </select>
    <button type="submit">compare</button>
  </form>
  {{if .HasResult}}
  <div class="path">{{.Base}}..{{.Head}}</div>
  {{if .Cmp.Files}}
  <p class="hint">{{len .Cmp.Files}} file{{if ne (len .Cmp.Files) 1}}s{{end}} changed, +{{.Cmp.Additions}} −{{.Cmp.Deletions}}</p>
  {{range .Cmp.Files}}{{template "filediff" .}}{{end}}
  {{if .Cmp.Truncated}}<p class="empty">diff truncated (too many files)</p>{{end}}
  {{else}}
  <p class="empty">no differences.</p>
  {{end}}
  {{end}}
</div>
{{end}}
{{template "base" .}}
```

Register `compare.html` in `render.go`'s page list (the same list `commits.html`/`commit.html` are in — `renderBuffered`/`render` 500s on an unregistered page; this is the same registration Task-9 of the prior feature learned about).

The picker `<select>` rendering requires the result page to also carry `.Refs`. On the result branch above, `s.header(..., refs, "", "")` populates `browseHeader.Refs`, so the pickers render on the result page too (enables "re-compare in place"). Confirm `browseHeader` exposes `Refs` (it does — used by the ref-switcher).

- [ ] **Step 6: Run tests**

Run: `go test ./internal/web/ -run TestCompare -v` (fix the renamed `TestCompare_MissingSeparator404`).
Expected: PASS. Then `go test ./internal/web/ 2>&1 | tail -3` and `go build ./...`.

- [ ] **Step 7: Commit**

```bash
git add internal/web/browse.go internal/web/templates/compare.html internal/web/render.go internal/web/browse_test.go
git commit -m "feat(web): compare view (two-dot ref diff) with ref-picker page"
```

---

## Final verification

- [ ] `go test ./... 2>&1 | tail -20` — all green.
- [ ] `go build ./... && go vet ./internal/gitcli/ ./internal/gitbrowse/ ./internal/browsemodel/ ./internal/web/`.
- [ ] `gofmt -l internal/gitcli internal/gitbrowse internal/browsemodel internal/web` — no branch-touched files flagged (pre-existing drift in `internal/buildtrigger/apply.go`/`store_test.go` is out of scope).
- [ ] Manual: serve with `--ui`, push a repo with a couple of branches; visit `/{t}/{r}/compare`, pick base/head → see the diff; click `[history]` on a file and a directory; confirm pager carries the path; confirm all works with JS disabled.

---

## Notes / conventions

- **Anti-enumeration & errors:** compare/history reuse `browseError` (ErrNotFound→404, ErrWarming→warming) exactly like tree/blob/commit; bad refs/paths/malformed URLs → 404. No new authz surface (read views go through the existing `handleBrowse` repo authorization).
- **Caps:** compare reuses `maxDiffPatchBytes` + `parseUnifiedDiff`'s per-file/whole-diff caps → `Truncated`/`TooLarge` render identically to single-commit diffs.
- **No-JS:** the compare picker is a plain GET `<form>` that the handler 303-redirects to the canonical `/compare/<base>..<head>` URL; history and compare results are fully server-rendered.
- **DRY:** `filediff` partial is the single diff renderer; `parseUnifiedDiff`/`parseLog` are reused, not duplicated.
