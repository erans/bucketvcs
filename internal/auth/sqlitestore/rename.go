package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// ErrRepoExists is returned by RenameRepo when the destination (tenant, name)
// is already registered. Operators must delete or rename the destination
// first.
var ErrRepoExists = errors.New("sqlitestore: destination repo already exists")

// RenameRepo updates the (tenant, name) primary key of the repos row from
// (tenant, oldName) to (tenant, newName) and propagates the new name to
// every FK-bearing table — same-tenant only.
//
// Implementation: a transaction with PRAGMA foreign_keys=OFF so the
// referential constraints don't fire mid-UPDATE. The walk is children-then-
// parent to keep the database internally consistent for any intermediate
// reader (none exist on sqlite's single-writer model, but the order matches
// M15.1's DELETE pattern for predictability).
//
// Returns auth.ErrNoSuchRepo when (tenant, oldName) doesn't exist, or
// ErrRepoExists when (tenant, newName) already does.
//
// Same-tenant only: the caller is responsible for enforcing this at the CLI
// layer. The API takes only the new bare name (not a tenant/name pair) to
// make cross-tenant rename impossible by signature.
func (s *Store) RenameRepo(ctx context.Context, tenant, oldName, newName string) error {
	if newName == oldName {
		return fmt.Errorf("sqlitestore.RenameRepo: new name equals old name")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: begin: %w", err)
	}
	defer tx.Rollback()

	// Existence + collision checks inside the transaction (consistent
	// snapshot, prevents racing renamers from both passing the check).
	var have int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM repos WHERE tenant=? AND name=?`,
		tenant, oldName).Scan(&have); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: check old: %w", err)
	}
	if have == 0 {
		return auth.ErrNoSuchRepo
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM repos WHERE tenant=? AND name=?`,
		tenant, newName).Scan(&have); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: check new: %w", err)
	}
	if have > 0 {
		return ErrRepoExists
	}

	// Defer FK enforcement until COMMIT. SQLite documents
	// `PRAGMA foreign_keys = OFF` as a no-op inside a transaction, but
	// `PRAGMA defer_foreign_keys = TRUE` IS honored mid-transaction and
	// auto-resets to FALSE at COMMIT/ROLLBACK. This lets us update child
	// rows first without tripping the FK trigger on each UPDATE; the final
	// FK check at COMMIT sees consistent rows since the parent UPDATE
	// arrives before we commit.
	if err := tx.DeferForeignKeys(); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: defer_foreign_keys: %w", err)
	}

	// Children first. Each statement updates 0..N rows; that's fine —
	// child tables may have no entries for the renamed repo.
	type stmt struct {
		table, q string
	}
	for _, st := range []stmt{
		{"repo_permissions", `UPDATE repo_permissions SET repo=? WHERE tenant=? AND repo=?`},
		// ssh_keys uses scope_(tenant, repo) column names.
		{"ssh_keys", `UPDATE ssh_keys SET scope_repo=? WHERE scope_tenant=? AND scope_repo=?`},
		// lfs_locks has no FK to repos (M15.1 manually sweeps on delete).
		{"lfs_locks", `UPDATE lfs_locks SET repo=? WHERE tenant=? AND repo=?`},
		{"protected_refs", `UPDATE protected_refs SET repo=? WHERE tenant=? AND repo=?`},
		{"webhook_endpoints", `UPDATE webhook_endpoints SET repo=? WHERE tenant=? AND repo=?`},
		{"protected_paths", `UPDATE protected_paths SET repo=? WHERE tenant=? AND repo=?`},
		{"hooks", `UPDATE hooks SET repo=? WHERE tenant=? AND repo=?`},
	} {
		if _, err := tx.ExecContext(ctx, st.q, newName, tenant, oldName); err != nil {
			return fmt.Errorf("sqlitestore.RenameRepo: update %s: %w", st.table, err)
		}
	}

	// Parent last. The PK update writes a new row and removes the old.
	//
	// RowsAffected MUST be checked: under Postgres READ COMMITTED two
	// concurrent renamers can both pass the COUNT(*) existence guard above
	// (a plain SELECT takes no row lock). The first commits and moves the
	// row; the second re-evaluates this UPDATE's WHERE against the new
	// committed snapshot, finds zero rows still named oldName, and would
	// otherwise commit a silent no-op (observed as ok=2/failed=0 in the
	// concurrency conformance test). Treating zero affected rows as
	// ErrNoSuchRepo makes exactly one renamer win. On sqlite/libsql the
	// single-writer model guarantees exactly one row here on success, so
	// this is a no-op for them.
	res, err := tx.ExecContext(ctx,
		`UPDATE repos SET name=? WHERE tenant=? AND name=?`,
		newName, tenant, oldName)
	if err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: update repos: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: rows affected: %w", err)
	} else if n == 0 {
		// The source row vanished between the guard and this UPDATE
		// (concurrent rename/delete). Roll back the child updates.
		return auth.ErrNoSuchRepo
	}

	// defer_foreign_keys auto-resets to FALSE at the end of the transaction;
	// no explicit re-enable needed.
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore.RenameRepo: commit: %w", err)
	}
	return nil
}
