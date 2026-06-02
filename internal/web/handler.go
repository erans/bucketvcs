package web

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
)

// DefaultSessionTTL is used when Deps.SessionTTL is zero.
const DefaultSessionTTL = 168 * time.Hour

// Deps are the web handler's dependencies (composition-root wired).
type Deps struct {
	Store      DataStore
	Logger     *slog.Logger
	Limiter    *ratelimit.Limiter // nil => no rate limiting
	UIDir      string             // "" => embedded assets
	SessionTTL time.Duration      // 0 => DefaultSessionTTL
	TrustProxy bool               // for Secure-cookie / client-IP decisions
}

type server struct {
	store      DataStore
	logger     *slog.Logger
	limiter    *ratelimit.Limiter
	render     *renderer
	ttl        time.Duration
	trustProxy bool
	mux        *http.ServeMux
}

// NewHandler builds the web UI http.Handler. Panics only on an unrecoverable
// asset-parse error (embedded templates are validated at build/test time).
func NewHandler(d Deps) http.Handler {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.SessionTTL <= 0 {
		d.SessionTTL = DefaultSessionTTL
	}
	r, err := newRenderer(d.UIDir)
	if err != nil {
		panic("web: parse templates: " + err.Error())
	}
	s := &server{
		store:      d.Store,
		logger:     d.Logger,
		limiter:    d.Limiter,
		render:     r,
		ttl:        d.SessionTTL,
		trustProxy: d.TrustProxy,
		mux:        http.NewServeMux(),
	}
	s.mux.Handle("/_ui/static/", staticHandler(d.UIDir))
	s.mux.HandleFunc("/login", s.handleLogin)
	s.mux.HandleFunc("/logout", s.handleLogout)
	s.mux.HandleFunc("/", s.handleLanding)

	return sessionMiddleware(s.store, s.ttl)(s.mux)
}

// renderError writes a styled error page with the given status code.
func (s *server) renderError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.WriteHeader(code)
	_ = s.render.render(w, "error.html", errorData{
		base:    base{Session: SessionFromContext(r.Context())},
		Code:    code,
		Message: msg,
	})
	EmitRequestMetric(r.Context(), s.logger, "error", code)
}

// safeNext returns a local redirect target, defaulting to "/". Prevents open redirects.
func safeNext(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return "/"
	}
	return v
}
