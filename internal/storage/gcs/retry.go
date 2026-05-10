package gcs

import (
	gstorage "cloud.google.com/go/storage"
)

// retryParams captures the parameters we hand to BucketHandle.Retryer.
// We always use RetryAlways (the GCS SDK's "retry idempotent reads and
// preconditioned writes" policy); 412 PreconditionFailed is NOT retried
// by design — that case is the conditional-write contract we rely on.
type retryParams struct {
	maxAttempts int
	policy      gstorage.RetryPolicy
}

func retryOpts(cfg Config) retryParams {
	return retryParams{
		maxAttempts: cfg.MaxRetries,
		policy:      gstorage.RetryAlways,
	}
}

// applyRetry wraps a BucketHandle with the configured retryer.
func applyRetry(b *gstorage.BucketHandle, p retryParams) *gstorage.BucketHandle {
	return b.Retryer(
		gstorage.WithMaxAttempts(p.maxAttempts),
		gstorage.WithPolicy(p.policy),
	)
}
