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
		t.Fatalf("SlowDown throttle code must be retryable by SDK retryer (regardless of HTTP status)")
	}
}

func TestRetryerDoesNotRetryPlain429(t *testing.T) {
	// The SDK standard retryer's default retryable HTTP status set is
	// {500, 502, 503, 504}; 429 is NOT included. 429 retries are
	// driven by smithy throttle error codes (SlowDown, etc.), not by
	// the status alone. This test pins that behavior so we notice if
	// the SDK changes its default set.
	r := newRetryer(5)
	err := fakeHTTPError(429, "") // no recognized throttle code
	if r.IsErrorRetryable(err) {
		t.Fatalf("plain 429 (no throttle error code) is NOT in the SDK default retryable HTTP status set; if this fails, the SDK changed and we should reconsider whether to extend the retryable set")
	}
}

func TestRetryerRetries5xx(t *testing.T) {
	r := newRetryer(5)
	err := fakeHTTPError(503, "")
	if !r.IsErrorRetryable(err) {
		t.Fatalf("503 must be retryable by SDK default retryer (DefaultRetryableHTTPStatusCodes includes 500/502/503/504)")
	}
}
