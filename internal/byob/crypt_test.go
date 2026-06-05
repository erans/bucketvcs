package byob_test

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/byob"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	plain := []byte(`{"access_key_id":"AKIA...","secret_access_key":"abc123"}`)
	ct, err := byob.Encrypt(key, plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ct, plain) {
		t.Fatal("ciphertext must not equal plaintext")
	}
	got, err := byob.Decrypt(key, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("Decrypt: got %q want %q", got, plain)
	}
}

func TestEncryptProducesDistinctNonces(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	a, _ := byob.Encrypt(key, []byte("hello"))
	b, _ := byob.Encrypt(key, []byte("hello"))
	if bytes.Equal(a, b) {
		t.Fatal("same plaintext must produce distinct ciphertexts (random nonce)")
	}
}

func TestDecryptWrongKey(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	ct, _ := byob.Encrypt(key, []byte("secret"))
	wrong := make([]byte, 32)
	rand.Read(wrong)
	if _, err := byob.Decrypt(wrong, ct); err == nil {
		t.Fatal("Decrypt with wrong key must fail")
	}
}

func TestDecryptTruncated(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	if _, err := byob.Decrypt(key, []byte("tooshort")); err == nil {
		t.Fatal("Decrypt of truncated ciphertext must fail")
	}
}

func TestKeyTooShort(t *testing.T) {
	if _, err := byob.Encrypt(make([]byte, 16), []byte("x")); err == nil {
		t.Fatal("Encrypt with 16-byte key must fail (need 32)")
	}
}
