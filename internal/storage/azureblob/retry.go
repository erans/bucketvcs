package azureblob

import (
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// retryOpts returns the policy used by the Azure pipeline. 412 is NOT
// retried — that case is the conditional-write contract we rely on.
// The Azure SDK does not retry 4xx by default, so no explicit opt-out
// is required.
func retryOpts(cfg Config) policy.RetryOptions {
	return policy.RetryOptions{
		MaxRetries:    int32(cfg.MaxRetries),
		TryTimeout:    cfg.RequestTimeout,
		RetryDelay:    250 * time.Millisecond,
		MaxRetryDelay: 30 * time.Second,
	}
}
