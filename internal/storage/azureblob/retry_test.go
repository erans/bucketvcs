package azureblob

import "testing"

func TestRetryOptionsApplied(t *testing.T) {
	cfg := Config{MaxRetries: 7}
	cfg.applyDefaults()
	opts := retryOpts(cfg)
	if opts.MaxRetries != 7 {
		t.Errorf("MaxRetries = %d, want 7", opts.MaxRetries)
	}
}
