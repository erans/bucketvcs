package gcs_test

import (
	"context"
	"os"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/gcs"
)

func TestOpenRejectsBadConfig(t *testing.T) {
	_, err := gcs.Open(context.Background(), gcs.Config{})
	if err == nil {
		t.Fatal("Open: want error for empty Bucket")
	}
}

// TestOpenAgainstFakeGCS verifies Open succeeds and the resulting
// adapter exposes a working bucket handle. Skipped when the emulator
// is not running.
func TestOpenAgainstFakeGCS(t *testing.T) {
	endpoint := os.Getenv("BUCKETVCS_GCS_ENDPOINT")
	bucket := os.Getenv("BUCKETVCS_GCS_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("set BUCKETVCS_GCS_ENDPOINT and BUCKETVCS_GCS_BUCKET (e.g. via scripts/conformance-emulators.sh) to enable")
	}
	g, err := gcs.Open(context.Background(), gcs.Config{
		Bucket:   bucket,
		Endpoint: endpoint,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if g == nil {
		t.Fatal("Open returned nil GCS")
	}
	if err := g.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
