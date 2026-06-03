# M24 Phase 2 Polish Pass Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Style the Phase 2 browse pages (direction B: minimal rules + tree glyphs), fix the broken white-box chroma highlighting with class-based monokai + line numbers, add tinted diff rows, info enrichment (relative times, humanized sizes, bounded-walk tree last-commit column), the htmx ref-switcher swap, and a strict UI-wide CSP.

**Architecture:** All styling becomes class-based (zero inline styles) to enable the strict CSP. The chroma stylesheet is generated once at startup and served from memory at `/_ui/static/chroma.css`. Shared tree/ref-switcher markup moves to a `_partials.html` parsed alongside `base.html` + each page; a new `renderPartial` serves bare fragments for htmx. The tree last-commit column comes from ONE bounded `git log --name-status` walk per tree page (new `gitcli.LogNameStatus` + `gitbrowse.TreeActivity` + `ContentStore` method).

**Tech Stack:** Go 1.26, html/template, chroma v2 (already a dep), htmx (already vendored). Branch: `m24-phase2-polish` (already created; spec committed as `dfd58c9`).

**Verified current-state anchors (read the file before editing, but these are confirmed):**
- `internal/web/render.go`: `parsePage(fsys, dir, page)` parses `base.html` + one page with FuncMap `{plus1, minus1, urlpath}`; `newRenderer` caches 8 pages (landing/login/error/repo/tree/blob/commits/commit); `renderer.render(w, page, data)`; `staticHandler(dir)` serves `/_ui/static/`. View-models: `browseHeader{base; Tenant, Repo, Ref, OID string; Refs}` with method `RefOrOID()`; `repoHomeData{browseHeader; Entries; ReadmeHTML}`; `treeData{browseHeader; Path; Entries}`; `blobData`; `commitsData{...; Page; HasMore}`; `commitData{...; Detail}`.
- `internal/web/handler.go`: `NewHandler` registers `/_ui/static/` → `staticHandler(d.UIDir)`, `/login`, `/logout`, optional oidc routes, `/` → `handleLanding`; returns `sessionMiddleware(s.store, s.ttl)(s.mux)`.
- `internal/web/browse.go`: `handleTree` does `ListRefs` → `browsemodel.ResolveRest(refs, br.rest)` → `ReadTree` → `renderBrowse(w, r, "tree.html", treeData{...})`. `renderBrowse` buffers, sets `Content-Type: text/html`, emits `web_requests_total`. `handleRaw` sets its own stricter CSP via `w.Header().Set` (which will override any middleware value). `rfc5987Encode` lives here.
- `internal/web/highlight.go`: `highlight(filename, src)` uses `chromahtml.New(chromahtml.WithClasses(false), chromahtml.Standalone(false))` + style "bw" (broken on dark), `plainPre` fallback, `maxHighlightBytes = 1<<20`.
- `internal/web/browse_test.go`: `browseDataStore` (DataStore fake) + `fakeContent` (ContentStore fake with fields refs/tree/blob/log/more/commit/warm). `newBrowseServer(content, visible)`.
- `internal/web/contentstore.go`: `ContentStore` interface = ListRefs/ReadTree/ReadBlob/Log/Commit (Resolve was removed; resolution is `browsemodel.ResolveRest`).
- `internal/gitcli/gitcli.go`: `runCapped(ctx, dir string, capBytes int64, args ...string) ([]byte, error)` + `ErrOutputCapped` (check via `errors.Is` on the returned error — runCapped wraps the sentinel itself); `validRevPath(s)`; `DiffTreePatch` passes `-c core.quotePath=false`.
- `internal/gitbrowse/`: `Service{store, mgr, timeout, logger}` + `openMirror` (returns mirror, release func, error; maps deadline→ErrWarming); fixture builds acme/demo with oids map keys c1/c2/feat/merge and files a.txt, README.md, sub/b.txt, bin.dat, "a file.txt", café.txt (café.txt added in c2; merge branch "merged").
- `internal/browsemodel`: `CommitMeta{OID, ShortOID, Summary, AuthorName, AuthorEmail string; AuthorTime int64}`, `TreeEntry{Name, Path, Mode, Type string; Size int64; OID string}`, `ResolveRest`, `IsHex40`.
- Current templates (repo/tree/blob/commits/commit.html) are committed as of `f1c8164`; repo.html and tree.html contain DUPLICATE tree-table markup that this plan dedups.
- `scripts/web-ui-phase2-smoke-localfs.sh` is the e2e smoke; `internal/web/static/style.css` is the Phase 1 stylesheet (no browse classes yet).

**Conventions:** TDD; `go vet ./...` before each commit; commit messages `feat(web): …`/`fix(...)` style ending with trailer `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. git-binary tests start with `if testing.Short() { t.Skip("requires git binary") }`.

---

### Task 1: Formatting template funcs (`reltime` / `abstime` / `humansize` / `diffclass`)

**Files:**
- Create: `internal/web/format.go`
- Create: `internal/web/format_test.go`
- Modify: `internal/web/render.go` (FuncMap)

- [ ] **Step 1: Write the failing test.** Create `internal/web/format_test.go`:
```go
package web

import (
	"testing"
	"time"
)

func TestRelTimeAt(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	cases := map[int64]string{
		0:                                    "—",
		now.Unix():                           "now",
		now.Add(-30 * time.Second).Unix():    "now",
		now.Add(-5 * time.Minute).Unix():     "5m ago",
		now.Add(-2 * time.Hour).Unix():       "2h ago",
		now.Add(-3 * 24 * time.Hour).Unix():  "3d ago",
		now.Add(-70 * 24 * time.Hour).Unix(): "2mo ago",
		now.Add(-800 * 24 * time.Hour).Unix(): "2y ago",
		now.Add(5 * time.Minute).Unix():      "now", // future clock skew clamps to now
	}
	for in, want := range cases {
		if got := relTimeAt(now, in); got != want {
			t.Errorf("relTimeAt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAbsTime(t *testing.T) {
	ts := time.Date(2026, 6, 3, 18, 44, 0, 0, time.UTC).Unix()
	if got := absTime(ts); got != "2026-06-03 18:44 UTC" {
		t.Fatalf("absTime = %q", got)
	}
	if got := absTime(0); got != "" {
		t.Fatalf("absTime(0) = %q, want empty", got)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:           "0 B",
		312:         "312 B",
		1024:        "1.0 KiB",
		1228:        "1.2 KiB",
		10 * 1024:   "10 KiB",
		4 << 20:     "4.0 MiB",
		1181116006:  "1.1 GiB",
	}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestDiffClass(t *testing.T) {
	cases := map[byte]string{'+': "add", '-': "del", ' ': "ctx", 'x': "ctx"}
	for in, want := range cases {
		if got := diffClass(in); got != want {
			t.Errorf("diffClass(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify FAIL:** `go test ./internal/web/ -run 'RelTime|AbsTime|HumanSize|DiffClass'` → undefined identifiers.

- [ ] **Step 3: Create `internal/web/format.go`:**
```go
package web

import (
	"fmt"
	"time"
)

// relTimeAt renders a coarse "2h ago" relative time against now. Zero/negative
// timestamps render "—"; future timestamps (clock skew) clamp to "now".
func relTimeAt(now time.Time, unix int64) string {
	if unix <= 0 {
		return "—"
	}
	d := now.Sub(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/(24*365)))
	}
}

// absTime renders the absolute UTC time for tooltips; empty for zero.
func absTime(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04") + " UTC"
}

// humanSize renders byte counts in binary units, one decimal under 10.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	val := float64(n) / float64(div)
	suffix := []string{"KiB", "MiB", "GiB", "TiB"}[exp]
	if val < 10 {
		return fmt.Sprintf("%.1f %s", val, suffix)
	}
	return fmt.Sprintf("%.0f %s", val, suffix)
}

// diffClass maps a DiffLine.Kind byte to its CSS class.
func diffClass(kind byte) string {
	switch kind {
	case '+':
		return "add"
	case '-':
		return "del"
	default:
		return "ctx"
	}
}
```
NOTE: the future-skew case (`d < 0`) falls into `d < time.Minute` → "now" — that is the intended clamp. The `10 KiB` case prints `%.0f` → "10 KiB".

- [ ] **Step 4: Register the funcs in `internal/web/render.go` `parsePage` FuncMap** (alongside plus1/minus1/urlpath):
```go
		"reltime":   func(unix int64) string { return relTimeAt(time.Now(), unix) },
		"abstime":   absTime,
		"humansize": humanSize,
		"diffclass": func(kind byte) string { return diffClass(kind) },
```
Add `"time"` to render.go imports.

- [ ] **Step 5: Run** `go test ./internal/web/...` → PASS (all existing tests too). `go vet ./internal/web/...`.

- [ ] **Step 6: Commit:** `git add internal/web/format.go internal/web/format_test.go internal/web/render.go && git commit -m "feat(web): reltime/abstime/humansize/diffclass template funcs"` (+ trailer).

---

### Task 2: Diff line classes in commit.html

**Files:**
- Modify: `internal/web/templates/commit.html`
- Modify: `internal/web/browse_test.go`

- [ ] **Step 1: Write the failing test.** Append to `internal/web/browse_test.go`:
```go
func TestCommit_DiffLineClasses(t *testing.T) {
	content := &fakeContent{
		commit: browsemodel.CommitDetail{
			Meta:    browsemodel.CommitMeta{OID: "c2", ShortOID: "c2", Summary: "update a", AuthorName: "Ann", AuthorTime: 1700000000},
			Message: "update a\n",
			Files: []browsemodel.FileDiff{{
				NewPath: "a.txt", Status: "M", Additions: 1, Deletions: 1,
				Hunks: []browsemodel.Hunk{{Header: "@@ -1 +1 @@", Lines: []browsemodel.DiffLine{
					{Kind: ' ', Text: "ctx line"},
					{Kind: '-', Text: "old"},
					{Kind: '+', Text: "new"},
				}}},
			}},
		},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/commit/c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{`class="dl ctx"`, `class="dl del"`, `class="dl add"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in commit view: %s", want, body)
		}
	}
	if strings.Contains(body, `class="dl k`) {
		t.Errorf("old k-class scheme still present")
	}
}
```

- [ ] **Step 2: Run to verify FAIL:** `go test ./internal/web/ -run TestCommit_DiffLineClasses`.

- [ ] **Step 3: Edit `internal/web/templates/commit.html`** — replace the diff-line element:
```html
        {{range .Lines}}<div class="dl {{diffclass .Kind}}">{{printf "%c" .Kind}}{{.Text}}</div>{{end}}
```
(was `<div class="dl k{{printf "%c" .Kind}}">…`). Leave the leading glyph rendering as-is.

- [ ] **Step 4: Run** `go test ./internal/web/...` → PASS.

- [ ] **Step 5: Commit:** `git add internal/web/templates/commit.html internal/web/browse_test.go && git commit -m "feat(web): named diff-line classes (add/del/ctx)"` (+ trailer).

---

### Task 3: Class-based monokai highlighting + line numbers + served chroma.css

**Files:**
- Modify: `internal/web/highlight.go`
- Modify: `internal/web/render.go` (chroma.css handler helper) and `internal/web/handler.go` (route)
- Modify: `internal/web/templates/base.html` (stylesheet link)
- Modify: `internal/web/browse_test.go`, Create: `internal/web/highlight_test.go`

- [ ] **Step 1: Write the failing tests.** Create `internal/web/highlight_test.go`:
```go
package web

import (
	"strings"
	"testing"
)

func TestHighlight_ClassBasedWithLineNumbers(t *testing.T) {
	out := string(highlight("main.go", []byte("package main // <x>\nfunc main() {}\n")))
	if !strings.Contains(out, `class="chroma"`) {
		t.Fatalf("expected class-based chroma output: %s", out)
	}
	if strings.Contains(out, "style=") {
		t.Fatalf("inline styles present (breaks strict CSP): %s", out)
	}
	if strings.Contains(out, "<x>") {
		t.Fatalf("content not escaped: %s", out)
	}
	// Line-number table mode emits .lnt (line number table) cells.
	if !strings.Contains(out, "lnt") {
		t.Fatalf("expected line-number markup: %s", out)
	}
}

func TestChromaCSS_NonEmpty(t *testing.T) {
	css := string(chromaCSS())
	if !strings.Contains(css, ".chroma") {
		t.Fatalf("chroma CSS missing .chroma selectors: %.200s", css)
	}
}
```
Append a route test to `internal/web/browse_test.go`:
```go
func TestChromaCSSRoute(t *testing.T) {
	h := newBrowseServer(&fakeContent{}, map[string]bool{})
	req := httptest.NewRequest("GET", "/_ui/static/chroma.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), ".chroma") {
		t.Fatalf("chroma.css route: code=%d body=%.120s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("content-type = %q", ct)
	}
}
```

- [ ] **Step 2: Run to verify FAIL:** `go test ./internal/web/ -run 'Highlight_Class|ChromaCSS'`.

- [ ] **Step 3: Rewrite `internal/web/highlight.go`:**
```go
package web

import (
	"bytes"
	"html"
	"html/template"
	"sync"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// maxHighlightBytes caps source size eligible for syntax highlighting; larger
// text blobs render as plain escaped <pre>.
const maxHighlightBytes = 1 << 20 // 1 MiB

// chromaStyleName is the dark scheme chosen in the polish-pass design.
const chromaStyleName = "monokai"

func chromaStyle() *chroma.Style {
	if s := styles.Get(chromaStyleName); s != nil {
		return s
	}
	return styles.Fallback
}

// chromaFormatter is class-based (zero inline styles — required by the strict
// UI CSP) with line numbers in a separate table column so selection/copy
// excludes them.
func chromaFormatter() *chromahtml.Formatter {
	return chromahtml.New(
		chromahtml.WithClasses(true),
		chromahtml.WithLineNumbers(true),
		chromahtml.LineNumbersInTable(true),
		chromahtml.Standalone(false),
	)
}

var (
	chromaCSSOnce  sync.Once
	chromaCSSBytes []byte
)

// chromaCSS renders the monokai stylesheet once, with overrides that blend the
// code frame into the page theme and dim the line-number column.
func chromaCSS() []byte {
	chromaCSSOnce.Do(func() {
		var b bytes.Buffer
		_ = chromaFormatter().WriteCSS(&b, chromaStyle())
		b.WriteString("\n/* bucketvcs overrides */\n")
		b.WriteString(".chroma{background-color:#111;border:1px solid #333;padding:.5rem;overflow-x:auto}\n")
		b.WriteString(".chroma .lnt,.chroma .ln{color:#666;-webkit-user-select:none;user-select:none}\n")
		chromaCSSBytes = b.Bytes()
	})
	return chromaCSSBytes
}

// plainPre returns an HTML-escaped <pre> block (the safe fallback).
func plainPre(src []byte) template.HTML {
	return template.HTML("<pre class=\"blob\">" + html.EscapeString(string(src)) + "</pre>")
}

// highlight returns syntax-highlighted HTML for a text blob, chosen by filename.
// Output is class-based (styled by /_ui/static/chroma.css) and escaped by the
// chroma tokeniser, so it is safe to mark template.HTML. Oversized input or any
// tokeniser/format error falls back to an HTML-escaped <pre>.
func highlight(filename string, src []byte) template.HTML {
	if len(src) > maxHighlightBytes {
		return plainPre(src)
	}
	lexer := lexers.Match(filename)
	if lexer == nil {
		lexer = lexers.Analyse(string(src))
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)

	it, err := lexer.Tokenise(nil, string(src))
	if err != nil {
		return plainPre(src)
	}
	var buf bytes.Buffer
	if err := chromaFormatter().Format(&buf, chromaStyle(), it); err != nil {
		return plainPre(src)
	}
	return template.HTML(buf.String())
}
```

- [ ] **Step 4: Serve the CSS.** In `internal/web/render.go` add:
```go
// chromaCSSHandler serves the generated highlight stylesheet. With dir != ""
// a static/chroma.css file on disk overrides the generated one (theming hook).
func chromaCSSHandler(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dir != "" {
			if b, err := os.ReadFile(filepath.Join(dir, "static", "chroma.css")); err == nil {
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(b)
				return
			}
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(chromaCSS())
	}
}
```
In `internal/web/handler.go` `NewHandler`, BEFORE the `/_ui/static/` registration, add:
```go
	s.mux.HandleFunc("/_ui/static/chroma.css", chromaCSSHandler(d.UIDir))
```
(Exact-path mux entries beat the `/_ui/static/` prefix entry regardless of order, but keep it adjacent for readability.)

- [ ] **Step 5: Link it.** In `internal/web/templates/base.html`, after the existing style.css `<link>`, add:
```html
<link rel="stylesheet" href="/_ui/static/chroma.css">
```

- [ ] **Step 6: Run** `go test ./internal/web/...` → ALL PASS (the existing `TestBlob_HighlightedAndEscaped` keeps passing — class-based output still escapes). `go vet ./internal/web/...`.

- [ ] **Step 7: Commit:** `git add internal/web/highlight.go internal/web/highlight_test.go internal/web/render.go internal/web/handler.go internal/web/templates/base.html internal/web/browse_test.go && git commit -m "feat(web): class-based monokai highlighting with line numbers; serve generated chroma.css"` (+ trailer).

---

### Task 4: Shared partials renderer (`_partials.html` + `renderPartial`) and template dedup

**Files:**
- Create: `internal/web/templates/_partials.html`
- Modify: `internal/web/render.go` (parsePage signature/behavior, renderer.partials, renderPartial)
- Modify: `internal/web/templates/repo.html`, `internal/web/templates/tree.html`
- Modify: `internal/web/render_test.go` or `browse_test.go` (tests)

- [ ] **Step 1: Write the failing test.** Append to `internal/web/browse_test.go`:
```go
func TestRenderPartial_TreeRows(t *testing.T) {
	r, err := newRenderer("")
	if err != nil {
		t.Fatal(err)
	}
	data := treeData{
		browseHeader: browseHeader{Tenant: "acme", Repo: "demo", Ref: "main"},
		Path:         "",
		Entries:      []browsemodel.TreeEntry{{Name: "a.txt", Path: "a.txt", Type: "blob", Size: 6, OID: "x"}},
	}
	var buf bytes.Buffer
	if err := r.renderPartial(&buf, "treeRows", data); err != nil {
		t.Fatalf("renderPartial: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `id="tree"`) || !strings.Contains(out, "a.txt") {
		t.Fatalf("partial missing container/entry: %s", out)
	}
	if strings.Contains(out, "<html") {
		t.Fatalf("partial must not include the base layout: %s", out)
	}
}
```
Add `"bytes"` to the test file imports if missing.

- [ ] **Step 2: Run to verify FAIL:** `go test ./internal/web/ -run TestRenderPartial`.

- [ ] **Step 3: Create `internal/web/templates/_partials.html`** (shared fragments; defines only — no top-level output, no title/content):
```html
{{define "treeRows"}}<div id="tree">
  {{if .Entries}}
  <table class="tree">
    {{range .Entries}}
    <tr>
      {{if eq .Type "tree"}}<td class="kind">dir</td><td class="name"><a href="/{{$.Tenant}}/{{$.Repo}}/tree/{{urlpath $.RefOrOID}}/{{urlpath .Path}}">{{.Name}}/</a></td><td class="size"></td>{{template "treeAge" (index $.Activity .Path)}}
      {{else if eq .Type "blob"}}<td class="kind">file</td><td class="name"><a href="/{{$.Tenant}}/{{$.Repo}}/blob/{{urlpath $.RefOrOID}}/{{urlpath .Path}}">{{.Name}}</a></td><td class="size">{{humansize .Size}}</td>{{template "treeAge" (index $.Activity .Path)}}
      {{else}}<td class="kind">mod</td><td class="name">{{.Name}}</td><td class="size"></td>{{template "treeAge" (index $.Activity .Path)}}{{end}}
    </tr>
    {{end}}
  </table>
  {{else}}<p class="empty">{{if .Path}}empty directory{{else}}empty repository{{end}}</p>{{end}}
</div>{{end}}

{{define "treeAge"}}{{if .OID}}<td class="age" title="{{abstime .AuthorTime}}"><span class="lastmsg">{{.Summary}}</span> · {{reltime .AuthorTime}}</td>{{else}}<td class="age">—</td>{{end}}{{end}}

{{define "refswitcher"}}<form class="refswitch" method="get" action="/{{.Tenant}}/{{.Repo}}/tree/" hx-get="/{{.Tenant}}/{{.Repo}}/tree/" hx-target="#tree" hx-swap="outerHTML" hx-push-url="true" hx-trigger="change">
  <select name="ref">
    {{range .Refs.Branches}}<option value="{{.Name}}" {{if eq .Name $.Ref}}selected{{end}}>{{.Name}}</option>{{end}}
    {{range .Refs.Tags}}<option value="{{.Name}}" {{if eq .Name $.Ref}}selected{{end}}>tag: {{.Name}}</option>{{end}}
  </select>
  <noscript><button type="submit">go</button></noscript>
</form>{{end}}
```
NOTE: `treeRows` references `$.Activity` (a `map[string]browsemodel.CommitMeta`) — added to the view-models in Task 7; in THIS task add the field now so the template parses and renders the "—" branch (`index` of a nil map returns a zero `CommitMeta`, whose `.OID` is empty → "—" branch). `repoHomeData` and `treeData` both need it; add to `browseHeader` instead so one field serves both:
```go
// in render.go browseHeader:
	Activity map[string]browsemodel.CommitMeta // entry path -> last commit (best-effort; nil ⇒ "—")
```
ALSO: `treeData.Path` is referenced by the partial's `{{if .Path}}` empty-state branch, but `repoHomeData` has NO `Path` field — html/template errors on missing fields. Fix by moving `Path string` from `treeData` into `browseHeader` (delete it from `treeData`; `handleTree` sets it via the header literal or a named field — adjust `handleTree`'s `treeData{...}` literal accordingly; repo home leaves it ""). Read render.go and browse.go and make the moves compile.

- [ ] **Step 4: Renderer parses partials.** In `internal/web/render.go`:
  - `parsePage`: parse THREE files — change the tail to:
```go
	partials := "_partials.html"
	if dir != "" && dir != "." {
		partials = dir + "/_partials.html"
	}
	return template.New("").Funcs(funcs).ParseFS(fsys, base, partials, pg)
```
  (Extract the FuncMap construction into a package-level `func templateFuncs() template.FuncMap` so the next step reuses it without duplication.)
  - Add a partials-only set + method:
```go
// partialsSet parses just _partials.html (no base/page) for fragment rendering.
func partialsSet(fsys fs.FS, dir string) (*template.Template, error) {
	p := "_partials.html"
	if dir != "" && dir != "." {
		p = dir + "/_partials.html"
	}
	return template.New("").Funcs(templateFuncs()).ParseFS(fsys, p)
}

// renderPartial renders a named fragment from _partials.html (htmx swaps).
func (r *renderer) renderPartial(w io.Writer, name string, data any) error {
	if r.dir != "" {
		t, err := partialsSet(os.DirFS(filepath.Join(r.dir, "templates")), ".")
		if err != nil {
			return err
		}
		return t.ExecuteTemplate(w, name, data)
	}
	return r.partials.ExecuteTemplate(w, name, data)
}
```
  - `renderer` struct gains `partials *template.Template`; `newRenderer` (embedded mode) sets it via `partialsSet(assetsFS, "templates")` (error → return err).

- [ ] **Step 5: Dedup the pages.** In `repo.html` and `tree.html`, replace the entire `{{if .Entries}}<table class="tree">…{{end}}` block (including the empty-state `<p>`) with:
```html
  {{template "treeRows" .}}
```
and add the switcher into both repohdr lines:
```html
  <div class="repohdr"><span class="path">{{.Tenant}}/{{.Repo}}</span>{{template "refswitcher" .}}</div>
```
Keep the `refs` line (branch/tag links) in both pages — it remains the no-JS-visible ref list; the switcher is the compact control. (If that feels redundant visually it gets resolved by CSS in Task 8 — the refs line becomes dim/secondary.)

- [ ] **Step 6: Run** `go test ./internal/web/...` → ALL PASS (existing TestRepoHome_RendersTree / TestTree_RendersPathEntries exercise the dedup; fix any field-move fallout). `go vet`.

- [ ] **Step 7: Commit:** `git add internal/web/templates/_partials.html internal/web/templates/repo.html internal/web/templates/tree.html internal/web/render.go internal/web/browse.go internal/web/browse_test.go && git commit -m "feat(web): shared treeRows/refswitcher partials + renderPartial; dedup tree markup"` (+ trailer).

---

### Task 5: htmx ref-switcher swap (`?ref=` + HX-Request fragment)

**Files:**
- Modify: `internal/web/browse.go` (`handleTree`)
- Modify: `internal/web/browse_test.go`

- [ ] **Step 1: Write the failing tests.** Append:
```go
func TestTree_QueryRefSelectsRef(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{
			{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"},
			{Name: "dev", OID: "1234567890123456789012345678901234567890"},
		}},
		tree: []browsemodel.TreeEntry{{Name: "a.txt", Path: "a.txt", Type: "blob", OID: "x"}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/tree/?ref=dev", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("?ref= tree: code %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "tree/dev/") {
		t.Fatalf("expected links on ref dev: %s", rec.Body.String())
	}
}

func TestTree_HXRequestReturnsFragment(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		tree: []browsemodel.TreeEntry{{Name: "a.txt", Path: "a.txt", Type: "blob", OID: "x"}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/tree/main", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Fatalf("HX-Request should get a bare fragment: %s", body)
	}
	if !strings.Contains(body, `id="tree"`) || !strings.Contains(body, "a.txt") {
		t.Fatalf("fragment missing tree content: %s", body)
	}
}
```

- [ ] **Step 2: Run to verify FAIL.**

- [ ] **Step 3: Modify `handleTree`** in `internal/web/browse.go`. After the `ListRefs` call and before resolution, insert the `?ref=` override; after building `data`, branch on HX-Request:
```go
	rest := br.rest
	if qr := r.URL.Query().Get("ref"); qr != "" {
		// The ref-switcher form serializes the select as ?ref=<name> (both the
		// htmx request and the no-JS GET submit). It navigates to the selected
		// ref's root.
		rest = qr
	}
	res, err := browsemodel.ResolveRest(refs, rest)
```
(`rest` replaces `br.rest` in the ResolveRest call only; ReadTree continues to use `res.OID, res.Path`.) Then replace the final `s.renderBrowse(...)` with:
```go
	data := treeData{ /* existing fields incl. header with Activity (Task 7) */ }
	if r.Header.Get("HX-Request") == "true" {
		var buf bytes.Buffer
		if err := s.render.renderPartial(&buf, "treeRows", data); err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "render error")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = buf.WriteTo(w)
		EmitRequestMetric(r.Context(), s.logger, "tree", http.StatusOK)
		return
	}
	s.renderBrowse(w, r, "tree.html", data)
```
(Read the current handleTree body and keep its existing field assignments; `bytes` is already imported in browse.go.)

- [ ] **Step 4: Run** `go test ./internal/web/...` → PASS. Also confirm `TestBrowse_Routing`'s `/acme/demo/tree/main/sub` still 200s.

- [ ] **Step 5: Commit:** `git add internal/web/browse.go internal/web/browse_test.go && git commit -m "feat(web): htmx tree swap via HX-Request fragment + ?ref= switcher fallback"` (+ trailer).

---

### Task 6: `gitcli.LogNameStatus` (bounded name-status walk)

**Files:**
- Modify: `internal/gitcli/gitcli.go`
- Modify: `internal/gitcli/browse_test.go`

- [ ] **Step 1: Write the failing test.** Append to `internal/gitcli/browse_test.go`:
```go
func TestLogNameStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	bare, oid := makeBrowseBare(t)
	out, err := LogNameStatus(context.Background(), bare, oid, 10, "")
	if err != nil {
		t.Fatalf("LogNameStatus: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "\x1e") || !strings.Contains(s, "\x1f") {
		t.Fatalf("missing record/field separators: %q", s)
	}
	if !strings.Contains(s, "A\ta.txt") {
		t.Fatalf("missing name-status entry for a.txt: %q", s)
	}
	// Scoped to a directory: only sub/ entries appear.
	scoped, err := LogNameStatus(context.Background(), bare, oid, 10, "sub")
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if !strings.Contains(string(scoped), "sub/b.txt") || strings.Contains(string(scoped), "A\ta.txt") {
		t.Fatalf("scoping wrong: %q", scoped)
	}
}

func TestLogNameStatus_RejectsBadArgs(t *testing.T) {
	if _, err := LogNameStatus(context.Background(), "/tmp", "--evil", 10, ""); err == nil {
		t.Fatal("flag-like oid accepted")
	}
	if _, err := LogNameStatus(context.Background(), "/tmp", "abc", 10, "-evil"); err == nil {
		t.Fatal("flag-like scope path accepted")
	}
}
```

- [ ] **Step 2: Run to verify FAIL.**

- [ ] **Step 3: Append to `internal/gitcli/gitcli.go`:**
```go
// maxLogNameStatusBytes caps the bounded attribution walk used by the web
// tree view's last-commit column.
const maxLogNameStatusBytes = 8 << 20 // 8 MiB

// LogNameStatus returns up to max commits reachable from oid with the paths
// each touched, as 0x1e-separated records:
//
//	0x1e <oid> 0x1f <author-name> 0x1f <author-email> 0x1f <unixtime> 0x1f <subject> \n
//	<STATUS>\t<path>\n ...
//
// Renames are disabled (--no-renames: a rename reports as A+D, so the rename
// commit is the path's last touch) and paths are emitted verbatim
// (core.quotePath=false). scopePath, when non-empty, restricts the walk to
// commits touching that directory. Output is byte-capped; on overflow the
// captured prefix is returned with an error wrapping ErrOutputCapped.
func LogNameStatus(ctx context.Context, dir, oid string, max int, scopePath string) ([]byte, error) {
	if !validRefOrOID(oid) {
		return nil, fmt.Errorf("gitcli: LogNameStatus: invalid oid %q", oid)
	}
	if max <= 0 {
		return nil, fmt.Errorf("gitcli: LogNameStatus: bad max %d", max)
	}
	args := []string{
		"-c", "core.quotePath=false", "--no-replace-objects", "log", oid,
		fmt.Sprintf("--max-count=%d", max), "--name-status", "--no-color",
		"--no-renames", "--pretty=format:%x1e%H%x1f%an%x1f%ae%x1f%at%x1f%s",
	}
	if scopePath != "" {
		if !validRevPath(scopePath) {
			return nil, fmt.Errorf("gitcli: LogNameStatus: invalid scope path %q", scopePath)
		}
		args = append(args, "--", scopePath+"/")
	}
	return runCapped(ctx, dir, maxLogNameStatusBytes, args...)
}
```
NOTE: in `makeBrowseBare` the single commit ADDS files, so name-status shows `A\ta.txt` — that is what the test asserts. If the fixture's commit shape differs, read it and match the assertion to reality rather than changing the helper.

- [ ] **Step 4: Run** `go test ./internal/gitcli/ -run LogNameStatus` → PASS; full `go test ./internal/gitcli/...` → PASS.

- [ ] **Step 5: Commit:** `git add internal/gitcli/gitcli.go internal/gitcli/browse_test.go && git commit -m "feat(gitcli): bounded LogNameStatus walk for tree last-commit attribution"` (+ trailer).

---

### Task 7: `gitbrowse.TreeActivity` + `ContentStore` method + handler wiring

**Files:**
- Create: `internal/gitbrowse/activity.go`, `internal/gitbrowse/activity_test.go`
- Modify: `internal/web/contentstore.go`, `internal/web/browse.go`, `internal/web/browse_test.go`

- [ ] **Step 1: Write the failing gitbrowse test.** Create `internal/gitbrowse/activity_test.go`:
```go
package gitbrowse

import (
	"context"
	"testing"
)

func TestTreeActivity_RootAttribution(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	act, err := svc.TreeActivity(context.Background(), tenant, repo, oids["c2"], "")
	if err != nil {
		t.Fatalf("TreeActivity: %v", err)
	}
	// a.txt was last touched by c2 ("update a"); sub/ by c1 ("init").
	a, ok := act["a.txt"]
	if !ok || a.Summary != "update a" || a.OID != oids["c2"] {
		t.Fatalf("a.txt attribution = %+v (ok=%v)", a, ok)
	}
	sub, ok := act["sub"]
	if !ok || sub.Summary != "init" {
		t.Fatalf("sub attribution = %+v (ok=%v)", sub, ok)
	}
}

func TestTreeActivity_ScopedToSubdir(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	act, err := svc.TreeActivity(context.Background(), tenant, repo, oids["c2"], "sub")
	if err != nil {
		t.Fatalf("TreeActivity sub: %v", err)
	}
	if m, ok := act["sub/b.txt"]; !ok || m.Summary != "init" {
		t.Fatalf("sub/b.txt attribution = %+v (ok=%v)", m, ok)
	}
	if _, ok := act["a.txt"]; ok {
		t.Fatal("root file leaked into scoped activity")
	}
}

func TestParseNameStatusWalk(t *testing.T) {
	raw := "\x1e" + "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1\x1fAnn\x1fann@x\x1f1700000000\x1fsecond\n" +
		"M\tdir/x.txt\n" +
		"\x1e" + "e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0\x1fAnn\x1fann@x\x1f1699999999\x1ffirst\n" +
		"A\ta.txt\nA\tdir/x.txt\n"
	recs := parseNameStatusWalk([]byte(raw))
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0].meta.Summary != "second" || len(recs[0].paths) != 1 || recs[0].paths[0] != "dir/x.txt" {
		t.Fatalf("rec0 = %+v", recs[0])
	}
	if recs[1].meta.AuthorTime != 1699999999 || len(recs[1].paths) != 2 {
		t.Fatalf("rec1 = %+v", recs[1])
	}
}
```

- [ ] **Step 2: Run to verify FAIL:** `go test ./internal/gitbrowse/ -run 'TreeActivity|NameStatusWalk'`.

- [ ] **Step 3: Create `internal/gitbrowse/activity.go`:**
```go
package gitbrowse

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// treeActivityWindow bounds the attribution walk: entries not touched within
// the most recent N commits render without a last-commit annotation.
const treeActivityWindow = 200

type walkRecord struct {
	meta  browsemodel.CommitMeta
	paths []string
}

// TreeActivity returns, for each direct child of the listed directory, the most
// recent commit touching it — attributed from a single bounded history walk.
// Keys are entry paths as ReadTree produces them (e.g. "sub" or "sub/b.txt"
// when listing "sub"). Entries untouched within the window are absent.
func (s *Service) TreeActivity(ctx context.Context, tenant, repoID, oid, path string) (map[string]browsemodel.CommitMeta, error) {
	clean := strings.Trim(path, "/")
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return nil, err
	}
	defer release()

	raw, err := gitcli.LogNameStatus(ctx, m.BareDir(), oid, treeActivityWindow, clean)
	if err != nil && !errors.Is(err, gitcli.ErrOutputCapped) {
		return nil, err
	}
	out := map[string]browsemodel.CommitMeta{}
	for _, rec := range parseNameStatusWalk(raw) {
		for _, p := range rec.paths {
			key, ok := childKey(clean, p)
			if !ok {
				continue
			}
			if _, seen := out[key]; !seen {
				out[key] = rec.meta // records are newest-first; first wins
			}
		}
	}
	return out, nil
}

// childKey maps a touched repo path to the listing entry it belongs to: the
// direct child of dir ("" = root). For dir="": "a/b/c" -> "a", "x.txt" -> "x.txt".
// For dir="sub": "sub/b.txt" -> "sub/b.txt", "sub/d/e" -> "sub/d"; paths outside
// dir return ok=false.
func childKey(dir, p string) (string, bool) {
	rel := p
	if dir != "" {
		if !strings.HasPrefix(p, dir+"/") {
			return "", false
		}
		rel = p[len(dir)+1:]
	}
	if rel == "" {
		return "", false
	}
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		rel = rel[:i]
	}
	if dir != "" {
		return dir + "/" + rel, true
	}
	return rel, true
}

// parseNameStatusWalk parses gitcli.LogNameStatus output (0x1e records: header
// line with 0x1f fields, then STATUS\tpath lines).
func parseNameStatusWalk(raw []byte) []walkRecord {
	var out []walkRecord
	for _, rec := range strings.Split(string(raw), "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		lines := strings.Split(rec, "\n")
		f := strings.Split(lines[0], "\x1f")
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
		w := walkRecord{meta: browsemodel.CommitMeta{
			OID: oid, ShortOID: short, AuthorName: f[1], AuthorEmail: f[2],
			AuthorTime: at, Summary: f[4],
		}}
		for _, ln := range lines[1:] {
			tab := strings.IndexByte(ln, '\t')
			if tab <= 0 {
				continue
			}
			w.paths = append(w.paths, ln[tab+1:])
		}
		out = append(out, w)
	}
	return out
}
```

- [ ] **Step 4: Run gitbrowse tests** → PASS.

- [ ] **Step 5: Wire the interface + handlers.**
  - `internal/web/contentstore.go`: add to `ContentStore`:
```go
	TreeActivity(ctx context.Context, tenant, repo, oid, path string) (map[string]browsemodel.CommitMeta, error)
```
  - `internal/web/browse_test.go`: add to `fakeContent` a field `activity map[string]browsemodel.CommitMeta` and method:
```go
func (f *fakeContent) TreeActivity(ctx context.Context, t, r, oid, p string) (map[string]browsemodel.CommitMeta, error) {
	return f.activity, nil
}
```
  - `internal/web/browse.go`: in `handleRepoHome` and `handleTree`, after the successful `ReadTree`, add:
```go
	activity, aerr := s.content.TreeActivity(r.Context(), br.tenant, br.repo, res.OID, res.Path)
	if aerr != nil {
		// Best-effort column: log and render "—" rather than failing the page.
		s.logger.WarnContext(r.Context(), "tree activity failed", "tenant", br.tenant, "repo", br.repo, "err", aerr)
		activity = nil
	}
```
(for repo home the path argument is `""`). Set `Activity: activity` in the `browseHeader` literal each handler builds (via `s.header(...)` return value: assign `h := s.header(...); h.Activity = activity` then use `h`).

- [ ] **Step 6: Web test.** Append:
```go
func TestTree_ActivityColumnRendered(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		tree: []browsemodel.TreeEntry{
			{Name: "a.txt", Path: "a.txt", Type: "blob", Size: 6, OID: "x"},
			{Name: "old.txt", Path: "old.txt", Type: "blob", Size: 1, OID: "y"},
		},
		activity: map[string]browsemodel.CommitMeta{
			"a.txt": {OID: "abc", Summary: "update a", AuthorTime: 1700000000},
		},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/tree/main", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "update a") {
		t.Fatalf("attributed entry missing summary: %s", body)
	}
	if !strings.Contains(body, "—") {
		t.Fatalf("unattributed entry should render —: %s", body)
	}
}
```

- [ ] **Step 7: Run** `go test ./internal/web/... ./internal/gitbrowse/...` → ALL PASS. `go build ./... && go vet ./...` (the interface addition ripples only to fakeContent — `gitbrowse.Service` already satisfies it).

- [ ] **Step 8: Commit:** `git add internal/gitbrowse/activity.go internal/gitbrowse/activity_test.go internal/web/contentstore.go internal/web/browse.go internal/web/browse_test.go && git commit -m "feat(gitbrowse,web): bounded-walk tree last-commit column (TreeActivity)"` (+ trailer).

---

### Task 8: Enrichment in commits/commit/blob templates

**Files:**
- Modify: `internal/web/templates/commits.html`, `commit.html`, `blob.html`
- Modify: `internal/web/browse_test.go`

- [ ] **Step 1: Write the failing test.** Append:
```go
func TestCommits_AgeColumn(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		log:  []browsemodel.CommitMeta{{OID: "c2", ShortOID: "c2", Summary: "update a", AuthorName: "Ann", AuthorTime: time.Now().Add(-2 * time.Hour).Unix()}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/commits/main", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "2h ago") {
		t.Fatalf("commit log missing relative age: %s", rec.Body.String())
	}
}

func TestBlob_HumanizedSize(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		blob: browsemodel.Blob{Path: "bin.dat", Size: 4 << 20, Binary: true, Bytes: []byte{0}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/blob/main/bin.dat", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "4.0 MiB") {
		t.Fatalf("binary notice missing humanized size: %s", rec.Body.String())
	}
}
```
Add `"time"` to the test imports if missing.

- [ ] **Step 2: Run to verify FAIL.**

- [ ] **Step 3: Template edits.**
  - `commits.html`: add an age cell after the author cell:
```html
      <td class="who">{{.AuthorName}}</td>
      <td class="age" title="{{abstime .AuthorTime}}">{{reltime .AuthorTime}}</td>
```
  - `commit.html`: extend the `who` line:
```html
    <div class="who">{{.Detail.Meta.ShortOID}} · {{.Detail.Meta.AuthorName}} &lt;{{.Detail.Meta.AuthorEmail}}&gt; · <span title="{{abstime .Detail.Meta.AuthorTime}}">{{reltime .Detail.Meta.AuthorTime}}</span></div>
```
  - `blob.html`: replace both `({{.Blob.Size}} bytes)` occurrences with `({{humansize .Blob.Size}})`.

- [ ] **Step 4: Run** `go test ./internal/web/...` → ALL PASS (adjust any existing test that asserted raw `bytes` strings — read failures and fix assertions to the new output).

- [ ] **Step 5: Commit:** `git add internal/web/templates/ internal/web/browse_test.go && git commit -m "feat(web): relative ages + humanized sizes across browse views"` (+ trailer).

---

### Task 9: Direction-B stylesheet

**Files:**
- Modify: `internal/web/static/style.css`
- Modify: `internal/web/templates/base.html` (only if a wrapper class is needed — see Step 1)

- [ ] **Step 1: Append the browse styling to `internal/web/static/style.css`:**
```css
/* ── Phase 2 browse (direction B: minimal rules + tree glyphs) ─────────── */
main:has(.browse) { max-width: 100ch; }

.browse .repohdr { display: flex; justify-content: space-between; align-items: baseline;
  border-bottom: 1px solid var(--dim); padding-bottom: .4rem; margin-bottom: .6rem; }
.browse .repohdr .path { color: var(--accent); }
.browse .refs { color: var(--dim); font-size: .92em; margin-bottom: 1rem; }
.browse .refs a { color: var(--accent); }
.browse > .path { color: var(--fg); margin: .6rem 0; }

.refswitch { display: inline-flex; gap: .4rem; align-items: center; }
.refswitch select { background: #111; color: var(--fg); border: 1px solid var(--dim);
  font: inherit; padding: .1rem .4rem; }

table.tree { border-collapse: collapse; width: 100%; }
table.tree td { padding: .1rem .6rem .1rem 0; vertical-align: baseline; }
table.tree td.kind { color: var(--dim); width: 4ch; white-space: nowrap; }
table.tree td.kind::before { content: "├─ "; color: var(--dim); }
table.tree tr:last-child td.kind::before { content: "└─ "; }
table.tree td.name a { color: var(--accent); }
table.tree td.size { color: var(--dim); text-align: right; white-space: nowrap; width: 9ch; }
table.tree td.age { color: var(--dim); text-align: right; white-space: nowrap;
  max-width: 38ch; overflow: hidden; text-overflow: ellipsis; }
table.tree td.age .lastmsg { overflow: hidden; text-overflow: ellipsis; }

.readme { margin-top: 1.5rem; border-top: 1px dashed var(--dim); padding-top: .8rem;
  max-width: 72ch; }
.readme img { max-width: 100%; }

table.commits { border-collapse: collapse; width: 100%; }
table.commits td { padding: .15rem .8rem .15rem 0; vertical-align: baseline; }
table.commits td.oid { width: 13ch; }
table.commits td.who, table.commits td.age { color: var(--dim); white-space: nowrap; }
table.commits td.age { text-align: right; }
.pager { margin-top: 1rem; }
.pager a { margin-right: 1rem; }
.pager a::before { content: "["; color: var(--dim); }
.pager a::after { content: "]"; color: var(--dim); }

.commitmeta { border-bottom: 1px solid var(--dim); padding-bottom: .6rem; margin-bottom: 1rem; }
.commitmeta .who { color: var(--dim); margin: .3rem 0; }
.commitmeta .message { border-left: 2px solid var(--dim); padding-left: .8rem;
  color: var(--fg); white-space: pre-wrap; }

.filediff { margin: 1.2rem 0; }
.filediff .fhdr { color: var(--dim); border-bottom: 1px dashed var(--dim);
  padding-bottom: .2rem; margin-bottom: .3rem; }
.filediff .hunk { color: var(--dim); background: #161616; padding: 0 .5rem; margin-top: .4rem; }
.dl { white-space: pre-wrap; padding: 0 .5rem; }
.dl.add { background: rgba(143, 217, 143, .13); color: #b9e9b9; }
.dl.del { background: rgba(217, 143, 143, .13); color: #e9b3b3; }
.dl.ctx { color: var(--fg); }

.code { margin-top: .6rem; }
pre.blob { background: #111; border: 1px solid #333; padding: .5rem; overflow-x: auto; }
```
NOTE on `main:has(.browse)`: `:has()` is supported by all current browsers; the graceful degradation (72ch everywhere) is acceptable for older ones. If you prefer determinism, alternatively add `class="wide"` to `<main>` — but that requires a per-page base-template signal; use `:has()` as specified.

- [ ] **Step 2: Verify no test regressions:** `go test ./internal/web/...` (CSS is data; tests assert structure not style — must stay green). Run the e2e smoke: `bash scripts/web-ui-phase2-smoke-localfs.sh` → passes.

- [ ] **Step 3: Commit:** `git add internal/web/static/style.css && git commit -m "feat(web): direction-B browse styling (rules, tree glyphs, tinted diffs)"` (+ trailer).

---

### Task 10: UI-wide CSP

**Files:**
- Modify: `internal/web/handler.go`
- Modify: `internal/web/browse_test.go` (or a new `csp_test.go`)

- [ ] **Step 1: Write the failing test.** Append:
```go
func TestUIWideCSP(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		tree: []browsemodel.TreeEntry{{Name: "a.txt", Path: "a.txt", Type: "blob", OID: "x"}},
		blob: browsemodel.Blob{Path: "a.txt", Size: 2, Bytes: []byte("x\n")},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	const want = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	for _, path := range []string{"/", "/login", "/acme/demo", "/acme/demo/tree/main", "/acme/demo/blob/main/a.txt", "/nope"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Content-Security-Policy"); got != want {
			t.Errorf("%s: CSP = %q", path, got)
		}
	}
	// The raw endpoint keeps its own stricter policy.
	req := httptest.NewRequest("GET", "/acme/demo/raw/main/a.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
		t.Errorf("raw CSP = %q", got)
	}
}
```

- [ ] **Step 2: Run to verify FAIL.**

- [ ] **Step 3: Add the middleware in `internal/web/handler.go`:**
```go
// uiCSP is the strict policy for all UI responses. Possible because the UI has
// zero inline styles/scripts (class-based chroma + diff classes). Blocks remote
// README images by design (img-src 'self') — see operator guide. The raw
// endpoint overrides this with its own stricter policy.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

func cspMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", uiCSP)
		next.ServeHTTP(w, r)
	})
}
```
Change `NewHandler`'s return to `return sessionMiddleware(s.store, s.ttl)(cspMiddleware(s.mux))`.
(`handleRaw` already calls `w.Header().Set("Content-Security-Policy", ...)` AFTER the middleware ran — Set replaces, so raw keeps its sandbox policy; the test proves it.)

- [ ] **Step 4: Run** `go test ./internal/web/...` → ALL PASS.

- [ ] **Step 5: Commit:** `git add internal/web/handler.go internal/web/browse_test.go && git commit -m "feat(web): strict UI-wide Content-Security-Policy"` (+ trailer).

---

### Task 11: Smoke + operator guide

**Files:**
- Modify: `scripts/web-ui-phase2-smoke-localfs.sh`
- Modify: `docs/operator-guides/web-ui.md`

- [ ] **Step 1: Smoke additions.** After the existing raw-header checks in `scripts/web-ui-phase2-smoke-localfs.sh`, add:
```bash
# 8b. chroma stylesheet + UI CSP.
echo "== chroma.css + CSP =="
curl -sf "$BASE_URL/_ui/static/chroma.css" | grep -q ".chroma" || { echo "FAIL: chroma.css missing"; exit 1; }
home_csp=$(curl -sSI "$BASE_URL/acme/demo" | grep -i "^Content-Security-Policy:" || true)
printf '%s' "$home_csp" | grep -q "script-src 'self'" || { echo "FAIL: UI CSP missing on browse page: $home_csp"; exit 1; }
echo "  chroma.css + CSP OK"
```

- [ ] **Step 2: Run the smoke:** `bash scripts/web-ui-phase2-smoke-localfs.sh` → ALL CHECKS PASSED.

- [ ] **Step 3: Operator guide.** In `docs/operator-guides/web-ui.md`:
  - §6.2 (switcher): now a dropdown with htmx in-place tree swap; plain-GET fallback (`?ref=`) without JS; the htmx-deferred note in §6.9 is removed.
  - §6.4: highlighting is class-based monokai with line numbers; the stylesheet is generated at startup and served at `/_ui/static/chroma.css` (a `--ui-dir` `static/chroma.css` overrides it).
  - New §6.x "Tree activity column": last-commit summary/age per entry from a single bounded walk (200 commits, 8 MiB cap, `--no-renames`); entries older than the window show "—"; the column is best-effort (failures degrade to "—").
  - §6.5/§6.8-area + security notes: all HTML pages now carry the strict CSP (quote it); remote images in READMEs are blocked by `img-src 'self'` (closes the §6.9 viewer-IP-leak deferral — update that bullet); raw keeps `default-src 'none'; sandbox`.
  - §6.9 deferred list: remove "htmx partial swaps" and the CSP bullet; add "README remote-image proxy (images are blocked instead)".
  - Note relative-time/size presentation briefly in the routes/feature text.

- [ ] **Step 4: Commit:** `git add scripts/web-ui-phase2-smoke-localfs.sh docs/operator-guides/web-ui.md && git commit -m "docs(web-ui),test: polish-pass smoke checks + operator guide updates"` (+ trailer).

---

### Task 12: Full verification + visual gate + finalize

- [ ] **Step 1:** `go build ./... && go vet ./... && go test ./...` — entire module green.
- [ ] **Step 2:** `bash scripts/web-ui-phase2-smoke-localfs.sh` — passes.
- [ ] **Step 3: Manual visual gate.** Launch a real server against an imported fixture repo (reuse the smoke script's setup with a long sleep, or run the commands by hand) and view `/{tenant}/{repo}`, a subdirectory, a blob (check monokai + line numbers + copy/paste excludes numbers), the commit log, and a commit diff (tinted rows). Confirm "looks like direction B" with the user BEFORE merging. Check the htmx switcher swaps the tree without a full reload, and that disabling JS still navigates.
- [ ] **Step 4:** Update memory (`m24_phase2_progress.md` or a new polish note) per controller's process; request branch review (roborev refine loop per project protocol); then finishing-a-development-branch (push, PR, squash-merge on green CI — and remember `go.mod` did NOT change in this pass, so no license regen needed).

---

## Self-Review

**Spec coverage:** §2 styling → Task 9 (+ partial markup Tasks 4/8); §3.1 chroma → Task 3; §3.2 diff classes → Tasks 2+9; §4.1 funcs → Task 1 (+ templates Task 8); §4.2 TreeActivity → Tasks 6+7; §4.3 log dates → Task 8; §5 partials/htmx → Tasks 4+5; §6 CSP → Task 10; §7 testing → distributed + Tasks 11/12; operator guide → Task 11. No gaps.

**Placeholder scan:** Task 5 Step 3 references "existing fields incl. header with Activity (Task 7)" — Task 4 adds the `Activity` field to `browseHeader` (so templates parse) and Task 7 populates it; the handler literal instruction in Task 5 says to keep existing assignments, which is concrete enough given the read-current-file directive. Task 11 Step 3 describes doc edits in prose (docs task — acceptable). No TBDs.

**Type consistency:** `relTimeAt(now, unix)`/`absTime`/`humanSize`/`diffClass` (Task 1) match FuncMap registrations (`reltime`/`abstime`/`humansize`/`diffclass`) and template usages (Tasks 4/8). `browseHeader.Activity map[string]browsemodel.CommitMeta` (Task 4) matches `TreeActivity`'s return (Task 7) and the `treeAge` partial's `.OID` gate (zero-value CommitMeta → "—"). `Path` moves from `treeData` to `browseHeader` in Task 4 — Task 5's handler snippet builds `treeData{...}` accordingly; Task 4 Step 6 requires the full suite green, which forces the move to compile before later tasks run. `gitcli.LogNameStatus(ctx, dir, oid, max, scopePath)` (Task 6) matches the Task 7 call. `renderPartial(w, name, data)` (Task 4) matches Task 5's use. CSP string in Task 10's test matches the `uiCSP` const exactly.

**Ordering note:** Tasks 4 and 7 are coupled via `Activity` (4 adds the field + "—" rendering against nil; 7 adds the data source). Task 5 depends on 4 (renderPartial). Tasks must run in numeric order.
