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
		".dotfile",
		"a/.b",
		".tmp",
		".gitkeep/sub",
	}
	for _, k := range invalid {
		if err := validateKey(k); err == nil {
			t.Errorf("validateKey(%q) returned nil, want error", k)
		}
	}
}

func TestValidatePrefix(t *testing.T) {
	valid := []string{
		"",
		"a",
		"a/",
		"a/b",
		"a/b/",
		"tenants/t1/repos/",
		"par",
		strings.Repeat("a", 1024),
	}
	for _, p := range valid {
		if err := validatePrefix(p); err != nil {
			t.Errorf("validatePrefix(%q) returned %v, want nil", p, err)
		}
	}

	invalid := []string{
		"/leading-slash",
		"with\x00null",
		"with\\backslash",
		"has/../seg",
		"a/./b/",
		"a/b//c",
		strings.Repeat("a", 1025),
		".dotseg/sub",
		"a/.dotseg/sub",
		"foo.meta/bar",
	}
	for _, p := range invalid {
		if err := validatePrefix(p); err == nil {
			t.Errorf("validatePrefix(%q) returned nil, want error", p)
		}
	}
}
