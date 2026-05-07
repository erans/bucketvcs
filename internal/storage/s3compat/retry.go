package s3compat

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
)

// newRetryer returns the SDK standard retryer configured with our
// MaxAttempts. We deliberately do NOT extend the retryable error set:
// the standard retryer already covers 5xx, 429, RequestTimeout,
// SlowDown, and connection errors, and explicitly does NOT retry 412
// PreconditionFailed (which we depend on for CAS correctness).
func newRetryer(maxAttempts int) aws.Retryer {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = maxAttempts
	})
}
