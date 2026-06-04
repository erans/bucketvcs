package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"
)

// BuildUserSSHKey parses an authorized_keys line and assembles a user-owned
// SSHKey row (ID minted, fingerprint computed). The caller persists it.
//
// Note: the SHA256 fingerprint is computed inline (sha256+base64 of the wire
// marshal) rather than delegating to internal/sshd.SHA256Fingerprint because
// internal/sshd already imports internal/auth, creating an import cycle if
// auth were to import sshd.
func BuildUserSSHKey(pubLine []byte, userID, label string) (SSHKey, error) {
	parsedKey, _, _, _, err := ssh.ParseAuthorizedKey(pubLine)
	if err != nil {
		return SSHKey{}, fmt.Errorf("auth: parse authorized_key: %w", err)
	}

	wireBytes := parsedKey.Marshal()
	sum := sha256.Sum256(wireBytes)
	fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	keyType := parsedKey.Type()

	id, err := GenerateSSHKeyID()
	if err != nil {
		return SSHKey{}, fmt.Errorf("auth: generate ssh key id: %w", err)
	}

	return SSHKey{
		ID:          id,
		Fingerprint: fp,
		PublicKey:   wireBytes,
		KeyType:     keyType,
		Label:       label,
		UserID:      userID,
		CreatedAt:   time.Now().Unix(),
	}, nil
}
