package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/web"
)

func TestWebAdapter_IdentityMethods(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlitestore.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	if err := s.SetEmail(ctx, "alice", "alice@corp.com"); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}

	var ds web.DataStore = newWebAdapter(s)

	a, err := ds.FindUserByEmail(ctx, "alice@corp.com")
	if err != nil || a.UserID != uid {
		t.Fatalf("FindUserByEmail: %+v err %v", a, err)
	}
	if err := ds.LinkIdentity(ctx, uid, "https://i", "sub1", "alice@corp.com"); err != nil {
		t.Fatalf("LinkIdentity: %v", err)
	}
	a2, err := ds.FindIdentity(ctx, "https://i", "sub1")
	if err != nil || a2.UserID != uid {
		t.Fatalf("FindIdentity: %+v err %v", a2, err)
	}
}

func TestWebAdapter_Satisfies(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlitestore.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.RegisterRepo(ctx, "acme", "demo"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	if err := s.SetRepoPublic(ctx, "acme", "demo", true); err != nil {
		t.Fatalf("SetRepoPublic: %v", err)
	}

	var ds web.DataStore = newWebAdapter(s) // must satisfy the interface
	repos, err := ds.ListAccessibleRepos(ctx, (*auth.Actor)(nil))
	if err != nil {
		t.Fatalf("ListAccessibleRepos: %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "demo" || !repos[0].PublicRead {
		t.Fatalf("repos = %+v", repos)
	}
}
