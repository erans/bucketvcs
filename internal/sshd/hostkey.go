package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"golang.org/x/crypto/ssh"
)

// LoadOrGenerateHostKey loads the SSH host key at path, or generates a fresh
// ed25519 key and persists it (mode 0600) if the file does not exist.
//
// Logs the SHA256 fingerprint at INFO on every load and on generate. If the
// existing file's mode is looser than 0600, logs a warning but loads anyway.
func LoadOrGenerateHostKey(path string, logger *slog.Logger) (ssh.Signer, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		if st, statErr := os.Stat(path); statErr == nil {
			if st.Mode().Perm() != 0o600 {
				logger.Warn("ssh host key file mode is permissive",
					"path", path, "mode", st.Mode().Perm())
			}
		}
		signer, parseErr := ssh.ParsePrivateKey(raw)
		if parseErr != nil {
			return nil, fmt.Errorf("parse host key %s: %w", path, parseErr)
		}
		logger.Info("loaded ssh host key",
			"path", path,
			"fingerprint", SHA256Fingerprint(signer.PublicKey()))
		return signer, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}

	// Generate fresh ed25519
	_, priv, genErr := ed25519.GenerateKey(rand.Reader)
	if genErr != nil {
		return nil, fmt.Errorf("generate ed25519 host key: %w", genErr)
	}
	pemBlock, marshalErr := ssh.MarshalPrivateKey(priv, "")
	if marshalErr != nil {
		return nil, fmt.Errorf("marshal ed25519 private key: %w", marshalErr)
	}
	pemBytes := pem.EncodeToMemory(pemBlock)
	if writeErr := os.WriteFile(path, pemBytes, 0o600); writeErr != nil {
		return nil, fmt.Errorf("write host key %s: %w", path, writeErr)
	}
	signer, parseErr := ssh.ParsePrivateKey(pemBytes)
	if parseErr != nil {
		return nil, fmt.Errorf("re-parse generated key: %w", parseErr)
	}
	logger.Info("generated ssh host key",
		"path", path,
		"fingerprint", SHA256Fingerprint(signer.PublicKey()))
	return signer, nil
}
