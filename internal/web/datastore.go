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
}
