package s3compat

import (
	"fmt"
	"strings"
)

// ParseURL parses a "--store" URL of the form:
//
//	s3://<bucket>[/<prefix>]
//	r2://<bucket>[/<prefix>]
//
// It populates a Config seed; the CLI is responsible for layering env
// vars onto the result before calling Validate / Open.
//
// ParseURL deliberately rejects credentials in the URL: the only
// supported credential paths are env vars and the SDK shared-config
// profile.
func ParseURL(raw string) (Config, error) {
	colon := strings.Index(raw, "://")
	if colon <= 0 {
		// Allow the legacy "scheme:path" shape for parity with the
		// existing CLI parser, but reject if scheme is unknown.
		if i := strings.IndexByte(raw, ':'); i > 0 {
			scheme := raw[:i]
			if scheme == "s3" || scheme == "r2" {
				return Config{}, fmt.Errorf("s3compat: %q: bucket required (use %s://<bucket>[/<prefix>])", raw, scheme)
			}
		}
		return Config{}, fmt.Errorf("s3compat: unsupported scheme in %q (want s3:// or r2://)", raw)
	}
	scheme := raw[:colon]
	rest := raw[colon+3:]
	switch scheme {
	case "s3", "r2":
	default:
		return Config{}, fmt.Errorf("s3compat: unsupported scheme %q (want s3:// or r2://)", scheme)
	}
	if rest == "" {
		return Config{}, fmt.Errorf("s3compat: %s://: bucket required", scheme)
	}
	bucket, prefix, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return Config{}, fmt.Errorf("s3compat: %s://: bucket required", scheme)
	}
	// Reject credentials in the URL (user:pass@host or user@host form).
	// Credentials must come from env vars or a shared-config profile.
	if strings.ContainsRune(bucket, '@') {
		return Config{}, fmt.Errorf("s3compat: %s:// URL must not contain credentials; set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY in env or use a shared-config profile (BUCKETVCS_S3_PROFILE / AWS_PROFILE)", scheme)
	}
	cfg := Config{
		scheme: scheme,
		Bucket: bucket,
	}
	if prefix != "" {
		norm, err := normalizePrefix(prefix)
		if err != nil {
			return Config{}, fmt.Errorf("s3compat: %s:// prefix: %w", scheme, err)
		}
		cfg.Prefix = norm
	}
	if scheme == "r2" {
		cfg.ForcePathStyle = true
		cfg.Region = "auto"
	}
	return cfg, nil
}
