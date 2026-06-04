package web

import (
	"net/http"
	"net/url"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// requireUser ensures a logged-in session; on failure it 303s to login with
// ?next= and returns false.
func (s *server) requireUser(w http.ResponseWriter, r *http.Request) bool {
	if SessionFromContext(r.Context()) != nil {
		return true
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
	return false
}

// isGlobalAdmin reports whether the request's session is a global admin.
func isGlobalAdmin(r *http.Request) bool {
	sess := SessionFromContext(r.Context())
	return sess != nil && sess.IsAdmin
}

// canAdminRepo reports whether the session may manage (tenant, repo) settings:
// global admin, or holder of the repo-level admin permission. Errors (including
// no-such-repo) deny — callers respond with uniform 404.
func (s *server) canAdminRepo(r *http.Request, tenant, repo string) bool {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		return false
	}
	if sess.IsAdmin {
		return true
	}
	perm, err := s.store.LookupRepoPerm(r.Context(), actorFromSession(sess), tenant, repo)
	return err == nil && perm == auth.PermAdmin
}
