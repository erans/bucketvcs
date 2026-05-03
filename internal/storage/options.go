package storage

import (
	"io"
	"time"
)

// Object is the result of a successful Get. The caller must Close Body
// when done.
type Object struct {
	Body     io.ReadCloser
	Metadata ObjectMetadata
}

// ObjectMetadata describes a stored object without its body bytes.
type ObjectMetadata struct {
	Key         string
	Version     ObjectVersion
	Size        int64
	ContentType string
	ModifiedAt  time.Time
}

// Capabilities advertises what an adapter supports. Conformance tests
// gate behavior on these flags so an adapter can declare honestly that
// it does not implement an optional capability.
type Capabilities struct {
	// SignedURLs reports whether SignedGetURL returns a working URL. If
	// false, SignedGetURL returns ErrNotSupported.
	SignedURLs bool

	// MultipartMinPartSize is the minimum allowed part size in bytes for
	// non-final parts. Zero means no minimum.
	MultipartMinPartSize int64

	// MultipartMaxParts is the maximum number of parts the adapter will
	// accept. Zero means no adapter-imposed cap.
	MultipartMaxParts int

	// MaxObjectSize is the maximum size of a single object in bytes.
	// Zero means no adapter-imposed cap.
	MaxObjectSize int64

	// StrongList reports whether List provides strong read-after-write
	// for objects PUT before the call.
	StrongList bool
}

// GetOptions controls Get behavior.
type GetOptions struct {
	// IfVersionMatches, when non-nil, causes Get to return
	// ErrVersionMismatch if the on-store version differs.
	IfVersionMatches *ObjectVersion
}

// PutOptions controls Put-family behavior. M0 ships only ContentType;
// user-defined metadata is intentionally deferred (AD9 in the M0 design
// spec). Cloud adapters at M5/M7 reintroduce metadata mapped to
// provider-native fields (S3 x-amz-meta-*, GCS object metadata, etc.).
type PutOptions struct {
	ContentType string
}

// ListOptions controls List behavior.
type ListOptions struct {
	// MaxKeys caps the page size. Zero means adapter-default.
	MaxKeys int

	// ContinuationToken is the NextToken from a previous ListPage. Empty
	// means start from the beginning of the prefix.
	ContinuationToken string

	// Delimiter, if non-empty, causes the adapter to roll keys ending in
	// Delimiter into CommonPrefixes rather than Objects.
	Delimiter string
}

// ListPage is one page of List results.
type ListPage struct {
	Objects        []ObjectMetadata
	NextToken      string
	CommonPrefixes []string
}

// MultipartOptions controls CreateMultipart.
type MultipartOptions struct {
	ContentType string
}

// SignedURLOptions controls SignedGetURL.
type SignedURLOptions struct {
	Expires time.Duration
	Method  string // typically "GET"
}
