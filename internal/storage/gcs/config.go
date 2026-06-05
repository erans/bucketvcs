package gcs

import (
	"encoding/json"
	"fmt"
	"time"
)

type Config struct {
	Bucket string
	Prefix string

	Endpoint string

	CredentialsJSON []byte
	CredentialsFile string

	UserProject string

	UploadChunkSize   int
	MaxRetries        int
	RequestTimeout    time.Duration
	PresignDefaultTTL time.Duration
}

const (
	defaultUploadChunkSize   = 8 << 20
	defaultMaxRetries        = 5
	defaultRequestTimeout    = 60 * time.Second
	defaultPresignDefaultTTL = 15 * time.Minute
)

func (c *Config) Validate() error {
	if c.Bucket == "" {
		return fmt.Errorf("gcs: bucket is required")
	}
	if _, err := normalizePrefix(c.Prefix); err != nil {
		return fmt.Errorf("gcs: invalid prefix: %w", err)
	}
	return nil
}

// ApplyCredsJSON overlays credential fields from a JSON object onto this
// Config. Structural fields (Bucket, Prefix, Endpoint, UserProject)
// are NOT touched — they come from the store URL and must not change.
// Unknown JSON keys are silently ignored.
func (c *Config) ApplyCredsJSON(raw []byte) error {
	var m struct {
		ServiceAccountJSON string `json:"service_account_json"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("gcs: parse creds JSON: %w", err)
	}
	if m.ServiceAccountJSON != "" {
		c.CredentialsJSON = []byte(m.ServiceAccountJSON)
	}
	return nil
}

func (c *Config) applyDefaults() {
	if p, err := normalizePrefix(c.Prefix); err == nil {
		c.Prefix = p
	}
	if c.UploadChunkSize == 0 {
		c.UploadChunkSize = defaultUploadChunkSize
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = defaultMaxRetries
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = defaultRequestTimeout
	}
	if c.PresignDefaultTTL == 0 {
		c.PresignDefaultTTL = defaultPresignDefaultTTL
	}
}
