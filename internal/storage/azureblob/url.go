package azureblob

import (
	"fmt"
	"strings"
)

// ParseURL parses a "--store" URL of the form:
//
//	azureblob://<container>[/<prefix>]
//
// Account and credentials are NEVER taken from the URL — they come
// from env vars or DefaultAzureCredential.
func ParseURL(raw string) (Config, error) {
	colon := strings.Index(raw, "://")
	if colon <= 0 {
		return Config{}, fmt.Errorf("azureblob: unsupported scheme in %q (want azureblob://)", raw)
	}
	scheme := raw[:colon]
	if scheme != "azureblob" {
		return Config{}, fmt.Errorf("azureblob: unsupported scheme %q (want azureblob://)", scheme)
	}
	rest := raw[colon+3:]
	if rest == "" {
		return Config{}, fmt.Errorf("azureblob: azureblob://: container required")
	}
	cont, prefix, _ := strings.Cut(rest, "/")
	if cont == "" {
		return Config{}, fmt.Errorf("azureblob: azureblob://: container required")
	}
	if strings.ContainsRune(cont, '@') {
		return Config{}, fmt.Errorf("azureblob: azureblob:// URL must not contain credentials; use BUCKETVCS_AZURE_ACCOUNT_KEY, BUCKETVCS_AZURE_CONNECTION_STRING, or DefaultAzureCredential")
	}
	cfg := Config{Container: cont}
	if prefix != "" {
		norm, err := normalizePrefix(prefix)
		if err != nil {
			return Config{}, fmt.Errorf("azureblob: azureblob:// prefix: %w", err)
		}
		cfg.Prefix = norm
	}
	return cfg, nil
}
