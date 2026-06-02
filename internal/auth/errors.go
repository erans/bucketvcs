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
	ErrNoSession            = errors.New("auth: no session")
	ErrConflict             = errors.New("auth: conflict")
	ErrDuplicateFingerprint = errors.New("auth: duplicate fingerprint")
)

// ErrInsufficientScope is returned by CheckScope when the actor's token
// scopes don't satisfy the required scope. M17.
var ErrInsufficientScope = errors.New("auth: insufficient scope")

// IsCredentialError reports whether err represents a known credential-state
// failure — wrong secret, unknown user, expired/revoked token, disabled
// user, unknown SSH key. Used by both the HTTPS gateway and the SSH
// transport to decide (1) which errors map to 401, and (2) which errors
// should count toward the M18 rate-limit bucket. Other errors (DB / disk
// / network) must surface as 500 and must NOT count as credential
// failures.
//
// ErrNoSuchUser is included for forward-safety: today the sqlitestore
// folds unknown-user lookups into ErrInvalidCredential at the boundary,
// so this arm is currently unreachable on both transports, but a future
// store implementation that surfaces ErrNoSuchUser separately would
// otherwise bypass rate-limit counting and map to 500.
func IsCredentialError(err error) bool {
	return errors.Is(err, ErrInvalidCredential) ||
		errors.Is(err, ErrTokenExpired) ||
		errors.Is(err, ErrTokenRevoked) ||
		errors.Is(err, ErrUserDisabled) ||
		errors.Is(err, ErrNoSuchUser) ||
		errors.Is(err, ErrNoSuchKey)
}
