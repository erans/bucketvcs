package web

import "net/http"

func (s *server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.handleBrowse(w, r) // repo browse (Phase 2); 404s for non-repo paths
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
		// takeFlash: redirect targets like repo delete land on "/" — consume
		// the notice here so it shows once instead of going stale.
		base:  base{Session: sess, CSRF: tok, Flash: takeFlash(w, r)},
		Repos: grouped,
	})
	EmitRequestMetric(r.Context(), s.logger, "landing", 200)
}
