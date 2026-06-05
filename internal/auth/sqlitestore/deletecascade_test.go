package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// TestDeleteRepoCascade_SweepsDependentsKeepsWebhooks verifies that every
// non-webhook dependent (repo_permissions, deploy ssh_keys, lfs_locks,
// protected_refs, protected_paths, hooks, oidc_trust_rules + oidc_rule_claims)
// plus the repos row itself is removed, while webhook_endpoints survives so a
// pending repo.deleted delivery can drain.
//
// Regression guard for the M15.1 sweep gap: protected_paths (0007), hooks
// (0009), and oidc_trust_rules (0010) carry FK ON DELETE CASCADE to repos, but
// the cascade can't fire while foreign_keys=OFF, so they must be swept
// manually. Before this fix they were orphaned on delete.
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
	// oidc_trust_rules (0010) carries FK (tenant, repo)→repos ON DELETE CASCADE,
	// and oidc_rule_claims is its child via rule_id. Both must be swept manually
	// because the cascade can't fire with foreign_keys=OFF. Seed an issuer first
	// (oidc_trust_rules.issuer_alias FKs to oidc_issuers).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO oidc_issuers (alias, issuer_url, created_at)
		 VALUES ('gh', 'https://token.actions.githubusercontent.com', 1)`); err != nil {
		t.Fatalf("seed oidc_issuers: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO oidc_trust_rules (id, issuer_alias, audience, tenant, repo, scopes, ttl_seconds, created_at)
		 VALUES ('r1', 'gh', 'aud', 'acme', 'foo', 0, 900, 1)`); err != nil {
		t.Fatalf("seed oidc_trust_rules: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO oidc_rule_claims (rule_id, claim_name, claim_value)
		 VALUES ('r1', 'sub', 'repo:acme/foo:ref:refs/heads/main')`); err != nil {
		t.Fatalf("seed oidc_rule_claims: %v", err)
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
		{"oidc_trust_rules", "tenant", "repo"},
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

	// oidc_rule_claims is the child of oidc_trust_rules (keyed by rule_id, not
	// tenant/repo) and must also be swept — assert no claim rows survive.
	var claims int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM oidc_rule_claims WHERE rule_id='r1'`).Scan(&claims); err != nil {
		t.Errorf("oidc_rule_claims: count: %v", err)
	} else if claims != 0 {
		t.Errorf("oidc_rule_claims: count=%d after delete, want 0 (orphaned row)", claims)
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

// fkEnforced reports whether the connection currently enforces foreign keys by
// attempting an FK-violating insert (a repo_permissions row for a nonexistent
// repo) and observing whether it is rejected. repo_permissions carries
// FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
// (migration 0001), so with enforcement ON the insert MUST fail. The probe
// references a real user to isolate the repos FK from the users FK.
func fkEnforced(t *testing.T, s *Store, userID string) bool {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO repo_permissions (user_id, tenant, repo, perm, granted_at)
		 VALUES (?, 'ghost-tenant', 'ghost-repo', 'read', 1)`, userID)
	if err == nil {
		// Insert succeeded => FK NOT enforced. Clean it up so it can't leak.
		_, _ = s.db.ExecContext(context.Background(),
			`DELETE FROM repo_permissions WHERE tenant='ghost-tenant' AND repo='ghost-repo'`)
		return false
	}
	return true
}

// TestDeleteRepoCascade_RestoresFKEnforcement is the concurrency-safety
// regression guard. The cascade pins one connection, flips foreign_keys=OFF for
// the destructive sweep, and MUST restore foreign_keys=ON before that
// connection returns to the (single-conn) pool. We can't pause the sweep
// mid-flight without hooks, so we assert the OBSERVABLE post-condition: after a
// successful DeleteRepoCascade, a subsequent FK-violating insert is still
// rejected. Before connection pinning, a failed/raced restore could leave the
// shared connection stuck FK-off, and this insert would silently succeed.
func TestDeleteRepoCascade_RestoresFKEnforcement(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	userID, err := s.CreateUser(ctx, "probe", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}

	if err := s.DeleteRepoCascade(ctx, "acme", "foo"); err != nil {
		t.Fatalf("DeleteRepoCascade: %v", err)
	}

	if !fkEnforced(t, s, userID) {
		t.Fatal("foreign_keys NOT enforced after successful cascade: connection left FK-off")
	}
}

// TestDeleteRepoCascade_RestoresFKOnMidSequenceError verifies the restore path
// runs even when a DELETE errors mid-sweep, AND that the sweep is atomic: the
// child-table DELETEs run in one transaction, so a mid-sweep failure rolls the whole
// sweep back. We DROP the hooks table (third in the sweep) so the hooks DELETE
// fails; the cascade must (1) return the error, (2) re-enable foreign_keys
// before the connection is pooled, and (3) leave the rows from the
// already-executed DELETEs (protected_refs, protected_paths) intact because the
// transaction rolled back.
func TestDeleteRepoCascade_RestoresFKOnMidSequenceError(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	userID, err := s.CreateUser(ctx, "probe", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}

	// Seed rows in the two tables swept BEFORE hooks (protected_refs is #1,
	// protected_paths is #2; hooks is #3). After the forced hooks failure these
	// must still be present — proving the transaction rolled back.
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

	// Force the mid-sweep DELETE to fail: drop the hooks table so
	// `DELETE FROM hooks ...` errors with "no such table".
	if _, err := s.db.ExecContext(ctx, `DROP TABLE hooks`); err != nil {
		t.Fatalf("DROP TABLE hooks: %v", err)
	}

	if err := s.DeleteRepoCascade(ctx, "acme", "foo"); err == nil {
		t.Fatal("DeleteRepoCascade: want error after dropping hooks table, got nil")
	}

	// Despite the mid-sweep failure, FK enforcement must be restored.
	if !fkEnforced(t, s, userID) {
		t.Fatal("foreign_keys NOT enforced after mid-sequence error: restore path skipped")
	}

	// Atomicity: the rows swept before the failure must STILL be present
	// because the transaction rolled back (no partial sweep committed).
	for _, c := range []struct{ table, tCol, rCol string }{
		{"protected_refs", "tenant", "repo"},
		{"protected_paths", "tenant", "repo"},
		{"repos", "tenant", "name"},
	} {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+c.table+` WHERE `+c.tCol+`=? AND `+c.rCol+`=?`,
			"acme", "foo").Scan(&n); err != nil {
			t.Fatalf("%s: count: %v", c.table, err)
		}
		if n != 1 {
			t.Errorf("%s: count=%d after rolled-back sweep, want 1 (rollback must preserve)", c.table, n)
		}
	}
}
