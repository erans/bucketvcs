package web

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
	"github.com/bucketvcs/bucketvcs/internal/oidc"
	"golang.org/x/oauth2"
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
	OIDC       *OIDCProvider      // nil => OIDC login disabled
	Content    ContentStore       // nil => code browse disabled (routes 404)
}

type server struct {
	store      DataStore
	logger     *slog.Logger
	limiter    *ratelimit.Limiter
	render     *renderer
	ttl        time.Duration
	trustProxy bool
	mux        *http.ServeMux
	oidc       *OIDCProvider
	content    ContentStore
	oauthCfg   *oauth2.Config
	verifier   idTokenVerifier
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
		content:    d.Content,
		mux:        http.NewServeMux(),
	}
	if d.OIDC != nil {
		if len(d.OIDC.HMACKey) < 16 {
			// Without a real key the bvcs_oidc temp cookie is forgeable.
			panic("web: oidc: HMACKey must be at least 16 bytes")
		}
		s.oidc = d.OIDC
		s.oauthCfg = d.OIDC.oauthConfig()
		s.verifier = d.OIDC.Verifier // used by handleOIDCCallback (Task 9)
		if s.verifier == nil {
			s.verifier = oidc.NewVerifier()
		}
	}
	s.mux.Handle("/_ui/static/", staticHandler(d.UIDir))
	s.mux.HandleFunc("/login", s.handleLogin)
	s.mux.HandleFunc("/logout", s.handleLogout)
	if s.oidc != nil {
		s.mux.HandleFunc("/login/oidc", s.handleOIDCAuthorize)
		s.mux.HandleFunc("/login/oidc/callback", s.handleOIDCCallback)
	}
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

// safeNext returns a local redirect target, defaulting to "/". Prevents open
// redirects: it rejects empty, non-"/"-prefixed, "//"-prefixed (protocol-relative),
// and any value containing a backslash (browsers normalize "\" to "/" in the
// authority position, so "/\evil.com" would redirect off-site).
func safeNext(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.ContainsRune(v, '\\') {
		return "/"
	}
	return v
}
