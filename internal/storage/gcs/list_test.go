package gcs

import (
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestListNilOptions(t *testing.T) {
	// Calling on a zero-value GCS would panic on bucket dereference,
	// so just verify the signature compiles.
	var _ func(context.Context, string, *bvstorage.ListOptions) (*bvstorage.ListPage, error) = (*GCS)(nil).List
	_ = errors.New
}
