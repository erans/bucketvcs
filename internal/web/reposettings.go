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
	// Repo-existence probe (roborev round 16): canAdminRepo short-circuits for
	// global admins without touching the repos table, so a missing repo would
	// otherwise render empty webhooks/policy/hooks tabs while general/access
	// 404'd — one probe here makes every tab uniform.
	if _, err := s.store.GetRepoFlags(r.Context(), sr.tenant, sr.repo); err != nil {
		if !errors.Is(err, auth.ErrNoSuchRepo) {
			s.logger.Error("repo settings: existence probe", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		if s.aliasRedirect(w, r, sr.tenant, sr.repo) {
			return
		}
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
		s.repoSettingsHooks(w, r, sr)
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
// (POST /settings/rename). This is global-admin-only — repo-admins get a
// uniform 404 BEFORE the CSRF check. Rename is an operator procedure: M21
// rename is auth-only (the auth.db row + dependent tables move atomically, but
// storage keys are NOT migrated). The operator moves
// tenants/<t>/repos/<old>/ → .../<new>/ out of band; a repo-admin cannot
// complete that, so the web surface is restricted to global admin like delete.
// Before the rename we run the SAME destination-prefix collision probe the CLI
// uses (renameCheck) so a web rename can never point a name at leftover/foreign
// objects (e.g. delete-no-purge then rename-into-that-name).
func (s *server) repoSettingsRename(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if !isGlobalAdmin(r) {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	newName := r.PostFormValue("newname")
	if !routenames.ValidateName(newName) {
		EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "invalid")
		s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings", "invalid repo name")
		return
	}
	if newName == sr.repo {
		// Benign no-op: renaming to the same name. Short-circuit before the
		// store call (RenameRepo would return ErrRepoExists/plain error and
		// fall through to a 500 on otherwise-valid input).
		EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "invalid")
		s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings", "name unchanged")
		return
	}
	// No storage handle → the collision probe is impossible; refuse rather than
	// silently skipping the CLI safeguard (mirrors RepoInit's nil handling).
	if s.renameCheck == nil {
		EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "invalid")
		s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings",
			"rename unavailable (no storage handle)")
		return
	}
	// Destination-prefix collision probe (CLI pre-check 3). renameCheck returns
	// an error when the destination prefix is non-empty OR the probe failed.
	// Per the round-4 masking convention the detail is logged, not flashed.
	if err := s.renameCheck(r.Context(), sr.tenant, newName); err != nil {
		s.logger.Warn("repo rename: storage collision probe", "tenant", sr.tenant, "newname", newName, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "invalid")
		s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings",
			"destination storage prefix not empty (or probe failed); refusing rename")
		return
	}
	// Auth-row pre-check: the repo.renamed webhook must be enqueued BEFORE
	// RenameRepo (Enqueue resolves endpoints by the old name), so a rename
	// that would fail on a name conflict would otherwise emit a spurious
	// event. Probing the destination row first closes the user-triggerable
	// conflict path; the residual TOCTOU window matches the CLI's tradeoff.
	if _, err := s.store.GetRepoFlags(r.Context(), sr.tenant, newName); err == nil {
		EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "invalid")
		s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings", "name already taken")
		return
	}
	sess := SessionFromContext(r.Context())
	actor := ""
	if sess != nil {
		actor = sess.Name
	}
	// Enqueue the repo.renamed webhook BEFORE RenameRepo runs, against the OLD
	// name. RenameRepo propagates the new name into the webhook_endpoints rows;
	// Enqueue resolves subscribers via a (tenant, repo) SELECT, so enqueuing
	// after the rename would match ZERO endpoints (the rows now carry newName)
	// and the delivery would be silently dropped. Mirrors the M21 CLI rename and
	// the repo.deleted ordering below. Fail-open: an enqueue error must not block
	// the rename; the accepted tradeoff is that a rename failing after enqueue
	// sends a spurious event (same as the delete path).
	if s.webhooks != nil {
		payload := webhooks.RepoRenamedPayload{OldName: sr.repo, NewName: newName}
		if err := s.webhooks.Enqueue(r.Context(), webhooks.EventRepoRenamed, sr.tenant, sr.repo, actor, payload); err != nil {
			s.logger.Warn("webhooks.enqueue_failed", "event", "repo.renamed", "err", err)
		}
	}
	if err := s.store.RenameRepo(r.Context(), sr.tenant, sr.repo, newName); err != nil {
		switch {
		case errors.Is(err, auth.ErrNoSuchRepo):
			s.renderError(w, r, http.StatusNotFound, "not found")
		case errors.Is(err, sqlitestore.ErrRepoExists):
			// "invalid" keeps the result enum closed (ok|invalid|error) —
			// a name collision is user input, like the pre-check flash.
			EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "invalid")
			s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings", "name already taken")
		default:
			s.logger.Error("repo rename", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "error")
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
		}
		return
	}
	s.emitAdmin(r.Context(), "repo.renamed",
		slog.String("tenant", sr.tenant), slog.String("from", sr.repo), slog.String("to", newName))
	EmitAdminActionMetric(r.Context(), s.logger, "repo", "rename", "ok")
	s.redirectFlash(w, r, "/"+sr.tenant+"/"+newName+"/settings",
		"repo renamed; storage keys NOT migrated — move tenants/"+sr.tenant+"/repos/"+sr.repo+"/ to .../"+newName+"/ out of band (see operator guide)")
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
			// Reserved for future backends that can't preserve the M15.1 drain
			// design (every shipped backend supports the cascade as of M25).
			// Flash rather than 500 — operator-environment limitation, not a
			// server fault.
			EmitAdminActionMetric(r.Context(), s.logger, "repo", "delete", "error")
			s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings",
				"repo delete is not supported on this server's database backend")
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
