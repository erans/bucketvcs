package s3compat

import (
	"fmt"
	"strings"
)

// normalizePrefix validates and canonicalizes a key prefix:
//   - empty stays empty
//   - non-empty gets a single trailing "/"
//   - leading "/" is rejected
//   - "." or ".." path components are rejected
func normalizePrefix(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("s3compat: prefix must not start with '/' (got %q)", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("s3compat: prefix must not contain '.' or '..' segments (got %q)", p)
		}
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p, nil
}

// applyPrefix prepends the configured prefix to a logical key. Caller
// must ensure prefix has been run through normalizePrefix first.
func applyPrefix(prefix, key string) string {
	if prefix == "" {
		return key
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + key
}

// stripPrefix removes the configured prefix from a stored key. Returns
// an error if the stored key does not start with prefix; the caller
// should treat this as a provider-side bug, not a missing object.
func stripPrefix(prefix, stored string) (string, error) {
	if prefix == "" {
		return stored, nil
	}
	if !strings.HasPrefix(stored, prefix) {
		return "", fmt.Errorf("s3compat: stored key %q does not begin with prefix %q", stored, prefix)
	}
	return stored[len(prefix):], nil
}
