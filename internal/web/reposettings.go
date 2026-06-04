package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/gateway/routenames"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// settingsRoute is the parsed shape of "/{tenant}/{repo}/settings[/{tab}[/{action...}]]".
type settingsRoute struct {
	tenant, repo, tab, action string
}

// parseSettingsPath parses a repo-settings URL. ok=false means "not a settings
// path" (caller falls through to browse). It validates tenant/repo names.
func parseSettingsPath(p string) (settingsRoute, bool) {
	p = strings.TrimPrefix(p, "/")
	seg := strings.SplitN(p, "/", 5)
	if len(seg) < 3 || seg[2] != "settings" {
		return settingsRoute{}, false
	}
	if !routenames.ValidateName(seg[0]) || !routenames.ValidateName(seg[1]) {
		return settingsRoute{}, false
	}
	sr := settingsRoute{tenant: seg[0], repo: seg[1]}
	if len(seg) >= 4 {
		sr.tab = seg[3]
	}
	if len(seg) == 5 {
		sr.action = seg[4]
	}
	return sr, true
}

// repoSettingsData is the view-model for the repo-settings pages.
type repoSettingsData struct {
	base
	Tenant, Repo string
	Tab          string       // active tab for the nav
	Public       bool         // current public-read flag
	Quota        *quota.State // nil => hide quota section
	IsAdmin      bool         // global admin (controls hooks-tab visibility + Task 8/12 surfaces)
}

// handleRepoSettings authorizes the repo then dispatches to the active tab.
func (s *server) handleRepoSettings(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if !s.requireUser(w, r) {
		return
	}
	if !s.canAdminRepo(r, sr.tenant, sr.repo) {
		// Uniform 404 for not-found and not-authorized (anti-enumeration).
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	switch sr.tab {
	case "":
		s.repoSettingsGeneral(w, r, sr)
	case "public":
		s.repoSettingsSetPublic(w, r, sr)
	case "rename":
		s.repoSettingsRename(w, r, sr)
	case "delete":
		s.repoSettingsDelete(w, r, sr)
	case "access":
		s.repoSettingsAccess(w, r, sr)
	case "webhooks":
		s.repoSettingsWebhooks(w, r, sr)
	case "policy":
		s.repoSettingsPolicy(w, r, sr)
	case "hooks":
		// Task 12 replaces this case.
		s.renderError(w, r, http.StatusNotFound, "not found")
	default:
		s.renderError(w, r, http.StatusNotFound, "not found")
	}
}

// repoSettingsGeneral renders the GENERAL tab (public toggle + read-only quota).
func (s *server) repoSettingsGeneral(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	flags, err := s.store.GetRepoFlags(r.Context(), sr.tenant, sr.repo)
	if err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("repo settings: get flags", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	d := repoSettingsData{
		base:    base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:  sr.tenant,
		Repo:    sr.repo,
		Tab:     "general",
		Public:  flags.PublicRead,
		IsAdmin: isGlobalAdmin(r),
	}
	if s.quotas != nil {
		if st, qerr := s.quotas.Get(r.Context(), sr.tenant); qerr == nil {
			d.Quota = &st
		} else {
			s.logger.Warn("repo settings: get quota", "tenant", sr.tenant, "err", qerr)
		}
	}
	if err := s.renderBuffered(w, "reposettings.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings", http.StatusOK)
}

// repoSettingsSetPublic toggles repos.public_read (POST /settings/public).
func (s *server) repoSettingsSetPublic(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if !s.postGuard(w, r) {
		return
	}
	public := r.PostFormValue("public") == "on"
	if err := s.store.SetRepoPublic(r.Context(), sr.tenant, sr.repo, public); err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("repo settings: set public", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "repo", "public_set", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "repo.public_set",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo), slog.Bool("public", public))
	EmitAdminActionMetric(r.Context(), s.logger, "repo", "public_set", "ok")
	msg := "repo is now private"
	if public {
		msg = "repo is now public"
	}
	s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings", msg)
}

// repoSettingsRename renames (tenant, repo) to (tenant, newName)
// (POST /settings/rename). Repo-admin or global-admin (chassis-gated).
func (s *server) repoSettingsRename(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if !s.postGuard(w, r) {
		return
	}
	newName := r.PostFormValue("newname")
	if !routenames.ValidateName(newName) {
		EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "invalid")
		s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings", "invalid repo name")
		return
	}
	if err := s.store.RenameRepo(r.Context(), sr.tenant, sr.repo, newName); err != nil {
		switch {
		case errors.Is(err, auth.ErrNoSuchRepo):
			s.renderError(w, r, http.StatusNotFound, "not found")
		case errors.Is(err, sqlitestore.ErrRepoExists):
			EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "conflict")
			s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings", "name already taken")
		default:
			s.logger.Error("repo rename", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "error")
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
		}
		return
	}
	sess := SessionFromContext(r.Context())
	actor := ""
	if sess != nil {
		actor = sess.Name
	}
	if s.webhooks != nil {
		payload := webhooks.RepoRenamedPayload{OldName: sr.repo, NewName: newName}
		if err := s.webhooks.Enqueue(r.Context(), webhooks.EventRepoRenamed, sr.tenant, sr.repo, actor, payload); err != nil {
			s.logger.Warn("webhooks.enqueue_failed", "event", "repo.renamed", "err", err)
		}
	}
	s.emitAdmin(r.Context(), "repo.renamed",
		slog.String("tenant", sr.tenant), slog.String("from", sr.repo), slog.String("to", newName))
	EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "ok")
	s.redirectFlash(w, r, "/"+sr.tenant+"/"+newName+"/settings", "repo renamed")
}

// repoSettingsDelete deletes (tenant, repo) (POST /settings/delete). This is
// global-admin-only — repo-admins get a uniform 404 BEFORE the CSRF check so
// the surface is indistinguishable from a non-existent route. Storage objects
// are NOT purged (operator must run the CLI with --purge-storage).
func (s *server) repoSettingsDelete(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if !isGlobalAdmin(r) {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	if r.PostFormValue("confirm") != sr.tenant+"/"+sr.repo {
		EmitAdminActionMetric(r.Context(), s.logger, "repo", "delete", "invalid")
		s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings", "type the repo name to confirm")
		return
	}
	sess := SessionFromContext(r.Context())
	actor := ""
	if sess != nil {
		actor = sess.Name
	}
	// Enqueue BEFORE the row delete so Enqueue's endpoint SELECT still finds
	// the (tenant, repo) rows; DeleteRepoCascade leaves webhook tables intact
	// so the repo.deleted delivery can drain. Fail-open: an enqueue error must
	// not block the delete (mirrors the CLI).
	if s.webhooks != nil {
		if err := s.webhooks.Enqueue(r.Context(), webhooks.EventRepoDeleted, sr.tenant, sr.repo, actor, webhooks.RepoLifecyclePayload{}); err != nil {
			s.logger.Warn("webhooks.enqueue_failed", "event", "repo.deleted", "err", err)
		}
	}
	if err := s.store.DeleteRepoCascade(r.Context(), sr.tenant, sr.repo); err != nil {
		if errors.Is(err, sqlitestore.ErrCascadeUnsupportedBackend) {
			// Postgres can't suppress the webhook_endpoints cascade (M15.1
			// drain design). Refuse with a flash rather than a 500 — this is an
			// operator-environment limitation, not a server fault.
			EmitAdminActionMetric(r.Context(), s.logger, "repo", "delete", "error")
			s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings",
				"repo delete is not supported on this server's database backend (postgres); use the CLI on a sqlite/libsql deployment")
			return
		}
		s.logger.Error("repo delete", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "repo", "delete", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "repo.deleted",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo))
	EmitAdminActionMetric(r.Context(), s.logger, "repo", "delete", "ok")
	s.redirectFlash(w, r, "/", "repo "+sr.tenant+"/"+sr.repo+" deleted (storage not purged; see CLI --purge-storage)")
}
