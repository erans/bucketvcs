package s3compat_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/conformance"
	"github.com/bucketvcs/bucketvcs/internal/storage/s3compat"
)

// makeFactory returns a conformance.Factory bound to base. Each call
// gets a fresh prefix under the base config so the suite's
// many-fresh-store calls don't collide.
func makeFactory(t *testing.T, base s3compat.Config) conformance.Factory {
	t.Helper()
	if err := base.Validate(); err != nil {
		t.Fatalf("base config invalid: %v", err)
	}
	return func(tb testing.TB) (storage.ObjectStore, func()) {
		tb.Helper()
		cfg := base
		cfg.Prefix = fmt.Sprintf("conformance/%s/", uuid.New().String())

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s, err := s3compat.Open(ctx, cfg)
		if err != nil {
			tb.Fatalf("s3compat.Open: %v", err)
		}
		cleanup := func() {
			cleanupCtx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer ccancel()
			cleanupPrefix(tb, s, cleanupCtx)
		}
		return s, cleanup
	}
}

// cleanupPrefix attempts to delete every object under the test's
// configured prefix. Runs after each Factory invocation. Uses
// best-effort semantics: failures are logged, never fatal, since
// cleanup must not block other tests in the same suite from running.
//
// Step 1 aborts any in-progress multipart uploads that the conformance
// suite may have left behind (e.g. the "multipart complete cannot
// silently overwrite" tests). These accumulate cost on real S3/R2 if
// not reclaimed.
//
// Step 2 deletes listed objects page-by-page via DeleteIfVersionMatches.
// If a delete fails (race with another goroutine, transient error), we
// log and move on.
func cleanupPrefix(tb testing.TB, s storage.ObjectStore, ctx context.Context) {
	tb.Helper()

	// Step 1: Abort orphan multipart uploads.
	if sc, ok := s.(*s3compat.S3Compat); ok {
		if err := sc.AbortMultipartsUnderPrefix(ctx); err != nil {
			tb.Logf("conformance cleanup: abort multiparts: %v", err)
		}
	}

	// Step 2: Delete listed objects, page-by-page.
	for {
		page, err := s.List(ctx, "", nil)
		if err != nil {
			tb.Logf("conformance cleanup: list error: %v", err)
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
			break
		}
	}
}

func TestConformance_R2(t *testing.T) {
	bucket := os.Getenv("BUCKETVCS_R2_BUCKET")
	endpoint := os.Getenv("BUCKETVCS_R2_ENDPOINT")
	if bucket == "" || endpoint == "" {
		t.Skip("R2 conformance: set BUCKETVCS_R2_BUCKET, BUCKETVCS_R2_ENDPOINT, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY")
	}
	cfg := s3compat.Config{
		Bucket:          bucket,
		Region:          envOr("BUCKETVCS_R2_REGION", "auto"),
		Endpoint:        endpoint,
		ForcePathStyle:  true,
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}
	conformance.Run(t, makeFactory(t, cfg))
}

func TestConformance_S3(t *testing.T) {
	bucket := os.Getenv("BUCKETVCS_S3_BUCKET")
	region := os.Getenv("BUCKETVCS_S3_REGION")
	if bucket == "" || region == "" {
		t.Skip("S3 conformance: set BUCKETVCS_S3_BUCKET, BUCKETVCS_S3_REGION, AWS credentials")
	}
	cfg := s3compat.Config{
		Bucket:          bucket,
		Region:          region,
		Endpoint:        os.Getenv("BUCKETVCS_S3_ENDPOINT"),
		ForcePathStyle:  os.Getenv("BUCKETVCS_S3_FORCE_PATH_STYLE") == "true",
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}
	conformance.Run(t, makeFactory(t, cfg))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
