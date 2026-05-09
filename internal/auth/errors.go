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
