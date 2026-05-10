package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
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
		return "", "", fmt.Errorf(`--store: scheme %q is reserved; cloud adapter for this provider lands at M7`, scheme)
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
	default:
		return nil, fmt.Errorf("unreachable: scheme %q passed parseStoreURL but openStore has no constructor", scheme)
	}
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
