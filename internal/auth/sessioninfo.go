package auth

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
