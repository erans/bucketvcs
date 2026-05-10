package gcs

import (
	"strings"
	"testing"
)

func TestParseURL(t *testing.T) {
	tests := []struct {
		raw, wantBucket, wantPrefix, wantErr string
	}{
		{"gcs://my-bucket", "my-bucket", "", ""},
		{"gcs://my-bucket/repos", "my-bucket", "repos/", ""},
		{"gcs://my-bucket/repos/staging/", "my-bucket", "repos/staging/", ""},
		{"gcs://", "", "", "bucket required"},
		{"s3://my-bucket", "", "", "unsupported scheme"},
		{"gcs://user:pass@bucket", "", "", "must not contain credentials"},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := ParseURL(tc.raw)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ParseURL(%q): want nil, got %v", tc.raw, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("ParseURL(%q): want %q, got nil", tc.raw, tc.wantErr)
			case tc.wantErr != "":
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseURL(%q): want %q, got %v", tc.raw, tc.wantErr, err)
				}
				return
			}
			if cfg.Bucket != tc.wantBucket {
				t.Errorf("Bucket = %q, want %q", cfg.Bucket, tc.wantBucket)
			}
			if cfg.Prefix != tc.wantPrefix {
				t.Errorf("Prefix = %q, want %q", cfg.Prefix, tc.wantPrefix)
			}
		})
	}
}
