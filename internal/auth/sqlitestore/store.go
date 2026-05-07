package sqlitestore

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store is the SQLite-backed implementation of auth.Store.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path, enables WAL and
// foreign keys, and applies any pending migrations.
func Open(path string) (*Store, error) {
	// Use file: URI so we can request WAL via _journal=WAL and foreign
	// keys via _pragma=foreign_keys(1) at connection setup time.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// Single connection for the writer side simplifies WAL semantics for
	// our use case (low concurrency on writes, many concurrent reads).
	db.SetMaxOpenConns(1)

	if err := RunMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }
