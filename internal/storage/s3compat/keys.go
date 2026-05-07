package s3compat

import (
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

const maxKeyBytes = 1024

// validateKey checks that key is well-formed for S3-compatible storage.
// Returns a storage.ErrInvalidArgument-wrapped error on rejection.
//
// Rules (mirrors internal/storage/localfs/keys.go validateKey, with
// max length raised to 1024 bytes per the S3 object key spec):
//   - non-empty
//   - no leading "/"
//   - no trailing "/"
//   - no "//" (empty interior segments)
//   - no ASCII NUL
//   - no backslash
//   - no "." or ".." path segments
//   - <= 1024 bytes
func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: key is empty", storage.ErrInvalidArgument)
	}
	if len(key) > maxKeyBytes {
		return fmt.Errorf("%w: key exceeds %d bytes (got %d)", storage.ErrInvalidArgument, maxKeyBytes, len(key))
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("%w: key must not start with '/' (got %q)", storage.ErrInvalidArgument, key)
	}
	if strings.HasSuffix(key, "/") {
		return fmt.Errorf("%w: key must not end with '/' (got %q)", storage.ErrInvalidArgument, key)
	}
	if strings.Contains(key, "\x00") {
		return fmt.Errorf("%w: key contains NUL byte", storage.ErrInvalidArgument)
	}
	if strings.Contains(key, "\\") {
		return fmt.Errorf("%w: key must not contain backslash (got %q)", storage.ErrInvalidArgument, key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" {
			return fmt.Errorf("%w: key must not contain empty segment (got %q)", storage.ErrInvalidArgument, key)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("%w: key must not contain '.' or '..' segments (got %q)", storage.ErrInvalidArgument, key)
		}
	}
	return nil
}
