package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

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
