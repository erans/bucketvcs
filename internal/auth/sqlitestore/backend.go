package sqlitestore

import (
	"context"
	"database/sql"
	"net/url"
	"strings"

	_ "modernc.org/sqlite"
)

// Backend abstracts the driver-specific concerns that differ between the
// SQLite (modernc), libSQL (Turso), and PostgreSQL backends.
type Backend interface {
	// Name reports the backend for logging: "sqlite" | "libsql" | "postgres".
	Name() string
	// Open opens the *sql.DB with this backend's driver, DSN, and pool
	// config. It does NOT run migrations.
	Open() (*sql.DB, error)
	// ApplyMigration executes one migration file body within tx.
	ApplyMigration(tx *sql.Tx, body string) error

	// Rebind converts a ?-placeholder query to the backend's placeholder
	// style. sqlite/libsql: identity. postgres: ?→$1,$2,… (literal-aware).
	Rebind(query string) string
	// IsUniqueViolation / IsCheckViolation classify constraint errors.
	IsUniqueViolation(err error) bool
	IsCheckViolation(err error) bool
	// IsFingerprintUniqueViolation reports a UNIQUE violation specifically on
	// the ssh_keys.fingerprint constraint.
	IsFingerprintUniqueViolation(err error) bool

	// NowSeconds returns a SQL expression yielding the current unix time in
	// seconds: sqlite "strftime('%s','now')"; postgres
	// "EXTRACT(EPOCH FROM now())::bigint".
	NowSeconds() string
	// Greatest returns a SQL expression for max(expr, floor): sqlite
	// "MAX(expr, floor)"; postgres "GREATEST(expr, floor)".
	Greatest(expr, floor string) string
	// DeferForeignKeys defers FK checks to COMMIT for the given tx. sqlite
	// execs "PRAGMA defer_foreign_keys = TRUE"; postgres is a no-op because
	// its FKs are declared DEFERRABLE INITIALLY DEFERRED.
	DeferForeignKeys(tx *sql.Tx) error
	// InsertReturningID runs an INSERT and returns the generated integer id.
	// sqlite execs then uses LastInsertId; postgres appends " RETURNING id"
	// and scans. The table's surrogate key MUST be named "id".
	InsertReturningID(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error)
	// SupportsSkipLocked reports whether the backend supports
	// SELECT … FOR UPDATE SKIP LOCKED concurrent row claiming. true for
	// postgres; false for sqlite/libsql (single-writer).
	SupportsSkipLocked() bool
}

// options carries Open-time configuration resolved from functional Options.
type options struct {
	maxConns int // Postgres pool size; 0 means default (10). Ignored by sqlite/libsql.
}

// Option configures Open/resolveBackend.
type Option func(*options)

// WithMaxConns sets the Postgres connection-pool size (SetMaxOpenConns).
// Ignored by sqlite/libsql, which always use a single connection.
func WithMaxConns(n int) Option { return func(o *options) { o.maxConns = n } }

const defaultPostgresMaxConns = 10

// resolveBackend selects a Backend from the --auth-db value. postgres:// /
// postgresql:// selects PostgreSQL; libsql/http/https selects libSQL; anything
// else (bare path, file:, sqlite:, or a Windows drive path) is a filesystem
// path → SQLite.
func resolveBackend(value string, opts ...Option) (Backend, error) {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	if isPostgresValue(value) {
		return newPostgresBackend(value, o.maxConns)
	}
	if isLibsqlValue(value) {
		return newLibsqlBackend(value)
	}
	return sqliteBackend{path: sqlitePath(value)}, nil
}

// isPostgresValue reports whether value is a PostgreSQL URL.
func isPostgresValue(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
		return true
	default:
		return false
	}
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

func (sqliteBackend) Rebind(query string) string { return query }

func (sqliteBackend) IsUniqueViolation(err error) bool { return sqliteIsUnique(err) }
func (sqliteBackend) IsCheckViolation(err error) bool  { return sqliteIsCheck(err) }
func (sqliteBackend) IsFingerprintUniqueViolation(err error) bool {
	return sqliteIsUnique(err) &&
		(strings.Contains(err.Error(), "ssh_keys.fingerprint") ||
			strings.Contains(err.Error(), "fingerprint"))
}

func (sqliteBackend) NowSeconds() string { return "strftime('%s','now')" }
func (sqliteBackend) Greatest(expr, floor string) string {
	return "MAX(" + expr + ", " + floor + ")"
}
func (sqliteBackend) DeferForeignKeys(tx *sql.Tx) error {
	_, err := tx.Exec("PRAGMA defer_foreign_keys = TRUE")
	return err
}
func (sqliteBackend) InsertReturningID(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
func (sqliteBackend) SupportsSkipLocked() bool { return false }

// sqliteIsUnique / sqliteIsCheck are the substring matchers shared by the
// sqlite and libsql backends (libSQL surfaces the same SQLite error text).
func sqliteIsUnique(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "UNIQUE constraint failed") ||
		strings.Contains(m, "constraint failed: UNIQUE")
}
func sqliteIsCheck(err error) bool {
	return err != nil && strings.Contains(err.Error(), "CHECK constraint failed")
}
