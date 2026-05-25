// Package hooks implements Tier 3 operator-defined receive hooks (pre-receive
// + post-receive subprocess execution). See spec at
// docs/superpowers/specs/2026-05-24-m20-hooks-tier3-design.md.
package hooks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Trigger names.
const (
	TriggerPreReceive  = "pre-receive"
	TriggerPostReceive = "post-receive"
)

// ErrNotFound is returned by Remove / SetEnabled when no row matches.
var ErrNotFound = errors.New("hooks: not found")

// Row is the data shape stored per registered hook.
type Row struct {
	Tenant     string
	Repo       string
	Trigger    string
	ScriptName string
	SortOrder  int
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time

	// Now is used as both CreatedAt (on insert) and UpdatedAt (on upsert).
	// Test injection; production code can leave zero — Store falls back to time.Now.
	Now time.Time
}

// Store wraps a *sql.DB (the M4 authdb) with CRUD over the hooks table.
// Constructed by NewStore; the DB lifetime is the caller's responsibility.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Add inserts or updates a hook row. On upsert, created_at is preserved.
func (s *Store) Add(ctx context.Context, r Row) error {
	if err := validateRow(r); err != nil {
		return err
	}
	now := r.Now
	if now.IsZero() {
		now = time.Now()
	}
	en := 0
	if r.Enabled {
		en = 1
	}
	// SQLite upsert: ON CONFLICT preserves created_at, advances updated_at.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO hooks (tenant, repo, trigger, script_name, sort_order, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant, repo, trigger, script_name) DO UPDATE SET
			sort_order = excluded.sort_order,
			enabled    = excluded.enabled,
			updated_at = excluded.updated_at
	`, r.Tenant, r.Repo, r.Trigger, r.ScriptName, r.SortOrder, en, now.Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("hooks.Add: %w", err)
	}
	return nil
}

// List returns all hook rows for (tenant, repo). triggerFilter=="" returns all triggers.
// Ordered by (trigger, sort_order ASC, script_name ASC) for deterministic output.
func (s *Store) List(ctx context.Context, tenant, repo, triggerFilter string) ([]Row, error) {
	q := `SELECT tenant, repo, trigger, script_name, sort_order, enabled, created_at, updated_at
	      FROM hooks WHERE tenant = ? AND repo = ?`
	args := []any{tenant, repo}
	if triggerFilter != "" {
		q += ` AND trigger = ?`
		args = append(args, triggerFilter)
	}
	q += ` ORDER BY trigger, sort_order, script_name`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("hooks.List: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// ListActiveForTrigger returns enabled=1 rows for (tenant, repo, trigger),
// ordered by (sort_order ASC, script_name ASC). Used by Service at push time.
func (s *Store) ListActiveForTrigger(ctx context.Context, tenant, repo, trigger string) ([]Row, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tenant, repo, trigger, script_name, sort_order, enabled, created_at, updated_at
		FROM hooks
		WHERE tenant = ? AND repo = ? AND trigger = ? AND enabled = 1
		ORDER BY sort_order, script_name`, tenant, repo, trigger)
	if err != nil {
		return nil, fmt.Errorf("hooks.ListActiveForTrigger: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// Remove deletes one row. Returns ErrNotFound if no row matched.
func (s *Store) Remove(ctx context.Context, tenant, repo, trigger, scriptName string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM hooks WHERE tenant = ? AND repo = ? AND trigger = ? AND script_name = ?`,
		tenant, repo, trigger, scriptName)
	if err != nil {
		return fmt.Errorf("hooks.Remove: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetEnabled flips the enabled flag. Returns ErrNotFound if no row matched.
func (s *Store) SetEnabled(ctx context.Context, tenant, repo, trigger, scriptName string, enabled bool, now time.Time) error {
	en := 0
	if enabled {
		en = 1
	}
	if now.IsZero() {
		now = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE hooks SET enabled = ?, updated_at = ?
		 WHERE tenant = ? AND repo = ? AND trigger = ? AND script_name = ?`,
		en, now.Unix(), tenant, repo, trigger, scriptName)
	if err != nil {
		return fmt.Errorf("hooks.SetEnabled: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanRows(rows *sql.Rows) ([]Row, error) {
	var out []Row
	for rows.Next() {
		var r Row
		var en int
		var createdAt, updatedAt int64
		if err := rows.Scan(&r.Tenant, &r.Repo, &r.Trigger, &r.ScriptName,
			&r.SortOrder, &en, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		r.Enabled = en != 0
		r.CreatedAt = time.Unix(createdAt, 0)
		r.UpdatedAt = time.Unix(updatedAt, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func validateRow(r Row) error {
	if r.Tenant == "" || r.Repo == "" {
		return fmt.Errorf("hooks: tenant and repo are required")
	}
	if r.Trigger != TriggerPreReceive && r.Trigger != TriggerPostReceive {
		return fmt.Errorf("hooks: trigger must be %q or %q", TriggerPreReceive, TriggerPostReceive)
	}
	if !ValidScriptName(r.ScriptName) {
		return fmt.Errorf("hooks: invalid script_name %q (must be [A-Za-z0-9._-]+, no path separators)", r.ScriptName)
	}
	return nil
}

// ValidScriptName matches the routenames-style charset: letters, digits,
// dot, underscore, hyphen. No "/", no "..", no empty. Enforced at registration
// time AND re-checked at runtime as defense-in-depth against an operator who
// hand-edits the sqlite db.
func ValidScriptName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
