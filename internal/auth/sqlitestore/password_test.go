package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestSetAndVerifyPassword(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := s.SetPassword(ctx, "alice", "correct horse battery"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	got, err := s.VerifyPassword(ctx, "alice", "correct horse battery")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if got.Name != "alice" || got.UserID == "" || got.IsAdmin {
		t.Fatalf("actor = %+v", got)
	}

	if _, err := s.VerifyPassword(ctx, "alice", "wrong"); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("wrong password: want ErrInvalidCredential, got %v", err)
	}
}

func TestVerifyPassword_NoHashOrUnknownOrDisabled(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	// unknown user
	if _, err := s.VerifyPassword(ctx, "ghost", "x"); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("unknown: want ErrInvalidCredential, got %v", err)
	}

	// user with no password set
	if _, err := s.CreateUser(ctx, "nopw", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := s.VerifyPassword(ctx, "nopw", "x"); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("no-hash: want ErrInvalidCredential, got %v", err)
	}

	// disabled user with a password
	if _, err := s.CreateUser(ctx, "bob", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.SetPassword(ctx, "bob", "pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if err := s.SetUserDisabled(ctx, "bob", true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if _, err := s.VerifyPassword(ctx, "bob", "pw"); !errors.Is(err, auth.ErrUserDisabled) {
		t.Fatalf("disabled: want ErrUserDisabled, got %v", err)
	}
}

func TestSetPassword_UnknownUser(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	if err := s.SetPassword(context.Background(), "ghost", "x"); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Fatalf("want ErrNoSuchUser, got %v", err)
	}
}
