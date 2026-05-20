package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/gateway/routenames"
)

// Op is the protocol operation a request maps to.
type Op int

const (
	OpInfoRefsUpload Op = iota + 1
	OpInfoRefsReceive
	OpUploadPack
	OpReceivePack
	OpLFSBatch
	OpLFSLocksCreate
	OpLFSLocksList
	OpLFSLocksVerify
	OpLFSLocksUnlock
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
	if !routenames.ValidateName(tenant) || !routenames.ValidateName(repoID) {
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
	case method == http.MethodPost && rest == "info/lfs/objects/batch":
		// RequiredAction is read; the LFS handler performs a secondary
		// write check after parsing the body's operation field.
		return &RoutedRequest{tenant, repoID, OpLFSBatch, auth.ActionRead}, nil
	case method == http.MethodPost && rest == "info/lfs/locks":
		return &RoutedRequest{tenant, repoID, OpLFSLocksCreate, auth.ActionWrite}, nil
	case method == http.MethodGet && rest == "info/lfs/locks":
		return &RoutedRequest{tenant, repoID, OpLFSLocksList, auth.ActionRead}, nil
	case method == http.MethodPost && rest == "info/lfs/locks/verify":
		return &RoutedRequest{tenant, repoID, OpLFSLocksVerify, auth.ActionRead}, nil
	case method == http.MethodPost && strings.HasPrefix(rest, "info/lfs/locks/") && strings.HasSuffix(rest, "/unlock"):
		// Lock ID is the segment between "info/lfs/locks/" and "/unlock".
		// We don't extract it here; the LFS handler parses URL.Path.
		// Reject paths that have no body between the prefix and suffix
		// (the lock id MUST be non-empty), and paths containing extra
		// slashes (the id MUST be a single segment).
		mid := strings.TrimSuffix(strings.TrimPrefix(rest, "info/lfs/locks/"), "/unlock")
		if mid == "" || strings.Contains(mid, "/") {
			return nil, ErrRouteNoMatch
		}
		return &RoutedRequest{tenant, repoID, OpLFSLocksUnlock, auth.ActionWrite}, nil
	default:
		return nil, ErrRouteNoMatch
	}
}

// routeRepo dispatches /{tenant}/{repo}.git/<sub-path>.
func (s *Server) routeRepo(w http.ResponseWriter, r *http.Request) {
	rr, err := ParseRoute(r.Method, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, ok := RunAuth(w, r, s.opts.AuthStore, rr); !ok {
		return
	}
	switch rr.Op {
	case OpInfoRefsUpload, OpInfoRefsReceive:
		s.handleInfoRefs(w, r, rr.Tenant, rr.Repo)
	case OpUploadPack:
		s.handleUploadPack(w, r, rr.Tenant, rr.Repo)
	case OpReceivePack:
		s.handleReceivePack(w, r, rr.Tenant, rr.Repo)
	case OpLFSBatch, OpLFSLocksCreate, OpLFSLocksList, OpLFSLocksVerify, OpLFSLocksUnlock:
		if s.lfsHandler == nil {
			http.NotFound(w, r)
			return
		}
		s.lfsHandler.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}
