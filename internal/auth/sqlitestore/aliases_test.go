package sqlitestore

import (
	"context"
	"testing"
)

func TestResolveAlias_HitMiss(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "app"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ResolveAlias(ctx, "acme", "old"); err != nil || ok {
		t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
	}
	if err := s.insertAliasForTest(ctx, "acme", "old", "app"); err != nil {
		t.Fatal(err)
	}
	target, ok, err := s.ResolveAlias(ctx, "acme", "old")
	if err != nil || !ok || target != "app" {
		t.Fatalf("resolve: target=%q ok=%v err=%v", target, ok, err)
	}
}

func TestListAliases(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "app")
	_ = s.insertAliasForTest(ctx, "acme", "old1", "app")
	_ = s.insertAliasForTest(ctx, "acme", "old2", "app")
	_ = s.insertAliasForTest(ctx, "acme", "other", "different")
	got, err := s.ListAliases(ctx, "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 aliases targeting app, got %d: %+v", len(got), got)
	}
}

func TestRegisterRepo_ShadowsAlias(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "a")
	_ = s.RenameRepo(ctx, "acme", "a", "b") // alias a->b
	if err := s.RegisterRepo(ctx, "acme", "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ResolveAlias(ctx, "acme", "a"); ok {
		t.Fatal("registering a real repo named 'a' must drop the 'a' alias")
	}
}

func TestRegisterRepoIfNew_ShadowsAlias(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "a")
	_ = s.RenameRepo(ctx, "acme", "a", "b") // alias a->b
	inserted, err := s.RegisterRepoIfNew(ctx, "acme", "a")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("expected inserted=true for re-created name")
	}
	if _, ok, _ := s.ResolveAlias(ctx, "acme", "a"); ok {
		t.Fatal("RegisterRepoIfNew must also drop the alias")
	}
}

func TestDeleteRepoCascade_CleansAliases(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "a")
	_ = s.RenameRepo(ctx, "acme", "a", "b") // alias a->b, live repo b
	if err := s.DeleteRepoCascade(ctx, "acme", "b"); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	if _, ok, _ := s.ResolveAlias(ctx, "acme", "a"); ok {
		t.Fatal("deleting target repo must remove aliases pointing at it")
	}
}
