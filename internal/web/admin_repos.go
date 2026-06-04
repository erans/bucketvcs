package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/gateway/routenames"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

type adminReposData struct {
	base
	Repos       []Repo
	CanRegister bool // s.repoInit != nil → show register form
}

// handleAdminRepos renders GET /admin/repos: all registered repos plus a
// registration form. requireAdmin (global-admin only). The list comes from
// ListAccessibleRepos with an admin actor (returns ALL repos). Repo deletion
// is intentionally NOT offered here — each row links to the repo's settings
// page, which holds the single delete path in the codebase.
func (s *server) handleAdminRepos(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess := SessionFromContext(r.Context())
	repos, err := s.store.ListAccessibleRepos(r.Context(), actorFromSession(sess))
	if err != nil {
		s.logger.Error("admin repos: list", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	d := adminReposData{
		base:        base{Session: sess, CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Repos:       repos,
		CanRegister: s.repoInit != nil,
	}
	if err := s.renderBuffered(w, "admin_repos.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "admin_repos", http.StatusOK)
}

// handleAdminRepoRegister processes POST /admin/repos/register: it initializes
// the repo's storage layout IN-PROCESS (s.repoInit) then registers the repo in
// the auth DB, mirroring the CLI's init-then-register ordering (a
// half-registered repo with no storage is worse than initialized storage with a
// missing registry row, which a register retry heals).
func (s *server) handleAdminRepoRegister(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	const dest = "/admin/repos"
	tenant := r.PostFormValue("tenant")
	name := r.PostFormValue("name")
	if !routenames.ValidateName(tenant) || !routenames.ValidateName(name) {
		EmitAdminActionMetric(r.Context(), s.logger, "admin_repos", "register", "invalid")
		s.redirectFlash(w, r, dest, "invalid tenant or repo name")
		return
	}
	// No storage handle → registration is unavailable; surface as 404 so the
	// route is indistinguishable from a disabled feature.
	if s.repoInit == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}

	sess := SessionFromContext(r.Context())
	actor := ""
	if sess != nil {
		actor = sess.Name
	}

	// Init storage FIRST (mirrors the CLI). repo.Create returns
	// repoerrs.ErrRepoExists when the root manifest already exists in storage.
	// That is not a hard failure here: the storage is intact and the registry
	// row may simply be missing (a half-registered state), so we CONTINUE to
	// RegisterRepoIfNew which heals it. Any other error aborts before touching
	// the registry.
	if err := s.repoInit(r.Context(), tenant, name, actor); err != nil {
		if !errors.Is(err, repoerrs.ErrRepoExists) {
			s.logger.Error("admin repos: storage init", "tenant", tenant, "repo", name, "err", err)
			EmitAdminActionMetric(r.Context(), s.logger, "admin_repos", "register", "error")
			s.redirectFlash(w, r, dest, "storage init failed")
			return
		}
		// ErrRepoExists → fall through to register (healing path).
	}

	inserted, err := s.store.RegisterRepoIfNew(r.Context(), tenant, name)
	if err != nil {
		s.logger.Error("admin repos: register", "tenant", tenant, "repo", name, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "admin_repos", "register", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !inserted {
		EmitAdminActionMetric(r.Context(), s.logger, "admin_repos", "register", "invalid")
		s.redirectFlash(w, r, dest, "repo already registered")
		return
	}

	// Fail-open webhook: an enqueue error must not block the registration.
	if s.webhooks != nil {
		if err := s.webhooks.Enqueue(r.Context(), webhooks.EventRepoCreated, tenant, name, actor, webhooks.RepoLifecyclePayload{}); err != nil {
			s.logger.Warn("webhooks.enqueue_failed", "event", "repo.created", "tenant", tenant, "repo", name, "err", err)
		}
	}
	s.emitAdmin(r.Context(), "repo.created",
		slog.String("tenant", tenant), slog.String("repo", name))
	EmitAdminActionMetric(r.Context(), s.logger, "admin_repos", "register", "ok")
	s.redirectFlash(w, r, dest, "repo registered: "+tenant+"/"+name)
}
