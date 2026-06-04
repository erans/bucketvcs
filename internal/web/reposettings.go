package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/gateway/routenames"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
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
	case "rename", "delete", "access", "webhooks", "policy", "hooks":
		// Tasks 8-12 replace these cases one by one.
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
