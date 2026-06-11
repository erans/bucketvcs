package storage

import (
	"context"
	"io"
	"net/http"
)

// ObjectStore is the provider-neutral storage contract. Every bucketvcs
// adapter implements every method and must pass the conformance suite at
// internal/storage/conformance for the specific backend/configuration in
// use.
//
// Method semantics:
//
//   - Get/Head/GetRange: read paths; return ErrNotFound if the key is
//     absent.
//   - PutIfAbsent: create-only; returns ErrAlreadyExists if the key is
//     already present.
//   - PutIfVersionMatches: update-only with optimistic concurrency;
//     returns ErrVersionMismatch if the on-store version differs from
//     expected, or ErrVersionMismatch if the key does not exist.
//   - DeleteIfVersionMatches: delete with optimistic concurrency; returns
//     ErrVersionMismatch on version skew, ErrNotFound if absent.
//   - List: prefix listing, paginated via ContinuationToken/NextToken.
//   - CreateMultipart/CompleteMultipartIfAbsent: large-object upload
//     path; CompleteMultipartIfAbsent returns ErrAlreadyExists if the
//     target key is already present (the spec §29 #8 invariant).
//   - SignedGetURL: returns a short-lived URL the caller can hand to a
//     third party for read access. Adapters that do not support this
//     return ErrNotSupported and report Capabilities{SignedURLs: false}.
type ObjectStore interface {
	// Name returns the canonical backend kind: "localfs", "s3compat",
	// "gcs", "azureblob". Used by receivepack to populate
	// PushPayload.StorageBackend (spec §24) and by operator-facing logs.
	// Implementations MUST return one of the documented kind strings;
	// new backends pick a stable lowercase identifier.
	Name() string

	// Capabilities reports adapter features and limits.
	Capabilities() Capabilities

	// Get reads an object. Caller must Close the returned Body.
	Get(ctx context.Context, key string, opts *GetOptions) (*Object, error)

	// Head reads metadata without the body.
	Head(ctx context.Context, key string) (*ObjectMetadata, error)

	// GetRange reads bytes [start, endInclusive] from an object. If
	// endInclusive exceeds the object size, the returned reader yields
	// only the existing bytes. Negative indices are ErrInvalidArgument.
	GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error)

	// PutIfAbsent stores body at key only if no object exists at key.
	// Returns ErrAlreadyExists otherwise.
	PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *PutOptions) (ObjectVersion, error)

	// PutIfVersionMatches stores body at key only if the on-store
	// version matches expected. Returns ErrVersionMismatch otherwise
	// (including when the key does not exist).
	PutIfVersionMatches(ctx context.Context, key string, expected ObjectVersion, body io.Reader, opts *PutOptions) (ObjectVersion, error)

	// DeleteIfVersionMatches removes the object only if the on-store
	// version matches expected. Returns ErrVersionMismatch on skew or
	// ErrNotFound if absent.
	DeleteIfVersionMatches(ctx context.Context, key string, expected ObjectVersion) error

	// List returns one page of objects under prefix. Keys are returned in
	// lexicographically ascending order, both within a page and across
	// pages of one logical listing (S3/GCS/Azure guarantee this natively;
	// localfs sorts). Callers may rely on the first key of an unfiltered
	// listing being the lexicographic minimum under the prefix.
	List(ctx context.Context, prefix string, opts *ListOptions) (*ListPage, error)

	// CreateMultipart begins a multipart upload targeting key.
	CreateMultipart(ctx context.Context, key string, opts *MultipartOptions) (MultipartUpload, error)

	// CompleteMultipartIfAbsent assembles parts into the target key only
	// if no object already exists at the target. Returns ErrAlreadyExists
	// otherwise.
	CompleteMultipartIfAbsent(ctx context.Context, upload MultipartUpload, parts []MultipartPart) (ObjectVersion, error)

	// SignedGetURL returns a short-lived URL granting access to the named
	// key. The URL's HTTP method is determined by opts.Method ("GET" or
	// "PUT"). The returned header set carries headers the caller MUST
	// include on the request that uses the signed URL — Azure Blob, for
	// example, requires `x-ms-blob-type: BlockBlob` on a PUT. The header
	// is nil/empty when the backend imposes no such requirement (S3,
	// GCS, localfs). Adapters without signed-URL support return
	// ErrNotSupported.
	//
	// On error (including ErrNotSupported and ErrInvalidArgument) the
	// returned URL is "" and the returned http.Header is nil. Callers
	// can assume an empty URL implies a nil header.
	//
	// Callers that overlay their own headers (e.g. internal/lfs.Store
	// adds Content-Type: application/octet-stream) should merge with the
	// backend's returned header rather than overwriting it.
	SignedGetURL(ctx context.Context, key string, opts SignedURLOptions) (string, http.Header, error)
}
