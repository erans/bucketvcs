package localfs

import (
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

const maxKeyBytes = 1024

// validateKey returns ErrInvalidArgument if key violates localfs key
// rules. The rules are conservative — they match the most-restrictive
// floor across cloud providers — so a key valid for localfs is also
// valid for S3, GCS, R2, and Azure Blob. Localfs additionally reserves
// the ".meta" suffix for its own JSON sidecars; keys ending in ".meta"
// are rejected so that the sidecar namespace cannot collide with real
// object keys.
func validateKey(key string) error {
	if key == "" {
		return errKey("empty")
	}
	if len(key) > maxKeyBytes {
		return errKey(fmt.Sprintf("longer than %d bytes", maxKeyBytes))
	}
	if strings.HasPrefix(key, "/") {
		return errKey("has leading /")
	}
	if strings.HasSuffix(key, "/") {
		return errKey("has trailing /")
	}
	if strings.HasSuffix(key, ".meta") {
		return errKey("ends in .meta (reserved for localfs sidecars)")
	}
	if strings.Contains(key, "\\") {
		return errKey("contains backslash")
	}
	if strings.ContainsRune(key, 0) {
		return errKey("contains null byte")
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return errKey(fmt.Sprintf("contains forbidden segment %q", seg))
		}
	}
	return nil
}

func errKey(reason string) error {
	return fmt.Errorf("%w: key %s", storage.ErrInvalidArgument, reason)
}
