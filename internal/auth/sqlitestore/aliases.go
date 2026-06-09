package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Alias is one repo rename alias: (Tenant, OldName) currently resolves to
// the live repo Target.
type Alias struct {
	Tenant    string
	OldName   string
	Target    string
	CreatedAt int64
}

// ResolveAlias returns the current live target for a renamed-away name.
// ok is false when there is no alias for (tenant, name). The caller must
// still verify the target is a live repo and enforce auth on it.
func (s *Store) ResolveAlias(ctx context.Context, tenant, name string) (target string, ok bool, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT target_name FROM repo_aliases WHERE tenant=? AND old_name=?`,
		tenant, name)
	switch err := row.Scan(&target); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("sqlitestore.ResolveAlias: %w", err)
	}
	return target, true, nil
}

// ListAliases returns all aliases whose target is (tenant, target), ordered
// by old_name. Returns a nil slice (not error) when none.
func (s *Store) ListAliases(ctx context.Context, tenant, target string) ([]Alias, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant, old_name, target_name, created_at
		   FROM repo_aliases WHERE tenant=? AND target_name=? ORDER BY old_name ASC`,
		tenant, target)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore.ListAliases: %w", err)
	}
	defer rows.Close()
	var out []Alias
	for rows.Next() {
		var a Alias
		if err := rows.Scan(&a.Tenant, &a.OldName, &a.Target, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlitestore.ListAliases scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RemoveAlias deletes one alias by (tenant, old_name). Idempotent; reports
// whether a row was removed.
func (s *Store) RemoveAlias(ctx context.Context, tenant, oldName string) (removed bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM repo_aliases WHERE tenant=? AND old_name=?`, tenant, oldName)
	if err != nil {
		return false, fmt.Errorf("sqlitestore.RemoveAlias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlitestore.RemoveAlias rows: %w", err)
	}
	return n > 0, nil
}

// insertAliasForTest is a test-only helper to seed an alias row directly.
func (s *Store) insertAliasForTest(ctx context.Context, tenant, oldName, target string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO repo_aliases (tenant, old_name, target_name, created_at)
		 VALUES (?, ?, ?, ?)`,
		tenant, oldName, target, time.Now().Unix())
	return err
}
