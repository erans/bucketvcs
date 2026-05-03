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
// the ".meta" suffix in *every* path segment for its own JSON sidecars:
// the sidecar of object "foo" lives at "<root>/objects/foo.meta", so a
// key like "foo.meta" would collide with that file, and a key like
// "foo.meta/bar" would require "foo.meta" to be both a file (the
// sidecar of "foo") and a directory at the same time. Both are
// rejected up front.
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
		if strings.HasSuffix(seg, ".meta") {
			return errKey(fmt.Sprintf("contains segment %q ending in .meta (reserved for localfs sidecars)", seg))
		}
	}
	return nil
}

func errKey(reason string) error {
	return fmt.Errorf("%w: key %s", storage.ErrInvalidArgument, reason)
}
