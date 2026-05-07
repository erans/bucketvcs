package s3compat

import (
	"fmt"
	"time"
)

// Config is the only constructor input to Open. The CLI builds one from
// a parsed URL plus environment variables; tests construct it directly.
//
// Credentials are passed as fields rather than read from env to keep the
// adapter testable. The CLI is responsible for env -> Config translation
// and for honoring the AWS SDK default credential chain when no static
// credentials are provided.
type Config struct {
	Bucket          string // required
	Prefix          string // optional; trailing "/" normalized
	Region          string // required; "auto" for R2
	Endpoint        string // optional for AWS S3; required for R2/MinIO
	ForcePathStyle  bool   // true for R2/MinIO; false for AWS S3
	AccessKeyID     string // optional; falls back to default chain
	SecretAccessKey string // pairs with AccessKeyID
	SessionToken    string // optional STS session token
	Profile         string // optional shared-config profile name

	UploadPartSize    int64
	MaxRetries        int
	RequestTimeout    time.Duration
	PresignDefaultTTL time.Duration

	// scheme is set by ParseURL ("s3" | "r2") to drive scheme-specific
	// validation rules. Not exported; callers that build Config directly
	// can leave it empty (we apply S3 defaults).
	scheme string
}

const (
	defaultUploadPartSize    = 8 << 20
	defaultMaxRetries        = 5
	defaultRequestTimeout    = 60 * time.Second
	defaultPresignDefaultTTL = 15 * time.Minute
)

// Validate checks required fields. It does NOT mutate the receiver.
// Call applyDefaults explicitly to populate optional tunables.
func (c *Config) Validate() error {
	if c.Bucket == "" {
		return fmt.Errorf("s3compat: bucket is required")
	}
	if c.Region == "" {
		return fmt.Errorf("s3compat: region is required (use \"auto\" for R2)")
	}
	if _, err := normalizePrefix(c.Prefix); err != nil {
		return fmt.Errorf("s3compat: invalid prefix: %w", err)
	}
	if c.scheme == "r2" && c.Endpoint == "" {
		return fmt.Errorf("s3compat: r2:// requires Endpoint (set BUCKETVCS_S3_ENDPOINT)")
	}
	return nil
}

// applyDefaults populates zero-valued tunables. After this returns, the
// Config is suitable for handing to the SDK.
func (c *Config) applyDefaults() {
	if p, err := normalizePrefix(c.Prefix); err == nil {
		c.Prefix = p
	}
	if c.UploadPartSize == 0 {
		c.UploadPartSize = defaultUploadPartSize
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
