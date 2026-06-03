package web

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
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

// --- placeholder page handlers (replaced in Tasks 13–16) ---

func (s *server) handleRepoHome(w http.ResponseWriter, r *http.Request, br browseRoute) {
	refs, err := s.content.ListRefs(r.Context(), br.tenant, br.repo)
	if err != nil {
		s.browseError(w, r, err)
		return
	}
	if refs.Default == "" {
		s.renderBrowse(w, r, "repo.html", repoHomeData{browseHeader: s.header(w, r, br, refs, "")})
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
	readme := s.renderReadme(r.Context(), br, res.OID, entries)
	s.renderBrowse(w, r, "repo.html", repoHomeData{
		browseHeader: s.header(w, r, br, refs, refs.Default),
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
	s.renderBrowse(w, r, "tree.html", treeData{
		browseHeader: s.header(w, r, br, refs, res.Ref),
		Path:         res.Path,
		Entries:      entries,
	})
}

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
		browseHeader: s.header(w, r, br, refs, res.Ref),
		Path:         res.Path,
		Blob:         b,
		Code:         code,
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
	// Force a safe content type so attacker-controlled repo content (an HTML or
	// SVG blob) can never execute inline in the UI origin.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if b.Binary || b.TooLarge {
		w.Header().Set("Content-Type", "application/octet-stream")
		// RFC 5987 filename* avoids quoted-string breakage for names containing
		// quotes; percent-encoding also neutralizes any odd bytes in the name.
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filepath.Base(res.Path)))
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "inline")
	}
	EmitRequestMetric(r.Context(), s.logger, "raw", http.StatusOK)
	_, _ = w.Write(b.Bytes)
}
func (s *server) handleCommits(w http.ResponseWriter, r *http.Request, br browseRoute) { w.WriteHeader(http.StatusOK) }
func (s *server) handleCommit(w http.ResponseWriter, r *http.Request, br browseRoute)  { w.WriteHeader(http.StatusOK) }

// header builds the common browse header view-model. It issues a CSRF token for
// the layout's logout form when the request is authenticated.
func (s *server) header(w http.ResponseWriter, r *http.Request, br browseRoute, refs browsemodel.Refs, ref string) browseHeader {
	sess := SessionFromContext(r.Context())
	tok := ""
	if sess != nil {
		tok = issueCSRF(w, requestIsTLS(r, s.trustProxy))
	}
	return browseHeader{
		base:   base{Session: sess, CSRF: tok},
		Tenant: br.tenant, Repo: br.repo, Ref: ref, Refs: refs,
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

// renderReadme is implemented in Task 16; this stub returns no README.
func (s *server) renderReadme(ctx context.Context, br browseRoute, oid string, entries []browsemodel.TreeEntry) template.HTML {
	return ""
}
