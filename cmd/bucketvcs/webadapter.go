package main

import (
	"context"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/web"
)

// webAdapter implements web.DataStore over *sqlitestore.Store, converting the
// store's Repo type into the web view type. It lives in the composition root so
// the internal/web package never imports the storage layer.
type webAdapter struct{ s *sqlitestore.Store }

func newWebAdapter(s *sqlitestore.Store) *webAdapter { return &webAdapter{s: s} }

func (a *webAdapter) VerifyPassword(ctx context.Context, u, p string) (*auth.Actor, error) {
	return a.s.VerifyPassword(ctx, u, p)
}
func (a *webAdapter) CreateSession(ctx context.Context, userID, provider string, ttl time.Duration) (string, error) {
	return a.s.CreateSession(ctx, userID, provider, ttl)
}
func (a *webAdapter) LookupSession(ctx context.Context, raw string) (*auth.Session, error) {
	return a.s.LookupSession(ctx, raw)
}
func (a *webAdapter) TouchSession(ctx context.Context, raw string, ttl time.Duration) error {
	return a.s.TouchSession(ctx, raw, ttl)
}
func (a *webAdapter) DeleteSession(ctx context.Context, raw string) error {
	return a.s.DeleteSession(ctx, raw)
}
func (a *webAdapter) ListAccessibleRepos(ctx context.Context, actor *auth.Actor) ([]web.Repo, error) {
	rs, err := a.s.ListAccessibleRepos(ctx, actor)
	if err != nil {
		return nil, err
	}
	out := make([]web.Repo, 0, len(rs))
	for _, r := range rs {
		out = append(out, web.Repo{Tenant: r.Tenant, Name: r.Name, PublicRead: r.PublicRead, CreatedAt: r.CreatedAt})
	}
	return out, nil
}
