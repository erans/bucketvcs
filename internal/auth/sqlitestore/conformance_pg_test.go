//go:build postgres

package sqlitestore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func openPostgres(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("BUCKETVCS_POSTGRES_URL")
	if url == "" {
		t.Skip("BUCKETVCS_POSTGRES_URL not set")
	}
	s, err := Open(url)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.backend.Name() != "postgres" {
		t.Fatalf("backend=%s, want postgres", s.backend.Name())
	}
	return s
}

func TestPostgresConformance(t *testing.T) {
	s := openPostgres(t)
	ctx := context.Background()

	if _, err := s.GetUserByName(ctx, "_oidc"); err != nil {
		t.Fatalf("migrations did not apply (no _oidc user): %v", err)
	}
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.CreateUser(ctx, "alice", false); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("dup user: want ErrConflict, got %v", err)
	}
	if err := s.RegisterRepo(ctx, "acme", "web"); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	u, err := s.GetUserByName(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Grant(ctx, "alice", "acme", "web", "write"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	actor := &auth.Actor{UserID: u.ID, Name: "alice"}
	if perm, err := s.LookupRepoPerm(ctx, actor, "acme", "web"); err != nil || perm != auth.PermWrite {
		t.Fatalf("perm=%v err=%v want write", perm, err)
	}

	tok, id, secret, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateToken(ctx, id, u.ID, hash, "lap", nil, auth.ScopeRepoWrite, "", "", ""); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if gotActor, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok}); err != nil || gotActor == nil || gotActor.Name != "alice" {
		t.Fatalf("verify: actor=%v err=%v", gotActor, err)
	}

	// CHECK enforcement: scope_perm CHECK on tokens (migration 0010) → must be
	// classified by the postgres SQLSTATE matcher.
	exp := time.Now().Unix() + 900
	err = s.CreateToken(ctx, "BADPERMTOKEN0000000000AA", "_oidc", hash, "x", &exp,
		auth.ScopeRepoRead, "acme", "web", "BOGUS")
	if err == nil {
		t.Fatal("CHECK on scope_perm should reject 'BOGUS'")
	}
	if !s.backend.IsCheckViolation(err) {
		t.Fatalf("postgres CHECK error not matched by IsCheckViolation: %v", err)
	}

	// OIDC mint round-trips.
	mint, err := s.MintOIDCToken(ctx, MintOIDCParams{
		Tenant: "acme", Repo: "web", Perm: auth.PermWrite,
		Scopes: auth.ScopeRepoWrite, TTLSeconds: 900, Label: "oidc:gh:sub",
	})
	if err != nil {
		t.Fatalf("mint oidc: %v", err)
	}
	if _, _, scope, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "x", Password: mint}); err != nil || scope == nil || scope.Repo != "web" {
		t.Fatalf("verify minted: scope=%v err=%v", scope, err)
	}

	// FK cascade: deleting the repo removes its permission rows.
	if err := s.DeleteRepo(ctx, "acme", "web"); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	if perm, _ := s.LookupRepoPerm(ctx, actor, "acme", "web"); perm != auth.PermNone {
		t.Fatalf("after repo delete, perm=%v want none (cascade)", perm)
	}

	// Rename works single-node on postgres (deferred FKs).
	if err := s.RegisterRepo(ctx, "acme", "old"); err != nil {
		t.Fatalf("register old: %v", err)
	}
	if err := s.Grant(ctx, "alice", "acme", "old", "write"); err != nil {
		t.Fatalf("grant old: %v", err)
	}
	if err := s.RenameRepo(ctx, "acme", "old", "new"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if perm, _ := s.LookupRepoPerm(ctx, actor, "acme", "new"); perm != auth.PermWrite {
		t.Fatalf("after rename, perm on new=%v want write", perm)
	}
}
