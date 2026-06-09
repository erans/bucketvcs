# CodeBuild Permanent-Error Classification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Classify `codeBuildDeliverer` `StartBuild` failures as permanent (immediate dead-letter, `reason=permanent`) vs transient, bringing CodeBuild to parity with the other four build-trigger providers.

**Architecture:** A `codeBuildPermanent(err) bool` helper follows the existing `s3compat` AWS-error house pattern — match `smithy.APIError` code (throttling → retryable; ResourceNotFound/InvalidInput/AccessDenied → permanent), fall back to `awshttp.ResponseError` HTTP status via the existing `httpStatusPermanent`, default retryable. A permanent `StartBuild` error is wrapped with `permanentf` so the already-shipped `recordResult` routing dead-letters it immediately. `clientFor` and mint failures stay retryable. No worker/metric/migration/CLI/wire changes.

**Tech Stack:** Go, `github.com/aws/smithy-go` (`smithy.APIError`), `github.com/aws/aws-sdk-go-v2/aws/transport/http` (`awshttp.ResponseError`), `github.com/aws/smithy-go/transport/http` (`smithyhttp` — test only), the existing `permanentf`/`httpStatusPermanent`/`ErrPermanent` primitives in `deliver.go`.

**Spec:** `docs/superpowers/specs/2026-06-08-codebuild-permanent-errors-design.md`

---

## File Structure

**Modify:**
- `internal/buildtrigger/codebuild.go` — add `codeBuildPermanent`; classify the `StartBuild` error in `Deliver`.
- `internal/buildtrigger/codebuild_test.go` — classifier table test + deliverer-level permanence tests (add a `fakeAPIError` + an error-returning fake `startBuildAPI`).
- `docs/operator-guides/build-triggers.md` — update the §9.2 "codebuild retry-only" line.
- `docs/build-triggers.md` — update any codebuild retry-only caveat if present.

No new files. The classifier lives next to the deliverer it serves.

---

## Task 1: `codeBuildPermanent` classifier

**Files:**
- Modify: `internal/buildtrigger/codebuild.go`
- Test: `internal/buildtrigger/codebuild_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/buildtrigger/codebuild_test.go`:

```go
// fakeAPIError satisfies smithy.APIError for classifier tests.
type fakeAPIError struct{ code string }

func (e fakeAPIError) Error() string                 { return e.code }
func (e fakeAPIError) ErrorCode() string             { return e.code }
func (e fakeAPIError) ErrorMessage() string          { return e.code }
func (e fakeAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }

func httpRespErr(status int) error {
	return &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		},
	}
}

func TestCodeBuildPermanent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"resource-not-found", fakeAPIError{"ResourceNotFoundException"}, true},
		{"invalid-input", fakeAPIError{"InvalidInputException"}, true},
		{"access-denied", fakeAPIError{"AccessDeniedException"}, true},
		{"throttling", fakeAPIError{"ThrottlingException"}, false},
		{"request-limit", fakeAPIError{"RequestLimitExceeded"}, false},
		{"http-404", httpRespErr(404), true},
		{"http-503", httpRespErr(503), false},
		{"http-429", httpRespErr(429), false},
		{"plain", errors.New("boom"), false},
		{"nil-ish-plain", errors.New(""), false},
	}
	for _, tc := range cases {
		if got := codeBuildPermanent(tc.err); got != tc.want {
			t.Errorf("%s: codeBuildPermanent=%v, want %v", tc.name, got, tc.want)
		}
	}
}
```

Add these imports to `codebuild_test.go` (it already imports `context`, `errors`, `testing`, `codebuild`, `cbtypes`):

```go
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run 'CodeBuildPermanent' -v`
Expected: FAIL — `undefined: codeBuildPermanent`.

- [ ] **Step 3: Implement the classifier**

In `internal/buildtrigger/codebuild.go`, add to the import block: `"errors"` (top, with stdlib), and in the third-party group `awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"` and `smithy "github.com/aws/smithy-go"`. Then append to the file:

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

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/buildtrigger/ -run 'CodeBuildPermanent' -v`
Expected: PASS.

- [ ] **Step 5: Run the full package**

Run: `go test ./internal/buildtrigger/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/buildtrigger/codebuild.go internal/buildtrigger/codebuild_test.go
git commit -m "feat(buildtrigger): codeBuildPermanent classifier (APIError code + HTTP fallback)"
```

---

## Task 2: Classify the `StartBuild` error in `Deliver`

**Files:**
- Modify: `internal/buildtrigger/codebuild.go`
- Test: `internal/buildtrigger/codebuild_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/buildtrigger/codebuild_test.go`:

```go
// errStartBuild is a startBuildAPI fake that always returns err.
type errStartBuild struct{ err error }

func (f errStartBuild) StartBuild(ctx context.Context, in *codebuild.StartBuildInput, _ ...func(*codebuild.Options)) (*codebuild.StartBuildOutput, error) {
	return nil, f.err
}

func TestCodeBuildDeliverer_PermanentError(t *testing.T) {
	d := &codeBuildDeliverer{
		clientFor: func(Trigger) (startBuildAPI, error) {
			return errStartBuild{err: fakeAPIError{"ResourceNotFoundException"}}, nil
		},
		mintFn: func(context.Context, Trigger, BuildPayload) (string, error) { return "", nil },
	}
	tr := Trigger{Kind: KindCodeBuild, TokenMode: TokenNone, Config: Config{AWSProject: "p"}}
	_, err := d.Deliver(context.Background(), tr, BuildPayload{Repo: "app"})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("ResourceNotFoundException should be permanent, got %v", err)
	}
}

func TestCodeBuildDeliverer_ThrottlingIsRetryable(t *testing.T) {
	d := &codeBuildDeliverer{
		clientFor: func(Trigger) (startBuildAPI, error) {
			return errStartBuild{err: fakeAPIError{"ThrottlingException"}}, nil
		},
		mintFn: func(context.Context, Trigger, BuildPayload) (string, error) { return "", nil },
	}
	tr := Trigger{Kind: KindCodeBuild, TokenMode: TokenNone, Config: Config{AWSProject: "p"}}
	_, err := d.Deliver(context.Background(), tr, BuildPayload{Repo: "app"})
	if err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("throttling must be retryable, got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/buildtrigger/ -run 'CodeBuildDeliverer_(PermanentError|ThrottlingIsRetryable)' -v`
Expected: FAIL — `TestCodeBuildDeliverer_PermanentError` fails because the StartBuild error is currently a plain `fmt.Errorf` (not `ErrPermanent`). (`ThrottlingIsRetryable` already passes — it asserts non-permanence.)

- [ ] **Step 3: Wire the classifier into Deliver**

In `internal/buildtrigger/codebuild.go`, change the `StartBuild` error site from:

```go
	out, err := client.StartBuild(ctx, &codebuild.StartBuildInput{
		ProjectName:                  aws.String(tr.Config.AWSProject),
		SourceVersion:                aws.String(sourceVersion),
		EnvironmentVariablesOverride: envOverrides,
	})
	if err != nil {
		return 0, fmt.Errorf("codebuild StartBuild: %w", err)
	}
```

to:

```go
	out, err := client.StartBuild(ctx, &codebuild.StartBuildInput{
		ProjectName:                  aws.String(tr.Config.AWSProject),
		SourceVersion:                aws.String(sourceVersion),
		EnvironmentVariablesOverride: envOverrides,
	})
	if err != nil {
		if codeBuildPermanent(err) {
			return 0, permanentf("codebuild StartBuild: %v", err)
		}
		return 0, fmt.Errorf("codebuild StartBuild: %w", err)
	}
```

Leave the `clientFor` error return and the mint-failure return unchanged (retryable).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/buildtrigger/ -run 'CodeBuildDeliverer' -v`
Expected: PASS — including the pre-existing `TestCodeBuildDeliverer_StartBuildInputs` and `TestCodeBuildDeliverer_NoTokenWhenModeNone`.

- [ ] **Step 5: Run the full package + vet**

Run: `go test ./internal/buildtrigger/ && go vet ./internal/buildtrigger/`
Expected: PASS, no vet findings.

- [ ] **Step 6: Commit**

```bash
git add internal/buildtrigger/codebuild.go internal/buildtrigger/codebuild_test.go
git commit -m "feat(buildtrigger): codebuild marks non-throttling StartBuild errors permanent"
```

---

## Task 3: Documentation

**Files:**
- Modify: `docs/operator-guides/build-triggers.md`
- Modify: `docs/build-triggers.md` (only if a stale caveat is found)

- [ ] **Step 1: Update the operator guide §9.2**

In `docs/operator-guides/build-triggers.md`, find the line in the "Permanent vs. transient failures" subsection that reads:

```markdown
`codebuild` errors are currently always treated as transient (retry-only).
```

Replace it with:

```markdown
`codebuild` `StartBuild` errors are classified the same way: `ResourceNotFoundException`
(project missing), `InvalidInputException`, `AccessDeniedException`, and other
non-throttling 4xx responses are permanent; throttling (`ThrottlingException`,
`RequestLimitExceeded`) and 5xx remain transient. The AWS config/credential-load
step stays retry-only (it can fail transiently, and real credential errors
surface at `StartBuild` as a 403).
```

- [ ] **Step 2: Scan for other stale caveats**

Run: `grep -rn "retry-only\|HTTP deliverers only\|codebuild.*transient\|codeBuildDeliverer is untouched" docs/operator-guides/build-triggers.md docs/build-triggers.md`

For each hit that asserts CodeBuild is *not* classified (e.g. a "permanent classification applies to HTTP deliverers only" phrasing), update it to state all five providers are now classified. Do NOT edit files under `docs/superpowers/specs/` or `docs/superpowers/plans/` — those are point-in-time records. If the grep returns no relevant hits beyond the §9.2 line already fixed, make no further edits.

- [ ] **Step 3: Commit**

```bash
git add docs/operator-guides/build-triggers.md docs/build-triggers.md
git commit -m "docs(buildtrigger): codebuild now classifies permanent vs transient errors"
```

(If `docs/build-triggers.md` was not modified, omit it from the `git add`.)

---

## Task 4: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 2: Vet**

Run: `go vet ./internal/buildtrigger/`
Expected: no findings.

- [ ] **Step 3: Full package test**

Run: `go test ./internal/buildtrigger/ -v`
Expected: PASS — including `TestCodeBuildPermanent`, `TestCodeBuildDeliverer_PermanentError`, `TestCodeBuildDeliverer_ThrottlingIsRetryable`, and all pre-existing codebuild/worker/deliver tests.

- [ ] **Step 4: Confirm no migration/wire drift**

Run: `git diff --stat main...HEAD -- internal/buildtrigger/testdata/ internal/auth/sqlitestore/migrations/`
Expected: empty output.

- [ ] **Step 5: Request code review**

Use the superpowers:requesting-code-review skill (or `/roborev-review-branch`) before merging.
