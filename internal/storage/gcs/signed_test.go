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

func TestSignedGetURL_ExpectedHash_DoesNotBreakValidation(t *testing.T) {
	// The field is silently accepted (not bound at the URL layer) on
	// GCS; this unit test only verifies
	// that supplying ExpectedHash does not interfere with the existing
	// key-validation path. Positive-path coverage (URL is byte-identical
	// fetchable) lives in RunCapabilitySigning conformance.
	g := &GCS{}
	_, err := g.SignedGetURL(context.Background(), "/leading", bvstorage.SignedURLOptions{
		ExpectedHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument with ExpectedHash set, got %v", err)
	}
}

func TestSignedGetURLRejectsUnknownMethod(t *testing.T) {
	g := &GCS{}
	_, err := g.SignedGetURL(context.Background(), "k", bvstorage.SignedURLOptions{
		Method: "DELETE",
	})
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}

func TestSignedGetURLRejectsBadKey_PUT(t *testing.T) {
	// PUT path must still reject invalid keys before signing is attempted.
	g := &GCS{}
	_, err := g.SignedGetURL(context.Background(), "/leading", bvstorage.SignedURLOptions{
		Method: "PUT",
	})
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}
