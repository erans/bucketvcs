package diffharness

// auth_helper_test.go provides the small auth.Store fixtures the diffharness
// oracles need now that gateway.Options requires an AuthStore (M4 Task 18+).
//
// Two flavors:
//
//   - newDiffharnessAuthStore: registers (tenant, repo) and marks it
//     public_read=true. Suitable for clone oracles which only need
//     anonymous read access — equivalent to the old AuthMode=Anonymous
//     behavior for read paths.
//
//   - newDiffharnessAuthStoreWithAdminToken: same registration plus a
//     freshly-minted admin user and token. Returns the cleartext token
//     and username so push oracles can drive `git push` with Basic auth.
//     Admins short-circuit Decide so no per-repo grant is needed.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// newDiffharnessAuthStore creates a fresh sqlitestore with the given
// (tenant, repo) registered as public_read=true. Cleanup is registered via
// t.Cleanup; caller must NOT close the returned store.
func newDiffharnessAuthStore(t *testing.T, tenant, repo string) *sqlitestore.Store {
	t.Helper()
	s, err := sqlitestore.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, tenant, repo); err != nil {
		t.Fatalf("RegisterRepo(%s/%s): %v", tenant, repo, err)
	}
	if err := s.SetRepoPublic(ctx, tenant, repo, true); err != nil {
		t.Fatalf("SetRepoPublic(%s/%s): %v", tenant, repo, err)
	}
	return s
}

// newDiffharnessAuthStoreWithAdminToken creates a fresh sqlitestore with
// the given (tenant, repo) registered (NOT public_read), plus an admin user
// "diffadmin" with a freshly-minted token. Returns the store, the admin
// username, and the cleartext token string. Cleanup is registered via
// t.Cleanup; caller must NOT close the returned store.
//
// The admin role short-circuits LookupRepoPerm to PermAdmin without
// consulting permission rows (see sqlitestore.Store.LookupRepoPerm), so
// no explicit Grant is needed and the same credential covers both read
// and write paths.
func newDiffharnessAuthStoreWithAdminToken(t *testing.T, tenant, repo string) (store *sqlitestore.Store, username, token string) {
	t.Helper()
	s, err := sqlitestore.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, tenant, repo); err != nil {
		t.Fatalf("RegisterRepo(%s/%s): %v", tenant, repo, err)
	}
	const adminName = "diffadmin"
	uid, err := s.CreateUser(ctx, adminName, true)
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", adminName, err)
	}
	tokStr, id, secret, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	if err := s.CreateToken(ctx, id, uid, hash, "diffharness", nil, auth.ScopeLegacy); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return s, adminName, tokStr
}
