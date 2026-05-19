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

func TestEmulatorHostAndScheme(t *testing.T) {
	// Production default: empty endpoint means SDK falls back to
	// storage.googleapis.com over HTTPS — we leave Hostname/Insecure
	// unset by returning ok=false.
	cases := []struct {
		name         string
		endpoint     string
		wantHost     string
		wantInsecure bool
		wantOK       bool
	}{
		{"empty endpoint, production default", "", "", false, false},
		{"fake-gcs http", "http://localhost:4443/storage/v1/", "localhost:4443", true, true},
		{"fake-gcs https", "https://emul.example:4443/storage/v1/", "emul.example:4443", false, true},
		{"host only", "http://127.0.0.1:9000", "127.0.0.1:9000", true, true},
		{"unparseable", ":::not a url:::", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, insecure, ok := emulatorHostAndScheme(c.endpoint)
			if host != c.wantHost || insecure != c.wantInsecure || ok != c.wantOK {
				t.Errorf("got (%q, %v, %v); want (%q, %v, %v)",
					host, insecure, ok, c.wantHost, c.wantInsecure, c.wantOK)
			}
		})
	}
}
