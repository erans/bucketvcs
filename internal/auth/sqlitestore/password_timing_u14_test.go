package sqlitestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// Wave-4 U-14 regression coverage: before the fix, VerifyPassword returned
// fast on sql.ErrNoRows (unknown user) while the existing-user paths burned
// argon2id work — username enumeration by timing. The no-rows path now
// verifies against a fixed dummy PHC with the same auth.HashSecret cost
// params, so unknown-user and known-user-wrong-password executions carry the
// same hashing work.
//
// Coarse wall-clock equality with generous bounds (the local convention for
// cost tests, cf. stress_test.go): the unknown-user duration must be at
// least half the known-user-wrong-password duration across a few samples.
// Pre-fix the ratio is ~0 (microseconds vs ~100ms); post-fix ~1.0.

func verifyDuration(t *testing.T, s *Store, user string) time.Duration {
	t.Helper()
	ctx := context.Background()
	start := time.Now()
	if _, err := s.VerifyPassword(ctx, user, "wrong password"); err == nil {
		t.Fatalf("VerifyPassword(%q) unexpectedly succeeded", user)
	}
	return time.Since(start)
}

func TestU14_VerifyPassword_UnknownUserBurnsVerifyCost(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.SetPassword(ctx, "alice", "correct horse battery"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// Error-class pin: unknown user still collapses to ErrInvalidCredential.
	if _, err := s.VerifyPassword(ctx, "ghost", "wrong password"); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("unknown user: want ErrInvalidCredential, got %v", err)
	}

	// Timing-structure pin: both branches execute the same hashing work.
	// Median-of-3 to avoid scheduler noise on shared CI.
	durations := func(user string, n int) time.Duration {
		ds := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			ds = append(ds, verifyDuration(t, s, user))
		}
		sortDurations(ds)
		return ds[len(ds)/2]
	}
	known := durations("alice", 3)
	unknown := durations("ghost", 3)
	t.Logf("median known-user-wrong-password %s; unknown-user %s", known, unknown)
	if unknown < known/2 {
		t.Fatalf("unknown-user verify took %s vs known-user %s — no-rows path is not burning the verify cost", unknown, known)
	}
}

func sortDurations(ds []time.Duration) {
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && ds[j] < ds[j-1]; j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
}
