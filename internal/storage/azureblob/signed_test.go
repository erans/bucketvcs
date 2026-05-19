package azureblob

import (
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestSignedGetURLNoKeyReturnsNotSupported(t *testing.T) {
	a := &AzureBlob{}
	_, _, err := a.SignedGetURL(context.Background(), "k", bvstorage.SignedURLOptions{})
	if err == nil || !errors.Is(err, bvstorage.ErrNotSupported) {
		t.Fatalf("got %v, want ErrNotSupported", err)
	}
}

func TestSignedGetURL_ExpectedHash_IgnoredOnNoCredentials(t *testing.T) {
	// ExpectedHash is silently ignored on azureblob; this unit test
	// only verifies that supplying it does not change the no-credentials
	// ErrNotSupported path. Positive-path coverage (URL is fetchable)
	// lives in RunCapabilitySigning conformance.
	a := &AzureBlob{}
	_, _, err := a.SignedGetURL(context.Background(), "k", bvstorage.SignedURLOptions{
		ExpectedHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err == nil || !errors.Is(err, bvstorage.ErrNotSupported) {
		t.Fatalf("got %v with ExpectedHash set, want ErrNotSupported", err)
	}
}

func TestSignedGetURLRejectsUnknownMethod(t *testing.T) {
	a := &AzureBlob{}
	_, _, err := a.SignedGetURL(context.Background(), "k", bvstorage.SignedURLOptions{
		Method: "DELETE",
	})
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("got %v, want ErrInvalidArgument", err)
	}
}

func TestSignedGetURL_PUT_NoCredentials_StillNotSupported(t *testing.T) {
	// Like the existing GET no-credentials test: PUT with no credentials
	// must still return ErrNotSupported (not ErrInvalidArgument) because
	// the method is valid; signing is what fails.
	a := &AzureBlob{}
	_, _, err := a.SignedGetURL(context.Background(), "k", bvstorage.SignedURLOptions{
		Method: "PUT",
	})
	if err == nil || !errors.Is(err, bvstorage.ErrNotSupported) {
		t.Fatalf("got %v, want ErrNotSupported", err)
	}
}

func TestSignedGetURLRejectsBadKey_PUT(t *testing.T) {
	// PUT path must still reject invalid keys before signing is attempted.
	a := &AzureBlob{}
	_, _, err := a.SignedGetURL(context.Background(), "/leading", bvstorage.SignedURLOptions{
		Method: "PUT",
	})
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("got %v, want ErrInvalidArgument", err)
	}
}
