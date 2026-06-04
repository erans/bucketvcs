package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestListRepoGrants_SortedByUserName(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	if err := s.RegisterRepo(ctx, "acme", "demo"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	if _, err := s.CreateUser(ctx, "bob", false); err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	if err := s.Grant(ctx, "bob", "acme", "demo", "write"); err != nil {
		t.Fatalf("Grant bob: %v", err)
	}
	if err := s.Grant(ctx, "alice", "acme", "demo", "admin"); err != nil {
		t.Fatalf("Grant alice: %v", err)
	}

	got, err := s.ListRepoGrants(ctx, "acme", "demo")
	if err != nil {
		t.Fatalf("ListRepoGrants: %v", err)
	}
	want := []RepoGrant{
		{UserName: "alice", Perm: "admin"},
		{UserName: "bob", Perm: "write"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d grants, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("grant[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestListRepoGrants_UnknownRepo(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	_, err := s.ListRepoGrants(ctx, "acme", "nope")
	if !errors.Is(err, auth.ErrNoSuchRepo) {
		t.Fatalf("ListRepoGrants unknown repo: err = %v, want ErrNoSuchRepo", err)
	}
}

func TestListRepoGrants_NoGrants(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	if err := s.RegisterRepo(ctx, "acme", "demo"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	got, err := s.ListRepoGrants(ctx, "acme", "demo")
	if err != nil {
		t.Fatalf("ListRepoGrants: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d grants, want 0: %+v", len(got), got)
	}
}
