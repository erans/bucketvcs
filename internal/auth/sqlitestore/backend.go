package sqlitestore

import (
	"database/sql"
	"net/url"
	"strings"

	_ "modernc.org/sqlite"
)

// Backend abstracts the driver-specific concerns that differ between the
// SQLite (modernc) and libSQL (Turso) backends. Phase B adds a postgres
// implementation plus a SQL-dialect helper for the divergent statements.
type Backend interface {
	// Name reports the backend for logging: "sqlite" | "libsql".
	Name() string
	// Open opens the *sql.DB with this backend's driver, DSN, and pool
	// config. It does NOT run migrations.
	Open() (*sql.DB, error)
	// ApplyMigration executes one migration file body within tx.
	ApplyMigration(tx *sql.Tx, body string) error
}

// resolveBackend selects a Backend from the --auth-db value. A recognized URL
// scheme (libsql/http/https) selects libSQL; anything else (bare path, file:,
// sqlite:, or a Windows drive path) is a filesystem path → SQLite.
func resolveBackend(value string) (Backend, error) {
	if isLibsqlValue(value) {
		return newLibsqlBackend(value)
	}
	return sqliteBackend{path: sqlitePath(value)}, nil
}

// isLibsqlValue reports whether value is a libSQL/Turso URL.
func isLibsqlValue(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "libsql", "http", "https":
		return true
	default:
		return false
	}
}

// sqlitePath strips a leading sqlite:/file: scheme if present, else returns
// value unchanged (a bare filesystem path).
func sqlitePath(value string) string {
	if u, err := url.Parse(value); err == nil {
		switch strings.ToLower(u.Scheme) {
		case "sqlite", "file":
			if u.Opaque != "" {
				return u.Opaque
			}
			return u.Path
		}
	}
	return value
}

// sqliteBackend is the default modernc.org/sqlite backend — exactly the
// behavior shipped before M23.
type sqliteBackend struct{ path string }

func (sqliteBackend) Name() string { return "sqlite" }

func (b sqliteBackend) Open() (*sql.DB, error) {
	u := &url.URL{
		Scheme: "file",
		Opaque: (&url.URL{Path: b.path}).EscapedPath(),
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// ApplyMigration execs the whole multi-statement body — modernc.org/sqlite
// supports this, and it is the proven pre-M23 path.
func (sqliteBackend) ApplyMigration(tx *sql.Tx, body string) error {
	_, err := tx.Exec(body)
	return err
}
