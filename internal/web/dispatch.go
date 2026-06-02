package web

import (
	"net/http"
	"strings"
)

// Dispatcher routes git/internal traffic to gitHandler and everything else to
// uiHandler. Git and LFS paths always contain a ".git/" segment; gateway
// internals start with "/_" or are "/healthz". The UI owns "/_ui/" (its static
// assets) even though it shares the "/_" prefix, so that case is special-cased.
func Dispatcher(gitHandler, uiHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isGitOrInternal(r.URL.Path) {
			gitHandler.ServeHTTP(w, r)
			return
		}
		uiHandler.ServeHTTP(w, r)
	})
}

func isGitOrInternal(p string) bool {
	if strings.HasPrefix(p, "/_ui/") {
		return false // UI static assets
	}
	if p == "/healthz" || strings.HasPrefix(p, "/_") {
		return true
	}
	if strings.HasSuffix(p, ".git") || strings.Contains(p, ".git/") {
		return true
	}
	return false
}
