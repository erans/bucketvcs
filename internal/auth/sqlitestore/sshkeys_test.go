package sqlitestore

import (
	"context"
	"errors"
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
