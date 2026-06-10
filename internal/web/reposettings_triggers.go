package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

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

// --- stubs replaced in later tasks (Tasks 10-12) ---

func (s *server) triggersNewForm(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	s.renderError(w, r, http.StatusNotFound, "not found")
}
func (s *server) triggersAdd(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	s.renderError(w, r, http.StatusNotFound, "not found")
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
