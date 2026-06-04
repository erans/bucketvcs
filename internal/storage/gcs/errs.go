package gcs

import (
	"errors"
	"fmt"
	"net/http"

	gstorage "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

type gcsOp int

const (
	opGet              gcsOp = iota
	opHead                   // HEAD request, no conditional headers
	opGetRange               // GET with Range header
	opList                   // list pages
	opPutIfAbsent            // PUT with If-None-Match: *  -> 412 = ErrAlreadyExists
	opPutIfMatch             // PUT with If-Match: <etag>  -> 412 = ErrVersionMismatch
	opDeleteIfMatch          // DELETE with If-Match: <etag> -> 412 = ErrVersionMismatch
	opCreateMultipart        // CreateMultipartUpload
	opUploadPart             // UploadPart
	opCompleteIfAbsent       // CompleteMultipartUpload with If-None-Match: *
	opAbortMultipart         // AbortMultipartUpload
	opSignedURL              // SignedURL generation
)

// classify maps an SDK error to a storage sentinel. The original error
// remains reachable via errors.As / errors.Unwrap.
func classify(op gcsOp, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gstorage.ErrObjectNotExist) || errors.Is(err, gstorage.ErrBucketNotExist) {
		return wrap(bvstorage.ErrNotFound, err)
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusNotFound:
			return wrap(bvstorage.ErrNotFound, err)
		case http.StatusPreconditionFailed:
			switch op {
			case opPutIfAbsent, opCompleteIfAbsent:
				return wrap(bvstorage.ErrAlreadyExists, err)
			case opPutIfMatch, opDeleteIfMatch, opGet:
				return wrap(bvstorage.ErrVersionMismatch, err)
			default:
				return wrap(bvstorage.ErrTransient, err)
			}
		case http.StatusTooManyRequests:
			return wrap(bvstorage.ErrThrottled, err)
		case http.StatusUnauthorized, http.StatusForbidden:
			return wrap(bvstorage.ErrAccessDenied, err)
		case http.StatusBadRequest:
			return wrap(bvstorage.ErrInvalidArgument, err)
		case http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusBadGateway, http.StatusInternalServerError:
			return wrap(bvstorage.ErrTransient, err)
		}
	}
	return fmt.Errorf("gcs: %w", err)
}

func wrap(sentinel, cause error) error {
	return fmt.Errorf("gcs: %w: %v", sentinel, cause)
}
