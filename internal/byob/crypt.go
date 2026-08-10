// Package byob implements bring-your-own-bucket credential helpers:
// AES-256-GCM encryption for credentials stored in the auth DB (crypt.go)
// and the per-tenant store resolver (resolver.go, added in the next task).
package byob

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const nonceSize = 12 // GCM standard nonce length

// Encrypt encrypts plaintext with AES-256-GCM using a 32-byte key.
// Returns nonce || ciphertext. Uses nil AAD for backward compat.
func Encrypt(key, plaintext []byte) ([]byte, error) {
	return EncryptForTenant(key, plaintext, "")
}

// EncryptForTenant encrypts with tenant bound as AAD (when non-empty).
func EncryptForTenant(key, plaintext []byte, tenant string) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("byob: encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("byob: nonce: %w", err)
	}
	var aad []byte
	if tenant != "" {
		aad = []byte(tenant)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// Decrypt decrypts a ciphertext produced by Encrypt.
func Decrypt(key, ciphertext []byte) ([]byte, error) {
	return DecryptForTenant(key, ciphertext, "")
}

// DecryptForTenant decrypts with tenant AAD, falling back to nil AAD for
// backward compat with rows encrypted before tenant binding.
func DecryptForTenant(key, ciphertext []byte, tenant string) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("byob: encryption key must be 32 bytes, got %d", len(key))
	}
	if len(ciphertext) < nonceSize {
		return nil, errors.New("byob: ciphertext too short")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	var aad []byte
	if tenant != "" {
		aad = []byte(tenant)
	}
	plain, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil && tenant != "" {
		// Fallback for old rows encrypted without AAD.
		plain, err = gcm.Open(nil, nonce, ct, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("byob: decrypt: %w", err)
	}
	return plain, nil
}
