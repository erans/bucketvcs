package azureblob

import (
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestValidateKey(t *testing.T) {
	good := []string{"a", "a/b", "objects/ab/cdef"}
	for _, k := range good {
		if err := validateKey(k); err != nil {
			t.Errorf("validateKey(%q) unexpected err: %v", k, err)
		}
	}
	bad := []string{"", "/leading", "trailing/", "double//slash", "contains/../segment", "with\x00null", "with\\backslash"}
	for _, k := range bad {
		err := validateKey(k)
		if err == nil {
			t.Errorf("validateKey(%q) expected error", k)
			continue
		}
		if !errors.Is(err, bvstorage.ErrInvalidArgument) {
			t.Errorf("validateKey(%q) err = %v, want wraps ErrInvalidArgument", k, err)
		}
	}
}
