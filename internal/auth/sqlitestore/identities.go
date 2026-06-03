package sqlitestore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// normalizeEmail lower-cases and trims; "" means "no email".
func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// newIdentityID returns a "bvid_"-prefixed 16-byte hex id.
func newIdentityID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "bvid_" + hex.EncodeToString(b), nil
}

// SetEmail sets (or clears, when email=="") the user's email, stored lower-cased.
// ErrNoSuchUser if the user is absent; ErrConflict if the email is taken.
func (s *Store) SetEmail(ctx context.Context, userName, email string) error {
	norm := normalizeEmail(email)
	var val any
	if norm == "" {
		val = nil
	} else {
		val = norm
	}
	res, err := s.db.ExecContext(ctx, `UPDATE users SET email = ? WHERE name = ?`, val, userName)
	if err != nil {
		if s.backend.IsUniqueViolation(err) {
			return auth.ErrConflict
		}
		return fmt.Errorf("set email: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return auth.ErrNoSuchUser
	}
	return nil
}

// FindUserByEmail resolves a verified-email lookup to an Actor. Email match is
// case-insensitive. ErrNoSuchUser if no match; ErrUserDisabled if disabled.
func (s *Store) FindUserByEmail(ctx context.Context, email string) (*auth.Actor, error) {
	norm := normalizeEmail(email)
	if norm == "" {
		return nil, auth.ErrNoSuchUser
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, is_admin, disabled_at FROM users WHERE email = ?`, norm)
	return scanActorRow(row)
}

// scanActorRow scans (id, name, is_admin, disabled_at) into an Actor, mapping
// no-rows → ErrNoSuchUser and disabled → ErrUserDisabled.
func scanActorRow(row *sql.Row) (*auth.Actor, error) {
	var (
		id, name string
		adminInt int
		disabled sql.NullInt64
	)
	if err := row.Scan(&id, &name, &adminInt, &disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrNoSuchUser
		}
		return nil, fmt.Errorf("scan actor: %w", err)
	}
	if disabled.Valid {
		return nil, auth.ErrUserDisabled
	}
	return &auth.Actor{UserID: id, Name: name, IsAdmin: adminInt != 0, Scopes: auth.ScopeLegacy}, nil
}
