package gcs

import "testing"

func TestRetryOptions(t *testing.T) {
	cfg := Config{Bucket: "b", MaxRetries: 7}
	cfg.applyDefaults()
	opts := retryOpts(cfg)
	if opts.maxAttempts != 7 {
		t.Errorf("maxAttempts = %d, want 7", opts.maxAttempts)
	}
}
