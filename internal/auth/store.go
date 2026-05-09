package auth

import "context"

// Store is the persistence and identity-lookup seam used by the gateway
// middleware and the admin CLI. It is transport-neutral: M4 uses
// BasicPassword credentials; M6 will additionally use SSHKeyFingerprint.
//
// All methods take ctx so timeouts and cancellation propagate from the
// HTTP handler. Implementations must honor ctx promptly.
type Store interface {
	// VerifyCredential validates a credential and returns:
	//   - actor: the principal (synthetic for deploy keys)
	//   - credentialID: tokens.id for BasicPassword, ssh_keys.id for SSHKeyFingerprint
	//   - scope: nil for HTTP token credentials and user SSH keys; populated
	//            with (Tenant, Repo, Perm) for deploy-key SSH credentials so
	//            the gateway can short-circuit per-repo permission lookup
	//
	// Errors:
	//   ErrInvalidCredential — credential did not match any record
	//   ErrTokenExpired      — record matched but expires_at < now
	//   ErrTokenRevoked      — record matched but revoked_at != null
	//   ErrUserDisabled      — record matched but user disabled_at != null
	VerifyCredential(ctx context.Context, c Credential) (actor *Actor, credentialID string, scope *Scope, err error)

	// LookupRepoPerm returns the actor's permission level on (tenant, repo).
	// Anonymous actors (nil) return PermNone without consulting storage.
	// is_admin actors return PermAdmin without consulting permission rows.
	LookupRepoPerm(ctx context.Context, actor *Actor, tenant, repo string) (Perm, error)

	// GetRepoFlags returns the per-repo authorization-relevant flags.
	// Returns ErrNoSuchRepo if (tenant, repo) is not registered.
	GetRepoFlags(ctx context.Context, tenant, repo string) (RepoFlags, error)

	// TouchTokenUsage updates the last_used_at timestamp for the token id
	// to time.Now(). Best-effort: callers run this in a fire-and-forget
	// goroutine off the request hot path. A missing tokenID is not an
	// error; implementations may no-op silently.
	TouchTokenUsage(ctx context.Context, tokenID string) error

	// AddSSHKey persists an ssh_keys row. The caller computes ID, Fingerprint,
	// PublicKey, KeyType, Label, and either UserID (user key) or
	// ScopeTenant+ScopeRepo+ScopePerm (deploy key). Returns
	// ErrDuplicateFingerprint if Fingerprint already exists.
	AddSSHKey(ctx context.Context, k SSHKey) error

	// ListSSHKeysForUser returns all keys belonging to userID, including
	// revoked. Returns nil slice (not error) if user has no keys.
	ListSSHKeysForUser(ctx context.Context, userID string) ([]SSHKey, error)

	// ListSSHKeysForRepo returns all deploy keys bound to (tenant, repo),
	// including revoked. Returns nil slice if repo has none.
	ListSSHKeysForRepo(ctx context.Context, tenant, repo string) ([]SSHKey, error)

	// RevokeSSHKey sets revoked_at to now. keyIDOrPrefix may be the full ID
	// or any unique prefix. Returns ErrNoSuchKey if no match, or ErrConflict
	// if the prefix matches multiple rows.
	RevokeSSHKey(ctx context.Context, keyIDOrPrefix string) error

	// TouchSSHKeyUsage updates last_used_at. Best-effort: missing keyID is
	// not an error.
	TouchSSHKeyUsage(ctx context.Context, keyID string) error

	// Close releases backing resources (DB connections etc).
	Close() error
}
