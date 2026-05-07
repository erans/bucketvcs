package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Op is the protocol operation a request maps to.
type Op int

const (
	OpInfoRefsUpload Op = iota + 1
	OpInfoRefsReceive
	OpUploadPack
	OpReceivePack
)

// RoutedRequest is the parsed shape of a Git-protocol request.
type RoutedRequest struct {
	Tenant         string
	Repo           string
	Op             Op
	RequiredAction auth.Action
}

// ErrRouteNoMatch means the request URL does not look like a Git smart-HTTP
// request handled by this gateway. Callers should respond with 404.
var ErrRouteNoMatch = errors.New("gateway: no route match")

// ParseRoute is the single source of truth for "this URL means this op."
// It is pure: no http.Request, no http.ResponseWriter, no auth.Store.
//
// Caller is responsible for translating the returned error into 404.
func ParseRoute(method, urlPath, rawQuery string) (*RoutedRequest, error) {
	if urlPath != path.Clean(urlPath) {
		return nil, fmt.Errorf("gateway: invalid path: %w", ErrRouteNoMatch)
	}
	parts := strings.SplitN(strings.TrimPrefix(urlPath, "/"), "/", 3)
	if len(parts) < 3 {
		return nil, ErrRouteNoMatch
	}
	tenant := parts[0]
	repoSeg := parts[1]
	rest := parts[2]

	if !strings.HasSuffix(repoSeg, ".git") || repoSeg == ".git" {
		return nil, ErrRouteNoMatch
	}
	repoID := strings.TrimSuffix(repoSeg, ".git")
	if !nameRE.MatchString(tenant) || !nameRE.MatchString(repoID) {
		return nil, ErrRouteNoMatch
	}

	q, _ := url.ParseQuery(rawQuery)
	switch {
	case method == http.MethodGet && rest == "info/refs":
		switch q.Get("service") {
		case "git-upload-pack":
			return &RoutedRequest{tenant, repoID, OpInfoRefsUpload, auth.ActionRead}, nil
		case "git-receive-pack":
			return &RoutedRequest{tenant, repoID, OpInfoRefsReceive, auth.ActionWrite}, nil
		default:
			return nil, ErrRouteNoMatch
		}
	case method == http.MethodPost && rest == "git-upload-pack":
		return &RoutedRequest{tenant, repoID, OpUploadPack, auth.ActionRead}, nil
	case method == http.MethodPost && rest == "git-receive-pack":
		return &RoutedRequest{tenant, repoID, OpReceivePack, auth.ActionWrite}, nil
	default:
		return nil, ErrRouteNoMatch
	}
}

// routeRepo dispatches /{tenant}/{repo}.git/<sub-path>.
func (s *Server) routeRepo(w http.ResponseWriter, r *http.Request) {
	// Reject paths that contain traversal sequences after decode.
	// path.Clean("/../etc/x.git/info/refs") == "/etc/x.git/info/refs"
	// which differs from the original, so the check fires.
	if r.URL.Path != path.Clean(r.URL.Path) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	tenant := parts[0]
	repoSeg := parts[1]
	rest := parts[2]

	if !strings.HasSuffix(repoSeg, ".git") {
		http.NotFound(w, r)
		return
	}
	repoID := strings.TrimSuffix(repoSeg, ".git")

	if !nameRE.MatchString(tenant) || !nameRE.MatchString(repoID) {
		http.Error(w, "invalid tenant or repository name", http.StatusBadRequest)
		return
	}

	switch {
	case r.Method == http.MethodGet && rest == "info/refs":
		if !s.authorize(w, r) {
			return
		}
		s.handleInfoRefs(w, r, tenant, repoID)
	case r.Method == http.MethodPost && rest == "git-upload-pack":
		if !s.authorize(w, r) {
			return
		}
		s.handleUploadPack(w, r, tenant, repoID)
	case r.Method == http.MethodPost && rest == "git-receive-pack":
		if !s.authorize(w, r) {
			return
		}
		s.handleReceivePack(w, r, tenant, repoID)
	default:
		http.NotFound(w, r)
	}
}
