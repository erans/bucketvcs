package storage

import (
	"context"
	"io"
)

// MultipartPart describes one uploaded part of a multipart upload.
// Adapters define the meaning of Token; localfs uses hex sha256 of part
// bytes.
type MultipartPart struct {
	PartNumber int
	Token      string
	Size       int64
}

// MultipartUpload is a handle to an in-progress multipart upload.
// CreateMultipart returns one of these. The caller uploads parts via
// UploadPart, then completes the upload via
// ObjectStore.CompleteMultipartIfAbsent. If the upload should be
// discarded, the caller calls Abort.
type MultipartUpload interface {
	// UploadID is the adapter-defined identifier for this upload. It
	// must be stable for the life of the upload.
	UploadID() string

	// Key is the target object key the upload will become on completion.
	Key() string

	// UploadPart uploads one part. PartNumber is 1-based (1, 2, 3,
	// ...). Out-of-order and repeated part numbers are allowed at
	// upload time; uploading the same partNumber twice overwrites the
	// prior part. Final ordering and contiguity are validated at
	// CompleteMultipartIfAbsent. Body may exceed MultipartMinPartSize
	// for non-final parts.
	UploadPart(ctx context.Context, partNumber int, body io.Reader) (MultipartPart, error)

	// Abort cancels the upload and removes any temporary state. After
	// Abort, no further calls on this upload are valid.
	Abort(ctx context.Context) error
}
