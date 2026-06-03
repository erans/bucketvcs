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

func TestIdentityLinkFindListRemove(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	const iss, sub = "https://idp.example.com", "subject-123"

	if _, err := s.FindIdentity(ctx, iss, sub); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Fatalf("unlinked: want ErrNoSuchUser, got %v", err)
	}
	if err := s.LinkIdentity(ctx, uid, iss, sub, "alice@corp.com"); err != nil {
		t.Fatalf("LinkIdentity: %v", err)
	}
	got, err := s.FindIdentity(ctx, iss, sub)
	if err != nil {
		t.Fatalf("FindIdentity: %v", err)
	}
	if got.UserID != uid || got.Name != "alice" {
		t.Fatalf("actor = %+v", got)
	}
	uid2, _ := s.CreateUser(ctx, "bob", false)
	if err := s.LinkIdentity(ctx, uid2, iss, sub, "bob@corp.com"); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("dup identity: want ErrConflict, got %v", err)
	}
	ids, err := s.ListIdentities(ctx, "alice")
	if err != nil || len(ids) != 1 || ids[0].Issuer != iss || ids[0].Subject != sub {
		t.Fatalf("ListIdentities = %+v, err %v", ids, err)
	}
	if err := s.RemoveIdentity(ctx, iss, sub); err != nil {
		t.Fatalf("RemoveIdentity: %v", err)
	}
	if _, err := s.FindIdentity(ctx, iss, sub); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Fatalf("after remove: want ErrNoSuchUser, got %v", err)
	}
}

func TestIdentity_CascadeOnUserDelete(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "carol", false)
	if err := s.LinkIdentity(ctx, uid, "https://i", "s", "c@x.com"); err != nil {
		t.Fatalf("LinkIdentity: %v", err)
	}
	if err := s.DeleteUser(ctx, "carol"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.FindIdentity(ctx, "https://i", "s"); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Fatalf("after user delete: want ErrNoSuchUser, got %v", err)
	}
}
