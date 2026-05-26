package sqlitestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestMigration0010_OIDCTablesAndSystemUser(t *testing.T) {
	s := mustOpen(t) // package test helper in store_test.go (temp-dir store, migrations applied)
	ctx := context.Background()

	// _oidc system user exists and is enabled.
	u, err := s.GetUserByName(ctx, "_oidc")
	if err != nil {
		t.Fatalf("get _oidc user: %v", err)
	}
	if u.DisabledAt != nil {
		t.Fatal("_oidc user must not be disabled")
	}

	// New oidc tables accept inserts (sanity: raw exec).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO oidc_issuers (alias, issuer_url, created_at) VALUES ('gh','https://i',1)`); err != nil {
		t.Fatalf("insert issuer: %v", err)
	}
}

func TestOIDCIssuerAndRuleCRUD(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "org", "app"); err != nil {
		t.Fatalf("register repo: %v", err)
	}

	if err := s.AddOIDCIssuer(ctx, "gh", "https://token.actions.githubusercontent.com"); err != nil {
		t.Fatalf("add issuer: %v", err)
	}
	// duplicate alias -> ErrConflict
	if err := s.AddOIDCIssuer(ctx, "gh", "https://other"); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("want ErrConflict on dup alias, got %v", err)
	}

	rule := auth.OIDCTrustRule{
		IssuerAlias: "gh", Audience: "https://bvcs.example",
		Tenant: "org", Repo: "app", Scopes: auth.ScopeRepoWrite, TTLSeconds: 900,
		Claims: map[string]string{"repository": "org/app", "ref": "refs/heads/main"},
	}
	id, err := s.AddOIDCRule(ctx, rule)
	if err != nil {
		t.Fatalf("add rule: %v", err)
	}
	if id == "" {
		t.Fatal("want non-empty rule id")
	}

	// empty audience rejected
	bad := rule
	bad.Audience = ""
	if _, err := s.AddOIDCRule(ctx, bad); err == nil {
		t.Fatal("want error for empty audience")
	}

	// FindOIDCIssuerByURL
	iss, err := s.FindOIDCIssuerByURL(ctx, "https://token.actions.githubusercontent.com")
	if err != nil {
		t.Fatalf("find issuer: %v", err)
	}
	if iss.Alias != "gh" {
		t.Fatalf("alias = %q", iss.Alias)
	}

	// ListOIDCRulesForIssuer returns the rule with its claims.
	rules, err := s.ListOIDCRulesForIssuer(ctx, "gh")
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(rules) != 1 || rules[0].Claims["ref"] != "refs/heads/main" {
		t.Fatalf("rules = %+v", rules)
	}

	// Remove rule, then issuer.
	if err := s.RemoveOIDCRule(ctx, id); err != nil {
		t.Fatalf("remove rule: %v", err)
	}
	if err := s.RemoveOIDCIssuer(ctx, "gh"); err != nil {
		t.Fatalf("remove issuer: %v", err)
	}
}

func TestMintAndSweepOIDCToken(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "org", "app"); err != nil {
		t.Fatal(err)
	}

	tok, err := s.MintOIDCToken(ctx, MintOIDCParams{
		Tenant: "org", Repo: "app", Perm: auth.PermWrite,
		Scopes: auth.ScopeRepoWrite, TTLSeconds: 900, Label: "oidc:gh:sub",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// The minted token authenticates and is repo-bound.
	_, _, scope, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "x", Password: tok})
	if err != nil || scope == nil || scope.Repo != "app" {
		t.Fatalf("verify minted: scope=%+v err=%v", scope, err)
	}

	// Mint an already-expired token and sweep it.
	_, err = s.MintOIDCToken(ctx, MintOIDCParams{
		Tenant: "org", Repo: "app", Perm: auth.PermRead,
		Scopes: auth.ScopeRepoRead, TTLSeconds: -1, Label: "oidc:gh:old",
	})
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	n, err := s.SweepExpiredOIDCTokens(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n < 1 {
		t.Fatalf("swept %d, want >= 1", n)
	}
}

func TestRepoBoundTokenVerifies(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "org", "app"); err != nil {
		t.Fatal(err)
	}

	token, id, secret, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Unix() + 900
	if err := s.CreateToken(ctx, id, "_oidc", hash, "oidc:gh:sub", &exp,
		auth.ScopeRepoWrite, "org", "app", "write"); err != nil {
		t.Fatalf("create scoped token: %v", err)
	}

	// Any username works for a repo-bound token (the binding is the credential).
	actor, tokID, scope, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "x-access-token", Password: token})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if scope == nil || scope.Tenant != "org" || scope.Repo != "app" || scope.Perm != auth.PermWrite {
		t.Fatalf("scope = %+v", scope)
	}
	if tokID != id {
		t.Fatalf("tokID = %q want %q", tokID, id)
	}
	if actor.Scopes != auth.ScopeRepoWrite {
		t.Fatalf("actor scopes = %v", actor.Scopes)
	}
	if actor.Name != "oidc:gh:sub" {
		t.Fatalf("actor name = %q (want label-derived)", actor.Name)
	}
}

func TestAddOIDCRule_TTLCeiling(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "org", "app"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddOIDCIssuer(ctx, "gh", "https://i"); err != nil {
		t.Fatal(err)
	}
	rule := auth.OIDCTrustRule{
		IssuerAlias: "gh", Audience: "aud", Tenant: "org", Repo: "app",
		Scopes: auth.ScopeRepoWrite, TTLSeconds: OIDCMaxTTLSeconds + 1,
		Claims: map[string]string{},
	}
	if _, err := s.AddOIDCRule(ctx, rule); err == nil {
		t.Fatal("want error for ttl over ceiling")
	}
	rule.TTLSeconds = OIDCMaxTTLSeconds // exactly at ceiling is allowed
	if _, err := s.AddOIDCRule(ctx, rule); err != nil {
		t.Fatalf("ttl at ceiling should be allowed: %v", err)
	}
}

func TestAddOIDCIssuer_DuplicateURL(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.AddOIDCIssuer(ctx, "gh", "https://i"); err != nil {
		t.Fatal(err)
	}
	// Same URL under a different alias must conflict (issuer_url is UNIQUE).
	if err := s.AddOIDCIssuer(ctx, "gh2", "https://i"); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("want ErrConflict on duplicate url, got %v", err)
	}
}

func TestMatchRule_ArrayAudFailsClosed(t *testing.T) {
	// Array-valued aud is not supported in v1; such a token must NOT match
	// (fails closed). Documented limitation.
	rules := []auth.OIDCTrustRule{{
		ID: "r1", Audience: "aud", Tenant: "org", Repo: "app", Claims: map[string]string{},
	}}
	claims := map[string]any{"aud": []any{"aud", "other"}}
	if got := auth.MatchRule(rules, claims); got != nil {
		t.Fatalf("array aud should fail closed (no match), got %+v", got)
	}
}

func TestReservedOIDCUserProtected(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.SetUserDisabled(ctx, "_oidc", true); !errors.Is(err, ErrReservedUser) {
		t.Fatalf("disable _oidc: want ErrReservedUser, got %v", err)
	}
	if err := s.DeleteUser(ctx, "_oidc"); !errors.Is(err, ErrReservedUser) {
		t.Fatalf("delete _oidc: want ErrReservedUser, got %v", err)
	}
	// And it is hidden from ListUsers.
	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Name == "_oidc" {
			t.Fatal("_oidc must not appear in ListUsers")
		}
	}
}
