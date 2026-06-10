package web

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"path/filepath"
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
	if br.verb == "" {
		if len(seg) == 3 {
			return br, true // trailing slash on repo home: "/{tenant}/{repo}/"
		}
		return browseRoute{}, false // "//"-style path: not a route
	}
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
	// Repo-settings paths ("/{tenant}/{repo}/settings...") are not browse verbs;
	// dispatch them before the content guard (settings does not require Content).
	if sr, ok := parseSettingsPath(r.URL.Path); ok {
		s.handleRepoSettings(w, r, sr)
		return
	}
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
		if s.aliasRedirect(w, r, br.tenant, br.repo) {
			return
		}
		// Uniform 404 for both not-found and not-authorized (anti-enumeration).
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}

	view := br.verb
	if view == "" {
		view = "repo"
	}
	EmitBrowseMetric(r.Context(), s.logger, view)

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
// maxLogPage caps ?page= so a crafted URL cannot force an O(history)
// `git log --skip` walk on a large repo (2000 pages × 50/page reaches the
// most recent 100k commits; beyond that the pager simply pins to the cap).
const maxLogPage = 2000

func queryPage(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 0 {
		return 0
	}
	if n > maxLogPage {
		return maxLogPage
	}
	return n
}

func (s *server) handleRepoHome(w http.ResponseWriter, r *http.Request, br browseRoute) {
	refs, err := s.content.ListRefs(r.Context(), br.tenant, br.repo)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	if refs.Default == "" {
		s.renderBrowse(w, r, "repo.html", repoHomeData{browseHeader: s.header(w, r, br, refs, "", "")})
		return
	}
	res, err := browsemodel.ResolveRest(refs, refs.Default)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	entries, err := s.content.ReadTree(r.Context(), br.tenant, br.repo, res.OID, "")
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	readme := s.renderReadme(r.Context(), br, res.OID, entries)
	h := s.header(w, r, br, refs, refs.Default, res.OID)
	activity, aerr := s.content.TreeActivity(r.Context(), br.tenant, br.repo, res.OID, "")
	if aerr != nil {
		// Best-effort column: log and render "—" rather than failing the page.
		s.logger.WarnContext(r.Context(), "tree activity failed", "tenant", br.tenant, "repo", br.repo, "err", aerr)
		activity = nil
	}
	h.Activity = activity
	s.renderBrowse(w, r, "repo.html", repoHomeData{
		browseHeader: h,
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
	rest := br.rest
	if qr := r.URL.Query().Get("ref"); qr != "" {
		// The ref-switcher form serializes its select as ?ref=<name> (both the
		// htmx request and the no-JS GET submit); it navigates to that ref's root.
		rest = qr
	}
	res, err := browsemodel.ResolveRest(refs, rest)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	entries, err := s.content.ReadTree(r.Context(), br.tenant, br.repo, res.OID, res.Path)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	h := s.header(w, r, br, refs, res.Ref, res.OID)
	h.Path = res.Path
	activity, aerr := s.content.TreeActivity(r.Context(), br.tenant, br.repo, res.OID, res.Path)
	if aerr != nil {
		// Best-effort column: log and render "—" rather than failing the page.
		s.logger.WarnContext(r.Context(), "tree activity failed", "tenant", br.tenant, "repo", br.repo, "err", aerr)
		activity = nil
	}
	h.Activity = activity
	var readme template.HTML
	if res.Path == "" {
		readme = s.renderReadme(r.Context(), br, res.OID, entries)
	}
	data := treeData{browseHeader: h, Entries: entries, ReadmeHTML: readme}
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
}

func (s *server) handleBlob(w http.ResponseWriter, r *http.Request, br browseRoute) {
	refs, err := s.content.ListRefs(r.Context(), br.tenant, br.repo)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	res, err := browsemodel.ResolveRest(refs, br.rest)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	b, err := s.content.ReadBlob(r.Context(), br.tenant, br.repo, res.OID, res.Path)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	md := isMarkdownPath(res.Path) && !b.Binary && !b.TooLarge && len(b.Bytes) <= maxHighlightBytes
	var rendered template.HTML
	var code template.HTML
	if md && r.URL.Query().Get("view") == "rendered" {
		rendered = renderMarkdown(b.Bytes)
	} else if !b.Binary && !b.TooLarge {
		code = highlight(res.Path, b.Bytes)
	}
	s.renderBrowse(w, r, "blob.html", blobData{
		browseHeader: s.header(w, r, br, refs, res.Ref, res.OID),
		Path:         res.Path,
		Blob:         b,
		Code:         code,
		Markdown:     md,
		Rendered:     rendered,
	})
}

func (s *server) handleRaw(w http.ResponseWriter, r *http.Request, br browseRoute) {
	refs, err := s.content.ListRefs(r.Context(), br.tenant, br.repo)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	res, err := browsemodel.ResolveRest(refs, br.rest)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	b, err := s.content.ReadBlob(r.Context(), br.tenant, br.repo, res.OID, res.Path)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	// Force a safe content type so attacker-controlled repo content (an HTML or
	// SVG blob) can never execute inline in the UI origin.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if b.TooLarge {
		// The 10 MiB cap is intentional; do not stream large blobs. Return a
		// 413 so the client gets a clear signal instead of a 0-byte download.
		s.renderError(w, r, http.StatusRequestEntityTooLarge, "file too large to serve")
		return
	}
	if b.Binary {
		w.Header().Set("Content-Type", "application/octet-stream")
		// RFC 5987 filename* avoids quoted-string breakage for names containing
		// quotes; attr-char percent-encoding neutralizes any odd bytes in the
		// name, including the single quote that delimits the ext-value fields.
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+rfc5987Encode(filepath.Base(res.Path)))
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "inline")
	}
	EmitRequestMetric(r.Context(), s.logger, "raw", http.StatusOK)
	_, _ = w.Write(b.Bytes)
}

// rfc5987Encode percent-encodes s per RFC 5987 attr-char rules: every byte
// except ALPHA / DIGIT / "!" / "#" / "$" / "&" / "+" / "-" / "." / "^" / "_" /
// "`" / "|" / "~" is %-encoded. Stricter than url.PathEscape, which leaves
// the ext-value delimiter "'" (and a few other non-attr-chars) bare.
func rfc5987Encode(s string) string {
	const hexdigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '!', c == '#', c == '$', c == '&', c == '+', c == '-',
			c == '.', c == '^', c == '_', c == '`', c == '|', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexdigits[c>>4])
			b.WriteByte(hexdigits[c&0xf])
		}
	}
	return b.String()
}

func (s *server) handleCommits(w http.ResponseWriter, r *http.Request, br browseRoute) {
	refs, err := s.content.ListRefs(r.Context(), br.tenant, br.repo)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	res, err := browsemodel.ResolveRest(refs, br.rest)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
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
}

func (s *server) handleCommit(w http.ResponseWriter, r *http.Request, br browseRoute) {
	oid := strings.Trim(br.rest, "/")
	if !browsemodel.IsHex40(oid) {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	detail, err := s.content.Commit(r.Context(), br.tenant, br.repo, oid)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	s.renderBrowse(w, r, "commit.html", commitData{
		browseHeader: s.header(w, r, br, browsemodel.Refs{}, "", detail.Meta.OID),
		Detail:       detail,
	})
}

// header builds the common browse header view-model. It issues a CSRF token for
// the layout's logout form when the request is authenticated.
// oid is the resolved object ID; it is stored as a fallback for RefOrOID() so
// that navigation links work when the page was reached via a raw 40-hex OID
// (in which case ref is empty and oid provides the non-empty value).
func (s *server) header(w http.ResponseWriter, r *http.Request, br browseRoute, refs browsemodel.Refs, ref, oid string) browseHeader {
	sess := SessionFromContext(r.Context())
	tok := ""
	if sess != nil {
		tok = issueCSRF(w, requestIsTLS(r, s.trustProxy))
	}
	return browseHeader{
		base:   base{Session: sess, CSRF: tok},
		Tenant: br.tenant, Repo: br.repo, Ref: ref, OID: oid, Refs: refs,
		CanAdmin: s.canAdminRepo(r, br.tenant, br.repo),
	}
}

// renderBrowse renders a browse page to a buffer (so a render error becomes a
// clean 500 rather than a truncated 200) and records the request metric.
func (s *server) renderBrowse(w http.ResponseWriter, r *http.Request, page string, data any) {
	var buf bytes.Buffer
	if err := s.render.render(&buf, page, data); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
	EmitRequestMetric(r.Context(), s.logger, strings.TrimSuffix(page, ".html"), http.StatusOK)
}
