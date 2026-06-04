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

func (a *webAdapter) GetVisibleRepo(ctx context.Context, actor *auth.Actor, tenant, name string) (*web.Repo, error) {
	r, err := a.s.GetVisibleRepo(ctx, actor, tenant, name)
	if err != nil {
		return nil, err
	}
	return &web.Repo{Tenant: r.Tenant, Name: r.Name, PublicRead: r.PublicRead, CreatedAt: r.CreatedAt}, nil
}

func (a *webAdapter) LookupRepoPerm(ctx context.Context, actor *auth.Actor, tenant, repo string) (auth.Perm, error) {
	return a.s.LookupRepoPerm(ctx, actor, tenant, repo)
}

func (a *webAdapter) GetRepoFlags(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
	return a.s.GetRepoFlags(ctx, tenant, repo)
}

func (a *webAdapter) SetRepoPublic(ctx context.Context, tenant, repo string, public bool) error {
	return a.s.SetRepoPublic(ctx, tenant, repo, public)
}

func (a *webAdapter) RenameRepo(ctx context.Context, tenant, oldName, newName string) error {
	return a.s.RenameRepo(ctx, tenant, oldName, newName)
}

func (a *webAdapter) DeleteRepoCascade(ctx context.Context, tenant, repo string) error {
	return a.s.DeleteRepoCascade(ctx, tenant, repo)
}

func (a *webAdapter) FindUserByEmail(ctx context.Context, email string) (*auth.Actor, error) {
	return a.s.FindUserByEmail(ctx, email)
}
func (a *webAdapter) FindIdentity(ctx context.Context, issuer, subject string) (*auth.Actor, error) {
	return a.s.FindIdentity(ctx, issuer, subject)
}
func (a *webAdapter) LinkIdentity(ctx context.Context, userID, issuer, subject, email string) error {
	return a.s.LinkIdentity(ctx, userID, issuer, subject, email)
}
func (a *webAdapter) GetUserByName(ctx context.Context, name string) (*auth.User, error) {
	return a.s.GetUserByName(ctx, name)
}
func (a *webAdapter) SetPassword(ctx context.Context, userName, plaintext string) error {
	return a.s.SetPassword(ctx, userName, plaintext)
}
func (a *webAdapter) HasPassword(ctx context.Context, userName string) (bool, error) {
	return a.s.HasPassword(ctx, userName)
}

func (a *webAdapter) ListTokensForUser(ctx context.Context, name string) ([]web.TokenInfo, error) {
	rows, err := a.s.ListTokensForUser(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]web.TokenInfo, 0, len(rows))
	for _, t := range rows {
		out = append(out, web.TokenInfo{
			ID:         t.ID,
			Label:      t.Label,
			Scopes:     t.Scopes,
			CreatedAt:  t.CreatedAt,
			ExpiresAt:  t.ExpiresAt,
			LastUsedAt: t.LastUsedAt,
			RevokedAt:  t.RevokedAt,
		})
	}
	return out, nil
}

func (a *webAdapter) GetTokenOwner(ctx context.Context, id string) (string, error) {
	t, err := a.s.GetTokenByID(ctx, id)
	if err != nil {
		return "", err
	}
	return t.UserID, nil
}

func (a *webAdapter) CreateToken(ctx context.Context, id, userID, secretHash, label string, expiresAt *int64, scopes auth.TokenScope) error {
	return a.s.CreateToken(ctx, id, userID, secretHash, label, expiresAt, scopes, "", "", "")
}

func (a *webAdapter) RevokeToken(ctx context.Context, id string) error {
	return a.s.RevokeToken(ctx, id)
}

func (a *webAdapter) RotateToken(ctx context.Context, id, newSecretHash string) error {
	return a.s.RotateToken(ctx, id, newSecretHash)
}

func (a *webAdapter) ListSSHKeysForUser(ctx context.Context, userID string) ([]auth.SSHKey, error) {
	return a.s.ListSSHKeysForUser(ctx, userID)
}

func (a *webAdapter) AddSSHKey(ctx context.Context, k auth.SSHKey) error {
	return a.s.AddSSHKey(ctx, k)
}

func (a *webAdapter) RevokeSSHKey(ctx context.Context, keyIDOrPrefix string) error {
	return a.s.RevokeSSHKey(ctx, keyIDOrPrefix)
}
