package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// TestDeleteRepoCascade_SweepsDependentsKeepsWebhooks verifies that every
// non-webhook dependent (repo_permissions, deploy ssh_keys, lfs_locks,
// protected_refs, protected_paths, hooks) plus the repos row itself is
// removed, while webhook_endpoints survives so a pending repo.deleted
// delivery can drain.
//
// Regression guard for the M15.1 sweep gap: protected_paths (0007) and hooks
// (0009) carry FK ON DELETE CASCADE to repos, but the cascade can't fire while
// foreign_keys=OFF, so they must be swept manually. Before this fix they were
// orphaned on delete.
func TestDeleteRepoCascade_SweepsDependentsKeepsWebhooks(t *testing.T) {
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

	// Seed rows referencing (acme, foo) in every dependent table.
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
		`INSERT INTO protected_paths (tenant, repo, refname_pattern, path_pattern, created_at)
		 VALUES ('acme', 'foo', 'refs/heads/main', 'secrets/**', 1)`); err != nil {
		t.Fatalf("seed protected_paths: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO hooks (tenant, repo, trigger, script_name, sort_order, enabled, created_at, updated_at)
		 VALUES ('acme', 'foo', 'pre-receive', 'lint.sh', 0, 1, 1, 1)`); err != nil {
		t.Fatalf("seed hooks: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_endpoints (tenant, repo, url, secret, event_mask, active, created_at)
		 VALUES ('acme', 'foo', 'https://example.test/hook', 's3cret', 1, 1, 1)`); err != nil {
		t.Fatalf("seed webhook_endpoints: %v", err)
	}

	if err := s.DeleteRepoCascade(ctx, "acme", "foo"); err != nil {
		t.Fatalf("DeleteRepoCascade: %v", err)
	}

	// repos row gone.
	if _, err := s.GetRepoFlags(ctx, "acme", "foo"); !errors.Is(err, auth.ErrNoSuchRepo) {
		t.Errorf("repos row still exists, err=%v", err)
	}

	// Dependents that MUST be swept to zero.
	swept := []struct {
		table, tCol, rCol string
	}{
		{"repos", "tenant", "name"},
		{"repo_permissions", "tenant", "repo"},
		{"ssh_keys", "scope_tenant", "scope_repo"},
		{"lfs_locks", "tenant", "repo"},
		{"protected_refs", "tenant", "repo"},
		{"protected_paths", "tenant", "repo"},
		{"hooks", "tenant", "repo"},
	}
	for _, c := range swept {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+c.table+` WHERE `+c.tCol+`=? AND `+c.rCol+`=?`,
			"acme", "foo").Scan(&n); err != nil {
			t.Errorf("%s: count: %v", c.table, err)
			continue
		}
		if n != 0 {
			t.Errorf("%s: count=%d after delete, want 0 (orphaned row)", c.table, n)
		}
	}

	// webhook_endpoints MUST survive (intentional — lets repo.deleted drain).
	var wh int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webhook_endpoints WHERE tenant=? AND repo=?`,
		"acme", "foo").Scan(&wh); err != nil {
		t.Fatalf("count webhook_endpoints: %v", err)
	}
	if wh != 1 {
		t.Errorf("webhook_endpoints count=%d after delete, want 1 (must survive)", wh)
	}
}
