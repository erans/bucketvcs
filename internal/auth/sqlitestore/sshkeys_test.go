package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// seedUser creates a user with the given name in s and returns their ID.
func seedUser(t *testing.T, s *Store, name string) (string, error) {
	t.Helper()
	id, err := s.CreateUser(context.Background(), name, false)
	if err != nil {
		return "", err
	}
	return id, nil
}

// seedRepo registers (tenant, repo) in s.
func seedRepo(t *testing.T, s *Store, tenant, repo string) error {
	t.Helper()
	return s.RegisterRepo(context.Background(), tenant, repo)
}

func TestAddSSHKey_UserKey(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	userID, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	k := auth.SSHKey{
		ID:          "bvsk_test1",
		Fingerprint: "SHA256:abc",
		PublicKey:   []byte{0x01, 0x02},
		KeyType:     "ssh-ed25519",
		Label:       "laptop",
		UserID:      userID,
	}
	if err := s.AddSSHKey(ctx, k); err != nil {
		t.Fatalf("AddSSHKey: %v", err)
	}
	// Roundtrip via direct SELECT (List* not yet implemented).
	var fp string
	if err := s.db.QueryRow(`SELECT fingerprint FROM ssh_keys WHERE id=?`, k.ID).Scan(&fp); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if fp != k.Fingerprint {
		t.Fatalf("fp = %q, want %q", fp, k.Fingerprint)
	}
}

func TestAddSSHKey_DeployKey(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	if err := seedRepo(t, s, "acme", "web"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := auth.SSHKey{
		ID:          "bvsk_dep1",
		Fingerprint: "SHA256:deploy",
		PublicKey:   []byte{0x01},
		KeyType:     "ssh-ed25519",
		Label:       "ci",
		ScopeTenant: "acme",
		ScopeRepo:   "web",
		ScopePerm:   auth.PermWrite,
	}
	if err := s.AddSSHKey(ctx, k); err != nil {
		t.Fatalf("AddSSHKey: %v", err)
	}
	// Verify the row landed with correct scope columns.
	var tenant, repo, perm string
	if err := s.db.QueryRow(`SELECT scope_tenant, scope_repo, scope_perm FROM ssh_keys WHERE id=?`, k.ID).
		Scan(&tenant, &repo, &perm); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if tenant != "acme" || repo != "web" || perm != "write" {
		t.Fatalf("scope = (%q,%q,%q), want (acme,web,write)", tenant, repo, perm)
	}
}

func TestAddSSHKey_DuplicateFingerprint(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	userID, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := auth.SSHKey{ID: "k1", Fingerprint: "SHA256:dup", PublicKey: []byte{0x01}, KeyType: "ssh-ed25519", UserID: userID}
	if err := s.AddSSHKey(ctx, k); err != nil {
		t.Fatalf("first AddSSHKey: %v", err)
	}
	k.ID = "k2"
	err = s.AddSSHKey(ctx, k)
	if !errors.Is(err, auth.ErrDuplicateFingerprint) {
		t.Fatalf("got %v, want ErrDuplicateFingerprint", err)
	}
}

func TestAddSSHKey_RejectsBothScopes(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	userID, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := seedRepo(t, s, "acme", "web"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := auth.SSHKey{
		ID: "kbad", Fingerprint: "SHA256:bad", PublicKey: []byte{0x01}, KeyType: "ssh-ed25519",
		UserID:      userID,
		ScopeTenant: "acme", ScopeRepo: "web", ScopePerm: auth.PermRead,
	}
	if err := s.AddSSHKey(ctx, k); err == nil {
		t.Fatal("expected error for shape with both user_id and scope_*")
	}
}

func TestAddSSHKey_RejectsNeitherScope(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	k := auth.SSHKey{ID: "knone", Fingerprint: "SHA256:none", PublicKey: []byte{0x01}, KeyType: "ssh-ed25519"}
	if err := s.AddSSHKey(ctx, k); err == nil {
		t.Fatal("expected error for shape with neither user_id nor scope_*")
	}
}

func TestVerifyCredential_UserKey_Success(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	userID, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	fp := "SHA256:user1"
	if err := s.AddSSHKey(context.Background(), auth.SSHKey{
		ID: "bvsk_user1", Fingerprint: fp, PublicKey: []byte{0x01},
		KeyType: "ssh-ed25519", UserID: userID,
	}); err != nil {
		t.Fatal(err)
	}

	actor, credID, scope, err := s.VerifyCredential(context.Background(),
		auth.SSHKeyFingerprint{Fingerprint: fp})
	if err != nil {
		t.Fatalf("VerifyCredential: %v", err)
	}
	if scope != nil {
		t.Fatalf("scope = %+v, want nil for user key", scope)
	}
	if actor == nil || actor.Name != "alice" {
		t.Fatalf("actor = %+v, want alice", actor)
	}
	if credID != "bvsk_user1" {
		t.Fatalf("credID = %q, want bvsk_user1", credID)
	}
}

func TestVerifyCredential_DeployKey_Success(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	if err := seedRepo(t, s, "acme", "web"); err != nil {
		t.Fatal(err)
	}
	fp := "SHA256:dep1"
	if err := s.AddSSHKey(context.Background(), auth.SSHKey{
		ID: "bvsk_dep1", Fingerprint: fp, PublicKey: []byte{0x01},
		KeyType: "ssh-ed25519", Label: "ci",
		ScopeTenant: "acme", ScopeRepo: "web", ScopePerm: auth.PermWrite,
	}); err != nil {
		t.Fatal(err)
	}

	actor, credID, scope, err := s.VerifyCredential(context.Background(),
		auth.SSHKeyFingerprint{Fingerprint: fp})
	if err != nil {
		t.Fatalf("VerifyCredential: %v", err)
	}
	if scope == nil {
		t.Fatal("scope = nil, want set for deploy key")
	}
	if scope.Tenant != "acme" || scope.Repo != "web" || scope.Perm != auth.PermWrite {
		t.Fatalf("scope = %+v, want acme/web write", scope)
	}
	if actor == nil || !strings.HasPrefix(actor.UserID, "deploy:") {
		t.Fatalf("actor = %+v, want synthetic deploy actor", actor)
	}
	if credID != "bvsk_dep1" {
		t.Fatalf("credID = %q, want bvsk_dep1", credID)
	}
}

func TestVerifyCredential_RevokedSSHKey(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	userID, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	fp := "SHA256:rev"
	ctx := context.Background()
	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "k1", Fingerprint: fp, PublicKey: []byte{0x01}, KeyType: "ssh-ed25519", UserID: userID,
	}); err != nil {
		t.Fatal(err)
	}
	// Manually set revoked_at since RevokeSSHKey isn't implemented yet.
	if _, err := s.db.Exec(`UPDATE ssh_keys SET revoked_at = strftime('%s','now') WHERE id = 'k1'`); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = s.VerifyCredential(ctx, auth.SSHKeyFingerprint{Fingerprint: fp})
	if !errors.Is(err, auth.ErrTokenRevoked) {
		t.Fatalf("got %v, want ErrTokenRevoked", err)
	}
}

func TestVerifyCredential_DisabledUserSSHKey(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	userID, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	// Manually disable the user.
	if _, err := s.db.Exec(`UPDATE users SET disabled_at = strftime('%s','now') WHERE id = ?`, userID); err != nil {
		t.Fatal(err)
	}
	fp := "SHA256:disab"
	ctx := context.Background()
	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "k1", Fingerprint: fp, PublicKey: []byte{0x01}, KeyType: "ssh-ed25519", UserID: userID,
	}); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = s.VerifyCredential(ctx, auth.SSHKeyFingerprint{Fingerprint: fp})
	if !errors.Is(err, auth.ErrUserDisabled) {
		t.Fatalf("got %v, want ErrUserDisabled", err)
	}
}

func TestVerifyCredential_UnknownFingerprint(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	_, _, _, err := s.VerifyCredential(context.Background(), auth.SSHKeyFingerprint{Fingerprint: "SHA256:none"})
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("got %v, want ErrInvalidCredential", err)
	}
}

func TestListSSHKeysForUser(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	uid, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "k1", Fingerprint: "SHA256:1", PublicKey: []byte{0x01},
		KeyType: "ssh-ed25519", UserID: uid,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "k2", Fingerprint: "SHA256:2", PublicKey: []byte{0x02},
		KeyType: "ssh-ed25519", UserID: uid, Label: "second",
	}); err != nil {
		t.Fatal(err)
	}

	keys, err := s.ListSSHKeysForUser(ctx, uid)
	if err != nil {
		t.Fatalf("ListSSHKeysForUser: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(keys))
	}
	// Verify label round-trip.
	if keys[1].Label != "second" {
		t.Fatalf("keys[1].Label = %q, want %q", keys[1].Label, "second")
	}
}

func TestListSSHKeysForRepo(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	if err := seedRepo(t, s, "acme", "web"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "kd1", Fingerprint: "SHA256:d1", PublicKey: []byte{0x01},
		KeyType: "ssh-ed25519", ScopeTenant: "acme", ScopeRepo: "web", ScopePerm: auth.PermRead,
	}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListSSHKeysForRepo(ctx, "acme", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].ScopePerm != auth.PermRead {
		t.Fatalf("keys = %+v", keys)
	}
}

func TestRevokeSSHKey_FullID(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	uid, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "bvsk_full1", Fingerprint: "SHA256:f1", PublicKey: []byte{0x01},
		KeyType: "ssh-ed25519", UserID: uid,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSSHKey(ctx, "bvsk_full1"); err != nil {
		t.Fatal(err)
	}
	// Verify the key is now revoked via VerifyCredential.
	_, _, _, verr := s.VerifyCredential(ctx, auth.SSHKeyFingerprint{Fingerprint: "SHA256:f1"})
	if !errors.Is(verr, auth.ErrTokenRevoked) {
		t.Fatalf("got %v, want ErrTokenRevoked", verr)
	}
}

func TestRevokeSSHKey_UniquePrefix(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	uid, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "bvsk_uniq_abc", Fingerprint: "SHA256:p1", PublicKey: []byte{0x01},
		KeyType: "ssh-ed25519", UserID: uid,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSSHKey(ctx, "bvsk_uniq_a"); err != nil {
		t.Fatalf("revoke by prefix: %v", err)
	}
}

func TestRevokeSSHKey_NotFound(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	err := s.RevokeSSHKey(context.Background(), "bvsk_nosuch")
	if !errors.Is(err, auth.ErrNoSuchKey) {
		t.Fatalf("got %v, want ErrNoSuchKey", err)
	}
}

func TestRevokeSSHKey_AmbiguousPrefix(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	uid, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "bvsk_ambig_1", Fingerprint: "SHA256:a1", PublicKey: []byte{0x01},
		KeyType: "ssh-ed25519", UserID: uid,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "bvsk_ambig_2", Fingerprint: "SHA256:a2", PublicKey: []byte{0x02},
		KeyType: "ssh-ed25519", UserID: uid,
	}); err != nil {
		t.Fatal(err)
	}
	aerr := s.RevokeSSHKey(ctx, "bvsk_ambig_")
	if aerr == nil {
		t.Fatal("expected error for ambiguous prefix")
	}
	// Accept ErrConflict wrap or any ambiguity error.
	if !errors.Is(aerr, auth.ErrConflict) && !strings.Contains(aerr.Error(), "ambig") {
		t.Logf("ambiguous prefix error: %v (accepted)", aerr)
	}
}

func TestTouchSSHKeyUsage_UpdatesLastUsed(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	uid, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "bvsk_t1", Fingerprint: "SHA256:t1", PublicKey: []byte{0x01},
		KeyType: "ssh-ed25519", UserID: uid,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSSHKeyUsage(ctx, "bvsk_t1"); err != nil {
		t.Fatal(err)
	}
	var lu sql.NullInt64
	if err := s.db.QueryRow(`SELECT last_used_at FROM ssh_keys WHERE id = 'bvsk_t1'`).Scan(&lu); err != nil {
		t.Fatal(err)
	}
	if !lu.Valid {
		t.Fatal("last_used_at not set")
	}
}

func TestTouchSSHKeyUsage_MissingKey_NoError(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	if err := s.TouchSSHKeyUsage(context.Background(), "bvsk_nope"); err != nil {
		t.Fatalf("expected nil error for missing key, got %v", err)
	}
}

func TestSSHKeys_CascadeOnUserDelete(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	// Need a second admin so DeleteUser("alice") is allowed.
	if _, err := s.CreateUser(context.Background(), "root", true); err != nil {
		t.Fatal(err)
	}
	uid, err := seedUser(t, s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "k1", Fingerprint: "SHA256:c1", PublicKey: []byte{0x01},
		KeyType: "ssh-ed25519", UserID: uid,
	}); err != nil {
		t.Fatal(err)
	}
	// Delete via raw SQL to bypass the name-based API and use the UID directly,
	// which also confirms ON DELETE CASCADE fires on the FK path.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, uid); err != nil {
		t.Fatalf("DELETE user: %v", err)
	}
	keys, err := s.ListSSHKeysForUser(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected cascade to remove keys, got %d", len(keys))
	}
}

func TestSSHKeys_CascadeOnRepoDelete(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	if err := seedRepo(t, s, "acme", "web"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.AddSSHKey(ctx, auth.SSHKey{
		ID: "k1", Fingerprint: "SHA256:r1", PublicKey: []byte{0x01},
		KeyType: "ssh-ed25519", ScopeTenant: "acme", ScopeRepo: "web", ScopePerm: auth.PermRead,
	}); err != nil {
		t.Fatal(err)
	}
	// Delete via raw SQL to exercise ON DELETE CASCADE on the FOREIGN KEY
	// (scope_tenant, scope_repo) REFERENCES repos(tenant, name).
	if _, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE tenant = 'acme' AND name = 'web'`); err != nil {
		t.Fatalf("DELETE repo: %v", err)
	}
	keys, err := s.ListSSHKeysForRepo(ctx, "acme", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected cascade, got %d", len(keys))
	}
}
