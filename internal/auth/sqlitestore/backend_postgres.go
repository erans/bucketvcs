package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// postgresBackend is the PostgreSQL backend (pgx via the database/sql stdlib
// adapter — pure-Go, preserves CGO_ENABLED=0). Phase B1: single-node
// (MaxOpenConns(1)); multi-node hardening is B2.
type postgresBackend struct {
	dsn      string
	maxConns int
}

// newPostgresBackend builds the backend from a postgres://|postgresql:// URL.
// The password is resolved OFF the CLI: BUCKETVCS_DB_AUTH_TOKEN env (precedence),
// else standard libpq mechanisms (PGPASSWORD/.pgpass) honored by pgx. A password
// embedded in the URL is allowed but warns (visible to other processes).
func newPostgresBackend(rawURL string, maxConns int) (Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse url: %w", err)
	}
	if tok := os.Getenv(dbAuthTokenEnv); tok != "" {
		user := u.User.Username()
		u.User = url.UserPassword(user, tok) // env password takes precedence
	} else if _, hasPw := u.User.Password(); hasPw {
		slog.Default().Warn("postgres URL embeds a password; prefer "+dbAuthTokenEnv+" or PGPASSWORD (URL is visible to other processes)",
			"host", u.Host)
	}
	if maxConns <= 0 {
		maxConns = defaultPostgresMaxConns
	}
	return postgresBackend{dsn: u.String(), maxConns: maxConns}, nil
}

func (postgresBackend) Name() string { return "postgres" }

func (b postgresBackend) Open() (*sql.DB, error) {
	db, err := sql.Open("pgx", b.dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(b.maxConns)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return db, nil
}

func (postgresBackend) ApplyMigration(tx *sql.Tx, body string) error {
	for _, stmt := range splitSQLStatements(body) {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("postgres: exec statement %q: %w", truncate(stmt, 80), err)
		}
	}
	return nil
}

// Rebind converts ? placeholders to $1,$2,… skipping ? inside single-quoted
// string literals. Our migrations/queries contain no ? in literals, but the
// scan is literal-aware to stay safe. It deliberately does NOT track SQL line
// comments (--) or double-quoted identifiers ("..."); no query the store issues
// puts a ? in either, so a future query author must avoid that.
func (postgresBackend) Rebind(query string) string {
	var sb strings.Builder
	sb.Grow(len(query) + 8)
	n := 0
	inLit := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			inLit = !inLit
			sb.WriteByte(c)
		case c == '?' && !inLit:
			n++
			sb.WriteByte('$')
			sb.WriteString(itoa(n))
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func (postgresBackend) pgErr(err error) *pgconn.PgError {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}
func (b postgresBackend) IsUniqueViolation(err error) bool {
	pe := b.pgErr(err)
	return pe != nil && pe.Code == "23505"
}
func (b postgresBackend) IsCheckViolation(err error) bool {
	pe := b.pgErr(err)
	return pe != nil && pe.Code == "23514"
}
func (b postgresBackend) IsFingerprintUniqueViolation(err error) bool {
	pe := b.pgErr(err)
	return pe != nil && pe.Code == "23505" && strings.Contains(pe.ConstraintName, "fingerprint")
}

func (postgresBackend) NowSeconds() string { return "EXTRACT(EPOCH FROM now())::bigint" }
func (postgresBackend) Greatest(expr, floor string) string {
	return "GREATEST(" + expr + ", " + floor + ")"
}

// DeferForeignKeys is a no-op on postgres: the schema declares its FKs
// DEFERRABLE INITIALLY DEFERRED, so checks already defer to COMMIT.
func (postgresBackend) DeferForeignKeys(tx *sql.Tx) error { return nil }

// InsertReturningID appends RETURNING id and scans the generated key.
func (postgresBackend) InsertReturningID(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	// query carries ? placeholders; ApplyMigration/dbWrap rebind elsewhere, but
	// this runs the raw tx, so rebind here.
	q := postgresBackend{}.Rebind(query) + " RETURNING id"
	var id int64
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}
func (postgresBackend) SupportsSkipLocked() bool { return true }
