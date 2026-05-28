//go:build libsql

package sqlitestore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// openLibsql opens a fresh-schema libSQL store from BUCKETVCS_LIBSQL_URL.
// The CI job points this at a per-run sqld instance (empty DB).
func openLibsql(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("BUCKETVCS_LIBSQL_URL")
	if url == "" {
		t.Skip("BUCKETVCS_LIBSQL_URL not set")
	}
	s, err := Open(url)
	if err != nil {
		t.Fatalf("open libsql: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.backend.Name() != "libsql" {
		t.Fatalf("backend=%s, want libsql", s.backend.Name())
	}
	return s
}

// TestLibsqlConformance exercises the core behaviors end-to-end on libSQL and
// asserts they match the documented SQLite behavior, including the
// error-classification mapping (ErrConflict on UNIQUE, CHECK enforcement) and
// the FK cascade that Phase B's concurrency work will build on.
func TestLibsqlConformance(t *testing.T) {
	s := openLibsql(t)
	ctx := context.Background()

	// Migrations applied: the reserved _oidc user exists.
	if _, err := s.GetUserByName(ctx, "_oidc"); err != nil {
		t.Fatalf("migrations did not apply (no _oidc user): %v", err)
	}

	// User + token lifecycle.
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("create user: %v", err)
	}
	// UNIQUE on users.name → ErrConflict (proves error classification on libSQL).
	if _, err := s.CreateUser(ctx, "alice", false); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("dup user: want ErrConflict, got %v", err)
	}

	// Repo register + grant + perm lookup.
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
	perm, err := s.LookupRepoPerm(ctx, actor, "acme", "web")
	if err != nil || perm != auth.PermWrite {
		t.Fatalf("perm=%v err=%v want write", perm, err)
	}

	// Token create + verify.
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
	gotActor, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if err != nil || gotActor == nil || gotActor.Name != "alice" {
		t.Fatalf("verify: actor=%v err=%v", gotActor, err)
	}

	// CHECK enforcement: scope_perm CHECK on tokens (migration 0010).
	// CreateToken wraps the raw driver error, so asserting isCheckViolation on
	// it verifies our CHECK matcher against the LIVE libSQL error string (the
	// design's stated purpose for this suite), not merely that an error occurred.
	exp := time.Now().Unix() + 900
	err = s.CreateToken(ctx, "BADPERMTOKEN0000000000AA", "_oidc", hash, "x", &exp,
		auth.ScopeRepoRead, "acme", "web", "BOGUS")
	if err == nil {
		t.Fatal("CHECK on scope_perm should reject 'BOGUS'")
	}
	if !isCheckViolation(err) {
		t.Fatalf("libSQL CHECK error not matched by isCheckViolation: %v", err)
	}

	// OIDC mint round-trips (exercises migration 0010 tables + token binding).
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
}
