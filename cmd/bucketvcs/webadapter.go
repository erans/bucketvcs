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

// Compile-time guards: *webAdapter must satisfy the web DataStore AND the
// optional auth.RepoAliasResolver capability (the web handler type-asserts to
// the latter at runtime; this assertion makes a missing forwarder a build
// failure rather than a silently-dead redirect).
var _ auth.RepoAliasResolver = (*webAdapter)(nil)

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
func (a *webAdapter) DeleteSessionsForUser(ctx context.Context, userID, exceptRawID string) (int64, error) {
	return a.s.DeleteSessionsForUser(ctx, userID, exceptRawID)
}
func (a *webAdapter) ListSessionsForUser(ctx context.Context, userID, currentRawID string) ([]auth.SessionInfo, error) {
	return a.s.ListSessionsForUser(ctx, userID, currentRawID)
}
func (a *webAdapter) DeleteSessionByHashForUser(ctx context.Context, userID, idHash string) (int64, error) {
	return a.s.DeleteSessionByHashForUser(ctx, userID, idHash)
}
func (a *webAdapter) ListAllSessions(ctx context.Context, limit int) ([]auth.AdminSessionInfo, int, error) {
	return a.s.ListAllSessions(ctx, limit)
}
func (a *webAdapter) SessionOwnerByHash(ctx context.Context, idHash string) (string, string, error) {
	return a.s.SessionOwnerByHash(ctx, idHash)
}
func (a *webAdapter) DeleteSessionByHash(ctx context.Context, idHash string) (int64, error) {
	return a.s.DeleteSessionByHash(ctx, idHash)
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

func (a *webAdapter) RegisterRepoIfNew(ctx context.Context, tenant, name string) (bool, error) {
	return a.s.RegisterRepoIfNew(ctx, tenant, name)
}

// ResolveAlias forwards to the store so the web UI can 302-redirect renamed-away
// repo names. Required for *webAdapter to satisfy auth.RepoAliasResolver (the
// web handler type-asserts its DataStore to that interface); without this
// forwarder the assertion fails and the redirect silently no-ops.
func (a *webAdapter) ResolveAlias(ctx context.Context, tenant, name string) (string, bool, error) {
	return a.s.ResolveAlias(ctx, tenant, name)
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

func (a *webAdapter) ListRepoGrants(ctx context.Context, tenant, repo string) ([]web.RepoGrant, error) {
	rows, err := a.s.ListRepoGrants(ctx, tenant, repo)
	if err != nil {
		return nil, err
	}
	out := make([]web.RepoGrant, len(rows))
	for i, r := range rows {
		out[i] = web.RepoGrant{UserName: r.UserName, Perm: r.Perm}
	}
	return out, nil
}

func (a *webAdapter) Grant(ctx context.Context, userName, tenant, repo, perm string) error {
	return a.s.Grant(ctx, userName, tenant, repo, perm)
}

func (a *webAdapter) RevokeRepoPermission(ctx context.Context, userName, tenant, repo string) error {
	return a.s.RevokeRepoPermission(ctx, userName, tenant, repo)
}

func (a *webAdapter) ListSSHKeysForRepo(ctx context.Context, tenant, repo string) ([]auth.SSHKey, error) {
	return a.s.ListSSHKeysForRepo(ctx, tenant, repo)
}

func (a *webAdapter) ListUsers(ctx context.Context) ([]web.UserInfo, error) {
	users, err := a.s.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]web.UserInfo, 0, len(users))
	for _, u := range users {
		out = append(out, web.UserInfo{
			ID:        u.ID,
			Name:      u.Name,
			Email:     u.Email,
			IsAdmin:   u.IsAdmin,
			Disabled:  u.DisabledAt != nil,
			CreatedAt: u.CreatedAt,
		})
	}
	return out, nil
}

func (a *webAdapter) CreateUser(ctx context.Context, name string, isAdmin bool) (string, error) {
	return a.s.CreateUser(ctx, name, isAdmin)
}

func (a *webAdapter) SetUserDisabled(ctx context.Context, name string, disabled bool) error {
	return a.s.SetUserDisabled(ctx, name, disabled)
}

func (a *webAdapter) DeleteUser(ctx context.Context, name string) error {
	return a.s.DeleteUser(ctx, name)
}

func (a *webAdapter) SetEmail(ctx context.Context, userName, email string) error {
	return a.s.SetEmail(ctx, userName, email)
}
