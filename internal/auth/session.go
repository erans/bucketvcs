package auth

import "time"

// Session is a browser session record. Name and IsAdmin are denormalized from
// the users table at lookup time so request handlers can render identity without
// a second query.
type Session struct {
	UserID    string
	Name      string
	IsAdmin   bool
	Provider  string // "password" | "oidc"
	CreatedAt time.Time
	ExpiresAt time.Time
}
