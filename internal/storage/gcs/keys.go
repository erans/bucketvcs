package gcs

import (
	"fmt"
	"strings"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func validateKey(k string) error {
	if k == "" {
		return fmt.Errorf("%w: key must not be empty", bvstorage.ErrInvalidArgument)
	}
	if strings.HasPrefix(k, "/") {
		return fmt.Errorf("%w: key must not start with /: %q", bvstorage.ErrInvalidArgument, k)
	}
	if strings.HasSuffix(k, "/") {
		return fmt.Errorf("%w: key must not end with /: %q", bvstorage.ErrInvalidArgument, k)
	}
	if strings.Contains(k, "//") {
		return fmt.Errorf("%w: key must not contain consecutive /: %q", bvstorage.ErrInvalidArgument, k)
	}
	// Reject dot-dot segments that could escape the key namespace.
	if k == ".." || strings.HasPrefix(k, "../") || strings.HasSuffix(k, "/..") || strings.Contains(k, "/../") {
		return fmt.Errorf("%w: key must not contain .. segments: %q", bvstorage.ErrInvalidArgument, k)
	}
	// Reject null bytes: GCS allows them but they corrupt many storage
	// layers and some servers return 500 Internal Server Error.
	if strings.ContainsRune(k, '\x00') {
		return fmt.Errorf("%w: key must not contain null bytes: %q", bvstorage.ErrInvalidArgument, k)
	}
	// Reject backslashes: not a valid separator in the storage contract.
	if strings.ContainsRune(k, '\\') {
		return fmt.Errorf("%w: key must not contain backslashes: %q", bvstorage.ErrInvalidArgument, k)
	}
	return nil
}
