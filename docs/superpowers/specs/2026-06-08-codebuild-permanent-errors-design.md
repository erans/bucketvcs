# CodeBuild permanent-error classification

Date: 2026-06-08
Builds on: build-trigger permanent-error classification
([[m31 azure build triggers]] preceded it; the permanent-error mechanism —
`ErrPermanent`, `permanentf`, `httpStatusPermanent`, `recordResult` routing,
`reason={permanent,exhausted}` — shipped in PR #23). Follows the AWS error
classification house pattern in `internal/storage/s3compat/errs.go`.

## 1. Goal

The permanent-error feature classified the HTTP deliverers (generic, cloudbuild,
azurewebhook, azurepipelines) but explicitly deferred `codeBuildDeliverer`,
which goes through the AWS SDK and returns typed errors. This closes that
deferral: a `StartBuild` failure that cannot succeed on retry (missing project,
invalid input, IAM denial, or any other non-throttling 4xx) now dead-letters
**immediately** with `reason=permanent` instead of churning the full
1m→30m→2h→12h (~14h) backoff. CodeBuild reaches parity with the other four
providers.

### 1.1 In scope

- A `codeBuildPermanent(err error) bool` classifier in `codebuild.go`.
- Wrap a permanent `StartBuild` error with `permanentf` (so `recordResult`'s
  existing `errors.Is(err, ErrPermanent)` check routes it to dead_letter).
- Unit tests for the classifier + a deliverer-level permanence test.
- Docs: remove the "codebuild is retry-only" caveats now that it classifies.

### 1.2 Out of scope (deferred / unchanged)

- **The `clientFor` (AWS config/credential load) error stays retryable.** It can
  fail transiently (IMDS/credential-provider hiccups), and genuine credential
  or permission failures surface at `StartBuild` as a 403 → already classified
  permanent here. Only `StartBuild` errors are classified.
- **Mint-failure** return stays retryable (unchanged).
- No new metric/audit (reuses the `reason=permanent` path), no migration, no CLI
  change, no wire-shape change.

## 2. The classifier (`internal/buildtrigger/codebuild.go`)

Add imports: `errors`, `github.com/aws/smithy-go` (as `smithy`),
`github.com/aws/aws-sdk-go-v2/aws/transport/http` (as `awshttp`).

```go
// codeBuildPermanent reports whether a StartBuild error is a permanent failure
// (won't succeed on retry). Mirrors the s3compat classification house pattern:
// match the API error code, fall back to HTTP status, default to retryable.
func codeBuildPermanent(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "ThrottlingException", "RequestLimitExceeded", "TooManyRequestsException":
			return false // transient — retry
		case "ResourceNotFoundException", "InvalidInputException", "AccessDeniedException":
			return true
		}
	}
	var httpErr *awshttp.ResponseError
	if errors.As(err, &httpErr) {
		if httpErr.Response != nil && httpErr.Response.Response != nil {
			return httpStatusPermanent(httpErr.Response.Response.StatusCode)
		}
	}
	return false // default: prefer retry over wrongly-permanent
}
```

Ordering matters: the throttling carve-out is checked **before** the HTTP-status
fallback, so a throttle that surfaces as a 4xx is never misclassified as
permanent. `httpStatusPermanent` (defined in `deliver.go`) is reused, so the
408/429 carve-out applies to the fallback automatically.

## 3. Deliverer change

In `codeBuildDeliverer.Deliver`, the `StartBuild` error site changes from:

```go
	out, err := client.StartBuild(ctx, &codebuild.StartBuildInput{ ... })
	if err != nil {
		return 0, fmt.Errorf("codebuild StartBuild: %w", err)
	}
```

to:

```go
	out, err := client.StartBuild(ctx, &codebuild.StartBuildInput{ ... })
	if err != nil {
		if codeBuildPermanent(err) {
			return 0, permanentf("codebuild StartBuild: %v", err)
		}
		return 0, fmt.Errorf("codebuild StartBuild: %w", err)
	}
```

`permanentf` is used with `%v` (consistent with the azure deliverer): only
`ErrPermanent` rides the `errors.Is` chain; the AWS error's message is preserved
in the string for `last_error`/audit. The `clientFor` error and the mint-failure
return are unchanged (retryable).

No worker change: `recordResult` already routes `ErrPermanent` to immediate
dead_letter with `reason=permanent`.

## 4. Testing (`internal/buildtrigger/codebuild_test.go`)

A small fake satisfying `smithy.APIError` (used to drive the classifier):

```go
type fakeAPIError struct{ code string }

func (e fakeAPIError) Error() string                 { return e.code }
func (e fakeAPIError) ErrorCode() string             { return e.code }
func (e fakeAPIError) ErrorMessage() string          { return e.code }
func (e fakeAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }
```

1. **`codeBuildPermanent` table test:**
   - `fakeAPIError{"ResourceNotFoundException"}`, `{"InvalidInputException"}`,
     `{"AccessDeniedException"}` → `true`
   - `fakeAPIError{"ThrottlingException"}`, `{"RequestLimitExceeded"}` → `false`
   - `&awshttp.ResponseError{Response: &awshttp.Response{Response:
     &http.Response{StatusCode: 404}}}` → `true`; `503` → `false`; `429` → `false`
   - `errors.New("boom")` → `false`
2. **Deliverer-level permanence test:** add an error-returning fake
   `startBuildAPI` (the existing `fakeStartBuild` always succeeds), e.g.
   `errStartBuild{err: fakeAPIError{"ResourceNotFoundException"}}`, and assert
   `Deliver` returns an error with `errors.Is(err, ErrPermanent) == true`; with
   `fakeAPIError{"ThrottlingException"}` assert `errors.Is(...) == false`.

The worker routing (`reason=permanent` → immediate dead_letter) is already
covered by `TestWorker_PermanentErrorDeadLettersImmediately`.

## 5. Docs

- `docs/operator-guides/build-triggers.md` §9.2 currently states *"`codebuild`
  errors are currently always treated as transient (retry-only)."* Replace with:
  codebuild now classifies too — `ResourceNotFoundException`,
  `InvalidInputException`, `AccessDeniedException`, and other non-throttling 4xx
  responses are permanent; throttling and 5xx remain transient. (The AWS
  config/credential-load step stays retry-only.)
- Grep the operator guide and `docs/build-triggers.md` for any other
  "codebuild … retry-only" / "HTTP deliverers only" caveats from the prior
  milestone and update them to reflect full provider coverage.

## 6. Verification

- `go build ./...`, `go vet ./internal/buildtrigger/`, `go test ./...` all pass.
- `git diff --stat` shows no migration or testdata/golden changes.
