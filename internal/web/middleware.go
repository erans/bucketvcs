package web

import (
	"net/http"
	"time"
)

const sessionCookieName = "bvcs_session"

// sessionMiddleware loads a session from the cookie (if present and live),
// slides its expiry, and attaches it to the request context. Anonymous requests
// pass through with a nil session.
func sessionMiddleware(store DataStore, ttl time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
				if sess, err := store.LookupSession(r.Context(), c.Value); err == nil {
					_ = store.TouchSession(r.Context(), c.Value, ttl) // best-effort sliding expiry
					r = r.WithContext(withSession(r.Context(), sess))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requestIsTLS reports whether the original request arrived over TLS, honoring a
// trusted X-Forwarded-Proto when trustProxy is set.
func requestIsTLS(r *http.Request, trustProxy bool) bool {
	if r.TLS != nil {
		return true
	}
	if trustProxy && r.Header.Get("X-Forwarded-Proto") == "https" {
		return true
	}
	return false
}
