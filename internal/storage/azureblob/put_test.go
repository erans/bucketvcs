package azureblob

import (
	"bytes"
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestPutIfAbsentRejectsBadKey(t *testing.T) {
	a := &AzureBlob{}
	_, err := a.PutIfAbsent(context.Background(), "", bytes.NewReader(nil), nil)
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("got %v, want ErrInvalidArgument", err)
	}
}

func TestPutIfVersionMatchesRejectsWrongProvider(t *testing.T) {
	a := &AzureBlob{}
	_, err := a.PutIfVersionMatches(context.Background(), "k", bvstorage.ObjectVersion{Provider: "gcs", Token: "x"}, bytes.NewReader(nil), nil)
	if err == nil || !errors.Is(err, bvstorage.ErrVersionMismatch) {
		t.Fatalf("got %v, want ErrVersionMismatch", err)
	}
}
