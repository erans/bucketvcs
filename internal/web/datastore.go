package web

import (
	"context"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// Repo is the web view of a repository (decoupled from the storage layer's type).
type Repo struct {
	Tenant     string
	Name       string
	PublicRead bool
	CreatedAt  int64
}

// UserInfo is the web view of a user row for the admin area.
type UserInfo struct {
	ID        string
	Name      string
	Email     string
	IsAdmin   bool
	Disabled  bool
	CreatedAt int64
}

// TokenInfo is the web view of a token row (no secret hash).
type TokenInfo struct {
	ID         string
	Label      string
	Scopes     auth.TokenScope
	CreatedAt  int64
	ExpiresAt  *int64
	LastUsedAt *int64
	RevokedAt  *int64
}

// RepoGrant mirrors sqlitestore.RepoGrant for the web layer.
type RepoGrant struct {
	UserName string
	Perm     string
}

// DataStore is the read/identity surface the web UI needs. It is implemented in
// the composition root (cmd/bucketvcs) by an adapter over *sqlitestore.Store, and
// by a fake in tests.
type DataStore interface {
	VerifyPassword(ctx context.Context, userName, plaintext string) (*auth.Actor, error)
	CreateSession(ctx context.Context, userID, provider string, ttl time.Duration) (string, error)
	LookupSession(ctx context.Context, rawID string) (*auth.Session, error)
	TouchSession(ctx context.Context, rawID string, ttl time.Duration) error
	DeleteSession(ctx context.Context, rawID string) error
	// DeleteSessionsForUser deletes all of a user's sessions except the one
	// identified by exceptRawID ("" = delete all). Returns the number deleted.
	// Used on password change to revoke attacker-held cookies.
	DeleteSessionsForUser(ctx context.Context, userID, exceptRawID string) (int64, error)

	// ListSessionsForUser returns the user's sessions newest-first, marking the
	// one whose stored hash matches the SHA-256 of currentRawID. The raw cookie id is never
	// returned (only the SHA-256 hash). DeleteSessionByHashForUser is user-scoped
	// (a cross-user hash affects 0 rows); ListAllSessions/DeleteSessionByHash are
	// the unscoped admin surface. ListAllSessions returns at most limit rows
	// (limit <= 0 = unlimited) plus the total session count.
	ListSessionsForUser(ctx context.Context, userID, currentRawID string) ([]auth.SessionInfo, error)
	DeleteSessionByHashForUser(ctx context.Context, userID, idHash string) (int64, error)
	ListAllSessions(ctx context.Context, limit int) ([]auth.AdminSessionInfo, int, error)
	DeleteSessionByHash(ctx context.Context, idHash string) (int64, error)
	ListAccessibleRepos(ctx context.Context, actor *auth.Actor) ([]Repo, error)
	// GetVisibleRepo returns the repo if the actor may view it, or an error.
	// The web layer treats any error as 404 (anti-enumeration).
	GetVisibleRepo(ctx context.Context, actor *auth.Actor, tenant, name string) (*Repo, error)

	// LookupRepoPerm returns the actor's effective permission on (tenant, repo).
	// Used by the repo-settings authz gate (PermAdmin or global admin).
	LookupRepoPerm(ctx context.Context, actor *auth.Actor, tenant, repo string) (auth.Perm, error)

	// GetRepoFlags returns the per-repo authorization-relevant flags (public-read).
	// Returns auth.ErrNoSuchRepo when the repo is not registered.
	GetRepoFlags(ctx context.Context, tenant, repo string) (auth.RepoFlags, error)

	// SetRepoPublic toggles anonymous-read visibility for (tenant, repo).
	SetRepoPublic(ctx context.Context, tenant, repo string, public bool) error

	// RenameRepo renames (tenant, oldName) to (tenant, newName), propagating
	// the new name to every dependent table. Returns auth.ErrNoSuchRepo when
	// the source is absent, or sqlitestore.ErrRepoExists when the destination
	// already exists.
	RenameRepo(ctx context.Context, tenant, oldName, newName string) error

	// DeleteRepoCascade deletes the repos row and its non-webhook dependents,
	// leaving webhook_endpoints/_deliveries intact so a pending repo.deleted
	// delivery can drain. Storage objects are NOT purged.
	DeleteRepoCascade(ctx context.Context, tenant, repo string) error

	// RegisterRepoIfNew inserts the (tenant, name) repos row if absent and
	// reports whether a new row was actually inserted (false => already
	// registered). Used by the admin repo-registration page.
	RegisterRepoIfNew(ctx context.Context, tenant, name string) (bool, error)

	// OIDC (Phase 1.5)
	FindUserByEmail(ctx context.Context, email string) (*auth.Actor, error)
	FindIdentity(ctx context.Context, issuer, subject string) (*auth.Actor, error)
	LinkIdentity(ctx context.Context, userID, issuer, subject, email string) error

	// User profile (Phase 3 settings pages)
	GetUserByName(ctx context.Context, name string) (*auth.User, error)
	SetPassword(ctx context.Context, userName, plaintext string) error
	HasPassword(ctx context.Context, userName string) (bool, error)

	// Tokens (self-service; ownership enforced by handlers).
	ListTokensForUser(ctx context.Context, name string) ([]TokenInfo, error)
	GetTokenOwner(ctx context.Context, id string) (userID string, err error)
	CreateToken(ctx context.Context, id, userID, secretHash, label string,
		expiresAt *int64, scopes auth.TokenScope) error
	RevokeToken(ctx context.Context, id string) error
	RotateToken(ctx context.Context, id, newSecretHash string) error

	// SSH keys (self-service user keys; ownership enforced by handlers).
	ListSSHKeysForUser(ctx context.Context, userID string) ([]auth.SSHKey, error)
	AddSSHKey(ctx context.Context, k auth.SSHKey) error
	RevokeSSHKey(ctx context.Context, keyIDOrPrefix string) error

	// Repo access (admin tab): explicit user grants + repo-scoped deploy keys.
	ListRepoGrants(ctx context.Context, tenant, repo string) ([]RepoGrant, error)
	Grant(ctx context.Context, userName, tenant, repo, perm string) error
	RevokeRepoPermission(ctx context.Context, userName, tenant, repo string) error
	ListSSHKeysForRepo(ctx context.Context, tenant, repo string) ([]auth.SSHKey, error)

	// Admin area: global user management.
	ListUsers(ctx context.Context) ([]UserInfo, error)
	CreateUser(ctx context.Context, name string, isAdmin bool) (string, error)
	SetUserDisabled(ctx context.Context, name string, disabled bool) error
	DeleteUser(ctx context.Context, name string) error
	SetEmail(ctx context.Context, userName, email string) error
}
