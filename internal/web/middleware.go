package web

import (
	"net/http"
	"sync"
	"time"
)

const sessionCookieName = "bvcs_session"

// sessionMiddleware loads a session from the cookie (if present and live),
// slides its expiry within the absolute max-age cap, and attaches it to
// the request context. Anonymous requests pass through with a nil session.
//
// maxAge bounds total session lifetime from creation (A2): ttl alone is a
// pure idle timeout, so without the cap an active session — including a
// stolen cookie under continuous use — never expires. Sessions past
// created+maxAge are deleted and treated as anonymous; the slide is
// clamped so expiry never extends past created+maxAge. A zero CreatedAt
// (unknown age) skips the cap rather than killing the session.
func sessionMiddleware(store DataStore, ttl, maxAge time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
				if sess, err := store.LookupSession(r.Context(), c.Value); err == nil {
					if !sess.CreatedAt.IsZero() && maxAge > 0 && time.Since(sess.CreatedAt) > maxAge {
						_ = store.DeleteSession(r.Context(), c.Value) // past absolute cap: revoke
					} else {
						slide := ttl
						if !sess.CreatedAt.IsZero() && maxAge > 0 {
							if remaining := time.Until(sess.CreatedAt.Add(maxAge)); remaining < slide {
								slide = remaining
							}
						}
						if slide > 0 {
							_ = store.TouchSession(r.Context(), c.Value, slide) // best-effort sliding expiry
						}
						r = r.WithContext(withSession(r.Context(), sess))
					}
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
