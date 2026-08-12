package ratelimit

import (
	"testing"
	"time"
)

func newSweepTestLimiter(t *testing.T, refill float64, now *time.Time) *Limiter {
	t.Helper()
	l := NewLimiter(Config{
		Burst:           3,
		RefillPerMinute: refill,
		SweepInterval:   5 * time.Minute,
		Now:             func() time.Time { return *now },
	})
	t.Cleanup(l.Close)
	return l
}

func TestSweep_NoDecayLoadedBucketPersistsUntilSuccess_U12(t *testing.T) {
	now := time.Now()
	l := newSweepTestLimiter(t, 0, &now)
	for i := 0; i < 3; i++ {
		l.MarkFailure("ip", "alice")
	}
	if allowed, _ := l.Check("ip", "alice"); allowed {
		t.Fatal("precondition: loaded bucket allowed")
	}

	now = now.Add(11 * time.Minute) // older than 2*sweep interval
	l.sweepOnce()
	if allowed, _ := l.Check("ip", "alice"); allowed {
		t.Fatal("refill-disabled loaded bucket was evicted by idle sweep")
	}

	l.MarkSuccess("ip", "alice")
	if allowed, _ := l.Check("ip", "alice"); !allowed {
		t.Fatal("MarkSuccess did not clear refill-disabled bucket")
	}
}

func TestSweep_RefillModeStillEvictsIdleLoadedBucket_U12(t *testing.T) {
	now := time.Now()
	l := newSweepTestLimiter(t, 0.01, &now)
	for i := 0; i < 3; i++ {
		l.MarkFailure("ip", "alice")
	}
	now = now.Add(11 * time.Minute) // only 0.11 failures decay; age eviction is decisive
	l.sweepOnce()
	if allowed, _ := l.Check("ip", "alice"); !allowed {
		t.Fatal("normal refill mode no longer evicts an idle loaded bucket")
	}
}
