package locks

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
)

// generateLockID returns "lock_" followed by 26 base32 characters of
// 16 random bytes (128 bits). Uses crypto/rand. The lower-case "lock_"
// prefix is consistent with the existing "bvts_" token-style naming.
//
// Globally unique by birthday-paradox argument at 128 bits; the
// PRIMARY KEY constraint on lfs_locks.id is the in-DB safety net.
func generateLockID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("locks: idgen: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	return "lock_" + encoded, nil
}
