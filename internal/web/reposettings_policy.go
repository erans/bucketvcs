package web

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/policy"
)

// policyData is the view-model for the repo-settings POLICY tab.
type policyData struct {
	base
	Tenant, Repo string
	IsAdmin      bool // global admin (controls hooks-tab nav visibility)
	Enabled      bool // false when s.policy == nil
	Refs         []policy.ProtectedRef
	Paths        []policy.ProtectedPath
}

// policyBase returns the canonical redirect target for the policy tab.
func (sr settingsRoute) policyBase() string {
	return "/" + sr.tenant + "/" + sr.repo + "/settings/policy"
}

// repoSettingsPolicy is the action dispatcher for the POLICY tab.
// The chassis (handleRepoSettings) has already authorized the actor.
func (s *server) repoSettingsPolicy(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	switch sr.action {
	case "":
		s.policyPage(w, r, sr)
	case "refs/add":
		s.policyRefsAdd(w, r, sr)
	case "refs/remove":
		s.policyRefsRemove(w, r, sr)
	case "paths/add":
		s.policyPathsAdd(w, r, sr)
	case "paths/remove":
		s.policyPathsRemove(w, r, sr)
	default:
		s.renderError(w, r, http.StatusNotFound, "not found")
	}
}

// policyPage renders GET /{t}/{r}/settings/policy.
func (s *server) policyPage(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d := policyData{
		base:    base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:  sr.tenant,
		Repo:    sr.repo,
		IsAdmin: isGlobalAdmin(r),
		Enabled: s.policy != nil,
	}
	if s.policy != nil {
		refs, err := s.policy.List(r.Context(), sr.tenant, sr.repo)
		if err != nil {
			s.logger.Error("policy: list refs", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		paths, err := s.policy.ListPathRules(r.Context(), sr.tenant, sr.repo)
		if err != nil {
			s.logger.Error("policy: list paths", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		d.Refs = refs
		d.Paths = paths
	}
	if err := s.renderBuffered(w, "reposettings_policy.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_policy", http.StatusOK)
}

// policyRefsAdd processes POST /{t}/{r}/settings/policy/refs/add.
func (s *server) policyRefsAdd(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.policy == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	pattern := r.PostFormValue("pattern")
	if pattern == "" {
		EmitAdminActionMetric(r.Context(), s.logger, "policy", "ref_add", "invalid")
		s.redirectFlash(w, r, sr.policyBase(), "pattern required")
		return
	}
	blockDeletion := r.PostFormValue("block_deletion") == "on"
	blockForcePush := r.PostFormValue("block_force_push") == "on"
	if !blockDeletion && !blockForcePush {
		EmitAdminActionMetric(r.Context(), s.logger, "policy", "ref_add", "invalid")
		s.redirectFlash(w, r, sr.policyBase(), "select at least one protection")
		return
	}
	if err := s.policy.Add(r.Context(), policy.ProtectedRef{
		Tenant:         sr.tenant,
		Repo:           sr.repo,
		RefnamePattern: pattern,
		BlockDeletion:  blockDeletion,
		BlockForcePush: blockForcePush,
		CreatedAt:      time.Now(),
	}); err != nil {
		// Surface all Add errors as operator-grade flashes. The service returns
		// user-actionable messages (empty pattern, bad glob) and also DB errors
		// whose wrapped text is operator-readable. ErrInvalidInput and ErrConflict
		// are also matched for interface conformance.
		EmitAdminActionMetric(r.Context(), s.logger, "policy", "ref_add", "invalid")
		s.redirectFlash(w, r, sr.policyBase(), err.Error())
		return
	}
	s.emitAdmin(r.Context(), "policy.ref.rule_added",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("pattern", pattern),
		slog.Bool("block_deletion", blockDeletion),
		slog.Bool("block_force_push", blockForcePush))
	EmitAdminActionMetric(r.Context(), s.logger, "policy", "ref_add", "ok")
	s.redirectFlash(w, r, sr.policyBase(), "ref rule added")
}

// policyRefsRemove processes POST /{t}/{r}/settings/policy/refs/remove.
func (s *server) policyRefsRemove(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.policy == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	pattern := r.PostFormValue("pattern")
	if pattern == "" {
		EmitAdminActionMetric(r.Context(), s.logger, "policy", "ref_remove", "invalid")
		s.redirectFlash(w, r, sr.policyBase(), "pattern required")
		return
	}
	if err := s.policy.Remove(r.Context(), sr.tenant, sr.repo, pattern); err != nil {
		if errors.Is(err, policy.ErrNotFound) {
			EmitAdminActionMetric(r.Context(), s.logger, "policy", "ref_remove", "invalid")
			s.redirectFlash(w, r, sr.policyBase(), "no such rule")
			return
		}
		s.logger.Error("policy: remove ref rule", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "policy", "ref_remove", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "policy.ref.rule_removed",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("pattern", pattern))
	EmitAdminActionMetric(r.Context(), s.logger, "policy", "ref_remove", "ok")
	s.redirectFlash(w, r, sr.policyBase(), "ref rule removed")
}

// policyPathsAdd processes POST /{t}/{r}/settings/policy/paths/add.
func (s *server) policyPathsAdd(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.policy == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	refnamePattern := r.PostFormValue("refname_pattern")
	pathPattern := r.PostFormValue("path_pattern")
	if refnamePattern == "" || pathPattern == "" {
		EmitAdminActionMetric(r.Context(), s.logger, "policy", "path_add", "invalid")
		s.redirectFlash(w, r, sr.policyBase(), "refname_pattern and path_pattern required")
		return
	}
	if err := s.policy.AddPathRule(r.Context(), policy.ProtectedPath{
		Tenant:         sr.tenant,
		Repo:           sr.repo,
		RefnamePattern: refnamePattern,
		PathPattern:    pathPattern,
		CreatedAt:      time.Now(),
	}); err != nil {
		// Surface all AddPathRule errors as operator-grade flashes. The service
		// validates patterns (invalid refname_pattern, invalid path_pattern) and
		// returns operator-readable messages. ErrInvalidInput is also matched for
		// interface conformance.
		EmitAdminActionMetric(r.Context(), s.logger, "policy", "path_add", "invalid")
		s.redirectFlash(w, r, sr.policyBase(), err.Error())
		return
	}
	s.emitAdmin(r.Context(), "policy.path.rule_added",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("refname_pattern", refnamePattern),
		slog.String("path_pattern", pathPattern))
	EmitAdminActionMetric(r.Context(), s.logger, "policy", "path_add", "ok")
	s.redirectFlash(w, r, sr.policyBase(), "path rule added")
}

// policyPathsRemove processes POST /{t}/{r}/settings/policy/paths/remove.
func (s *server) policyPathsRemove(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.policy == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	refnamePattern := r.PostFormValue("refname_pattern")
	pathPattern := r.PostFormValue("path_pattern")
	if refnamePattern == "" || pathPattern == "" {
		EmitAdminActionMetric(r.Context(), s.logger, "policy", "path_remove", "invalid")
		s.redirectFlash(w, r, sr.policyBase(), "both patterns required")
		return
	}
	if err := s.policy.RemovePathRule(r.Context(), sr.tenant, sr.repo, refnamePattern, pathPattern); err != nil {
		if errors.Is(err, policy.ErrNotFound) {
			EmitAdminActionMetric(r.Context(), s.logger, "policy", "path_remove", "invalid")
			s.redirectFlash(w, r, sr.policyBase(), "no such rule")
			return
		}
		s.logger.Error("policy: remove path rule", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "policy", "path_remove", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "policy.path.rule_removed",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("refname_pattern", refnamePattern),
		slog.String("path_pattern", pathPattern))
	EmitAdminActionMetric(r.Context(), s.logger, "policy", "path_remove", "ok")
	s.redirectFlash(w, r, sr.policyBase(), "path rule removed")
}
