package gateway

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// AuthMode controls which requests require authentication.
type AuthMode int

const (
	// AuthAnonymous accepts all requests without authentication.
	AuthAnonymous AuthMode = iota
	// AuthWriteOnly accepts read requests anonymously; write requests require
	// the configured token.
	AuthWriteOnly
	// AuthAll requires the configured token for every request.
	AuthAll
)

const authRealm = `Basic realm="bucketvcs"`
const authUser = "bucketvcs"

// classify returns true for write requests (receive-pack), false for reads.
func classify(r *http.Request) (isWrite bool) {
	if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
		return true
	}
	if strings.HasSuffix(r.URL.Path, "/info/refs") {
		return r.URL.Query().Get("service") == "git-receive-pack"
	}
	return false
}

// authorize returns true if the request should proceed; if false, it has
// already written a 401 response.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	switch s.opts.AuthMode {
	case AuthAnonymous:
		return true
	case AuthWriteOnly:
		if !classify(r) {
			return true
		}
	case AuthAll:
		// Fall through to credential check.
	}
	user, pass, ok := r.BasicAuth()
	if !ok ||
		subtle.ConstantTimeCompare([]byte(user), []byte(authUser)) != 1 ||
		subtle.ConstantTimeCompare([]byte(pass), []byte(s.opts.AuthToken)) != 1 {
		w.Header().Set("WWW-Authenticate", authRealm)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return false
	}
	return true
}
