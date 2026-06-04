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

// DataStore is the read/identity surface the web UI needs. It is implemented in
// the composition root (cmd/bucketvcs) by an adapter over *sqlitestore.Store, and
// by a fake in tests.
type DataStore interface {
	VerifyPassword(ctx context.Context, userName, plaintext string) (*auth.Actor, error)
	CreateSession(ctx context.Context, userID, provider string, ttl time.Duration) (string, error)
	LookupSession(ctx context.Context, rawID string) (*auth.Session, error)
	TouchSession(ctx context.Context, rawID string, ttl time.Duration) error
	DeleteSession(ctx context.Context, rawID string) error
	ListAccessibleRepos(ctx context.Context, actor *auth.Actor) ([]Repo, error)
	// GetVisibleRepo returns the repo if the actor may view it, or an error.
	// The web layer treats any error as 404 (anti-enumeration).
	GetVisibleRepo(ctx context.Context, actor *auth.Actor, tenant, name string) (*Repo, error)

	// LookupRepoPerm returns the actor's effective permission on (tenant, repo).
	// Used by the repo-settings authz gate (PermAdmin or global admin).
	LookupRepoPerm(ctx context.Context, actor *auth.Actor, tenant, repo string) (auth.Perm, error)

	// OIDC (Phase 1.5)
	FindUserByEmail(ctx context.Context, email string) (*auth.Actor, error)
	FindIdentity(ctx context.Context, issuer, subject string) (*auth.Actor, error)
	LinkIdentity(ctx context.Context, userID, issuer, subject, email string) error

	// User profile (Phase 3 settings pages)
	GetUserByName(ctx context.Context, name string) (*auth.User, error)
	SetPassword(ctx context.Context, userName, plaintext string) error
	HasPassword(ctx context.Context, userName string) (bool, error)
}
