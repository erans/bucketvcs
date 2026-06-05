package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed migrations_postgres/*.sql
var postgresMigrations embed.FS

// migrationsFor returns the embedded migration FS + dir name for the backend.
// Postgres uses the hand-translated set in migrations_postgres; all other
// backends (sqlite, libsql) use the canonical sqlite set.
func migrationsFor(b Backend) (fs.FS, string) {
	if b.Name() == "postgres" {
		return postgresMigrations, "migrations_postgres"
	}
	return migrationsFS, "migrations"
}

// RunMigrations applies any unapplied migrations in lexical filename order.
// It creates schema_version on first run via the embedded 0001_init.sql.
//
// Migrations are idempotent at the runner level: each is wrapped in a
// transaction, and each numbered migration is applied at most once based on
// its leading <NNNN_> prefix. The schema_version row is inserted by each
// migration's SQL so that schema_version itself can be created in 0001.
func RunMigrations(db *sql.DB, backend Backend) error {
	migFS, dir := migrationsFor(backend)
	entries, err := fs.ReadDir(migFS, dir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	applied, err := loadAppliedVersions(db)
	if err != nil {
		return err
	}

	for _, name := range names {
		ver, err := parseVersion(name)
		if err != nil {
			return fmt.Errorf("parse migration %q: %w", name, err)
		}
		if applied[ver] {
			continue
		}
		body, err := fs.ReadFile(migFS, dir+"/"+name)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", name, err)
		}
		if err := applyOne(db, backend, string(body)); err != nil {
			return fmt.Errorf("apply migration %q: %w", name, err)
		}
	}
	return nil
}

// loadAppliedVersions returns the set of already-applied version numbers,
// or an empty set if schema_version doesn't exist yet.
func loadAppliedVersions(db *sql.DB) (map[int]bool, error) {
	out := map[int]bool{}
	rows, err := db.Query("SELECT version FROM schema_version")
	if err != nil {
		// schema_version not yet created -> empty set.
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func parseVersion(name string) (int, error) {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return 0, fmt.Errorf("expected NNNN_<name>.sql")
	}
	var v int
	if _, err := fmt.Sscanf(name[:i], "%d", &v); err != nil {
		return 0, err
	}
	return v, nil
}

func applyOne(db *sql.DB, backend Backend, body string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := backend.ApplyMigration(tx, body); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SchemaVersion returns the highest applied migration number, or 0 for a
// virgin database (schema_version table absent — the query error is treated
// as "no migrations", matching loadAppliedVersions' convention).
//
// CAVEAT: every query error maps to 0, including connectivity failures on
// remote backends. Callers that diagnose (doctor) must verify the connection
// is live (e.g. SELECT 1) BEFORE interpreting 0 as "virgin/needs-migration".
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, nil
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// LatestMigrationVersion returns the highest migration number shipped in
// this binary, computed from the embedded sqlite set. The postgres set is
// numbered in lockstep (asserted by TestMigrationSetsInLockstep).
func LatestMigrationVersion() int {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0 // unreachable: embedded FS
	}
	max := 0
	for _, e := range entries {
		if v, err := parseVersion(e.Name()); err == nil && v > max {
			max = v
		}
	}
	return max
}
