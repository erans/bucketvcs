package gcs

import (
	"fmt"
	"strings"
)

func normalizePrefix(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.Contains(p, "//") {
		return "", fmt.Errorf("prefix must not contain double slashes: %q", p)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("prefix must not start with leading slash: %q", p)
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p, nil
}

func applyPrefix(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + key
}

func stripPrefix(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return strings.TrimPrefix(key, prefix)
}
