package sqlitestore

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func names(repos []*Repo) map[string]bool {
	m := map[string]bool{}
	for _, r := range repos {
		m[r.Tenant+"/"+r.Name] = true
	}
	return m
}

func TestListAccessibleRepos(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	for _, n := range []string{"pub1", "pub2", "priv1", "priv2"} {
		if err := s.RegisterRepo(ctx, "acme", n); err != nil {
			t.Fatalf("RegisterRepo %s: %v", n, err)
		}
	}
	if err := s.SetRepoPublic(ctx, "acme", "pub1", true); err != nil {
		t.Fatalf("public pub1: %v", err)
	}
	if err := s.SetRepoPublic(ctx, "acme", "pub2", true); err != nil {
		t.Fatalf("public pub2: %v", err)
	}

	uid, _ := s.CreateUser(ctx, "alice", false)
	if err := s.Grant(ctx, "alice", "acme", "priv1", "write"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	alice := &auth.Actor{UserID: uid, Name: "alice"}

	// anon: public only
	anon, err := s.ListAccessibleRepos(ctx, nil)
	if err != nil {
		t.Fatalf("anon: %v", err)
	}
	got := names(anon)
	if !got["acme/pub1"] || !got["acme/pub2"] || got["acme/priv1"] || got["acme/priv2"] {
		t.Fatalf("anon visibility wrong: %v", got)
	}

	// alice: public + granted priv1, not priv2
	av, err := s.ListAccessibleRepos(ctx, alice)
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	got = names(av)
	if !got["acme/pub1"] || !got["acme/priv1"] || got["acme/priv2"] {
		t.Fatalf("alice visibility wrong: %v", got)
	}

	// admin: everything
	admin := &auth.Actor{UserID: "x", Name: "root", IsAdmin: true}
	adv, err := s.ListAccessibleRepos(ctx, admin)
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	if len(adv) != 4 {
		t.Fatalf("admin sees %d, want 4", len(adv))
	}
}
