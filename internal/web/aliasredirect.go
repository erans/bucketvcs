package web

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// aliasRedirect checks whether (tenant, name) is a rename alias to a live repo
// and, if so, writes a 302 to the same path with the repo segment swapped to
// the target, preserving the trailing sub-path and query. Returns true if it
// handled the request (redirect written). Auth is NOT bypassed — the redirect
// just points the browser at the canonical URL, which enforces its own auth.
func (s *server) aliasRedirect(w http.ResponseWriter, r *http.Request, tenant, name string) bool {
	resolver, ok := s.store.(auth.RepoAliasResolver)
	if !ok {
		return false
	}
	target, found, err := resolver.ResolveAlias(r.Context(), tenant, name)
	if err != nil || !found {
		return false
	}
	// Confirm the target is live before redirecting (defensive).
	if _, e := s.store.GetRepoFlags(r.Context(), tenant, target); e != nil {
		return false
	}
	oldPrefix := "/" + tenant + "/" + name
	newPrefix := "/" + tenant + "/" + target
	dest := newPrefix + strings.TrimPrefix(r.URL.Path, oldPrefix)
	if r.URL.RawQuery != "" {
		// Re-encode query via ParseQuery to strip attacker-controlled
		// raw encodings (e.g. %0d%0a) and ensure valid escaping.
		if q, err := url.ParseQuery(r.URL.RawQuery); err == nil {
			enc := q.Encode()
			if enc != "" {
				dest += "?" + enc
			}
		}
	}
	auth.EmitRepoAliasResolvedMetric(r.Context(), s.logger, "ui")
	http.Redirect(w, r, dest, http.StatusFound) // 302
	return true
}
