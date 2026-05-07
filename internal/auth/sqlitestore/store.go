package sqlitestore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/bucketvcs/bucketvcs/internal/auth"
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

// ErrLastAdmin is returned by DeleteUser when removing the user would
// leave the system with zero admins.
var ErrLastAdmin = errors.New("sqlitestore: refusing to delete the last admin")

// User is the row shape returned by user-lookup methods.
type User struct {
	ID         string
	Name       string
	IsAdmin    bool
	CreatedAt  int64
	DisabledAt *int64
}

// newID returns a random 16-byte hex identifier (32 chars). We use this
// for opaque user/token primary keys; for tokens, the public id segment
// is generated separately by auth.GenerateToken.
func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// CreateUser inserts a user row and returns its id.
func (s *Store) CreateUser(ctx context.Context, name string, isAdmin bool) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	adminInt := 0
	if isAdmin {
		adminInt = 1
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, name, is_admin, created_at) VALUES (?, ?, ?, ?)`,
		id, name, adminInt, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return "", auth.ErrConflict
		}
		return "", fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

// GetUserByName returns the user row with the given name.
func (s *Store) GetUserByName(ctx context.Context, name string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, is_admin, created_at, disabled_at FROM users WHERE name = ?`,
		name,
	)
	u := &User{}
	var adminInt int
	var disabled sql.NullInt64
	if err := row.Scan(&u.ID, &u.Name, &adminInt, &u.CreatedAt, &disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrNoSuchUser
		}
		return nil, fmt.Errorf("get user: %w", err)
	}
	u.IsAdmin = adminInt != 0
	if disabled.Valid {
		v := disabled.Int64
		u.DisabledAt = &v
	}
	return u, nil
}

// ListUsers returns all users ordered by name.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, is_admin, created_at, disabled_at FROM users ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		u := &User{}
		var adminInt int
		var disabled sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Name, &adminInt, &u.CreatedAt, &disabled); err != nil {
			return nil, err
		}
		u.IsAdmin = adminInt != 0
		if disabled.Valid {
			v := disabled.Int64
			u.DisabledAt = &v
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserDisabled toggles users.disabled_at. disabled=true sets to now;
// disabled=false sets to NULL.
func (s *Store) SetUserDisabled(ctx context.Context, name string, disabled bool) error {
	var res sql.Result
	var err error
	if disabled {
		res, err = s.db.ExecContext(ctx,
			`UPDATE users SET disabled_at = ? WHERE name = ?`,
			time.Now().Unix(), name,
		)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE users SET disabled_at = NULL WHERE name = ?`, name,
		)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return auth.ErrNoSuchUser
	}
	return nil
}

// DeleteUser removes the named user. It refuses to remove the user if doing
// so would leave the system with zero admins (ErrLastAdmin).
func (s *Store) DeleteUser(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var isAdmin int
	err = tx.QueryRowContext(ctx, `SELECT is_admin FROM users WHERE name = ?`, name).Scan(&isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.ErrNoSuchUser
	}
	if err != nil {
		return err
	}
	if isAdmin != 0 {
		var others int
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE is_admin = 1 AND name != ?`, name,
		).Scan(&others)
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE name = ?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

// isUniqueViolation reports whether err looks like a SQLite UNIQUE
// constraint failure. modernc.org/sqlite errors stringify with this
// substring across versions.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "constraint failed: UNIQUE")
}
