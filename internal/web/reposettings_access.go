package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// deployKeyRow is the per-row view model for the deploy-keys table. State and
// PermText are computed server-side so the template stays dumb.
type deployKeyRow struct {
	auth.SSHKey
	State    string // "active" | "revoked"
	PermText string // "read" | "write" | "admin"
}

// accessData is the view-model for the repo-settings ACCESS tab.
type accessData struct {
	base
	Tenant, Repo string
	IsAdmin      bool // global admin (controls hooks-tab nav visibility)
	Grants       []RepoGrant
	DeployKeys   []deployKeyRow
}

// repoSettingsAccess dispatches the ACCESS tab. The chassis (handleRepoSettings)
// has already authorized the actor as repo-admin or global-admin.
func (s *server) repoSettingsAccess(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	switch sr.action {
	case "":
		s.accessPage(w, r, sr)
	case "grant":
		s.accessGrant(w, r, sr)
	case "revoke":
		s.accessRevoke(w, r, sr)
	case "deploykey/add":
		s.accessDeployKeyAdd(w, r, sr)
	case "deploykey/revoke":
		s.accessDeployKeyRevoke(w, r, sr)
	default:
		s.renderError(w, r, http.StatusNotFound, "not found")
	}
}

// accessBase returns the redirect target for this tab's POST handlers.
func (sr settingsRoute) accessBase() string {
	return "/" + sr.tenant + "/" + sr.repo + "/settings/access"
}

// accessPage renders GET /settings/access.
func (s *server) accessPage(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	grants, err := s.store.ListRepoGrants(r.Context(), sr.tenant, sr.repo)
	if err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("access: list grants", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	keys, err := s.store.ListSSHKeysForRepo(r.Context(), sr.tenant, sr.repo)
	if err != nil {
		s.logger.Error("access: list deploy keys", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	rows := make([]deployKeyRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, deployKeyRow{SSHKey: k, State: sshKeyState(k), PermText: auth.PermToText(k.ScopePerm)})
	}
	d := accessData{
		base:       base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:     sr.tenant,
		Repo:       sr.repo,
		IsAdmin:    isGlobalAdmin(r),
		Grants:     grants,
		DeployKeys: rows,
	}
	if err := s.renderBuffered(w, "reposettings_access.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_access", http.StatusOK)
}

// accessGrant processes POST /settings/access/grant.
func (s *server) accessGrant(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if !s.postGuard(w, r) {
		return
	}
	userName := r.PostFormValue("username")
	perm := r.PostFormValue("perm")
	if userName == "" {
		EmitAdminActionMetric(r.Context(), s.logger, "repo_access", "grant", "invalid")
		s.redirectFlash(w, r, sr.accessBase(), "username required")
		return
	}
	if perm != "read" && perm != "write" && perm != "admin" {
		EmitAdminActionMetric(r.Context(), s.logger, "repo_access", "grant", "invalid")
		s.redirectFlash(w, r, sr.accessBase(), "permission must be read, write, or admin")
		return
	}
	if err := s.store.Grant(r.Context(), userName, sr.tenant, sr.repo, perm); err != nil {
		switch {
		case errors.Is(err, auth.ErrNoSuchUser):
			EmitAdminActionMetric(r.Context(), s.logger, "repo_access", "grant", "invalid")
			s.redirectFlash(w, r, sr.accessBase(), "no such user")
		case errors.Is(err, sqlitestore.ErrReservedUser):
			EmitAdminActionMetric(r.Context(), s.logger, "repo_access", "grant", "invalid")
			s.redirectFlash(w, r, sr.accessBase(), "cannot grant to reserved user")
		case errors.Is(err, auth.ErrNoSuchRepo):
			s.renderError(w, r, http.StatusNotFound, "not found")
		default:
			s.logger.Error("access: grant", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			EmitAdminActionMetric(r.Context(), s.logger, "repo_access", "grant", "error")
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
		}
		return
	}
	s.emitAdmin(r.Context(), "repo.grant.added",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("user", userName), slog.String("perm", perm))
	EmitAdminActionMetric(r.Context(), s.logger, "repo_access", "grant", "ok")
	s.redirectFlash(w, r, sr.accessBase(), "access granted")
}

// accessRevoke processes POST /settings/access/revoke.
func (s *server) accessRevoke(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if !s.postGuard(w, r) {
		return
	}
	userName := r.PostFormValue("username")
	if err := s.store.RevokeRepoPermission(r.Context(), userName, sr.tenant, sr.repo); err != nil {
		if errors.Is(err, auth.ErrNoSuchUser) {
			EmitAdminActionMetric(r.Context(), s.logger, "repo_access", "revoke", "invalid")
			s.redirectFlash(w, r, sr.accessBase(), "no such user")
			return
		}
		s.logger.Error("access: revoke", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "repo_access", "revoke", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "repo.grant.removed",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("user", userName))
	EmitAdminActionMetric(r.Context(), s.logger, "repo_access", "revoke", "ok")
	s.redirectFlash(w, r, sr.accessBase(), "access revoked")
}

// accessDeployKeyAdd processes POST /settings/access/deploykey/add. Deploy keys
// may be read or write only — admin is rejected server-side (M6 semantics).
func (s *server) accessDeployKeyAdd(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if !s.postGuard(w, r) {
		return
	}
	var perm auth.Perm
	switch r.PostFormValue("perm") {
	case "read":
		perm = auth.PermRead
	case "write":
		perm = auth.PermWrite
	default:
		EmitAdminActionMetric(r.Context(), s.logger, "deploykey", "add", "invalid")
		s.redirectFlash(w, r, sr.accessBase(), "deploy keys may be read or write only")
		return
	}
	k, err := auth.BuildDeploySSHKey([]byte(r.PostFormValue("pubkey")), sr.tenant, sr.repo, perm, r.PostFormValue("label"))
	if err != nil {
		EmitAdminActionMetric(r.Context(), s.logger, "deploykey", "add", "invalid")
		s.redirectFlash(w, r, sr.accessBase(), "could not parse public key")
		return
	}
	if err := s.store.AddSSHKey(r.Context(), k); err != nil {
		if errors.Is(err, auth.ErrDuplicateFingerprint) {
			EmitAdminActionMetric(r.Context(), s.logger, "deploykey", "add", "invalid")
			s.redirectFlash(w, r, sr.accessBase(), "key already registered")
			return
		}
		s.logger.Error("access: add deploy key", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "deploykey", "add", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.sshkey.added",
		slog.String("kind", "deploy"),
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("key_id", k.ID), slog.String("fingerprint", k.Fingerprint))
	EmitAdminActionMetric(r.Context(), s.logger, "deploykey", "add", "ok")
	s.redirectFlash(w, r, sr.accessBase(), "deploy key added")
}

// ownDeployKeyOr404 resolves the posted key id and verifies it belongs to this
// repo by listing the repo's deploy keys and doing a linear full-ID match.
// Uniform 404 hides both "no such key" and "not this repo's".
func (s *server) ownDeployKeyOr404(w http.ResponseWriter, r *http.Request, sr settingsRoute) (auth.SSHKey, bool) {
	id := r.PostFormValue("id")
	keys, err := s.store.ListSSHKeysForRepo(r.Context(), sr.tenant, sr.repo)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return auth.SSHKey{}, false
	}
	for _, k := range keys {
		if k.ID == id {
			return k, true
		}
	}
	s.renderError(w, r, http.StatusNotFound, "not found")
	return auth.SSHKey{}, false
}

// accessDeployKeyRevoke processes POST /settings/access/deploykey/revoke.
func (s *server) accessDeployKeyRevoke(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if !s.postGuard(w, r) {
		return
	}
	k, ok := s.ownDeployKeyOr404(w, r, sr)
	if !ok {
		return
	}
	if err := s.store.RevokeSSHKey(r.Context(), k.ID); err != nil {
		s.logger.Error("access: revoke deploy key", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "deploykey", "revoke", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.sshkey.revoked",
		slog.String("kind", "deploy"),
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("key_id", k.ID), slog.String("fingerprint", k.Fingerprint))
	EmitAdminActionMetric(r.Context(), s.logger, "deploykey", "revoke", "ok")
	s.redirectFlash(w, r, sr.accessBase(), "deploy key revoked")
}
