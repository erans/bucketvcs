package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// crockfordAlphabet is the standard Crockford base32 alphabet, uppercase,
// no padding, excludes I L O U. 32 characters.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	tokenPrefix    = "bvts"
	tokenIDLen     = 24
	tokenSecretLen = 52
)

// GenerateToken produces a fresh token along with its id and secret segments.
// The id has ~120 bits of randomness; the secret ~256.
func GenerateToken() (token, id, secret string, err error) {
	id, err = randomCrockford(tokenIDLen)
	if err != nil {
		return "", "", "", fmt.Errorf("generate token id: %w", err)
	}
	secret, err = randomCrockford(tokenSecretLen)
	if err != nil {
		return "", "", "", fmt.Errorf("generate token secret: %w", err)
	}
	token = tokenPrefix + "_" + id + "_" + secret
	return token, id, secret, nil
}

// ParseToken splits a token string into its id and secret segments,
// validating the prefix, separator count, segment lengths, and alphabet.
func ParseToken(token string) (id, secret string, err error) {
	parts := strings.Split(token, "_")
	if len(parts) != 3 {
		return "", "", errors.New("auth: token must have form bvts_<id>_<secret>")
	}
	if parts[0] != tokenPrefix {
		return "", "", errors.New("auth: token has wrong prefix")
	}
	if len(parts[1]) != tokenIDLen {
		return "", "", fmt.Errorf("auth: token id length must be %d", tokenIDLen)
	}
	if len(parts[2]) != tokenSecretLen {
		return "", "", fmt.Errorf("auth: token secret length must be %d", tokenSecretLen)
	}
	if !isCrockford(parts[1]) || !isCrockford(parts[2]) {
		return "", "", errors.New("auth: token contains non-Crockford-base32 characters")
	}
	return parts[1], parts[2], nil
}

// RandomCrockford returns n Crockford-base32 characters drawn from a CSPRNG.
func RandomCrockford(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = crockfordAlphabet[int(b)%32]
	}
	return string(out), nil
}

// randomCrockford is a thin wrapper kept for internal backward compatibility.
func randomCrockford(n int) (string, error) { return RandomCrockford(n) }

// GenerateSSHKeyID returns a fresh ssh_keys.id of the form
// "bvsk_<24 base32 chars>" — same shape as token IDs but with a distinct
// prefix so operators can tell user keys and tokens apart at a glance.
func GenerateSSHKeyID() (string, error) {
	s, err := RandomCrockford(24)
	if err != nil {
		return "", err
	}
	return "bvsk_" + s, nil
}

func isCrockford(s string) bool {
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(crockfordAlphabet, rune(s[i])) {
			return false
		}
	}
	return true
}

const (
	argon2Memory  = 64 * 1024 // KiB -> 64 MiB
	argon2Time    = 3
	argon2Threads = 4
	argon2KeyLen  = 32
	argon2SaltLen = 16
)

// HashSecret returns a PHC-encoded argon2id hash of secret.
func HashSecret(secret string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("hash secret: salt: %w", err)
	}
	key := argon2.IDKey([]byte(secret), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	enc := fmt.Sprintf(
		"$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
	return enc, nil
}

// VerifyHash compares secret against a PHC-encoded argon2id encoded hash.
// Returns nil on match; non-nil on mismatch or malformed encoding.
func VerifyHash(secret, encoded string) error {
	parts := strings.Split(encoded, "$")
	// Expected layout: ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return errors.New("auth: malformed argon2id encoding")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != 19 {
		return errors.New("auth: unsupported argon2 version")
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return errors.New("auth: malformed argon2id parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return errors.New("auth: malformed argon2id salt")
	}
	wantKey, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return errors.New("auth: malformed argon2id key")
	}
	gotKey := argon2.IDKey([]byte(secret), salt, time, memory, threads, uint32(len(wantKey)))
	if subtle.ConstantTimeCompare(gotKey, wantKey) != 1 {
		return ErrInvalidCredential
	}
	return nil
}
