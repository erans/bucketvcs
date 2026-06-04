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

// uiCSP is the strict policy for all UI responses. Possible because the UI has
// zero inline styles/scripts (class-based chroma + diff classes). Blocks remote
// README images by design (img-src 'self') — see the operator guide. The raw
// endpoint overrides this with its own stricter policy.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

func cspMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", uiCSP)
		next.ServeHTTP(w, r)
	})
}

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

	// Phase 3 admin services. All nil-able; nil disables the corresponding
	// settings pages (they render a "not enabled" notice or 404).
	Webhooks       WebhookAdmin
	Policy         PolicyAdmin
	Hooks          HookAdmin
	Quotas         QuotaAdmin
	QuotaReconcile QuotaReconciler
	RepoInit       RepoInitializer
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

	// Phase 3 admin services.
	webhooks       WebhookAdmin
	policy         PolicyAdmin
	hooks          HookAdmin
	quotas         QuotaAdmin
	quotaReconcile QuotaReconciler
	repoInit       RepoInitializer
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
		store:          d.Store,
		logger:         d.Logger,
		limiter:        d.Limiter,
		render:         r,
		ttl:            d.SessionTTL,
		trustProxy:     d.TrustProxy,
		content:        d.Content,
		mux:            http.NewServeMux(),
		webhooks:       d.Webhooks,
		policy:         d.Policy,
		hooks:          d.Hooks,
		quotas:         d.Quotas,
		quotaReconcile: d.QuotaReconcile,
		repoInit:       d.RepoInit,
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
	s.mux.HandleFunc("/_ui/static/chroma.css", chromaCSSHandler(d.UIDir))
	s.mux.HandleFunc("/login", s.handleLogin)
	s.mux.HandleFunc("/logout", s.handleLogout)
	if s.oidc != nil {
		s.mux.HandleFunc("/login/oidc", s.handleOIDCAuthorize)
		s.mux.HandleFunc("/login/oidc/callback", s.handleOIDCCallback)
	}
	s.mux.HandleFunc("/settings", s.handleSettings)
	s.mux.HandleFunc("/settings/password", s.handlePasswordChange)
	s.mux.HandleFunc("/settings/tokens", s.handleTokensPage)
	s.mux.HandleFunc("/settings/tokens/create", s.handleTokenCreate)
	s.mux.HandleFunc("/settings/tokens/revoke", s.handleTokenRevoke)
	s.mux.HandleFunc("/settings/tokens/rotate", s.handleTokenRotate)
	s.mux.HandleFunc("/settings/keys", s.handleKeysPage)
	s.mux.HandleFunc("/settings/keys/add", s.handleKeyAdd)
	s.mux.HandleFunc("/settings/keys/revoke", s.handleKeyRevoke)
	s.mux.HandleFunc("/admin", s.handleAdminIndex)
	s.mux.HandleFunc("/admin/users", s.handleAdminUsers)
	s.mux.HandleFunc("/admin/users/create", s.handleAdminUserCreate)
	s.mux.HandleFunc("/admin/users/disable", s.handleAdminUserDisable)
	s.mux.HandleFunc("/admin/users/enable", s.handleAdminUserEnable)
	s.mux.HandleFunc("/admin/users/delete", s.handleAdminUserDelete)
	s.mux.HandleFunc("/admin/users/email", s.handleAdminUserEmail)
	s.mux.HandleFunc("/admin/repos", s.handleAdminRepos)
	s.mux.HandleFunc("/admin/repos/register", s.handleAdminRepoRegister)
	s.mux.HandleFunc("/admin/quotas", s.handleAdminQuotas)
	s.mux.HandleFunc("/admin/quotas/set", s.handleAdminQuotaSet)
	s.mux.HandleFunc("/admin/quotas/clear", s.handleAdminQuotaClear)
	s.mux.HandleFunc("/admin/quotas/reconcile", s.handleAdminQuotaReconcile)
	s.mux.HandleFunc("/", s.handleLanding)

	return sessionMiddleware(s.store, s.ttl)(cspMiddleware(s.mux))
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
