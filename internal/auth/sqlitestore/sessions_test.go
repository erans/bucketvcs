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

func TestSessionExpiryAndSweep(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "bob", false)

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
