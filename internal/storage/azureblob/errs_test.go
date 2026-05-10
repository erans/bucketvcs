package azureblob

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestClassify(t *testing.T) {
	makeErr := func(status int, code string) error {
		return &azcore.ResponseError{
			StatusCode: status,
			ErrorCode:  code,
		}
	}
	tests := []struct {
		name string
		op   azureOp
		err  error
		want error
	}{
		{"nil", opGet, nil, nil},
		{"404", opGet, makeErr(http.StatusNotFound, "BlobNotFound"), bvstorage.ErrNotFound},
		{"412 putIfAbsent -> AlreadyExists", opPutIfAbsent, makeErr(http.StatusPreconditionFailed, "BlobAlreadyExists"), bvstorage.ErrAlreadyExists},
		{"412 putIfMatch -> VersionMismatch", opPutIfMatch, makeErr(http.StatusPreconditionFailed, "ConditionNotMet"), bvstorage.ErrVersionMismatch},
		{"409 BlobAlreadyExists -> AlreadyExists", opPutIfAbsent, makeErr(http.StatusConflict, "BlobAlreadyExists"), bvstorage.ErrAlreadyExists},
		{"429 throttled", opGet, makeErr(http.StatusTooManyRequests, "TooManyRequests"), bvstorage.ErrThrottled},
		{"403 access denied", opGet, makeErr(http.StatusForbidden, "AuthenticationFailed"), bvstorage.ErrAccessDenied},
		{"503 transient", opGet, makeErr(http.StatusServiceUnavailable, "ServerBusy"), bvstorage.ErrTransient},
		{"400 invalid", opPutIfAbsent, makeErr(http.StatusBadRequest, "InvalidBlobOrBlock"), bvstorage.ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.op, tc.err)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("got %v, want errors.Is(%v)", got, tc.want)
			}
		})
	}
}
