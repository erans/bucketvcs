package gcs

import (
	"errors"
	"net/http"
	"testing"

	gstorage "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		op   gcsOp
		err  error
		want error
	}{
		{"nil", opGet, nil, nil},
		{"object-not-exist", opGet, gstorage.ErrObjectNotExist, bvstorage.ErrNotFound},
		{"404", opGet, &googleapi.Error{Code: http.StatusNotFound}, bvstorage.ErrNotFound},
		{"412 putIfAbsent -> AlreadyExists", opPutIfAbsent, &googleapi.Error{Code: http.StatusPreconditionFailed}, bvstorage.ErrAlreadyExists},
		{"412 putIfMatch -> VersionMismatch", opPutIfMatch, &googleapi.Error{Code: http.StatusPreconditionFailed}, bvstorage.ErrVersionMismatch},
		{"412 deleteIfMatch -> VersionMismatch", opDeleteIfMatch, &googleapi.Error{Code: http.StatusPreconditionFailed}, bvstorage.ErrVersionMismatch},
		{"412 completeIfAbsent -> AlreadyExists", opCompleteIfAbsent, &googleapi.Error{Code: http.StatusPreconditionFailed}, bvstorage.ErrAlreadyExists},
		{"429 throttled", opGet, &googleapi.Error{Code: http.StatusTooManyRequests}, bvstorage.ErrThrottled},
		{"403 access denied", opGet, &googleapi.Error{Code: http.StatusForbidden}, bvstorage.ErrAccessDenied},
		{"503 transient", opGet, &googleapi.Error{Code: http.StatusServiceUnavailable}, bvstorage.ErrTransient},
		{"400 invalid", opPutIfAbsent, &googleapi.Error{Code: http.StatusBadRequest}, bvstorage.ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.op, tc.err)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("classify: want nil, got %v", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("classify: got %v, want errors.Is(%v)", got, tc.want)
			}
		})
	}
}
