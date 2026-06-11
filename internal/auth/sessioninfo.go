package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashSessionID returns SHA-256(rawID) as hex — the stored form of a session's
// cookie id. The single source of truth for the hash: the sqlite store uses it
// when writing/looking up rows, and the web layer uses it to compare a request
// cookie against a rendered IDHash (e.g. the current-session revoke guard). The
// id is high-entropy, so a single SHA-256 (not argon2) is sufficient: there is
// no low-entropy secret to brute-force, and lookups must be cheap (one per
// request).
func HashSessionID(rawID string) string {
	sum := sha256.Sum256([]byte(rawID))
	return hex.EncodeToString(sum[:])
}

// SessionInfo is a non-sensitive view of one session row for the UI. It never
// carries the raw session id (that exists only in the client cookie); IDHash is
// the stored SHA-256 hex and is safe to render and accept back on a revoke POST.
type SessionInfo struct {
	IDHash    string
	Provider  string
	CreatedAt int64 // unix seconds
	ExpiresAt int64
	LastSeen  int64
	IsCurrent bool
}

// AdminSessionInfo augments SessionInfo with owner identity for the admin list.
type AdminSessionInfo struct {
	SessionInfo
	UserID   string
	UserName string
}
