package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestRenameRepo_CreatesAliasAndFlattensChain(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameRepo(ctx, "acme", "a", "b"); err != nil {
		t.Fatalf("rename a->b: %v", err)
	}
	if tgt, ok, _ := s.ResolveAlias(ctx, "acme", "a"); !ok || tgt != "b" {
		t.Fatalf("alias a should target b, got %q ok=%v", tgt, ok)
	}
	if err := s.RenameRepo(ctx, "acme", "b", "c"); err != nil {
		t.Fatalf("rename b->c: %v", err)
	}
	if tgt, ok, _ := s.ResolveAlias(ctx, "acme", "a"); !ok || tgt != "c" {
		t.Fatalf("alias a should flatten to c, got %q ok=%v", tgt, ok)
	}
	if tgt, ok, _ := s.ResolveAlias(ctx, "acme", "b"); !ok || tgt != "c" {
		t.Fatalf("alias b should target c, got %q ok=%v", tgt, ok)
	}
}

func TestRenameRepo_RenameBackDropsShadow(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "a")
	_ = s.RenameRepo(ctx, "acme", "a", "b")
	if err := s.RenameRepo(ctx, "acme", "b", "a"); err != nil {
		t.Fatalf("rename b->a: %v", err)
	}
	if _, ok, _ := s.ResolveAlias(ctx, "acme", "a"); ok {
		t.Fatal("alias 'a' must be dropped when 'a' becomes a live repo again")
	}
	if tgt, ok, _ := s.ResolveAlias(ctx, "acme", "b"); !ok || tgt != "a" {
		t.Fatalf("alias b should target a, got %q ok=%v", tgt, ok)
	}
	assertNoAliasShadowsRepo(t, s, "acme")
}

func assertNoAliasShadowsRepo(t *testing.T, s *Store, tenant string) {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT a.old_name FROM repo_aliases a JOIN repos r
		   ON a.tenant=r.tenant AND a.old_name=r.name WHERE a.tenant=?`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		t.Errorf("invariant violated: alias old_name=%q is also a live repo", n)
	}
}

func TestRenameRepo_CarriesOidcAndBuildTriggers(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "a")
	// oidc_trust_rules requires an oidc_issuers row for the FK on issuer_alias.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO oidc_issuers (alias, issuer_url, created_at)
		 VALUES ('test-issuer','https://issuer.example.test', strftime('%s','now'))`); err != nil {
		t.Fatalf("seed oidc_issuers: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO oidc_trust_rules (id, issuer_alias, audience, tenant, repo, scopes, ttl_seconds, created_at)
		 VALUES ('r1','test-issuer','aud','acme','a',0,900, strftime('%s','now'))`); err != nil {
		t.Fatalf("seed oidc rule: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO build_triggers (id, tenant, repo, name, kind, config_json, ref_include, ref_exclude,
		    token_mode, token_scopes, token_ttl_seconds, active, created_at)
		 VALUES ('bt1','acme','a','n','generic','{}','[]','[]','none',0,900,1, strftime('%s','now'))`); err != nil {
		t.Fatalf("seed build trigger: %v", err)
	}
	if err := s.RenameRepo(ctx, "acme", "a", "b"); err != nil {
		t.Fatal(err)
	}
	var oidc, bt int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oidc_trust_rules WHERE tenant='acme' AND repo='b'`).Scan(&oidc)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM build_triggers WHERE tenant='acme' AND repo='b'`).Scan(&bt)
	if oidc != 1 || bt != 1 {
		t.Fatalf("rename must carry oidc_trust_rules (%d) and build_triggers (%d) to new name", oidc, bt)
	}
}

func TestRenameRepo_BasicRoundTrip(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	aliceID, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.Grant(ctx, "alice", "acme", "foo", "write"); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if err := s.RenameRepo(ctx, "acme", "foo", "bar"); err != nil {
		t.Fatalf("RenameRepo: %v", err)
	}

	// Old row gone.
	if _, err := s.GetRepoFlags(ctx, "acme", "foo"); !errors.Is(err, auth.ErrNoSuchRepo) {
		t.Errorf("old row still exists, err=%v", err)
	}
	// New row present.
	if _, err := s.GetRepoFlags(ctx, "acme", "bar"); err != nil {
		t.Errorf("new row missing: %v", err)
	}

	// Grant follows the rename.
	actor := &auth.Actor{UserID: aliceID, Name: "alice"}
	perm, err := s.LookupRepoPerm(ctx, actor, "acme", "bar")
	if err != nil {
		t.Fatalf("LookupRepoPerm on new name: %v", err)
	}
	if perm != auth.PermWrite {
		t.Errorf("perm = %v, want PermWrite", perm)
	}
	// Grant on the old name is gone.
	permOld, err := s.LookupRepoPerm(ctx, actor, "acme", "foo")
	if err == nil && permOld != auth.PermNone {
		t.Errorf("grant on old name still resolves: perm=%v", permOld)
	}
}

func TestRenameRepo_ErrRepoExists(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("RegisterRepo foo: %v", err)
	}
	if err := s.RegisterRepo(ctx, "acme", "bar"); err != nil {
		t.Fatalf("RegisterRepo bar: %v", err)
	}

	err := s.RenameRepo(ctx, "acme", "foo", "bar")
	if !errors.Is(err, ErrRepoExists) {
		t.Errorf("err = %v, want ErrRepoExists", err)
	}

	// Source row must still exist (rollback).
	if _, err := s.GetRepoFlags(ctx, "acme", "foo"); err != nil {
		t.Errorf("source repo missing after failed rename: %v", err)
	}
}

func TestRenameRepo_ErrNoSuchRepo(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	err := s.RenameRepo(ctx, "acme", "ghost", "bar")
	if !errors.Is(err, auth.ErrNoSuchRepo) {
		t.Errorf("err = %v, want auth.ErrNoSuchRepo", err)
	}
}

func TestRenameRepo_TouchesAllFKTables(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	aliceID, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Seeds repo_permissions row via production path.
	if err := s.Grant(ctx, "alice", "acme", "foo", "write"); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	// Seed rows referencing (acme, foo) in every FK-bearing + manual-cascade table.
	// ssh_keys: deploy-key shape (user_id NULL, scope_* NOT NULL).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ssh_keys (id, fingerprint, public_key, key_type, label, created_at,
			user_id, scope_tenant, scope_repo, scope_perm)
		 VALUES ('k1', 'fp1', X'00', 'ssh-rsa', 'lbl', 1,
			NULL, 'acme', 'foo', 'read')`); err != nil {
		t.Fatalf("seed ssh_keys: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO lfs_locks (id, tenant, repo, path, ref_name, owner_user_id, locked_at)
		 VALUES ('l1', 'acme', 'foo', 'file.bin', NULL, ?, 1)`, aliceID); err != nil {
		t.Fatalf("seed lfs_locks: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO protected_refs (tenant, repo, refname_pattern, block_deletion, block_force_push, created_at)
		 VALUES ('acme', 'foo', 'refs/heads/main', 1, 1, 1)`); err != nil {
		t.Fatalf("seed protected_refs: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_endpoints (tenant, repo, url, secret, event_mask, active, created_at)
		 VALUES ('acme', 'foo', 'https://example.test/hook', 's3cret', 1, 1, 1)`); err != nil {
		t.Fatalf("seed webhook_endpoints: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO protected_paths (tenant, repo, refname_pattern, path_pattern, created_at)
		 VALUES ('acme', 'foo', 'refs/heads/main', 'secrets/**', 1)`); err != nil {
		t.Fatalf("seed protected_paths: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO hooks (tenant, repo, trigger, script_name, sort_order, enabled, created_at, updated_at)
		 VALUES ('acme', 'foo', 'pre-receive', 'lint.sh', 0, 1, 1, 1)`); err != nil {
		t.Fatalf("seed hooks: %v", err)
	}

	if err := s.RenameRepo(ctx, "acme", "foo", "bar"); err != nil {
		t.Fatalf("RenameRepo: %v", err)
	}

	cases := []struct {
		table, tCol, rCol string
	}{
		{"repos", "tenant", "name"},
		{"repo_permissions", "tenant", "repo"},
		{"ssh_keys", "scope_tenant", "scope_repo"},
		{"lfs_locks", "tenant", "repo"},
		{"protected_refs", "tenant", "repo"},
		{"webhook_endpoints", "tenant", "repo"},
		{"protected_paths", "tenant", "repo"},
		{"hooks", "tenant", "repo"},
	}
	for _, c := range cases {
		var oldCount, newCount int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+c.table+` WHERE `+c.tCol+`=? AND `+c.rCol+`=?`,
			"acme", "foo").Scan(&oldCount); err != nil {
			t.Errorf("%s: count old: %v", c.table, err)
			continue
		}
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+c.table+` WHERE `+c.tCol+`=? AND `+c.rCol+`=?`,
			"acme", "bar").Scan(&newCount); err != nil {
			t.Errorf("%s: count new: %v", c.table, err)
			continue
		}
		if oldCount != 0 || newCount != 1 {
			t.Errorf("%s: old=%d new=%d, want old=0 new=1", c.table, oldCount, newCount)
		}
	}
}
