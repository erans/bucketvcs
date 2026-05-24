# M18: Auth — Rate-limiting on credential failures

**Status:** Design.
**Date:** 2026-05-23.
**Scope:** Close spec §30.5 MUST: "All authentication failures SHOULD be rate-limited and audited." In-memory token-bucket throttling per-(IP, user) on credential failures, with `Retry-After` on HTTPS and audit/metric coverage.

## 1. Goals

### 1.1 In scope

- New `internal/auth/ratelimit` package with a `Limiter` exposing `Check(ip, user)`, `MarkFailure(ip, user)`, `MarkSuccess(ip, user)`
- Token-bucket math (modeled as "failure count that decays") per-(IP, user) tuple. Two independent maps protected by a single mutex.
- Per-IP and per-user buckets evaluated independently. Failure increments BOTH (when user is known); limit hit on EITHER bucket → 429 + `Retry-After`.
- Default config: `Burst=10`, `RefillPerMinute=1`. 10 failures allowed in a tight window; 1 failure cleared per minute when idle (full recovery in 10 minutes of silence).
- Successful auth fully refills both buckets — good behavior earns quota back.
- HTTPS integration in `internal/gateway/auth.go`: `Check` before credential verification; `MarkFailure` on `credentialError`; `MarkSuccess` on success. Limit-hit returns 429 with `Retry-After: <seconds>` header.
- SSH integration in `internal/sshd/server.go::PublicKeyCallback`: Check before key resolution; MarkFailure on rejection; MarkSuccess on success. Limit-hit returns connection drop (SSH has no Retry-After).
- Failure counting scope: `ErrInvalidCredential`, `ErrNoSuchUser`, `ErrNoSuchKey`, `ErrTokenExpired`. Does NOT count: `ErrInsufficientScope` (authorization issue, not credential brute force), anonymous-no-credentials requests (normal flow signal).
- 4 new operator flags on `bucketvcs serve` (`--auth-rate-limit-burst`, `--auth-rate-limit-refill-per-minute`, `--trust-proxy-headers`, `--auth-rate-limit-disabled`).
- 5 metric outcomes (`auth_ratelimit_total{outcome}`) + 1 audit event (`auth.ratelimit.hit`).
- Periodic GC sweep removes idle buckets (failures=0) from the maps to bound memory.

### 1.2 Out of scope (deferred)

- **Persistent state across restart** (sqlite-backed) — restart-reset is acceptable for a SOFT defense
- **Per-(IP, repo) and per-(token-id, repo) buckets** — current keying is per-(IP, user)
- **CIDR-based exemptions / IP allowlist** (operator-trusted CI ranges)
- **Distributed coordination across `bucketvcs serve` replicas** — per-process state
- **Connection-level rate limit on SSH** (TCP layer vs auth-callback layer)
- **Adaptive backoff** (Retry-After grows under sustained pressure)
- **`bucketvcs auth rate-limit status` CLI** for operator inspection
- **Trusted user-agent whitelist** (e.g., known-good CI bots bypassing)
- **LFS verify-token path** — M13.1 HMAC verify is not a credential check (it validates a pre-issued token's HMAC). No rate-limiting added there for Tier 1.

## 2. Architecture overview

```
internal/auth/ratelimit/
  ratelimit.go         (new) — TokenBucket math + Limiter struct + Check/MarkFailure/MarkSuccess
  ratelimit_test.go    (new)
  config.go            (new) — Config struct + DefaultConfig()
  metrics.go           (new) — EmitRateLimitMetric

internal/auth/audit.go              (extend) — EmitRateLimitHit
internal/gateway/auth.go            (modified) — wire Check/MarkFailure/MarkSuccess; 429 response on limit hit
internal/gateway/ip.go              (new) — ClientIP(r, trustProxy) helper
internal/sshd/server.go             (modified) — wire Check/MarkFailure/MarkSuccess into PublicKeyCallback
cmd/bucketvcs/serve.go              (modified) — new flags + Limiter construction

scripts/m18-rate-limit-smoke.sh     (new)
```

**Wiring at HTTPS gateway** (`internal/gateway/auth.go`):
1. Extract `ip = ClientIP(r, cfg.TrustProxy)` and `user = parseBasicUsername(r.Header)` (may be empty for anon)
2. `allowed, retryAfter := limiter.Check(ip, user)`
3. If `!allowed`:
   - Write `429 Too Many Requests`
   - `Retry-After: <ceil(retryAfter.Seconds())>` header
   - Body: `"rate limited; retry after <N>s"`
   - Emit `auth.ratelimit.hit` audit
   - Increment `auth_ratelimit_total{outcome=limited_ip}` or `limited_user` based on which bucket tripped
   - Return
4. Proceed with existing auth flow
5. If `credentialError(err)`:
   - `limiter.MarkFailure(ip, user)`
   - Increment `auth_ratelimit_total{outcome=failure_counted}`
   - Write 401 (existing behavior unchanged)
6. If success:
   - `limiter.MarkSuccess(ip, user)`
   - Increment `auth_ratelimit_total{outcome=success_reset}`

**Wiring at SSH** (`internal/sshd/server.go::publicKeyCallback`):
1. `ip := sshRemoteIP(conn)` (parses `conn.RemoteAddr().String()`)
2. `allowed, _ := limiter.Check(ip, "")` (user not yet resolved at this point)
3. If `!allowed`: return error (connection drops). Audit + metric.
4. Resolve key → user
5. If key not found / verification error: `limiter.MarkFailure(ip, "")`. Audit + metric. Return error.
6. If success: `limiter.MarkSuccess(ip, resolvedUser)` — second call now that we know the user. Return permissions.

**Optionality / nil-Limiter behavior:** `Limiter == nil` is supported — every method short-circuits. Operators who set `--auth-rate-limit-disabled=true` get a nil Limiter through to integration points; the auth path runs identically to today.

**Failure counting scope:**
- COUNT (`credentialError(err)` already enumerates these):
  - `ErrInvalidCredential` (wrong secret)
  - `ErrTokenExpired`
  - `ErrNoSuchUser` (username doesn't exist — username enumeration defense)
  - `ErrNoSuchKey` (SSH key not registered)
- DON'T COUNT:
  - `ErrInsufficientScope` (right token, wrong scope — authorization issue)
  - Anonymous-no-credentials path (normal flow signal — there's no failed attempt)
  - DB/network errors (operational failure, not credential failure)

## 3. TokenBucket model

The standard token-bucket can be expressed equivalently as a "failure count that decays" — same math, clearer naming for this domain:

```go
type bucket struct {
    failures  float64   // accumulated failure count; refills naturally over time
    lastDecay time.Time // when failures was last computed
}
```

- **Decay rate:** `r = RefillPerMinute / 60` failures cleared per second
- **Refill on access:** `failures = max(0, failures - r * (now - lastDecay).Seconds()); lastDecay = now`
- **Check:** refill, then reject when `failures >= Burst`; return `retryAfter = ceil((failures - (Burst - 1)) / r)` seconds
- **MarkFailure:** refill, then `failures = min(Burst + 1, failures + 1)` (cap at Burst+1 so even when limit is hit, the math doesn't drift further off)
- **MarkSuccess:** `failures = 0; lastDecay = now`

`Burst+1` cap ensures retryAfter is bounded — if we let failures grow unboundedly (e.g., MarkFailure on a hot loop), retryAfter would grow proportionally. Capping at Burst+1 means worst-case retryAfter ≈ `ceil(2/r)` ≈ 2 minutes at default config.

## 4. Limiter API

```go
// internal/auth/ratelimit/ratelimit.go

// Limiter is the operator-facing handle. Two internal maps (per-IP, per-user)
// guarded by a single mutex. A nil *Limiter is a no-op (disabled mode).
type Limiter struct {
    cfg     Config
    mu      sync.Mutex
    perIP   map[string]*bucket
    perUser map[string]*bucket
    stop    chan struct{}
    wg      sync.WaitGroup
}

// NewLimiter constructs a Limiter and starts the background sweep goroutine.
// Close stops the sweep.
func NewLimiter(cfg Config) *Limiter
func (l *Limiter) Close()

// Check returns (allowed, retryAfter). When allowed=false, retryAfter is
// time until at least one bucket has room for one more failure (rounded
// UP to a whole second for the Retry-After header). Always (true, 0) when
// l == nil. user may be empty (SSH pre-resolution path); only the IP
// bucket is consulted in that case.
//
// Check does NOT increment failure counters — only MarkFailure does. The
// gate is "is the bucket already over its limit?", evaluated against the
// current decayed state.
func (l *Limiter) Check(ip, user string) (allowed bool, retryAfter time.Duration)

// MarkFailure adds 1 to the IP bucket (always) and the user bucket (when
// user != ""). Caps at Burst+1 so even tight loops can't push retryAfter
// unboundedly large.
func (l *Limiter) MarkFailure(ip, user string)

// MarkSuccess resets the IP bucket to 0 failures. If user != "", also
// resets the user bucket. Good behavior earns full quota back immediately.
func (l *Limiter) MarkSuccess(ip, user string)

// LimitedBucket is returned to callers that want to know WHICH bucket
// tripped (for the metric label).
type LimitedBucket int
const (
    BucketNone LimitedBucket = iota
    BucketIP
    BucketUser
)

// CheckDetailed is like Check but also returns which bucket tripped (for
// outcome labeling). When allowed=true, returns BucketNone.
func (l *Limiter) CheckDetailed(ip, user string) (allowed bool, retryAfter time.Duration, which LimitedBucket)
```

The gateway uses `CheckDetailed` to set the metric outcome label (`limited_ip` vs `limited_user`). SSH uses `Check` since the user is empty.

## 5. Config

```go
// internal/auth/ratelimit/config.go

type Config struct {
    // Burst is the max failure count a single (IP or user) bucket can hold
    // before Check rejects. Default 10.
    Burst int

    // RefillPerMinute is the rate at which failures decay when idle.
    // Default 1 (= one failure cleared per minute). Full recovery from
    // burst exhaustion takes Burst/RefillPerMinute minutes when idle.
    RefillPerMinute float64

    // SweepInterval is how often the background goroutine evicts buckets
    // that have decayed to zero failures. Default 5 minutes.
    SweepInterval time.Duration

    // TrustProxyHeaders honors the first hop of X-Forwarded-For when set.
    // Operators behind a proxy MUST enable this; operators NOT behind a
    // proxy MUST NOT enable it (X-F-F is client-spoofable otherwise).
    // Default false.
    TrustProxyHeaders bool

    // Now is the time source. Defaults to time.Now. Injected for tests.
    Now func() time.Time
}

func DefaultConfig() Config {
    return Config{
        Burst:             10,
        RefillPerMinute:   1,
        SweepInterval:     5 * time.Minute,
        TrustProxyHeaders: false,
        Now:               time.Now,
    }
}
```

## 6. Operator CLI flags

`bucketvcs serve` gains four flags:

```
--auth-rate-limit-burst=10               Max failures before throttling (default 10)
--auth-rate-limit-refill-per-minute=1    Failures cleared per minute (default 1)
--trust-proxy-headers=false              Honor X-Forwarded-For first hop (default false)
--auth-rate-limit-disabled=false         Disable rate-limiting entirely (default false)
```

When `--auth-rate-limit-disabled=true`, the Limiter is constructed as `nil` and every integration point is a no-op. This is the operator escape hatch for environments running their own external rate limiter (nginx, Cloudflare, etc.).

## 7. IP extraction (`internal/gateway/ip.go`)

```go
// ClientIP returns the request's client IP. When trustProxyHeaders is set,
// the FIRST entry of X-Forwarded-For takes precedence over RemoteAddr.
// When false, RemoteAddr.Host is used unconditionally (defends against
// client-spoofed X-F-F when not behind a proxy).
func ClientIP(r *http.Request, trustProxyHeaders bool) string {
    if trustProxyHeaders {
        if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
            // First hop is the original client; subsequent hops are proxies.
            if i := strings.IndexByte(xff, ','); i > 0 {
                return strings.TrimSpace(xff[:i])
            }
            return strings.TrimSpace(xff)
        }
    }
    host, _, _ := net.SplitHostPort(r.RemoteAddr)
    if host == "" {
        // RemoteAddr lacked a port (e.g., test code) — return as-is.
        return r.RemoteAddr
    }
    return host
}
```

## 8. Observability

### 8.1 Metric

New `auth_ratelimit_total{outcome}` counter (slog-emitted):
- `allowed` — Check returned true
- `limited_ip` — Check returned false because IP bucket was over Burst
- `limited_user` — Check returned false because user bucket was over Burst
- `failure_counted` — MarkFailure was called (one per counted credential failure)
- `success_reset` — MarkSuccess was called

### 8.2 Audit

`auth.ratelimit.hit` (slog WARN):

| Attr | Value |
|---|---|
| `ip` | The client IP (possibly empty for unparseable RemoteAddr) |
| `user` | The username if known; empty for SSH pre-resolution or anon HTTPS |
| `bucket` | `"ip"` or `"user"` — which bucket tripped |
| `retry_after_sec` | The Retry-After value in seconds |
| `transport` | `"https"` or `"ssh"` |

## 9. Failure modes

| Failure | Behavior |
|---|---|
| HTTPS: limit hit | 429 + `Retry-After: <N>` header + body `"rate limited; retry after <N>s"` + audit |
| SSH: limit hit | `PublicKeyCallback` returns an error; connection drops. No Retry-After (SSH protocol limitation). Audit fires. |
| Limiter is nil | All `Check`/`MarkFailure`/`MarkSuccess` calls short-circuit; auth path runs as today |
| `--trust-proxy-headers=false` + client sends X-F-F | Header ignored; rate-limit keyed on `RemoteAddr` |
| `--trust-proxy-headers=true` + no proxy (operator misconfig) | Honors any X-F-F a client sends → spoofable; documented in operator guide as a deployment foot-gun |
| Bucket map grows unboundedly | Periodic sweep (every `SweepInterval`) removes buckets at `failures <= 0` after decay |
| `Burst=0` (operator misconfig) | Treated as Limiter disabled; documented as the wrong way (use `--auth-rate-limit-disabled=true` instead). For safety, `NewLimiter` rejects `Burst < 1` with `ErrInvalidConfig` and falls back to defaults with a stderr warning. |
| `RefillPerMinute <= 0` | Same treatment as `Burst=0`: warn + default fallback |
| Limit hit during a tight retry loop | retryAfter caps at ~2 minutes (Burst+1 / RefillPerMinute math) — guarantees bounded wait time |
| Concurrent Check + MarkFailure same key | Mutex serializes; one ordering wins. Caller sees the bucket state at moment of their call. |

## 10. Testing

### 10.1 Unit (`internal/auth/ratelimit/ratelimit_test.go`)

- **Token bucket math:** failure decay over simulated time (inject Now); MarkFailure caps at Burst+1; MarkSuccess resets to 0
- **Check semantics:** allowed when failures < Burst; rejected with retryAfter when failures >= Burst; retryAfter cap ≈ 2 minutes at default config
- **Per-IP isolation:** 10 failures from IP A don't affect IP B's bucket
- **Per-user isolation:** 10 failures for user X don't affect user Y
- **Both buckets must allow:** IP at 10 with user at 5 → rejected (IP); IP at 5 with user at 10 → rejected (user)
- **MarkSuccess resets both:** after MarkSuccess(ip, user), both ip and user buckets are at 0
- **Empty user:** Check(ip, "") only consults the IP bucket; MarkFailure(ip, "") only increments IP
- **Nil Limiter:** all methods return safe defaults
- **CheckDetailed which-bucket label:** correctly identifies which bucket tripped
- **Sweep removes idle buckets:** after sweep interval with no activity, idle buckets are GC'd
- **Concurrent access:** 100 goroutines hammer Check + MarkFailure + MarkSuccess concurrently; no data race (run with `-race`)

### 10.2 Integration (gateway)

- HTTPS: a fake authstore returns ErrInvalidCredential. 11 consecutive requests from same IP+user → 11th returns 429 with `Retry-After` header populated. Audit + metric emitted.
- HTTPS: bucketvcs serves a public-read repo. 11 anonymous requests for it succeed (anon doesn't increment rate-limit).
- HTTPS: ErrInsufficientScope rejects requests but does NOT count toward the rate limit (after 20 such rejections, a 21st request with the wrong scope still gets 403 not 429).

### 10.3 Integration (sshd)

- 11 consecutive SSH key attempts with unknown keys from same IP → 11th gets connection drop. AuthLog callback observes the rate-limit reject before the key lookup.

### 10.4 Smoke

`scripts/m18-rate-limit-smoke.sh`:
1. Start `bucketvcs serve` with `--auth-rate-limit-burst=3 --auth-rate-limit-refill-per-minute=60` (low burst + fast refill for smoke-friendly timing)
2. Make 4 consecutive HTTPS requests with bad creds (curl with Basic header)
3. Assert 4th returns 429 with `Retry-After: 1` header (1 failure / minute = 60 failures / hour decay → ~1s for one slot to free)
4. Wait 2 seconds, retry → succeeds (still 401 for bad creds, but NOT 429)
5. Assert `auth.ratelimit.hit` audit appears in serve log
6. Ends `M18_RATE_LIMIT_SMOKE_OK`

## 11. Acceptance criteria

- 5 unit-test categories all pass
- gateway + sshd integration tests pass
- `scripts/m18-rate-limit-smoke.sh` passes
- All prior smokes still pass (rate-limit is a new optional layer; defaults don't trip on normal traffic)
- `bucketvcs serve --help` shows the 4 new flags
- Operator guide section covers: defaults, when to set `--trust-proxy-headers`, recommended values for high-traffic deployments
- `auth_ratelimit_total{outcome=*}` metric and `auth.ratelimit.hit` audit emit at documented sites

## 12. Open questions

None — all decisions captured above.
