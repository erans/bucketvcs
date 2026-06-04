package gcs_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	gcconformance "github.com/bucketvcs/bucketvcs/internal/gc/conformance"
	maintconformance "github.com/bucketvcs/bucketvcs/internal/maintenance/conformance"
	reachconformance "github.com/bucketvcs/bucketvcs/internal/reachability/conformance"
	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/conformance"
	"github.com/bucketvcs/bucketvcs/internal/storage/gcs"
)

func TestGCSConformance(t *testing.T) {
	bucket := os.Getenv("BUCKETVCS_GCS_BUCKET")
	if bucket == "" {
		t.Skip("BUCKETVCS_GCS_BUCKET unset — skipping live GCS conformance")
	}
	base := gcs.Config{
		Bucket:          bucket,
		Endpoint:        os.Getenv("BUCKETVCS_GCS_ENDPOINT"),
		CredentialsFile: os.Getenv("BUCKETVCS_GCS_CREDENTIALS_FILE"),
	}
	conformance.Run(t, makeGCSFactory(t, base))
}

func makeGCSFactory(t *testing.T, base gcs.Config) conformance.Factory {
	t.Helper()
	if err := base.Validate(); err != nil {
		t.Fatalf("base config invalid: %v", err)
	}
	return func(tb testing.TB) (bvstorage.ObjectStore, func()) {
		tb.Helper()
		cfg := base
		cfg.Prefix = fmt.Sprintf("conformance/%s/", uuid.New().String())
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s, err := gcs.Open(ctx, cfg)
		if err != nil {
			tb.Fatalf("gcs.Open: %v", err)
		}
		cleanup := func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer ccancel()
			cleanupGCSPrefix(tb, s, cctx)
			_ = s.Close()
		}
		return s, cleanup
	}
}

func cleanupGCSPrefix(tb testing.TB, s bvstorage.ObjectStore, ctx context.Context) {
	tb.Helper()
	if g, ok := s.(*gcs.GCS); ok {
		_ = g.AbortMultipartsUnderPrefix(ctx)
	}
	for {
		page, err := s.List(ctx, "", nil)
		if err != nil {
			tb.Logf("conformance cleanup: list: %v", err)
			return
		}
		if len(page.Objects) == 0 {
			return
		}
		for _, o := range page.Objects {
			if err := s.DeleteIfVersionMatches(ctx, o.Key, o.Version); err != nil {
				tb.Logf("conformance cleanup: delete %q: %v", o.Key, err)
			}
		}
		if page.NextToken == "" {
			return
		}
	}
}

func TestGcs_GCSafety(t *testing.T) {
	bucket := os.Getenv("BUCKETVCS_GCS_BUCKET")
	if bucket == "" {
		t.Skip("BUCKETVCS_GCS_BUCKET unset — skipping live GCS GC safety")
	}
	base := gcs.Config{
		Bucket:          bucket,
		Endpoint:        os.Getenv("BUCKETVCS_GCS_ENDPOINT"),
		CredentialsFile: os.Getenv("BUCKETVCS_GCS_CREDENTIALS_FILE"),
	}
	gcconformance.RunPropertyGCSafety(t, gcconformance.Factory(makeGCSFactory(t, base)))
	maintconformance.RunPropertyMaintenanceSafety(t, maintconformance.Factory(makeGCSFactory(t, base)))
	maintconformance.RunPropertyBundleSafety(t, maintconformance.Factory(makeGCSFactory(t, base)))
}

func TestGcs_ReachabilitySafety(t *testing.T) {
	bucket := os.Getenv("BUCKETVCS_GCS_BUCKET")
	if bucket == "" {
		t.Skip("BUCKETVCS_GCS_BUCKET unset — skipping live GCS reachability safety")
	}
	base := gcs.Config{
		Bucket:          bucket,
		Endpoint:        os.Getenv("BUCKETVCS_GCS_ENDPOINT"),
		CredentialsFile: os.Getenv("BUCKETVCS_GCS_CREDENTIALS_FILE"),
	}
	reachconformance.RunPropertyReachabilitySafety(t, reachconformance.Factory(makeGCSFactory(t, base)))
}

func TestGCS_Signing(t *testing.T) {
	bucket := os.Getenv("BUCKETVCS_GCS_BUCKET")
	if bucket == "" {
		t.Skip("BUCKETVCS_GCS_BUCKET unset — skipping live GCS signing conformance")
	}
	// fake-gcs-server (and any signing-incapable emulator) ignores the
	// cryptographic signature entirely: it returns 200 for expired or
	// tampered URLs, which RunCapabilitySigning correctly flags as a
	// signing failure. Signing semantics can only be validated against a
	// backend that actually verifies V4 signatures, so the emulator
	// conformance script sets BUCKETVCS_CONFORMANCE_NO_SIGNING to opt
	// this test out. The nightly real-cloud job leaves the marker unset
	// and runs the full signing suite against live GCS.
	if os.Getenv("BUCKETVCS_CONFORMANCE_NO_SIGNING") != "" {
		t.Skip("BUCKETVCS_CONFORMANCE_NO_SIGNING set — emulator cannot verify URL signatures; signing is covered by the real-cloud job")
	}
	base := gcs.Config{
		Bucket:          bucket,
		Endpoint:        os.Getenv("BUCKETVCS_GCS_ENDPOINT"),
		CredentialsFile: os.Getenv("BUCKETVCS_GCS_CREDENTIALS_FILE"),
	}
	conformance.RunCapabilitySigning(t, makeGCSFactory(t, base))
}
