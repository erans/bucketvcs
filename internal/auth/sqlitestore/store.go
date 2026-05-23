package sqlitestore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
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
	// Build the DSN as a URL so paths containing `?`, `#`, or other URI
	// metacharacters are escaped rather than misinterpreted as query/fragment.
	u := &url.URL{
		Scheme: "file",
		Opaque: (&url.URL{Path: path}).EscapedPath(),
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	dsn := u.String()

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

// DB returns the underlying *sql.DB for callers that need to attach
// additional schema-managed tables to the same handle (e.g., the
// M13.3 LFS locks store). External writers MUST respect the same
// concurrency constraints sqlitestore.Open enforces (WAL,
// foreign_keys, single open conn).
func (s *Store) DB() *sql.DB { return s.db }

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
func (s *Store) GetUserByName(ctx context.Context, name string) (*auth.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, is_admin, created_at, disabled_at FROM users WHERE name = ?`,
		name,
	)
	u := &auth.User{}
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
//
// When disabling, this method refuses to leave the system with zero
// ENABLED admins (ErrLastAdmin). Disabling an admin user is, for the
// purposes of authentication, equivalent to deleting them — an admin
// account that cannot log in is not a recovery path. The check uses the
// same predicate as DeleteUser's last-admin guard (is_admin = 1 AND
// disabled_at IS NULL) so the two operations agree on what "remaining
// admin" means. Re-enabling has no such guard: re-enabling can only
// strictly increase the count of enabled admins.
func (s *Store) SetUserDisabled(ctx context.Context, name string, disabled bool) error {
	if disabled {
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
				`SELECT COUNT(*) FROM users WHERE is_admin = 1 AND name != ? AND disabled_at IS NULL`, name,
			).Scan(&others)
			if err != nil {
				return err
			}
			if others == 0 {
				return ErrLastAdmin
			}
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE users SET disabled_at = ? WHERE name = ?`,
			time.Now().Unix(), name,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return auth.ErrNoSuchUser
		}
		return tx.Commit()
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET disabled_at = NULL WHERE name = ?`, name,
	)
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
// so would leave the system with zero ENABLED admins (ErrLastAdmin). A
// disabled admin doesn't count toward the remaining-admin total — they
// can't authenticate, so leaving "the last admin disabled" would lock
// every operator out.
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
			`SELECT COUNT(*) FROM users WHERE is_admin = 1 AND name != ? AND disabled_at IS NULL`, name,
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

// isSafeTokenIDPrefix accepts any non-empty ASCII alphanumeric prefix.
// Real token IDs use the Crockford-base32 alphabet (auth.GenerateToken),
// which is a strict subset; the broader alphanumeric check still excludes
// SQL LIKE metacharacters (% _) and any other shell/SQL-special characters
// while remaining permissive enough for synthetic IDs used in tests.
func isSafeTokenIDPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9',
			c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z':
			// safe
		default:
			return false
		}
	}
	return true
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
//
// The prefix is validated against the Crockford-base32 alphabet used by
// auth.GenerateToken before being used in a SQL LIKE expression — this
// guards against `%`/`_` wildcards in user input matching unintended rows.
func (s *Store) ResolveTokenIDPrefix(ctx context.Context, prefix string) (string, error) {
	if !isSafeTokenIDPrefix(prefix) {
		return "", auth.ErrNoSuchToken
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM tokens WHERE substr(id, 1, ?) = ? LIMIT 2`,
		len(prefix), prefix,
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

// RegisterRepoIfNew is like RegisterRepo but additionally reports whether
// the row was actually created. Returns inserted=true iff a new row was
// inserted (i.e., the (tenant, name) pair did not previously exist).
//
// Use this instead of pre-checking with GetRepoFlags + then calling
// RegisterRepo — that pattern races with a concurrent registration.
func (s *Store) RegisterRepoIfNew(ctx context.Context, tenant, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, ?)`,
		tenant, name, time.Now().Unix(),
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
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

// DeleteRepo removes a (tenant, name) from repos. SSH deploy keys bound to
// the repo are removed by CASCADE (schema enforces ON DELETE CASCADE on the
// FOREIGN KEY). No error if the repo did not exist.
func (s *Store) DeleteRepo(ctx context.Context, tenant, name string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM repos WHERE tenant = ? AND name = ?`, tenant, name,
	)
	return err
}

// DeleteUserByID removes the user with the given ID. Unlike DeleteUser (which
// uses name and guards against last-admin deletion), this method is intended
// for test teardown and operates on the raw ID without the last-admin check.
// SSH keys owned by the user are removed by CASCADE.
func (s *Store) DeleteUserByID(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

// DisableUserByID sets disabled_at for the user identified by ID. Intended for
// test-seeding where the caller holds the opaque ID rather than the name.
func (s *Store) DisableUserByID(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET disabled_at = ? WHERE id = ?`, time.Now().Unix(), id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return auth.ErrNoSuchUser
	}
	return nil
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

// VerifyCredential implements auth.Store.
func (s *Store) VerifyCredential(ctx context.Context, c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
	switch c := c.(type) {
	case auth.SSHKeyFingerprint:
		return s.verifySSHKey(ctx, c.Fingerprint)
	case auth.BasicPassword:
		return s.verifyBasicPassword(ctx, c)
	default:
		return nil, "", nil, auth.ErrInvalidCredential
	}
}

// verifyBasicPassword handles the BasicPassword credential case.
func (s *Store) verifyBasicPassword(ctx context.Context, bp auth.BasicPassword) (*auth.Actor, string, *auth.Scope, error) {
	tokenID, secret, err := auth.ParseToken(bp.Password)
	if err != nil {
		return nil, "", nil, auth.ErrInvalidCredential
	}
	tok, err := s.GetTokenByID(ctx, tokenID)
	if errors.Is(err, auth.ErrNoSuchToken) {
		return nil, "", nil, auth.ErrInvalidCredential
	}
	if err != nil {
		return nil, "", nil, err
	}
	if err := auth.VerifyHash(secret, tok.SecretHash); err != nil {
		return nil, "", nil, auth.ErrInvalidCredential
	}
	if tok.RevokedAt != nil {
		return nil, "", nil, auth.ErrTokenRevoked
	}
	if tok.ExpiresAt != nil && *tok.ExpiresAt <= time.Now().Unix() {
		return nil, "", nil, auth.ErrTokenExpired
	}
	// Lookup the user; check name match and disabled state.
	row := s.db.QueryRowContext(ctx,
		`SELECT name, is_admin, disabled_at FROM users WHERE id = ?`, tok.UserID,
	)
	var name string
	var adminInt int
	var disabled sql.NullInt64
	if err := row.Scan(&name, &adminInt, &disabled); err != nil {
		return nil, "", nil, auth.ErrInvalidCredential
	}
	if disabled.Valid {
		return nil, "", nil, auth.ErrUserDisabled
	}
	if bp.Username != name {
		return nil, "", nil, auth.ErrInvalidCredential
	}
	return &auth.Actor{
		UserID:  tok.UserID,
		Name:    name,
		IsAdmin: adminInt != 0,
	}, tokenID, nil, nil
}

// verifySSHKey handles the SSHKeyFingerprint credential case.
// It performs a single join to resolve both user keys and deploy keys.
// The schema enforces an XOR constraint: either user_id is set (user key) or
// scope_* fields are set (deploy key) — never both, never neither.
// We branch on userID.Valid after scanning to distinguish the two cases.
func (s *Store) verifySSHKey(ctx context.Context, fingerprint string) (*auth.Actor, string, *auth.Scope, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT k.id,
		       k.user_id, k.scope_tenant, k.scope_repo, k.scope_perm,
		       k.revoked_at, k.label,
		       u.id, u.name, u.is_admin, u.disabled_at
		FROM ssh_keys k
		LEFT JOIN users u ON u.id = k.user_id
		WHERE k.fingerprint = ?
	`, fingerprint)

	var (
		keyID                                     string
		userID, scopeTenant, scopeRepo, scopePerm sql.NullString
		revokedAt                                 sql.NullInt64
		keyLabel                                  sql.NullString
		uID, uName                                sql.NullString
		uIsAdmin                                  sql.NullInt64
		uDisabledAt                               sql.NullInt64
	)
	err := row.Scan(&keyID,
		&userID, &scopeTenant, &scopeRepo, &scopePerm,
		&revokedAt, &keyLabel,
		&uID, &uName, &uIsAdmin, &uDisabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil, auth.ErrInvalidCredential
	}
	if err != nil {
		return nil, "", nil, err
	}
	if revokedAt.Valid {
		return nil, "", nil, auth.ErrTokenRevoked
	}

	if userID.Valid {
		// User key. The user row must exist (JOIN guarantees it if user_id is
		// set and the FK is intact) and not be disabled.
		if uDisabledAt.Valid {
			return nil, "", nil, auth.ErrUserDisabled
		}
		actor := &auth.Actor{
			UserID:  uID.String,
			Name:    uName.String,
			IsAdmin: uIsAdmin.Valid && uIsAdmin.Int64 != 0,
		}
		return actor, keyID, nil, nil
	}

	// Deploy key. Build a synthetic actor; scope drives permission.
	label := keyLabel.String
	if label == "" {
		label = "(unlabeled)"
	}
	actor := &auth.Actor{
		UserID: "deploy:" + keyID,
		Name:   "deploy-key:" + label,
	}
	perm, err := permFromText(scopePerm.String)
	if err != nil {
		return nil, "", nil, err
	}
	return actor, keyID, &auth.Scope{
		Tenant: scopeTenant.String,
		Repo:   scopeRepo.String,
		Perm:   perm,
	}, nil
}

// TouchTokenUsage implements auth.Store. A missing tokenID is not an error.
func (s *Store) TouchTokenUsage(ctx context.Context, tokenID string) error {
	if tokenID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE id = ?`, time.Now().Unix(), tokenID,
	)
	return err
}

// nullableString returns a sql.NullString; Valid is true iff s is non-empty.
func nullableString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// permToText and permFromText delegate to the exported auth package helpers.
// They are retained as package-level aliases so existing callers within this
// file continue to compile without changes; a future cleanup can inline them.
//
// TODO(cleanup): replace call sites with auth.PermToText / auth.PermFromText
// directly and remove these wrappers.
func permToText(p auth.Perm) string            { return auth.PermToText(p) }
func permFromText(s string) (auth.Perm, error) { return auth.PermFromText(s) }

// isCheckViolation reports whether err looks like a SQLite CHECK constraint
// failure. modernc.org/sqlite uses the message "CHECK constraint failed".
func isCheckViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "CHECK constraint failed")
}

// isFingerprintUniqueViolation reports whether err is a UNIQUE constraint
// failure specifically on the ssh_keys fingerprint column/index.
func isFingerprintUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// modernc.org/sqlite formats UNIQUE errors as:
	//   "UNIQUE constraint failed: ssh_keys.fingerprint" OR
	//   "constraint failed: UNIQUE: ssh_keys.fingerprint"
	// The index name ssh_keys_fingerprint_idx may also appear, but checking
	// for the column reference is more specific.
	return (strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")) &&
		(strings.Contains(msg, "ssh_keys.fingerprint") ||
			strings.Contains(msg, "fingerprint"))
}

// AddSSHKey persists an ssh_keys row. Implements auth.Store.
func (s *Store) AddSSHKey(ctx context.Context, k auth.SSHKey) error {
	hasUser := k.UserID != ""
	hasScope := k.ScopeTenant != "" || k.ScopeRepo != "" || k.ScopePerm != auth.PermNone
	if hasUser == hasScope {
		return fmt.Errorf("invalid ssh key shape: must set exactly one of user_id or scope_*")
	}

	var (
		userID      sql.NullString
		scopeTenant sql.NullString
		scopeRepo   sql.NullString
		scopePerm   sql.NullString
	)
	if hasUser {
		userID = sql.NullString{String: k.UserID, Valid: true}
	} else {
		scopeTenant = sql.NullString{String: k.ScopeTenant, Valid: true}
		scopeRepo = sql.NullString{String: k.ScopeRepo, Valid: true}
		scopePerm = sql.NullString{String: permToText(k.ScopePerm), Valid: true}
	}

	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ssh_keys (id, fingerprint, public_key, key_type, label,
		                      created_at, user_id, scope_tenant, scope_repo, scope_perm)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, k.ID, k.Fingerprint, k.PublicKey, k.KeyType,
		nullableString(k.Label), now,
		userID, scopeTenant, scopeRepo, scopePerm)

	if err != nil {
		if isFingerprintUniqueViolation(err) {
			return auth.ErrDuplicateFingerprint
		}
		if isCheckViolation(err) {
			return fmt.Errorf("invalid ssh key: %w", err)
		}
		return err
	}
	return nil
}

// ListSSHKeysForUser returns all keys belonging to userID, ordered by created_at ASC.
func (s *Store) ListSSHKeysForUser(ctx context.Context, userID string) ([]auth.SSHKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, fingerprint, public_key, key_type, label,
		       created_at, last_used_at, revoked_at,
		       user_id, scope_tenant, scope_repo, scope_perm
		FROM ssh_keys
		WHERE user_id = ?
		ORDER BY created_at ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSSHKeys(rows)
}

// ListSSHKeysForRepo returns all deploy keys bound to (tenant, repo), ordered by created_at ASC.
func (s *Store) ListSSHKeysForRepo(ctx context.Context, tenant, repo string) ([]auth.SSHKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, fingerprint, public_key, key_type, label,
		       created_at, last_used_at, revoked_at,
		       user_id, scope_tenant, scope_repo, scope_perm
		FROM ssh_keys
		WHERE scope_tenant = ? AND scope_repo = ?
		ORDER BY created_at ASC
	`, tenant, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSSHKeys(rows)
}

// scanSSHKeys scans a *sql.Rows result set into a slice of auth.SSHKey values.
func scanSSHKeys(rows *sql.Rows) ([]auth.SSHKey, error) {
	var out []auth.SSHKey
	for rows.Next() {
		var k auth.SSHKey
		var label, userID, scopeTenant, scopeRepo, scopePerm sql.NullString
		var lastUsedAt, revokedAt sql.NullInt64
		if err := rows.Scan(
			&k.ID, &k.Fingerprint, &k.PublicKey, &k.KeyType, &label,
			&k.CreatedAt, &lastUsedAt, &revokedAt,
			&userID, &scopeTenant, &scopeRepo, &scopePerm,
		); err != nil {
			return nil, err
		}
		k.Label = label.String
		if lastUsedAt.Valid {
			k.LastUsedAt = lastUsedAt.Int64
		}
		if revokedAt.Valid {
			k.RevokedAt = revokedAt.Int64
		}
		if userID.Valid {
			k.UserID = userID.String
		}
		if scopeTenant.Valid {
			k.ScopeTenant = scopeTenant.String
		}
		if scopeRepo.Valid {
			k.ScopeRepo = scopeRepo.String
		}
		if scopePerm.Valid {
			p, err := permFromText(scopePerm.String)
			if err != nil {
				return nil, err
			}
			k.ScopePerm = p
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeSSHKey sets revoked_at to now for the key identified by full ID or
// unique prefix. Returns auth.ErrNoSuchKey if no key matches, or a wrapped
// auth.ErrConflict if the prefix is ambiguous (matches more than one key).
func (s *Store) RevokeSSHKey(ctx context.Context, keyIDOrPrefix string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM ssh_keys WHERE id LIKE ? || '%'`, keyIDOrPrefix)
	if err != nil {
		return err
	}
	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		matches = append(matches, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if len(matches) == 0 {
		return auth.ErrNoSuchKey
	}
	if len(matches) > 1 {
		return fmt.Errorf("%w: prefix %q matches %d keys", auth.ErrConflict, keyIDOrPrefix, len(matches))
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE ssh_keys SET revoked_at = strftime('%s','now') WHERE id = ?`,
		matches[0],
	)
	return err
}

// TouchSSHKeyUsage updates last_used_at to now for the given key ID.
// A missing keyID is not an error — UPDATE with no rows affected returns nil.
func (s *Store) TouchSSHKeyUsage(ctx context.Context, keyID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ssh_keys SET last_used_at = strftime('%s','now') WHERE id = ?`,
		keyID,
	)
	return err
}

// Compile-time check that *Store satisfies auth.Store.
var _ auth.Store = (*Store)(nil)
