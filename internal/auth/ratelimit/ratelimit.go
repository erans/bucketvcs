package ratelimit

import (
	"log/slog"
	"math"
	"sync"
	"time"
)

// LimitedBucket identifies which bucket tripped in CheckDetailed.
//
// Only BucketIP is reachable in the current implementation; BucketUser is
// retained as a reserved label so callers that still switch on it remain
// safe, but Check never returns it. See the package doc on why we dropped
// the cross-IP per-user bucket.
type LimitedBucket int

const (
	BucketNone LimitedBucket = iota
	BucketIP
	// BucketUser is reserved. The original design included a per-user
	// bucket that accumulated failures across all source IPs; an attacker
	// could weaponise that for a targeted account-lockout DoS by hammering
	// a known username from a botnet. The bucket was removed and only
	// per-IP gating remains. The constant is retained so the existing API
	// (and any future composite-keyed reintroduction) stays compatible.
	BucketUser
)

// bucket holds a failure count that decays toward 0 at RefillPerMinute / 60
// failures per second. failures is allowed to be fractional during decay
// computation; it caps at Burst+1 on MarkFailure to bound retryAfter.
type bucket struct {
	failures  float64
	lastDecay time.Time
}

// Limiter rate-limits credential failures per source IP. A nil *Limiter is
// a complete no-op (operators disable via --auth-rate-limit-disabled).
//
// Caveat — shared egress IPs (corporate NAT, CI farms, reverse proxies
// running with --trust-proxy-headers=false) share a single bucket. Burst
// credential failures from any of them returns 429 to the entire group.
// Operators behind a reverse proxy MUST enable --trust-proxy-headers; high-
// volume CI environments may need a higher --auth-rate-limit-burst or an
// upstream allowlist (currently deferred; see spec §1.2).
type Limiter struct {
	cfg   Config
	mu    sync.Mutex
	perIP map[string]*bucket
	stop  chan struct{}
	wg    sync.WaitGroup
}

// NewLimiter constructs a Limiter and starts the background sweep goroutine.
// Pathological inputs (Burst < 1, RefillPerMinute < 0) are clamped to safe
// defaults with an operator-visible WARN log so a fat-fingered flag value
// doesn't silently change rate-limit behavior. Operators wanting to disable
// limiting must pass --auth-rate-limit-disabled=true (which constructs a nil
// Limiter at the call site).
func NewLimiter(cfg Config) *Limiter {
	if cfg.Burst < 1 {
		slog.Warn("ratelimit: invalid Burst clamped to default",
			"requested", cfg.Burst, "effective", DefaultConfig().Burst)
		cfg.Burst = DefaultConfig().Burst
	}
	if cfg.RefillPerMinute < 0 {
		slog.Warn("ratelimit: negative RefillPerMinute clamped to 0",
			"requested", cfg.RefillPerMinute)
		cfg.RefillPerMinute = 0
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	l := &Limiter{
		cfg:   cfg,
		perIP: make(map[string]*bucket),
		stop:  make(chan struct{}),
	}
	if cfg.SweepInterval > 0 {
		l.wg.Add(1)
		go l.sweepLoop()
	}
	return l
}

// Close stops the background sweep goroutine and waits for it to exit.
// Safe to call on a nil Limiter.
func (l *Limiter) Close() {
	if l == nil {
		return
	}
	close(l.stop)
	l.wg.Wait()
}

// Check returns (allowed, retryAfter). Equivalent to CheckDetailed but
// discards the which-bucket label. Always (true, 0) when l == nil. The
// user parameter is accepted for API stability and audit attribution; it
// does not affect gating.
func (l *Limiter) Check(ip, user string) (bool, time.Duration) {
	allowed, retry, _ := l.CheckDetailed(ip, user)
	return allowed, retry
}

// CheckDetailed returns (allowed, retryAfter, which). retryAfter is the
// time until at least one slot frees up (computed from RefillPerMinute);
// rounded UP to a whole second by the caller for the Retry-After header.
// `which` reports BucketIP when allowed=false; BucketNone otherwise.
//
// Does NOT increment failure counters — only MarkFailure does. The check
// is "is the IP bucket over Burst right now?"
//
// The user parameter is accepted for API stability and audit attribution
// but does NOT gate: an attacker on different source IPs cannot accumulate
// failures against a victim username's bucket. See the Limiter doc.
func (l *Limiter) CheckDetailed(ip, user string) (bool, time.Duration, LimitedBucket) {
	if l == nil {
		return true, 0, BucketNone
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()

	// Read-without-create: under high unique-IP traffic, allocating a
	// bucket on every Check (including for clients that never fail) makes
	// perIP grow unboundedly between sweeps. Only MarkFailure creates
	// entries; a missing bucket is implicitly "0 failures, allowed."
	ipB, ok := l.perIP[ip]
	if !ok {
		return true, 0, BucketNone
	}
	l.decayLocked(ipB, now)
	if ipB.failures >= float64(l.cfg.Burst) {
		return false, l.retryAfterLocked(ipB), BucketIP
	}
	return true, 0, BucketNone
}

// MarkFailure increments the IP bucket for ip. The user parameter is
// accepted for audit attribution; it does NOT affect bucket state.
// Each increment caps at Burst+1 to bound retryAfter at worst-case
// ~2x the per-slot refill time.
func (l *Limiter) MarkFailure(ip, user string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()
	maxFailures := float64(l.cfg.Burst + 1)

	b := l.getBucketLocked(l.perIP, ip)
	l.decayLocked(b, now)
	if b.failures+1 > maxFailures {
		b.failures = maxFailures
	} else {
		b.failures++
	}
}

// MarkSuccess resets the IP bucket to 0 failures. The user parameter is
// accepted for audit attribution; it does NOT affect bucket state.
// Good behavior earns full quota back.
func (l *Limiter) MarkSuccess(ip, user string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()
	if b, ok := l.perIP[ip]; ok {
		b.failures = 0
		b.lastDecay = now
	}
}

func (l *Limiter) getBucketLocked(m map[string]*bucket, key string) *bucket {
	if b, ok := m[key]; ok {
		return b
	}
	b := &bucket{failures: 0, lastDecay: l.cfg.Now()}
	m[key] = b
	return b
}

func (l *Limiter) decayLocked(b *bucket, now time.Time) {
	if l.cfg.RefillPerMinute <= 0 {
		b.lastDecay = now
		return
	}
	elapsed := now.Sub(b.lastDecay).Seconds()
	if elapsed <= 0 {
		return
	}
	r := l.cfg.RefillPerMinute / 60.0
	b.failures -= r * elapsed
	if b.failures < 0 {
		b.failures = 0
	}
	b.lastDecay = now
}

// retryAfterLocked computes time until failures drops to (Burst - 1),
// i.e. one slot frees up. Caller holds l.mu.
func (l *Limiter) retryAfterLocked(b *bucket) time.Duration {
	if l.cfg.RefillPerMinute <= 0 {
		// No decay — operator must wait for MarkSuccess. Return a long
		// but finite Retry-After (10 minutes) so clients don't hammer.
		return 10 * time.Minute
	}
	r := l.cfg.RefillPerMinute / 60.0
	excess := b.failures - float64(l.cfg.Burst-1)
	if excess <= 0 {
		return 0
	}
	secs := math.Ceil(excess / r)
	return time.Duration(secs) * time.Second
}

// sweepLoop periodically evicts buckets that have decayed to 0 failures.
// Bounded memory under sustained low traffic.
func (l *Limiter) sweepLoop() {
	defer l.wg.Done()
	ticker := time.NewTicker(l.cfg.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.sweepOnce()
		}
	}
}

func (l *Limiter) sweepOnce() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()
	// Age-based eviction threshold: 2 sweep intervals of silence. An
	// actively-limiting bucket is touched on every Check/MarkFailure so
	// its lastDecay stays current; an idle bucket (attacker gave up, or
	// the failure count drained to zero) gets evicted. The age check
	// covers the RefillPerMinute=0 mode where failures never decay to 0
	// on their own — without it, decay-disabled deployments under
	// distributed probing would grow perIP unboundedly.
	idleCutoff := 2 * l.cfg.SweepInterval
	for k, b := range l.perIP {
		// Capture last-access BEFORE decayLocked overwrites it with `now`.
		idle := now.Sub(b.lastDecay)
		l.decayLocked(b, now)
		if b.failures <= 0 || idle > idleCutoff {
			delete(l.perIP, k)
		}
	}
}
