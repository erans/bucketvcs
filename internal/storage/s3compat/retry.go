package s3compat

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
)

// newRetryer returns the SDK standard retryer configured with our
// MaxAttempts. We deliberately do NOT extend the retryable error
// set: the SDK standard retryer already covers the cases we want
// (5xx 500/502/503/504, throttle error codes such as SlowDown and
// TooManyRequestsException, RequestTimeout, and connection errors)
// and explicitly does NOT retry 412 PreconditionFailed, which we
// depend on for CAS correctness.
//
// We clamp MaxAttempts < 1 to 1 (single attempt, no retry) rather
// than letting the SDK fall back to its default of 3. A misconfigured
// caller should get the safer "no retry" answer, not silent retries
// they didn't ask for.
func newRetryer(maxAttempts int) aws.Retryer {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = maxAttempts
	})
}
