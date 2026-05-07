package s3compat

import (
	"errors"
	"fmt"
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	smithy "github.com/aws/smithy-go"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// condOp tells classify which conditional header semantics applied to
// the request that produced err. 412 PreconditionFailed has different
// caller-visible meanings depending on whether the call set
// If-None-Match: * (create-only) or If-Match: <etag> (update-only).
type condOp int

const (
	opGet              condOp = iota
	opHead                    // HEAD request, no conditional headers
	opGetRange                // GET with Range header
	opList                    // ListObjectsV2 / list pages
	opPutIfAbsent             // PUT with If-None-Match: *  -> 412 = ErrAlreadyExists
	opPutIfMatch              // PUT with If-Match: <etag>  -> 412 = ErrVersionMismatch
	opDeleteIfMatch           // DELETE with If-Match: <etag> -> 412 = ErrVersionMismatch
	opCreateMultipart         // CreateMultipartUpload
	opUploadPart              // UploadPart
	opCompleteIfAbsent        // CompleteMultipartUpload with If-None-Match: *
	opAbortMultipart          // AbortMultipartUpload
)

// classify maps an SDK error to a storage sentinel. The original error
// remains reachable via errors.As / errors.Unwrap so operators see the
// provider code and HTTP status.
func classify(op condOp, err error) error {
	if err == nil {
		return nil
	}

	// Most-specific match: API error codes. Some codes (NoSuchKey,
	// SlowDown, AccessDenied) are unambiguous regardless of HTTP status.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return wrap(storage.ErrNotFound, err)
		case "SlowDown", "ThrottlingException", "RequestLimitExceeded":
			return wrap(storage.ErrThrottled, err)
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return wrap(storage.ErrAccessDenied, err)
		case "InvalidArgument", "MalformedXML", "EntityTooSmall":
			return wrap(storage.ErrInvalidArgument, err)
		}
	}

	// HTTP status fallback for cases the SDK didn't tag with a specific
	// code (e.g. plain 503, R2's occasional generic InternalError).
	var httpErr *awshttp.ResponseError
	if errors.As(err, &httpErr) {
		status := 0
		if httpErr.Response != nil && httpErr.Response.Response != nil {
			status = httpErr.Response.Response.StatusCode
		}
		switch status {
		case http.StatusNotFound:
			return wrap(storage.ErrNotFound, err)
		case http.StatusPreconditionFailed:
			switch op {
			case opPutIfAbsent, opCompleteIfAbsent:
				return wrap(storage.ErrAlreadyExists, err)
			case opPutIfMatch, opDeleteIfMatch:
				return wrap(storage.ErrVersionMismatch, err)
			default:
				return wrap(storage.ErrTransient, err)
			}
		case http.StatusTooManyRequests:
			return wrap(storage.ErrThrottled, err)
		case http.StatusUnauthorized, http.StatusForbidden:
			return wrap(storage.ErrAccessDenied, err)
		}
		if status >= 500 {
			return wrap(storage.ErrTransient, err)
		}
	}

	// Default fallthrough: caller-visible-but-retryable. Prefer
	// false-positive retry over false-positive permanent failure.
	return wrap(storage.ErrTransient, err)
}

// wrap returns sentinel joined with the underlying SDK error so callers
// can errors.Is(sentinel) and errors.As(*smithy.APIError) on the same
// returned value.
func wrap(sentinel, underlying error) error {
	return fmt.Errorf("%w: %w", sentinel, underlying)
}
