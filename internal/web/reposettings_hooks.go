package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/hooks"
)

// hooksData is the view-model for the repo-settings HOOKS tab.
type hooksData struct {
	base
	Tenant, Repo string
	IsAdmin      bool // global admin (same flag surfaced to nav)
	Enabled      bool // false when s.hooks == nil
	Hooks        []hooks.Row
}

// hooksBase returns the canonical redirect target for the hooks tab.
func (sr settingsRoute) hooksBase() string {
	return "/" + sr.tenant + "/" + sr.repo + "/settings/hooks"
}

// repoSettingsHooks is the action dispatcher for the HOOKS tab.
// The chassis (handleRepoSettings) has already verified canAdminRepo; here we
// tighten further to global-admin only — hooks reference operator scripts in
// --hooks-dir, so allowing repo-admins to register them would be privilege
// escalation (a repo-admin could attach an arbitrary operator script to a push
// path). Uniform 404 in all non-admin paths (anti-enumeration).
func (s *server) repoSettingsHooks(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if !isGlobalAdmin(r) {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	switch sr.action {
	case "":
		s.hooksPage(w, r, sr)
	case "add":
		s.hooksAdd(w, r, sr)
	case "remove":
		s.hooksRemove(w, r, sr)
	case "enable":
		s.hooksSetEnabled(w, r, sr, true)
	case "disable":
		s.hooksSetEnabled(w, r, sr, false)
	default:
		s.renderError(w, r, http.StatusNotFound, "not found")
	}
}

// hooksPage renders GET /{t}/{r}/settings/hooks.
func (s *server) hooksPage(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d := hooksData{
		base:    base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:  sr.tenant,
		Repo:    sr.repo,
		IsAdmin: true, // gate at the top of repoSettingsHooks ensures this
		Enabled: s.hooks != nil,
	}
	if s.hooks != nil {
		rows, err := s.hooks.List(r.Context(), sr.tenant, sr.repo, "")
		if err != nil {
			s.logger.Error("hooks: list", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		d.Hooks = rows
	}
	if err := s.renderBuffered(w, "reposettings_hooks.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_hooks", http.StatusOK)
}

// hooksAdd processes POST /{t}/{r}/settings/hooks/add.
func (s *server) hooksAdd(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.hooks == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	trigger := r.PostFormValue("trigger")
	if trigger != hooks.TriggerPreReceive && trigger != hooks.TriggerPostReceive {
		EmitAdminActionMetric(r.Context(), s.logger, "hooks", "add", "invalid")
		s.redirectFlash(w, r, sr.hooksBase(), "invalid trigger")
		return
	}
	scriptName := r.PostFormValue("script_name")
	if !hooks.ValidScriptName(scriptName) {
		EmitAdminActionMetric(r.Context(), s.logger, "hooks", "add", "invalid")
		s.redirectFlash(w, r, sr.hooksBase(), "invalid script name ([A-Za-z0-9._-])")
		return
	}
	sortOrderStr := r.PostFormValue("sort_order")
	sortOrder := 0
	if sortOrderStr != "" {
		n, err := strconv.Atoi(sortOrderStr)
		if err != nil {
			EmitAdminActionMetric(r.Context(), s.logger, "hooks", "add", "invalid")
			s.redirectFlash(w, r, sr.hooksBase(), "sort_order must be an integer")
			return
		}
		sortOrder = n
	}
	if err := s.hooks.Add(r.Context(), hooks.Row{
		Tenant:     sr.tenant,
		Repo:       sr.repo,
		Trigger:    trigger,
		ScriptName: scriptName,
		SortOrder:  sortOrder,
		Enabled:    true,
		Now:        time.Now(),
	}); err != nil {
		// Surface known validation errors (prefixed "hooks: ") as flashes; mask
		// unknown (wrapped DB) errors as a 500 so their text never leaks.
		if !flashableErr(err) {
			s.logger.Error("hooks: add", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			EmitAdminActionMetric(r.Context(), s.logger, "hooks", "add", "error")
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		EmitAdminActionMetric(r.Context(), s.logger, "hooks", "add", "invalid")
		s.redirectFlash(w, r, sr.hooksBase(), err.Error())
		return
	}
	s.emitAdmin(r.Context(), "policy.hook.added",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("trigger", trigger),
		slog.String("script", scriptName),
		slog.Int("sort_order", sortOrder))
	EmitAdminActionMetric(r.Context(), s.logger, "hooks", "add", "ok")
	s.redirectFlash(w, r, sr.hooksBase(), "hook added")
}

// hooksRemove processes POST /{t}/{r}/settings/hooks/remove.
func (s *server) hooksRemove(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.hooks == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	trigger := r.PostFormValue("trigger")
	scriptName := r.PostFormValue("script_name")
	if err := s.hooks.Remove(r.Context(), sr.tenant, sr.repo, trigger, scriptName); err != nil {
		if errors.Is(err, hooks.ErrNotFound) {
			EmitAdminActionMetric(r.Context(), s.logger, "hooks", "remove", "invalid")
			s.redirectFlash(w, r, sr.hooksBase(), "no such hook")
			return
		}
		s.logger.Error("hooks: remove", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "hooks", "remove", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "policy.hook.removed",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("trigger", trigger),
		slog.String("script", scriptName))
	EmitAdminActionMetric(r.Context(), s.logger, "hooks", "remove", "ok")
	s.redirectFlash(w, r, sr.hooksBase(), "hook removed")
}

// hooksSetEnabled processes POST /{t}/{r}/settings/hooks/enable and /disable.
func (s *server) hooksSetEnabled(w http.ResponseWriter, r *http.Request, sr settingsRoute, enabled bool) {
	if s.hooks == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	trigger := r.PostFormValue("trigger")
	scriptName := r.PostFormValue("script_name")
	if err := s.hooks.SetEnabled(r.Context(), sr.tenant, sr.repo, trigger, scriptName, enabled, time.Now()); err != nil {
		if errors.Is(err, hooks.ErrNotFound) {
			action := "enable"
			if !enabled {
				action = "disable"
			}
			EmitAdminActionMetric(r.Context(), s.logger, "hooks", action, "invalid")
			s.redirectFlash(w, r, sr.hooksBase(), "no such hook")
			return
		}
		s.logger.Error("hooks: set enabled", "tenant", sr.tenant, "repo", sr.repo, "enabled", enabled, "err", err)
		action := "enable"
		if !enabled {
			action = "disable"
		}
		EmitAdminActionMetric(r.Context(), s.logger, "hooks", action, "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	event := "policy.hook.enabled"
	action := "enable"
	flash := "hook enabled"
	if !enabled {
		event = "policy.hook.disabled"
		action = "disable"
		flash = "hook disabled"
	}
	s.emitAdmin(r.Context(), event,
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("trigger", trigger),
		slog.String("script", scriptName))
	EmitAdminActionMetric(r.Context(), s.logger, "hooks", action, "ok")
	s.redirectFlash(w, r, sr.hooksBase(), flash)
}
