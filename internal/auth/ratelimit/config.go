package ratelimit

import "time"

// Config tunes the Limiter. See DefaultConfig for the production values.
type Config struct {
	// Burst is the max failure count a single IP bucket can hold before
	// Check rejects.
	Burst int

	// UserBurst is the max failure count a single per-account bucket can
	// hold before Check rejects, accumulating across all source IPs.
	// Deliberately higher than Burst: it must catch distributed guessing
	// while making targeted account-lockout DoS expensive. 0 disables the
	// per-user bucket (IP-only gating).
	UserBurst int

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

// DefaultConfig returns production defaults: Burst=10, UserBurst=100,
// 1 failure cleared per minute, sweep every 5 minutes.
func DefaultConfig() Config {
	return Config{
		Burst:           10,
		UserBurst:       100,
		RefillPerMinute: 1,
		SweepInterval:   5 * time.Minute,
		Now:             time.Now,
	}
}
