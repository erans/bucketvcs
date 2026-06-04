package sqlitestore

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
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

	db := s.db.raw()
	// POOL-SAFETY: the PRAGMA-per-connection sequence below must run entirely on
	// ONE pinned connection. database/sql RELEASES a pooled connection between
	// separate ExecContext calls, so issuing PRAGMA foreign_keys=OFF on the pool
	// and then the DELETEs as independent calls would let another goroutine
	// borrow the connection inside the OFF window and run FK-unenforced
	// statements — and a failed restore would leave the SHARED connection stuck
	// FK-off forever. We therefore pin a single connection via db.Conn and route
	// PRAGMA OFF, every DELETE, and PRAGMA ON through it, so FK enforcement is
	// suppressed only for this one connection and only for this sequence.
	//
	// MaxOpenConns(1) (sqlite backend.go:150, libsql backend_libsql.go:60) is NOT
	// what provides the isolation — connection pinning is. The single-conn pool
	// is an ADDITIONAL property: pinning the lone connection quiesces the whole
	// authdb for the (rare, fast) duration of the delete, serializing any
	// concurrent writer behind it.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin connection: %w", err)
	}
	// restored tracks whether PRAGMA foreign_keys=ON succeeded. If it did not,
	// the connection is FK-off and MUST NOT return to the pool reusable: we
	// force-discard it (driver.ErrBadConn tells database/sql to drop the
	// underlying connection instead of recycling it) before Close.
	restored := false
	defer func() {
		if !restored {
			// Best-effort poison so a connection left at foreign_keys=OFF is
			// dropped rather than pooled. The Raw callback returning
			// driver.ErrBadConn makes database/sql discard the driver conn.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}()

	// Drop FK enforcement for the destructive sequence so cascades on
	// webhook_endpoints (ON DELETE CASCADE) and webhook_deliveries (via
	// endpoint_id) do not fire.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign_keys: %w", err)
	}

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
		if _, err := conn.ExecContext(ctx, st.sql, tenant, repo); err != nil {
			// Restore FK enforcement before returning so the connection is
			// safe to pool. If the restore itself fails the deferred discard
			// poisons the connection.
			if _, rerr := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); rerr == nil {
				restored = true
			} else {
				slog.Default().Error("DeleteRepoCascade: restore foreign_keys after delete error; discarding connection",
					"table", st.name, "delete_err", err, "restore_err", rerr)
			}
			return fmt.Errorf("delete from %s: %w", st.name, err)
		}
	}

	// Re-enable FK enforcement on the pinned connection before it returns to the
	// pool. A failure here must NOT silently leave the connection FK-off: surface
	// the error and let the deferred discard drop the connection.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		slog.Default().Error("DeleteRepoCascade: restore foreign_keys; discarding connection", "restore_err", err)
		return fmt.Errorf("restore foreign_keys: %w", err)
	}
	restored = true
	return nil
}
