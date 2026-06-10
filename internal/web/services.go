// internal/web/services.go
package web

import (
	"context"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/buildtrigger"
	"github.com/bucketvcs/bucketvcs/internal/hooks"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
	"github.com/bucketvcs/bucketvcs/internal/policy"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// WebhookAdmin is the slice of *webhooks.Service the settings UI needs.
type WebhookAdmin interface {
	Create(ctx context.Context, in webhooks.EndpointInput) (webhooks.Endpoint, error)
	List(ctx context.Context, tenant, repo string) ([]webhooks.Endpoint, error)
	Remove(ctx context.Context, id int64) error
	Enable(ctx context.Context, id int64) error
	Disable(ctx context.Context, id int64) error
	RotateSecret(ctx context.Context, id int64) (string, error)
	ListDeliveries(ctx context.Context, f webhooks.ListDeliveriesFilter) ([]webhooks.Delivery, error)
	ShowDelivery(ctx context.Context, id string) (webhooks.Delivery, error)
	ReplayDelivery(ctx context.Context, id string) error
	Enqueue(ctx context.Context, event webhooks.Event, tenant, repo, actor string, payload any) error
}

// TriggerAdmin is the slice of *buildtrigger.Service the settings UI needs.
type TriggerAdmin interface {
	Create(ctx context.Context, in buildtrigger.TriggerInput) (buildtrigger.Trigger, error)
	List(ctx context.Context, tenant, repo string) ([]buildtrigger.Trigger, error)
	Get(ctx context.Context, id string) (buildtrigger.Trigger, error)
	Edit(ctx context.Context, id string, in buildtrigger.EditInput) (buildtrigger.Trigger, error)
	RotateSecret(ctx context.Context, id string) (string, error)
	Enable(ctx context.Context, id string) error
	Disable(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	ListDeliveriesPage(ctx context.Context, triggerID, status string, before time.Time, limit int) ([]buildtrigger.Delivery, error)
	RecentDeliveryIDs(ctx context.Context, triggerID string, n int) ([]string, error)
	GetDelivery(ctx context.Context, id string) (buildtrigger.Delivery, error)
	ReplayDelivery(ctx context.Context, id string) error
}

// ConnectorNames are the configured connector names (no secrets) surfaced in the
// trigger create/edit form's connector dropdowns. Empty when no --build-config.
type ConnectorNames struct {
	AWS   []string
	Azure []string
}

// PolicyAdmin is the slice of *policy.Service the settings UI needs (refs + paths).
type PolicyAdmin interface {
	Add(ctx context.Context, r policy.ProtectedRef) error
	List(ctx context.Context, tenant, repo string) ([]policy.ProtectedRef, error)
	Remove(ctx context.Context, tenant, repo, pattern string) error
	AddPathRule(ctx context.Context, in policy.ProtectedPath) error
	ListPathRules(ctx context.Context, tenant, repo string) ([]policy.ProtectedPath, error)
	RemovePathRule(ctx context.Context, tenant, repo, refnamePattern, pathPattern string) error
}

// HookAdmin is the slice of *hooks.Store the (global-admin-only) hooks tab needs.
type HookAdmin interface {
	Add(ctx context.Context, r hooks.Row) error
	List(ctx context.Context, tenant, repo, triggerFilter string) ([]hooks.Row, error)
	Remove(ctx context.Context, tenant, repo, trigger, scriptName string) error
	SetEnabled(ctx context.Context, tenant, repo, trigger, scriptName string, enabled bool, now time.Time) error
}

// QuotaAdmin is the slice of *quota.Service the admin UI needs. Reconcile is
// pre-bound to the ObjectStore in the composition root (QuotaReconciler).
type QuotaAdmin interface {
	Set(ctx context.Context, tenant string, limitBytes int64) error
	Get(ctx context.Context, tenant string) (quota.State, error)
	Clear(ctx context.Context, tenant string) error
	List(ctx context.Context) ([]quota.State, error)
}

// QuotaReconciler runs a storage-backed reconcile for one tenant.
type QuotaReconciler func(ctx context.Context, tenant string, dryRun bool) (quota.Report, error)

// RepoInitializer creates a repo's storage layout (in-process equivalent of
// `bucketvcs init`). Wired in serve.go as a closure over the ObjectStore.
type RepoInitializer func(ctx context.Context, tenant, repoName, actor string) error

// RepoRenameCheck probes the destination storage prefix for a web rename. It
// mirrors the M21 `bucketvcs repo rename` pre-check 3: M21 rename is auth-only
// (the auth.db row + dependent tables move atomically, but storage keys are NOT
// migrated — operators move tenants/<t>/repos/<old>/ → .../<new>/ out of band).
// The CLI refuses to rename when the destination prefix is non-empty to avoid a
// confused read against leftover/foreign objects; the web handler needs the same
// guard. Returns nil when the destination prefix is empty, and a descriptive
// error when it is non-empty OR the probe itself fails (fail-closed). Wired in
// serve.go as a closure over the ObjectStore.
type RepoRenameCheck func(ctx context.Context, tenant, newName string) error
