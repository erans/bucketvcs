package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
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

// randomCrockford returns n Crockford-base32 characters drawn from a CSPRNG.
func randomCrockford(n int) (string, error) {
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

func isCrockford(s string) bool {
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(crockfordAlphabet, rune(s[i])) {
			return false
		}
	}
	return true
}
