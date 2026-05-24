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

func TestLimiter_UserParamDoesNotEnableCrossIPLockout(t *testing.T) {
	// Regression: an earlier design had a cross-IP per-user bucket that an
	// attacker could weaponise to lock out a victim user by hammering the
	// known username from a botnet. The bucket was removed; the user
	// parameter is now accepted for audit attribution but does NOT gate.
	// This test pins that behavior.
	_, now := fakeClock()
	l := newLimiter(t, 3, 0, now) // refill disabled — failures persist
	// 10 failures against the same user from 10 distinct IPs.
	for i := 0; i < 10; i++ {
		ip := "ip" + string(rune('a'+i))
		l.MarkFailure(ip, "victim")
	}
	// An 11th IP attempting auth as the victim must STILL be allowed:
	// the per-user bucket would have tripped at attempt 4, locking the
	// real victim out. The IP-only design does not lock out by user.
	allowed, _, which := l.CheckDetailed("ip-fresh", "victim")
	if !allowed {
		t.Errorf("cross-IP per-user lockout regression: ip-fresh blocked because user=victim has been hammered elsewhere (which=%v)", which)
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
