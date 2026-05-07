package s3compat

import (
	"errors"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// fakeAPIError builds a smithy APIError. The classifier matches by
// API error code first, then HTTP status from any wrapping
// awshttp.ResponseError.
func fakeAPIError(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code, Fault: smithy.FaultClient}
}

func fakeHTTPError(status int, code string) error {
	return &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
			Err:      fakeAPIError(code),
		},
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		op   condOp
		err  error
		want error
	}{
		{"not found by code", opGet, fakeAPIError("NoSuchKey"), storage.ErrNotFound},
		{"not found by 404", opGet, fakeHTTPError(404, "NotFound"), storage.ErrNotFound},
		{"412 on PutIfAbsent -> AlreadyExists", opPutIfAbsent, fakeHTTPError(412, "PreconditionFailed"), storage.ErrAlreadyExists},
		{"412 on PutIfMatch -> VersionMismatch", opPutIfMatch, fakeHTTPError(412, "PreconditionFailed"), storage.ErrVersionMismatch},
		{"412 on DeleteIfMatch -> VersionMismatch", opDeleteIfMatch, fakeHTTPError(412, "PreconditionFailed"), storage.ErrVersionMismatch},
		{"412 on opGet -> VersionMismatch", opGet, fakeHTTPError(412, "PreconditionFailed"), storage.ErrVersionMismatch},
		{"throttled by SlowDown", opGet, fakeAPIError("SlowDown"), storage.ErrThrottled},
		{"throttled by 429", opGet, fakeHTTPError(429, ""), storage.ErrThrottled},
		{"transient 5xx", opGet, fakeHTTPError(503, ""), storage.ErrTransient},
		{"access denied 403", opGet, fakeHTTPError(403, "AccessDenied"), storage.ErrAccessDenied},
		{"invalid argument", opPutIfAbsent, fakeAPIError("InvalidArgument"), storage.ErrInvalidArgument},
		{"unknown error -> transient", opGet, errors.New("connection refused"), storage.ErrTransient},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.op, tc.err)
			if !errors.Is(got, tc.want) {
				t.Fatalf("classify(%v, %v) = %v, want errors.Is %v", tc.op, tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyPreservesUnderlying(t *testing.T) {
	src := fakeAPIError("NoSuchKey")
	got := classify(opGet, src)
	if !errors.Is(got, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", got)
	}
	// Unwrapping should still surface the smithy APIError so operators
	// can see the provider code.
	var apiErr smithy.APIError
	if !errors.As(got, &apiErr) {
		t.Fatalf("classify must preserve the original smithy APIError via errors.As")
	}
	if apiErr.ErrorCode() != "NoSuchKey" {
		t.Fatalf("preserved code = %q, want NoSuchKey", apiErr.ErrorCode())
	}
}
