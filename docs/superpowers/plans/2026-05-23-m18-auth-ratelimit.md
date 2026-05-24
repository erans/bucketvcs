# M18: Auth Rate-Limiting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add in-memory token-bucket rate limiting on credential failures per-(IP, user). Closes spec §30.5 MUST ("authentication failures SHOULD be rate-limited and audited"). HTTPS returns 429 + `Retry-After`; SSH drops the connection.

**Architecture:** New `internal/auth/ratelimit` package owns the `Limiter` struct with two internal maps (per-IP, per-user) of failure-decay buckets guarded by a mutex. `internal/gateway/auth.go::RunAuth` and `internal/sshd/server.go::publicKeyCallback` are the two integration sites — each calls `Limiter.Check` before credential verification, `MarkFailure` on credential errors, `MarkSuccess` on success. A nil Limiter is a complete no-op (operators disable via `--auth-rate-limit-disabled=true`).

**Tech Stack:** Go stdlib (`sync`, `time`, `net`, `math`); no new dependencies. The Limiter math is "failure count that decays" — equivalent to a token bucket but clearer naming in the auth-failure domain.

**Reference:** `docs/superpowers/specs/2026-05-23-m18-auth-ratelimit-design.md`. Section numbers like "spec §3" refer to that file.

---

## File layout

**New:**
- `internal/auth/ratelimit/ratelimit.go` — Limiter struct + Check / CheckDetailed / MarkFailure / MarkSuccess + bucket math + background sweep
- `internal/auth/ratelimit/ratelimit_test.go`
- `internal/auth/ratelimit/config.go` — Config struct + DefaultConfig
- `internal/auth/ratelimit/metrics.go` — EmitRateLimitMetric (slog-based, matches M14/M15/M17 pattern)
- `internal/gateway/ip.go` — `ClientIP(r, trustProxyHeaders) string` helper
- `internal/gateway/ip_test.go`
- `scripts/m18-rate-limit-smoke.sh`

**Modified:**
- `internal/auth/audit.go` — add `EmitRateLimitHit(ctx, logger, ip, user, bucket, retryAfterSec, transport)`
- `internal/gateway/auth.go` — `RunAuth` signature gains `limiter *ratelimit.Limiter, trustProxy bool`; Check before credential verify; MarkFailure on credentialError; MarkSuccess on success; 429 response on limit hit
- `internal/gateway/auth_test.go` — update 10 RunAuth call sites to pass `nil, false`
- `internal/gateway/server.go` — `Options.Limiter *ratelimit.Limiter` + `Options.TrustProxyHeaders bool`; pass to RunAuth at the call site
- `internal/sshd/server.go` — `Options.Limiter *ratelimit.Limiter`; publicKeyCallback Check/MarkFailure/MarkSuccess wiring
- `cmd/bucketvcs/serve.go` — 4 new flags + Limiter construction + threading into gateway + sshd Options

**No new packages besides `internal/auth/ratelimit`. No new migrations** (state is in-memory only per design §1.1).

---

## Tasks

### Task 1: Limiter core (ratelimit package)

**Files:**
- Create: `internal/auth/ratelimit/ratelimit.go`
- Create: `internal/auth/ratelimit/config.go`
- Create: `internal/auth/ratelimit/metrics.go`
- Create: `internal/auth/ratelimit/ratelimit_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/auth/ratelimit/ratelimit_test.go`:

```go
package ratelimit_test

import (
	"sync"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
)

// fakeClock returns a controllable Now() function.
func fakeClock() (*time.Time, func() time.Time) {
	t := time.Now()
	now := &t
	return now, func() time.Time { return *now }
}

func newLimiter(t *testing.T, burst int, refillPerMinute float64, nowFn func() time.Time) *ratelimit.Limiter {
	t.Helper()
	l := ratelimit.NewLimiter(ratelimit.Config{
		Burst:           burst,
		RefillPerMinute: refillPerMinute,
		SweepInterval:   24 * time.Hour, // sweep disabled in unit tests
		Now:             nowFn,
	})
	t.Cleanup(l.Close)
	return l
}

func TestLimiter_NilIsNoop(t *testing.T) {
	var l *ratelimit.Limiter
	allowed, retry := l.Check("1.2.3.4", "alice")
	if !allowed || retry != 0 {
		t.Errorf("nil Limiter.Check: (%v, %v), want (true, 0)", allowed, retry)
	}
	l.MarkFailure("1.2.3.4", "alice") // must not panic
	l.MarkSuccess("1.2.3.4", "alice") // must not panic
	_, _, which := l.CheckDetailed("1.2.3.4", "alice")
	if which != ratelimit.BucketNone {
		t.Errorf("nil CheckDetailed which=%v, want BucketNone", which)
	}
}

func TestLimiter_AllowsUpToBurst(t *testing.T) {
	_, now := fakeClock()
	l := newLimiter(t, 3, 1, now)
	for i := 0; i < 3; i++ {
		allowed, _ := l.Check("ip1", "alice")
		if !allowed {
			t.Errorf("Check #%d allowed=false, want true (before MarkFailure)", i)
		}
		l.MarkFailure("ip1", "alice")
	}
	// 4th Check: bucket has 3 failures, Burst=3 → rejected.
	allowed, retryAfter := l.Check("ip1", "alice")
	if allowed {
		t.Errorf("Check after Burst failures allowed=true, want false")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter=%v, want >0", retryAfter)
	}
}

func TestLimiter_PerIPIsolation(t *testing.T) {
	_, now := fakeClock()
	l := newLimiter(t, 3, 1, now)
	for i := 0; i < 3; i++ {
		l.MarkFailure("ipA", "userA")
	}
	allowed, _ := l.Check("ipB", "userB")
	if !allowed {
		t.Errorf("ipB allowed=false; ipA failures should not affect ipB")
	}
}

func TestLimiter_PerUserIsolation(t *testing.T) {
	_, now := fakeClock()
	l := newLimiter(t, 3, 1, now)
	// Same user, different IPs. User bucket accumulates from both.
	for i := 0; i < 2; i++ {
		l.MarkFailure("ipA", "alice")
	}
	for i := 0; i < 2; i++ {
		l.MarkFailure("ipB", "alice")
	}
	// alice has 4 failures across two IPs. Burst=3, so the user bucket has tripped.
	allowed, _, which := l.CheckDetailed("ipC", "alice")
	if allowed {
		t.Errorf("alice should be rate-limited (user bucket=4 > Burst=3)")
	}
	if which != ratelimit.BucketUser {
		t.Errorf("CheckDetailed which=%v, want BucketUser", which)
	}
}

func TestLimiter_MarkSuccessResetsBoth(t *testing.T) {
	_, now := fakeClock()
	l := newLimiter(t, 3, 1, now)
	for i := 0; i < 3; i++ {
		l.MarkFailure("ipX", "bob")
	}
	if a, _ := l.Check("ipX", "bob"); a {
		t.Fatalf("precondition: bucket should be tripped after 3 failures")
	}
	l.MarkSuccess("ipX", "bob")
	if a, _ := l.Check("ipX", "bob"); !a {
		t.Errorf("after MarkSuccess: Check allowed=false, want true (reset)")
	}
}

func TestLimiter_DecayOverTime(t *testing.T) {
	nowVar, now := fakeClock()
	l := newLimiter(t, 3, 60, now) // 60 failures/min = 1/sec — fast decay for test
	for i := 0; i < 3; i++ {
		l.MarkFailure("ipD", "carol")
	}
	if a, _ := l.Check("ipD", "carol"); a {
		t.Fatalf("3 failures should trip the bucket")
	}
	// Advance time by 2 seconds → 2 failures decay → bucket has 1 left.
	*nowVar = nowVar.Add(2 * time.Second)
	if a, _ := l.Check("ipD", "carol"); !a {
		t.Errorf("after 2s of decay (1/sec): bucket should allow Check")
	}
}

func TestLimiter_RetryAfterIsBounded(t *testing.T) {
	_, now := fakeClock()
	l := newLimiter(t, 3, 1, now) // 1 failure/min
	// Hammer with way more failures than Burst — must NOT push retryAfter unboundedly.
	for i := 0; i < 1000; i++ {
		l.MarkFailure("ipH", "dave")
	}
	_, retryAfter := l.Check("ipH", "dave")
	// At RefillPerMinute=1, retryAfter for ONE slot to free is ~60s.
	// MarkFailure caps at Burst+1, so worst case retryAfter ≈ 60-120s.
	if retryAfter > 3*time.Minute {
		t.Errorf("retryAfter=%v, want bounded near 1-2 minutes", retryAfter)
	}
}

func TestLimiter_MarkFailureEmptyUser(t *testing.T) {
	_, now := fakeClock()
	l := newLimiter(t, 3, 1, now)
	// SSH pre-resolution: user is empty. Only IP bucket increments.
	for i := 0; i < 3; i++ {
		l.MarkFailure("ipS", "")
	}
	// User bucket for "" should NOT exist or should not affect any user.
	if a, _ := l.Check("ipS", "alice"); a {
		t.Errorf("ipS Check should fail (IP bucket=3)")
	}
	// Different IP, same user — should pass since user bucket was never touched.
	if a, _ := l.Check("ipT", "alice"); !a {
		t.Errorf("ipT Check should pass (different IP, user bucket clean)")
	}
}

func TestLimiter_ConcurrentSafe(t *testing.T) {
	_, now := fakeClock()
	l := newLimiter(t, 100, 60, now)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := "ip" + string(rune('a'+(i%5)))
			user := "user" + string(rune('a'+(i%5)))
			for j := 0; j < 100; j++ {
				l.Check(ip, user)
				l.MarkFailure(ip, user)
				if j%10 == 0 {
					l.MarkSuccess(ip, user)
				}
			}
		}(i)
	}
	wg.Wait()
	// Run with -race to confirm no data race.
}
```

(`go test -race` covers the concurrency assertion.)

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/auth/ratelimit/... -count=1
```
Expected: FAIL with "undefined: ratelimit.NewLimiter".

- [ ] **Step 3: Implement config.go**

Create `internal/auth/ratelimit/config.go`:

```go
package ratelimit

import (
	"errors"
	"time"
)

// Config tunes the Limiter. See DefaultConfig for the production values.
type Config struct {
	// Burst is the max failure count a single (IP or user) bucket can hold
	// before Check rejects.
	Burst int

	// RefillPerMinute is the rate at which failures decay when idle.
	// Setting >0 enables decay; setting 0 disables decay entirely (failures
	// only clear via MarkSuccess).
	RefillPerMinute float64

	// SweepInterval is how often the background goroutine evicts buckets
	// that have decayed to zero. 0 disables the sweep.
	SweepInterval time.Duration

	// Now is the time source. Defaults to time.Now. Injected for tests.
	Now func() time.Time
}

// DefaultConfig returns production defaults: Burst=10, 1 failure cleared
// per minute, sweep every 5 minutes.
func DefaultConfig() Config {
	return Config{
		Burst:           10,
		RefillPerMinute: 1,
		SweepInterval:   5 * time.Minute,
		Now:             time.Now,
	}
}

// ErrInvalidConfig is returned by NewLimiter when Burst < 1 or
// RefillPerMinute < 0. Operators typically don't see this — they use
// --auth-rate-limit-disabled=true instead of pathological numbers.
var ErrInvalidConfig = errors.New("ratelimit: invalid config")
```

- [ ] **Step 4: Implement ratelimit.go**

Create `internal/auth/ratelimit/ratelimit.go`:

```go
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// LimitedBucket identifies which bucket tripped in CheckDetailed.
type LimitedBucket int

const (
	BucketNone LimitedBucket = iota
	BucketIP
	BucketUser
)

// bucket holds a failure count that decays toward 0 at RefillPerMinute / 60
// failures per second. failures is allowed to be fractional during decay
// computation; it caps at Burst+1 on MarkFailure to bound retryAfter.
type bucket struct {
	failures  float64
	lastDecay time.Time
}

// Limiter rate-limits credential failures per-(IP, user). A nil *Limiter
// is a complete no-op (operators disable via --auth-rate-limit-disabled).
type Limiter struct {
	cfg     Config
	mu      sync.Mutex
	perIP   map[string]*bucket
	perUser map[string]*bucket
	stop    chan struct{}
	wg      sync.WaitGroup
}

// NewLimiter constructs a Limiter and starts the background sweep goroutine.
// Returns ErrInvalidConfig for pathological inputs.
func NewLimiter(cfg Config) *Limiter {
	if cfg.Burst < 1 {
		cfg.Burst = DefaultConfig().Burst
	}
	if cfg.RefillPerMinute < 0 {
		cfg.RefillPerMinute = 0
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
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
// discards the which-bucket label. Always (true, 0) when l == nil.
func (l *Limiter) Check(ip, user string) (bool, time.Duration) {
	allowed, retry, _ := l.CheckDetailed(ip, user)
	return allowed, retry
}

// CheckDetailed returns (allowed, retryAfter, which). retryAfter is the
// time until at least one slot frees up (computed from RefillPerMinute);
// rounded UP to a whole second by the caller for the Retry-After header.
// `which` reports BucketIP or BucketUser when allowed=false; BucketNone otherwise.
//
// Does NOT increment failure counters — only MarkFailure does. The check
// is "is the current bucket state over Burst?"
func (l *Limiter) CheckDetailed(ip, user string) (bool, time.Duration, LimitedBucket) {
	if l == nil {
		return true, 0, BucketNone
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()

	ipB := l.getBucketLocked(l.perIP, ip)
	l.decayLocked(ipB, now)
	if ipB.failures >= float64(l.cfg.Burst) {
		return false, l.retryAfterLocked(ipB), BucketIP
	}

	if user != "" {
		userB := l.getBucketLocked(l.perUser, user)
		l.decayLocked(userB, now)
		if userB.failures >= float64(l.cfg.Burst) {
			return false, l.retryAfterLocked(userB), BucketUser
		}
	}
	return true, 0, BucketNone
}

// MarkFailure increments the IP bucket (always) and the user bucket
// (when user != ""). Each increment caps at Burst+1 to bound retryAfter
// at worst-case ~2x the per-slot refill time.
func (l *Limiter) MarkFailure(ip, user string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()
	cap := float64(l.cfg.Burst + 1)

	b := l.getBucketLocked(l.perIP, ip)
	l.decayLocked(b, now)
	if b.failures+1 > cap {
		b.failures = cap
	} else {
		b.failures++
	}

	if user != "" {
		b := l.getBucketLocked(l.perUser, user)
		l.decayLocked(b, now)
		if b.failures+1 > cap {
			b.failures = cap
		} else {
			b.failures++
		}
	}
}

// MarkSuccess resets the IP bucket to 0 failures (and the user bucket
// when user != ""). Good behavior earns full quota back.
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
		if b, ok := l.perUser[user]; ok {
			b.failures = 0
			b.lastDecay = now
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
	for k, b := range l.perIP {
		l.decayLocked(b, now)
		if b.failures <= 0 {
			delete(l.perIP, k)
		}
	}
	for k, b := range l.perUser {
		l.decayLocked(b, now)
		if b.failures <= 0 {
			delete(l.perUser, k)
		}
	}
}
```

- [ ] **Step 5: Implement metrics.go**

Create `internal/auth/ratelimit/metrics.go`:

```go
package ratelimit

import (
	"context"
	"log/slog"
)

// EmitRateLimitMetric logs one auth_ratelimit_total{outcome} sample.
// Outcomes: allowed, limited_ip, limited_user, failure_counted, success_reset.
func EmitRateLimitMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "auth_ratelimit_total"),
		slog.String("outcome", outcome),
		slog.Int("value", 1),
	)
}
```

- [ ] **Step 6: Run tests**

```bash
go test ./internal/auth/ratelimit/... -count=1 -race
```
Expected: PASS, no data race.

- [ ] **Step 7: Build + vet**

```bash
go vet ./... && go build ./...
```
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/auth/ratelimit/
git commit -m "auth/ratelimit: Limiter with token-bucket math + per-(IP,user) keying (M18 Task 1)"
```

---

### Task 2: ClientIP helper + EmitRateLimitHit audit

**Files:**
- Create: `internal/gateway/ip.go`
- Create: `internal/gateway/ip_test.go`
- Modify: `internal/auth/audit.go` (extend)

- [ ] **Step 1: Write the failing test for ClientIP**

Create `internal/gateway/ip_test.go`:

```go
package gateway_test

import (
	"net/http/httptest"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gateway"
)

func TestClientIP_NoTrustProxy(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")

	got := gateway.ClientIP(r, false)
	if got != "10.0.0.5" {
		t.Errorf("ClientIP(trustProxy=false) = %q, want 10.0.0.5 (X-F-F ignored)", got)
	}
}

func TestClientIP_TrustProxyUsesFirstHop(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")

	got := gateway.ClientIP(r, true)
	if got != "1.2.3.4" {
		t.Errorf("ClientIP(trustProxy=true) = %q, want 1.2.3.4 (first X-F-F hop)", got)
	}
}

func TestClientIP_TrustProxyNoHeader(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	// No X-Forwarded-For — fall back to RemoteAddr.

	got := gateway.ClientIP(r, true)
	if got != "10.0.0.5" {
		t.Errorf("ClientIP(trustProxy=true, no XFF) = %q, want 10.0.0.5", got)
	}
}

func TestClientIP_RemoteAddrNoPort(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.1"

	got := gateway.ClientIP(r, false)
	if got != "192.168.1.1" {
		t.Errorf("ClientIP(no port) = %q, want 192.168.1.1", got)
	}
}

func TestClientIP_XFFWhitespace(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Forwarded-For", "  4.5.6.7  , 10.0.0.1")

	got := gateway.ClientIP(r, true)
	if got != "4.5.6.7" {
		t.Errorf("ClientIP(XFF with whitespace) = %q, want 4.5.6.7", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/gateway/... -run TestClientIP -count=1
```
Expected: FAIL with "undefined: gateway.ClientIP".

- [ ] **Step 3: Implement ip.go**

Create `internal/gateway/ip.go`:

```go
package gateway

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the request's client IP. When trustProxyHeaders is set,
// the FIRST entry of X-Forwarded-For takes precedence over RemoteAddr.
// When false, RemoteAddr.Host is used unconditionally (defends against
// client-spoofed X-F-F when not behind a proxy).
//
// Operators behind a reverse proxy MUST enable trustProxyHeaders so the
// rate limiter keys on the real client; operators NOT behind a proxy MUST
// NOT enable it (X-F-F is client-supplied and spoofable).
func ClientIP(r *http.Request, trustProxyHeaders bool) string {
	if trustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		// RemoteAddr lacked a port (test code, unusual transports).
		return r.RemoteAddr
	}
	return host
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/gateway/... -run TestClientIP -count=1
```
Expected: PASS.

- [ ] **Step 5: Add EmitRateLimitHit to auth/audit.go**

Edit `internal/auth/audit.go`. Find the existing audit emitters (EmitTokenRotated from M17, etc.). Append:

```go
// EmitRateLimitHit logs the auth.ratelimit.hit audit event when the
// rate limiter rejects an auth attempt. bucket is "ip" or "user". user
// is empty for SSH pre-resolution and anonymous HTTPS.
func EmitRateLimitHit(ctx context.Context, logger *slog.Logger,
	ip, user, bucket string, retryAfterSec int, transport string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "auth.ratelimit.hit",
		slog.String("ip", ip),
		slog.String("user", user),
		slog.String("bucket", bucket),
		slog.Int("retry_after_sec", retryAfterSec),
		slog.String("transport", transport),
	)
}
```

(Confirm `context` and `log/slog` are already imported in audit.go.)

- [ ] **Step 6: Add a test for EmitRateLimitHit**

Append to `internal/auth/audit_test.go`:

```go
func TestEmitRateLimitHit(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	auth.EmitRateLimitHit(context.Background(), logger,
		"1.2.3.4", "alice", "ip", 60, "https")
	out := buf.String()
	if !strings.Contains(out, "auth.ratelimit.hit") {
		t.Errorf("event name missing: %s", out)
	}
	for _, key := range []string{"ip=1.2.3.4", "user=alice", "bucket=ip",
		"retry_after_sec=60", "transport=https"} {
		if !strings.Contains(out, key) {
			t.Errorf("missing %q in output: %s", key, out)
		}
	}
}
```

- [ ] **Step 7: Build + vet + test**

```bash
go vet ./... && go build ./...
go test ./internal/auth/... ./internal/gateway/... -count=1
```
Expected: clean + PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/gateway/ip.go internal/gateway/ip_test.go \
        internal/auth/audit.go internal/auth/audit_test.go
git commit -m "gateway+auth: ClientIP helper + EmitRateLimitHit audit (M18 Task 2)"
```

---

### Task 3: HTTPS gateway integration

**Files:**
- Modify: `internal/gateway/auth.go` — RunAuth signature gains limiter+trustProxy; integrate Check/MarkFailure/MarkSuccess; 429 response
- Modify: `internal/gateway/auth_test.go` — update 10 RunAuth call sites
- Modify: `internal/gateway/server.go` — Options.Limiter + Options.TrustProxyHeaders; pass to RunAuth at call site

- [ ] **Step 1: Add Limiter + TrustProxyHeaders to gateway.Options**

Edit `internal/gateway/server.go`. Find `type Options struct`:

```bash
grep -nA20 "^type Options struct" internal/gateway/server.go
```

Add (parallel to AuthStore):

```go
type Options struct {
    // ... existing fields ...
    AuthStore         auth.Store
    Limiter           *ratelimit.Limiter // optional; nil disables rate limiting
    TrustProxyHeaders bool               // honor X-Forwarded-For first hop
    // ... rest ...
}
```

Add import:

```go
"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
```

- [ ] **Step 2: Write failing test for HTTPS rate limit**

Append to `internal/gateway/auth_test.go`:

```go
func TestRunAuth_RateLimitsRepeatedBadCreds(t *testing.T) {
	st := &fakeStore{
		flags:           auth.RepoFlags{PublicRead: false},
		verifyCredErr:   auth.ErrInvalidCredential,
		lookupPermPerm:  auth.PermNone,
	}
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Burst:           3,
		RefillPerMinute: 0, // no decay during test
		SweepInterval:   24 * time.Hour,
	})
	defer limiter.Close()

	rr := &gateway.RoutedRequest{Tenant: "acme", Repo: "site", RequiredAction: auth.ActionRead}

	// 3 bad-cred attempts: each returns 401, MarkFailure increments.
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
		r.SetBasicAuth("alice", "wrongpass")
		r.RemoteAddr = "1.2.3.4:54321"
		_, ok := gateway.RunAuth(w, r, st, rr, limiter, false)
		if ok {
			t.Fatalf("attempt %d: RunAuth ok=true; expected 401", i)
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("attempt %d: code=%d, want 401", i, w.Code)
		}
	}

	// 4th attempt from same IP+user: limiter returns 429 BEFORE the credential check.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
	r.SetBasicAuth("alice", "wrongpass")
	r.RemoteAddr = "1.2.3.4:54321"
	_, ok := gateway.RunAuth(w, r, st, rr, limiter, false)
	if ok {
		t.Errorf("rate-limited attempt: ok=true; want false")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("code=%d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("Retry-After header missing")
	}
}

func TestRunAuth_SuccessfulAuthResetsRateLimit(t *testing.T) {
	verifyErr := auth.ErrInvalidCredential
	st := &fakeStore{
		flags:           auth.RepoFlags{PublicRead: false},
		verifyCredErr:   verifyErr,
		lookupPermPerm:  auth.PermRead,
		actor:           &auth.Actor{UserID: "u1", Name: "alice"},
	}
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Burst:           3,
		RefillPerMinute: 0,
		SweepInterval:   24 * time.Hour,
	})
	defer limiter.Close()
	rr := &gateway.RoutedRequest{Tenant: "acme", Repo: "site", RequiredAction: auth.ActionRead}

	// 2 failures then success.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
		r.SetBasicAuth("alice", "wrong")
		r.RemoteAddr = "5.6.7.8:1111"
		gateway.RunAuth(w, r, st, rr, limiter, false)
	}
	st.verifyCredErr = nil // success on next call
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
	r.SetBasicAuth("alice", "right")
	r.RemoteAddr = "5.6.7.8:1111"
	if _, ok := gateway.RunAuth(w, r, st, rr, limiter, false); !ok {
		t.Fatalf("successful auth: ok=false")
	}

	// Bucket should be reset — 3 more failures should be allowed again
	// (would trigger 429 if MarkSuccess hadn't reset).
	st.verifyCredErr = verifyErr
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
		r.SetBasicAuth("alice", "wrong")
		r.RemoteAddr = "5.6.7.8:1111"
		_, ok := gateway.RunAuth(w, r, st, rr, limiter, false)
		if ok {
			t.Fatalf("attempt %d: ok=true; expected 401", i)
		}
		if w.Code == http.StatusTooManyRequests {
			t.Errorf("attempt %d: 429 (should have been reset by MarkSuccess)", i)
		}
	}
}

func TestRunAuth_NilLimiterIsNoop(t *testing.T) {
	st := &fakeStore{flags: auth.RepoFlags{PublicRead: false}, verifyCredErr: auth.ErrInvalidCredential}
	rr := &gateway.RoutedRequest{Tenant: "acme", Repo: "site", RequiredAction: auth.ActionRead}
	// 100 attempts; with nil Limiter all should be 401 (never 429).
	for i := 0; i < 100; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
		r.SetBasicAuth("alice", "wrong")
		r.RemoteAddr = "9.9.9.9:1111"
		_, _ = gateway.RunAuth(w, r, st, rr, nil, false)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: 429 with nil Limiter; expected 401 every time", i)
		}
	}
}
```

The existing `fakeStore` in auth_test.go may need additional fields. Read the file first:

```bash
grep -nA15 "type fakeStore struct" internal/gateway/auth_test.go
```

Adapt the field names (e.g., `verifyCredErr` may already exist; if not, add it + adjust `fakeStore.VerifyCredential` to honor it).

Add imports at the top of `auth_test.go`: `"net/http"`, `"net/http/httptest"`, `"time"`, `"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"`.

- [ ] **Step 3: Run failing tests**

```bash
go test ./internal/gateway/... -run "TestRunAuth_(RateLimits|SuccessfulAuthResets|NilLimiterIsNoop)" -count=1
```
Expected: FAIL with "too many arguments in call to RunAuth" (signature mismatch).

- [ ] **Step 4: Extend RunAuth signature**

Edit `internal/gateway/auth.go`. Change the signature:

```go
func RunAuth(w http.ResponseWriter, r *http.Request, store auth.Store, rr *RoutedRequest,
    limiter *ratelimit.Limiter, trustProxy bool) (*auth.Actor, bool) {
```

At the top of the function body (before `GetRepoFlags`), add the rate-limit check:

```go
ip := ClientIP(r, trustProxy)
var basicUser string
if user, _, ok := r.BasicAuth(); ok {
    basicUser = user
}
if allowed, retryAfter, which := limiter.CheckDetailed(ip, basicUser); !allowed {
    retrySec := int(retryAfter.Seconds())
    if retrySec < 1 {
        retrySec = 1
    }
    w.Header().Set("Retry-After", strconv.Itoa(retrySec))
    http.Error(w,
        fmt.Sprintf("rate limited; retry after %ds", retrySec),
        http.StatusTooManyRequests)
    bucketName := "ip"
    outcome := "limited_ip"
    if which == ratelimit.BucketUser {
        bucketName = "user"
        outcome = "limited_user"
    }
    auth.EmitRateLimitHit(r.Context(), nil, ip, basicUser, bucketName, retrySec, "https")
    ratelimit.EmitRateLimitMetric(r.Context(), nil, outcome)
    return nil, false
}
```

Add imports: `"fmt"`, `"strconv"`, `"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"`.

Then, in the existing `if isCredentialError(err)` branch (around line 56), add the MarkFailure call BEFORE writing the 401:

```go
if isCredentialError(err) {
    limiter.MarkFailure(ip, basicUser)
    ratelimit.EmitRateLimitMetric(r.Context(), nil, "failure_counted")
    challenge(w, "invalid credentials")
}
```

After successful credential verification (right after the existing `go func(id string) { ... }(tokenID)` block which records last_used), add:

```go
limiter.MarkSuccess(ip, basicUser)
ratelimit.EmitRateLimitMetric(r.Context(), nil, "success_reset")
```

Also emit the "allowed" metric on the early-return path AFTER the rate-limit check passes (just inside the `if !allowed` else path implicitly — add an explicit emit right after the rate-limit check passes):

```go
// After the rate-limit Check passes:
ratelimit.EmitRateLimitMetric(r.Context(), nil, "allowed")
```

- [ ] **Step 5: Update all RunAuth callers**

```bash
grep -rn "gateway.RunAuth\|RunAuth(" --include="*.go" internal/ | head
```

The 10 call sites in `internal/gateway/auth_test.go` need updating. For each, add `nil, false` as the last two args:

```go
// Before:
actor, ok := RunAuth(w, r, st, rr)
// After:
actor, ok := RunAuth(w, r, st, rr, nil, false)
```

If there are call sites elsewhere (e.g., `internal/gateway/server.go::ServeHTTP` or wherever the handler dispatches), update them too with `s.opts.Limiter, s.opts.TrustProxyHeaders`.

```bash
grep -n "RunAuth" internal/gateway/server.go internal/gateway/*.go | head
```

- [ ] **Step 6: Run tests**

```bash
go test ./internal/gateway/... -count=1
```
Expected: PASS for new + existing tests.

- [ ] **Step 7: Build + vet**

```bash
go vet ./... && go build ./...
```
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/gateway/auth.go internal/gateway/auth_test.go \
        internal/gateway/server.go
git commit -m "gateway: HTTPS rate-limit on credential failures (M18 Task 3)"
```

---

### Task 4: SSH integration

**Files:**
- Modify: `internal/sshd/server.go` — Options.Limiter; publicKeyCallback Check/MarkFailure/MarkSuccess

- [ ] **Step 1: Survey sshd Options + publicKeyCallback**

```bash
grep -nA15 "^type Options struct" internal/sshd/server.go
grep -nA20 "func.*publicKeyCallback" internal/sshd/server.go
```

Read both. Note how Options is constructed and how the callback resolves the user from a key fingerprint.

- [ ] **Step 2: Add Limiter to sshd.Options**

Edit `internal/sshd/server.go`. Add to Options:

```go
type Options struct {
    // ... existing fields ...
    Limiter *ratelimit.Limiter
    // TrustProxyHeaders is N/A for SSH (no X-Forwarded-For); IP comes
    // directly from the connection RemoteAddr.
}
```

Add import: `"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"`.

- [ ] **Step 3: Write failing test for SSH rate limit**

Append to `internal/sshd/server_test.go`:

```go
func TestPublicKeyCallback_RateLimitsAfterBurst(t *testing.T) {
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Burst:           3,
		RefillPerMinute: 0,
		SweepInterval:   24 * time.Hour,
	})
	defer limiter.Close()
	store := &mockStore{verifyErr: auth.ErrInvalidCredential}
	s := &Server{opts: Options{Store: store, Limiter: limiter}}

	fakeKey := mustGenKey(t)
	for i := 0; i < 3; i++ {
		_, err := s.publicKeyCallback(fakeConnMeta("1.2.3.4:5555", "git"), fakeKey)
		if err == nil {
			t.Errorf("attempt %d: nil err; want rejection", i)
		}
	}
	// 4th attempt: rate-limited BEFORE the store call. Store should NOT
	// be invoked a 4th time.
	storeCallsBefore := store.verifyCalls
	_, err := s.publicKeyCallback(fakeConnMeta("1.2.3.4:5555", "git"), fakeKey)
	if err == nil {
		t.Fatalf("4th attempt: nil err; want rate-limit rejection")
	}
	if store.verifyCalls != storeCallsBefore {
		t.Errorf("rate-limited attempt invoked store.VerifyCredential (calls went %d → %d)",
			storeCallsBefore, store.verifyCalls)
	}
}

// mockStore is a counting fake; existing tests may already define it.
// If so, add the verifyCalls counter to the existing struct.
```

The fixtures (`mustGenKey`, `fakeConnMeta`, `mockStore`) may already exist in the test file. Read first and adapt:

```bash
grep -n "mustGen\|fakeConnMeta\|mockStore\|TestPublicKey" internal/sshd/server_test.go | head -10
```

- [ ] **Step 4: Run test to verify it fails**

```bash
go test ./internal/sshd/... -run TestPublicKeyCallback_RateLimitsAfterBurst -count=1
```
Expected: FAIL — either no Limiter field on Options yet, or the callback never calls it.

- [ ] **Step 5: Wire Limiter into publicKeyCallback**

Edit `internal/sshd/server.go::publicKeyCallback`. At the top of the function (before `meta.User() != "git"`):

```go
ip := sshRemoteIP(meta)
if allowed, _, which := s.opts.Limiter.CheckDetailed(ip, ""); !allowed {
    retrySec := 1 // SSH has no Retry-After mechanism; the value is for the audit only
    bucketName := "ip"
    if which == ratelimit.BucketUser {
        bucketName = "user"
    }
    auth.EmitRateLimitHit(context.Background(), nil, ip, "", bucketName, retrySec, "ssh")
    ratelimit.EmitRateLimitMetric(context.Background(), nil, "limited_"+bucketName)
    return nil, errors.New("rate limited")
}
```

(Note: for SSH at this layer the user isn't known yet — the key carries it. So we always pass `user=""` to Check; only the IP bucket is consulted. We MarkSuccess with the resolved user later.)

After the existing `VerifyCredential` call, branch on success vs failure:

```go
actor, keyID, scope, err := s.opts.Store.VerifyCredential(...)
if err != nil {
    s.opts.Limiter.MarkFailure(ip, "")
    ratelimit.EmitRateLimitMetric(context.Background(), nil, "failure_counted")
    // ... existing error handling ...
    return nil, err
}
// On success:
s.opts.Limiter.MarkSuccess(ip, actor.Name) // actor.Name may be empty for deploy keys; safe
ratelimit.EmitRateLimitMetric(context.Background(), nil, "success_reset")
// ... existing success handling ...
```

Add a helper:

```go
func sshRemoteIP(meta ssh.ConnMetadata) string {
    addr := meta.RemoteAddr()
    if addr == nil {
        return ""
    }
    host, _, err := net.SplitHostPort(addr.String())
    if err != nil {
        return addr.String()
    }
    return host
}
```

Add imports: `"context"`, `"errors"`, `"net"`, `"github.com/bucketvcs/bucketvcs/internal/auth"`, `"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"`. (Most likely some already imported.)

- [ ] **Step 6: Run sshd tests**

```bash
go test ./internal/sshd/... -count=1
```
Expected: PASS (new test + existing pass; existing tests with nil Limiter are unaffected by the nil-noop).

- [ ] **Step 7: Build + vet**

```bash
go vet ./... && go build ./...
```
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/sshd/server.go internal/sshd/server_test.go
git commit -m "sshd: rate-limit publicKeyCallback failures (M18 Task 4)"
```

---

### Task 5: CLI flags + Limiter construction in serve.go

**Files:**
- Modify: `cmd/bucketvcs/serve.go` — 4 new flags + Limiter construction; thread into gateway.Options + sshd.Options

- [ ] **Step 1: Survey existing serve.go flag pattern**

```bash
grep -nE "fs.Bool\|fs.Int\|fs.String\|fs.Float\|fs.Duration" cmd/bucketvcs/serve.go | head -15
```

Read the existing flag declarations to match the style (`--auth-db`, `--listen`, etc.).

- [ ] **Step 2: Add the 4 flags**

In `runServe` (or wherever flags are declared):

```go
rateBurst := fs.Int("auth-rate-limit-burst", 10,
    "Max auth failures per (IP, user) bucket before throttling (0 disables)")
rateRefill := fs.Float64("auth-rate-limit-refill-per-minute", 1,
    "Failures cleared per minute when idle")
trustProxy := fs.Bool("trust-proxy-headers", false,
    "Honor X-Forwarded-For first hop for client IP")
rateLimitDisabled := fs.Bool("auth-rate-limit-disabled", false,
    "Disable rate limiting entirely (use when running an external rate limiter)")
```

- [ ] **Step 3: Construct the Limiter**

After flags are parsed, before constructing the gateway / sshd servers:

```go
var limiter *ratelimit.Limiter
if !*rateLimitDisabled {
    limiter = ratelimit.NewLimiter(ratelimit.Config{
        Burst:           *rateBurst,
        RefillPerMinute: *rateRefill,
        SweepInterval:   5 * time.Minute,
    })
    defer limiter.Close()
}
```

Add import: `"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"`.

- [ ] **Step 4: Thread into gateway + sshd**

Find where `gateway.New` (or `gateway.NewServer`) is called:

```bash
grep -n "gateway.New\|sshd.New" cmd/bucketvcs/serve.go
```

Add to the gateway Options literal:

```go
gw := gateway.New(gateway.Options{
    // ... existing fields ...
    Limiter:           limiter,
    TrustProxyHeaders: *trustProxy,
})
```

Same for sshd:

```go
ssh := sshd.New(sshd.Options{
    // ... existing fields ...
    Limiter: limiter,
})
```

- [ ] **Step 5: Build + vet + run all tests**

```bash
go vet ./... && go build ./...
go test ./... -count=1 -timeout 180s 2>&1 | grep -E "^FAIL" | head
```
Expected: clean + no FAIL.

- [ ] **Step 6: Commit**

```bash
git add cmd/bucketvcs/serve.go
git commit -m "cmd/serve: --auth-rate-limit-* flags + Limiter wiring (M18 Task 5)"
```

---

### Task 6: End-to-end smoke

**Files:**
- Create: `scripts/m18-rate-limit-smoke.sh`

- [ ] **Step 1: Write the smoke**

Create `scripts/m18-rate-limit-smoke.sh`:

```bash
#!/usr/bin/env bash
# M18 end-to-end smoke for auth rate-limiting.
#  1. Start serve with low burst (3) + fast refill (60/min = 1/sec)
#  2. Make 4 consecutive bad-cred HTTPS requests — 4th gets 429 with Retry-After
#  3. Wait 2 seconds for decay
#  4. Retry — should get 401 (still bad creds), NOT 429 (decayed)
#  5. Assert auth.ratelimit.hit appeared in serve log

set -euo pipefail
trap 'echo "M18_RATE_LIMIT_SMOKE_FAILED" >&2' ERR

WORK=$(mktemp -d)
echo "smoke working dir: $WORK"

go build -o "$WORK/bucketvcs" ./cmd/bucketvcs

AUTHDB="$WORK/auth.db"
"$WORK/bucketvcs" user add alice
"$WORK/bucketvcs" repo register --auth-db="$AUTHDB" --no-init acme/site

mkdir -p "$WORK/storage"
"$WORK/bucketvcs" serve \
    --addr=":12345" \
    --auth-db="$AUTHDB" \
    --store=localfs:"$WORK/storage" \
    --lfs=false \
    --auth-rate-limit-burst=3 \
    --auth-rate-limit-refill-per-minute=60 >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
trap 'kill $SERVE_PID 2>/dev/null || true; trap - ERR; echo "M18_RATE_LIMIT_SMOKE_FAILED" >&2; exit 1' ERR
sleep 1

URL="http://alice:wrongpass@127.0.0.1:12345/acme/site/info/refs?service=git-upload-pack"

# 3 bad-cred attempts: each returns 401.
for i in 1 2 3; do
    code=$(curl -s -o /dev/null -w "%{http_code}" -u alice:wrongpass \
        "http://127.0.0.1:12345/acme/site/info/refs?service=git-upload-pack")
    if [ "$code" != "401" ]; then
        echo "attempt $i: code=$code, expected 401"
        exit 1
    fi
done

# 4th attempt: 429 with Retry-After header populated.
HEADERS=$(curl -s -i -o - -u alice:wrongpass \
    "http://127.0.0.1:12345/acme/site/info/refs?service=git-upload-pack" | head -20)
if ! echo "$HEADERS" | grep -q "^HTTP/.* 429"; then
    echo "4th attempt: missing 429 status"
    echo "$HEADERS"
    exit 1
fi
if ! echo "$HEADERS" | grep -qi "^Retry-After:"; then
    echo "4th attempt: missing Retry-After header"
    echo "$HEADERS"
    exit 1
fi

# Wait 2 seconds: at 60/min decay rate, 2 slots free up.
sleep 2

# Retry: should be back to 401.
code=$(curl -s -o /dev/null -w "%{http_code}" -u alice:wrongpass \
    "http://127.0.0.1:12345/acme/site/info/refs?service=git-upload-pack")
if [ "$code" != "401" ]; then
    echo "after-decay attempt: code=$code, expected 401"
    cat "$WORK/serve.log"
    exit 1
fi

# Confirm audit fired in serve log.
grep -q "auth.ratelimit.hit" "$WORK/serve.log" || {
    echo "no auth.ratelimit.hit in serve.log"
    tail -50 "$WORK/serve.log"
    exit 1
}

kill $SERVE_PID 2>/dev/null || true
wait 2>/dev/null || true

echo "M18_RATE_LIMIT_SMOKE_OK"
```

Make it executable:

```bash
chmod +x scripts/m18-rate-limit-smoke.sh
```

- [ ] **Step 2: Run the smoke**

```bash
bash scripts/m18-rate-limit-smoke.sh 2>&1 | tail -10
```
Expected: ends `M18_RATE_LIMIT_SMOKE_OK`.

If a step fails, $WORK is preserved — inspect $WORK/serve.log.

Common adjustments:
- CLI flag names may differ. `user add` vs `user create`, `--addr` vs `--listen`, `--store` vs `--storage`. Match the actual flags via `bucketvcs <subcommand> --help`.
- `bucketvcs user add` may need additional flags (password). Match the smoke patterns in `scripts/m17-auth-scopes-smoke.sh`.

- [ ] **Step 3: Run prior smokes for regression**

```bash
bash scripts/m17-auth-scopes-smoke.sh 2>&1 | tail -3
bash scripts/m14-policy-smoke.sh 2>&1 | tail -3
bash scripts/m15-webhook-smoke.sh 2>&1 | tail -3
```
Expected: each ends `*_OK`. These run with default flags (Limiter active with default 10-burst); the smoke patterns don't generate enough failures to trip the limit.

- [ ] **Step 4: Commit**

```bash
git add scripts/m18-rate-limit-smoke.sh
git commit -m "scripts: M18 auth rate-limit end-to-end smoke (M18 Task 6)"
```

---

## Acceptance criteria

- 6 tasks complete
- `go test ./...` clean (with `-race` on the ratelimit package); `go vet ./...` clean
- `scripts/m18-rate-limit-smoke.sh` ends `M18_RATE_LIMIT_SMOKE_OK`
- All prior smokes still pass (default Burst=10 doesn't trip on normal traffic)
- 4 new `bucketvcs serve` flags visible in `--help`
- Limiter == nil produces zero behavior change (verified by `TestRunAuth_NilLimiterIsNoop`)
- Rate-limit hit at HTTPS returns 429 + `Retry-After` header + audit + metric
- Rate-limit hit at SSH drops the connection + audit + metric
- ErrInsufficientScope does NOT count toward the rate limit (existing M17 test covers this implicitly)

## Spec coverage check

| Spec section | Task |
|---|---|
| §2 Architecture overview | Task 1 (Limiter) + Tasks 3-5 (integration) |
| §3 TokenBucket model | Task 1 |
| §4 Limiter API | Task 1 |
| §5 Config + defaults | Task 1 |
| §6 Operator CLI flags | Task 5 |
| §7 IP extraction | Task 2 |
| §8 Observability (metric + audit) | Task 1 (metric) + Task 2 (audit) |
| §9 Failure modes | covered across Tasks 3 (HTTPS) + 4 (SSH) |
| §10 Testing (unit) | Task 1 |
| §10 Testing (integration HTTPS) | Task 3 |
| §10 Testing (integration SSH) | Task 4 |
| §10 Smoke | Task 6 |
