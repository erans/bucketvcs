package sqlitestore

import (
	"context"
	"fmt"
)

// DeleteRepoCascade deletes the repos row and its non-webhook dependents
// (protected_refs, repo_permissions, deploy-scoped ssh_keys, lfs_locks,
// protected_paths, hooks) while leaving webhook_endpoints/_deliveries intact
// so a pending repo.deleted delivery can drain. Moved from cmd/bucketvcs
// (M15.1, formerly deleteRepoKeepingWebhooks); behavior preserved except the
// sweep list was extended — see below.
//
// FK NOTE: this runs with PRAGMA foreign_keys=OFF so the ON DELETE CASCADE on
// webhook_endpoints does NOT fire (we want those rows to survive). But with FK
// enforcement off, the cascades on protected_paths (migration 0007) and hooks
// (migration 0009) — both of which declare FOREIGN KEY (tenant, repo)
// REFERENCES repos ON DELETE CASCADE — do not fire either, so those rows would
// be ORPHANED. The original M15.1 sweep predates those two tables and never
// swept them. They are therefore deleted manually here alongside the other
// dependents. The orphan webhook_endpoints row produced here is the intended
// known limitation; a webhook-prune sweeper can clean it up after the worker
// has drained any associated deliveries.
func (s *Store) DeleteRepoCascade(ctx context.Context, tenant, repo string) error {
	db := s.db
	// Drop FK enforcement for the destructive sequence so cascades on
	// webhook_endpoints (ON DELETE CASCADE) and webhook_deliveries (via
	// endpoint_id) do not fire.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign_keys: %w", err)
	}
	// Re-enable on every exit. We do NOT wrap in a tx because PRAGMA
	// foreign_keys is a no-op inside an open tx in sqlite; the safe path
	// is auto-commit statements bracketed by the pragma toggle.
	defer func() {
		_, _ = db.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
	}()

	// Cascade the non-webhook dependents manually so they're cleaned up
	// even though FK is off. protected_paths + hooks are swept explicitly
	// because their FK-cascade can't fire with foreign_keys=OFF (see godoc).
	stmts := []struct {
		name string
		sql  string
	}{
		{"protected_refs", `DELETE FROM protected_refs WHERE tenant=? AND repo=?`},
		{"protected_paths", `DELETE FROM protected_paths WHERE tenant=? AND repo=?`},
		{"hooks", `DELETE FROM hooks WHERE tenant=? AND repo=?`},
		{"repo_permissions", `DELETE FROM repo_permissions WHERE tenant=? AND repo=?`},
		{"ssh_keys (deploy-scope)", `DELETE FROM ssh_keys WHERE scope_tenant=? AND scope_repo=?`},
		{"lfs_locks", `DELETE FROM lfs_locks WHERE tenant=? AND repo=?`},
		{"repos", `DELETE FROM repos WHERE tenant=? AND name=?`},
	}
	for _, st := range stmts {
		if _, err := db.ExecContext(ctx, st.sql, tenant, repo); err != nil {
			return fmt.Errorf("delete from %s: %w", st.name, err)
		}
	}
	return nil
}
