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

func nowUnix() int64 { return time.Now().Unix() }

// Identity is one external login linked to a local user.
type Identity struct {
	ID        string
	UserID    string
	Provider  string
	Issuer    string
	Subject   string
	Email     string
	CreatedAt int64
}

// FindIdentity resolves (issuer, subject) → Actor. ErrNoSuchUser if unlinked;
// ErrUserDisabled if the linked user is disabled.
func (s *Store) FindIdentity(ctx context.Context, issuer, subject string) (*auth.Actor, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.name, u.is_admin, u.disabled_at
		   FROM user_identities i JOIN users u ON u.id = i.user_id
		  WHERE i.issuer = ? AND i.subject = ?`, issuer, subject)
	return scanActorRow(row)
}

// LinkIdentity records a (provider=oidc, issuer, subject) → user binding.
// ErrConflict if (issuer, subject) is already linked.
func (s *Store) LinkIdentity(ctx context.Context, userID, issuer, subject, email string) error {
	id, err := newIdentityID()
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO user_identities (id, user_id, provider, issuer, subject, email, created_at)
		 VALUES (?, ?, 'oidc', ?, ?, ?, ?)`,
		id, userID, issuer, subject, normalizeEmail(email), nowUnix())
	if err != nil {
		if s.backend.IsUniqueViolation(err) {
			return auth.ErrConflict
		}
		return fmt.Errorf("link identity: %w", err)
	}
	return nil
}

// ListIdentities returns the identities linked to userName (empty if none/unknown user).
func (s *Store) ListIdentities(ctx context.Context, userName string) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT i.id, i.user_id, i.provider, i.issuer, i.subject, COALESCE(i.email,''), i.created_at
		   FROM user_identities i JOIN users u ON u.id = i.user_id
		  WHERE u.name = ? ORDER BY i.created_at`, userName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Identity{}
	for rows.Next() {
		var it Identity
		if err := rows.Scan(&it.ID, &it.UserID, &it.Provider, &it.Issuer, &it.Subject, &it.Email, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// RemoveIdentity unlinks (issuer, subject). Absent is not an error.
func (s *Store) RemoveIdentity(ctx context.Context, issuer, subject string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM user_identities WHERE issuer = ? AND subject = ?`, issuer, subject)
	return err
}
