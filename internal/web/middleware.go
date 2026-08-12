package web

import (
	"net/http"
	"sync"
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

// warnIfProxyHeaderMismatched logs a warning when X-Forwarded-Proto is seen
// but trustProxy is false — a common misconfig that silently downgrades
// Secure cookies.
func warnIfProxyHeaderMismatched(r *http.Request, trustProxy bool, logger interface{ Warn(string, ...any) }) {
	if !trustProxy && r.Header.Get("X-Forwarded-Proto") != "" && logger != nil {
		logger.Warn("web: X-Forwarded-Proto seen but --trust-proxy-headers not set; Secure cookies may be downgraded")
	}
}

// proxyHeaderWarningMiddleware wires the mismatch diagnostic into every web
// request while bounding it to one warning per handler instance. The warning
// is intentionally outside cookie-setting handlers so it also catches a proxy
// misconfiguration before the first login attempt.
func proxyHeaderWarningMiddleware(next http.Handler, trustProxy bool, logger interface{ Warn(string, ...any) }) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !trustProxy && r.Header.Get("X-Forwarded-Proto") != "" {
			once.Do(func() { warnIfProxyHeaderMismatched(r, trustProxy, logger) })
		}
		next.ServeHTTP(w, r)
	})
}
