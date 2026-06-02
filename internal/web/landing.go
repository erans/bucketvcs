package web

import "net/http"

func (s *server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		// Unknown human path (repo browse is Phase 2). 404 for now.
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	sess := SessionFromContext(r.Context())
	repos, err := s.store.ListAccessibleRepos(r.Context(), actorFromSession(sess))
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "could not list repositories")
		return
	}
	grouped := map[string][]Repo{}
	for _, rp := range repos {
		grouped[rp.Tenant] = append(grouped[rp.Tenant], rp)
	}
	tok := issueCSRF(w, requestIsTLS(r, s.trustProxy)) // for the logout form in the layout
	_ = s.render.render(w, "landing.html", landingData{
		base:  base{Session: sess, CSRF: tok},
		Repos: grouped,
	})
	EmitRequestMetric(r.Context(), s.logger, "landing", 200)
}
