package azureblob

import (
	"fmt"
	"time"
)

// Config is the only constructor input to Open. The CLI builds one from
// a parsed URL plus environment variables; tests construct it directly.
type Config struct {
	Account   string // required if no ConnectionString
	Container string // required
	Prefix    string

	ServiceURL       string // optional override (Azurite uses this)
	AccountKey       string // optional Shared Key (enables SAS)
	ConnectionString string // optional; precedence over Account/ServiceURL/AccountKey

	UploadBlockSize   int64
	MaxRetries        int
	RequestTimeout    time.Duration
	PresignDefaultTTL time.Duration
}

const (
	defaultUploadBlockSize   = 8 << 20
	defaultMaxRetries        = 5
	defaultRequestTimeout    = 60 * time.Second
	defaultPresignDefaultTTL = 15 * time.Minute
)

func (c *Config) Validate() error {
	if c.Container == "" {
		return fmt.Errorf("azureblob: container is required")
	}
	if c.Account == "" && c.ConnectionString == "" {
		return fmt.Errorf("azureblob: account or connection string is required")
	}
	if _, err := normalizePrefix(c.Prefix); err != nil {
		return fmt.Errorf("azureblob: invalid prefix: %w", err)
	}
	return nil
}

func (c *Config) applyDefaults() {
	if p, err := normalizePrefix(c.Prefix); err == nil {
		c.Prefix = p
	}
	if c.UploadBlockSize == 0 {
		c.UploadBlockSize = defaultUploadBlockSize
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
