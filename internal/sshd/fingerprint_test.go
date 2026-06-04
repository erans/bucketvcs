package sshd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
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

// TestSHA256Fingerprint_LockedToAuthBuildUserSSHKey asserts that
// sshd.SHA256Fingerprint and the inline fingerprint computation inside
// auth.BuildUserSSHKey produce identical output for the same wire key.
//
// LOCK: internal/sshd cannot import internal/auth (cycle — auth imports sshd),
// so both packages duplicate the sha256+base64-no-padding formula.  This test
// keeps the two implementations in sync: if either formula changes (algorithm,
// prefix, encoding), one of the two sides will diverge and this test will fail
// loudly.  Do NOT remove this test without replacing it with an equivalent
// cross-package equivalence check.
func TestSHA256Fingerprint_LockedToAuthBuildUserSSHKey(t *testing.T) {
	// The fixture line is the same key that lives in testdata/ed25519.pub.
	const pubLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFpotneIfuGp8t6tsn1sFS3ehwRteumxH4JRK5ZzNSb8 bucketvcs-test-ed25519"

	// Build via auth.BuildUserSSHKey — exercises the inline formula in that package.
	built, err := auth.BuildUserSSHKey([]byte(pubLine), "locktest", "lock-label")
	if err != nil {
		t.Fatalf("auth.BuildUserSSHKey: %v", err)
	}

	// Parse the same line and compute via sshd.SHA256Fingerprint.
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubLine))
	if err != nil {
		t.Fatalf("ssh.ParseAuthorizedKey: %v", err)
	}
	sshdFP := SHA256Fingerprint(parsed)

	if sshdFP != built.Fingerprint {
		t.Errorf("fingerprint mismatch:\n  sshd.SHA256Fingerprint  = %q\n  auth.BuildUserSSHKey.FP = %q",
			sshdFP, built.Fingerprint)
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
