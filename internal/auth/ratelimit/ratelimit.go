package ratelimit

import (
	"log/slog"
	"math"
	"sync"
	"time"
)

// LimitedBucket identifies which bucket tripped in CheckDetailed.
type LimitedBucket int

const (
	BucketNone LimitedBucket = iota
	BucketIP
	// BucketUser is the cross-IP per-account bucket: failures against one
	// username accumulate regardless of source IP, so distributed guessing
	// spread across a botnet still trips a throttle. Deliberate tradeoff:
	// the same bucket lets an attacker fill a victim's account quota and
	// lock the legitimate user out, which is why UserBurst defaults an
	// order of magnitude above Burst — spraying trips it, casual
	// lockout-DoS costs 10x the traffic. Operators who prefer no
	// account-level gating can set --auth-rate-limit-user-burst=0 to
	// disable the per-user bucket (failures then gate on IP only).
	BucketUser
)

// bucket holds a failure count that decays toward 0 at RefillPerMinute / 60
// failures per second. failures is allowed to be fractional during decay
// computation; it caps at Burst+1 on MarkFailure to bound retryAfter.
type bucket struct {
	failures  float64
	lastDecay time.Time
}

// Limiter rate-limits credential failures per source IP and per account
// name. A nil *Limiter is a complete no-op (operators disable via
// --auth-rate-limit-disabled).
//
// Caveat — shared egress IPs (corporate NAT, CI farms, reverse proxies
// running with --trust-proxy-headers=false) share a single bucket. Burst
// credential failures from any of them returns 429 to the entire group.
// Operators behind a reverse proxy MUST enable --trust-proxy-headers; high-
// volume CI environments may need a higher --auth-rate-limit-burst or an
// upstream allowlist (currently deferred; see spec §1.2).
type Limiter struct {
	cfg     Config
	mu      sync.RWMutex
	perIP   map[string]*bucket
	perUser map[string]*bucket
	stop    chan struct{}
	wg      sync.WaitGroup
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
	if cfg.UserBurst < 0 {
		slog.Warn("ratelimit: negative UserBurst clamped to 0 (per-user bucket disabled)",
			"requested", cfg.UserBurst)
		cfg.UserBurst = 0
	}
	l := &Limiter{
		cfg:     cfg,
		perIP:   make(map[string]*bucket),
		perUser: make(map[string]*bucket),
		stop:    make(chan struct{}),
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
// `which` reports BucketIP or BucketUser when allowed=false; BucketNone
// otherwise.
//
// Does NOT increment failure counters — only MarkFailure does. The check
// is "is the IP bucket over Burst, or the per-user bucket over UserBurst,
// right now?" An empty user skips the per-user gate (pre-resolution callers
// such as the SSH gate pass user="").
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
	if ipB, ok := l.perIP[ip]; ok {
		l.decayLocked(ipB, now)
		if ipB.failures >= float64(l.cfg.Burst) {
			return false, l.retryAfterLocked(ipB, l.cfg.Burst), BucketIP
		}
	}
	if user != "" && l.cfg.UserBurst > 0 {
		if uB, ok := l.perUser[user]; ok {
			l.decayLocked(uB, now)
			if uB.failures >= float64(l.cfg.UserBurst) {
				return false, l.retryAfterLocked(uB, l.cfg.UserBurst), BucketUser
			}
		}
	}
	return true, 0, BucketNone
}

// MarkFailure increments the IP bucket for ip and, when user is non-empty,
// the per-user bucket for user. Each increment caps at burst+1 to bound
// retryAfter at worst-case ~2x the per-slot refill time.
func (l *Limiter) MarkFailure(ip, user string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()

	b := l.getBucketLocked(l.perIP, ip)
	l.decayLocked(b, now)
	if maxFailures := float64(l.cfg.Burst + 1); b.failures+1 > maxFailures {
		b.failures = maxFailures
	} else {
		b.failures++
	}
	if user != "" && l.cfg.UserBurst > 0 {
		ub := l.getBucketLocked(l.perUser, user)
		l.decayLocked(ub, now)
		if maxFailures := float64(l.cfg.UserBurst + 1); ub.failures+1 > maxFailures {
			ub.failures = maxFailures
		} else {
			ub.failures++
		}
	}
}

// MarkSuccess resets the IP bucket to 0 failures and, when user is
// non-empty, that user's per-user bucket. Resetting only the
// authenticated principal's bucket (not every bucket on the IP) is what
// closes throttle laundering: interleaving valid logins as user A no
// longer clears failures accumulated against user V on the same IP.
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
	if user != "" {
		if ub, ok := l.perUser[user]; ok {
			ub.failures = 0
			ub.lastDecay = now
		}
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

// retryAfterLocked computes time until failures drops to (burst - 1),
// i.e. one slot frees up. Caller holds l.mu.
func (l *Limiter) retryAfterLocked(b *bucket, burst int) time.Duration {
	if l.cfg.RefillPerMinute <= 0 {
		// No decay — operator must wait for MarkSuccess. Return a long
		// but finite Retry-After (10 minutes) so clients don't hammer.
		return 10 * time.Minute
	}
	r := l.cfg.RefillPerMinute / 60.0
	excess := b.failures - float64(burst-1)
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
		// U-12: refill-disabled mode promises that only MarkSuccess clears a
		// loaded bucket. Keep such buckets even after a long idle period;
		// normal refill mode retains age-based eviction as its memory bound.
		if b.failures <= 0 || (l.cfg.RefillPerMinute > 0 && idle > idleCutoff) {
			delete(l.perIP, k)
		}
	}
	for k, b := range l.perUser {
		idle := now.Sub(b.lastDecay)
		l.decayLocked(b, now)
		if b.failures <= 0 || (l.cfg.RefillPerMinute > 0 && idle > idleCutoff) {
			delete(l.perUser, k)
		}
	}
}
