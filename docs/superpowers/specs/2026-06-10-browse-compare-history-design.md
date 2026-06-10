# Browse depth: Compare + Per-file history — design

Date: 2026-06-10
Status: approved (brainstorm 2026-06-10)
Predecessors: M24 Web UI Phase 2 code browse (tree/blob/commit-diff/ref-switcher/line-anchors),
Build Triggers UI (`2026-06-09-build-triggers-ui-design.md`).

This is **Phase B** of the web-ui roadmap (A build-triggers UI ✓ → **B code-browse depth** →
C observability surface → D self-service & lifecycle). Phase B is scoped to the two
highest-value-per-effort missing browse features; **Blame** and **Search** (content +
filename) are deferred to a later phase (each has its own UX surface and cost concerns).

## 1. Goal

Make the code browser feel like a real code host for *reading and reviewing* by adding the
two genuinely-missing navigation features, reusing the existing Phase-2 rendering:

- **Compare** — view the diff between two arbitrary refs/commits (`base..head`, two-dot
  direct tree diff), rendered with the existing per-file diff UI.
- **Per-file history** — `git log` scoped to a file or directory, rendered with the existing
  commits list. The codebase already stubs this (`handleCommits` 404s on a path-scoped log
  today).

Non-goals: blame; content/filename search; three-dot/merge-base compare; a "commits between"
list on the compare page; any change to the git protocol or storage layers.

## 2. Architecture

No new packages. Both features follow the established read path
(`web.ContentStore` → `gitbrowse.Service` → `gitcli` shell-out → `browsemodel` types) and
**reuse existing templates**. New leaf helpers mirror existing ones (`DiffTreePatch`,
`LogRaw`).

| Layer | Addition |
|-------|----------|
| `internal/gitcli` | `DiffRefsPatch(ctx, dir, base, head)` → `git diff-tree -p -M --no-color <base> <head>`, capped at `maxDiffPatchBytes`; `LogRawPath(ctx, dir, rev, path string, follow bool, skip, max int)` → `git log [--follow] <rev> -- <path>`. Both validate inputs with existing `validRefOrOID` + path guard. |
| `internal/gitbrowse` | `Compare(ctx, tenant, repo, baseOID, headOID) (browsemodel.Comparison, error)` reusing the current patch→`[]FileDiff` parser; `LogPath(ctx, tenant, repo, oid, path string, offset, limit int) ([]CommitMeta, bool, error)`. |
| `internal/browsemodel` | `Comparison{ Base, Head Resolved; Files []FileDiff; Additions, Deletions int; Truncated bool }` (reuses `FileDiff`/`Hunk`/`DiffLine`). |
| `internal/web/contentstore.go` | `Compare(...)` + `LogPath(...)` on the `ContentStore` interface. `var _ ContentStore = (*gitbrowse.Service)(nil)` is the conformance guard. |
| `internal/web/browse.go` | add `compare` to the `parseBrowsePath` verb whitelist; wire the path-scoped log into `handleCommits`. |
| templates | extract a shared `{{define "filediff"}}` partial (from `commit.html`) into `_partials.html`; new `compare.html`; reuse `commits.html` for history. |

## 3. Per-file history

### Routing & handler
Reuses the existing `/{t}/{r}/commits/<ref>/<path>` route — `ResolveRest(refs, br.rest)`
already returns `{Ref, OID, Path}`. In `handleCommits`, replace the current
`if res.Path != "" → 404` branch with:

- `commits, more, err := s.content.LogPath(ctx, t, r, res.OID, res.Path, page*pageSize, pageSize)`.
- `s.content.LogPath` shells `git log <oid> -- <path>`. The web handler passes only
  `(oid, path, offset, limit)` — it does **not** decide rename-following. `gitbrowse.Service.LogPath`
  determines `follow` internally (it has the mirror open): `follow=true` when the resolved path
  is a single **blob** (track renames), `follow=false` for directories (git `--follow` is
  single-path only), then calls `gitcli.LogRawPath(..., follow, ...)`.

### Template (`commits.html`, extended)
- Add an `<h2>` shown only when a path is scoped: `history: <path> @ <ref>`.
- Thread `Path` through the view-model and the pager links: the prev/next anchors become
  `/{t}/{r}/commits/{ref}/{path}?page=N` when `Path != ""`, else the current
  `/{t}/{r}/commits/{ref}?page=N`.
- Empty result on a valid path → "no history for this path" empty state (distinct from 404).

### Entry points
- **Blob page** (`blob.html`): a `[history]` link → `/{t}/{r}/commits/<ref>/<blobpath>`.
- **Tree page** (`tree.html`): a `[history]` link in the header → directory history for the
  current tree path (root tree → whole-repo history, i.e. the existing commits page).

## 4. Compare

### Picker page
`GET /{t}/{r}/compare` (no `..` segment) renders a plain `<form method="get">` with two ref
`<select>`s populated from `browseHeader.Refs` (reusing `.refswitch` styling):
- `base` defaults to `Refs.Default`; `head` defaults to `?head=<ref>` when present (so a
  "compare" link from a branch can prefill head), else also the default.
- Submitting builds `/{t}/{r}/compare/<base>..<head>` (client-side via the form `action` +
  a tiny no-JS-friendly approach: the form GETs to `/compare` with `?base=&head=` and the
  handler 303-redirects to the `..` URL; this keeps it pure-HTML).

### Result page
`GET /{t}/{r}/compare/<base>..<head>`:
- Split `br.rest` on the **first** `..` → `base`, `head` (git forbids `..` within ref names,
  so this is unambiguous; `feature/x..main` works).
- Resolve each side against `ListRefs` (ref name) or accept a raw 40-hex OID, reusing the
  existing resolution helper. Unresolvable → `browseError` (404).
- `cmp, err := s.content.Compare(ctx, t, r, baseOID, headOID)`.
- Render `compare.html`: header with both pickers (re-compare in place) + a summary line
  (`N files changed, +A −D`) + the shared `{{template "filediff"}}` block per file.

### Caps & edge cases
- Reuse `maxDiffPatchBytes` + the per-file line cap → `Comparison.Truncated` and
  `FileDiff.TooLarge` surface exactly as single-commit diffs do.
- Identical base/head (or no diff) → "no differences" empty state.
- Binary files → existing `FileDiff.Binary` treatment.

### Shared diff partial
Extract the per-file diff markup currently inline in `commit.html` into
`{{define "filediff"}}…{{end}}` in `_partials.html`; `commit.html` and `compare.html` both
`{{range .Files}}{{template "filediff" .}}{{end}}`. This is a targeted cleanup that removes
duplication introduced by this feature; the rendered output for `commit.html` is unchanged
(golden/snapshot assertion in tests).

## 5. Authorization & no-JS

- Compare and history are **read** views: they go through the same `handleBrowse` repo
  authorization as tree/blob/commit (public repos readable; private require a session with
  read access). No new authz surface.
- No-JS: the compare picker is a plain GET form; history and compare results are fully
  server-rendered. Any htmx ref-switcher enhancement remains optional and degrades to a
  full navigation.

## 6. Error handling

- Invalid/unresolvable ref or OID (either side of compare; the history ref) → `browseError`
  → 404 (anti-enumeration, matching existing browse).
- Invalid/escaping path → the existing path guard → 404.
- `git`/mirror failures → `browseError` maps `ErrWarming` to the existing "warming up"
  response and everything else to 500 (logged), exactly as current browse handlers do.
- Malformed compare URL (missing `..`, empty side) → 404.

**Implementation note (follow flag):** `gitbrowse.Service.LogPath` owns the rename-follow
decision — it sets `follow=true` only when the resolved path is a single blob, since git
`--follow` is single-path only. The web `ContentStore.LogPath` signature carries no `follow`
arg. If determining blob-vs-tree cheaply inside `LogPath` proves awkward, defaulting to
`follow=false` (plain `git log -- <path>`) is an acceptable fallback — rename-tracking is a
nicety, not a blocker.

## 7. Testing

- **gitcli** (`gitcli_test.go`): `DiffRefsPatch` — two-dot patch between two commits; `-M`
  rename detection; cap truncation; invalid-ref rejection. `LogRawPath` — path-scoped log;
  `--follow` rename tracking for a file; directory scoping; invalid-path rejection.
- **gitbrowse** (service tests): `Compare` over a fixture repo — A/M/D/R file statuses +
  add/del counts + truncation. `LogPath` — file vs directory; rename-follow; empty result.
- **web** (`browse_test.go`): compare picker renders both ref lists + defaults; `/compare/a..b`
  renders diff + summary; identical refs → "no differences"; bad ref → 404; missing `..` → 404;
  per-file history renders for file and directory; pager carries the path; `[history]` links
  present on blob + tree; unknown path → 404. A snapshot assertion that the extracted
  `filediff` partial leaves `commit.html` output byte-identical.
- **No new metrics**; `EmitRequestMetric` is emitted for the new pages (`compare`,
  reused `commits`).

## 8. Out of scope (deferred)

Blame; content search (`git grep`); filename/path search; three-dot/merge-base compare;
"commits between" list on compare; compare across forks/repos; diff-side-by-side view;
syntax-highlighted diffs. Roadmap C (observability) and D (self-service) remain separate
phases.
