package replica

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ControllerConfig parameterizes the freshness controller.
type ControllerConfig struct {
	Mode          Mode
	LagBudget     time.Duration // bounded-stale staleness budget
	CheckInterval time.Duration // canonical poll TTL (serve defaults: budget/4, floor 15s)
	Regional      storage.ObjectStore
	Canonical     storage.ObjectStore
	Logger        *slog.Logger     // nil → slog.Default()
	Now           func() time.Time // nil → time.Now (tests inject)
}

// repoState is the per-repo freshness record. All times come from cfg.Now.
type repoState struct {
	regionalVer   uint64
	canonicalVer  uint64
	lastCheck     time.Time // last refresh attempt (TTL anchor)
	lastGoodCheck time.Time // last refresh whose canonical read succeeded
	laggedSince   time.Time // zero when not lagging
	unhealthy     bool      // last reported state (for transition audits)
}

// Controller tracks per-repo replication lag lazily — only repos that are
// actually fetched get sampled; there is no O(all-repos) polling loop.
// Safe for concurrent use.
type Controller struct {
	mu  sync.Mutex
	cfg ControllerConfig // Canonical swappable via SetCanonicalForTest

	repos map[string]*repoState
}

// NewController builds the controller. Callers ensure LagBudget/CheckInterval
// are sane (serve validates budget >= 30s, interval floor 15s).
func NewController(cfg ControllerConfig) *Controller {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Controller{cfg: cfg, repos: map[string]*repoState{}}
}

// SetCanonicalForTest swaps the canonical store (outage simulation).
// Canonical is the ONLY mutable cfg field; every other ControllerConfig
// field is immutable after NewController and may be read without the lock.
func (c *Controller) SetCanonicalForTest(s storage.ObjectStore) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.Canonical = s
}

// SetRegionalForTest swaps the regional store (outage simulation).
// Test-only; mirrors SetCanonicalForTest.
func (c *Controller) SetRegionalForTest(s storage.ObjectStore) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.Regional = s
}

// readVersion fetches a root manifest header's ManifestVersion from one store.
func readVersion(ctx context.Context, s storage.ObjectStore, tenant, repo string) (uint64, error) {
	rk, err := keys.NewRepo(tenant, repo)
	if err != nil {
		return 0, err
	}
	hdr, _, _, err := manifest.ReadRoot(ctx, s, rk.RootManifestKey())
	if err != nil {
		return 0, err
	}
	return hdr.ManifestVersion, nil
}

// CheckAdvertise implements Gate. strong-current: samples lag for metrics on
// the TTL and always returns nil. bounded-stale: returns *UnhealthyError when
// the repo has lagged past the budget, or when the canonical bucket has been
// unreachable past the budget ("cannot determine lag", spec §26.2 default).
func (c *Controller) CheckAdvertise(ctx context.Context, tenant, repo string) error {
	now := c.cfg.Now()
	key := tenant + "/" + repo

	c.mu.Lock()
	st, ok := c.repos[key]
	if !ok {
		st = &repoState{}
		c.repos[key] = st
	}
	due := st.lastCheck.IsZero() || now.Sub(st.lastCheck) >= c.cfg.CheckInterval
	canonical, regional := c.cfg.Canonical, c.cfg.Regional
	c.mu.Unlock()

	if due {
		// Bucket reads run UNLOCKED; refresh re-acquires the mutex to apply.
		// Benign TOCTOU: two concurrent due requests may both refresh — the
		// sampling is idempotent, so the duplicate read is acceptable.
		c.refresh(ctx, tenant, repo, st, now, canonical, regional)
	}

	// Mode is immutable after construction (only Canonical is swappable, via
	// SetCanonicalForTest), so reading it without the lock is safe.
	if c.cfg.Mode == ModeStrongCurrent {
		return nil // correctness never depends on lag; sampling was for metrics
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var verdict *UnhealthyError
	switch {
	case st.lastGoodCheck.IsZero() || now.Sub(st.lastGoodCheck) > c.cfg.LagBudget:
		// Canonical unreachable (or never reached) for longer than the
		// budget: cannot determine lag → unhealthy (spec §26.2 default).
		verdict = &UnhealthyError{Tenant: tenant, Repo: repo, Reason: "cannot determine replication lag"}
	case !st.laggedSince.IsZero() && now.Sub(st.laggedSince) > c.cfg.LagBudget:
		verdict = &UnhealthyError{Tenant: tenant, Repo: repo, Lag: now.Sub(st.laggedSince), Reason: "lag budget exceeded"}
	}

	c.transitionLocked(ctx, tenant, repo, st, verdict)
	if verdict != nil {
		return verdict
	}
	return nil
}

// refresh samples both buckets' root versions and updates lag bookkeeping.
// Bucket reads run without the lock; the state update re-acquires it.
func (c *Controller) refresh(ctx context.Context, tenant, repo string, st *repoState, now time.Time, canonical, regional storage.ObjectStore) {
	canonVer, canonErr := readVersion(ctx, canonical, tenant, repo)
	regVer, regErr := readVersion(ctx, regional, tenant, repo)

	c.mu.Lock()
	defer c.mu.Unlock()
	st.lastCheck = now
	if regErr == nil {
		st.regionalVer = regVer
	}
	if canonErr != nil {
		return // lastGoodCheck unchanged → the cannot-determine countdown runs
	}
	st.canonicalVer = canonVer
	st.lastGoodCheck = now
	if regErr != nil {
		// Regional read failed: we cannot observe regional state, so leave
		// the lag bookkeeping untouched (mirror of the canonical-failure
		// guard above). In bounded-stale mode a regional outage fails the
		// served fetches anyway; don't let it also masquerade as recovery.
		return
	}
	if canonVer > regVer {
		if st.laggedSince.IsZero() {
			st.laggedSince = now
		}
	} else {
		st.laggedSince = time.Time{}
	}
	c.emitLagMetricsLocked(ctx, now)
}

// transitionLocked emits audit events + the unhealthy counter on state
// changes; the counter also fires on every refused advertisement. Caller
// holds c.mu.
func (c *Controller) transitionLocked(ctx context.Context, tenant, repo string, st *repoState, verdict *UnhealthyError) {
	nowUnhealthy := verdict != nil
	if nowUnhealthy {
		c.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "metric",
			slog.String("name", "replica_advert_unhealthy_total"), slog.Int("value", 1))
	}
	if nowUnhealthy == st.unhealthy {
		return
	}
	st.unhealthy = nowUnhealthy
	if nowUnhealthy {
		c.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "replica.repo.unhealthy",
			slog.String("tenant", tenant), slog.String("repo", repo),
			slog.String("reason", verdict.Error()))
		return
	}
	c.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "replica.repo.recovered",
		slog.String("tenant", tenant), slog.String("repo", repo))
}

// emitLagMetricsLocked logs replica_lag_seconds (max across tracked repos)
// and replica_repos_lagging. Caller holds c.mu.
func (c *Controller) emitLagMetricsLocked(ctx context.Context, now time.Time) {
	maxLag, lagging := c.lagStatsLocked(now)
	c.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "replica_lag_seconds"), slog.Float64("value", maxLag.Seconds()))
	c.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "replica_repos_lagging"), slog.Int("value", lagging))
}

func (c *Controller) lagStatsLocked(now time.Time) (maxLag time.Duration, lagging int) {
	for _, st := range c.repos {
		if st.laggedSince.IsZero() {
			continue
		}
		lagging++
		if d := now.Sub(st.laggedSince); d > maxLag {
			maxLag = d
		}
	}
	return maxLag, lagging
}

// Snapshot feeds GET /healthz/replica.
func (c *Controller) Snapshot() HealthSnapshot {
	now := c.cfg.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	maxLag, lagging := c.lagStatsLocked(now)
	reachable := false
	for _, st := range c.repos {
		if !st.lastGoodCheck.IsZero() && now.Sub(st.lastGoodCheck) <= c.cfg.LagBudget {
			reachable = true
			break
		}
	}
	if len(c.repos) == 0 {
		reachable = true // nothing sampled yet — don't alarm on idle replicas
	}
	return HealthSnapshot{
		Role:               "replica",
		Mode:               c.cfg.Mode.String(),
		ReposTracked:       len(c.repos),
		ReposLagging:       lagging,
		MaxLagSeconds:      maxLag.Seconds(),
		CanonicalReachable: reachable,
	}
}
