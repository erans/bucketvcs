// Package storage defines the provider-neutral storage contract used by
// every bucketvcs adapter. Adapters (localfs in M0; AWS S3, GCS, R2, Azure
// Blob in later milestones) implement ObjectStore and must pass the
// conformance suite for the specific backend/configuration in use.
package storage

import "errors"

// Sentinel errors returned by ObjectStore implementations. Callers compare
// against these with errors.Is to make routing decisions.
//
// Adapters wrap their underlying provider errors with these sentinels so
// classification is consistent across providers. The conformance suite
// verifies the mapping per §29 #13 and #15 of the original spec.
var (
	// ErrNotFound: the requested object does not exist.
	ErrNotFound = errors.New("storage: object not found")

	// ErrAlreadyExists: PutIfAbsent or CompleteMultipartIfAbsent failed
	// because the target key is already present.
	ErrAlreadyExists = errors.New("storage: object already exists")

	// ErrVersionMismatch: PutIfVersionMatches or DeleteIfVersionMatches
	// failed because the on-store version differs from the expected
	// version.
	ErrVersionMismatch = errors.New("storage: version mismatch")

	// ErrThrottled: the provider is rate-limiting. Caller may retry with
	// backoff.
	ErrThrottled = errors.New("storage: throttled")

	// ErrTransient: a retryable transient failure (network blip, brief
	// provider unavailability). Caller may retry.
	ErrTransient = errors.New("storage: transient error")

	// ErrInvalidArgument: the caller supplied an argument that violates
	// the contract (malformed key, negative offset, etc.).
	ErrInvalidArgument = errors.New("storage: invalid argument")

	// ErrAccessDenied: authentication or authorization with the provider
	// failed. Not retryable.
	ErrAccessDenied = errors.New("storage: access denied")

	// ErrNotSupported: the operation is not supported by this adapter.
	// Inspect Capabilities() to decide before calling.
	ErrNotSupported = errors.New("storage: not supported by adapter")

	// ErrReadOnlyReplica: the store is the read-side composition of a
	// regional replica bucket over a canonical bucket; all write methods
	// are refused. Defense in depth under the gateway-level replica
	// refusals — no code path may mutate either bucket.
	ErrReadOnlyReplica = errors.New("storage: read-only replica store")
)
