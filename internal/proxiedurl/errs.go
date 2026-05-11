package proxiedurl

import "errors"

var (
	ErrTokenExpired = errors.New("proxiedurl: token expired")
	ErrTokenInvalid = errors.New("proxiedurl: token signature invalid")
	ErrKindMismatch = errors.New("proxiedurl: token kind does not match endpoint")
)
