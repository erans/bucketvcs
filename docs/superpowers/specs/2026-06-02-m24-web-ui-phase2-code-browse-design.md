# M24 — Web UI, Phase 2: Code Browse

**Status:** Design approved (2026-06-02)
**Scope:** Phase 2 of the 3-phase web UI workstream. This phase adds **read-only**
git-content browsing on top of the Phase 1 chassis + identity layer: repo home,
file tree, blob/raw views, commit log, single commit + diff, a branch/tag switcher,
README rendering, and syntax highlighting. It ships **no** settings/admin forms and
**no** write paths — those are Phase 3.

---

## 1. Background & motivation

Phase 1 shipped the web chassis (embedded server-rendered HTML + htmx), the
cookie-session/identity layer, login/logout, and a landing page listing visible
repos. Phase 1.5 added OIDC browser login. The web layer today shows *which* repos a
visitor can see but offers no way to look *inside* one.

Phase 2 makes repos browsable in the browser, GitHub/GitLab-style, while staying
strictly read-only. The full three-phase decomposition (unchanged):

- **Phase 1 / 1.5 (shipped):** chassis + identity + OIDC login.
- **Phase 2 (this spec):** browse (read) — repo home, file tree, commit log, single
  commit + diff, blob/raw views, branch/tag switcher, README, syntax highlighting,
  via a hybrid git-content reader.
- **Phase 3:** manage (admin) — CSRF-protected settings forms (public toggle, grants,
  tokens, SSH keys, webhooks, protected refs/paths, hooks, quotas, user/tenant admin).

### Locked design decisions (from brainstorming, 2026-06-02)

| Decision | Choice |
|---|---|
| Content reader | **Hybrid**: `refstore` for refs/HEAD; `Mirror.Open()` + `git` (`cat-file`/`ls-tree`/`log`/`diff-tree`) for content & diffs. Reuses the same warm mirror the gateway uses for fetches; gets git's diff/rename/log-graph machinery for free. |
| Package boundary | New `internal/gitbrowse` package holds browse domain logic and imports `mirror`/`refstore`/`repo`/`gitcli`/`storage`. `internal/web` consumes it **only** through a new `ContentStore` interface, wired in `cmd/bucketvcs/webadapter.go`. Preserves Phase 1's web↔storage decoupling; keeps `cmd` thin; makes browse logic independently testable. |
| In-scope features | Core read pages **plus** branch/tag switcher, README (Markdown) render, syntax highlighting, raw/download endpoint. |
| URL scheme | GitHub-style: `/{tenant}/{repo}[/{verb}/{ref}/{path...}]` with `verb ∈ {tree, blob, raw, commits, commit}`. |
| Ref↔path disambiguation | `gitbrowse.Resolve` picks the **longest** known-ref (or full 40-hex OID) prefix of the URL remainder; the rest is the path. |
| Access control | Same visibility rules as the Phase 1 landing page; **404 for both not-found and not-authorized** (anti-enumeration). |
| Cold-mirror UX | Bounded synchronous `Mirror.Open()` (`--ui-browse-timeout`, default `20s`); on timeout render a styled **503 "warming up — retry"** page. |
| Markdown | `goldmark` + `bluemonday` sanitization. |
| Syntax highlighting | `chroma`, server-side, class-based, embedded minimal theme, size-capped. |
| Log pagination | Offset-based (`?page=`, 50/page); cursor pagination deferred. |
| Aesthetic | Continue the Phase 1 retro ASCII / minimalist terminal look; htmx enhances the ref switcher; pages work without JS. |

---

## 2. Architecture

### 2.1 New package: `internal/gitbrowse`

All browse domain logic lives here. It is the only new package that touches the
storage/mirror layer for reads. Proposed structure (each file one clear purpose):

```
internal/gitbrowse/
  service.go     // Service struct; constructor takes mirror.Manager + storage.ObjectStore (+ keys)
  refs.go        // ListRefs (via repo.Open → manifest → refstore.List); default-branch detection
  resolve.go     // Resolve(rest) → {Ref, OID, Path} using known refs + 40-hex OID detection
  tree.go        // ReadTree: git ls-tree at (oid, path) → []TreeEntry
  blob.go        // ReadBlob: git cat-file blob + size/type; binary detection; size caps
  log.go         // Log: git log --skip --max-count → []CommitMeta + hasMore
  commit.go      // Commit: git show/diff-tree → CommitDetail (metadata + parsed diff)
  types.go       // Refs, Resolved, TreeEntry, Blob, CommitMeta, CommitDetail, FileDiff, Hunk
  mirror.go      // openMirror helper: bounded Mirror.Open + RLock; maps timeout → ErrWarming
```

`Service` is constructed at the composition root with the **existing** mirror manager
(the gateway already builds one) and the object store, so no second materialization
cache is introduced.

### 2.2 The `ContentStore` interface (consumed by `internal/web`)

Declared in `internal/web` so the web package depends only on an interface, never on
`mirror`/`refstore`/`storage`:

```go
// internal/web/contentstore.go
type ContentStore interface {
    ListRefs(ctx context.Context, tenant, repo string) (Refs, error)
    Resolve(ctx context.Context, tenant, repo, rest string) (Resolved, error)
    ReadTree(ctx context.Context, tenant, repo, oid, path string) ([]TreeEntry, error)
    ReadBlob(ctx context.Context, tenant, repo, oid, path string) (Blob, error)
    Log(ctx context.Context, tenant, repo, oid string, offset, limit int) (page []CommitMeta, more bool, err error)
    Commit(ctx context.Context, tenant, repo, oid string) (CommitDetail, error)
}
```

View-model types (`Refs`, `Resolved`, `TreeEntry`, `Blob`, `CommitMeta`,
`CommitDetail`, `FileDiff`, `Hunk`) are declared in `internal/web` and mirrored by
`gitbrowse`; the adapter translates. (If duplication proves noisy in implementation,
the adapter may convert from `gitbrowse` types — the interface is the contract, the
concrete types are an implementation detail.)

Sentinel errors surfaced across the interface:

- `ErrNotFound` — repo/ref/path/object absent → HTTP 404.
- `ErrWarming` — mirror materialization exceeded the timeout → HTTP 503.

`web.Deps` gains `Content ContentStore`. When `nil` (browse disabled or unwired) all
browse routes render 404, so the feature degrades gracefully.

### 2.3 Wiring (`cmd/bucketvcs/webadapter.go`)

The adapter already wraps the auth store for Phase 1/1.5 `DataStore` methods. Phase 2
adds:

- A `gitbrowse.Service` field, built from the gateway's `mirror.Manager` + the
  configured `storage.ObjectStore`.
- `ContentStore` method implementations that delegate to the service.
- The new `DataStore.GetVisibleRepo` method (§4), implemented over `sqlitestore`.

`cmd` remains thin wiring; no git parsing logic lives there.

### 2.4 Request flow

```
HTTP request
  → serve.go dispatcher  (.git/ + /_ + /healthz → gateway; else → web.Handler)
      → session middleware (actor from cookie, or anon)
      → repo router (parse /{tenant}/{repo}[/{verb}/...])
          → DataStore.GetVisibleRepo(actor, tenant, repo)  → 404 if not visible
          → ContentStore.<op>(...)                          → 404 / 503 on sentinels
          → html/template render (retro-ASCII layout)
```

---

## 3. Routing & URL scheme

### 3.1 Routes

| Route | Method | Page |
|---|---|---|
| `/{tenant}/{repo}` | GET | Repo home: default-branch root tree + rendered README |
| `/{tenant}/{repo}/tree/{ref}/{path...}` | GET | Directory listing at `path` on `ref` |
| `/{tenant}/{repo}/blob/{ref}/{path...}` | GET | File view (syntax-highlighted) |
| `/{tenant}/{repo}/raw/{ref}/{path...}` | GET | Raw file bytes (safe content-type, see §6.3) |
| `/{tenant}/{repo}/commits/{ref}` | GET | Commit log on `ref`, paginated (`?page=`) |
| `/{tenant}/{repo}/commit/{oid}` | GET | Single commit: metadata + diff |

All are GET-only (read-only phase). Unknown verbs under a valid repo → 404.

### 3.2 Router placement

The existing catch-all `/` handler is extended. Dispatch within the web mux:

- `path == "/"` → landing (unchanged).
- One path segment (e.g. `/acme`) that is not a known fixed route → 404 (tenant index
  pages are out of scope; the landing page already groups by tenant).
- Two-or-more segments → repo router.

Fixed routes registered explicitly on the mux (`/login`, `/logout`, `/_ui/`,
`/login/oidc`, `/login/oidc/callback`) take precedence and are not shadowed; none
collide with the 2-segment repo grammar. Tenant and repo names are validated with
`routenames.ValidateName` before any store call (invalid name → 404, no lookup).

### 3.3 Ref ↔ path disambiguation

Git ref names contain slashes (`refs/heads/feature/foo` → display ref `feature/foo`),
so the remainder after `/tree/` or `/blob/` is ambiguous between ref and path. The
router passes the raw remainder to `ContentStore.Resolve`, which:

1. If the first 40 characters form a valid hex OID, treats that as `OID`, the rest as
   `Path` (after the trailing `/`).
2. Otherwise fetches the ref list (`ListRefs`) and selects the **longest** ref name
   that is a `/`-delimited prefix of the remainder; the remaining tail is `Path`.
3. If no ref matches → `ErrNotFound` (404).

`Resolve` returns `Resolved{Ref string, OID string, Path string}`. The display `Ref`
is echoed in links and the switcher; the resolved `OID` is the stable handle passed to
`ReadTree`/`ReadBlob`/`Log`. (Resolving to an OID once per request avoids TOCTOU
between the switcher reflecting a moving branch and the content read.)

---

## 4. Access control

Browse authorization reuses the Phase 1 landing visibility rules exactly:

| Visitor | May browse |
|---|---|
| Anonymous | repos with `public_read = true` |
| Logged-in user | public repos + repos where the user holds any permission |
| Admin | all repos |

New read method on `DataStore`, implemented in `sqlitestore`:

```go
GetVisibleRepo(ctx context.Context, actor *auth.Actor, tenant, repo string) (*Repo, error)
// returns (*Repo, nil) if visible to actor; (nil, ErrRepoNotVisible) otherwise.
```

The router maps **both** "repo does not exist" and "exists but not visible to this
actor" to a uniform **404** — no existence oracle (consistent with the gateway's
anti-enumeration posture). Authorization runs **before** any mirror open or content
read, so an unauthorized request never triggers materialization.

---

## 5. Content reader (`gitbrowse`, hybrid)

### 5.1 Mirror access

`gitbrowse.Service` opens the bare mirror via the shared `mirror.Manager`:

```
openMirror(ctx, tenant, repo):
    ctx, cancel = context.WithTimeout(ctx, browseTimeout)   // default 20s
    m, err = manager.Open(ctx, tenant, repo)                // materializes if cold; verifies if warm
    if ctx deadline exceeded → return ErrWarming
    m.RLock(); return m, releaseFn(=RUnlock)
```

Reads hold the mirror **read lock** (`RLock`) for the duration of the `git`
invocation(s), matching the gateway's fetch path so a concurrent push (write lock)
does not race the read. The lock is released as soon as bytes/structs are captured.

`browseTimeout` comes from `--ui-browse-timeout` (default `20s`). On timeout the web
layer renders the 503 "warming up" page. Most repos are already warm (shared with the
gateway) or materialize quickly.

### 5.2 Refs (`refs.go`)

`ListRefs` resolves refs **without** a mirror, via the manifest path:
`repo.Open` → `ReadRoot` → `manifest.UnmarshalBody` → `refstore.New` → `refstore.List`.
It partitions `refs/heads/*` (branches) and `refs/tags/*` (tags), strips prefixes for
display, and determines the **default branch**:

1. The symbolic `HEAD` target if recorded in the manifest/refstore;
2. else `main` if present, else `master` if present;
3. else the lexicographically first branch;
4. else (no branches) the repo renders an "empty repository" home.

`Refs{Default string, Branches []RefInfo, Tags []RefInfo}` where
`RefInfo{Name, OID}`.

### 5.3 Tree (`tree.go`)

`ReadTree(oid, path)` runs `git ls-tree` (non-recursive, one directory level) in the
bare dir at `<oid>:<path>` and parses entries into:

```go
TreeEntry{ Name string; Path string; Mode string; Type string /* "tree"|"blob"|"commit" */;
           Size int64 /* blobs only */; OID string }
```

Entries are sorted directories-first then name (GitHub convention). Submodule entries
(`commit` type / gitlink) are shown as non-navigable rows. A missing path → 404.

### 5.4 Blob (`blob.go`)

`ReadBlob(oid, path)`:

1. `git cat-file -t` to confirm the path resolves to a blob (else 404).
2. `git cat-file -s` for size; if `size > maxBlobBytes` (**hard cap, default 10 MiB**)
   the blob is returned with `TooLarge=true` and no bytes (view shows "file too large —
   download via raw").
3. Otherwise read bytes via `git cat-file blob`.
4. **Binary detection:** a NUL byte within the first 8 KiB ⇒ `Binary=true` (view shows
   "binary file" + raw/download link; no highlighting).

```go
Blob{ Path, OID string; Size int64; Binary, TooLarge bool; Bytes []byte }
```

### 5.5 Log (`log.go`)

`Log(oid, offset, limit)` runs
`git log <oid> --skip=<offset> --max-count=<limit+1> --pretty=...` and parses a
NUL/record-delimited format into:

```go
CommitMeta{ OID, ShortOID string; Summary string; AuthorName, AuthorEmail string;
            AuthorTime int64 /* unix */ }
```

It requests `limit+1` rows to compute `more bool` (whether a next page exists) without
a second count query. `limit` defaults to 50, capped at 100; `offset = page*limit`.
`page` is parsed from `?page=`, clamped to `>= 0`.

### 5.6 Commit + diff (`commit.go`)

`Commit(oid)`:

1. `git cat-file commit` (or `git show --no-patch`) for metadata: author/committer
   name+email+time, full message, parent OID(s), tree OID.
2. Diff against the first parent (root commit ⇒ diff against the empty tree) via
   `git diff-tree -p -M --no-color <oid>` (rename detection on). The unified-diff
   output is parsed into structured hunks:

```go
CommitDetail{ Meta CommitMeta; Message string; Parents []string; Files []FileDiff;
              Truncated bool }
FileDiff{ OldPath, NewPath string; Status string /* A|M|D|R|C */; Binary bool;
          Additions, Deletions int; Hunks []Hunk; TooLarge bool }
Hunk{ Header string; Lines []DiffLine }
DiffLine{ Kind byte /* ' '|'+'|'-' */; Text string }
```

**Diff caps (anti-DoS, anti-OOM):** at most `maxDiffFiles` (**default 300**) files and
`maxDiffLinesPerFile` (**default 3000**) lines per file are parsed into hunks; beyond
either, that file is marked `TooLarge` (or the commit `Truncated`) and the view links
to the raw patch. Binary file diffs show "Binary files differ", no hunks.

### 5.7 Why shell-out, not pure-Go

`git` provides correct unified diffs, rename/copy detection, and log-graph traversal
that would be a large, bug-prone reimplementation in Go. The mirror is already a warm,
shared cache. The pure-Go pack reader (`internal/pack`) remains available and may be
adopted later for hot single-object reads if materialization cost becomes a
bottleneck; it is explicitly **not** used in Phase 2.

---

## 6. Pages & rendering

### 6.1 Templates (new, under `internal/web/templates/`)

Reuse the Phase 1 `base.html` (navbar, retro layout, logout form). New templates:

```
repo.html      // repo home: header bar + ref switcher + root tree + rendered README
tree.html      // directory listing (also the htmx swap target for the ref switcher)
blob.html      // file view: path breadcrumb, highlighted source, raw/download link
commits.html   // commit log list + prev/next pager
commit.html    // single commit: metadata, message, file-by-file diff
_tree.html      // partial: tree rows (returned bare for htmx ref-switch swaps)
_diff.html      // partial: one FileDiff (hunks)
```

Retro-ASCII layout grammar from Phase 1 continues (box-drawing borders, monospace, one
accent). The branch/tag switcher is a `<select>`/menu; htmx swaps `#tree` (or
navigates) on change, with a plain-link fallback so it works without JS. Detailed
visual treatment is produced at implementation time via the `frontend-design` skill;
this spec fixes direction and the page set.

Reference repo-home sketch:

```
┌─ acme/demo ─────────────────────────[ main ▾ ]──[ alice ▾ ]─┐
│  branches: main, dev    tags: v1.0                          │
│                                                             │
│  📁 src                            update readme   2h ago   │
│  📁 docs                           initial import  3d ago   │
│  📄 README.md            1.2 KiB   initial import  3d ago   │
│  📄 go.mod                312 B    initial import  3d ago   │
│                                                             │
│  ── README.md ──────────────────────────────────────────   │
│  <rendered markdown>                                        │
└─────────────────────────────────────────────────────────────┘
```

(Emoji shown for clarity; the real UI uses ASCII glyphs per the retro aesthetic.)

### 6.2 README & Markdown

On the repo home, the root tree is scanned (case-insensitive) for `README.md` /
`README.markdown` / `README`. A Markdown file is rendered with **goldmark** and the
HTML **sanitized with bluemonday** (UGC policy) before templating — untrusted repo
content is never injected unsanitized. A plain `README` (no extension) is shown as
preformatted text. Absent README ⇒ home shows the tree only.

### 6.3 Blob view, raw endpoint & XSS safety

- **Blob view (`/blob/`):** text blobs under the highlight cap (**default 1 MiB**) are
  highlighted with **chroma** (class-based HTML + an embedded minimal stylesheet that
  matches the retro theme). Over the cap ⇒ plain `<pre>` (no highlight). Binary ⇒
  "binary file" notice + download link. Lexer chosen by filename/extension, falling
  back to plaintext.
- **Raw endpoint (`/raw/`):** streams the blob bytes with a **forced safe content
  type** — `text/plain; charset=utf-8` for text, `application/octet-stream` for binary
  — plus `X-Content-Type-Options: nosniff` and a restrictive
  `Content-Security-Policy` (e.g. `default-src 'none'`). This guarantees
  attacker-controlled repo content (an HTML or SVG blob) can never execute inline in
  the UI's origin. `Content-Disposition` is `inline` for text, `attachment` for binary.
  The raw endpoint honors the same blob size cap.

### 6.4 Cold-mirror / warming page

When `ContentStore` returns `ErrWarming`, the web layer renders a styled **HTTP 503**
"repository is warming up — retry shortly" page. As progressive enhancement the page
includes an htmx/meta-refresh poll that re-requests after a short delay; without JS the
visitor simply reloads.

---

## 7. Configuration (new serve flags)

| Flag | Default | Purpose |
|---|---|---|
| `--ui-browse-timeout` | `20s` | Max synchronous wait for cold mirror materialization on a browse request before returning the 503 warming page. |

Browse is enabled whenever the UI is enabled (`--ui`, default on) and the
`ContentStore` is wired. No separate enable flag. The blob/diff caps (§5.4–5.6) are
constants in `gitbrowse` for Phase 2 (not operator-tunable yet).

---

## 8. Observability

- **Metrics:**
  - Existing `web_requests_total{route,status}` gains the new browse routes
    (route label is the **template**, e.g. `blob`, not the raw path — bounded
    cardinality).
  - `web_browse_total{view}` — counter, `view ∈ {repo,tree,blob,raw,commits,commit}`.
  - `web_browse_mirror_wait_seconds` — histogram of the `Mirror.Open` wait (surfaces
    cold-materialization latency / warming-timeout rate).
- **Audit events:** none for Phase 2 (reads are not audited elsewhere in the codebase).
  A per-read/browse audit trail is explicitly deferred.

---

## 9. Testing

`internal/gitbrowse` unit tests run against a **fixture bare repo** built in
`testdata` (or constructed in-test with `git`), exercising:

- `ListRefs`: branches/tags partition; default-branch detection (HEAD, then
  main/master, then first, then empty-repo).
- `Resolve`: branch with slashes (`feature/foo`) vs path; tag; full-OID handle;
  no-match → `ErrNotFound`; longest-prefix wins when a branch name prefixes a path.
- `ReadTree`: root and nested path; dirs-first ordering; submodule rows; missing path
  → 404.
- `ReadBlob`: text blob bytes/size; binary detection (NUL); over-cap `TooLarge`.
- `Log`: pagination (`offset`/`limit`, `more` via `limit+1`); ordering; root.
- `Commit`: metadata, multi-file diff parsing, rename detection, root-commit
  (vs empty tree), binary-file diff, `maxDiffFiles`/`maxDiffLinesPerFile` truncation.

`internal/web` tests:

- Routing: `/{t}/{r}`, each verb; unknown verb → 404; one-segment path → 404; fixed
  routes (`/login`) not shadowed; invalid tenant/repo name → 404 (no store call).
- Access control: anon sees public only; authed sees granted; admin sees all; both
  not-found and not-authorized → **uniform 404** (no existence leak).
- Raw endpoint: forced `Content-Type`, `nosniff`, CSP; HTML blob is **not** served as
  `text/html`.
- Markdown: a README with a script/`onerror` payload is sanitized (no script in
  output).
- Cold-mirror: `ErrWarming` → 503 warming page.
- htmx: ref-switch returns the bare `_tree.html` partial on htmx requests, full page
  otherwise.

---

## 10. Out of scope (deferred)

- **Phase 3:** all settings/admin forms and write paths.
- Path-filtered commit log (`/commits/{ref}/{path}`), `git blame`, file/commit/code
  search, compare/PR/branch-diff views.
- Branch/tag **creation or deletion** from the UI (Phase 2 is read-only).
- Cursor-based log pagination (offset-based ships now).
- Per-read / per-browse audit trail and session-history admin views.
- Operator-tunable blob/diff caps (constants for now).
- Adopting the pure-Go pack reader for hot single-object reads (mirror+git ships now).
- Web-based clone helper / "code" download (tarball/zip export).

---

## 11. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Cold mirror materialization blocks a request for a long time | Bounded `--ui-browse-timeout` (20s) + styled 503 warming page; shared warm mirror with the gateway means most reads are warm; `web_browse_mirror_wait_seconds` makes the cost observable. |
| Ref-vs-path ambiguity (slash refs) misroutes | `Resolve` uses longest known-ref prefix + 40-hex OID detection; unit-tested against slash branches and branch-name-prefixes-path cases. |
| XSS via attacker-controlled repo content (HTML/SVG blob, malicious README) | Raw forces safe content-type + `nosniff` + CSP; Markdown sanitized with bluemonday; blob view emits highlighted **text**, never raw HTML. |
| Existence enumeration of private repos | Uniform 404 for not-found and not-authorized; authorization before any mirror open. |
| Huge blob / huge diff OOM or slow render | Hard blob cap (10 MiB) + highlight cap (1 MiB) + diff file/line caps (300 / 3000) with raw-patch fallback. |
| Concurrent push racing a read | Reads hold the mirror `RLock`, same discipline as the gateway fetch path. |
| Import cycle / web↔storage recoupling | Browse logic in `internal/gitbrowse`; `internal/web` depends only on the `ContentStore` interface; wiring in `cmd`. |
| Browse disabled / `ContentStore` unwired | `Deps.Content == nil` ⇒ browse routes 404; feature degrades gracefully. |
