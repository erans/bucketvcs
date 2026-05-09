package azureblob

import (
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestDeleteRejectsBadKey(t *testing.T) {
	a := &AzureBlob{}
	err := a.DeleteIfVersionMatches(context.Background(), "/leading", bvstorage.ObjectVersion{Token: "x"})
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("got %v, want ErrInvalidArgument", err)
	}
}
