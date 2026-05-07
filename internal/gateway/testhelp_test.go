package gateway

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// newAnonymousTestAuthStore returns a store with the repo registered and
// public_read = pub. No users.
func newAnonymousTestAuthStore(t *testing.T, tenant, repo string, pub bool) *sqlitestore.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := sqlitestore.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.RegisterRepo(context.Background(), tenant, repo); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	if pub {
		if err := s.SetRepoPublic(context.Background(), tenant, repo, true); err != nil {
			t.Fatalf("SetRepoPublic: %v", err)
		}
	}
	return s
}

// permissiveAuthStore is a test-only auth.Store wrapper that grants
// PermWrite to ALL callers (including anonymous). It is used by M3-era
// gateway tests that exercised receive-pack without an auth layer; under
// M4 the default flow requires a real credential for writes, but those
// tests only care about the protocol behavior of the receive-pack
// handler itself, not the auth middleware (covered separately by
// auth_test.go).
type permissiveAuthStore struct {
	tenant, repo string
}

// newPermissiveAuthStore returns a store that registers no users and
// reports the (tenant, repo) as PublicRead-true with implicit write
// permission for all actors. Useful for receive-pack protocol tests.
func newPermissiveAuthStore(_ *testing.T, tenant, repo string) auth.Store {
	return &permissiveAuthStore{tenant: tenant, repo: repo}
}

func (p *permissiveAuthStore) VerifyCredential(ctx context.Context, c auth.Credential) (*auth.Actor, string, error) {
	return nil, "", auth.ErrInvalidCredential
}
func (p *permissiveAuthStore) LookupRepoPerm(ctx context.Context, _ *auth.Actor, _, _ string) (auth.Perm, error) {
	return auth.PermWrite, nil
}
func (p *permissiveAuthStore) GetRepoFlags(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
	if tenant != p.tenant || repo != p.repo {
		return auth.RepoFlags{}, auth.ErrNoSuchRepo
	}
	return auth.RepoFlags{PublicRead: true}, nil
}
func (p *permissiveAuthStore) TouchTokenUsage(ctx context.Context, _ string) error { return nil }
func (p *permissiveAuthStore) Close() error                                        { return nil }
