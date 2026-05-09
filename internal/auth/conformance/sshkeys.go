package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// RunSSHKeyTests exercises auth.Store SSH-key behavior (conformance tests
// #14-#22). Called by Run after the M4 (BasicPassword) tests, so any Store
// implementation that passes the full conformance suite automatically covers
// both credential families.
func RunSSHKeyTests(t *testing.T, factory Factory) {
	// #14 — duplicate fingerprint across users is rejected.
	t.Run("AddSSHKey_RejectsDuplicateFingerprint", func(t *testing.T) {
		s, seed := factory(t)
		defer s.Close()
		ctx := context.Background()
		u1 := seed.SeedUser(t, "alice", false)
		u2 := seed.SeedUser(t, "bob", false)
		fp := "SHA256:dup"
		if err := s.AddSSHKey(ctx, auth.SSHKey{
			ID: "k1", Fingerprint: fp, PublicKey: []byte{0x01}, KeyType: "ssh-ed25519", UserID: u1,
		}); err != nil {
			t.Fatal(err)
		}
		err := s.AddSSHKey(ctx, auth.SSHKey{
			ID: "k2", Fingerprint: fp, PublicKey: []byte{0x01}, KeyType: "ssh-ed25519", UserID: u2,
		})
		if !errors.Is(err, auth.ErrDuplicateFingerprint) {
			t.Fatalf("got %v, want ErrDuplicateFingerprint", err)
		}
	})

	// #15 — key with neither user_id nor scope is rejected.
	t.Run("AddSSHKey_RejectsEmptyShape", func(t *testing.T) {
		s, _ := factory(t)
		defer s.Close()
		err := s.AddSSHKey(context.Background(), auth.SSHKey{
			ID: "k1", Fingerprint: "SHA256:none", PublicKey: []byte{0x01}, KeyType: "ssh-ed25519",
		})
		if err == nil {
			t.Fatal("expected error for empty shape (no user_id and no scope)")
		}
	})

	// #16 — key with both user_id and scope is rejected.
	t.Run("AddSSHKey_RejectsBothShapes", func(t *testing.T) {
		s, seed := factory(t)
		defer s.Close()
		u := seed.SeedUser(t, "alice", false)
		seed.SeedRepo(t, "acme", "web", false)
		err := s.AddSSHKey(context.Background(), auth.SSHKey{
			ID: "k1", Fingerprint: "SHA256:both", PublicKey: []byte{0x01}, KeyType: "ssh-ed25519",
			UserID: u, ScopeTenant: "acme", ScopeRepo: "web", ScopePerm: auth.PermRead,
		})
		if err == nil {
			t.Fatal("expected error for key with both user_id and scope")
		}
	})

	// #17 — VerifyCredential with a user SSH key returns actor with nil scope.
	t.Run("VerifyCredential_SSHUserKey_NilScope", func(t *testing.T) {
		s, seed := factory(t)
		defer s.Close()
		u := seed.SeedUser(t, "alice", false)
		fp := "SHA256:user"
		if err := s.AddSSHKey(context.Background(), auth.SSHKey{
			ID: "ku", Fingerprint: fp, PublicKey: []byte{0x01}, KeyType: "ssh-ed25519", UserID: u,
		}); err != nil {
			t.Fatal(err)
		}
		actor, credID, scope, err := s.VerifyCredential(context.Background(), auth.SSHKeyFingerprint{Fingerprint: fp})
		if err != nil {
			t.Fatal(err)
		}
		if scope != nil {
			t.Fatalf("scope = %+v, want nil", scope)
		}
		if actor == nil {
			t.Fatal("actor nil")
		}
		if credID == "" {
			t.Fatal("credID empty")
		}
	})

	// #18 — VerifyCredential with a deploy key returns synthetic actor + non-nil scope.
	t.Run("VerifyCredential_SSHDeployKey_ScopeSet", func(t *testing.T) {
		s, seed := factory(t)
		defer s.Close()
		seed.SeedRepo(t, "acme", "web", false)
		fp := "SHA256:dep"
		if err := s.AddSSHKey(context.Background(), auth.SSHKey{
			ID: "kd", Fingerprint: fp, PublicKey: []byte{0x01}, KeyType: "ssh-ed25519",
			ScopeTenant: "acme", ScopeRepo: "web", ScopePerm: auth.PermWrite,
		}); err != nil {
			t.Fatal(err)
		}
		actor, _, scope, err := s.VerifyCredential(context.Background(), auth.SSHKeyFingerprint{Fingerprint: fp})
		if err != nil {
			t.Fatal(err)
		}
		if scope == nil {
			t.Fatal("scope nil; want set")
		}
		if scope.Tenant != "acme" || scope.Repo != "web" || scope.Perm != auth.PermWrite {
			t.Fatalf("scope = %+v", scope)
		}
		if actor == nil {
			t.Fatal("actor nil")
		}
	})

	// #19 — VerifyCredential rejects a revoked SSH key.
	t.Run("VerifyCredential_RejectsRevokedSSHKey", func(t *testing.T) {
		s, seed := factory(t)
		defer s.Close()
		u := seed.SeedUser(t, "alice", false)
		fp := "SHA256:rev"
		ctx := context.Background()
		if err := s.AddSSHKey(ctx, auth.SSHKey{
			ID: "kr", Fingerprint: fp, PublicKey: []byte{0x01}, KeyType: "ssh-ed25519", UserID: u,
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.RevokeSSHKey(ctx, "kr"); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := s.VerifyCredential(ctx, auth.SSHKeyFingerprint{Fingerprint: fp})
		if !errors.Is(err, auth.ErrTokenRevoked) {
			t.Fatalf("got %v, want ErrTokenRevoked", err)
		}
	})

	// #20 — VerifyCredential rejects SSH key whose user is disabled.
	t.Run("VerifyCredential_RejectsSSHKeyForDisabledUser", func(t *testing.T) {
		s, seed := factory(t)
		defer s.Close()
		u := seed.SeedUser(t, "alice", false)
		fp := "SHA256:dis"
		ctx := context.Background()
		if err := s.AddSSHKey(ctx, auth.SSHKey{
			ID: "kdis", Fingerprint: fp, PublicKey: []byte{0x01}, KeyType: "ssh-ed25519", UserID: u,
		}); err != nil {
			t.Fatal(err)
		}
		seed.DisableUser(t, u)
		_, _, _, err := s.VerifyCredential(ctx, auth.SSHKeyFingerprint{Fingerprint: fp})
		if !errors.Is(err, auth.ErrUserDisabled) {
			t.Fatalf("got %v, want ErrUserDisabled", err)
		}
	})

	// #21 — RevokeSSHKey by prefix succeeds; subsequent call is idempotent.
	t.Run("RevokeSSHKey_PrefixAndIdempotent", func(t *testing.T) {
		s, seed := factory(t)
		defer s.Close()
		u := seed.SeedUser(t, "alice", false)
		ctx := context.Background()
		if err := s.AddSSHKey(ctx, auth.SSHKey{
			ID: "bvsk_unique_xyz", Fingerprint: "SHA256:p1", PublicKey: []byte{0x01}, KeyType: "ssh-ed25519", UserID: u,
		}); err != nil {
			t.Fatal(err)
		}
		// Revoke by unique prefix.
		if err := s.RevokeSSHKey(ctx, "bvsk_unique_x"); err != nil {
			t.Fatalf("revoke by prefix: %v", err)
		}
		// Second revoke (full ID) must not error (idempotent).
		if err := s.RevokeSSHKey(ctx, "bvsk_unique_xyz"); err != nil {
			t.Fatalf("second revoke (idempotent): %v", err)
		}
	})

	// #22 — Cascade: deleting a user removes its user keys; deleting a repo
	// removes its deploy keys.
	t.Run("Cascade_UserDeleteRemovesUserKeys_RepoDeleteRemovesDeployKeys", func(t *testing.T) {
		s, seed := factory(t)
		defer s.Close()
		u := seed.SeedUser(t, "alice", false)
		seed.SeedRepo(t, "acme", "web", false)
		ctx := context.Background()
		if err := s.AddSSHKey(ctx, auth.SSHKey{
			ID: "ku", Fingerprint: "SHA256:cascu", PublicKey: []byte{0x01}, KeyType: "ssh-ed25519", UserID: u,
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddSSHKey(ctx, auth.SSHKey{
			ID: "kd", Fingerprint: "SHA256:cascd", PublicKey: []byte{0x01}, KeyType: "ssh-ed25519",
			ScopeTenant: "acme", ScopeRepo: "web", ScopePerm: auth.PermRead,
		}); err != nil {
			t.Fatal(err)
		}

		seed.DeleteUser(t, u)
		keys, err := s.ListSSHKeysForUser(ctx, u)
		if err != nil {
			t.Fatalf("ListSSHKeysForUser after delete: %v", err)
		}
		if len(keys) != 0 {
			t.Fatalf("user-key cascade: %d keys remain, want 0", len(keys))
		}

		seed.DeleteRepo(t, "acme", "web")
		keys, err = s.ListSSHKeysForRepo(ctx, "acme", "web")
		if err != nil {
			t.Fatalf("ListSSHKeysForRepo after delete: %v", err)
		}
		if len(keys) != 0 {
			t.Fatalf("deploy-key cascade: %d keys remain, want 0", len(keys))
		}
	})
}
