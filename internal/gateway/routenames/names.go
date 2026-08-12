// Package routenames provides the shared tenant/repository name validator used
// by HTTP, SSH, LFS, web administration, and CLI registration entry points.
package routenames

import "github.com/bucketvcs/bucketvcs/internal/repo/keys"

// ValidateName reports whether s satisfies the durable-key identifier
// contract: 1..128 ASCII letters, digits, underscores, or hyphens. Dots are
// intentionally rejected because keys.NewRepo cannot represent them.
func ValidateName(s string) bool {
	return keys.ValidateID(s)
}
