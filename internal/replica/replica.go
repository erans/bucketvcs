// Package replica implements the M26 read-replica freshness model (spec
// §26.2): a Gate consulted by ref-advertisement entry points, a lag-tracking
// Controller behind it, and the shared refusal plumbing for read-only
// gateways. See docs/superpowers/specs/2026-06-05-m26-multi-region-design.md.
package replica

import (
	"context"
	"fmt"
	"time"
)

// Mode is the replica freshness mode (spec §26.2).
type Mode int

const (
	// ModeStrongCurrent serves root manifests from the canonical bucket;
	// the gate never blocks on lag (lag is sampled for metrics only).
	ModeStrongCurrent Mode = iota
	// ModeBoundedStale serves the newest regionally-visible manifest while
	// replication lag stays within the configured budget.
	ModeBoundedStale
)

func (m Mode) String() string {
	if m == ModeBoundedStale {
		return "bounded-stale"
	}
	return "strong-current"
}

// ParseMode parses the --replica-mode flag value.
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "strong-current":
		return ModeStrongCurrent, true
	case "bounded-stale":
		return ModeBoundedStale, true
	}
	return 0, false
}

// Gate is consulted by ref-advertisement and upload-pack entry points
// (HTTPS and SSH). A non-nil error refuses the request; gateways map
// *UnhealthyError to 503.
type Gate interface {
	CheckAdvertise(ctx context.Context, tenant, repo string) error
}

// UnhealthyError reports a bounded-stale replica past its lag budget.
type UnhealthyError struct {
	Tenant, Repo string
	Lag          time.Duration // 0 when lag could not be determined
	Reason       string        // "lag budget exceeded" | "cannot determine replication lag"
}

func (e *UnhealthyError) Error() string {
	if e.Lag > 0 {
		return fmt.Sprintf("replica unhealthy for %s/%s: %s (lag %s)", e.Tenant, e.Repo, e.Reason, e.Lag.Round(time.Second))
	}
	return fmt.Sprintf("replica unhealthy for %s/%s: %s", e.Tenant, e.Repo, e.Reason)
}

// RefusalMessage is the operator-facing read-only message used by every
// write refusal surface (receive-pack HTTPS+SSH, LFS upload, LFS locks).
func RefusalMessage(writeRegionURL string) string {
	if writeRegionURL == "" {
		return "this gateway is a read-only replica"
	}
	return "this gateway is a read-only replica; push to " + writeRegionURL
}

// GatewayConfig is what serve hands the HTTP gateway and SSH server when
// running as a replica. Gate may be nil (gateways must tolerate it).
type GatewayConfig struct {
	WriteRegionURL string
	Gate           Gate
	// Health feeds GET /healthz/replica.
	Health func() HealthSnapshot
}

// HealthSnapshot is the /healthz/replica JSON payload.
type HealthSnapshot struct {
	Role               string  `json:"role"` // always "replica"
	Mode               string  `json:"mode"`
	ReposTracked       int     `json:"repos_tracked"`
	ReposLagging       int     `json:"repos_lagging"`
	MaxLagSeconds      float64 `json:"max_lag_seconds"`
	CanonicalReachable bool    `json:"canonical_reachable"`
}
