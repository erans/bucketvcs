package sshd

import (
	"crypto/sha256"
	"encoding/base64"

	"golang.org/x/crypto/ssh"
)

// SHA256Fingerprint computes the OpenSSH-style SHA256 fingerprint of a
// public key. Format: "SHA256:" + base64-no-padding(sha256(wire_pubkey)).
// This matches what `ssh-keygen -lf <pubkey>` and OpenSSH log lines print,
// so operators can compare fingerprints visually.
func SHA256Fingerprint(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}
