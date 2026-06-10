package web

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/buildtrigger"
)

func (sr settingsRoute) triggersBase() string {
	return "/" + sr.tenant + "/" + sr.repo + "/settings/triggers"
}

func (sr settingsRoute) triggerDeliveriesURL(triggerID string) string {
	return fmt.Sprintf("/%s/%s/settings/triggers/deliveries?trigger=%s", sr.tenant, sr.repo, triggerID)
}

// triggerRow is the per-row view-model for the triggers table.
type triggerRow struct {
	buildtrigger.Trigger
	RefSummary  string
	LastFireOK  *bool
	LastFireRel string
	CanRotate   bool
}

// triggersData is the view-model for the triggers tab.
type triggersData struct {
	base
	Tenant, Repo string
	IsAdmin      bool
	Enabled      bool
	Triggers     []triggerRow
}

// triggerFormData is the view-model for the create/edit form (and its htmx
// kindfields fragment).
type triggerFormData struct {
	base
	Tenant, Repo string
	Kind         string
	Connectors   ConnectorNames
	Editing      bool
	Trigger      buildtrigger.Trigger
	HXFragment   bool
}

// repoSettingsTriggers is the action dispatcher for the TRIGGERS tab. The
// chassis (handleRepoSettings) has already authorized the actor.
func (s *server) repoSettingsTriggers(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	switch sr.action {
	case "":
		s.triggersPage(w, r, sr)
	case "new":
		s.triggersNewForm(w, r, sr)
	case "add":
		s.triggersAdd(w, r, sr)
	case "edit":
		s.triggersEdit(w, r, sr)
	case "enable":
		s.triggersSetActive(w, r, sr, true)
	case "disable":
		s.triggersSetActive(w, r, sr, false)
	case "remove":
		s.triggersRemove(w, r, sr)
	case "rotate-secret":
		s.triggersRotateSecret(w, r, sr)
	case "deliveries":
		s.triggersDeliveries(w, r, sr)
	case "replay":
		s.triggersReplay(w, r, sr)
	default:
		s.renderError(w, r, http.StatusNotFound, "not found")
	}
}

// triggersPage renders GET /{t}/{r}/settings/triggers.
func (s *server) triggersPage(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d := triggersData{
		base:    base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:  sr.tenant,
		Repo:    sr.repo,
		IsAdmin: isGlobalAdmin(r),
		Enabled: s.triggers != nil,
	}
	if s.triggers != nil {
		trs, err := s.triggers.List(r.Context(), sr.tenant, sr.repo)
		if err != nil {
			s.logger.Error("triggers: list", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		rows := make([]triggerRow, 0, len(trs))
		for _, tr := range trs {
			row := triggerRow{
				Trigger:    tr,
				RefSummary: refSummary(tr.RefInclude, tr.RefExclude),
				CanRotate:  tr.Kind == buildtrigger.KindGeneric || tr.Kind == buildtrigger.KindCloudBuild,
			}
			recent, derr := s.triggers.ListDeliveriesPage(r.Context(), tr.ID, "", time.Time{}, 1)
			if derr != nil {
				s.logger.Warn("triggers: last-fire probe", "trigger_id", tr.ID, "err", derr)
			} else if len(recent) == 1 {
				ok := recent[0].Status == "delivered"
				row.LastFireOK = &ok
				row.LastFireRel = relTimeAt(time.Now(), recent[0].CreatedAt.Unix())
			}
			rows = append(rows, row)
		}
		d.Triggers = rows
	}
	if err := s.renderBuffered(w, "reposettings_triggers.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_triggers", http.StatusOK)
}

// refSummary renders include/exclude ref globs as "+a +b -c" (or "(all)").
func refSummary(inc, exc []string) string {
	parts := make([]string, 0, len(inc)+len(exc))
	for _, p := range inc {
		parts = append(parts, "+"+p)
	}
	for _, p := range exc {
		parts = append(parts, "-"+p)
	}
	if len(parts) == 0 {
		return "(all)"
	}
	return strings.Join(parts, " ")
}

// --- stubs replaced in later tasks (Tasks 11-12) ---

// triggersNewForm renders GET .../settings/triggers/new[?kind=X][&id=Y]. It
// serves the full create/edit form page, OR — for an htmx kind-swap (HX-Request
// with a kind query and no id) — only the #kindfields fragment.
func (s *server) triggersNewForm(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind == "" {
		kind = string(buildtrigger.KindGeneric)
	}
	id := q.Get("id")
	d := triggerFormData{
		base:       base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:     sr.tenant,
		Repo:       sr.repo,
		Kind:       kind,
		Connectors: s.connectors,
		HXFragment: r.Header.Get("HX-Request") == "true",
	}
	if id != "" {
		tr, ok := s.ownTriggerOr404(w, r, sr, id)
		if !ok {
			return
		}
		d.Editing = true
		d.Trigger = tr
		d.Kind = string(tr.Kind)
	}
	// htmx kind-swap: re-render only the per-kind field block (no id, kind query
	// present). The connector dropdowns and per-kind inputs swap in place.
	if d.HXFragment && q.Get("kind") != "" && id == "" {
		var buf bytes.Buffer
		if err := s.render.renderPartial(&buf, "kindfields", d); err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "render error")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = buf.WriteTo(w)
		EmitRequestMetric(r.Context(), s.logger, "reposettings_triggers_form", http.StatusOK)
		return
	}
	if err := s.renderBuffered(w, "reposettings_triggers_form.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_triggers_form", http.StatusOK)
}

// ownTriggerOr404 fetches the trigger by id and confirms it belongs to this
// (tenant, repo). Any mismatch yields a uniform 404 (anti-enumeration). Shared
// by the form (edit pre-fill) and the Task 11/12 mutation handlers.
func (s *server) ownTriggerOr404(w http.ResponseWriter, r *http.Request, sr settingsRoute, id string) (buildtrigger.Trigger, bool) {
	if id == "" {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return buildtrigger.Trigger{}, false
	}
	tr, err := s.triggers.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, buildtrigger.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return buildtrigger.Trigger{}, false
		}
		s.logger.Error("triggers: get for ownership", "id", id, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return buildtrigger.Trigger{}, false
	}
	if tr.Tenant != sr.tenant || tr.Repo != sr.repo {
		s.renderError(w, r, http.StatusNotFound, "not found") // anti-enumeration
		return buildtrigger.Trigger{}, false
	}
	return tr, true
}

// splitCSVField splits an operator-supplied list field on commas, spaces,
// newlines, and tabs, trimming each item and dropping empties.
func splitCSVField(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ',', ' ', '\n', '\r', '\t':
			return true
		}
		return false
	})
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// parseTriggerInput builds a TriggerInput from the POST form. It returns a
// non-empty error string for client-correctable parse problems (bad
// pipeline-id, scopes, or ttl); the caller flashes that string.
func parseTriggerInput(r *http.Request, sr settingsRoute) (buildtrigger.TriggerInput, string) {
	in := buildtrigger.TriggerInput{
		Tenant: sr.tenant,
		Repo:   sr.repo,
		Name:   r.PostFormValue("name"),
		Kind:   buildtrigger.Kind(r.PostFormValue("kind")),
		Config: buildtrigger.Config{
			URL:             r.PostFormValue("url"),
			Secret:          r.PostFormValue("secret"),
			AWSRegion:       r.PostFormValue("aws_region"),
			AWSProject:      r.PostFormValue("aws_project"),
			AWSConnector:    r.PostFormValue("aws_connector"),
			AzureWebhookURL: r.PostFormValue("azure_webhook_url"),
			AzureSigHeader:  r.PostFormValue("azure_sig_header"),
			AzureConnector:  r.PostFormValue("azure_connector"),
			AzureProject:    r.PostFormValue("azure_project"),
		},
		RefInclude: splitCSVField(r.PostFormValue("ref_include")),
		RefExclude: splitCSVField(r.PostFormValue("ref_exclude")),
		TokenMode:  buildtrigger.TokenMode(r.PostFormValue("token_mode")),
	}
	if raw := strings.TrimSpace(r.PostFormValue("azure_pipeline_id")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return buildtrigger.TriggerInput{}, "invalid azure pipeline id: must be an integer"
		}
		in.Config.AzurePipelineID = n
	}
	if raw := strings.TrimSpace(r.PostFormValue("token_scopes")); raw != "" {
		sc, err := auth.ParseScopes(raw)
		if err != nil {
			return buildtrigger.TriggerInput{}, "invalid token scopes: " + err.Error()
		}
		in.TokenScopes = sc
	}
	if raw := strings.TrimSpace(r.PostFormValue("token_ttl")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return buildtrigger.TriggerInput{}, "invalid token ttl: " + err.Error()
		}
		in.TokenTTL = d
	}
	return in, ""
}

// triggersAdd processes POST .../settings/triggers/add.
func (s *server) triggersAdd(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	in, errstr := parseTriggerInput(r, sr)
	if errstr != "" {
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "add", "invalid")
		s.redirectFlash(w, r, sr.triggersBase(), errstr)
		return
	}
	tr, err := s.triggers.Create(r.Context(), in)
	if err != nil {
		if errors.Is(err, buildtrigger.ErrConflict) {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "add", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "a trigger with that name already exists")
			return
		}
		if errors.Is(err, buildtrigger.ErrInvalidInput) {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "add", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "invalid trigger: "+strings.TrimPrefix(err.Error(), "buildtrigger: "))
			return
		}
		s.logger.Error("triggers: create", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "add", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "buildtrigger.created",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("trigger_id", tr.ID), slog.String("kind", string(tr.Kind)))
	EmitAdminActionMetric(r.Context(), s.logger, "trigger", "add", "ok")
	if tr.Secret != "" {
		s.renderSecretOnce(w, r, "build trigger created", tr.Secret, sr.triggersBase())
		return
	}
	s.redirectFlash(w, r, sr.triggersBase(), "trigger created")
}
func (s *server) triggersEdit(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	s.renderError(w, r, http.StatusNotFound, "not found")
}
func (s *server) triggersSetActive(w http.ResponseWriter, r *http.Request, sr settingsRoute, active bool) {
	s.renderError(w, r, http.StatusNotFound, "not found")
}
func (s *server) triggersRemove(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	s.renderError(w, r, http.StatusNotFound, "not found")
}
func (s *server) triggersRotateSecret(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	s.renderError(w, r, http.StatusNotFound, "not found")
}
func (s *server) triggersDeliveries(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	s.renderError(w, r, http.StatusNotFound, "not found")
}
func (s *server) triggersReplay(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	s.renderError(w, r, http.StatusNotFound, "not found")
}
