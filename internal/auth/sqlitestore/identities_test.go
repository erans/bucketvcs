package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestSetEmailAndFindByEmail(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := s.SetEmail(ctx, "alice", "Alice@Corp.com"); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}
	got, err := s.FindUserByEmail(ctx, "alice@corp.com")
	if err != nil {
		t.Fatalf("FindUserByEmail: %v", err)
	}
	if got.UserID != uid || got.Name != "alice" {
		t.Fatalf("actor = %+v", got)
	}
	if _, err := s.FindUserByEmail(ctx, "nobody@corp.com"); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Fatalf("unknown email: want ErrNoSuchUser, got %v", err)
	}
}

func TestSetEmail_DupAndUnknownAndClearAndDisabled(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	if _, err := s.CreateUser(ctx, "bob", false); err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}
	if err := s.SetEmail(ctx, "alice", "shared@corp.com"); err != nil {
		t.Fatalf("SetEmail alice: %v", err)
	}
	if err := s.SetEmail(ctx, "bob", "shared@corp.com"); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("dup email: want ErrConflict, got %v", err)
	}
	if err := s.SetEmail(ctx, "ghost", "x@y.com"); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Fatalf("unknown user: want ErrNoSuchUser, got %v", err)
	}
	if err := s.SetEmail(ctx, "alice", ""); err != nil {
		t.Fatalf("clear email: %v", err)
	}
	if _, err := s.FindUserByEmail(ctx, "shared@corp.com"); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Fatalf("after clear: want ErrNoSuchUser, got %v", err)
	}
	if err := s.SetEmail(ctx, "bob", "bob@corp.com"); err != nil {
		t.Fatalf("SetEmail bob: %v", err)
	}
	if err := s.SetUserDisabled(ctx, "bob", true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if _, err := s.FindUserByEmail(ctx, "bob@corp.com"); !errors.Is(err, auth.ErrUserDisabled) {
		t.Fatalf("disabled: want ErrUserDisabled, got %v", err)
	}
}
