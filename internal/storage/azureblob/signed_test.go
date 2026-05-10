package azureblob

import (
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestSignedGetURLNoKeyReturnsNotSupported(t *testing.T) {
	a := &AzureBlob{}
	_, err := a.SignedGetURL(context.Background(), "k", bvstorage.SignedURLOptions{})
	if err == nil || !errors.Is(err, bvstorage.ErrNotSupported) {
		t.Fatalf("got %v, want ErrNotSupported", err)
	}
}
