package auth

import "context"

// Store is the persistence and identity-lookup seam used by the gateway
// middleware and the admin CLI. It is transport-neutral: M4 uses
// BasicPassword credentials; M6 will additionally use SSHKeyFingerprint.
//
// All methods take ctx so timeouts and cancellation propagate from the
// HTTP handler. Implementations must honor ctx promptly.
type Store interface {
	// VerifyCredential validates a credential and returns the associated
	// actor plus the originating token id (empty for credential types
	// that don't carry an id).
	//
	// Errors:
	//   ErrInvalidCredential   — credential did not match any record
	//   ErrTokenExpired        — record matched but expires_at < now
	//   ErrTokenRevoked        — record matched but revoked_at != null
	//   ErrUserDisabled        — record matched but user disabled_at != null
	VerifyCredential(ctx context.Context, c Credential) (actor *Actor, tokenID string, err error)

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

	// Close releases backing resources (DB connections etc).
	Close() error
}
