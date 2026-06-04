package web

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
)

type adminQuotasData struct {
	base
	Quotas       []quota.State
	Enabled      bool // s.quotas != nil
	CanReconcile bool // s.quotaReconcile != nil
}

// handleAdminQuotas renders GET /admin/quotas: per-tenant LFS quota list.
// Requires global admin. nil s.quotas renders a "not enabled" notice.
func (s *server) handleAdminQuotas(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess := SessionFromContext(r.Context())
	d := adminQuotasData{
		base:    base{Session: sess, CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Enabled: s.quotas != nil,
	}
	if s.quotas != nil {
		d.CanReconcile = s.quotaReconcile != nil
		states, err := s.quotas.List(r.Context())
		if err != nil {
			s.logger.Error("admin quotas: list", "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		d.Quotas = states
	}
	if err := s.renderBuffered(w, "admin_quotas.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "admin_quotas", http.StatusOK)
}

// handleAdminQuotaSet processes POST /admin/quotas/set.
// Sets the LFS quota for a tenant. Returns 404 when s.quotas is nil.
func (s *server) handleAdminQuotaSet(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.quotas == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	const dest = "/admin/quotas"
	fail := func(msg string) {
		EmitAdminActionMetric(r.Context(), s.logger, "admin_quotas", "set", "invalid")
		s.redirectFlash(w, r, dest, msg)
	}
	tenant := r.PostFormValue("tenant")
	if tenant == "" {
		fail("tenant required")
		return
	}
	limitStr := r.PostFormValue("limit")
	limitBytes, err := quota.ParseSize(limitStr)
	if err != nil {
		fail(fmt.Sprintf("invalid limit %q: %v", limitStr, err))
		return
	}
	if err := s.quotas.Set(r.Context(), tenant, limitBytes); err != nil {
		s.logger.Error("admin quotas: set", "tenant", tenant, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "admin_quotas", "set", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "quota.set",
		slog.String("tenant", tenant),
		slog.Int64("limit_bytes", limitBytes),
	)
	EmitAdminActionMetric(r.Context(), s.logger, "admin_quotas", "set", "ok")
	s.redirectFlash(w, r, dest, fmt.Sprintf("quota set for %s: %s", tenant, humanSize(limitBytes)))
}

// handleAdminQuotaClear processes POST /admin/quotas/clear.
// Removes the LFS quota row (back to unlimited). Returns 404 when s.quotas is nil.
func (s *server) handleAdminQuotaClear(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.quotas == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	const dest = "/admin/quotas"
	fail := func(msg string) {
		EmitAdminActionMetric(r.Context(), s.logger, "admin_quotas", "clear", "invalid")
		s.redirectFlash(w, r, dest, msg)
	}
	tenant := r.PostFormValue("tenant")
	if tenant == "" {
		fail("tenant required")
		return
	}
	if err := s.quotas.Clear(r.Context(), tenant); err != nil {
		s.logger.Error("admin quotas: clear", "tenant", tenant, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "admin_quotas", "clear", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "quota.cleared", slog.String("tenant", tenant))
	EmitAdminActionMetric(r.Context(), s.logger, "admin_quotas", "clear", "ok")
	s.redirectFlash(w, r, dest, "quota cleared for "+tenant+" (now unlimited)")
}

// handleAdminQuotaReconcile processes POST /admin/quotas/reconcile.
// Runs a storage-backed reconcile for a tenant. Reconcile errors are operator-retryable
// and surface as flash messages (never 500). Returns 404 when s.quotas is nil.
func (s *server) handleAdminQuotaReconcile(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.quotas == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	const dest = "/admin/quotas"
	tenant := r.PostFormValue("tenant")
	if tenant == "" {
		EmitAdminActionMetric(r.Context(), s.logger, "admin_quotas", "reconcile", "invalid")
		s.redirectFlash(w, r, dest, "tenant required")
		return
	}
	// nil reconciler: quotas is enabled but no storage handle was provided.
	if s.quotaReconcile == nil {
		EmitAdminActionMetric(r.Context(), s.logger, "admin_quotas", "reconcile", "invalid")
		s.redirectFlash(w, r, dest, "reconcile unavailable (no storage handle)")
		return
	}
	rep, err := s.quotaReconcile(r.Context(), tenant, false)
	if err != nil {
		s.logger.Warn("admin quotas: reconcile failed", "tenant", tenant, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "admin_quotas", "reconcile", "error")
		s.redirectFlash(w, r, dest, "reconcile failed: "+err.Error())
		return
	}
	s.emitAdmin(r.Context(), "quota.reconciled",
		slog.String("tenant", tenant),
		slog.Int64("drift_bytes", rep.DriftBytes),
	)
	EmitAdminActionMetric(r.Context(), s.logger, "admin_quotas", "reconcile", "ok")
	s.redirectFlash(w, r, dest, fmt.Sprintf("reconciled %s: drift %d bytes", tenant, rep.DriftBytes))
}
