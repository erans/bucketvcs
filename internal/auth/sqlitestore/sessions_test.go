package sqlitestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestSessionLifecycle(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "alice", true)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	raw, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(raw) < 32 {
		t.Fatalf("session id too short: %q", raw)
	}

	got, err := s.LookupSession(ctx, raw)
	if err != nil {
		t.Fatalf("LookupSession: %v", err)
	}
	if got.UserID != uid || got.Name != "alice" || !got.IsAdmin || got.Provider != "password" {
		t.Fatalf("session = %+v", got)
	}

	// unknown id
	if _, err := s.LookupSession(ctx, "does-not-exist"); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("unknown id: want ErrNoSession, got %v", err)
	}

	// delete (logout)
	if err := s.DeleteSession(ctx, raw); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.LookupSession(ctx, raw); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("after delete: want ErrNoSession, got %v", err)
	}
}

func TestLookupSession_RejectsDisabledUser(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	// Two admins so disabling one does not trip the last-admin guard.
	if _, err := s.CreateUser(ctx, "keeper", true); err != nil {
		t.Fatalf("CreateUser keeper: %v", err)
	}
	uid, err := s.CreateUser(ctx, "dave", true)
	if err != nil {
		t.Fatalf("CreateUser dave: %v", err)
	}
	if err := s.SetPassword(ctx, "dave", "pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	raw, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Sanity: the session works while the user is enabled.
	if _, err := s.LookupSession(ctx, raw); err != nil {
		t.Fatalf("LookupSession (enabled): %v", err)
	}

	if err := s.SetUserDisabled(ctx, "dave", true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}

	// A disabled user's session must no longer resolve.
	if _, err := s.LookupSession(ctx, raw); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("disabled-user lookup: want ErrNoSession, got %v", err)
	}
}

func TestDeleteSessionsForUser(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	uid, err := s.CreateUser(ctx, "alice", true)
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	otherUID, err := s.CreateUser(ctx, "mallory", false)
	if err != nil {
		t.Fatalf("CreateUser mallory: %v", err)
	}

	raw1, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}
	raw2, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}
	raw3, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession 3: %v", err)
	}
	otherRaw, err := s.CreateSession(ctx, otherUID, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession other: %v", err)
	}

	// Delete alice's other sessions, keeping raw2 (the "current" one).
	n, err := s.DeleteSessionsForUser(ctx, uid, raw2)
	if err != nil {
		t.Fatalf("DeleteSessionsForUser: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d sessions, want 2", n)
	}

	// raw2 survives.
	if _, err := s.LookupSession(ctx, raw2); err != nil {
		t.Fatalf("LookupSession raw2 (kept): %v", err)
	}
	// raw1 + raw3 are gone.
	if _, err := s.LookupSession(ctx, raw1); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("LookupSession raw1: want ErrNoSession, got %v", err)
	}
	if _, err := s.LookupSession(ctx, raw3); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("LookupSession raw3: want ErrNoSession, got %v", err)
	}
	// The other user's session is untouched.
	if _, err := s.LookupSession(ctx, otherRaw); err != nil {
		t.Fatalf("LookupSession other user: %v", err)
	}

	// exceptRawID == "" deletes all remaining sessions for the user.
	n, err = s.DeleteSessionsForUser(ctx, uid, "")
	if err != nil {
		t.Fatalf("DeleteSessionsForUser (all): %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d sessions, want 1", n)
	}
	if _, err := s.LookupSession(ctx, raw2); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("LookupSession raw2 after delete-all: want ErrNoSession, got %v", err)
	}
	if _, err := s.LookupSession(ctx, otherRaw); err != nil {
		t.Fatalf("LookupSession other user after delete-all: %v", err)
	}
}

func TestSessionExpiryAndSweep(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "bob", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// already-expired session (negative TTL)
	raw, err := s.CreateSession(ctx, uid, "password", -time.Minute)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.LookupSession(ctx, raw); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("expired lookup: want ErrNoSession, got %v", err)
	}

	n, err := s.SweepExpiredSessions(ctx, time.Now())
	if err != nil {
		t.Fatalf("SweepExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
}

func TestTouchSession_SlidesAndThrottles(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	uid, err := s.CreateUser(ctx, "carol", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	raw, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// --- assertion 1: no-op within the minute ---
	// last_seen was just set to "now" at insert, so last_seen <= now-60 is false.
	before, err := s.LookupSession(ctx, raw)
	if err != nil {
		t.Fatalf("LookupSession (before touch): %v", err)
	}

	if err := s.TouchSession(ctx, raw, 2*time.Hour); err != nil {
		t.Fatalf("TouchSession (throttled): %v", err)
	}

	afterThrottle, err := s.LookupSession(ctx, raw)
	if err != nil {
		t.Fatalf("LookupSession (after throttled touch): %v", err)
	}
	if afterThrottle.ExpiresAt.Unix() != before.ExpiresAt.Unix() {
		t.Fatalf("throttle guard failed: ExpiresAt changed from %v to %v (want no change)",
			before.ExpiresAt, afterThrottle.ExpiresAt)
	}

	// --- assertion 2: slides when last_seen is old ---
	// Force last_seen 120 seconds into the past so the guard allows an update.
	idh := hashSessionID(raw)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen = ? WHERE id_hash = ?`,
		time.Now().Unix()-120, idh); err != nil {
		t.Fatalf("age last_seen: %v", err)
	}

	if err := s.TouchSession(ctx, raw, 2*time.Hour); err != nil {
		t.Fatalf("TouchSession (slide): %v", err)
	}

	afterSlide, err := s.LookupSession(ctx, raw)
	if err != nil {
		t.Fatalf("LookupSession (after slide touch): %v", err)
	}
	// New ExpiresAt should be ~now+2h; original was ~now+1h.
	// Require at least original+50min to tolerate second-boundary races.
	minExpected := before.ExpiresAt.Add(50 * time.Minute)
	if !afterSlide.ExpiresAt.After(minExpected) {
		t.Fatalf("ExpiresAt did not slide: got %v, want > %v", afterSlide.ExpiresAt, minExpected)
	}
}
