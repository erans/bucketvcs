# Build-trigger outbound-shape verification tests

Date: 2026-06-08
Builds on: M30 build triggers (`internal/buildtrigger`).

## 1. Goal

Prove, hermetically and in normal CI, that for each supported build-target kind
the `buildtrigger` delivery worker **actually emits an outbound request** and
that the request is in the **correct shape** for that build system. "Shape" is
anchored by committed golden files (HTTP bodies) and inline assertions
(CodeBuild `StartBuild` params), so any accidental change to what a build
system receives fails the test loudly and shows up as a reviewable diff.

This closes the highest-ROI gap from the M30 final review: the CodeBuild path
had no over-the-wire coverage (only an interface fake), and there was no
committed contract for the generic/cloudbuild request bodies.

### 1.1 In scope

- **Worker-driven capture.** Drive the real `StartWorker` loop
  (enqueue → claim → deliver → record), not the deliverer in isolation, so the
  test proves the request is genuinely sent through the production path.
- **Three targets**, `token_mode=inject` only (the richest body — it carries
  the minted token):
  - `generic` — real `httpDeliverer` POST to a local `httptest.Server`.
  - `cloudbuild` — same, with the flattened `ref`/`commit` fields.
  - `codebuild` — real `codeBuildDeliverer` + real aws-sdk-go-v2 client pointed
    at a fake CodeBuild `httptest` endpoint via `BaseEndpoint`.
- **Golden snapshot contracts** for the generic + cloudbuild bodies; inline
  assertions for the decoded CodeBuild `StartBuild` input.
- **Signature validation** for generic/cloudbuild: recompute
  `webhooks.Sign(secret, t, capturedBody)` from the `BucketVCS-Signature`
  header and compare — proves we sign the exact bytes we send.

### 1.2 Out of scope (explicit, deferred)

- OIDC-pull token flow (build exchanging a JWT) — future broader e2e harness.
- SSH transport — enqueue is unit-tested; wire shape is transport-independent.
- Full `serve` + real `git push` — the existing `scripts/smoke_buildtriggers.sh`
  covers the generic push path; this is a focused shape test, not another smoke.
- `token_mode=none` golden — the no-token shape is already covered by the
  Task 7 deliverer unit test.
- Expired-token, retry/dead-letter/orphan — already covered by worker tests.
- CodeBuild SigV4 **cryptographic re-verification** — that tests the AWS SDK's
  signer, not our code; we assert an `Authorization` header is present (SigV4
  ran) and assert the request *params* we construct.
- Real cloud endpoints (Cloud Build / CodeBuild) — need creds + cost; the local
  fakes cover the contract that actually regresses.

## 2. Architecture

```
test (per kind)
  ├─ open authdb (sqlitestore, in-mem/tmp) + seed repo acme/app   [reuse newTestSvc]
  ├─ create trigger(kind, token_mode=inject) → capture target
  ├─ Enqueue(fixedPushInfo)                                        [real enqueue]
  ├─ StartWorker(ctx, svc, cfg) with:
  │     cfg.Deliverers[kind] = <real deliverer, capture-pointed>   [injected seam]
  │     cfg.MintFn           = fixed-token mint                    [determinism]
  │     fast tick + short backoff
  ├─ waitUntil(delivered)                                          [reuse helper]
  └─ assert captured request vs golden / inline

capture targets:
  generic / cloudbuild → httptest.Server records {method,path,headers,body}, 200
  codebuild            → httptest.Server speaks AWS JSON: decodes
                         X-Amz-Target=CodeBuild_20161006.StartBuild body,
                         records parsed input, returns minimal StartBuild JSON
```

### 2.1 Injected seams (already exist — no production change)

- `WorkerConfig.Deliverers map[Kind]Deliverer` — when non-nil, `StartWorker`
  uses it verbatim (production builds it via `ProductionDeliverers`). The test
  supplies real deliverers wired to the capture targets.
- `httpDeliverer{client *http.Client; mintFn MintFunc}` — test passes a plain
  `http.Client` (NOT the egress-guarded one) so a loopback `httptest` target is
  not blocked by the default-deny egress policy. (Egress is separately tested;
  this test is about shape.)
- `codeBuildDeliverer{clientFor func(Trigger)(startBuildAPI,error); mintFn}` —
  test supplies a `clientFor` returning a real `codebuild.Client` built with
  dummy static creds, a fixed region, and `BaseEndpoint = <fake server URL>`.
  This runs **real SigV4** against the fake.
- `WorkerConfig.MintFn` — test supplies a deterministic `MintFunc` returning a
  fixed token string (e.g. `bvts_TESTTOKEN`) so the rendered body is stable.

No production code changes are required; all seams are pre-existing.

### 2.2 Determinism → exact goldens

Fixed inputs make the rendered body fully deterministic:

- `PushInfo{Tenant:"acme", Repo:"app", Actor:"tester", TxID:"tx-test",
  HeadOID:"<40-hex>", RefUpdates:[{Refname:"refs/heads/main",
  OldOID:"<zero>", NewOID:"<40-hex>"}]}`.
- `MintFn` → fixed `bvts_TESTTOKEN`.

`RenderBody` draws every field from `BuildPayload` + the token, so the body
bytes are identical run-to-run → golden files are exact, no normalization. The
only dynamic value (`t` in the signature header) is validated dynamically, not
stored in a golden.

## 3. Files

- `internal/buildtrigger/wireshape_test.go` — harness + `TestWireShape_Generic`,
  `TestWireShape_CloudBuild`, `TestWireShape_CodeBuild`. Harness pieces:
  - `newGenericCapture(t)` / `newCloudBuildCapture(t)` → `*httptest.Server` +
    a captured-request accessor.
  - `newCodeBuildFake(t)` → `*httptest.Server` that decodes the AWS JSON
    `StartBuild` body and exposes the parsed input + headers; plus a
    `clientFor` factory targeting it (`BaseEndpoint`, dummy static creds,
    region `us-east-1`).
  - `assertGolden(t, name, gotBody []byte)` with a package-level
    `var update = flag.Bool("update", false, "rewrite golden files")` —
    `go test ./internal/buildtrigger/ -run WireShape -update` regenerates.
  - `fixedPush()` and `fixedMint` helpers.
  - Reuses existing `newTestSvc`, `waitUntil`, `countByStatus`.
- `internal/buildtrigger/testdata/generic_body.golden.json`
- `internal/buildtrigger/testdata/cloudbuild_body.golden.json`

(No CodeBuild golden file — its params are asserted inline per the chosen
approach.)

## 4. Assertions per target

### 4.1 generic
- exactly one request received; method `POST`, path `/`.
- headers: `Content-Type: application/json`, `User-Agent: bucketvcs-buildtrigger/1`,
  `BucketVCS-Signature` present and of form `t=<unix>,v1=<hex>`.
- signature valid: parse `t`, recompute `webhooks.Sign(triggerSecret, t, body)`,
  assert equal to the header's value.
- body bytes equal `generic_body.golden.json` (contains `tenant/repo/actor/
  tx_id/head_oid/ref_update/bvts_token`; NO `ref`/`commit` flattening).

### 4.2 cloudbuild
- as generic, but body equals `cloudbuild_body.golden.json`, which additionally
  carries top-level `ref` (= refname) and `commit` (= new_oid) alongside
  `bvts_token`.

### 4.3 codebuild
- the fake receives exactly one `StartBuild` (assert `X-Amz-Target` ==
  `CodeBuild_20161006.StartBuild`) with an `Authorization` header present
  (SigV4 ran).
- decoded input asserts: `projectName == "<trigger project>"`,
  `sourceVersion == HeadOID`, and `environmentVariablesOverride` contains
  `BV_REF=refs/heads/main`, `BV_REPO=acme/app`, `BV_COMMIT=<HeadOID>`,
  `BVTS_TOKEN=bvts_TESTTOKEN` (each `PLAINTEXT`).

## 5. Error handling / robustness

- Each test uses its own authdb + capture server + `t.Cleanup` to close them;
  no shared state, parallel-safe.
- `waitUntil` bounds the wait (e.g. 3s) and fails with a clear message if the
  delivery never reaches `delivered` (so a regression that *stops sending*
  fails as a timeout, not a hang).
- The fake CodeBuild endpoint returns a minimal but valid `StartBuild` JSON
  response (`{"build":{"id":"b-1"}}`) so the SDK marks the call successful and
  the worker records `delivered`.
- Golden mismatch prints a unified diff (got vs want) to make drift obvious.

## 6. Testing (of the tests)

- Run `go test ./internal/buildtrigger/ -run WireShape -race` — all three pass.
- Sanity that the goldens are real contracts: a reviewer can eyeball
  `testdata/*.json` and see exactly what each build system receives.
- The `-update` flag regenerates goldens after an intentional shape change
  (documented in the test file header comment).

## 7. Acceptance criteria

- For each of generic / cloudbuild / codebuild, the test drives the real worker,
  confirms an outbound request was actually emitted, and asserts its shape
  (golden body + valid signature for HTTP; decoded `StartBuild` params + SigV4
  presence for CodeBuild).
- Tests are hermetic (loopback only), deterministic, and run under the default
  `go test` (no build tag, no creds, no network egress).
- No production code changes — only test + testdata files.
- An accidental change to `RenderBody` output or the CodeBuild input mapping
  fails the relevant test with a clear diff.

## 8. Resolved details

- **Golden format = exact captured body bytes.** `RenderBody` marshals a
  `map[string]any`, and Go's `encoding/json` emits map keys in sorted order, so
  the body is byte-for-byte deterministic across runs. The golden stores the raw
  captured body and the comparison is a byte compare (no canonicalization step);
  `-update` writes the raw bytes. For human readability the golden is committed
  pretty-printed only if `RenderBody` itself emits indented JSON — it does not,
  so the golden is the compact form exactly as sent. (A readable diff on
  mismatch is produced by the test, not by reformatting the golden.)
- **Three focused test funcs over a shared harness**, not one table — the
  CodeBuild capture (AWS JSON + SigV4) differs enough from the plain POST path
  that three funcs read more clearly.
