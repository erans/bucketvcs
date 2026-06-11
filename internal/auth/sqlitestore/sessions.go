package sqlitestore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// newSessionID returns a 256-bit URL-safe random id (the cookie value).
func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashSessionID delegates to auth.HashSessionID, the single source of truth
// for the stored session-id hash (shared with the web current-session guard).
func hashSessionID(rawID string) string {
	return auth.HashSessionID(rawID)
}

// CreateSession inserts a session for userID and returns the raw cookie id.
func (s *Store) CreateSession(ctx context.Context, userID, provider string, ttl time.Duration) (string, error) {
	raw, err := newSessionID()
	if err != nil {
		return "", err
	}
	now := time.Now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id_hash, user_id, provider, created_at, expires_at, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		hashSessionID(raw), userID, provider, now.Unix(), now.Add(ttl).Unix(), now.Unix())
	if err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}
	return raw, nil
}

// LookupSession returns the live session for rawID, joining users for identity.
// Expired, absent, or disabled-user sessions return auth.ErrNoSession.
func (s *Store) LookupSession(ctx context.Context, rawID string) (*auth.Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT s.user_id, u.name, u.is_admin, s.provider, s.created_at, s.expires_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.id_hash = ? AND s.expires_at > ? AND u.disabled_at IS NULL`,
		hashSessionID(rawID), time.Now().Unix())
	var (
		userID, name, provider string
		adminInt               int
		created, expires       int64
	)
	if err := row.Scan(&userID, &name, &adminInt, &provider, &created, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrNoSession
		}
		return nil, fmt.Errorf("lookup session: %w", err)
	}
	return &auth.Session{
		UserID:    userID,
		Name:      name,
		IsAdmin:   adminInt != 0,
		Provider:  provider,
		CreatedAt: time.Unix(created, 0),
		ExpiresAt: time.Unix(expires, 0),
	}, nil
}

// TouchSession slides expiry forward, but writes at most once per minute per
// session (the `last_seen <= now-60` guard) to avoid write amplification.
// Best-effort: a no-op update (recently touched, or gone) is not an error.
func (s *Store) TouchSession(ctx context.Context, rawID string, ttl time.Duration) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET expires_at = ?, last_seen = ?
		   WHERE id_hash = ? AND last_seen <= ?`,
		now.Add(ttl).Unix(), now.Unix(), hashSessionID(rawID), now.Unix()-60)
	return err
}

// DeleteSession removes a session (logout). Absent id is not an error.
func (s *Store) DeleteSession(ctx context.Context, rawID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, hashSessionID(rawID))
	return err
}

// DeleteSessionsForUser deletes all of a user's sessions except the one
// identified by exceptRawID ("" = delete all). Returns the number deleted.
// Used on password change so credential rotation revokes attacker-held cookies.
func (s *Store) DeleteSessionsForUser(ctx context.Context, userID, exceptRawID string) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if exceptRawID == "" {
		res, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	} else {
		res, err = s.db.ExecContext(ctx,
			`DELETE FROM sessions WHERE user_id = ? AND id_hash != ?`,
			userID, hashSessionID(exceptRawID))
	}
	if err != nil {
		return 0, fmt.Errorf("delete sessions for user: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListSessionsForUser returns the user's sessions newest-first (by last_seen),
// marking the session whose stored hash matches hashSessionID(currentRawID) so
// the UI can label "this device". The raw cookie id is never returned — only
// the stored SHA-256 hash, which is safe to render and accept on a revoke POST.
func (s *Store) ListSessionsForUser(ctx context.Context, userID, currentRawID string) ([]auth.SessionInfo, error) {
	currentHash := hashSessionID(currentRawID)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id_hash, provider, created_at, expires_at, last_seen
		   FROM sessions WHERE user_id = ?
		  ORDER BY last_seen DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sessions for user: %w", err)
	}
	defer rows.Close()

	var out []auth.SessionInfo
	for rows.Next() {
		var info auth.SessionInfo
		if err := rows.Scan(&info.IDHash, &info.Provider, &info.CreatedAt, &info.ExpiresAt, &info.LastSeen); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		info.IsCurrent = info.IDHash == currentHash
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return out, nil
}

// DeleteSessionByHashForUser deletes the session identified by idHash only if it
// belongs to userID. The user_id predicate is a security boundary: a cross-user
// delete (a user submitting another user's hash) affects 0 rows. Returns the
// number of rows deleted.
func (s *Store) DeleteSessionByHashForUser(ctx context.Context, userID, idHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND id_hash = ?`, userID, idHash)
	if err != nil {
		return 0, fmt.Errorf("delete session by hash for user: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListAllSessions returns sessions joined with their owner's identity, for the
// admin view, plus the total session count. Ordered newest-first by last_seen.
// limit > 0 caps the returned rows (the count is still the full total) so a
// large deployment never loads the whole table for a display-capped page;
// limit <= 0 returns every row. LEFT JOIN + COALESCE keeps an orphaned session
// (no matching user row) visible — and revocable — as "(deleted)" rather than
// silently diverging from the COUNT(*).
func (s *Store) ListAllSessions(ctx context.Context, limit int) ([]auth.AdminSessionInfo, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count sessions: %w", err)
	}
	query := `SELECT s.id_hash, s.provider, s.created_at, s.expires_at, s.last_seen, s.user_id,
	        COALESCE(u.name, '(deleted)')
	   FROM sessions s LEFT JOIN users u ON u.id = s.user_id
	  ORDER BY s.last_seen DESC`
	var args []any
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list all sessions: %w", err)
	}
	defer rows.Close()

	var out []auth.AdminSessionInfo
	for rows.Next() {
		var info auth.AdminSessionInfo
		if err := rows.Scan(&info.IDHash, &info.Provider, &info.CreatedAt, &info.ExpiresAt, &info.LastSeen,
			&info.UserID, &info.UserName); err != nil {
			return nil, 0, fmt.Errorf("scan admin session: %w", err)
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate admin sessions: %w", err)
	}
	return out, total, nil
}

// SessionOwnerByHash resolves a stored session id hash to its owning user, for
// audit attribution before an admin revoke deletes the row (afterwards the
// hash can no longer be resolved). A missing user row resolves to "(deleted)"
// so attribution survives orphaned sessions. Returns auth.ErrNoSession when
// no session matches (NOT ErrNoSuchUser — that sentinel is classified as a
// credential failure by auth.IsCredentialError and would count toward the M18
// rate limiter if ever surfaced through an auth path).
func (s *Store) SessionOwnerByHash(ctx context.Context, idHash string) (userID, userName string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT s.user_id, COALESCE(u.name, '(deleted)')
		   FROM sessions s LEFT JOIN users u ON u.id = s.user_id
		  WHERE s.id_hash = ?`, idHash).Scan(&userID, &userName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", auth.ErrNoSession
	}
	if err != nil {
		return "", "", fmt.Errorf("session owner by hash: %w", err)
	}
	return userID, userName, nil
}

// DeleteSessionByHash deletes the session identified by idHash with no user
// scoping (admin force-revoke). Returns the number of rows deleted; an absent
// hash is a 0-row no-op.
func (s *Store) DeleteSessionByHash(ctx context.Context, idHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
	if err != nil {
		return 0, fmt.Errorf("delete session by hash: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SweepExpiredSessions deletes sessions whose expiry is at or before `now`.
func (s *Store) SweepExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
