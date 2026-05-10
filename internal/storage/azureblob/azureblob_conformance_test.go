package azureblob_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	gcconformance "github.com/bucketvcs/bucketvcs/internal/gc/conformance"
	maintconformance "github.com/bucketvcs/bucketvcs/internal/maintenance/conformance"
	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/azureblob"
	"github.com/bucketvcs/bucketvcs/internal/storage/conformance"
)

func TestAzureBlobConformance(t *testing.T) {
	cont := os.Getenv("BUCKETVCS_AZURE_CONTAINER")
	if cont == "" {
		t.Skip("BUCKETVCS_AZURE_CONTAINER unset — skipping live azureblob conformance")
	}
	base := azureblob.Config{
		Container:        cont,
		Account:          os.Getenv("BUCKETVCS_AZURE_ACCOUNT"),
		AccountKey:       os.Getenv("BUCKETVCS_AZURE_ACCOUNT_KEY"),
		ConnectionString: os.Getenv("BUCKETVCS_AZURE_CONNECTION_STRING"),
		ServiceURL:       os.Getenv("BUCKETVCS_AZURE_SERVICE_URL"),
	}
	conformance.Run(t, makeFactory(t, base))
}

func makeFactory(t *testing.T, base azureblob.Config) conformance.Factory {
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
		s, err := azureblob.Open(ctx, cfg)
		if err != nil {
			tb.Fatalf("azureblob.Open: %v", err)
		}
		cleanup := func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer ccancel()
			cleanupPrefix(tb, s, cctx)
			_ = s.Close()
		}
		return s, cleanup
	}
}

func cleanupPrefix(tb testing.TB, s bvstorage.ObjectStore, ctx context.Context) {
	tb.Helper()
	if a, ok := s.(*azureblob.AzureBlob); ok {
		_ = a.AbortMultipartsUnderPrefix(ctx)
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

func TestAzureBlob_GCSafety(t *testing.T) {
	cont := os.Getenv("BUCKETVCS_AZURE_CONTAINER")
	if cont == "" {
		t.Skip("BUCKETVCS_AZURE_CONTAINER unset — skipping live azureblob GC safety")
	}
	base := azureblob.Config{
		Container:        cont,
		Account:          os.Getenv("BUCKETVCS_AZURE_ACCOUNT"),
		AccountKey:       os.Getenv("BUCKETVCS_AZURE_ACCOUNT_KEY"),
		ConnectionString: os.Getenv("BUCKETVCS_AZURE_CONNECTION_STRING"),
		ServiceURL:       os.Getenv("BUCKETVCS_AZURE_SERVICE_URL"),
	}
	gcconformance.RunPropertyGCSafety(t, gcconformance.Factory(makeFactory(t, base)))
	maintconformance.RunPropertyMaintenanceSafety(t, maintconformance.Factory(makeFactory(t, base)))
}
