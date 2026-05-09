package gcs

import (
	"strings"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"ok", Config{Bucket: "b"}, ""},
		{"missing bucket", Config{}, "bucket is required"},
		{"bad prefix", Config{Bucket: "b", Prefix: "//bad"}, "invalid prefix"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate: want nil, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate: want %q, got nil", tc.wantErr)
			case tc.wantErr != "" && err != nil:
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Validate: want %q, got %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestConfigApplyDefaults(t *testing.T) {
	c := Config{Bucket: "b"}
	c.applyDefaults()
	if c.UploadChunkSize != defaultUploadChunkSize {
		t.Errorf("UploadChunkSize = %d, want %d", c.UploadChunkSize, defaultUploadChunkSize)
	}
	if c.MaxRetries != defaultMaxRetries {
		t.Errorf("MaxRetries = %d, want %d", c.MaxRetries, defaultMaxRetries)
	}
	if c.RequestTimeout != defaultRequestTimeout {
		t.Errorf("RequestTimeout = %v, want %v", c.RequestTimeout, defaultRequestTimeout)
	}
	if c.PresignDefaultTTL != defaultPresignDefaultTTL {
		t.Errorf("PresignDefaultTTL = %v, want %v", c.PresignDefaultTTL, defaultPresignDefaultTTL)
	}
}
