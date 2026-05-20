package locks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// Store is the sqlite-backed implementation of the lock data layer.
// Backed by the *sqlitestore.Store handle from M4 authdb so locks
// and auth share a single sqlite file (and a single backup target).
type Store struct {
	db *sql.DB
}

// New constructs a locks.Store from an open authdb handle.
func New(authdb *sqlitestore.Store) *Store {
	return &Store{db: authdb.DB()}
}

// CreateInput parameterises a single lock creation.
type CreateInput struct {
	Tenant      string
	Repo        string
	Path        string
	RefName     string // empty = repo-wide
	OwnerUserID string
	Now         time.Time // injected for deterministic tests; pass time.Now() in prod
}

// Create inserts a new lock row and returns its server-generated ID.
// If a lock already exists for (tenant, repo, path), returns ErrAlreadyLocked.
func (s *Store) Create(ctx context.Context, in CreateInput) (string, error) {
	id, err := generateLockID()
	if err != nil {
		return "", err
	}
	var refName sql.NullString
	if in.RefName != "" {
		refName = sql.NullString{String: in.RefName, Valid: true}
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO lfs_locks (id, tenant, repo, path, ref_name, owner_user_id, locked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, in.Tenant, in.Repo, in.Path, refName, in.OwnerUserID, in.Now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrAlreadyLocked
		}
		return "", fmt.Errorf("locks: insert: %w", err)
	}
	return id, nil
}

// Get returns the lock with the given (tenant, repo, id) tuple.
// Returns ErrNotFound if no matching row exists.
func (s *Store) Get(ctx context.Context, tenant, repo, id string) (Lock, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT l.id, l.tenant, l.repo, l.path, l.ref_name, l.owner_user_id, l.locked_at, u.name
		FROM lfs_locks l
		JOIN users u ON u.id = l.owner_user_id
		WHERE l.tenant = ? AND l.repo = ? AND l.id = ?`,
		tenant, repo, id)
	return scanLock(row)
}

func scanLock(row *sql.Row) (Lock, error) {
	var l Lock
	var refName sql.NullString
	var lockedAt int64
	if err := row.Scan(&l.ID, &l.Tenant, &l.Repo, &l.Path, &refName, &l.Owner.UserID, &lockedAt, &l.Owner.Name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Lock{}, ErrNotFound
		}
		return Lock{}, fmt.Errorf("locks: scan: %w", err)
	}
	l.RefName = refName.String
	l.LockedAt = time.Unix(lockedAt, 0)
	return l, nil
}

// scanLockRows is the multi-row variant used by List/Verify.
func scanLockRows(rows *sql.Rows) ([]Lock, error) {
	defer rows.Close()
	var out []Lock
	for rows.Next() {
		var l Lock
		var refName sql.NullString
		var lockedAt int64
		if err := rows.Scan(&l.ID, &l.Tenant, &l.Repo, &l.Path, &refName, &l.Owner.UserID, &lockedAt, &l.Owner.Name); err != nil {
			return nil, fmt.Errorf("locks: scan row: %w", err)
		}
		l.RefName = refName.String
		l.LockedAt = time.Unix(lockedAt, 0)
		out = append(out, l)
	}
	return out, rows.Err()
}

// Delete removes the lock with the given (tenant, repo, id) tuple.
// Returns ErrNotFound if no matching row existed (so the caller can
// distinguish a no-op from a successful delete).
func (s *Store) Delete(ctx context.Context, tenant, repo, id string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM lfs_locks
		WHERE tenant = ? AND repo = ? AND id = ?`,
		tenant, repo, id)
	if err != nil {
		return fmt.Errorf("locks: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("locks: rows-affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// resolveLimit clamps ListOptions.Limit into [1, MaxLimit].
func resolveLimit(n int) int {
	switch {
	case n <= 0:
		return defaultLimit
	case n > MaxLimit:
		return MaxLimit
	default:
		return n
	}
}

// maxOffset bounds the largest accepted page-cursor offset. 10*MaxLimit
// is well above any legitimate page-count for a per-repo lock list
// (typical repos have <1k locks). Beyond this we reject to prevent
// abuse — a misbehaving client cannot make sqlite scan 10M+ rows just
// to discard them.
const maxOffset = MaxLimit * 10

// decodeCursor parses an opaque page cursor (a base-10 offset) into
// an integer. Empty string is treated as offset 0. Malformed strings
// surface as ErrBadCursor so the handler can emit HTTP 400.
func decodeCursor(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > maxOffset {
		return 0, ErrBadCursor
	}
	return n, nil
}

// encodeCursor formats an offset for the wire. Empty string means
// "no more pages" (caller stops iterating).
func encodeCursor(offset int) string {
	if offset <= 0 {
		return ""
	}
	return strconv.Itoa(offset)
}

// List returns locks matching opts, ordered by id, paginated via the
// opaque ListOptions.Cursor / Limit. NextCursor is empty when the
// returned page is the last one.
//
// Cursor contract: the returned NextCursor is scoped to the EXACT
// filter tuple (tenant, repo, Path, ID, RefName) that produced it.
// Callers paginating with a cursor MUST hold all filter fields
// constant across calls; changing a filter mid-pagination yields
// undefined results (pages from different filter universes stitched
// together).
func (s *Store) List(ctx context.Context, tenant, repo string, opts ListOptions) ([]Lock, string, error) {
	offset, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := resolveLimit(opts.Limit)

	// Build the WHERE clause incrementally. We fetch limit+1 rows so
	// we know whether a next page exists without a separate COUNT.
	var (
		whereSQL strings.Builder
		args     []any
	)
	whereSQL.WriteString("l.tenant = ? AND l.repo = ?")
	args = append(args, tenant, repo)
	if opts.Path != "" {
		whereSQL.WriteString(" AND l.path = ?")
		args = append(args, opts.Path)
	}
	if opts.ID != "" {
		whereSQL.WriteString(" AND l.id = ?")
		args = append(args, opts.ID)
	}
	if opts.RefName != "" {
		whereSQL.WriteString(" AND (l.ref_name = ? OR l.ref_name IS NULL)")
		args = append(args, opts.RefName)
	}

	query := `
		SELECT l.id, l.tenant, l.repo, l.path, l.ref_name, l.owner_user_id, l.locked_at, u.name
		FROM lfs_locks l
		JOIN users u ON u.id = l.owner_user_id
		WHERE ` + whereSQL.String() + `
		ORDER BY l.id
		LIMIT ? OFFSET ?`
	args = append(args, limit+1, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("locks: list query: %w", err)
	}
	all, err := scanLockRows(rows)
	if err != nil {
		return nil, "", err
	}
	if len(all) <= limit {
		return all, "", nil
	}
	// We fetched limit+1; trim and return a cursor for the next page.
	nextOffset := offset + limit
	if nextOffset > maxOffset {
		// Don't emit a cursor we'd reject. Past this offset, callers
		// must use additional filters (path/refspec) to narrow further.
		// Documented in the operator guide §8.1.
		return all[:limit], "", nil
	}
	return all[:limit], encodeCursor(nextOffset), nil
}

// Verify lists locks matching opts and partitions them by ownership.
// Ours = locks where owner_user_id == callerUserID. Theirs = the rest.
//
// Pagination: NextCursor is shared across Ours and Theirs (the LFS
// spec permits this and most servers implement it this way). A page
// with empty Ours but non-empty Theirs may still have more "ours"
// locks at a higher offset; callers MUST iterate NextCursor until
// it returns empty to enumerate all of the caller's locks.
func (s *Store) Verify(ctx context.Context, tenant, repo, callerUserID string, opts ListOptions) (VerifyResult, error) {
	page, next, err := s.List(ctx, tenant, repo, opts)
	if err != nil {
		return VerifyResult{}, err
	}
	var out VerifyResult
	out.NextCursor = next
	for _, l := range page {
		if l.Owner.UserID == callerUserID {
			out.Ours = append(out.Ours, l)
		} else {
			out.Theirs = append(out.Theirs, l)
		}
	}
	return out, nil
}

// isUniqueViolation reports whether err looks like a SQLite UNIQUE
// constraint failure. Mirrors the pattern used by sqlitestore.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
