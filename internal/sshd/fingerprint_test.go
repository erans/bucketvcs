package sshd

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Pinned fingerprints captured from `ssh-keygen -lf testdata/<name>.pub`
// at fixture-generation time. If you regenerate the fixtures, update these.
const (
	ed25519FP   = "SHA256:hvGMabpq3pBxFaV2kkKlrm7F/32tH+buKfqm2pgESYg"
	rsa2048FP   = "SHA256:6tx2BkqsDGuTgtpRQAXd6RHUfhJ0SbV6lHUwi7nKjbg"
	ecdsap256FP = "SHA256:K8fMTOOU4KoQDPVzjKbM/bWyhGE/sVN3Gr1sKvw1y10"
)

func TestSHA256Fingerprint_Ed25519(t *testing.T) {
	pub := mustReadAuthorizedKey(t, "testdata/ed25519.pub")
	if got := SHA256Fingerprint(pub); got != ed25519FP {
		t.Fatalf("got %q, want %q", got, ed25519FP)
	}
}

func TestSHA256Fingerprint_RSA2048(t *testing.T) {
	pub := mustReadAuthorizedKey(t, "testdata/rsa2048.pub")
	if got := SHA256Fingerprint(pub); got != rsa2048FP {
		t.Fatalf("got %q, want %q", got, rsa2048FP)
	}
}

func TestSHA256Fingerprint_ECDSAP256(t *testing.T) {
	pub := mustReadAuthorizedKey(t, "testdata/ecdsap256.pub")
	if got := SHA256Fingerprint(pub); got != ecdsap256FP {
		t.Fatalf("got %q, want %q", got, ecdsap256FP)
	}
}

func mustReadAuthorizedKey(t *testing.T, relpath string) ssh.PublicKey {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(relpath))
	if err != nil {
		t.Fatal(err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
