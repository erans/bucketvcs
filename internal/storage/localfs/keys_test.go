package localfs

import (
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	valid := []string{
		"a",
		"a/b",
		"tenants/t1/repos/r1/manifest/root.json",
		strings.Repeat("a", 1024),
	}
	for _, k := range valid {
		if err := validateKey(k); err != nil {
			t.Errorf("validateKey(%q) returned %v, want nil", k, err)
		}
	}

	invalid := []string{
		"",
		"/leading-slash",
		"trailing-slash/",
		"has/../segment",
		"..",
		"with\x00nullbyte",
		"with\\backslash",
		strings.Repeat("a", 1025),
		"foo.meta",
		"a/b.meta",
		"foo.meta/bar",
		"a/foo.meta/b",
		".meta",
	}
	for _, k := range invalid {
		if err := validateKey(k); err == nil {
			t.Errorf("validateKey(%q) returned nil, want error", k)
		}
	}
}
