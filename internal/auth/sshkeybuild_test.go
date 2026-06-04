package auth

import (
	"strings"
	"testing"
)

// testEd25519PubLine is a known valid authorized_keys line used across auth
// package tests. Taken from internal/sshd/testdata/ed25519.pub.
const testEd25519PubLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFpotneIfuGp8t6tsn1sFS3ehwRteumxH4JRK5ZzNSb8 bucketvcs-test-ed25519"

func TestBuildUserSSHKey_Valid(t *testing.T) {
	k, err := BuildUserSSHKey([]byte(testEd25519PubLine), "user1", "my-laptop")
	if err != nil {
		t.Fatalf("BuildUserSSHKey: unexpected error: %v", err)
	}

	if !strings.HasPrefix(k.ID, "bvsk_") {
		t.Errorf("ID = %q, want prefix bvsk_", k.ID)
	}
	if !strings.HasPrefix(k.Fingerprint, "SHA256:") {
		t.Errorf("Fingerprint = %q, want prefix SHA256:", k.Fingerprint)
	}
	if k.KeyType != "ssh-ed25519" {
		t.Errorf("KeyType = %q, want ssh-ed25519", k.KeyType)
	}
	if k.UserID != "user1" {
		t.Errorf("UserID = %q, want user1", k.UserID)
	}
	if k.Label != "my-laptop" {
		t.Errorf("Label = %q, want my-laptop", k.Label)
	}
	if len(k.PublicKey) == 0 {
		t.Error("PublicKey is empty")
	}
	if k.CreatedAt == 0 {
		t.Error("CreatedAt is zero")
	}
}

func TestBuildUserSSHKey_Invalid(t *testing.T) {
	_, err := BuildUserSSHKey([]byte("not a key"), "user1", "label")
	if err == nil {
		t.Fatal("expected error for invalid pubkey, got nil")
	}
}

func TestBuildUserSSHKey_EmptyLabel(t *testing.T) {
	k, err := BuildUserSSHKey([]byte(testEd25519PubLine), "u2", "")
	if err != nil {
		t.Fatalf("BuildUserSSHKey with empty label: %v", err)
	}
	if k.Label != "" {
		t.Errorf("Label = %q, want empty", k.Label)
	}
	if k.UserID != "u2" {
		t.Errorf("UserID = %q, want u2", k.UserID)
	}
}

// TestBuildUserSSHKey_OptionsPrefix verifies that an authorized_keys line with
// leading options (e.g. restrict,command=...) is accepted and produces the same
// fingerprint as the plain line — options are stripped by ssh.ParseAuthorizedKey
// before the wire bytes are marshalled, so only the key material matters.
func TestBuildUserSSHKey_OptionsPrefix(t *testing.T) {
	// Same ed25519 key material as testEd25519PubLine, but with a leading
	// restrict,command= options field, as a client tool might paste it.
	const optionsLine = `restrict,command="/bin/false" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFpotneIfuGp8t6tsn1sFS3ehwRteumxH4JRK5ZzNSb8 bucketvcs-test-ed25519`

	plain, err := BuildUserSSHKey([]byte(testEd25519PubLine), "u3", "plain")
	if err != nil {
		t.Fatalf("BuildUserSSHKey plain: %v", err)
	}
	opts, err := BuildUserSSHKey([]byte(optionsLine), "u3", "with-options")
	if err != nil {
		t.Fatalf("BuildUserSSHKey options: %v", err)
	}

	if opts.Fingerprint != plain.Fingerprint {
		t.Errorf("options-prefixed line fingerprint %q != plain line fingerprint %q; options should be discarded",
			opts.Fingerprint, plain.Fingerprint)
	}
}

func TestBuildDeploySSHKey_Valid(t *testing.T) {
	k, err := BuildDeploySSHKey([]byte(testEd25519PubLine), "acme", "demo", PermRead, "ci-deploy")
	if err != nil {
		t.Fatalf("BuildDeploySSHKey: unexpected error: %v", err)
	}

	if !strings.HasPrefix(k.ID, "bvsk_") {
		t.Errorf("ID = %q, want prefix bvsk_", k.ID)
	}
	if !strings.HasPrefix(k.Fingerprint, "SHA256:") {
		t.Errorf("Fingerprint = %q, want prefix SHA256:", k.Fingerprint)
	}
	if k.KeyType != "ssh-ed25519" {
		t.Errorf("KeyType = %q, want ssh-ed25519", k.KeyType)
	}
	if k.UserID != "" {
		t.Errorf("UserID = %q, want empty (deploy key)", k.UserID)
	}
	if k.ScopeTenant != "acme" {
		t.Errorf("ScopeTenant = %q, want acme", k.ScopeTenant)
	}
	if k.ScopeRepo != "demo" {
		t.Errorf("ScopeRepo = %q, want demo", k.ScopeRepo)
	}
	if k.ScopePerm != PermRead {
		t.Errorf("ScopePerm = %v, want PermRead", k.ScopePerm)
	}
	if k.Label != "ci-deploy" {
		t.Errorf("Label = %q, want ci-deploy", k.Label)
	}
	if len(k.PublicKey) == 0 {
		t.Error("PublicKey is empty")
	}
	if k.CreatedAt == 0 {
		t.Error("CreatedAt is zero")
	}
}

func TestBuildDeploySSHKey_Invalid(t *testing.T) {
	_, err := BuildDeploySSHKey([]byte("not a key"), "acme", "demo", PermWrite, "label")
	if err == nil {
		t.Fatal("expected error for invalid pubkey, got nil")
	}
}

// TestBuildDeploySSHKey_SameFingerprintAsUserKey verifies the deploy builder
// computes the identical fingerprint/wire bytes as the user builder for the
// same key material — only the ownership/scope fields differ.
func TestBuildDeploySSHKey_SameFingerprintAsUserKey(t *testing.T) {
	user, err := BuildUserSSHKey([]byte(testEd25519PubLine), "u1", "laptop")
	if err != nil {
		t.Fatalf("BuildUserSSHKey: %v", err)
	}
	deploy, err := BuildDeploySSHKey([]byte(testEd25519PubLine), "acme", "demo", PermWrite, "ci")
	if err != nil {
		t.Fatalf("BuildDeploySSHKey: %v", err)
	}
	if deploy.Fingerprint != user.Fingerprint {
		t.Errorf("deploy fingerprint %q != user fingerprint %q", deploy.Fingerprint, user.Fingerprint)
	}
	if string(deploy.PublicKey) != string(user.PublicKey) {
		t.Error("deploy PublicKey wire bytes differ from user PublicKey")
	}
	if deploy.KeyType != user.KeyType {
		t.Errorf("deploy KeyType %q != user KeyType %q", deploy.KeyType, user.KeyType)
	}
}
