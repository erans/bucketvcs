package gcs

import (
	"bytes"
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestPutIfAbsentRejectsBadKey(t *testing.T) {
	g := &GCS{}
	_, err := g.PutIfAbsent(context.Background(), "/leading", bytes.NewReader(nil), nil)
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("got %v, want ErrInvalidArgument", err)
	}
}

func TestPutIfVersionMatchesRejectsWrongProviderToken(t *testing.T) {
	g := &GCS{}
	_, err := g.PutIfVersionMatches(context.Background(), "k", bvstorage.ObjectVersion{Provider: "s3compat", Token: "1"}, bytes.NewReader(nil), nil)
	if err == nil || !errors.Is(err, bvstorage.ErrVersionMismatch) {
		t.Fatalf("got %v, want ErrVersionMismatch", err)
	}
}
