package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// HasPassword reports whether the user has a password hash set (false for
// OIDC-only accounts). Returns auth.ErrNoSuchUser if the user is absent.
func (s *Store) HasPassword(ctx context.Context, userName string) (bool, error) {
	var hash sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE name = ?`, userName).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, auth.ErrNoSuchUser
	}
	if err != nil {
		return false, fmt.Errorf("has password: %w", err)
	}
	return hash.Valid && hash.String != "", nil
}

// SetPassword hashes plaintext (argon2id PHC) and stores it on the user.
// Returns auth.ErrNoSuchUser if the user does not exist.
func (s *Store) SetPassword(ctx context.Context, userName, plaintext string) error {
	enc, err := auth.HashSecret(plaintext)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE name = ?`, enc, userName)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return auth.ErrNoSuchUser
	}
	return nil
}

// VerifyPassword validates a username+password and returns the Actor on success.
// All "can't authenticate" outcomes collapse to auth.ErrInvalidCredential except
// a disabled-but-otherwise-valid account, which returns auth.ErrUserDisabled.
func (s *Store) VerifyPassword(ctx context.Context, userName, plaintext string) (*auth.Actor, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, is_admin, disabled_at, password_hash FROM users WHERE name = ?`,
		userName)
	var (
		id, name string
		adminInt int
		disabled sql.NullInt64
		pwHash   sql.NullString
	)
	if err := row.Scan(&id, &name, &adminInt, &disabled, &pwHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrInvalidCredential
		}
		return nil, fmt.Errorf("lookup user: %w", err)
	}
	if !pwHash.Valid || pwHash.String == "" {
		return nil, auth.ErrInvalidCredential // no password set
	}
	if err := auth.VerifyHash(plaintext, pwHash.String); err != nil {
		return nil, auth.ErrInvalidCredential
	}
	if disabled.Valid {
		return nil, auth.ErrUserDisabled
	}
	return &auth.Actor{UserID: id, Name: name, IsAdmin: adminInt != 0, Scopes: auth.ScopeLegacy}, nil
}
