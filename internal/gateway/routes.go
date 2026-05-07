package gateway

import (
	"net/http"
	"path"
	"regexp"
	"strings"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

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

// Stub for Task 17. handleInfoRefs lives in inforefs.go; handleUploadPack
// lives in upload_pack.go.
func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	http.Error(w, "receive-pack not yet implemented", http.StatusNotImplemented)
}
