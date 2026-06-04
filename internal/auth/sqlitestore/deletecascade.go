package sqlitestore

import (
	"context"
	"errors"
	"fmt"
)

// ErrCascadeUnsupportedBackend: DeleteRepoCascade relies on toggling sqlite's
// per-connection foreign_keys pragma to keep webhook_endpoints rows alive
// (M15.1 drain design). Postgres FK actions (ON DELETE CASCADE on
// webhook_endpoints) cannot be suppressed, so the operation is refused rather
// than silently destroying pending repo.deleted deliveries. Requires a
// postgres schema change (drop the webhook_endpoints→repos FK) — deferred.
var ErrCascadeUnsupportedBackend = errors.New("sqlitestore: repo delete cascade not supported on this backend")

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
	// Backend gate: this function's correctness depends on suppressing the
	// webhook_endpoints ON DELETE CASCADE via the sqlite per-connection
	// foreign_keys pragma. On postgres the pragma is a syntax error AND the FK
	// is REFERENCES repos ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
	// (migrations_postgres/0006_webhooks.sql:11) — it cannot be suppressed, so
	// the M15.1 "orphan endpoints so deliveries drain" design cannot hold.
	// Refuse rather than silently destroy pending repo.deleted deliveries.
	if s.backend.Name() == "postgres" {
		return ErrCascadeUnsupportedBackend
	}

	db := s.db
	// POOL-SAFETY: the PRAGMA-per-connection sequence below is only safe
	// because the sqlite (internal/auth/sqlitestore/backend.go:150) and libsql
	// (backend_libsql.go:60) backends ALWAYS SetMaxOpenConns(1). With a single
	// pooled connection, the foreign_keys=OFF set here, every DELETE, and the
	// deferred foreign_keys=ON restore all run on the SAME connection — no
	// other statement can borrow a second conn that still has FK enforcement
	// on. On a multi-connection pool (postgres, N=10) this would race; the
	// backend gate above rejects postgres before we get here.
	//
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
