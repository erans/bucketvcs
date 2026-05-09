// Package routenames provides the shared name-validation regex for
// tenant and repo names. Both the HTTP route parser (internal/gateway)
// and the SSH exec-command parser (internal/sshd) import this package
// so that exactly the same character set is accepted on both transports.
package routenames

import "regexp"

// nameRE is the canonical character class for tenant and repo names.
// A name must be non-empty and consist solely of ASCII letters, digits,
// dots, underscores, and hyphens.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateName reports whether s is an acceptable tenant or repo name.
func ValidateName(s string) bool {
	return nameRE.MatchString(s)
}
