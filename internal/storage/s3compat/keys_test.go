package s3compat

import (
	"errors"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestValidateKey(t *testing.T) {
	good := []string{
		"a",
		"foo/bar",
		"deeply/nested/path/file.json",
		strings.Repeat("a", 1024),
	}
	for _, k := range good {
		if err := validateKey(k); err != nil {
			t.Errorf("validateKey(%q) unexpected error: %v", k, err)
		}
	}

	bad := []struct {
		key  string
		want string // substring expected in error message
	}{
		{"", "empty"},
		{strings.Repeat("a", 1025), "exceeds"},
		{"/foo", "start"},
		{"foo/", "end"},
		{"foo//bar", "empty segment"},
		{"foo\x00bar", "NUL"},
		{"foo\\bar", "backslash"},
		{"./foo", "'.' or '..'"},
		{"foo/./bar", "'.' or '..'"},
		{"foo/../bar", "'.' or '..'"},
		{"..", "'.' or '..'"},
	}
	for _, tc := range bad {
		t.Run(tc.key, func(t *testing.T) {
			err := validateKey(tc.key)
			if err == nil {
				t.Fatalf("validateKey(%q) expected error", tc.key)
			}
			if !errors.Is(err, storage.ErrInvalidArgument) {
				t.Fatalf("validateKey(%q) error %v not ErrInvalidArgument", tc.key, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateKey(%q) error %q does not contain %q", tc.key, err.Error(), tc.want)
			}
		})
	}
}
