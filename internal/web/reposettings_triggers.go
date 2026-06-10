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

// triggersEdit processes POST .../settings/triggers/edit. Kind and Config are
// immutable on edit (change kind via delete+recreate).
func (s *server) triggersEdit(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	tr, ok := s.ownTriggerOr404(w, r, sr, r.PostFormValue("id"))
	if !ok {
		return
	}
	in := buildtrigger.EditInput{
		Name:       r.PostFormValue("name"),
		RefInclude: splitCSVField(r.PostFormValue("ref_include")),
		RefExclude: splitCSVField(r.PostFormValue("ref_exclude")),
		TokenMode:  buildtrigger.TokenMode(r.PostFormValue("token_mode")),
		Active:     r.PostFormValue("active") == "on",
	}
	if raw := strings.TrimSpace(r.PostFormValue("token_scopes")); raw != "" {
		sc, err := auth.ParseScopes(raw)
		if err != nil {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "invalid token scopes: "+err.Error())
			return
		}
		in.TokenScopes = sc
	}
	if raw := strings.TrimSpace(r.PostFormValue("token_ttl")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "invalid token ttl: "+err.Error())
			return
		}
		in.TokenTTL = d
	}
	if _, err := s.triggers.Edit(r.Context(), tr.ID, in); err != nil {
		if errors.Is(err, buildtrigger.ErrConflict) {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "a trigger with that name already exists")
			return
		}
		if errors.Is(err, buildtrigger.ErrInvalidInput) {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "invalid trigger: "+strings.TrimPrefix(err.Error(), "buildtrigger: "))
			return
		}
		s.logger.Error("triggers: edit", "tenant", sr.tenant, "repo", sr.repo, "trigger_id", tr.ID, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "buildtrigger.edited",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("trigger_id", tr.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "ok")
	s.redirectFlash(w, r, sr.triggersBase(), "trigger updated")
}

// triggersSetActive processes POST .../settings/triggers/{enable,disable}.
func (s *server) triggersSetActive(w http.ResponseWriter, r *http.Request, sr settingsRoute, active bool) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	tr, ok := s.ownTriggerOr404(w, r, sr, r.PostFormValue("id"))
	if !ok {
		return
	}
	action := "disable"
	verb := s.triggers.Disable
	if active {
		action = "enable"
		verb = s.triggers.Enable
	}
	if err := verb(r.Context(), tr.ID); err != nil {
		if errors.Is(err, buildtrigger.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("triggers: "+action, "tenant", sr.tenant, "repo", sr.repo, "trigger_id", tr.ID, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", action, "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "buildtrigger."+action+"d",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("trigger_id", tr.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "trigger", action, "ok")
	s.redirectFlash(w, r, sr.triggersBase(), "trigger "+action+"d")
}

// triggersRemove processes POST .../settings/triggers/remove.
func (s *server) triggersRemove(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	tr, ok := s.ownTriggerOr404(w, r, sr, r.PostFormValue("id"))
	if !ok {
		return
	}
	if err := s.triggers.Remove(r.Context(), tr.ID); err != nil {
		if errors.Is(err, buildtrigger.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("triggers: remove", "tenant", sr.tenant, "repo", sr.repo, "trigger_id", tr.ID, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "remove", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "buildtrigger.removed",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("trigger_id", tr.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "trigger", "remove", "ok")
	s.redirectFlash(w, r, sr.triggersBase(), "trigger removed")
}

// triggersRotateSecret processes POST .../settings/triggers/rotate-secret.
func (s *server) triggersRotateSecret(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	tr, ok := s.ownTriggerOr404(w, r, sr, r.PostFormValue("id"))
	if !ok {
		return
	}
	secret, err := s.triggers.RotateSecret(r.Context(), tr.ID)
	if err != nil {
		if errors.Is(err, buildtrigger.ErrInvalidInput) {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "rotate_secret", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "this trigger kind has no rotatable secret")
			return
		}
		if errors.Is(err, buildtrigger.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("triggers: rotate secret", "tenant", sr.tenant, "repo", sr.repo, "trigger_id", tr.ID, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "rotate_secret", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "buildtrigger.secret_rotated",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("trigger_id", tr.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "trigger", "rotate_secret", "ok")
	s.renderSecretOnce(w, r, "build trigger secret rotated", secret, sr.triggersBase())
}

const (
	triggerDeliveriesPageSize = 20
	triggerReplayWindow       = 10
)

// triggerDeliveryRow is the per-row view-model for the deliveries table.
type triggerDeliveryRow struct {
	buildtrigger.Delivery
	CreatedRel     string
	LastErrorTrunc string // server-side truncated to ≤80 runes
	Replayable     bool   // id is within the recent replay window
}

// triggerDeliveriesData is the view-model for the deliveries sub-page.
type triggerDeliveriesData struct {
	base
	Tenant, Repo string
	IsAdmin      bool
	Trigger      buildtrigger.Trigger
	Status       string // active status filter ("" = all)
	Deliveries   []triggerDeliveryRow
	NextCursor   string // unix-seconds cursor for the [older] pager link ("" = no more)
}

// triggersDeliveries renders GET .../settings/triggers/deliveries?trigger=<id>[&status=][&before=].
func (s *server) triggersDeliveries(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tr, ok := s.ownTriggerOr404(w, r, sr, r.URL.Query().Get("trigger"))
	if !ok {
		return
	}
	// status filter: allow only the known terminal/in-progress states; an
	// unrecognized value falls back to "" (all) rather than 400.
	status := r.URL.Query().Get("status")
	switch status {
	case "", "pending", "in_flight", "delivered", "dead_letter":
	default:
		status = ""
	}
	// before cursor: unix seconds → time; parse errors yield the zero time
	// (which ListDeliveriesPage treats as "from the newest").
	var before time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		if sec, err := strconv.ParseInt(raw, 10, 64); err == nil {
			before = time.Unix(sec, 0)
		}
	}
	ctx := r.Context()
	ds, err := s.triggers.ListDeliveriesPage(ctx, tr.ID, status, before, triggerDeliveriesPageSize)
	if err != nil {
		s.logger.Error("triggers: list deliveries", "trigger_id", tr.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	recent, err := s.triggers.RecentDeliveryIDs(ctx, tr.ID, triggerReplayWindow)
	if err != nil {
		s.logger.Error("triggers: recent delivery ids", "trigger_id", tr.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	recentSet := make(map[string]struct{}, len(recent))
	for _, id := range recent {
		recentSet[id] = struct{}{}
	}
	now := time.Now()
	rows := make([]triggerDeliveryRow, 0, len(ds))
	for _, d := range ds {
		_, replayable := recentSet[d.ID]
		rows = append(rows, triggerDeliveryRow{
			Delivery:       d,
			CreatedRel:     relTimeAt(now, d.CreatedAt.Unix()),
			LastErrorTrunc: truncateRunes(d.LastError, 80),
			Replayable:     replayable,
		})
	}
	nextCursor := ""
	if len(ds) == triggerDeliveriesPageSize {
		nextCursor = strconv.FormatInt(ds[len(ds)-1].CreatedAt.Unix(), 10)
	}
	data := triggerDeliveriesData{
		base:       base{Session: SessionFromContext(ctx), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:     sr.tenant,
		Repo:       sr.repo,
		IsAdmin:    isGlobalAdmin(r),
		Trigger:    tr,
		Status:     status,
		Deliveries: rows,
		NextCursor: nextCursor,
	}
	if err := s.renderBuffered(w, "reposettings_triggers_deliveries.html", data); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(ctx, s.logger, "reposettings_triggers_deliveries", http.StatusOK)
}

// triggersReplay processes POST .../settings/triggers/replay. Replay is bounded
// to the most-recent triggerReplayWindow deliveries, enforced SERVER-SIDE here
// (the template merely hides the button; a hand-crafted POST for an
// out-of-window delivery is rejected). Two ownership gates apply: the posted
// trigger_id must belong to this (tenant, repo), and the posted delivery must
// belong to that trigger — a foreign delivery is indistinguishable from missing.
func (s *server) triggersReplay(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	tr, ok := s.ownTriggerOr404(w, r, sr, r.PostFormValue("trigger_id"))
	if !ok {
		return
	}
	deliveryID := r.PostFormValue("delivery_id")
	if deliveryID == "" {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	ctx := r.Context()
	// Cross-trigger guard: bind the delivery to the owned trigger. A 404 here
	// hides no-such-delivery and foreign-delivery (cross-trigger/repo/tenant)
	// alike, so ReplayDelivery (which checks only status, not ownership) can
	// never be reached for a delivery the actor does not own.
	d, err := s.triggers.GetDelivery(ctx, deliveryID)
	if err != nil || d.TriggerID != tr.ID {
		if err != nil && !errors.Is(err, buildtrigger.ErrNotFound) {
			s.logger.Error("triggers replay: get delivery", "delivery_id", deliveryID, "err", err)
		}
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	backURL := sr.triggerDeliveriesURL(tr.ID)
	// Authoritative bounded-window check (server-side; the button-hiding in the
	// template is advisory only).
	recent, err := s.triggers.RecentDeliveryIDs(ctx, tr.ID, triggerReplayWindow)
	if err != nil {
		s.logger.Error("triggers replay: recent delivery ids", "trigger_id", tr.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	inWindow := false
	for _, id := range recent {
		if id == deliveryID {
			inWindow = true
			break
		}
	}
	if !inWindow {
		EmitAdminActionMetric(ctx, s.logger, "trigger", "delivery_replay", "invalid")
		s.redirectFlash(w, r, backURL, "only the 10 most recent deliveries can be replayed")
		return
	}
	if err := s.triggers.ReplayDelivery(ctx, deliveryID); err != nil {
		if errors.Is(err, buildtrigger.ErrReplayInFlight) {
			EmitAdminActionMetric(ctx, s.logger, "trigger", "delivery_replay", "invalid")
			s.redirectFlash(w, r, backURL, "delivery attempt in progress; try again shortly")
			return
		}
		if errors.Is(err, buildtrigger.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("triggers: replay delivery", "delivery_id", deliveryID, "err", err)
		EmitAdminActionMetric(ctx, s.logger, "trigger", "delivery_replay", "error")
		s.redirectFlash(w, r, backURL, "replay failed (internal error); see server log")
		return
	}
	s.emitAdmin(ctx, "buildtrigger.delivery_replayed",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("trigger_id", tr.ID), slog.String("delivery_id", deliveryID))
	EmitAdminActionMetric(ctx, s.logger, "trigger", "delivery_replay", "ok")
	s.redirectFlash(w, r, backURL, "delivery queued for replay")
}
