package gcs

import (
	"fmt"
	"strings"
)

// ParseURL parses --store URLs of the form gcs://<bucket>[/<prefix>].
// Credentials in the URL are rejected — the only supported credential
// paths are env vars, Application Default Credentials, and explicit
// CredentialsJSON / CredentialsFile.
func ParseURL(raw string) (Config, error) {
	colon := strings.Index(raw, "://")
	if colon <= 0 {
		return Config{}, fmt.Errorf("gcs: unsupported scheme in %q (want gcs://)", raw)
	}
	scheme := raw[:colon]
	if scheme != "gcs" {
		return Config{}, fmt.Errorf("gcs: unsupported scheme %q (want gcs://)", scheme)
	}
	rest := raw[colon+3:]
	if rest == "" {
		return Config{}, fmt.Errorf("gcs: gcs://: bucket required")
	}
	bucket, prefix, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return Config{}, fmt.Errorf("gcs: gcs://: bucket required")
	}
	if strings.ContainsRune(bucket, '@') {
		return Config{}, fmt.Errorf("gcs: gcs:// URL must not contain credentials; use Application Default Credentials or CredentialsJSON/CredentialsFile")
	}
	cfg := Config{Bucket: bucket}
	if prefix != "" {
		norm, err := normalizePrefix(prefix)
		if err != nil {
			return Config{}, fmt.Errorf("gcs: gcs:// prefix: %w", err)
		}
		cfg.Prefix = norm
	}
	return cfg, nil
}
