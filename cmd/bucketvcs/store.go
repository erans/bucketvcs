package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/byob"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/azureblob"
	"github.com/bucketvcs/bucketvcs/internal/storage/gcs"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
	"github.com/bucketvcs/bucketvcs/internal/storage/s3compat"
)

// parseStoreURL parses a --store value into (scheme, scheme-specific
// remainder). Supports localfs:, s3:, r2:, gcs:, and azureblob:.
func parseStoreURL(s string) (scheme, path string, err error) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 {
		return "", "", fmt.Errorf(`--store: missing scheme; want "localfs:<path>", "s3://<bucket>[/<prefix>]", "r2://<bucket>[/<prefix>]", "gcs://<bucket>[/<prefix>]", or "azureblob://<container>[/<prefix>]"`)
	}
	scheme = s[:colon]
	rest := s[colon+1:]
	switch scheme {
	case "localfs":
		if rest == "" {
			return "", "", fmt.Errorf(`--store: %q scheme requires a non-empty path (got %q)`, scheme, s)
		}
		return scheme, rest, nil
	case "s3", "r2":
		// rest should be "//bucket[/prefix]"
		if !strings.HasPrefix(rest, "//") {
			return "", "", fmt.Errorf(`--store: %s URL must use the form %s://<bucket>[/<prefix>] (got %q)`, scheme, scheme, s)
		}
		bucketPath := strings.TrimPrefix(rest, "//")
		bucket, _, _ := strings.Cut(bucketPath, "/")
		if bucket == "" {
			return "", "", fmt.Errorf(`--store: %s:// requires a bucket name (got %q)`, scheme, s)
		}
		return scheme, bucketPath, nil
	case "gcs":
		// rest should be "//bucket[/prefix]"
		if !strings.HasPrefix(rest, "//") {
			return "", "", fmt.Errorf(`--store: %s URL must use the form %s://<bucket>[/<prefix>] (got %q)`, scheme, scheme, s)
		}
		bucketPath := strings.TrimPrefix(rest, "//")
		bucket, _, _ := strings.Cut(bucketPath, "/")
		if bucket == "" {
			return "", "", fmt.Errorf(`--store: %s:// requires a bucket name (got %q)`, scheme, s)
		}
		return scheme, bucketPath, nil
	case "azureblob":
		// rest should be "//container[/prefix]"
		if !strings.HasPrefix(rest, "//") {
			return "", "", fmt.Errorf(`--store: %s URL must use the form %s://<container>[/<prefix>] (got %q)`, scheme, scheme, s)
		}
		containerPath := strings.TrimPrefix(rest, "//")
		container, _, _ := strings.Cut(containerPath, "/")
		if container == "" {
			return "", "", fmt.Errorf(`--store: %s:// requires a container name (got %q)`, scheme, s)
		}
		return scheme, containerPath, nil
	default:
		return "", "", fmt.Errorf(`--store: unknown scheme %q; want "localfs:<path>", "s3://<bucket>[/<prefix>]", "r2://<bucket>[/<prefix>]", "gcs://<bucket>[/<prefix>]", or "azureblob://<container>[/<prefix>]"`, scheme)
	}
}

// openStore parses the --store URL and returns a constructed
// ObjectStore. Caller is responsible for releasing it via closeStore on
// shutdown.
func openStore(url string) (storage.ObjectStore, error) {
	scheme, path, err := parseStoreURL(url)
	if err != nil {
		return nil, err
	}
	switch scheme {
	case "localfs":
		s, err := localfs.Open(path)
		if err != nil {
			return nil, fmt.Errorf("localfs: %w", err)
		}
		return s, nil
	case "s3", "r2":
		cfg, err := s3compat.ParseURL(url)
		if err != nil {
			return nil, err
		}
		applyEnvToConfig(&cfg, scheme)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s, err := s3compat.Open(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("s3compat: %w", err)
		}
		return s, nil
	case "gcs":
		cfg, err := gcs.ParseURL(url)
		if err != nil {
			return nil, err
		}
		applyEnvToGCSConfig(&cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s, err := gcs.Open(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("gcs: %w", err)
		}
		return s, nil
	case "azureblob":
		cfg, err := azureblob.ParseURL(url)
		if err != nil {
			return nil, err
		}
		applyEnvToAzureConfig(&cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s, err := azureblob.Open(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("azureblob: %w", err)
		}
		return s, nil
	default:
		return nil, fmt.Errorf("unreachable: scheme %q passed parseStoreURL but openStore has no constructor", scheme)
	}
}

// openStoreWithCreds is like openStore but applies the decrypted credential
// JSON to the parsed config before opening. Used by the BYOB resolver to
// open per-tenant stores. Does NOT call applyEnvToConfig — env vars are
// operator-level; tenant creds come exclusively from credsJSON.
func openStoreWithCreds(rawURL string, credsJSON []byte) (storage.ObjectStore, error) {
	scheme, _, err := parseStoreURL(rawURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch scheme {
	case "s3", "r2":
		cfg, err := s3compat.ParseURL(rawURL)
		if err != nil {
			return nil, err
		}
		if err := cfg.ApplyCredsJSON(credsJSON); err != nil {
			return nil, err
		}
		return s3compat.Open(ctx, cfg)
	case "gcs":
		cfg, err := gcs.ParseURL(rawURL)
		if err != nil {
			return nil, err
		}
		if err := cfg.ApplyCredsJSON(credsJSON); err != nil {
			return nil, err
		}
		return gcs.Open(ctx, cfg)
	case "azureblob":
		cfg, err := azureblob.ParseURL(rawURL)
		if err != nil {
			return nil, err
		}
		if err := cfg.ApplyCredsJSON(credsJSON); err != nil {
			return nil, err
		}
		return azureblob.Open(ctx, cfg)
	case "localfs":
		return openStore(rawURL) // localfs ignores credsJSON
	default:
		return nil, fmt.Errorf("openStoreWithCreds: unknown scheme %q", scheme)
	}
}

// openByobStore looks up the per-tenant BYOB binding from authdb and opens
// that store. Returns (store, true) when a binding exists; (nil, false) when
// absent or on error — the caller falls back to the --store flag. Errors are
// printed to stderr.
func openByobStore(ctx context.Context, tenant, authDBPath, keyFile string, stderr io.Writer) (storage.ObjectStore, bool) {
	rawKey, err := os.ReadFile(keyFile)
	if err != nil {
		fmt.Fprintf(stderr, "byob: read key: %v\n", err)
		return nil, false
	}
	rawKey = bytes.TrimSpace(rawKey)
	if len(rawKey) < 32 {
		fmt.Fprintf(stderr, "byob: key must be >= 32 bytes\n")
		return nil, false
	}
	authStore, _, err := openAuthDB(authDBPath)
	if err != nil {
		fmt.Fprintf(stderr, "byob: authdb: %v\n", err)
		return nil, false
	}
	defer authStore.Close()

	b, err := authStore.GetStorageBinding(ctx, tenant)
	if errors.Is(err, auth.ErrNoSuchBinding) {
		return nil, false // no binding; caller uses --store
	}
	if err != nil {
		fmt.Fprintf(stderr, "byob: binding lookup %s: %v\n", tenant, err)
		return nil, false
	}
	plain, err := byob.Decrypt(rawKey[:32], b.CredsJSON)
	if err != nil {
		fmt.Fprintf(stderr, "byob: decrypt creds %s: %v\n", tenant, err)
		return nil, false
	}
	s, err := openStoreWithCreds(b.StoreURL, plain)
	if err != nil {
		fmt.Fprintf(stderr, "byob: open store %s: %v\n", tenant, err)
		return nil, false
	}
	return s, true
}

// applyEnvToConfig layers env vars onto a Config seed produced by
// ParseURL. Standard AWS_* env vars are honored by the SDK default
// chain when AccessKeyID is left empty.
func applyEnvToConfig(cfg *s3compat.Config, scheme string) {
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

// applyEnvToGCSConfig layers GCS-specific env vars onto a Config seed
// produced by gcs.ParseURL.
func applyEnvToGCSConfig(cfg *gcs.Config) {
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

// applyEnvToAzureConfig layers Azure-specific env vars onto a Config seed
// produced by azureblob.ParseURL.
func applyEnvToAzureConfig(cfg *azureblob.Config) {
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
