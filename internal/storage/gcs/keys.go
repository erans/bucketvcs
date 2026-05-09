package gcs

import (
	"fmt"
	"strings"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func validateKey(k string) error {
	if k == "" {
		return fmt.Errorf("%w: key must not be empty", bvstorage.ErrInvalidArgument)
	}
	if strings.HasPrefix(k, "/") {
		return fmt.Errorf("%w: key must not start with /: %q", bvstorage.ErrInvalidArgument, k)
	}
	if strings.HasSuffix(k, "/") {
		return fmt.Errorf("%w: key must not end with /: %q", bvstorage.ErrInvalidArgument, k)
	}
	if strings.Contains(k, "//") {
		return fmt.Errorf("%w: key must not contain consecutive /: %q", bvstorage.ErrInvalidArgument, k)
	}
	return nil
}
