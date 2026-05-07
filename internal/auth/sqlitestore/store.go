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

// ErrAmbiguousPrefix is returned by ResolveTokenIDPrefix when the prefix
// matches more than one token id.
var ErrAmbiguousPrefix = errors.New("sqlitestore: ambiguous token id prefix")

// Token is the row shape returned by token-lookup methods. Note: SecretHash
// is the PHC-encoded argon2id hash, not the plaintext secret.
type Token struct {
	ID         string
	UserID     string
	SecretHash string
	Label      string
	CreatedAt  int64
	ExpiresAt  *int64
	LastUsedAt *int64
	RevokedAt  *int64
}

// CreateToken inserts a token row. The caller supplies the token-id segment
// (from auth.GenerateToken) and the PHC-encoded argon2id hash of the secret
// segment (from auth.HashSecret).
func (s *Store) CreateToken(ctx context.Context, id, userID, secretHash, label string, expiresAt *int64) error {
	now := time.Now().Unix()
	var exp sql.NullInt64
	if expiresAt != nil {
		exp = sql.NullInt64{Int64: *expiresAt, Valid: true}
	}
	var lbl sql.NullString
	if label != "" {
		lbl = sql.NullString{String: label, Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (id, user_id, secret_hash, label, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, userID, secretHash, lbl, now, exp,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return auth.ErrConflict
		}
		return fmt.Errorf("create token: %w", err)
	}
	return nil
}

// GetTokenByID fetches a token row.
func (s *Store) GetTokenByID(ctx context.Context, id string) (*Token, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, secret_hash, COALESCE(label,''), created_at,
		        expires_at, last_used_at, revoked_at
		   FROM tokens WHERE id = ?`, id,
	)
	t := &Token{}
	var exp, last, rev sql.NullInt64
	if err := row.Scan(&t.ID, &t.UserID, &t.SecretHash, &t.Label, &t.CreatedAt, &exp, &last, &rev); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrNoSuchToken
		}
		return nil, err
	}
	if exp.Valid {
		v := exp.Int64
		t.ExpiresAt = &v
	}
	if last.Valid {
		v := last.Int64
		t.LastUsedAt = &v
	}
	if rev.Valid {
		v := rev.Int64
		t.RevokedAt = &v
	}
	return t, nil
}

// ListTokensForUser returns all tokens for user `name` ordered by created_at desc.
func (s *Store) ListTokensForUser(ctx context.Context, name string) ([]*Token, error) {
	u, err := s.GetUserByName(ctx, name)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, secret_hash, COALESCE(label,''), created_at,
		        expires_at, last_used_at, revoked_at
		   FROM tokens WHERE user_id = ?
		  ORDER BY created_at DESC`, u.ID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Token{}
	for rows.Next() {
		t := &Token{}
		var exp, last, rev sql.NullInt64
		if err := rows.Scan(&t.ID, &t.UserID, &t.SecretHash, &t.Label, &t.CreatedAt, &exp, &last, &rev); err != nil {
			return nil, err
		}
		if exp.Valid {
			v := exp.Int64
			t.ExpiresAt = &v
		}
		if last.Valid {
			v := last.Int64
			t.LastUsedAt = &v
		}
		if rev.Valid {
			v := rev.Int64
			t.RevokedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken sets revoked_at on the token row identified by full id.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().Unix(), id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either token doesn't exist or was already revoked. Disambiguate.
		if _, err := s.GetTokenByID(ctx, id); err != nil {
			return err
		}
		// Already revoked: idempotent success.
		return nil
	}
	return nil
}

// ResolveTokenIDPrefix returns the full token id for the given prefix.
// Returns auth.ErrNoSuchToken if no match, ErrAmbiguousPrefix if >1 match.
func (s *Store) ResolveTokenIDPrefix(ctx context.Context, prefix string) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM tokens WHERE id LIKE ? || '%' LIMIT 2`, prefix,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	matches := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		matches = append(matches, id)
	}
	switch len(matches) {
	case 0:
		return "", auth.ErrNoSuchToken
	case 1:
		return matches[0], nil
	default:
		return "", ErrAmbiguousPrefix
	}
}

// Repo is the registry row shape.
type Repo struct {
	Tenant     string
	Name       string
	PublicRead bool
	CreatedAt  int64
}

// RegisterRepo idempotently inserts a (tenant, name) into repos.
func (s *Store) RegisterRepo(ctx context.Context, tenant, name string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, ?)`,
		tenant, name, time.Now().Unix(),
	)
	return err
}

// GetRepoFlags returns the per-repo authorization flags.
func (s *Store) GetRepoFlags(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT public_read FROM repos WHERE tenant = ? AND name = ?`, tenant, repo,
	)
	var pub int
	if err := row.Scan(&pub); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.RepoFlags{}, auth.ErrNoSuchRepo
		}
		return auth.RepoFlags{}, err
	}
	return auth.RepoFlags{PublicRead: pub != 0}, nil
}

// SetRepoPublic toggles repos.public_read.
func (s *Store) SetRepoPublic(ctx context.Context, tenant, repo string, public bool) error {
	v := 0
	if public {
		v = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE repos SET public_read = ? WHERE tenant = ? AND name = ?`, v, tenant, repo,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return auth.ErrNoSuchRepo
	}
	return nil
}

// Grant creates or replaces a permission row. perm must be "read", "write",
// or "admin". Refuses if the (tenant, repo) is not registered.
func (s *Store) Grant(ctx context.Context, userName, tenant, repo, perm string) error {
	if perm != "read" && perm != "write" && perm != "admin" {
		return fmt.Errorf("grant: invalid perm %q", perm)
	}
	u, err := s.GetUserByName(ctx, userName)
	if err != nil {
		return err
	}
	if _, err := s.GetRepoFlags(ctx, tenant, repo); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO repo_permissions (user_id, tenant, repo, perm, granted_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(user_id, tenant, repo) DO UPDATE SET perm = excluded.perm,
		                                                  granted_at = excluded.granted_at`,
		u.ID, tenant, repo, perm, time.Now().Unix(),
	)
	return err
}

// RevokeRepoPermission removes the permission row for (userName, tenant, repo).
// No error if the row didn't exist.
func (s *Store) RevokeRepoPermission(ctx context.Context, userName, tenant, repo string) error {
	u, err := s.GetUserByName(ctx, userName)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM repo_permissions WHERE user_id = ? AND tenant = ? AND repo = ?`,
		u.ID, tenant, repo,
	)
	return err
}

// LookupRepoPerm returns the actor's permission level on (tenant, repo).
// Implements auth.Store.
func (s *Store) LookupRepoPerm(ctx context.Context, actor *auth.Actor, tenant, repo string) (auth.Perm, error) {
	if actor == nil {
		return auth.PermNone, nil
	}
	if actor.IsAdmin {
		return auth.PermAdmin, nil
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT perm FROM repo_permissions
		   WHERE user_id = ? AND tenant = ? AND repo = ?`,
		actor.UserID, tenant, repo,
	)
	var p string
	if err := row.Scan(&p); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.PermNone, nil
		}
		return auth.PermNone, err
	}
	switch p {
	case "read":
		return auth.PermRead, nil
	case "write":
		return auth.PermWrite, nil
	case "admin":
		return auth.PermAdmin, nil
	default:
		return auth.PermNone, fmt.Errorf("lookup repo perm: unknown perm %q", p)
	}
}

// ListRepos returns repos in `tenant`, or all repos if tenant == "".
// Ordered by (tenant, name).
func (s *Store) ListRepos(ctx context.Context, tenant string) ([]*Repo, error) {
	var rows *sql.Rows
	var err error
	if tenant == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT tenant, name, public_read, created_at FROM repos ORDER BY tenant, name`)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT tenant, name, public_read, created_at FROM repos WHERE tenant = ? ORDER BY name`,
			tenant)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Repo{}
	for rows.Next() {
		r := &Repo{}
		var pub int
		if err := rows.Scan(&r.Tenant, &r.Name, &pub, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.PublicRead = pub != 0
		out = append(out, r)
	}
	return out, rows.Err()
}
