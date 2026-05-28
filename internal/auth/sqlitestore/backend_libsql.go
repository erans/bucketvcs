package sqlitestore

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// dbAuthTokenEnv is the environment variable holding the libSQL/Turso auth
// token. The token is NEVER passed as a CLI argument (it would leak via ps).
const dbAuthTokenEnv = "BUCKETVCS_DB_AUTH_TOKEN"

// libsqlBackend is the pure-Go libSQL (Turso) backend. Remote only — embedded
// replicas require the cgo go-libsql driver, which would break CGO_ENABLED=0
// cross-compilation.
type libsqlBackend struct {
	dsn string // full libSQL DSN including auth token
}

// newLibsqlBackend builds the backend from the --auth-db URL, resolving the
// auth token from BUCKETVCS_DB_AUTH_TOKEN (preferred) or an authToken query
// param already on the URL. The token is OPTIONAL: self-hosted sqld over
// http(s) commonly runs without auth (and the conformance suite targets such
// an instance), so we do not hard-fail when it is missing. We warn for the
// libsql:// scheme (Turso almost always needs one) and let the connection
// surface a clear auth error if the server actually requires it.
func newLibsqlBackend(rawURL string) (Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("libsql: parse url: %w", err)
	}
	q := u.Query()
	if envTok := os.Getenv(dbAuthTokenEnv); envTok != "" {
		q.Set("authToken", envTok) // env takes precedence over any in-URL token
	}
	if q.Get("authToken") == "" && strings.EqualFold(u.Scheme, "libsql") {
		slog.Default().Warn("libsql URL has no auth token; set "+dbAuthTokenEnv+" if the server requires one",
			"host", u.Host)
	}
	u.RawQuery = q.Encode()
	return libsqlBackend{dsn: u.String()}, nil
}

func (libsqlBackend) Name() string { return "libsql" }

func (b libsqlBackend) Open() (*sql.DB, error) {
	db, err := sql.Open("libsql", b.dsn)
	if err != nil {
		return nil, err
	}
	// Phase A: single connection preserves the single-writer serialization
	// the current store code assumes (quota ring-lock, webhook claim, …).
	// Multi-node concurrency hardening + pooling is Phase B.
	db.SetMaxOpenConns(1)
	// Remote sqld ignores the modernc _pragma DSN syntax; enforce FKs via a
	// statement (sqld honors it — confirmed in Task 1).
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("libsql: enable foreign_keys: %w", err)
	}
	return db, nil
}

// ApplyMigration applies the body one statement at a time within tx, since the
// libSQL HTTP driver may not accept a multi-statement Exec.
func (libsqlBackend) ApplyMigration(tx *sql.Tx, body string) error {
	for _, stmt := range splitSQLStatements(body) {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("libsql: exec statement %q: %w", truncate(stmt, 80), err)
		}
	}
	return nil
}

// truncate shortens s to at most n runes (not bytes) so the cut never lands
// mid-rune and corrupts a multi-byte character in the error message.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
