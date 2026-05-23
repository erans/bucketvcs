package auth

import "errors"

var (
	ErrInvalidCredential    = errors.New("auth: invalid credential")
	ErrTokenExpired         = errors.New("auth: token expired")
	ErrTokenRevoked         = errors.New("auth: token revoked")
	ErrUserDisabled         = errors.New("auth: user disabled")
	ErrNoSuchRepo           = errors.New("auth: no such repo")
	ErrNoSuchUser           = errors.New("auth: no such user")
	ErrNoSuchToken          = errors.New("auth: no such token")
	ErrNoSuchKey            = errors.New("auth: no such ssh key")
	ErrConflict             = errors.New("auth: conflict")
	ErrDuplicateFingerprint = errors.New("auth: duplicate fingerprint")
)

// ErrInsufficientScope is returned by CheckScope when the actor's token
// scopes don't satisfy the required scope. M17.
var ErrInsufficientScope = errors.New("auth: insufficient scope")
