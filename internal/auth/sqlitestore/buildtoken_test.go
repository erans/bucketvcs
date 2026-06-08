package sqlitestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestMintBuildToken_ScopedReadOnly(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "app"); err != nil {
		t.Fatal(err)
	}
	tok, err := s.MintBuildToken(ctx, MintBuildParams{
		Tenant:     "acme",
		Repo:       "app",
		Scopes:     auth.ScopeRepoRead | auth.ScopeLFSRead,
		TTLSeconds: 900,
		Label:      "build:acme/app:main",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	// Parse the id out of the wire-format token (bvts_<id>_<secret>).
	id, _, err := auth.ParseToken(tok)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	row, err := s.GetTokenByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.UserID != "_build" {
		t.Fatalf("UserID = %q, want _build", row.UserID)
	}
	if row.ScopeTenant != "acme" || row.ScopeRepo != "app" || row.ScopePerm != "read" {
		t.Fatalf("unexpected scope binding: tenant=%q repo=%q perm=%q", row.ScopeTenant, row.ScopeRepo, row.ScopePerm)
	}
	if row.Scopes != auth.ScopeRepoRead|auth.ScopeLFSRead {
		t.Fatalf("scopes = %v, want ScopeRepoRead|ScopeLFSRead", row.Scopes)
	}
}

func TestSweepExpiredBuildTokens_OnlyBuildUser(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "app"); err != nil {
		t.Fatal(err)
	}

	// Mint an already-expired _build token (TTLSeconds=-1 means exp = now-1).
	_, err := s.MintBuildToken(ctx, MintBuildParams{
		Tenant:     "acme",
		Repo:       "app",
		Scopes:     auth.ScopeRepoRead,
		TTLSeconds: -1,
		Label:      "build:acme/app:expired",
	})
	if err != nil {
		t.Fatalf("mint expired build token: %v", err)
	}

	// Also create an expired token for an ordinary user — it must NOT be swept.
	regularUserID, err := s.CreateUser(ctx, "regularuser", false)
	if err != nil {
		t.Fatalf("create regular user: %v", err)
	}
	_, regularTokID, regularSecret, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("generate regular token: %v", err)
	}
	regularHash, err := auth.HashSecret(regularSecret)
	if err != nil {
		t.Fatalf("hash regular secret: %v", err)
	}
	past := time.Now().Add(-time.Hour).Unix()
	if err := s.CreateToken(ctx, regularTokID, regularUserID, regularHash, "regular-expired", &past,
		auth.ScopeRepoRead, "", "", ""); err != nil {
		t.Fatalf("create regular expired token: %v", err)
	}

	n, err := s.SweepExpiredBuildTokens(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}

	// The ordinary user's expired token must still exist.
	if _, err := s.GetTokenByID(ctx, regularTokID); err != nil {
		t.Fatalf("regular expired token should still exist: %v", err)
	}
}

// TestReservedBuildUserProtected mirrors TestReservedOIDCUserProtected for "_build".
func TestReservedBuildUserProtected(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if err := s.SetUserDisabled(ctx, "_build", true); !errors.Is(err, ErrReservedUser) {
		t.Fatalf("disable _build: want ErrReservedUser, got %v", err)
	}
	if err := s.DeleteUser(ctx, "_build"); !errors.Is(err, ErrReservedUser) {
		t.Fatalf("delete _build: want ErrReservedUser, got %v", err)
	}
	// Granting repo permissions to _build is refused — the guard fires before
	// user/repo resolution, so a missing repo doesn't mask it.
	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	if err := s.Grant(ctx, "_build", "acme", "foo", "read"); !errors.Is(err, ErrReservedUser) {
		t.Fatalf("grant to _build: want ErrReservedUser, got %v", err)
	}
	// And it is hidden from ListUsers.
	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Name == "_build" {
			t.Fatal("_build must not appear in ListUsers")
		}
	}
}
