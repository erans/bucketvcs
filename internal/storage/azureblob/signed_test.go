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

func TestSignedGetURL_ExpectedHash_IgnoredOnNoCredentials(t *testing.T) {
	// ExpectedHash is silently ignored on azureblob; this unit test
	// only verifies that supplying it does not change the no-credentials
	// ErrNotSupported path. Positive-path coverage (URL is fetchable)
	// lives in RunCapabilitySigning conformance.
	a := &AzureBlob{}
	_, err := a.SignedGetURL(context.Background(), "k", bvstorage.SignedURLOptions{
		ExpectedHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err == nil || !errors.Is(err, bvstorage.ErrNotSupported) {
		t.Fatalf("got %v with ExpectedHash set, want ErrNotSupported", err)
	}
}
