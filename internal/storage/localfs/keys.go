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
//
// Localfs also reserves segments beginning with "." for its own use:
// atomic-write temp files are named with a leading dot, so a leading-
// dot segment in a key would be indistinguishable from in-flight or
// crashed-leftover temp files in a directory listing. Cloud adapters
// at M5/M7 will not impose this restriction; document it in the M0
// localfs README per Task 32.
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
		if strings.HasPrefix(seg, ".") {
			return errKey(fmt.Sprintf("contains segment %q starting with . (reserved for localfs internal files)", seg))
		}
		if strings.HasSuffix(seg, ".meta") {
			return errKey(fmt.Sprintf("contains segment %q ending in .meta (reserved for localfs sidecars)", seg))
		}
	}
	return nil
}

// validatePrefix is the relaxed counterpart of validateKey for List
// inputs. An empty prefix means "list all keys". A non-empty prefix may
// end with "/" (callers commonly use a trailing delimiter to scope to
// a logical directory) and may end mid-segment (its last segment need
// not satisfy the per-segment ".meta"/leading-dot reservations because
// it might be matching a partial key). It still rejects any input that
// could escape the bucket: leading "/", "..", ".", null, backslash,
// or oversized strings.
func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if len(prefix) > maxKeyBytes {
		return errKey(fmt.Sprintf("prefix longer than %d bytes", maxKeyBytes))
	}
	if strings.HasPrefix(prefix, "/") {
		return errKey("prefix has leading /")
	}
	if strings.Contains(prefix, "\\") {
		return errKey("prefix contains backslash")
	}
	if strings.ContainsRune(prefix, 0) {
		return errKey("prefix contains null byte")
	}
	// Strip a single trailing "/" so the directory-style "p/" case
	// shares the segment check below; the prefix's *last* segment
	// stays partial-tolerant either way (no per-segment reservation
	// check), but every non-final segment must still be a valid one.
	p := strings.TrimSuffix(prefix, "/")
	if p == "" {
		return nil
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if seg == ".." || seg == "." || seg == "" {
			return errKey(fmt.Sprintf("prefix contains forbidden segment %q", seg))
		}
		// The last segment may be a partial key match. Skip the
		// leading-dot/.meta reservations for it; other segments must
		// satisfy the same rules as a key.
		if i == len(segs)-1 && !strings.HasSuffix(prefix, "/") {
			continue
		}
		if strings.HasPrefix(seg, ".") {
			return errKey(fmt.Sprintf("prefix contains segment %q starting with . (reserved)", seg))
		}
		if strings.HasSuffix(seg, ".meta") {
			return errKey(fmt.Sprintf("prefix contains segment %q ending in .meta (reserved)", seg))
		}
	}
	return nil
}

func errKey(reason string) error {
	return fmt.Errorf("%w: key %s", storage.ErrInvalidArgument, reason)
}
