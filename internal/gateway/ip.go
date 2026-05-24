package gateway

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the request's client IP. When trustProxyHeaders is set,
// the RIGHTMOST entry of X-Forwarded-For takes precedence over RemoteAddr.
// When false, RemoteAddr.Host is used unconditionally (defends against
// client-spoofed X-F-F when not behind a proxy).
//
// IMPORTANT: we take the RIGHTMOST hop, not the leftmost. With a standard
// appending reverse-proxy configuration (nginx's `$proxy_add_x_forwarded_for`,
// Envoy, ALB), the rightmost entry is the value the trusted proxy appended
// and is therefore not client-spoofable. The leftmost entry is whatever the
// client sent and an attacker can rotate it to defeat the per-IP rate limit
// (fresh fake leftmost → fresh bucket) or to poison a victim's bucket. For
// chained-proxy deployments (LB → reverse-proxy → app), this yields the
// previous-hop IP, not the original client; a future `--trusted-proxy-hops=N`
// flag can strip N rightmost hops once operators ask. Today: one trusted
// proxy directly in front of the gateway.
//
// Operators behind a reverse proxy MUST enable trustProxyHeaders so the
// rate limiter keys on the proxy-appended client IP; operators NOT behind
// a proxy MUST NOT enable it (the header is then entirely client-supplied).
func ClientIP(r *http.Request, trustProxyHeaders bool) string {
	if trustProxyHeaders {
		// Use Values, not Get: an attacker can prepend a duplicate
		// X-Forwarded-For header line, and Get returns only the first.
		// The proxy's appended header is the LAST one seen by the
		// gateway, so we take the rightmost hop of the LAST header line.
		if xffs := r.Header.Values("X-Forwarded-For"); len(xffs) > 0 {
			xff := xffs[len(xffs)-1]
			last := xff
			if i := strings.LastIndexByte(xff, ','); i >= 0 {
				last = xff[i+1:]
			}
			if v := strings.TrimSpace(last); v != "" {
				return v
			}
			// Empty / whitespace-only trailing segment (e.g., "10.0.0.1, ")
			// must not produce an empty bucket key — fall through to
			// RemoteAddr so all such requests don't collide on "".
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		return r.RemoteAddr
	}
	return host
}
