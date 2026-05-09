package gcs

import (
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestDeleteRejectsBadKey(t *testing.T) {
	g := &GCS{}
	err := g.DeleteIfVersionMatches(context.Background(), "", bvstorage.ObjectVersion{Token: "1"})
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("got %v, want ErrInvalidArgument", err)
	}
}
