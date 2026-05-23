package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/conformance"
)

type sqliteSeeder struct{ s *Store }

func (sd *sqliteSeeder) CreateUser(ctx context.Context, name string, isAdmin bool) string {
	id, err := sd.s.CreateUser(ctx, name, isAdmin)
	if err != nil {
		panic(err)
	}
	return id
}
func (sd *sqliteSeeder) CreateToken(ctx context.Context, userID, tokenID, hash string, exp *int64) {
	if err := sd.s.CreateToken(ctx, tokenID, userID, hash, "", exp, auth.ScopeLegacy); err != nil {
		panic(err)
	}
}
func (sd *sqliteSeeder) RevokeToken(ctx context.Context, tokenID string) {
	if err := sd.s.RevokeToken(ctx, tokenID); err != nil {
		panic(err)
	}
}
func (sd *sqliteSeeder) SetUserDisabled(ctx context.Context, name string, dis bool) {
	if err := sd.s.SetUserDisabled(ctx, name, dis); err != nil {
		panic(err)
	}
}
func (sd *sqliteSeeder) RegisterRepo(ctx context.Context, tenant, repo string) {
	if err := sd.s.RegisterRepo(ctx, tenant, repo); err != nil {
		panic(err)
	}
}
func (sd *sqliteSeeder) SetRepoPublic(ctx context.Context, tenant, repo string, pub bool) {
	if err := sd.s.SetRepoPublic(ctx, tenant, repo, pub); err != nil {
		panic(err)
	}
}
func (sd *sqliteSeeder) Grant(ctx context.Context, user, tenant, repo, perm string) {
	if err := sd.s.Grant(ctx, user, tenant, repo, perm); err != nil {
		panic(err)
	}
}

// M6 SSH-key seeder methods.

func (sd *sqliteSeeder) SeedUser(t *testing.T, name string, isAdmin bool) string {
	t.Helper()
	id, err := sd.s.CreateUser(context.Background(), name, isAdmin)
	if err != nil {
		t.Fatalf("SeedUser %q: %v", name, err)
	}
	return id
}

func (sd *sqliteSeeder) SeedRepo(t *testing.T, tenant, repo string, publicRead bool) {
	t.Helper()
	if err := sd.s.RegisterRepo(context.Background(), tenant, repo); err != nil {
		t.Fatalf("SeedRepo %s/%s: %v", tenant, repo, err)
	}
	if publicRead {
		if err := sd.s.SetRepoPublic(context.Background(), tenant, repo, true); err != nil {
			t.Fatalf("SeedRepo SetRepoPublic %s/%s: %v", tenant, repo, err)
		}
	}
}

func (sd *sqliteSeeder) DisableUser(t *testing.T, userID string) {
	t.Helper()
	if err := sd.s.DisableUserByID(context.Background(), userID); err != nil {
		t.Fatalf("DisableUser %q: %v", userID, err)
	}
}

func (sd *sqliteSeeder) DeleteUser(t *testing.T, userID string) {
	t.Helper()
	if err := sd.s.DeleteUserByID(context.Background(), userID); err != nil {
		t.Fatalf("DeleteUser %q: %v", userID, err)
	}
}

func (sd *sqliteSeeder) DeleteRepo(t *testing.T, tenant, repo string) {
	t.Helper()
	if err := sd.s.DeleteRepo(context.Background(), tenant, repo); err != nil {
		t.Fatalf("DeleteRepo %s/%s: %v", tenant, repo, err)
	}
}

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) (auth.Store, conformance.Seeder) {
		dir := t.TempDir()
		s, err := Open(filepath.Join(dir, "auth.db"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s, &sqliteSeeder{s: s}
	})
}
