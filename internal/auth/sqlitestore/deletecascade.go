package sqlitestore

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
)

// ErrCascadeUnsupportedBackend is reserved for future auth-db backends on
// which DeleteRepoCascade cannot preserve the M15.1 drain design (webhook
// endpoint rows must survive repo deletion). As of M25 every shipped backend
// (sqlite, libsql, postgres) supports the cascade and this error is not
// returned; callers keep their errors.Is branches as cheap insurance.
var ErrCascadeUnsupportedBackend = errors.New("sqlitestore: repo delete cascade not supported on this backend")

// cascadeStmts is the ordered child-table sweep shared by the sqlite and
// postgres paths of DeleteRepoCascade. webhook_endpoints/_deliveries are
// deliberately absent — those rows survive (M15.1 drain design).
var cascadeStmts = []struct {
	name string
	sql  string
}{
	{"protected_refs", `DELETE FROM protected_refs WHERE tenant=? AND repo=?`},
	{"protected_paths", `DELETE FROM protected_paths WHERE tenant=? AND repo=?`},
	{"hooks", `DELETE FROM hooks WHERE tenant=? AND repo=?`},
	{"oidc_rule_claims", `DELETE FROM oidc_rule_claims WHERE rule_id IN
		(SELECT id FROM oidc_trust_rules WHERE tenant=? AND repo=?)`},
	{"oidc_trust_rules", `DELETE FROM oidc_trust_rules WHERE tenant=? AND repo=?`},
	{"repo_permissions", `DELETE FROM repo_permissions WHERE tenant=? AND repo=?`},
	{"ssh_keys (deploy-scope)", `DELETE FROM ssh_keys WHERE scope_tenant=? AND scope_repo=?`},
	{"lfs_locks", `DELETE FROM lfs_locks WHERE tenant=? AND repo=?`},
	{"repos", `DELETE FROM repos WHERE tenant=? AND name=?`},
}

// DeleteRepoCascade deletes the repos row and its non-webhook dependents
// (protected_refs, repo_permissions, deploy-scoped ssh_keys, lfs_locks,
// protected_paths, hooks, oidc_trust_rules + oidc_rule_claims) while leaving
// webhook_endpoints/_deliveries intact so a pending repo.deleted delivery can
// drain. Moved from cmd/bucketvcs (M15.1, formerly deleteRepoKeepingWebhooks);
// behavior preserved except the sweep list was extended — see below.
//
// FK NOTE: on sqlite this runs with PRAGMA foreign_keys=OFF so the ON DELETE
// CASCADE on webhook_endpoints does NOT fire (we want those rows to survive).
// But with FK enforcement off, the cascades on protected_paths (migration
// 0007), hooks (migration 0009), and oidc_trust_rules (migration 0010, M25
// fix) — all of which declare FOREIGN KEY (tenant, repo) REFERENCES repos ON
// DELETE CASCADE — do not fire either, so those rows would be ORPHANED. The
// original M15.1 sweep predates these post-M15.1 tables and never swept them.
// They are therefore deleted manually here alongside the other dependents
// (oidc_rule_claims, the child of oidc_trust_rules, is swept first via a
// subselect so no claim rows dangle once their parent rule is gone). The orphan
// webhook_endpoints row produced here is the intended known limitation; a
// webhook-prune sweeper can clean it up after the worker has drained any
// associated deliveries.
//
// On postgres (M25) the path is deleteRepoCascadePostgres: migration 0015
// dropped the webhook_endpoints→repos FK, so endpoint rows survive by
// construction and a plain transaction running the same explicit child-table
// sweep suffices (no pragma gymnastics). The remaining child-table FKs still
// declare ON DELETE CASCADE on postgres, so the explicit DELETEs are redundant
// there — they are kept so both backends read identically.
//
// ATOMICITY: the child-table DELETEs run inside a single transaction on the pinned
// connection, so a mid-sweep failure rolls the whole sweep back — there is no
// partial-failure window in which (e.g.) protected_refs is gone but repos
// survives. PRAGMA foreign_keys=OFF is issued on the connection OUTSIDE the
// transaction; sqlite keeps the per-connection FK setting effective for
// statements inside the tx, and the setting cannot be changed within a tx
// anyway. On any DELETE error the tx is rolled back before the existing
// FK-restore / connection-poison logic runs.
func (s *Store) DeleteRepoCascade(ctx context.Context, tenant, repo string) error {
	// Postgres path (M25): migration 0015 dropped the webhook_endpoints→repos
	// FK, so a plain transaction suffices — endpoint rows survive by
	// construction and no pragma gymnastics are needed.
	if s.backend.Name() == "postgres" {
		return s.deleteRepoCascadePostgres(ctx, tenant, repo)
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
	// even though FK is off. protected_paths + hooks + oidc_trust_rules are
	// swept explicitly because their FK-cascade can't fire with foreign_keys=OFF
	// (see godoc).

	// restoreFK re-enables FK enforcement on the pinned connection, marking
	// `restored` so the deferred poison is skipped, or logging+leaving it false
	// so the deferred discard drops the connection.
	restoreFK := func(cause string, causeErr error) {
		if _, rerr := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); rerr == nil {
			restored = true
		} else {
			slog.Default().Error("DeleteRepoCascade: restore foreign_keys; discarding connection",
				"cause", cause, "cause_err", causeErr, "restore_err", rerr)
		}
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		restoreFK("begin tx", err)
		return fmt.Errorf("begin delete tx: %w", err)
	}
	// All child-table DELETEs run in a single transaction so a mid-sweep failure
	// rolls the whole sweep back (no partial-failure window).
	for _, st := range cascadeStmts {
		if _, err := tx.ExecContext(ctx, st.sql, tenant, repo); err != nil {
			// Roll back so no partial sweep is committed, then restore FK
			// enforcement before returning so the connection is safe to pool.
			_ = tx.Rollback()
			restoreFK("delete from "+st.name, err)
			return fmt.Errorf("delete from %s: %w", st.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		restoreFK("commit", err)
		return fmt.Errorf("commit delete tx: %w", err)
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

// deleteRepoCascadePostgres sweeps the same child tables as the sqlite path,
// in one transaction. The remaining child-table FKs still declare ON DELETE
// CASCADE on postgres, so the explicit DELETEs are redundant there — they are
// kept so both backends read identically and behavior does not silently
// depend on FK presence. webhook_endpoints is untouched: its FK to repos was
// dropped by migration 0015 precisely so these rows outlive the repo.
func (s *Store) deleteRepoCascadePostgres(ctx context.Context, tenant, repo string) error {
	return s.db.RunInTx(ctx, func(tx Tx) error {
		for _, st := range cascadeStmts {
			if _, err := tx.ExecContext(ctx, st.sql, tenant, repo); err != nil {
				return fmt.Errorf("delete from %s: %w", st.name, err)
			}
		}
		return nil
	})
}
