package sqlitestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestCreateTokenWithScopes(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	uid, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	scopes := auth.ScopeRepoRead | auth.ScopeLFSRead
	if err := s.CreateToken(ctx, "tokid001AAAAAAAAAAAAAAAA", uid, "$argon2id$h1", "label", nil, scopes); err != nil {
		t.Fatalf("CreateToken with scopes: %v", err)
	}
	got, err := s.GetTokenByID(ctx, "tokid001AAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatalf("GetTokenByID: %v", err)
	}
	if got.Scopes != scopes {
		t.Errorf("Token.Scopes = %d, want %d", got.Scopes, scopes)
	}
}

func TestCreateTokenLegacyScopes(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.CreateToken(ctx, "tokid001AAAAAAAAAAAAAAAA", uid, "$argon2id$h1", "label", nil, auth.ScopeLegacy); err != nil {
		t.Fatalf("CreateToken with legacy scopes: %v", err)
	}
	got, err := s.GetTokenByID(ctx, "tokid001AAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatalf("GetTokenByID: %v", err)
	}
	if got.Scopes != auth.ScopeLegacy {
		t.Errorf("Token.Scopes = %d, want ScopeLegacy (0)", got.Scopes)
	}
}

func TestRotateToken(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	exp := time.Now().Add(24 * time.Hour).Unix()
	if err := s.CreateToken(ctx, "tokid001AAAAAAAAAAAAAAAA", uid, "origHash", "label",
		&exp, auth.ScopeRepoWrite); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if err := s.RotateToken(ctx, "tokid001AAAAAAAAAAAAAAAA", "newHash"); err != nil {
		t.Fatalf("RotateToken: %v", err)
	}
	got, err := s.GetTokenByID(ctx, "tokid001AAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatalf("GetTokenByID: %v", err)
	}
	if got.SecretHash != "newHash" {
		t.Errorf("after rotate: SecretHash = %q, want newHash", got.SecretHash)
	}
	if got.Scopes != auth.ScopeRepoWrite {
		t.Errorf("after rotate: Scopes = %d, want ScopeRepoWrite (preserved)", got.Scopes)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != exp {
		t.Errorf("after rotate: ExpiresAt changed; got %v, want %d", got.ExpiresAt, exp)
	}
}

func TestRotateTokenNotFound(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	err := s.RotateToken(context.Background(), "nonexistentAAAAAAAAAAAAA", "newHash")
	if !errors.Is(err, auth.ErrNoSuchToken) {
		t.Errorf("RotateToken nonexistent: err=%v, want ErrNoSuchToken", err)
	}
}

func TestListTokensIncludesScopes(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.CreateToken(ctx, "tok1AAAAAAAAAAAAAAAAAAAA", uid, "h1", "a", nil, auth.ScopeRepoRead); err != nil {
		t.Fatalf("CreateToken 1: %v", err)
	}
	if err := s.CreateToken(ctx, "tok2AAAAAAAAAAAAAAAAAAAA", uid, "h2", "b", nil, auth.ScopeRepoAdmin); err != nil {
		t.Fatalf("CreateToken 2: %v", err)
	}
	list, err := s.ListTokensForUser(ctx, "alice")
	if err != nil {
		t.Fatalf("ListTokensForUser: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListTokensForUser len=%d, want 2", len(list))
	}
	byID := map[string]auth.TokenScope{}
	for _, tk := range list {
		byID[tk.ID] = tk.Scopes
	}
	if byID["tok1AAAAAAAAAAAAAAAAAAAA"] != auth.ScopeRepoRead {
		t.Errorf("tok1 scopes = %d, want ScopeRepoRead", byID["tok1AAAAAAAAAAAAAAAAAAAA"])
	}
	if byID["tok2AAAAAAAAAAAAAAAAAAAA"] != auth.ScopeRepoAdmin {
		t.Errorf("tok2 scopes = %d, want ScopeRepoAdmin", byID["tok2AAAAAAAAAAAAAAAAAAAA"])
	}
}
