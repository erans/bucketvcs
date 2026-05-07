package s3compat

import (
	"strings"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	good := Config{Bucket: "b", Region: "us-east-1"}
	if err := good.Validate(); err != nil {
		t.Fatalf("good config: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"empty bucket", func(c *Config) { c.Bucket = "" }, "bucket"},
		{"empty region", func(c *Config) { c.Region = "" }, "region"},
		{"bad prefix leading slash", func(c *Config) { c.Prefix = "/foo/" }, "prefix"},
		{"bad prefix dotdot", func(c *Config) { c.Prefix = "foo/../bar/" }, "prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestConfigValidateDefaults(t *testing.T) {
	c := Config{Bucket: "b", Region: "us-east-1"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.applyDefaults()
	if c.UploadPartSize == 0 {
		t.Fatalf("UploadPartSize default not applied")
	}
	if c.MaxRetries == 0 {
		t.Fatalf("MaxRetries default not applied")
	}
	if c.RequestTimeout <= 0 {
		t.Fatalf("RequestTimeout default not applied")
	}
	if c.PresignDefaultTTL <= 0 {
		t.Fatalf("PresignDefaultTTL default not applied")
	}
	// Sanity check magnitudes.
	if c.UploadPartSize < 5<<20 {
		t.Fatalf("UploadPartSize %d below 5 MiB", c.UploadPartSize)
	}
	if c.RequestTimeout < time.Second {
		t.Fatalf("RequestTimeout %v unreasonably small", c.RequestTimeout)
	}
}

func TestConfigValidateR2RequiresEndpoint(t *testing.T) {
	c := Config{Bucket: "b", Region: "auto", scheme: "r2"}
	if err := c.Validate(); err == nil {
		t.Fatalf("r2:// without endpoint must fail validation")
	}
	c.Endpoint = "https://abc.r2.cloudflarestorage.com"
	if err := c.Validate(); err != nil {
		t.Fatalf("r2:// with endpoint should pass: %v", err)
	}
}
