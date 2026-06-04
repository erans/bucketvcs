package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// webhooksBase returns the canonical redirect target for the webhooks tab.
func (sr settingsRoute) webhooksBase() string {
	return "/" + sr.tenant + "/" + sr.repo + "/settings/webhooks"
}

// deliveriesURL builds the deliveries sub-page URL for a given endpoint id.
func (sr settingsRoute) deliveriesURL(endpointID int64) string {
	return fmt.Sprintf("/%s/%s/settings/webhooks/deliveries?endpoint=%d", sr.tenant, sr.repo, endpointID)
}

// endpointRow is the per-row view-model for the endpoints table.
type endpointRow struct {
	webhooks.Endpoint
	EventsStr  string // FormatEvents output for display
	CreatedRel string // reltime string
}

// deliveryRow is the per-row view-model for the deliveries table.
type deliveryRow struct {
	webhooks.Delivery
	CreatedRel     string
	LastErrorTrunc string // server-side truncated to ≤80 runes
}

// webhooksData is the view-model for the webhooks tab.
type webhooksData struct {
	base
	Tenant, Repo string
	IsAdmin      bool
	Enabled      bool // false when s.webhooks==nil
	Endpoints    []endpointRow
}

// deliveriesData is the view-model for the deliveries sub-page.
type deliveriesData struct {
	base
	Tenant, Repo string
	IsAdmin      bool
	Endpoint     webhooks.Endpoint
	Deliveries   []deliveryRow
}

// repoSettingsWebhooks is the action dispatcher for the WEBHOOKS tab.
// The chassis (handleRepoSettings) has already authorized the actor.
func (s *server) repoSettingsWebhooks(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	switch sr.action {
	case "":
		s.webhooksPage(w, r, sr)
	case "add":
		s.webhooksAdd(w, r, sr)
	case "enable":
		s.webhooksEnable(w, r, sr)
	case "disable":
		s.webhooksDisable(w, r, sr)
	case "remove":
		s.webhooksRemove(w, r, sr)
	case "rotate":
		s.webhooksRotate(w, r, sr)
	case "deliveries":
		s.webhooksDeliveries(w, r, sr)
	case "replay":
		s.webhooksReplay(w, r, sr)
	default:
		s.renderError(w, r, http.StatusNotFound, "not found")
	}
}

// webhooksPage renders GET /{t}/{r}/settings/webhooks.
func (s *server) webhooksPage(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d := webhooksData{
		base:    base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:  sr.tenant,
		Repo:    sr.repo,
		IsAdmin: isGlobalAdmin(r),
		Enabled: s.webhooks != nil,
	}
	if s.webhooks != nil {
		eps, err := s.webhooks.List(r.Context(), sr.tenant, sr.repo)
		if err != nil {
			s.logger.Error("webhooks: list endpoints", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		rows := make([]endpointRow, 0, len(eps))
		for _, ep := range eps {
			rows = append(rows, endpointRow{
				Endpoint:   ep,
				EventsStr:  webhooks.FormatEvents(ep.EventMask),
				CreatedRel: relTimeAt(time.Now(), ep.CreatedAt.Unix()),
			})
		}
		d.Endpoints = rows
	}
	if err := s.renderBuffered(w, "reposettings_webhooks.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_webhooks", http.StatusOK)
}

// webhooksAdd processes POST /{t}/{r}/settings/webhooks/add.
func (s *server) webhooksAdd(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.webhooks == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	rawURL := r.PostFormValue("url")
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_add", "invalid")
		s.redirectFlash(w, r, sr.webhooksBase(), "invalid url: must start with http:// or https://")
		return
	}
	rawEvents := r.PostFormValue("events")
	mask, err := webhooks.ParseEvents(rawEvents)
	if err != nil {
		EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_add", "invalid")
		s.redirectFlash(w, r, sr.webhooksBase(), "invalid events: "+err.Error())
		return
	}
	ep, err := s.webhooks.Create(r.Context(), webhooks.EndpointInput{
		Tenant:    sr.tenant,
		Repo:      sr.repo,
		URL:       rawURL,
		EventMask: mask,
	})
	if err != nil {
		if errors.Is(err, webhooks.ErrConflict) {
			EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_add", "invalid")
			s.redirectFlash(w, r, sr.webhooksBase(), "endpoint url already registered")
			return
		}
		s.logger.Error("webhooks: create endpoint", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_add", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "webhooks.endpoint_created",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.Int64("endpoint_id", ep.ID), slog.String("url", ep.URL))
	EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_add", "ok")
	s.renderSecretOnce(w, r, "webhook endpoint created", ep.Secret, sr.webhooksBase())
}

// ownEndpointOr404 reads the endpoint id from the given form field, lists the
// repo's endpoints, and returns the matching one. Any mismatch (id not int64,
// endpoint not in this repo) yields a uniform 404 without calling the service.
// Anti-enumeration: identical response for "no such endpoint" and "not yours".
func (s *server) ownEndpointOr404(w http.ResponseWriter, r *http.Request, sr settingsRoute, field string) (webhooks.Endpoint, bool) {
	rawID := r.PostFormValue(field)
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return webhooks.Endpoint{}, false
	}
	// anti-enumeration: always list this repo's endpoints before acting
	eps, err := s.webhooks.List(r.Context(), sr.tenant, sr.repo)
	if err != nil {
		s.logger.Error("webhooks: list for ownership check", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return webhooks.Endpoint{}, false
	}
	for _, ep := range eps {
		if ep.ID == id {
			return ep, true
		}
	}
	// id not found in this repo (anti-enumeration)
	s.renderError(w, r, http.StatusNotFound, "not found")
	return webhooks.Endpoint{}, false
}

// webhooksEnable processes POST /{t}/{r}/settings/webhooks/enable.
func (s *server) webhooksEnable(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.webhooks == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	ep, ok := s.ownEndpointOr404(w, r, sr, "id")
	if !ok {
		return
	}
	if err := s.webhooks.Enable(r.Context(), ep.ID); err != nil {
		if errors.Is(err, webhooks.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("webhooks: enable", "id", ep.ID, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_enable", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "webhooks.endpoint_enabled",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.Int64("endpoint_id", ep.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_enable", "ok")
	s.redirectFlash(w, r, sr.webhooksBase(), "endpoint enabled")
}

// webhooksDisable processes POST /{t}/{r}/settings/webhooks/disable.
func (s *server) webhooksDisable(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.webhooks == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	ep, ok := s.ownEndpointOr404(w, r, sr, "id")
	if !ok {
		return
	}
	if err := s.webhooks.Disable(r.Context(), ep.ID); err != nil {
		if errors.Is(err, webhooks.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("webhooks: disable", "id", ep.ID, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_disable", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "webhooks.endpoint_disabled",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.Int64("endpoint_id", ep.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_disable", "ok")
	s.redirectFlash(w, r, sr.webhooksBase(), "endpoint disabled")
}

// webhooksRemove processes POST /{t}/{r}/settings/webhooks/remove.
func (s *server) webhooksRemove(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.webhooks == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	ep, ok := s.ownEndpointOr404(w, r, sr, "id")
	if !ok {
		return
	}
	if err := s.webhooks.Remove(r.Context(), ep.ID); err != nil {
		if errors.Is(err, webhooks.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("webhooks: remove", "id", ep.ID, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_remove", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "webhooks.endpoint_removed",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.Int64("endpoint_id", ep.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_remove", "ok")
	s.redirectFlash(w, r, sr.webhooksBase(), "endpoint removed")
}

// webhooksRotate processes POST /{t}/{r}/settings/webhooks/rotate.
func (s *server) webhooksRotate(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.webhooks == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	ep, ok := s.ownEndpointOr404(w, r, sr, "id")
	if !ok {
		return
	}
	secret, err := s.webhooks.RotateSecret(r.Context(), ep.ID)
	if err != nil {
		if errors.Is(err, webhooks.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("webhooks: rotate secret", "id", ep.ID, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_rotate", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "webhooks.endpoint_secret_rotated",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.Int64("endpoint_id", ep.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_rotate", "ok")
	s.renderSecretOnce(w, r, "webhook secret rotated", secret, sr.webhooksBase())
}

// webhooksDeliveries renders GET /{t}/{r}/settings/webhooks/deliveries?endpoint=<id>.
func (s *server) webhooksDeliveries(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.webhooks == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Parse and ownership-check the endpoint id from the query string.
	rawID := r.URL.Query().Get("endpoint")
	epID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	eps, err := s.webhooks.List(r.Context(), sr.tenant, sr.repo)
	if err != nil {
		s.logger.Error("webhooks: list for deliveries check", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	var ep webhooks.Endpoint
	found := false
	for _, e := range eps {
		if e.ID == epID {
			ep = e
			found = true
			break
		}
	}
	if !found {
		// anti-enumeration: same 404 whether id doesn't exist or isn't ours
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	deliveries, err := s.webhooks.ListDeliveries(r.Context(), webhooks.ListDeliveriesFilter{
		EndpointID: ep.ID,
		Limit:      50,
	})
	if err != nil {
		s.logger.Error("webhooks: list deliveries", "endpoint_id", ep.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	rows := make([]deliveryRow, 0, len(deliveries))
	for _, d := range deliveries {
		rows = append(rows, deliveryRow{
			Delivery:       d,
			CreatedRel:     relTimeAt(time.Now(), d.CreatedAt.Unix()),
			LastErrorTrunc: truncateRunes(d.LastError, 80),
		})
	}
	data := deliveriesData{
		base:       base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:     sr.tenant,
		Repo:       sr.repo,
		IsAdmin:    isGlobalAdmin(r),
		Endpoint:   ep,
		Deliveries: rows,
	}
	if err := s.renderBuffered(w, "reposettings_deliveries.html", data); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_deliveries", http.StatusOK)
}

// webhooksReplay processes POST /{t}/{r}/settings/webhooks/replay.
func (s *server) webhooksReplay(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.webhooks == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	deliveryID := r.PostFormValue("delivery_id")
	if deliveryID == "" {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	// Two-step ownership check, both required:
	//  1. the posted endpoint_id must belong to this (tenant, repo), and
	//  2. the posted delivery_id must actually belong to that endpoint.
	// Step 1 alone is insufficient: a repo-admin could otherwise replay ANY
	// delivery in the system by pairing their own endpoint_id with a foreign
	// delivery_id (ReplayDelivery checks only status, not ownership).
	eps, err := s.webhooks.List(r.Context(), sr.tenant, sr.repo)
	if err != nil {
		s.logger.Error("webhooks: list for replay check", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	rawEPID := r.PostFormValue("endpoint_id")
	epID, err := strconv.ParseInt(rawEPID, 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	var ep webhooks.Endpoint
	found := false
	for _, e := range eps {
		if e.ID == epID {
			ep = e
			found = true
			break
		}
	}
	if !found {
		// anti-enumeration
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	// Step 2: bind the delivery to the owned endpoint. A 404 here hides
	// no-such-delivery and foreign-delivery (cross-repo/tenant) alike.
	d, err := s.webhooks.ShowDelivery(r.Context(), deliveryID)
	if err != nil || d.EndpointID != ep.ID {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	backURL := sr.deliveriesURL(ep.ID)
	if err := s.webhooks.ReplayDelivery(r.Context(), deliveryID); err != nil {
		if errors.Is(err, webhooks.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		EmitAdminActionMetric(r.Context(), s.logger, "webhook", "delivery_replay", "error")
		if errors.Is(err, webhooks.ErrReplayInFlight) {
			// Benign: worker is mid-delivery; tell the operator to wait.
			s.redirectFlash(w, r, backURL, err.Error())
			return
		}
		// Unknown/DB error: log internally, show generic message.
		s.logger.Error("webhooks: replay delivery", "delivery_id", deliveryID, "err", err)
		s.redirectFlash(w, r, backURL, "replay failed (internal error); see server log")
		return
	}
	s.emitAdmin(r.Context(), "webhooks.delivery_replayed",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("delivery_id", deliveryID), slog.Int64("endpoint_id", ep.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "webhook", "delivery_replay", "ok")
	s.redirectFlash(w, r, backURL, "delivery queued for replay")
}

// truncateRunes truncates s to at most n runes, appending "…" if truncated.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "…"
}
