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
