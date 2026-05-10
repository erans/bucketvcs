package diffharness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/azureblob"
	"github.com/bucketvcs/bucketvcs/internal/storage/gcs"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
	"github.com/bucketvcs/bucketvcs/internal/storage/s3compat"
)

// openStoreFromURL parses url and returns a storage.ObjectStore. The
// supported schemes mirror cmd/bucketvcs: localfs:<path>, s3://<bucket>,
// r2://<bucket>, gcs://<bucket>, azureblob://<container>. For cloud
// schemes, env vars supply the missing config like the CLI does.
func openStoreFromURL(t *testing.T, url string) (storage.ObjectStore, error) {
	t.Helper()
	switch {
	case strings.HasPrefix(url, "localfs:"):
		return localfs.Open(strings.TrimPrefix(url, "localfs:"))
	case strings.HasPrefix(url, "s3://"), strings.HasPrefix(url, "r2://"):
		cfg, err := s3compat.ParseURL(url)
		if err != nil {
			return nil, err
		}
		envOverlay(&cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return s3compat.Open(ctx, cfg)
	case strings.HasPrefix(url, "gcs://"):
		cfg, err := gcs.ParseURL(url)
		if err != nil {
			return nil, err
		}
		envOverlayGCS(&cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return gcs.Open(ctx, cfg)
	case strings.HasPrefix(url, "azureblob://"):
		cfg, err := azureblob.ParseURL(url)
		if err != nil {
			return nil, err
		}
		envOverlayAzure(&cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return azureblob.Open(ctx, cfg)
	default:
		return nil, fmt.Errorf("unsupported diffharness store URL %q (want localfs:<path>, s3://<bucket>, r2://<bucket>, gcs://<bucket>, or azureblob://<container>)", url)
	}
}

// envOverlay mirrors cmd/bucketvcs/applyEnvToConfig. Kept inline here
// because cmd/bucketvcs is a main package and can't be imported.
func envOverlay(cfg *s3compat.Config) {
	if v := os.Getenv("BUCKETVCS_S3_REGION"); v != "" {
		cfg.Region = v
	} else if v := os.Getenv("AWS_REGION"); v != "" && cfg.Region == "" {
		cfg.Region = v
	}
	if v := os.Getenv("BUCKETVCS_S3_ENDPOINT"); v != "" {
		cfg.Endpoint = v
	}
	if v := os.Getenv("BUCKETVCS_S3_FORCE_PATH_STYLE"); v != "" {
		cfg.ForcePathStyle = (v == "true" || v == "1")
	}
	if v := os.Getenv("BUCKETVCS_S3_PROFILE"); v != "" {
		cfg.Profile = v
	} else if v := os.Getenv("AWS_PROFILE"); v != "" && cfg.Profile == "" {
		cfg.Profile = v
	}
	if v := os.Getenv("AWS_ACCESS_KEY_ID"); v != "" {
		cfg.AccessKeyID = v
		cfg.SecretAccessKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
		cfg.SessionToken = os.Getenv("AWS_SESSION_TOKEN")
	}
}

// envOverlayGCS mirrors cmd/bucketvcs/applyEnvToGCSConfig. Kept inline here
// because cmd/bucketvcs is a main package and can't be imported.
func envOverlayGCS(cfg *gcs.Config) {
	if v := os.Getenv("BUCKETVCS_GCS_ENDPOINT"); v != "" {
		cfg.Endpoint = v
	}
	if v := os.Getenv("BUCKETVCS_GCS_CREDENTIALS_FILE"); v != "" {
		cfg.CredentialsFile = v
	}
	if v := os.Getenv("BUCKETVCS_GCS_USER_PROJECT"); v != "" {
		cfg.UserProject = v
	}
}

// envOverlayAzure mirrors cmd/bucketvcs/applyEnvToAzureConfig. Kept inline
// here because cmd/bucketvcs is a main package and can't be imported.
func envOverlayAzure(cfg *azureblob.Config) {
	if v := os.Getenv("BUCKETVCS_AZURE_ACCOUNT"); v != "" {
		cfg.Account = v
	}
	if v := os.Getenv("BUCKETVCS_AZURE_SERVICE_URL"); v != "" {
		cfg.ServiceURL = v
	}
	if v := os.Getenv("BUCKETVCS_AZURE_ACCOUNT_KEY"); v != "" {
		cfg.AccountKey = v
	}
	if v := os.Getenv("BUCKETVCS_AZURE_CONNECTION_STRING"); v != "" {
		cfg.ConnectionString = v
	}
}

func closeStore(s storage.ObjectStore) error {
	if c, ok := s.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// TestNewTestStoreFallsBackToLocalfs sanity-checks the env override:
// without BUCKETVCS_DIFFHARNESS_STORE we still get a usable store.
func TestNewTestStoreFallsBackToLocalfs(t *testing.T) {
	t.Setenv("BUCKETVCS_DIFFHARNESS_STORE", "")
	s := newTestStore(t)
	if s == nil {
		t.Fatal("newTestStore returned nil")
	}
}

// TestNewTestStoreLocalfsScheme exercises the localfs:<path> override.
func TestNewTestStoreLocalfsScheme(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BUCKETVCS_DIFFHARNESS_STORE", "localfs:"+dir)
	s := newTestStore(t)
	if s == nil {
		t.Fatal("newTestStore returned nil")
	}
}
