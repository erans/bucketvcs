# M5 — First Cloud Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `internal/storage/s3compat`, an S3-compatible `storage.ObjectStore` backed by `aws-sdk-go-v2`. Adapter is canonical for Cloudflare R2 at M5; AWS S3 is exercised in conformance to prove generalization and is formally promoted at M7. Wire `s3://` and `r2://` schemes through `cmd/bucketvcs`. Conformance suite runs against real R2 + real S3 buckets via a `scripts/conformance-cloud.sh` ship-gate runner; a forward-looking GitHub Actions workflow is included for when project CI lands.

**Architecture:** Single package `internal/storage/s3compat` thin-wraps the AWS SDK behind `storage.ObjectStore`. R2 and S3 share the same code path; `r2://` is a CLI alias for `s3://` with R2-flavored defaults. Errors normalize to `internal/storage` sentinels via a single `classify` function. Retries delegate to the SDK retryer with explicit assertions that 412 is not retried. Multipart and presigned URLs use SDK clients directly. The rest of the codebase imports only `storage.ObjectStore`; nothing outside the package imports AWS types.

**Tech Stack:** Go 1.25, `github.com/aws/aws-sdk-go-v2` (`config`, `service/s3`, `feature/s3/manager` for presign, `aws/retry`), stdlib `net/http/httptest` for unit-mocked tests, existing `internal/storage` contract and `internal/storage/conformance` suite from M0.

**Spec:** `docs/superpowers/specs/2026-05-07-m5-first-cloud-backend-design.md`

---

## File Structure

**New files:**

```
internal/storage/s3compat/
  doc.go                          // package overview + usage notes
  s3compat.go                     // type S3Compat, interface assertion, Capabilities()
  config.go                       // Config struct, Validate, defaults
  config_test.go
  url.go                          // ParseURL("s3://..." | "r2://...") -> Config seed
  url_test.go
  prefix.go                       // applyPrefix / stripPrefix
  prefix_test.go
  errs.go                         // classify(op string, err error) error
  errs_test.go
  retry.go                        // newRetryer(cfg) helper
  retry_test.go
  open.go                         // Open(ctx, Config) (*S3Compat, error)
  open_test.go                    // httptest-mocked Open
  get.go
  get_test.go                     // httptest-mocked Get/Head/GetRange
  put.go
  put_test.go                     // httptest-mocked PutIfAbsent / PutIfVersionMatches
  delete.go
  delete_test.go                  // httptest-mocked DeleteIfVersionMatches
  list.go
  list_test.go                    // httptest-mocked List + delimiter
  multipart.go
  multipart_test.go               // httptest-mocked multipart roundtrip
  signed.go
  signed_test.go                  // signed URL form assertion
  s3compat_conformance_test.go    // live R2 + S3 conformance, skip when env unset
  README.md                       // ops + provider quirks (R1 from spec)

scripts/
  conformance-cloud.sh            // ship-gate runner

.github/workflows/
  conformance-cloud.yml           // forward-looking; no-op until project CI lands

docs/
  m5-cloud-quickstart.md          // operator guide for s3:// and r2://
```

**Modified files:**

```
go.mod                            // add aws-sdk-go-v2 dependencies
go.sum                            // resolved module checksums

cmd/bucketvcs/store.go            // wire s3:// and r2:// schemes via s3compat
cmd/bucketvcs/store_test.go       // parser tests for new schemes

internal/diffharness/roundtrip_helpers_test.go
                                  // env-driven store override:
                                  //   BUCKETVCS_DIFFHARNESS_STORE=s3://... | r2://...
```

---

## Task 1: Add aws-sdk-go-v2 dependencies and package skeleton

**Files:**
- Create: `internal/storage/s3compat/doc.go`
- Create: `internal/storage/s3compat/s3compat.go`
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add SDK dependencies**

Run:

```bash
cd /home/eran/work/bucketvcs
go get github.com/aws/aws-sdk-go-v2@latest
go get github.com/aws/aws-sdk-go-v2/config@latest
go get github.com/aws/aws-sdk-go-v2/credentials@latest
go get github.com/aws/aws-sdk-go-v2/service/s3@latest
go get github.com/aws/aws-sdk-go-v2/aws/retry@latest
```

Expected: `go.mod` and `go.sum` updated; `go build ./...` still passes (no usage yet).

- [ ] **Step 2: Create `doc.go`**

```go
// Package s3compat implements storage.ObjectStore against any
// S3-compatible object store via aws-sdk-go-v2. M5 ships this adapter
// as the canonical Cloudflare R2 backend and exercises it against AWS
// S3 to validate generalization; AWS S3 is formally promoted to a
// canonical backend at M7.
//
// The CLI exposes two schemes that route to this package:
//
//   s3://<bucket>[/<prefix>]   AWS S3 defaults (vhost addressing, no
//                              endpoint override required)
//   r2://<bucket>[/<prefix>]   Cloudflare R2 defaults (region "auto",
//                              path-style addressing, endpoint env required)
//
// All credentials come from the AWS SDK default credential chain
// (env vars, shared profile). Credentials are never URL-embedded.
//
// See docs/superpowers/specs/2026-05-07-m5-first-cloud-backend-design.md
// for the design rationale.
package s3compat
```

- [ ] **Step 3: Create `s3compat.go` skeleton**

```go
package s3compat

import (
	"context"
	"errors"
	"io"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// S3Compat is the S3-compatible storage.ObjectStore implementation.
type S3Compat struct {
	cfg     Config
	client  *s3.Client
	presign *s3.PresignClient
}

var _ storage.ObjectStore = (*S3Compat)(nil)

// Capabilities reports the S3-compatible adapter capabilities. Values
// match real provider limits (5 MiB / 10000 parts / 5 TiB; strong list;
// signed URLs).
func (s *S3Compat) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		SignedURLs:           true,
		StrongList:           true,
		MultipartMinPartSize: 5 << 20,
		MultipartMaxParts:    10000,
		MaxObjectSize:        5 << 40,
	}
}

// All other ObjectStore methods return ErrNotSupported until their
// dedicated tasks land. This keeps the package buildable while
// individual methods land one at a time.

var errNotImpl = errors.New("s3compat: not yet implemented (skeleton)")

func (s *S3Compat) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	return nil, errNotImpl
}

func (s *S3Compat) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	return nil, errNotImpl
}

func (s *S3Compat) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	return nil, errNotImpl
}

func (s *S3Compat) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errNotImpl
}

func (s *S3Compat) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errNotImpl
}

func (s *S3Compat) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	return errNotImpl
}

func (s *S3Compat) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	return nil, errNotImpl
}

func (s *S3Compat) CreateMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, errNotImpl
}

func (s *S3Compat) CompleteMultipartIfAbsent(ctx context.Context, upload storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errNotImpl
}

func (s *S3Compat) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	return "", errNotImpl
}
```

- [ ] **Step 4: Verify the package builds**

Run: `go build ./internal/storage/s3compat/...`
Expected: build succeeds (no test files yet).

- [ ] **Step 5: Verify the rest of the repo still builds**

Run: `go build ./...`
Expected: build succeeds.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/storage/s3compat/
git commit -m "M5 task 1: add s3compat package skeleton + aws-sdk-go-v2 deps"
```

---

## Task 2: Path prefix helpers

**Files:**
- Create: `internal/storage/s3compat/prefix.go`
- Create: `internal/storage/s3compat/prefix_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/storage/s3compat/prefix_test.go`:

```go
package s3compat

import "testing"

func TestApplyPrefix(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		key     string
		want    string
	}{
		{"empty prefix", "", "manifests/root.json", "manifests/root.json"},
		{"trailing slash", "tenants/", "acme/repo/manifests/root.json", "tenants/acme/repo/manifests/root.json"},
		{"no trailing slash", "tenants", "acme/repo", "tenants/acme/repo"},
		{"deeply nested", "a/b/c/", "x", "a/b/c/x"},
		{"empty key with prefix", "p/", "", "p/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyPrefix(tc.prefix, tc.key)
			if got != tc.want {
				t.Fatalf("applyPrefix(%q, %q) = %q, want %q", tc.prefix, tc.key, got, tc.want)
			}
		})
	}
}

func TestStripPrefix(t *testing.T) {
	tests := []struct {
		name      string
		prefix    string
		stored    string
		want      string
		wantErr   bool
	}{
		{"empty prefix", "", "manifests/root.json", "manifests/root.json", false},
		{"matching prefix", "tenants/", "tenants/acme/repo", "acme/repo", false},
		{"mismatch is fatal", "tenants/", "other/x", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stripPrefix(tc.prefix, tc.stored)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("stripPrefix(%q, %q) want error, got %q", tc.prefix, tc.stored, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("stripPrefix(%q, %q): unexpected error %v", tc.prefix, tc.stored, err)
			}
			if got != tc.want {
				t.Fatalf("stripPrefix(%q, %q) = %q, want %q", tc.prefix, tc.stored, got, tc.want)
			}
		})
	}
}

func TestNormalizePrefix(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"foo", "foo/", false},
		{"foo/", "foo/", false},
		{"foo/bar/", "foo/bar/", false},
		{"/foo/", "", true},        // leading slash
		{"foo/../bar/", "", true},  // path traversal
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizePrefix(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizePrefix(%q) want error, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePrefix(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("normalizePrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./internal/storage/s3compat/...`
Expected: FAIL — `applyPrefix` / `stripPrefix` / `normalizePrefix` undefined.

- [ ] **Step 3: Implement `prefix.go`**

```go
package s3compat

import (
	"fmt"
	"strings"
)

// normalizePrefix validates and canonicalizes a key prefix:
//   - empty stays empty
//   - non-empty gets a single trailing "/"
//   - leading "/" is rejected
//   - "." or ".." path components are rejected
func normalizePrefix(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("s3compat: prefix must not start with '/' (got %q)", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("s3compat: prefix must not contain '.' or '..' segments (got %q)", p)
		}
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p, nil
}

// applyPrefix prepends the configured prefix to a logical key. Caller
// must ensure prefix has been run through normalizePrefix first.
func applyPrefix(prefix, key string) string {
	if prefix == "" {
		return key
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + key
}

// stripPrefix removes the configured prefix from a stored key. Returns
// an error if the stored key does not start with prefix; the caller
// should treat this as a provider-side bug, not a missing object.
func stripPrefix(prefix, stored string) (string, error) {
	if prefix == "" {
		return stored, nil
	}
	if !strings.HasPrefix(stored, prefix) {
		return "", fmt.Errorf("s3compat: stored key %q does not begin with prefix %q", stored, prefix)
	}
	return stored[len(prefix):], nil
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/storage/s3compat/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/s3compat/prefix.go internal/storage/s3compat/prefix_test.go
git commit -m "M5 task 2: s3compat prefix helpers"
```

---

## Task 3: Error classification

**Files:**
- Create: `internal/storage/s3compat/errs.go`
- Create: `internal/storage/s3compat/errs_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/storage/s3compat/errs_test.go`:

```go
package s3compat

import (
	"errors"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	smithy "github.com/aws/smithy-go"
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
		Response: &awshttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      fakeAPIError(code),
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
		{"throttled by SlowDown", opGet, fakeAPIError("SlowDown"), storage.ErrThrottled},
		{"throttled by 429", opGet, fakeHTTPError(429, ""), storage.ErrThrottled},
		{"transient 5xx", opGet, fakeHTTPError(503, ""), storage.ErrTransient},
		{"access denied 403", opGet, fakeHTTPError(403, "AccessDenied"), storage.ErrAccessDenied},
		{"invalid argument", opPutIfAbsent, fakeAPIError("InvalidArgument"), storage.ErrInvalidArgument},
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
```

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./internal/storage/s3compat/...`
Expected: FAIL — `classify`, `condOp` and op constants undefined.

- [ ] **Step 3: Implement `errs.go`**

```go
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
	opGet condOp = iota
	opHead
	opGetRange
	opList
	opPutIfAbsent     // PUT with If-None-Match: *  -> 412 = ErrAlreadyExists
	opPutIfMatch      // PUT with If-Match: <etag>  -> 412 = ErrVersionMismatch
	opDeleteIfMatch   // DELETE with If-Match: <etag> -> 412 = ErrVersionMismatch
	opCreateMultipart
	opUploadPart
	opCompleteIfAbsent // CompleteMultipartUpload with If-None-Match: *
	opAbortMultipart
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
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/storage/s3compat/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/s3compat/errs.go internal/storage/s3compat/errs_test.go
git commit -m "M5 task 3: s3compat error classifier"
```

---

## Task 4: Retry configuration

**Files:**
- Create: `internal/storage/s3compat/retry.go`
- Create: `internal/storage/s3compat/retry_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/storage/s3compat/retry_test.go`:

```go
package s3compat

import (
	"errors"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
)

func TestRetryerHonorsMaxAttempts(t *testing.T) {
	r := newRetryer(7)
	if got := r.MaxAttempts(); got != 7 {
		t.Fatalf("MaxAttempts() = %d, want 7", got)
	}
}

func TestRetryerDoesNotRetry412(t *testing.T) {
	r := newRetryer(5)
	err := &awshttp.ResponseError{
		Response: &awshttp.Response{Response: &http.Response{StatusCode: http.StatusPreconditionFailed}},
		Err:      errors.New("PreconditionFailed"),
	}
	if r.IsErrorRetryable(err) {
		t.Fatalf("412 PreconditionFailed must not be retried by SDK retryer")
	}
}

func TestRetryerRetriesThrottling(t *testing.T) {
	r := newRetryer(5)
	err := &awshttp.ResponseError{
		Response: &awshttp.Response{Response: &http.Response{StatusCode: http.StatusTooManyRequests}},
		Err:      errors.New("SlowDown"),
	}
	if !r.IsErrorRetryable(err) {
		t.Fatalf("429 must be retryable by SDK retryer")
	}
}
```

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./internal/storage/s3compat/... -run TestRetryer`
Expected: FAIL — `newRetryer` undefined.

- [ ] **Step 3: Implement `retry.go`**

```go
package s3compat

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
)

// newRetryer returns the SDK standard retryer configured with our
// MaxAttempts. We deliberately do NOT extend the retryable error set:
// the standard retryer already covers 5xx, 429, RequestTimeout,
// SlowDown, and connection errors, and explicitly does NOT retry 412
// PreconditionFailed (which we depend on for CAS correctness).
func newRetryer(maxAttempts int) aws.Retryer {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = maxAttempts
	})
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/storage/s3compat/... -run TestRetryer`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/s3compat/retry.go internal/storage/s3compat/retry_test.go
git commit -m "M5 task 4: s3compat retryer config (412 not retried)"
```

---

## Task 5: Config + URL parser

**Files:**
- Create: `internal/storage/s3compat/config.go`
- Create: `internal/storage/s3compat/config_test.go`
- Create: `internal/storage/s3compat/url.go`
- Create: `internal/storage/s3compat/url_test.go`

- [ ] **Step 1: Write the failing tests for Config validation**

Create `internal/storage/s3compat/config_test.go`:

```go
package s3compat

import (
	"strings"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	good := Config{Bucket: "b", Region: "us-east-1"}
	if err := good.Validate(); err != nil {
		t.Fatalf("good config: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"empty bucket", func(c *Config) { c.Bucket = "" }, "bucket"},
		{"empty region", func(c *Config) { c.Region = "" }, "region"},
		{"bad prefix leading slash", func(c *Config) { c.Prefix = "/foo/" }, "prefix"},
		{"bad prefix dotdot", func(c *Config) { c.Prefix = "foo/../bar/" }, "prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestConfigValidateDefaults(t *testing.T) {
	c := Config{Bucket: "b", Region: "us-east-1"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.applyDefaults()
	if c.UploadPartSize == 0 {
		t.Fatalf("UploadPartSize default not applied")
	}
	if c.MaxRetries == 0 {
		t.Fatalf("MaxRetries default not applied")
	}
	if c.RequestTimeout <= 0 {
		t.Fatalf("RequestTimeout default not applied")
	}
	if c.PresignDefaultTTL <= 0 {
		t.Fatalf("PresignDefaultTTL default not applied")
	}
	// Sanity check magnitudes.
	if c.UploadPartSize < 5<<20 {
		t.Fatalf("UploadPartSize %d below 5 MiB", c.UploadPartSize)
	}
	if c.RequestTimeout < time.Second {
		t.Fatalf("RequestTimeout %v unreasonably small", c.RequestTimeout)
	}
}

func TestConfigValidateR2RequiresEndpoint(t *testing.T) {
	c := Config{Bucket: "b", Region: "auto", scheme: "r2"}
	if err := c.Validate(); err == nil {
		t.Fatalf("r2:// without endpoint must fail validation")
	}
	c.Endpoint = "https://abc.r2.cloudflarestorage.com"
	if err := c.Validate(); err != nil {
		t.Fatalf("r2:// with endpoint should pass: %v", err)
	}
}
```

- [ ] **Step 2: Write the failing tests for URL parser**

Create `internal/storage/s3compat/url_test.go`:

```go
package s3compat

import (
	"strings"
	"testing"
)

func TestParseURL(t *testing.T) {
	tests := []struct {
		in            string
		wantScheme    string
		wantBucket    string
		wantPrefix    string
		wantPathStyle bool
		wantErr       string
	}{
		{"s3://my-bucket", "s3", "my-bucket", "", false, ""},
		{"s3://my-bucket/data", "s3", "my-bucket", "data/", false, ""},
		{"s3://my-bucket/data/sub", "s3", "my-bucket", "data/sub/", false, ""},
		{"r2://repo-bucket", "r2", "repo-bucket", "", true, ""},
		{"r2://repo-bucket/p", "r2", "repo-bucket", "p/", true, ""},
		{"s3://", "", "", "", false, "bucket"},
		{"s3:", "", "", "", false, "bucket"},
		{"http://x", "", "", "", false, "scheme"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			c, err := ParseURL(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.scheme != tc.wantScheme {
				t.Fatalf("scheme = %q, want %q", c.scheme, tc.wantScheme)
			}
			if c.Bucket != tc.wantBucket {
				t.Fatalf("Bucket = %q, want %q", c.Bucket, tc.wantBucket)
			}
			if c.Prefix != tc.wantPrefix {
				t.Fatalf("Prefix = %q, want %q", c.Prefix, tc.wantPrefix)
			}
			if c.ForcePathStyle != tc.wantPathStyle {
				t.Fatalf("ForcePathStyle = %v, want %v", c.ForcePathStyle, tc.wantPathStyle)
			}
		})
	}
}

func TestParseURLR2DefaultsRegion(t *testing.T) {
	c, err := ParseURL("r2://b")
	if err != nil {
		t.Fatal(err)
	}
	if c.Region != "auto" {
		t.Fatalf("R2 default region = %q, want \"auto\"", c.Region)
	}
}
```

- [ ] **Step 3: Run tests, expect failure**

Run: `go test ./internal/storage/s3compat/...`
Expected: FAIL — `Config`, `Config.Validate`, `Config.applyDefaults`, `ParseURL` undefined.

- [ ] **Step 4: Implement `config.go`**

```go
package s3compat

import (
	"fmt"
	"time"
)

// Config is the only constructor input to Open. The CLI builds one from
// a parsed URL plus environment variables; tests construct it directly.
//
// Credentials are passed as fields rather than read from env to keep the
// adapter testable. The CLI is responsible for env -> Config translation
// and for honoring the AWS SDK default credential chain when no static
// credentials are provided.
type Config struct {
	Bucket          string        // required
	Prefix          string        // optional; trailing "/" normalized
	Region          string        // required; "auto" for R2
	Endpoint        string        // optional for AWS S3; required for R2/MinIO
	ForcePathStyle  bool          // true for R2/MinIO; false for AWS S3
	AccessKeyID     string        // optional; falls back to default chain
	SecretAccessKey string        // pairs with AccessKeyID
	SessionToken   string         // optional STS session token
	Profile         string        // optional shared-config profile name

	UploadPartSize    int64
	MaxRetries        int
	RequestTimeout    time.Duration
	PresignDefaultTTL time.Duration

	// scheme is set by ParseURL ("s3" | "r2") to drive scheme-specific
	// validation rules. Not exported; callers that build Config directly
	// can leave it empty (we apply S3 defaults).
	scheme string
}

const (
	defaultUploadPartSize    = 8 << 20
	defaultMaxRetries        = 5
	defaultRequestTimeout    = 60 * time.Second
	defaultPresignDefaultTTL = 15 * time.Minute
)

// Validate checks required fields. It does NOT mutate the receiver.
// Call applyDefaults explicitly to populate optional tunables.
func (c *Config) Validate() error {
	if c.Bucket == "" {
		return fmt.Errorf("s3compat: bucket is required")
	}
	if c.Region == "" {
		return fmt.Errorf("s3compat: region is required (use \"auto\" for R2)")
	}
	if _, err := normalizePrefix(c.Prefix); err != nil {
		return fmt.Errorf("s3compat: invalid prefix: %w", err)
	}
	if c.scheme == "r2" && c.Endpoint == "" {
		return fmt.Errorf("s3compat: r2:// requires Endpoint (set BUCKETVCS_S3_ENDPOINT)")
	}
	return nil
}

// applyDefaults populates zero-valued tunables. After this returns, the
// Config is suitable for handing to the SDK.
func (c *Config) applyDefaults() {
	if p, err := normalizePrefix(c.Prefix); err == nil {
		c.Prefix = p
	}
	if c.UploadPartSize == 0 {
		c.UploadPartSize = defaultUploadPartSize
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = defaultMaxRetries
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = defaultRequestTimeout
	}
	if c.PresignDefaultTTL == 0 {
		c.PresignDefaultTTL = defaultPresignDefaultTTL
	}
}
```

- [ ] **Step 5: Implement `url.go`**

```go
package s3compat

import (
	"fmt"
	"strings"
)

// ParseURL parses a "--store" URL of the form:
//
//   s3://<bucket>[/<prefix>]
//   r2://<bucket>[/<prefix>]
//
// It populates a Config seed; the CLI is responsible for layering env
// vars onto the result before calling Validate / Open.
//
// ParseURL deliberately rejects credentials in the URL: the only
// supported credential paths are env vars and the SDK shared-config
// profile.
func ParseURL(raw string) (Config, error) {
	colon := strings.Index(raw, "://")
	if colon <= 0 {
		// Allow the legacy "scheme:path" shape for parity with the
		// existing CLI parser, but reject if scheme is unknown.
		if i := strings.IndexByte(raw, ':'); i > 0 {
			scheme := raw[:i]
			if scheme == "s3" || scheme == "r2" {
				return Config{}, fmt.Errorf("s3compat: %q: bucket required (use %s://<bucket>[/<prefix>])", raw, scheme)
			}
		}
		return Config{}, fmt.Errorf("s3compat: unsupported scheme in %q (want s3:// or r2://)", raw)
	}
	scheme := raw[:colon]
	rest := raw[colon+3:]
	switch scheme {
	case "s3", "r2":
	default:
		return Config{}, fmt.Errorf("s3compat: unsupported scheme %q (want s3:// or r2://)", scheme)
	}
	if rest == "" {
		return Config{}, fmt.Errorf("s3compat: %s://: bucket required", scheme)
	}
	bucket, prefix, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return Config{}, fmt.Errorf("s3compat: %s://: bucket required", scheme)
	}
	cfg := Config{
		scheme: scheme,
		Bucket: bucket,
	}
	if prefix != "" {
		norm, err := normalizePrefix(prefix)
		if err != nil {
			return Config{}, fmt.Errorf("s3compat: %s:// prefix: %w", scheme, err)
		}
		cfg.Prefix = norm
	}
	if scheme == "r2" {
		cfg.ForcePathStyle = true
		cfg.Region = "auto"
	}
	return cfg, nil
}
```

- [ ] **Step 6: Run tests, expect pass**

Run: `go test ./internal/storage/s3compat/...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/storage/s3compat/config.go internal/storage/s3compat/config_test.go \
        internal/storage/s3compat/url.go internal/storage/s3compat/url_test.go
git commit -m "M5 task 5: s3compat Config + ParseURL"
```

---

## Task 6: Open() constructor with httptest mock

**Files:**
- Create: `internal/storage/s3compat/open.go`
- Create: `internal/storage/s3compat/open_test.go`
- Modify: `internal/storage/s3compat/s3compat.go` (delete the skeleton stubs that we're about to replace)

- [ ] **Step 1: Write the failing test**

Create `internal/storage/s3compat/open_test.go`:

```go
package s3compat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// httptestServer returns a server whose handler records the most recent
// request and returns 404 for everything. It is the mock substrate for
// SDK-call assertions across all method tests.
func httptestServer(t *testing.T) (*httptest.Server, *http.Request) {
	t.Helper()
	var lastReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastReq = r
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	_ = lastReq
	return srv, lastReq
}

func TestOpenAppliesPathStyle(t *testing.T) {
	srv, _ := httptestServer(t)
	cfg := Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		Endpoint:        srv.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.client == nil {
		t.Fatalf("client not initialized")
	}
	if s.presign == nil {
		t.Fatalf("presign client not initialized")
	}
	// Ensure a real request goes to the test server (not AWS).
	_, _ = s.Head(context.Background(), "anything")
}

func TestOpenRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"missing bucket", Config{Region: "us-east-1"}, "bucket"},
		{"missing region", Config{Bucket: "b"}, "region"},
		{"r2 without endpoint", Config{Bucket: "b", Region: "auto", scheme: "r2"}, "Endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(context.Background(), tc.cfg)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./internal/storage/s3compat/... -run TestOpen`
Expected: FAIL — `Open` undefined.

- [ ] **Step 3: Implement `open.go`**

```go
package s3compat

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Open builds an S3Compat from cfg. cfg.Validate must succeed; defaults
// are applied here so callers do not need to call applyDefaults
// themselves.
//
// Credential precedence:
//   1. Static (cfg.AccessKeyID + cfg.SecretAccessKey [+ SessionToken])
//   2. Shared-config profile (cfg.Profile)
//   3. SDK default chain (env, instance metadata, ...)
func Open(ctx context.Context, cfg Config) (*S3Compat, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.applyDefaults()

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	switch {
	case cfg.AccessKeyID != "":
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken,
			),
		))
	case cfg.Profile != "":
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3compat: load AWS config: %w", err)
	}
	awsCfg.Retryer = func() aws.Retryer { return newRetryer(cfg.MaxRetries) }

	clientOpts := []func(*s3.Options){
		func(o *s3.Options) { o.UsePathStyle = cfg.ForcePathStyle },
	}
	if cfg.Endpoint != "" {
		ep := cfg.Endpoint
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(ep)
		})
	}

	client := s3.NewFromConfig(awsCfg, clientOpts...)
	return &S3Compat{
		cfg:     cfg,
		client:  client,
		presign: s3.NewPresignClient(client),
	}, nil
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/storage/s3compat/... -run TestOpen`
Expected: PASS. Other method tests still fail with `errNotImpl`; that's expected — they land in later tasks.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/s3compat/open.go internal/storage/s3compat/open_test.go
git commit -m "M5 task 6: s3compat Open() constructor"
```

---

## Task 7: Read methods (Get, Head, GetRange)

**Files:**
- Modify: `internal/storage/s3compat/s3compat.go` (replace Get/Head/GetRange stubs)
- Create: `internal/storage/s3compat/get.go`
- Create: `internal/storage/s3compat/get_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/storage/s3compat/get_test.go`. The mock substrate is a small `mockBackend` we'll evolve task by task; introduce it here.

```go
package s3compat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// mockBackend is a minimal in-memory S3 server. Each test uses one.
// The handler matches request method + URL path against a tiny dispatch
// table the test registers via Set/SetGet/etc. before exercising the
// adapter. It exists ONLY for unit tests; live conformance covers real
// S3/R2 behavior.
type mockBackend struct {
	t       *testing.T
	objects map[string]mockObject // key (incl. bucket prefix) -> obj
}

type mockObject struct {
	body []byte
	etag string
}

func newMockBackend(t *testing.T) (*S3Compat, *mockBackend) {
	t.Helper()
	mb := &mockBackend{t: t, objects: map[string]mockObject{}}
	srv := httptest.NewServer(http.HandlerFunc(mb.serve))
	t.Cleanup(srv.Close)
	cfg := Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		Endpoint:        srv.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, mb
}

func (m *mockBackend) put(key string, body []byte, etag string) {
	m.objects[key] = mockObject{body: body, etag: etag}
}

func (m *mockBackend) keyFromPath(p string) string {
	// Path is "/<bucket>/<key>"; strip the leading "/<bucket>/".
	p = strings.TrimPrefix(p, "/")
	_, key, _ := strings.Cut(p, "/")
	return key
}

func (m *mockBackend) serve(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		key := m.keyFromPath(r.URL.Path)
		obj, ok := m.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", obj.etag)
		// Honor Range if present.
		if rng := r.Header.Get("Range"); rng != "" {
			start, end, ok := parseSimpleRange(rng, len(obj.body))
			if !ok {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.WriteHeader(http.StatusPartialContent)
			if r.Method == http.MethodGet {
				_, _ = w.Write(obj.body[start : end+1])
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(obj.body)
		}
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// parseSimpleRange handles "bytes=start-end" only.
func parseSimpleRange(h string, size int) (start, end int, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(h, prefix) {
		return 0, 0, false
	}
	parts := strings.SplitN(h[len(prefix):], "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	var s, e int
	if _, err := bytes2int(parts[0], &s); err != nil {
		return 0, 0, false
	}
	if _, err := bytes2int(parts[1], &e); err != nil {
		return 0, 0, false
	}
	if e >= size {
		e = size - 1
	}
	if s < 0 || s > e {
		return 0, 0, false
	}
	return s, e, true
}

func bytes2int(s string, dst *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(c-'0')
	}
	*dst = n
	return n, nil
}

func TestGetReturnsBody(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("foo", []byte("hello"), `"abc"`)

	obj, err := s.Get(context.Background(), "foo", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	body, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want %q", body, "hello")
	}
	if obj.Metadata.Version.Token == "" {
		t.Fatalf("Version.Token empty; want ETag")
	}
}

func TestGetNotFound(t *testing.T) {
	s, _ := newMockBackend(t)
	_, err := s.Get(context.Background(), "missing", nil)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestHeadReturnsMetadataOnly(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("foo", []byte("hello"), `"abc"`)
	md, err := s.Head(context.Background(), "foo")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Size != int64(len("hello")) {
		t.Fatalf("Size = %d, want %d", md.Size, len("hello"))
	}
}

func TestGetRangePartial(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("foo", []byte("0123456789"), `"abc"`)

	rc, err := s.GetRange(context.Background(), "foo", 2, 5)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "2345" {
		t.Fatalf("range = %q, want \"2345\"", body)
	}
}

func TestGetRangeRejectsNegative(t *testing.T) {
	s, _ := newMockBackend(t)
	_, err := s.GetRange(context.Background(), "foo", -1, 5)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}

func TestGetWithPrefix(t *testing.T) {
	// Verify that the configured prefix is applied on the wire.
	mb := &mockBackend{t: t, objects: map[string]mockObject{
		"acme/foo": {body: []byte("ok"), etag: `"x"`},
	}}
	srv := httptest.NewServer(http.HandlerFunc(mb.serve))
	t.Cleanup(srv.Close)
	cfg := Config{
		Bucket:          "test-bucket",
		Prefix:          "acme/",
		Region:          "us-east-1",
		Endpoint:        srv.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	obj, err := s.Get(context.Background(), "foo", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	if !bytes.Equal(body, []byte("ok")) {
		t.Fatalf("body = %q, want \"ok\"", body)
	}
}
```

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./internal/storage/s3compat/... -run TestGet -run TestHead`
Expected: FAIL — methods still return `errNotImpl`.

- [ ] **Step 3: Implement `get.go`**

```go
package s3compat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func (s *S3Compat) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	in := &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
	}
	if opts != nil && opts.IfVersionMatches != nil {
		in.IfMatch = aws.String(opts.IfVersionMatches.Token)
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		return nil, classify(opGet, err)
	}
	md := storage.ObjectMetadata{
		Key: key,
		Version: storage.ObjectVersion{
			Provider: "s3compat",
			Token:    aws.ToString(out.ETag),
			Kind:     "etag",
		},
		Size: aws.ToInt64(out.ContentLength),
	}
	if out.ContentType != nil {
		md.ContentType = *out.ContentType
	}
	if out.LastModified != nil {
		md.ModifiedAt = *out.LastModified
	} else {
		md.ModifiedAt = time.Time{}
	}
	return &storage.Object{Body: out.Body, Metadata: md}, nil
}

func (s *S3Compat) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
	})
	if err != nil {
		return nil, classify(opHead, err)
	}
	md := &storage.ObjectMetadata{
		Key: key,
		Version: storage.ObjectVersion{
			Provider: "s3compat",
			Token:    aws.ToString(out.ETag),
			Kind:     "etag",
		},
		Size: aws.ToInt64(out.ContentLength),
	}
	if out.ContentType != nil {
		md.ContentType = *out.ContentType
	}
	if out.LastModified != nil {
		md.ModifiedAt = *out.LastModified
	}
	return md, nil
}

func (s *S3Compat) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	if start < 0 || endInclusive < 0 || endInclusive < start {
		return nil, fmt.Errorf("%w: invalid range [%d, %d]", storage.ErrInvalidArgument, start, endInclusive)
	}
	rangeHeader := fmt.Sprintf("bytes=%d-%d", start, endInclusive)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		return nil, classify(opGetRange, err)
	}
	return out.Body, nil
}

// Sanity assertion to keep get.go honest if storage.GetOptions evolves.
var _ = errors.New
```

- [ ] **Step 4: Remove the corresponding skeleton stubs in `s3compat.go`**

Edit `internal/storage/s3compat/s3compat.go` and delete the `Get`, `Head`, and `GetRange` skeleton methods (the `errNotImpl` versions). Keep the unimplemented ones for the methods later tasks land.

- [ ] **Step 5: Run tests, expect pass**

Run: `go test ./internal/storage/s3compat/... -run "TestGet|TestHead"`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/storage/s3compat/get.go internal/storage/s3compat/get_test.go internal/storage/s3compat/s3compat.go
git commit -m "M5 task 7: s3compat read methods (Get, Head, GetRange)"
```

---

## Task 8: PutIfAbsent and PutIfVersionMatches

**Files:**
- Modify: `internal/storage/s3compat/s3compat.go` (drop Put* stubs)
- Create: `internal/storage/s3compat/put.go`
- Create: `internal/storage/s3compat/put_test.go`

- [ ] **Step 1: Extend the mock backend to support conditional PUT**

Edit `internal/storage/s3compat/get_test.go` `mockBackend.serve` to also handle PUT. Add a new helper file `internal/storage/s3compat/mock_test.go` to share between tests:

```go
package s3compat

import (
	"io"
	"net/http"
	"strconv"
)

// servePut handles PUT with optional If-None-Match: * and If-Match: <etag>
// semantics that mirror the production adapter's CAS expectations.
func (m *mockBackend) servePut(w http.ResponseWriter, r *http.Request) {
	key := m.keyFromPath(r.URL.Path)
	body, _ := io.ReadAll(r.Body)
	existing, exists := m.objects[key]

	if r.Header.Get("If-None-Match") == "*" && exists {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if im := r.Header.Get("If-Match"); im != "" {
		if !exists || existing.etag != im {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}

	etag := `"v` + strconv.Itoa(len(m.objects)+1) + `"`
	m.objects[key] = mockObject{body: body, etag: etag}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}
```

Then update the dispatch in `mockBackend.serve` (in `get_test.go`):

```go
case http.MethodPut:
    m.servePut(w, r)
```

- [ ] **Step 2: Write the failing tests**

Create `internal/storage/s3compat/put_test.go`:

```go
package s3compat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestPutIfAbsentNew(t *testing.T) {
	s, _ := newMockBackend(t)
	v, err := s.PutIfAbsent(context.Background(), "k", strings.NewReader("hi"), nil)
	if err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}
	if v.Token == "" {
		t.Fatalf("returned ObjectVersion has empty Token")
	}
}

func TestPutIfAbsentConflict(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("existing"), `"v0"`)

	_, err := s.PutIfAbsent(context.Background(), "k", strings.NewReader("new"), nil)
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestPutIfVersionMatchesSuccess(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("v0"), `"v0"`)
	expected := storage.ObjectVersion{Token: `"v0"`, Kind: "etag"}

	v, err := s.PutIfVersionMatches(context.Background(), "k", expected, bytes.NewReader([]byte("v1")), nil)
	if err != nil {
		t.Fatalf("PutIfVersionMatches: %v", err)
	}
	if v.Token == expected.Token {
		t.Fatalf("returned token unchanged; expected new etag")
	}
}

func TestPutIfVersionMatchesMismatch(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("v0"), `"v0"`)
	expected := storage.ObjectVersion{Token: `"WRONG"`, Kind: "etag"}

	_, err := s.PutIfVersionMatches(context.Background(), "k", expected, bytes.NewReader([]byte("nope")), nil)
	if !errors.Is(err, storage.ErrVersionMismatch) {
		t.Fatalf("err = %v, want ErrVersionMismatch", err)
	}
}
```

- [ ] **Step 3: Run tests, expect failure**

Run: `go test ./internal/storage/s3compat/... -run TestPut`
Expected: FAIL — `PutIfAbsent` / `PutIfVersionMatches` still stubs.

- [ ] **Step 4: Implement `put.go`**

```go
package s3compat

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func (s *S3Compat) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	seekable, err := materializeForRetry(body, s.cfg.UploadPartSize)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	in := &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.Bucket),
		Key:         aws.String(applyPrefix(s.cfg.Prefix, key)),
		Body:        seekable,
		IfNoneMatch: aws.String("*"),
	}
	if opts != nil && opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		return storage.ObjectVersion{}, classify(opPutIfAbsent, err)
	}
	return storage.ObjectVersion{
		Provider: "s3compat",
		Token:    aws.ToString(out.ETag),
		Kind:     "etag",
	}, nil
}

func (s *S3Compat) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	seekable, err := materializeForRetry(body, s.cfg.UploadPartSize)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	in := &s3.PutObjectInput{
		Bucket:  aws.String(s.cfg.Bucket),
		Key:     aws.String(applyPrefix(s.cfg.Prefix, key)),
		Body:    seekable,
		IfMatch: aws.String(expected.Token),
	}
	if opts != nil && opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		return storage.ObjectVersion{}, classify(opPutIfMatch, err)
	}
	return storage.ObjectVersion{
		Provider: "s3compat",
		Token:    aws.ToString(out.ETag),
		Kind:     "etag",
	}, nil
}

// materializeForRetry returns a Reader that the SDK can rewind for
// retries. Bodies <= maxBuffer are buffered into memory; larger bodies
// must already be seekable (io.ReadSeeker) — we surface a clear error
// otherwise.
func materializeForRetry(body io.Reader, maxBuffer int64) (io.Reader, error) {
	if rs, ok := body.(io.ReadSeeker); ok {
		return rs, nil
	}
	limited := io.LimitReader(body, maxBuffer+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("s3compat: read body: %w", err)
	}
	if int64(len(buf)) > maxBuffer {
		return nil, fmt.Errorf("s3compat: non-seekable body exceeds %d-byte buffer; use multipart for larger uploads", maxBuffer)
	}
	return bytes.NewReader(buf), nil
}
```

- [ ] **Step 5: Drop the corresponding stubs in `s3compat.go`**

Remove the `PutIfAbsent` and `PutIfVersionMatches` skeleton methods.

- [ ] **Step 6: Run tests, expect pass**

Run: `go test ./internal/storage/s3compat/... -run TestPut`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/storage/s3compat/put.go internal/storage/s3compat/put_test.go \
        internal/storage/s3compat/mock_test.go internal/storage/s3compat/get_test.go \
        internal/storage/s3compat/s3compat.go
git commit -m "M5 task 8: s3compat conditional PutIfAbsent and PutIfVersionMatches"
```

---

## Task 9: DeleteIfVersionMatches

**Files:**
- Modify: `internal/storage/s3compat/s3compat.go` (drop Delete stub)
- Create: `internal/storage/s3compat/delete.go`
- Create: `internal/storage/s3compat/delete_test.go`

- [ ] **Step 1: Extend the mock backend to support DELETE**

Add to `mockBackend.serve` dispatch:

```go
case http.MethodDelete:
    m.serveDelete(w, r)
```

Add to `mock_test.go`:

```go
func (m *mockBackend) serveDelete(w http.ResponseWriter, r *http.Request) {
	key := m.keyFromPath(r.URL.Path)
	existing, exists := m.objects[key]
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if im := r.Header.Get("If-Match"); im != "" && existing.etag != im {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	delete(m.objects, key)
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/storage/s3compat/delete_test.go`:

```go
package s3compat

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestDeleteIfVersionMatchesSuccess(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("v"), `"v0"`)
	if err := s.DeleteIfVersionMatches(context.Background(), "k", storage.ObjectVersion{Token: `"v0"`}); err != nil {
		t.Fatalf("DeleteIfVersionMatches: %v", err)
	}
	if _, ok := mb.objects["k"]; ok {
		t.Fatalf("object should be deleted")
	}
}

func TestDeleteIfVersionMatchesMismatch(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("v"), `"v0"`)
	err := s.DeleteIfVersionMatches(context.Background(), "k", storage.ObjectVersion{Token: `"WRONG"`})
	if !errors.Is(err, storage.ErrVersionMismatch) {
		t.Fatalf("err = %v, want ErrVersionMismatch", err)
	}
}

func TestDeleteIfVersionMatchesAbsent(t *testing.T) {
	s, _ := newMockBackend(t)
	err := s.DeleteIfVersionMatches(context.Background(), "missing", storage.ObjectVersion{Token: `"v0"`})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 3: Run tests, expect failure**

Run: `go test ./internal/storage/s3compat/... -run TestDelete`
Expected: FAIL.

- [ ] **Step 4: Implement `delete.go`**

```go
package s3compat

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func (s *S3Compat) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:  aws.String(s.cfg.Bucket),
		Key:     aws.String(applyPrefix(s.cfg.Prefix, key)),
		IfMatch: aws.String(expected.Token),
	})
	if err != nil {
		return classify(opDeleteIfMatch, err)
	}
	return nil
}
```

- [ ] **Step 5: Drop the stub in `s3compat.go`**

- [ ] **Step 6: Run tests, expect pass**

Run: `go test ./internal/storage/s3compat/... -run TestDelete`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/storage/s3compat/delete.go internal/storage/s3compat/delete_test.go \
        internal/storage/s3compat/mock_test.go internal/storage/s3compat/get_test.go \
        internal/storage/s3compat/s3compat.go
git commit -m "M5 task 9: s3compat DeleteIfVersionMatches"
```

---

## Task 10: List with prefix translation and delimiter

**Files:**
- Modify: `internal/storage/s3compat/s3compat.go` (drop List stub)
- Create: `internal/storage/s3compat/list.go`
- Create: `internal/storage/s3compat/list_test.go`

- [ ] **Step 1: Extend the mock backend to support ListObjectsV2**

Add to `mockBackend.serve` (handles GET on bucket-level URL with `list-type=2`):

```go
// In serve(), at the top of the GET branch:
if r.URL.Query().Get("list-type") == "2" {
    m.serveList(w, r)
    return
}
```

Add to `mock_test.go`:

```go
import (
	"sort"
	"strconv"
)

func (m *mockBackend) serveList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	startAfter := q.Get("continuation-token")

	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	type entry struct {
		Key   string
		ETag  string
		Size  int
	}
	var contents []entry
	prefixes := map[string]struct{}{}

	for _, k := range keys {
		if startAfter != "" && k <= startAfter {
			continue
		}
		rem := strings.TrimPrefix(k, prefix)
		if delimiter != "" {
			if i := strings.Index(rem, delimiter); i >= 0 {
				prefixes[prefix+rem[:i+len(delimiter)]] = struct{}{}
				continue
			}
		}
		contents = append(contents, entry{Key: k, ETag: m.objects[k].etag, Size: len(m.objects[k].body)})
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>`))
	w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated>`))
	for _, c := range contents {
		w.Write([]byte(`<Contents><Key>` + c.Key + `</Key><ETag>` + c.ETag + `</ETag><Size>` + strconv.Itoa(c.Size) + `</Size></Contents>`))
	}
	for p := range prefixes {
		w.Write([]byte(`<CommonPrefixes><Prefix>` + p + `</Prefix></CommonPrefixes>`))
	}
	w.Write([]byte(`</ListBucketResult>`))
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/storage/s3compat/list_test.go`:

```go
package s3compat

import (
	"context"
	"sort"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestListEmpty(t *testing.T) {
	s, _ := newMockBackend(t)
	page, err := s.List(context.Background(), "anything", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != 0 {
		t.Fatalf("len(Objects) = %d, want 0", len(page.Objects))
	}
}

func TestListReturnsKeys(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("a/x", []byte("x"), `"e1"`)
	mb.put("a/y", []byte("y"), `"e2"`)
	mb.put("b/z", []byte("z"), `"e3"`)

	page, err := s.List(context.Background(), "a/", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, len(page.Objects))
	for i, o := range page.Objects {
		got[i] = o.Key
	}
	sort.Strings(got)
	want := []string{"a/x", "a/y"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestListWithDelimiter(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("d/a/1", []byte(""), `"e"`)
	mb.put("d/a/2", []byte(""), `"e"`)
	mb.put("d/b/1", []byte(""), `"e"`)
	mb.put("d/c", []byte(""), `"e"`)

	page, err := s.List(context.Background(), "d/", &storage.ListOptions{Delimiter: "/"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != 1 || page.Objects[0].Key != "d/c" {
		t.Fatalf("Objects = %v, want exactly d/c", page.Objects)
	}
	cps := append([]string(nil), page.CommonPrefixes...)
	sort.Strings(cps)
	want := []string{"d/a/", "d/b/"}
	if len(cps) != 2 || cps[0] != want[0] || cps[1] != want[1] {
		t.Fatalf("CommonPrefixes = %v, want %v", cps, want)
	}
}

func TestListStripsAdapterPrefix(t *testing.T) {
	// Same setup as TestGetWithPrefix but for List.
	mb := &mockBackend{t: t, objects: map[string]mockObject{
		"acme/foo": {body: nil, etag: `"e"`},
		"acme/bar": {body: nil, etag: `"e"`},
	}}
	srv := httptestNewMockServer(t, mb)
	cfg := Config{
		Bucket:          "test-bucket",
		Prefix:          "acme/",
		Region:          "us-east-1",
		Endpoint:        srv.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	page, err := s.List(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(page.Objects))
	for i, o := range page.Objects {
		got[i] = o.Key
	}
	sort.Strings(got)
	want := []string{"bar", "foo"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("keys = %v, want %v (adapter prefix should be stripped)", got, want)
	}
}
```

Add helper to `mock_test.go`:

```go
import "net/http/httptest"

func httptestNewMockServer(t *testing.T, mb *mockBackend) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(mb.serve))
	t.Cleanup(srv.Close)
	return srv
}
```

- [ ] **Step 3: Run tests, expect failure**

Run: `go test ./internal/storage/s3compat/... -run TestList`
Expected: FAIL.

- [ ] **Step 4: Implement `list.go`**

```go
package s3compat

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func (s *S3Compat) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	in := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(applyPrefix(s.cfg.Prefix, prefix)),
	}
	if opts != nil {
		if opts.MaxKeys > 0 {
			in.MaxKeys = aws.Int32(int32(opts.MaxKeys))
		}
		if opts.ContinuationToken != "" {
			in.ContinuationToken = aws.String(opts.ContinuationToken)
		}
		if opts.Delimiter != "" {
			in.Delimiter = aws.String(opts.Delimiter)
		}
	}
	out, err := s.client.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, classify(opList, err)
	}
	page := &storage.ListPage{}
	for _, obj := range out.Contents {
		stored := aws.ToString(obj.Key)
		k, err := stripPrefix(s.cfg.Prefix, stored)
		if err != nil {
			return nil, err
		}
		page.Objects = append(page.Objects, storage.ObjectMetadata{
			Key: k,
			Version: storage.ObjectVersion{
				Provider: "s3compat",
				Token:    aws.ToString(obj.ETag),
				Kind:     "etag",
			},
			Size: aws.ToInt64(obj.Size),
		})
	}
	for _, cp := range out.CommonPrefixes {
		stored := aws.ToString(cp.Prefix)
		p, err := stripPrefix(s.cfg.Prefix, stored)
		if err != nil {
			return nil, err
		}
		page.CommonPrefixes = append(page.CommonPrefixes, p)
	}
	if out.IsTruncated != nil && *out.IsTruncated {
		page.NextToken = aws.ToString(out.NextContinuationToken)
	}
	return page, nil
}
```

- [ ] **Step 5: Drop the stub in `s3compat.go`**

- [ ] **Step 6: Run tests, expect pass**

Run: `go test ./internal/storage/s3compat/... -run TestList`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/storage/s3compat/list.go internal/storage/s3compat/list_test.go \
        internal/storage/s3compat/mock_test.go internal/storage/s3compat/get_test.go \
        internal/storage/s3compat/s3compat.go
git commit -m "M5 task 10: s3compat List with prefix translation and delimiter"
```

---

## Task 11: Multipart upload (Create + UploadPart + Abort + CompleteIfAbsent)

**Files:**
- Modify: `internal/storage/s3compat/s3compat.go` (drop multipart stubs)
- Create: `internal/storage/s3compat/multipart.go`
- Create: `internal/storage/s3compat/multipart_test.go`

- [ ] **Step 1: Extend the mock backend to support multipart endpoints**

Add to `mock_test.go` (and dispatch from `serve`):

```go
import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

type mockUpload struct {
	id    string
	key   string
	parts map[int][]byte
}

// Embed in mockBackend:
//   uploads map[string]*mockUpload   // upload-id -> upload
// Add to newMockBackend init: mb.uploads = map[string]*mockUpload{}

func (m *mockBackend) serveMultipart(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := randID()
		key := m.keyFromPath(r.URL.Path)
		m.uploads[id] = &mockUpload{id: id, key: key, parts: map[int][]byte{}}
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(`<?xml version="1.0"?><InitiateMultipartUploadResult><UploadId>` + id + `</UploadId></InitiateMultipartUploadResult>`))
		return true
	case r.Method == http.MethodPut && q.Has("uploadId") && q.Has("partNumber"):
		id := q.Get("uploadId")
		pn := atoi(q.Get("partNumber"))
		body, _ := io.ReadAll(r.Body)
		m.uploads[id].parts[pn] = body
		etag := `"p` + q.Get("partNumber") + `-` + randID()[:6] + `"`
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		return true
	case r.Method == http.MethodPost && q.Has("uploadId"):
		id := q.Get("uploadId")
		up := m.uploads[id]
		// If-None-Match: * -> AlreadyExists
		if r.Header.Get("If-None-Match") == "*" {
			if _, exists := m.objects[up.key]; exists {
				w.WriteHeader(http.StatusPreconditionFailed)
				return true
			}
		}
		// Reassemble: simple concat in part-number order.
		var buf []byte
		for i := 1; ; i++ {
			b, ok := up.parts[i]
			if !ok {
				break
			}
			buf = append(buf, b...)
		}
		etag := `"complete-` + randID()[:6] + `"`
		m.objects[up.key] = mockObject{body: buf, etag: etag}
		delete(m.uploads, id)
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(`<?xml version="1.0"?><CompleteMultipartUploadResult><ETag>` + etag + `</ETag></CompleteMultipartUploadResult>`))
		return true
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(m.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func randID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// Avoid unused import warning if strings is not otherwise used here.
var _ = strings.Split
```

Then update `mockBackend.serve` to call `serveMultipart` first for any request with `uploads`/`uploadId`/`partNumber` query params.

Update `newMockBackend` to initialize `uploads`.

- [ ] **Step 2: Write the failing tests**

Create `internal/storage/s3compat/multipart_test.go`:

```go
package s3compat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestMultipartRoundtrip(t *testing.T) {
	s, mb := newMockBackend(t)

	up, err := s.CreateMultipart(context.Background(), "big.bin", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if up.UploadID() == "" {
		t.Fatalf("UploadID empty")
	}
	if up.Key() != "big.bin" {
		t.Fatalf("Key = %q, want big.bin", up.Key())
	}

	p1, err := up.UploadPart(context.Background(), 1, strings.NewReader("hello "))
	if err != nil {
		t.Fatalf("UploadPart 1: %v", err)
	}
	p2, err := up.UploadPart(context.Background(), 2, strings.NewReader("world"))
	if err != nil {
		t.Fatalf("UploadPart 2: %v", err)
	}

	v, err := s.CompleteMultipartIfAbsent(context.Background(), up, []storage.MultipartPart{p1, p2})
	if err != nil {
		t.Fatalf("CompleteMultipartIfAbsent: %v", err)
	}
	if v.Token == "" {
		t.Fatalf("Token empty")
	}
	if !bytes.Equal(mb.objects["big.bin"].body, []byte("hello world")) {
		t.Fatalf("assembled body = %q, want \"hello world\"", mb.objects["big.bin"].body)
	}
}

func TestCompleteMultipartIfAbsentConflict(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("existing"), `"e0"`)

	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1, err := up.UploadPart(context.Background(), 1, strings.NewReader("new"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CompleteMultipartIfAbsent(context.Background(), up, []storage.MultipartPart{p1})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestMultipartAbort(t *testing.T) {
	s, mb := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := up.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, ok := mb.uploads[up.UploadID()]; ok {
		t.Fatalf("upload still present after abort")
	}
}
```

- [ ] **Step 3: Run tests, expect failure**

Run: `go test ./internal/storage/s3compat/... -run TestMultipart -run TestCompleteMultipart`
Expected: FAIL.

- [ ] **Step 4: Implement `multipart.go`**

```go
package s3compat

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// upload is the S3-backed MultipartUpload. It holds a back-pointer to
// the parent S3Compat so UploadPart and Abort can issue requests.
type upload struct {
	parent   *S3Compat
	uploadID string
	key      string // logical (caller-visible) key
	storeKey string // adapter-prefixed key on the wire
}

var _ storage.MultipartUpload = (*upload)(nil)

func (u *upload) UploadID() string { return u.uploadID }
func (u *upload) Key() string      { return u.key }

func (u *upload) UploadPart(ctx context.Context, partNumber int, body io.Reader) (storage.MultipartPart, error) {
	seekable, err := materializeForRetry(body, u.parent.cfg.UploadPartSize)
	if err != nil {
		return storage.MultipartPart{}, err
	}
	out, err := u.parent.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(u.parent.cfg.Bucket),
		Key:        aws.String(u.storeKey),
		UploadId:   aws.String(u.uploadID),
		PartNumber: aws.Int32(int32(partNumber)),
		Body:       seekable,
	})
	if err != nil {
		return storage.MultipartPart{}, classify(opUploadPart, err)
	}
	size := bodyKnownSize(seekable)
	return storage.MultipartPart{
		PartNumber: partNumber,
		Token:      aws.ToString(out.ETag),
		Size:       size,
	}, nil
}

func (u *upload) Abort(ctx context.Context) error {
	_, err := u.parent.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(u.parent.cfg.Bucket),
		Key:      aws.String(u.storeKey),
		UploadId: aws.String(u.uploadID),
	})
	if err != nil {
		return classify(opAbortMultipart, err)
	}
	return nil
}

func (s *S3Compat) CreateMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	in := &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
	}
	if opts != nil && opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	out, err := s.client.CreateMultipartUpload(ctx, in)
	if err != nil {
		return nil, classify(opCreateMultipart, err)
	}
	return &upload{
		parent:   s,
		uploadID: aws.ToString(out.UploadId),
		key:      key,
		storeKey: applyPrefix(s.cfg.Prefix, key),
	}, nil
}

func (s *S3Compat) CompleteMultipartIfAbsent(ctx context.Context, mu storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	u, ok := mu.(*upload)
	if !ok {
		return storage.ObjectVersion{}, fmt.Errorf("%w: CompleteMultipartIfAbsent: upload is %T, want *s3compat.upload", storage.ErrInvalidArgument, mu)
	}
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		completed = append(completed, types.CompletedPart{
			ETag:       aws.String(p.Token),
			PartNumber: aws.Int32(int32(p.PartNumber)),
		})
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(u.parent.cfg.Bucket),
		Key:             aws.String(u.storeKey),
		UploadId:        aws.String(u.uploadID),
		IfNoneMatch:     aws.String("*"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return storage.ObjectVersion{}, classify(opCompleteIfAbsent, err)
	}
	return storage.ObjectVersion{
		Provider: "s3compat",
		Token:    aws.ToString(out.ETag),
		Kind:     "etag",
	}, nil
}

// bodyKnownSize attempts to determine the byte length of an
// already-seekable body. Returns 0 if unknown.
func bodyKnownSize(r io.Reader) int64 {
	switch v := r.(type) {
	case *bytes.Reader:
		return int64(v.Len())
	case *bytes.Buffer:
		return int64(v.Len())
	}
	return 0
}
```

- [ ] **Step 5: Drop the stubs in `s3compat.go`**

- [ ] **Step 6: Run tests, expect pass**

Run: `go test ./internal/storage/s3compat/...`
Expected: PASS for all multipart tests; SignedGetURL still returns `errNotImpl`.

- [ ] **Step 7: Commit**

```bash
git add internal/storage/s3compat/multipart.go internal/storage/s3compat/multipart_test.go \
        internal/storage/s3compat/mock_test.go internal/storage/s3compat/get_test.go \
        internal/storage/s3compat/s3compat.go
git commit -m "M5 task 11: s3compat multipart upload"
```

---

## Task 12: SignedGetURL

**Files:**
- Modify: `internal/storage/s3compat/s3compat.go` (drop SignedGetURL stub)
- Create: `internal/storage/s3compat/signed.go`
- Create: `internal/storage/s3compat/signed_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/storage/s3compat/signed_test.go`:

```go
package s3compat

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestSignedGetURLForm(t *testing.T) {
	s, _ := newMockBackend(t)
	got, err := s.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{
		Expires: 5 * time.Minute,
		Method:  "GET",
	})
	if err != nil {
		t.Fatalf("SignedGetURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("URL parse: %v", err)
	}
	if !strings.HasSuffix(u.Path, "/k") {
		t.Fatalf("URL path = %q, want suffix /k", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Signature") == "" {
		t.Fatalf("expected X-Amz-Signature in presigned URL")
	}
	if q.Get("X-Amz-Expires") == "" {
		t.Fatalf("expected X-Amz-Expires in presigned URL")
	}
}

func TestSignedGetURLDefaultsTTL(t *testing.T) {
	s, _ := newMockBackend(t)
	got, err := s.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{Method: "GET"})
	if err != nil {
		t.Fatalf("SignedGetURL: %v", err)
	}
	u, _ := url.Parse(got)
	exp := u.Query().Get("X-Amz-Expires")
	if exp == "" || exp == "0" {
		t.Fatalf("default TTL not applied; X-Amz-Expires = %q", exp)
	}
}
```

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./internal/storage/s3compat/... -run TestSigned`
Expected: FAIL.

- [ ] **Step 3: Implement `signed.go`**

```go
package s3compat

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func (s *S3Compat) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	ttl := opts.Expires
	if ttl <= 0 {
		ttl = s.cfg.PresignDefaultTTL
	}
	out, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
	}, func(po *s3.PresignOptions) {
		po.Expires = ttl
	})
	if err != nil {
		return "", classify(opGet, err)
	}
	return out.URL, nil
}
```

- [ ] **Step 4: Drop the stub in `s3compat.go`**

After this step, `s3compat.go` should contain only the type, the `Capabilities` method, and the interface assertion.

- [ ] **Step 5: Run tests, expect pass**

Run: `go test ./internal/storage/s3compat/...`
Expected: PASS for all in-tree tests.

- [ ] **Step 6: Commit**

```bash
git add internal/storage/s3compat/signed.go internal/storage/s3compat/signed_test.go internal/storage/s3compat/s3compat.go
git commit -m "M5 task 12: s3compat SignedGetURL"
```

---

## Task 13: Live conformance tests (skip-when-unset)

**Files:**
- Create: `internal/storage/s3compat/s3compat_conformance_test.go`

- [ ] **Step 1: Write the conformance harness**

Create `internal/storage/s3compat/s3compat_conformance_test.go`:

```go
package s3compat_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/conformance"
	"github.com/bucketvcs/bucketvcs/internal/storage/s3compat"
)

// envConfigs returns the CLI -> Config mapping used by both providers.
// Each test prefix is unique per Factory call so that the suite's
// many fresh-store calls do not collide.
func makeFactory(t *testing.T, base s3compat.Config) conformance.Factory {
	t.Helper()
	if err := base.Validate(); err != nil {
		t.Fatalf("base config invalid: %v", err)
	}
	return func(tb testing.TB) (storage.ObjectStore, func()) {
		tb.Helper()
		cfg := base
		cfg.Prefix = fmt.Sprintf("conformance/%s/", uuid.New().String())

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s, err := s3compat.Open(ctx, cfg)
		if err != nil {
			tb.Fatalf("s3compat.Open: %v", err)
		}
		cleanup := func() {
			cleanupCtx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer ccancel()
			cleanupPrefix(tb, s, cleanupCtx)
		}
		return s, cleanup
	}
}

func cleanupPrefix(tb testing.TB, s *s3compat.S3Compat, ctx context.Context) {
	tb.Helper()
	for {
		page, err := s.List(ctx, "", nil)
		if err != nil {
			tb.Logf("conformance cleanup: list error: %v", err)
			return
		}
		if len(page.Objects) == 0 && page.NextToken == "" {
			return
		}
		for _, o := range page.Objects {
			if err := s.DeleteIfVersionMatches(ctx, o.Key, o.Version); err != nil {
				tb.Logf("conformance cleanup: delete %q: %v", o.Key, err)
			}
		}
		if page.NextToken == "" {
			break
		}
	}
}

func TestConformance_R2(t *testing.T) {
	bucket := os.Getenv("BUCKETVCS_R2_BUCKET")
	endpoint := os.Getenv("BUCKETVCS_R2_ENDPOINT")
	if bucket == "" || endpoint == "" {
		t.Skip("R2 conformance: set BUCKETVCS_R2_BUCKET, BUCKETVCS_R2_ENDPOINT, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY")
	}
	cfg := s3compat.Config{
		Bucket:          bucket,
		Region:          envOr("BUCKETVCS_R2_REGION", "auto"),
		Endpoint:        endpoint,
		ForcePathStyle:  true,
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	conformance.Run(t, makeFactory(t, cfg))
}

func TestConformance_S3(t *testing.T) {
	bucket := os.Getenv("BUCKETVCS_S3_BUCKET")
	region := os.Getenv("BUCKETVCS_S3_REGION")
	if bucket == "" || region == "" {
		t.Skip("S3 conformance: set BUCKETVCS_S3_BUCKET, BUCKETVCS_S3_REGION, AWS credentials")
	}
	cfg := s3compat.Config{
		Bucket:          bucket,
		Region:          region,
		Endpoint:        os.Getenv("BUCKETVCS_S3_ENDPOINT"), // optional
		ForcePathStyle:  os.Getenv("BUCKETVCS_S3_FORCE_PATH_STYLE") == "true",
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	conformance.Run(t, makeFactory(t, cfg))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

(Note: `Config.Validate` must not currently be exported in a way that exposes `scheme`. Confirm `Config` fields used here are public.)

- [ ] **Step 2: Run with no env to confirm skip**

Run: `go test ./internal/storage/s3compat/... -run TestConformance`
Expected: SKIP for both with the env-vars hint message.

- [ ] **Step 3: (Optional, only if developer has R2 creds) Run R2 live**

Run:

```bash
BUCKETVCS_R2_BUCKET=<bucket> \
BUCKETVCS_R2_ENDPOINT=https://<account>.r2.cloudflarestorage.com \
AWS_ACCESS_KEY_ID=<key> \
AWS_SECRET_ACCESS_KEY=<secret> \
go test ./internal/storage/s3compat/... -run TestConformance_R2 -v
```

Expected: full conformance suite passes against R2.

If any test fails, do NOT mark this task complete — fix the adapter (add a regression test in the relevant prior task's test file, then re-run live conformance until pass).

- [ ] **Step 4: Add `github.com/google/uuid` to go.mod (it is already an existing dep, check go.mod)**

Run: `grep "google/uuid" go.mod`
Expected: it's already there from M0/M1. If not, `go get github.com/google/uuid@latest`.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/s3compat/s3compat_conformance_test.go
git commit -m "M5 task 13: s3compat live conformance harness (R2 + S3)"
```

---

## Task 14: CLI scheme wiring (s3:// and r2://)

**Files:**
- Modify: `cmd/bucketvcs/store.go`
- Modify: `cmd/bucketvcs/store_test.go`

- [ ] **Step 1: Write the failing tests for scheme wiring**

Append to `cmd/bucketvcs/store_test.go`:

```go
func TestParseStoreURL_S3(t *testing.T) {
	scheme, path, err := parseStoreURL("s3://my-bucket/data")
	if err != nil {
		t.Fatalf("parseStoreURL: %v", err)
	}
	if scheme != "s3" {
		t.Fatalf("scheme = %q, want s3", scheme)
	}
	if path != "my-bucket/data" {
		t.Fatalf("path = %q, want my-bucket/data", path)
	}
}

func TestParseStoreURL_R2(t *testing.T) {
	scheme, path, err := parseStoreURL("r2://my-bucket")
	if err != nil {
		t.Fatalf("parseStoreURL: %v", err)
	}
	if scheme != "r2" {
		t.Fatalf("scheme = %q, want r2", scheme)
	}
	if path != "my-bucket" {
		t.Fatalf("path = %q, want my-bucket", path)
	}
}

func TestParseStoreURL_RejectsCloudReservations(t *testing.T) {
	for _, scheme := range []string{"gcs", "azureblob"} {
		_, _, err := parseStoreURL(scheme + "://x")
		if err == nil {
			t.Fatalf("%s:// must still be reserved", scheme)
		}
		if !strings.Contains(err.Error(), "M7") {
			t.Fatalf("%s:// error %q does not mention M7", scheme, err.Error())
		}
	}
}
```

(Add `import "strings"` if not present.)

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./cmd/bucketvcs/... -run TestParseStoreURL`
Expected: FAIL — `s3` and `r2` still rejected by the existing parser.

- [ ] **Step 3: Update `cmd/bucketvcs/store.go`**

Replace the file with:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
	"github.com/bucketvcs/bucketvcs/internal/storage/s3compat"
)

// parseStoreURL parses a --store value into (scheme, scheme-specific
// remainder). M5 supports localfs:, s3:, and r2:; other cloud schemes
// are reserved.
func parseStoreURL(s string) (scheme, path string, err error) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 {
		return "", "", fmt.Errorf(`--store: missing scheme; want "localfs:<path>", "s3://<bucket>[/<prefix>]", or "r2://<bucket>[/<prefix>]"`)
	}
	scheme = s[:colon]
	rest := s[colon+1:]
	switch scheme {
	case "localfs":
		if rest == "" {
			return "", "", fmt.Errorf(`--store: %q scheme requires a non-empty path (got %q)`, scheme, s)
		}
		return scheme, rest, nil
	case "s3", "r2":
		// rest should be "//bucket[/prefix]"
		if !strings.HasPrefix(rest, "//") {
			return "", "", fmt.Errorf(`--store: %s URL must use the form %s://<bucket>[/<prefix>] (got %q)`, scheme, scheme, s)
		}
		bucketPath := strings.TrimPrefix(rest, "//")
		if bucketPath == "" {
			return "", "", fmt.Errorf(`--store: %s:// requires a bucket name`, scheme)
		}
		return scheme, bucketPath, nil
	case "gcs", "azureblob":
		return "", "", fmt.Errorf(`--store: scheme %q is reserved; cloud adapter for this provider lands at M7`, scheme)
	default:
		return "", "", fmt.Errorf(`--store: unknown scheme %q; want "localfs:<path>", "s3://<bucket>[/<prefix>]", or "r2://<bucket>[/<prefix>]"`, scheme)
	}
}

// openStore parses the --store URL and returns a constructed
// ObjectStore. Caller is responsible for releasing it via closeStore on
// shutdown.
func openStore(url string) (storage.ObjectStore, error) {
	scheme, path, err := parseStoreURL(url)
	if err != nil {
		return nil, err
	}
	switch scheme {
	case "localfs":
		s, err := localfs.Open(path)
		if err != nil {
			return nil, fmt.Errorf("localfs: %w", err)
		}
		return s, nil
	case "s3", "r2":
		cfg, err := s3compat.ParseURL(url)
		if err != nil {
			return nil, err
		}
		applyEnvToConfig(&cfg, scheme)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s, err := s3compat.Open(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("s3compat: %w", err)
		}
		return s, nil
	default:
		return nil, fmt.Errorf("unreachable: scheme %q passed parseStoreURL but openStore has no constructor", scheme)
	}
}

// applyEnvToConfig layers env vars onto a Config seed produced by
// ParseURL. Standard AWS_* env vars are honored by the SDK default
// chain when AccessKeyID is left empty.
func applyEnvToConfig(cfg *s3compat.Config, scheme string) {
	if v := os.Getenv("BUCKETVCS_S3_REGION"); v != "" {
		cfg.Region = v
	} else if v := os.Getenv("AWS_REGION"); v != "" && cfg.Region == "" {
		cfg.Region = v
	}
	if cfg.Region == "" && scheme == "s3" {
		cfg.Region = "us-east-1" // S3's traditional default
	}
	if v := os.Getenv("BUCKETVCS_S3_ENDPOINT"); v != "" {
		cfg.Endpoint = v
	}
	if v := os.Getenv("BUCKETVCS_S3_FORCE_PATH_STYLE"); v != "" {
		cfg.ForcePathStyle = (v == "true" || v == "1")
	}
	if v := os.Getenv("BUCKETVCS_S3_PROFILE"); v != "" {
		cfg.Profile = v
	} else if v := os.Getenv("AWS_PROFILE"); v != "" && cfg.Profile == "" {
		cfg.Profile = v
	}
	// Static credentials (optional; SDK default chain otherwise).
	if v := os.Getenv("AWS_ACCESS_KEY_ID"); v != "" {
		cfg.AccessKeyID = v
		cfg.SecretAccessKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
		cfg.SessionToken = os.Getenv("AWS_SESSION_TOKEN")
	}
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./cmd/bucketvcs/... -run TestParseStoreURL`
Expected: PASS.

- [ ] **Step 5: Run the full project build to confirm no regressions**

Run: `go build ./... && go test ./... -short`
Expected: build + short tests pass. Live conformance still skipped.

- [ ] **Step 6: Commit**

```bash
git add cmd/bucketvcs/store.go cmd/bucketvcs/store_test.go
git commit -m "M5 task 14: wire s3:// and r2:// schemes through cmd/bucketvcs"
```

---

## Task 15: Diff harness env-driven store override

**Files:**
- Modify: `internal/diffharness/roundtrip_helpers_test.go`

- [ ] **Step 1: Modify `roundtrip_helpers_test.go`**

Replace the body of `newTestStore` with:

```go
func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	if url := os.Getenv("BUCKETVCS_DIFFHARNESS_STORE"); url != "" {
		s, err := openStoreFromURL(t, url)
		if err != nil {
			t.Fatalf("BUCKETVCS_DIFFHARNESS_STORE=%q: %v", url, err)
		}
		t.Cleanup(func() { _ = closeStore(s) })
		return s
	}
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
```

Add `"os"` to the imports if it isn't already there.

Add a sibling helper file `internal/diffharness/store_override_test.go`:

```go
package diffharness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
	"github.com/bucketvcs/bucketvcs/internal/storage/s3compat"
)

func openStoreFromURL(t *testing.T, url string) (storage.ObjectStore, error) {
	t.Helper()
	switch {
	case strings.HasPrefix(url, "localfs:"):
		return localfs.Open(strings.TrimPrefix(url, "localfs:"))
	case strings.HasPrefix(url, "s3://"), strings.HasPrefix(url, "r2://"):
		cfg, err := s3compat.ParseURL(url)
		if err != nil {
			return nil, err
		}
		envOverlay(&cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return s3compat.Open(ctx, cfg)
	default:
		return nil, fmt.Errorf("unsupported diffharness store URL %q", url)
	}
}

// envOverlay mirrors cmd/bucketvcs/applyEnvToConfig. Kept inline here
// (rather than importing main) because cmd/bucketvcs is a main package.
func envOverlay(cfg *s3compat.Config) {
	if v := os.Getenv("BUCKETVCS_S3_REGION"); v != "" {
		cfg.Region = v
	} else if v := os.Getenv("AWS_REGION"); v != "" && cfg.Region == "" {
		cfg.Region = v
	}
	if v := os.Getenv("BUCKETVCS_S3_ENDPOINT"); v != "" {
		cfg.Endpoint = v
	}
	if v := os.Getenv("BUCKETVCS_S3_FORCE_PATH_STYLE"); v != "" {
		cfg.ForcePathStyle = (v == "true" || v == "1")
	}
	if v := os.Getenv("BUCKETVCS_S3_PROFILE"); v != "" {
		cfg.Profile = v
	} else if v := os.Getenv("AWS_PROFILE"); v != "" && cfg.Profile == "" {
		cfg.Profile = v
	}
	if v := os.Getenv("AWS_ACCESS_KEY_ID"); v != "" {
		cfg.AccessKeyID = v
		cfg.SecretAccessKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
		cfg.SessionToken = os.Getenv("AWS_SESSION_TOKEN")
	}
}

func closeStore(s storage.ObjectStore) error {
	if c, ok := s.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// TestNewTestStoreFallsBackToLocalfs sanity-checks the env override:
// without BUCKETVCS_DIFFHARNESS_STORE we still get a usable store.
func TestNewTestStoreFallsBackToLocalfs(t *testing.T) {
	t.Setenv("BUCKETVCS_DIFFHARNESS_STORE", "")
	s := newTestStore(t)
	if s == nil {
		t.Fatal("newTestStore returned nil")
	}
}
```

- [ ] **Step 2: Run all diffharness tests against localfs**

Run: `go test ./internal/diffharness/... -short`
Expected: PASS — same behavior as before.

- [ ] **Step 3: (Optional) Run the diffharness against R2 with the same env from task 13**

Run:

```bash
BUCKETVCS_DIFFHARNESS_STORE=r2://${BUCKETVCS_R2_BUCKET} \
BUCKETVCS_R2_ENDPOINT=... AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
go test ./internal/diffharness/... -v
```

Expected: PASS — proves that import/export round-trip and clone/push oracles work against a real cloud bucket.

- [ ] **Step 4: Commit**

```bash
git add internal/diffharness/
git commit -m "M5 task 15: diffharness env-driven store override (cloud parameterization)"
```

---

## Task 16: Ship-gate runner script

**Files:**
- Create: `scripts/conformance-cloud.sh`
- Create: `.github/workflows/conformance-cloud.yml`

- [ ] **Step 1: Create `scripts/conformance-cloud.sh`**

```bash
#!/usr/bin/env bash
# scripts/conformance-cloud.sh
# Run the M5 ship-gate: live conformance + diffharness against R2 and
# (optionally) AWS S3. Skips a provider when its env is unset.
#
# Required for R2:
#   BUCKETVCS_R2_BUCKET, BUCKETVCS_R2_ENDPOINT, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
# Required for S3:
#   BUCKETVCS_S3_BUCKET, BUCKETVCS_S3_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY

set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> Running in-tree tests (no creds needed)"
go test ./internal/storage/s3compat/... -count=1

if [[ -n "${BUCKETVCS_R2_BUCKET:-}" && -n "${BUCKETVCS_R2_ENDPOINT:-}" ]]; then
  echo "==> R2 conformance"
  go test ./internal/storage/s3compat/... -run TestConformance_R2 -v -count=1

  echo "==> R2 diffharness"
  BUCKETVCS_DIFFHARNESS_STORE="r2://${BUCKETVCS_R2_BUCKET}/diffharness-$$" \
    go test ./internal/diffharness/... -count=1
else
  echo "==> R2: SKIPPED (BUCKETVCS_R2_BUCKET / BUCKETVCS_R2_ENDPOINT unset)"
fi

if [[ -n "${BUCKETVCS_S3_BUCKET:-}" && -n "${BUCKETVCS_S3_REGION:-}" ]]; then
  echo "==> S3 conformance"
  go test ./internal/storage/s3compat/... -run TestConformance_S3 -v -count=1
else
  echo "==> S3: SKIPPED (BUCKETVCS_S3_BUCKET / BUCKETVCS_S3_REGION unset)"
fi

echo "==> Cloud conformance completed"
```

Run: `chmod +x scripts/conformance-cloud.sh`

- [ ] **Step 2: Verify the script runs (without creds, skips both)**

Run: `./scripts/conformance-cloud.sh`
Expected: in-tree tests pass; R2 + S3 sections both print "SKIPPED".

- [ ] **Step 3: Create `.github/workflows/conformance-cloud.yml`**

```yaml
name: conformance-cloud
on:
  push:
    branches: [main]
  pull_request:
    paths:
      - 'internal/storage/s3compat/**'
      - 'internal/storage/conformance/**'
      - 'internal/storage/*.go'
      - 'cmd/bucketvcs/store*.go'
      - '.github/workflows/conformance-cloud.yml'

jobs:
  cloud:
    # Skip on PRs from forks: secrets are not available there.
    if: github.event_name == 'push' || github.event.pull_request.head.repo.full_name == github.repository
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25'
      - name: Run ship-gate
        env:
          BUCKETVCS_R2_BUCKET:    ${{ secrets.R2_BUCKET }}
          BUCKETVCS_R2_ENDPOINT:  ${{ secrets.R2_ENDPOINT }}
          BUCKETVCS_S3_BUCKET:    ${{ secrets.AWS_S3_BUCKET }}
          BUCKETVCS_S3_REGION:    ${{ secrets.AWS_REGION }}
          AWS_ACCESS_KEY_ID:      ${{ secrets.AWS_ACCESS_KEY_ID }}
          AWS_SECRET_ACCESS_KEY:  ${{ secrets.AWS_SECRET_ACCESS_KEY }}
        run: ./scripts/conformance-cloud.sh
```

- [ ] **Step 4: Commit**

```bash
git add scripts/conformance-cloud.sh .github/workflows/conformance-cloud.yml
git commit -m "M5 task 16: ship-gate runner + forward-looking GitHub Actions workflow"
```

---

## Task 17: Documentation, README, and ship-gate verification

**Files:**
- Create: `internal/storage/s3compat/README.md`
- Create: `docs/m5-cloud-quickstart.md`
- Modify: `cmd/bucketvcs/main.go` (only if `--store` flag help text mentions schemes — update if so)

- [ ] **Step 1: Create `internal/storage/s3compat/README.md`**

```markdown
# s3compat — S3-compatible storage adapter

Implements `internal/storage.ObjectStore` against any S3-compatible
object store via `aws-sdk-go-v2`. M5 ships this adapter as the
canonical Cloudflare R2 backend; AWS S3 is exercised in conformance
testing and is formally promoted to canonical at M7.

## CLI usage

```text
--store=s3://<bucket>[/<prefix>]
--store=r2://<bucket>[/<prefix>]
```

Required env vars (R2):

```text
BUCKETVCS_R2_ENDPOINT          https://<account>.r2.cloudflarestorage.com
BUCKETVCS_S3_REGION            (defaults to "auto" for r2:// URLs)
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
```

Required env vars (S3):

```text
BUCKETVCS_S3_REGION            e.g. us-east-1
AWS_ACCESS_KEY_ID              or use AWS_PROFILE
AWS_SECRET_ACCESS_KEY
```

## Provider quirks

- Cloudflare R2 occasionally returns generic `InternalError` where AWS
  S3 returns specific codes. The adapter classifies these as
  `ErrTransient` so callers retry.
- `CompleteMultipartUpload` with `If-None-Match: *` is the only way to
  guarantee no silent overwrite during multipart completion. The
  adapter does not abort on conflict; orphan parts are reclaimed by
  M8 GC.
- Single PUT vs multipart split: objects ≤ 8 MiB go via single PUT;
  multipart is invoked explicitly via `CreateMultipart`.

## Conformance

Run `scripts/conformance-cloud.sh` from the repo root. The script
skips providers whose env is unset.

## Ship-gate (M5)

1. `go test ./internal/storage/s3compat/...` (in-tree, no creds).
2. `scripts/conformance-cloud.sh` (R2 + S3 conformance).
3. `BUCKETVCS_DIFFHARNESS_STORE=r2://...` `go test ./internal/diffharness/...`.
```

- [ ] **Step 2: Create `docs/m5-cloud-quickstart.md`**

```markdown
# M5 Quickstart: Running bucketvcs against Cloudflare R2

This guide walks through configuring `bucketvcs` to use Cloudflare R2
as canonical storage. Replace `<...>` placeholders with your account
values.

## 1. Provision an R2 bucket

In the Cloudflare dashboard:

1. Create an R2 bucket (any name, e.g. `bucketvcs-prod`).
2. Mint an R2 access key. Copy the access-key-id and secret.
3. Note the S3 endpoint: `https://<account-id>.r2.cloudflarestorage.com`.

## 2. Initialize a repo on R2

```bash
export BUCKETVCS_R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
export AWS_ACCESS_KEY_ID=<r2-access-key-id>
export AWS_SECRET_ACCESS_KEY=<r2-secret>

bucketvcs init --store=r2://bucketvcs-prod acme my-repo
bucketvcs inspect-manifest --store=r2://bucketvcs-prod acme my-repo
```

## 3. Serve the gateway

```bash
bucketvcs serve --store=r2://bucketvcs-prod \
  --auth-db=./auth.sqlite --listen=:8080
```

## 4. Run M3 protocol tests against the live bucket

```bash
git clone http://localhost:8080/acme/my-repo.git /tmp/clone-test
```

## 5. Migrating existing repo data from localfs

R2 layouts are bit-identical to localfs layouts. Use any S3-compatible
sync tool, for example:

```bash
aws s3 sync /var/lib/bucketvcs/ s3://bucketvcs-prod/ \
  --endpoint-url=https://<account-id>.r2.cloudflarestorage.com
```

After migration, point `--store` at `r2://bucketvcs-prod` and verify
with `bucketvcs inspect-manifest`.
```

- [ ] **Step 3: Verify all tests still pass**

Run: `go build ./... && go test ./... -short`
Expected: PASS.

- [ ] **Step 4: Verify the ship gate runs locally without creds**

Run: `./scripts/conformance-cloud.sh`
Expected: in-tree pass; both R2 + S3 sections print SKIPPED.

- [ ] **Step 5: (Manual, only with credentials) Run the full ship gate**

Set the R2 + S3 env vars and run:

```bash
BUCKETVCS_R2_BUCKET=...  BUCKETVCS_R2_ENDPOINT=...  \
BUCKETVCS_S3_BUCKET=...  BUCKETVCS_S3_REGION=...    \
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...     \
./scripts/conformance-cloud.sh
```

Expected:
- R2 conformance: all 15 correctness tests pass; stress tests pass.
- S3 conformance: same.
- R2 diffharness: clone/fetch/push oracles pass.

If anything fails, stop. Add a regression test in the relevant earlier task's test file, then re-run until pass.

- [ ] **Step 6: Update the auto-memory after merge**

Create `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m5_progress.md`:

```markdown
---
name: M5 first cloud backend merged to main
description: M5 progress entry — s3compat adapter for R2 + S3, conformance-gated
type: project
---

M5 first cloud backend merged to main.

- Commit: <fill in after merge>
- Tag: m5-complete (2026-05-XX)
- Package: internal/storage/s3compat (single S3-compatible adapter)
- Schemes wired in cmd/bucketvcs: s3:// (AWS S3 + MinIO), r2:// (Cloudflare R2)
- Conformance: full §29 suite passes against R2 (canonical) and S3 (M7 promotion in progress)
- Diffharness: import/export + clone/push oracles pass against live R2
- Ship gate: scripts/conformance-cloud.sh
- CI: .github/workflows/conformance-cloud.yml (forward-looking; activates when project CI lands)

What's NOT in M5: GCS (M7), Azure Blob (M7), R2 Worker bindings (post-MVP), localfs->cloud migration tool (manual `aws s3 sync` documented in docs/m5-cloud-quickstart.md).
```

Update `MEMORY.md` to add a new line after the M4 entry:

```markdown
- [M5 first cloud backend merged to main](m5_progress.md) — commit <hash>, tag m5-complete (2026-05-XX); s3compat adapter for R2 (canonical) + AWS S3 (M7 promotion)
```

- [ ] **Step 7: Commit docs and final ship-gate evidence**

```bash
git add internal/storage/s3compat/README.md docs/m5-cloud-quickstart.md
git commit -m "M5 task 17: README + quickstart + ship-gate verification"
```

---

## Final notes

- **Per-task review protocol** (per memory: M1+ review protocol): after each task's commit, run `superpowers:requesting-code-review` and `roborev-refine` against the commit per the user's established workflow. Do NOT proceed to the next task until the current task's reviews close.
- **Worktree:** if working in a worktree, the standard pattern is `git worktree add .claude/worktrees/m5-cloud` from `main` and merge back when M5 ships.
- **Spec authority:** if implementation reveals a gap in the spec, update the spec first (in a separate commit) and reference it in the implementation commit message. Do not silently diverge.
