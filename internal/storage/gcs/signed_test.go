package gcs

import (
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestSignedGetURLRejectsBadKey(t *testing.T) {
	g := &GCS{}
	_, err := g.SignedGetURL(context.Background(), "/leading", bvstorage.SignedURLOptions{})
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}
