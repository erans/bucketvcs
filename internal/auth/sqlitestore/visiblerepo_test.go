package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestGetVisibleRepo(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "pub"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterRepo(ctx, "acme", "priv"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoPublic(ctx, "acme", "pub", true); err != nil {
		t.Fatal(err)
	}

	// Anonymous: sees public, not private.
	if _, err := s.GetVisibleRepo(ctx, nil, "acme", "pub"); err != nil {
		t.Fatalf("anon pub: %v", err)
	}
	if _, err := s.GetVisibleRepo(ctx, nil, "acme", "priv"); !errors.Is(err, ErrRepoNotVisible) {
		t.Fatalf("anon priv: want ErrRepoNotVisible, got %v", err)
	}
	// Missing repo: not visible.
	if _, err := s.GetVisibleRepo(ctx, nil, "acme", "ghost"); !errors.Is(err, ErrRepoNotVisible) {
		t.Fatalf("missing: want ErrRepoNotVisible, got %v", err)
	}
	// Admin: sees private.
	admin := &auth.Actor{UserID: "admin1", IsAdmin: true}
	if _, err := s.GetVisibleRepo(ctx, admin, "acme", "priv"); err != nil {
		t.Fatalf("admin priv: %v", err)
	}
	// Granted user: sees the private repo they hold a grant on.
	uid, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.Grant(ctx, "alice", "acme", "priv", "write"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	alice := &auth.Actor{UserID: uid, Name: "alice"}
	if _, err := s.GetVisibleRepo(ctx, alice, "acme", "priv"); err != nil {
		t.Fatalf("granted alice priv: %v", err)
	}
	// Ungranted user: cannot see the private repo.
	bobUID, _ := s.CreateUser(ctx, "bob", false)
	bob := &auth.Actor{UserID: bobUID, Name: "bob"}
	if _, err := s.GetVisibleRepo(ctx, bob, "acme", "priv"); !errors.Is(err, ErrRepoNotVisible) {
		t.Fatalf("ungranted bob priv: want ErrRepoNotVisible, got %v", err)
	}
}
