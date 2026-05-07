package s3compat

import (
	"net/http"
	"testing"
)

func TestRetryerHonorsMaxAttempts(t *testing.T) {
	r := newRetryer(7)
	if got := r.MaxAttempts(); got != 7 {
		t.Fatalf("MaxAttempts() = %d, want 7", got)
	}
}

func TestRetryerDoesNotRetry412(t *testing.T) {
	r := newRetryer(5)
	err := fakeHTTPError(http.StatusPreconditionFailed, "PreconditionFailed")
	if r.IsErrorRetryable(err) {
		t.Fatalf("412 PreconditionFailed must not be retried by SDK retryer")
	}
}

func TestRetryerRetriesThrottling(t *testing.T) {
	r := newRetryer(5)
	err := fakeHTTPError(http.StatusTooManyRequests, "SlowDown")
	if !r.IsErrorRetryable(err) {
		t.Fatalf("429 must be retryable by SDK retryer")
	}
}
