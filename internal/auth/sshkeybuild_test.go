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
