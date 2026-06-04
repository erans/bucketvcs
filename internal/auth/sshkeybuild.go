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
	k, err := buildSSHKeyBase(pubLine, label)
	if err != nil {
		return SSHKey{}, err
	}
	k.UserID = userID
	return k, nil
}

// BuildDeploySSHKey parses an authorized_keys line and assembles a deploy key
// scoped to (tenant, repo, perm). The caller persists it.
//
// The fingerprint, wire bytes, key type, and ID are computed identically to
// BuildUserSSHKey — only the ownership fields differ: a deploy key leaves
// UserID empty and sets the Scope* fields instead.
func BuildDeploySSHKey(pubLine []byte, tenant, repo string, perm Perm, label string) (SSHKey, error) {
	k, err := buildSSHKeyBase(pubLine, label)
	if err != nil {
		return SSHKey{}, err
	}
	k.ScopeTenant = tenant
	k.ScopeRepo = repo
	k.ScopePerm = perm
	return k, nil
}

// buildSSHKeyBase parses pubLine, computes the SHA256 fingerprint and wire
// bytes, and mints a fresh key ID. The ownership fields (UserID / Scope*) are
// left for the caller to set.
func buildSSHKeyBase(pubLine []byte, label string) (SSHKey, error) {
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
		CreatedAt:   time.Now().Unix(),
	}, nil
}
