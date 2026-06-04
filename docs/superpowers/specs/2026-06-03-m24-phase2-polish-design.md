# M24 — Web UI, Phase 2 Polish Pass

**Status:** Design approved (2026-06-03)
**Scope:** Visual + UX polish for the Phase 2 code-browse pages, plus two deferred
hardening items. Four workstreams: (1) visual styling of the browse pages in the
"minimal rules + tree glyphs" direction, (2) class-based syntax highlighting and
tinted diff rows (fixing the white-box chroma bug), (3) info enrichment (relative
times, humanized sizes, blob line numbers, tree last-commit column), (4) the htmx
ref-switcher swap and a strict UI-wide Content-Security-Policy. No new routes; no
settings/admin (Phase 3).

---

## 1. Background

Phase 2 (merged as `dad16d9`, PR #4) shipped functional browse pages with almost no
CSS: none of the browse classes (`.browse`, `.tree`, `.filediff`, `.dl`, …) exist in
`style.css`. Worse, chroma's `bw` style hardcodes `background-color:#fff`, so the
blob view renders a white box on the dark theme. Phase 2 also deferred the htmx
ref-switcher swap, an HTML-page CSP (README remote images can leak a viewer's IP),
and all date/size presentation niceties.

### Locked decisions (brainstormed 2026-06-03, visual companion)

| Decision | Choice |
|---|---|
| Design direction | **B — minimal rules + tree glyphs**: thin horizontal rules, `├─`/`└─` row markers, dim right-aligned metadata, generous whitespace. Extends the Phase 1 landing look. (Full box-drawing chrome and phosphor-CRT directions rejected.) |
| Code highlighting | **Full-color dark scheme (monokai)**, class-based (`WithClasses(true)`), with **line numbers** (table mode). Replaces the broken inline `bw` style. |
| Diff coloring | **Background-tinted rows** (GitHub-dark style): full-width tint for add/del lines, dim hunk headers. |
| Enrichment | Relative timestamps ("2h ago" + absolute UTC tooltip), humanized sizes (1.2 KiB), blob line numbers, **tree last-commit column via a bounded attribution walk** (not exact per-entry log). |
| Styles delivery | **Class-based everything** — zero inline styles — to enable the strict CSP. Chroma CSS generated at startup, served from memory at `/_ui/static/chroma.css`. |
| CSP | Strict policy on all HTML pages; README remote images are **blocked** (alt text renders) rather than proxied. |

---

## 2. Workstream 1 — visual styling (direction B)

`internal/web/static/style.css` grows the browse vocabulary, keeping the existing
`:root` palette (`--fg:#d8d8d8; --bg:#0d0d0d; --accent:#8fd98f; --dim:#666`) and
adding muted diff colors (soft red `#d98f8f` family for deletions; tints at ~13%
alpha):

- **Layout:** browse pages (`.browse`) widen to `max-width: 100ch` (code and diffs
  need it); text-centric pages (landing, login, error) stay at the current 72ch.
  Implemented by a class on `<main>` or the `.browse` wrapper overriding the width.
- **Repo header:** repo path in accent; thin solid rule below (`border-bottom: 1px
  solid var(--dim)`); the refs line (`branches: … · tags: …`) in default fg with
  accent links and dim separators.
- **Tree table:** `├─` glyph prefix on each row via `td.kind::before`-style CSS
  content (last row gets `└─` via `:last-child`); the kind column (dir/file/mod)
  dim; name links accent; size and age columns dim and right-aligned. Dir rows sort
  first already.
- **README block:** `── README ──` style divider (dashed rule with inline label, as
  on the Phase 1 landing `h2`), README body constrained to ~72ch for readability.
- **Commit log:** table with short-OID (accent link), summary (fg), author + age
  (dim, right-aligned). Pager links as `[prev]` / `[next]`.
- **Commit view:** meta block (summary bold, oid/author/age dim), message in a
  `<pre>` with a left rule; per-file sections with a dim header rule showing
  status/path/(+X −Y), binary/too-large notices in `.empty` style.
- **Blob view:** path breadcrumb + `[raw]` link in the header line; code block
  framed by a thin `var(--dim)` border (chroma supplies the interior colors).

Shared tree markup dedups into the `treeRows` partial (workstream 4), eliminating
the repo.html/tree.html duplication noted in the Phase 2 reviews.

---

## 3. Workstream 2 — highlighting + diff classes

### 3.1 Class-based chroma (fixes the white-box bug)

`internal/web/highlight.go`:

- Formatter becomes `chromahtml.New(chromahtml.WithClasses(true),
  chromahtml.WithLineNumbers(true), chromahtml.LineNumbersInTable(true),
  chromahtml.Standalone(false))` — class-based output, line numbers in a separate
  table column so text selection/copy excludes them.
- Style: `styles.Get("monokai")` (fallback `styles.Fallback`).
- A package-level `chromaCSS() []byte` renders the stylesheet once (sync.Once) via
  `formatter.WriteCSS` against the monokai style, plus a small override so the
  `<pre>` background blends with the page (`background: #111` frame, line-number
  column dim). Served at `GET /_ui/static/chroma.css` by a tiny handler registered
  alongside the static handler (in-memory; `--ui-dir` mode may override by shipping
  a `static/chroma.css` file, which wins if present).
- `blob.html`/`base.html` link the stylesheet (base links it unconditionally — one
  small cacheable file).
- The size-cap fallback (`plainPre`) and escaping behavior are unchanged.

### 3.2 Diff line classes

- `browsemodel.DiffLine.Kind` stays a byte; the template gains a `diffclass` func
  mapping `' '→"ctx"`, `'+'→"add"`, `'-'→"del"` (replacing the broken
  `k{{printf "%c" .Kind}}` scheme that emitted `class="dl k "` for context lines).
- CSS: `.dl` rows are full-width `display:block` monospace lines; `.dl.add` tinted
  `rgba(143,217,143,.13)` with lightened text; `.dl.del` tinted
  `rgba(217,143,143,.13)`; `.hunk` headers dim. The leading `+`/`-`/space glyph
  remains part of the text (terminal-faithful).

---

## 4. Workstream 3 — info enrichment

### 4.1 Template funcs

`parsePage`'s FuncMap gains:

- `reltime(unix int64) string` — "now", "5m ago", "2h ago", "3d ago", "4mo ago",
  "2y ago" (coarse buckets; 0 ⇒ "—"). Rendered with the absolute time in a
  `title="2026-06-03 18:44 UTC"` attribute via a companion `abstime` func.
- `humansize(n int64) string` — binary units, one decimal under 10 (312 B,
  1.2 KiB, 4.0 MiB, 1.1 GiB).

Used in: tree rows (size + age), blob header and too-large/binary notices (size),
commit log (age column), commit view meta (age + absolute).

**Clock note:** relative times compare against the server's `time.Now()` at render;
pages are uncached HTML so staleness is bounded by page life.

### 4.2 Tree last-commit column (bounded attribution walk)

New `ContentStore` method (and `gitbrowse` implementation):

```go
// TreeActivity returns, for each entry path in a directory listing, the most
// recent commit touching it — attributed from a single bounded history walk.
// Entries not touched within the walk window are absent from the map (render "—").
TreeActivity(ctx context.Context, tenant, repo, oid, path string) (map[string]browsemodel.CommitMeta, error)
```

Implementation:

- New gitcli helper running ONE subprocess:
  `git log <oid> --max-count=<K> --name-status -z --no-color --no-renames
  --pretty=format:<oid/author/time/subject with 0x1f/0x1e separators> [-- <path>/]`
  (scoped to the listed directory when path ≠ root). Output byte-capped via
  `runCapped` (8 MiB) — over-cap attributes from the captured prefix.
- `gitbrowse` parses commit records + their touched paths; for each tree entry,
  the FIRST commit in the walk that touches the entry (exact path match for blobs,
  `entryPath + "/"` prefix match for dirs) wins. Walk window `K = 200` (constant).
- Web: `handleRepoHome`/`handleTree` call `TreeActivity` after `ReadTree` and pass
  the map to the template; rows render `summary · age` (summary truncated by CSS
  ellipsis) or "—" when unattributed. A `TreeActivity` failure degrades to "—" for
  the whole listing (logged; page still renders — the column is best-effort).
- Cost: one additional bounded git call + one mirror open per tree page (the
  per-operation `web_browse_mirror_wait_seconds` semantics already documented).

### 4.3 Commit log dates

`commits.html` adds the age column using `CommitMeta.AuthorTime` (already in the
DTO; no reader changes).

---

## 5. Workstream 4 — htmx ref-switcher swap (+ partials renderer)

The renderer work deferred in Phase 2, done properly:

- **Shared partials:** new `internal/web/templates/_partials.html` containing
  `{{define "treeRows"}}` (the tree table, used by repo.html and tree.html — kills
  the duplication) and `{{define "refswitcher"}}`. `parsePage` parses
  `base.html + _partials.html + page` (both embedded and `--ui-dir` modes), so
  every page set has the partials; partials define no `title`/`content`, so no
  collisions.
- **Partial rendering:** `renderer` gains a dedicated partials-only template set
  (parsed from `_partials.html`) and `renderPartial(w, name, data) error`.
- **Switcher:** a `<form method="get" action="<tree-url>" hx-get="<tree-url>"
  hx-target="#tree" hx-swap="outerHTML" hx-push-url="true" hx-trigger="change">`
  wrapping a `<select name="ref">` listing branches and tags. Both the htmx
  request and the no-JS GET submit serialize the select as `?ref=<name>`, so the
  two paths are identical: `handleTree` accepts `?ref=` as an alternative to the
  path's ref segment. With `HX-Request: true` it returns the bare `treeRows`
  fragment (wrapped in the `#tree` container); otherwise the full page.
- **No-JS fallback:** a `<noscript><button>go</button></noscript>` submit inside
  the form; all other links remain real URLs.
- htmx swaps target only the tree; ref switches on blob/commits pages remain
  full navigations to the equivalent page on the selected ref.

---

## 6. Workstream 5 — UI-wide CSP

All web HTML responses (a small wrapper where pages are rendered — `renderBrowse`,
`renderError`, login/landing render sites, or one middleware around the mux) get:

```
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self';
img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none';
frame-ancestors 'none'
```

- Possible because workstreams 1–4 leave **zero inline styles/scripts** (class-based
  chroma, class-based diff tints, no `style=`/`onchange=` attributes anywhere).
- htmx 2.x operates correctly under this policy (`script-src 'self'`,
  `connect-src 'self'` covers its AJAX).
- **Consequence (intended):** remote images in rendered READMEs no longer load —
  closing the viewer-IP-leak deferral from Phase 2. Alt text renders. Documented in
  the operator guide; an image proxy remains future work.
- The raw endpoint keeps its stricter `default-src 'none'; sandbox`.
- Static-asset cache headers are out of scope for this pass.

---

## 7. Testing

- **Funcs:** table tests for `reltime`, `humansize`, `diffclass` (incl. 0/negative/
  boundary values).
- **Chroma:** `/_ui/static/chroma.css` serves non-empty CSS containing `.chroma`
  selectors; blob view output contains class-based markup (`class="chroma"`), no
  `style=` attributes, and line-number markup.
- **Diff classes:** commit view renders `.dl.add`/`.dl.del`/`.dl.ctx` (no `k `
  class).
- **TreeActivity:** gitbrowse fixture tests — newest-commit attribution for a file
  and a directory; un-touched-within-window entry absent from the map; capped/
  failure path degrades (web test: page renders with "—").
- **htmx:** `HX-Request: true` on the tree URL returns the bare fragment (no
  `<html`); without the header, full page. `?ref=` fallback navigates.
- **CSP:** every HTML route (landing, login, error, all six browse views) carries
  the exact policy; raw keeps its own; a README with a remote `<img>` renders the
  tag but the policy blocks the load (test asserts header, not browser behavior).
- **Smoke:** existing e2e smoke gains chroma.css + CSP-header assertions.
- **Manual screenshot pass** against a real server before merge (visual companion
  or browser) — gate on "looks like direction B".

---

## 8. Out of scope

`#L` line anchors, file/commit search, blame, mobile/responsive layout, theme
switching, README image proxy (blocked instead), sticky table headers, Phase 3
settings/admin, exact (per-entry `git log -1`) tree attribution.

---

## 9. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Strict CSP breaks something subtle (htmx, chroma) | Class-based output verified by tests asserting zero `style=` attributes; htmx 2.x is CSP-clean for the features used; manual screenshot pass before merge |
| TreeActivity slows tree pages | Single bounded subprocess (K=200, 8 MiB cap, dir-scoped pathspec); column is best-effort — failure degrades to "—", page still renders |
| `--no-renames` makes a renamed file attribute to its rename commit | Acceptable: the rename IS the last touch of that path |
| Line-number table mode breaks copy/paste or wraps badly | Chroma's standard two-column table; CSS `user-select:none` on the number column as belt-and-braces |
| Partials parse change regresses existing pages | All pages re-parse with the partials file; full web test suite + smoke gate |
