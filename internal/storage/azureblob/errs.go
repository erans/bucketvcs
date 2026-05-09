package azureblob

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

type azureOp int

const (
	opGet           azureOp = iota
	opHead                  // HEAD request
	opGetRange              // GET with Range header
	opList                  // list pages
	opPutIfAbsent           // PUT with If-None-Match: *  -> 412/409 = ErrAlreadyExists
	opPutIfMatch            // PUT with If-Match: <etag>  -> 412 = ErrVersionMismatch
	opDeleteIfMatch         // DELETE with If-Match: <etag> -> 412 = ErrVersionMismatch
	opStageBlock            // StageBlock (PutBlock)
	opCommitIfAbsent        // CommitBlockList with If-None-Match: *
	opSignedURL             // SAS URL generation
)

// classify maps an SDK error to a storage sentinel. The original error
// remains reachable via errors.As / errors.Unwrap.
func classify(op azureOp, err error) error {
	if err == nil {
		return nil
	}
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		switch re.StatusCode {
		case http.StatusNotFound:
			return wrap(bvstorage.ErrNotFound, err)
		case http.StatusPreconditionFailed:
			switch op {
			case opPutIfAbsent, opCommitIfAbsent:
				return wrap(bvstorage.ErrAlreadyExists, err)
			case opPutIfMatch, opDeleteIfMatch, opGet:
				return wrap(bvstorage.ErrVersionMismatch, err)
			default:
				return wrap(bvstorage.ErrTransient, err)
			}
		case http.StatusConflict:
			// BlobAlreadyExists also returns 409 in some create paths
			// (Put Block List on a blob with a snapshot etc.).
			switch op {
			case opPutIfAbsent, opCommitIfAbsent:
				return wrap(bvstorage.ErrAlreadyExists, err)
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
	return fmt.Errorf("azureblob: %w", err)
}

func wrap(sentinel, cause error) error {
	return fmt.Errorf("azureblob: %w: %v", sentinel, cause)
}
