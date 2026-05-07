package sqlitestore

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies any unapplied migrations in lexical filename order.
// It creates schema_version on first run via the embedded 0001_init.sql.
//
// Migrations are idempotent at the runner level: each is wrapped in a
// transaction, and each numbered migration is applied at most once based on
// its leading <NNNN_> prefix. The schema_version row is inserted by each
// migration's SQL so that schema_version itself can be created in 0001.
func RunMigrations(db *sql.DB) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
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
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", name, err)
		}
		if err := applyOne(db, string(body)); err != nil {
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

func applyOne(db *sql.DB, body string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(body); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
