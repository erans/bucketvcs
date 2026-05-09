package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestOpen_CreatesFileAndAppliesMigrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var pragma string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&pragma); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if pragma != "wal" {
		t.Errorf("journal_mode = %q, want wal", pragma)
	}

	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}

	var v int
	if err := s.db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&v); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if v != 1 {
		t.Errorf("schema_version = %d, want 1", v)
	}
}

func TestOpen_ReopenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.db")
	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close a: %v", err)
	}
	b, err := Open(path)
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	defer b.Close()
	if err := b.db.PingContext(context.Background()); err != nil {
		t.Fatalf("Ping after reopen: %v", err)
	}
}

func TestCreateUser_AndGetByName(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	id, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id == "" {
		t.Fatal("CreateUser returned empty id")
	}
	got, err := s.GetUserByName(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserByName: %v", err)
	}
	if got.ID != id || got.Name != "alice" || got.IsAdmin {
		t.Fatalf("got %+v", got)
	}
}

func TestCreateUser_DuplicateName(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := s.CreateUser(ctx, "alice", false)
	if !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestSetUserDisabled_AndDelete(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	id, _ := s.CreateUser(ctx, "alice", false)

	if err := s.SetUserDisabled(ctx, "alice", true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	u, _ := s.GetUserByName(ctx, "alice")
	if u.DisabledAt == nil {
		t.Fatal("expected DisabledAt set")
	}
	if err := s.SetUserDisabled(ctx, "alice", false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	u, _ = s.GetUserByName(ctx, "alice")
	if u.DisabledAt != nil {
		t.Fatal("expected DisabledAt cleared")
	}
	if err := s.DeleteUser(ctx, "alice"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.GetUserByName(ctx, "alice"); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Fatalf("want ErrNoSuchUser, got %v", err)
	}
	_ = id
}

func TestDeleteUser_RefusesLastAdmin(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "root", true); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	err := s.DeleteUser(ctx, "root")
	if err == nil || !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("want ErrLastAdmin, got %v", err)
	}
}

// TestSetUserDisabled_RefusesLastEnabledAdmin verifies that disabling the
// only enabled admin is rejected with ErrLastAdmin (M4 ship-gate roborev
// iteration 3 finding 3a). Without this guard an operator could lock
// themselves out of the system by accident.
func TestSetUserDisabled_RefusesLastEnabledAdmin(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "root", true); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	err := s.SetUserDisabled(ctx, "root", true)
	if err == nil || !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("want ErrLastAdmin, got %v", err)
	}
	// Re-enabling (the no-op equivalent here) must still succeed; the
	// guard only fires on the disable path.
	if err := s.SetUserDisabled(ctx, "root", false); err != nil {
		t.Fatalf("re-enable on already-enabled admin: %v", err)
	}
}

// TestDeleteUser_DisabledAdminDoesntCount verifies that DeleteUser's
// last-admin guard counts only ENABLED admins (M4 ship-gate roborev
// iteration 3 finding 3b). Before the fix, two admins where one was
// disabled satisfied the "another admin exists" count, so deleting the
// remaining ENABLED admin succeeded — leaving the system with no usable
// admin account.
func TestDeleteUser_DisabledAdminDoesntCount(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "root", true); err != nil {
		t.Fatalf("create root admin: %v", err)
	}
	if _, err := s.CreateUser(ctx, "alice", true); err != nil {
		t.Fatalf("create alice admin: %v", err)
	}
	// Disable alice. With two enabled admins this is allowed.
	if err := s.SetUserDisabled(ctx, "alice", true); err != nil {
		t.Fatalf("disable alice: %v", err)
	}
	// Now deleting root should fail: alice is admin but disabled, so the
	// remaining-enabled-admin count is zero.
	err := s.DeleteUser(ctx, "root")
	if err == nil || !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("DeleteUser(root) want ErrLastAdmin, got %v", err)
	}
}

func TestListUsers(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_, _ = s.CreateUser(ctx, "alice", false)
	_, _ = s.CreateUser(ctx, "bob", true)
	got, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestCreateToken_AndGet(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)

	exp := time.Now().Add(24 * time.Hour).Unix()
	err := s.CreateToken(ctx, "tokid001AAAAAAAAAAAAAAAA", uid, "$argon2id$v=19$m=65536,t=3,p=4$AAAA$BBBB",
		"laptop", &exp)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	got, err := s.GetTokenByID(ctx, "tokid001AAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatalf("GetTokenByID: %v", err)
	}
	if got.UserID != uid || got.Label != "laptop" {
		t.Fatalf("got %+v", got)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != exp {
		t.Fatalf("ExpiresAt mismatch")
	}
}

func TestRevokeToken(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	_ = s.CreateToken(ctx, "tokid001AAAAAAAAAAAAAAAA", uid, "$argon2id$x", "", nil)
	if err := s.RevokeToken(ctx, "tokid001AAAAAAAAAAAAAAAA"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	tok, _ := s.GetTokenByID(ctx, "tokid001AAAAAAAAAAAAAAAA")
	if tok.RevokedAt == nil {
		t.Fatal("expected RevokedAt set")
	}
}

func TestListTokensForUser(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	_ = s.CreateToken(ctx, "tok1AAAAAAAAAAAAAAAAAAAA", uid, "$argon2id$1", "a", nil)
	_ = s.CreateToken(ctx, "tok2AAAAAAAAAAAAAAAAAAAA", uid, "$argon2id$2", "b", nil)
	rows, err := s.ListTokensForUser(ctx, "alice")
	if err != nil {
		t.Fatalf("ListTokensForUser: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
}

func TestResolveTokenIDPrefix(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	full := "tokABCDE0000000000000000"
	_ = s.CreateToken(ctx, full, uid, "$argon2id$1", "", nil)
	got, err := s.ResolveTokenIDPrefix(ctx, "tokABCDE")
	if err != nil {
		t.Fatalf("ResolveTokenIDPrefix: %v", err)
	}
	if got != full {
		t.Fatalf("got %q want %q", got, full)
	}

	// Unique-prefix violation: add a second token whose id shares the prefix.
	_ = s.CreateToken(ctx, "tokABCDE9999999999999999", uid, "$argon2id$2", "", nil)
	if _, err := s.ResolveTokenIDPrefix(ctx, "tokABC"); !errors.Is(err, ErrAmbiguousPrefix) {
		t.Fatalf("want ErrAmbiguousPrefix, got %v", err)
	}
}

func TestDeleteUser_CascadesTokens(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	// Create another admin so the user-delete is allowed.
	_, _ = s.CreateUser(ctx, "root", true)
	uid, _ := s.CreateUser(ctx, "alice", false)
	_ = s.CreateToken(ctx, "tok1AAAAAAAAAAAAAAAAAAAA", uid, "$argon2id$1", "", nil)
	if err := s.DeleteUser(ctx, "alice"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.GetTokenByID(ctx, "tok1AAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, auth.ErrNoSuchToken) {
		t.Fatalf("want ErrNoSuchToken, got %v", err)
	}
}

func TestRegisterRepo_AndGetFlags(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	flags, err := s.GetRepoFlags(ctx, "acme", "foo")
	if err != nil {
		t.Fatalf("GetRepoFlags: %v", err)
	}
	if flags.PublicRead {
		t.Fatal("default should be private")
	}
}

func TestSetRepoPublic(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "foo")
	if err := s.SetRepoPublic(ctx, "acme", "foo", true); err != nil {
		t.Fatalf("SetRepoPublic on: %v", err)
	}
	flags, _ := s.GetRepoFlags(ctx, "acme", "foo")
	if !flags.PublicRead {
		t.Fatal("expected PublicRead = true")
	}
	_ = s.SetRepoPublic(ctx, "acme", "foo", false)
	flags, _ = s.GetRepoFlags(ctx, "acme", "foo")
	if flags.PublicRead {
		t.Fatal("expected PublicRead = false after toggle off")
	}
}

func TestGetRepoFlags_NoSuchRepo(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.GetRepoFlags(ctx, "ghost", "x"); !errors.Is(err, auth.ErrNoSuchRepo) {
		t.Fatalf("want ErrNoSuchRepo, got %v", err)
	}
}

func TestRegisterRepo_Idempotent(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("second (should be idempotent): %v", err)
	}
}

func TestListRepos(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "foo")
	_ = s.RegisterRepo(ctx, "acme", "bar")
	_ = s.RegisterRepo(ctx, "other", "x")
	got, err := s.ListRepos(ctx, "acme")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	all, _ := s.ListRepos(ctx, "")
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
}

func TestGrantAndLookup(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_, _ = s.CreateUser(ctx, "alice", false)
	_ = s.RegisterRepo(ctx, "acme", "foo")
	if err := s.Grant(ctx, "alice", "acme", "foo", "write"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	u, _ := s.GetUserByName(ctx, "alice")
	a := &auth.Actor{UserID: u.ID, Name: u.Name}
	perm, err := s.LookupRepoPerm(ctx, a, "acme", "foo")
	if err != nil {
		t.Fatalf("LookupRepoPerm: %v", err)
	}
	if perm != auth.PermWrite {
		t.Fatalf("perm = %v, want PermWrite", perm)
	}
}

func TestGrant_RefusesUnregisteredRepo(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_, _ = s.CreateUser(ctx, "alice", false)
	if err := s.Grant(ctx, "alice", "acme", "foo", "read"); !errors.Is(err, auth.ErrNoSuchRepo) {
		t.Fatalf("want ErrNoSuchRepo, got %v", err)
	}
}

func TestRevokeRepoPermission(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_, _ = s.CreateUser(ctx, "alice", false)
	_ = s.RegisterRepo(ctx, "acme", "foo")
	_ = s.Grant(ctx, "alice", "acme", "foo", "read")
	if err := s.RevokeRepoPermission(ctx, "alice", "acme", "foo"); err != nil {
		t.Fatalf("RevokeRepoPermission: %v", err)
	}
	u, _ := s.GetUserByName(ctx, "alice")
	a := &auth.Actor{UserID: u.ID}
	perm, _ := s.LookupRepoPerm(ctx, a, "acme", "foo")
	if perm != auth.PermNone {
		t.Fatalf("perm = %v, want PermNone after revoke", perm)
	}
}

func TestLookupRepoPerm_AdminShortCircuits(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "root", true)
	_ = s.RegisterRepo(ctx, "acme", "foo")
	a := &auth.Actor{UserID: uid, IsAdmin: true}
	perm, err := s.LookupRepoPerm(ctx, a, "acme", "foo")
	if err != nil {
		t.Fatalf("LookupRepoPerm: %v", err)
	}
	if perm != auth.PermAdmin {
		t.Fatalf("perm = %v, want PermAdmin", perm)
	}
}

func TestLookupRepoPerm_NilActorIsPermNone(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	perm, err := s.LookupRepoPerm(ctx, nil, "acme", "foo")
	if err != nil {
		t.Fatalf("LookupRepoPerm(nil): %v", err)
	}
	if perm != auth.PermNone {
		t.Fatalf("perm = %v, want PermNone", perm)
	}
}

func TestVerifyCredential_HappyPath(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = s.CreateToken(ctx, id, uid, hash, "laptop", nil)

	got, gotID, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if err != nil {
		t.Fatalf("VerifyCredential: %v", err)
	}
	if got == nil || got.UserID != uid || got.Name != "alice" {
		t.Fatalf("actor = %+v", got)
	}
	if gotID != id {
		t.Fatalf("returned tokenID = %q want %q", gotID, id)
	}
}

func TestVerifyCredential_BadPassword(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	_, id, _, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret("real-secret-string")
	_ = s.CreateToken(ctx, id, uid, hash, "", nil)

	_, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{
		Username: "alice",
		Password: "bvts_" + id + "_" + strings.Repeat("A", 52),
	})
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("want ErrInvalidCredential, got %v", err)
	}
}

func TestVerifyCredential_UnknownTokenID(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	tok, _, _, _ := auth.GenerateToken()
	_, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("want ErrInvalidCredential, got %v", err)
	}
}

func TestVerifyCredential_Expired(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	past := time.Now().Add(-time.Hour).Unix()
	_ = s.CreateToken(ctx, id, uid, hash, "", &past)
	_, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
}

func TestVerifyCredential_Revoked(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = s.CreateToken(ctx, id, uid, hash, "", nil)
	_ = s.RevokeToken(ctx, id)
	_, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if !errors.Is(err, auth.ErrTokenRevoked) {
		t.Fatalf("want ErrTokenRevoked, got %v", err)
	}
}

func TestVerifyCredential_Disabled(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = s.CreateToken(ctx, id, uid, hash, "", nil)
	_ = s.SetUserDisabled(ctx, "alice", true)
	_, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if !errors.Is(err, auth.ErrUserDisabled) {
		t.Fatalf("want ErrUserDisabled, got %v", err)
	}
	_ = uid
}

func TestVerifyCredential_UsernameMustMatch(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = s.CreateToken(ctx, id, uid, hash, "", nil)
	// Wrong username, valid token: reject. (Spec §30.1: username + token-as-password.)
	_, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "bob", Password: tok})
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("want ErrInvalidCredential, got %v", err)
	}
}

func TestTouchTokenUsage(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	_, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = s.CreateToken(ctx, id, uid, hash, "", nil)
	if err := s.TouchTokenUsage(ctx, id); err != nil {
		t.Fatalf("TouchTokenUsage: %v", err)
	}
	tok, _ := s.GetTokenByID(ctx, id)
	if tok.LastUsedAt == nil {
		t.Fatal("LastUsedAt not set")
	}
	// Missing id = no error.
	if err := s.TouchTokenUsage(ctx, "noSuchAAAAAAAAAAAAAAAAAA"); err != nil {
		t.Fatalf("TouchTokenUsage missing id: %v", err)
	}
}

// mustOpen is a tiny test helper.
func mustOpen(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}
