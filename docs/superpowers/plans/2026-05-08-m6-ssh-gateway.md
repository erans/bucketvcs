# M6 — SSH Gateway and SSH Public-Key Authentication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Git-over-SSH to the OSS gateway (`golang.org/x/crypto/ssh`), with user SSH keys and per-repo deploy keys, sharing the M4 transport-neutral authorization engine. Refactor the upload-pack and receive-pack engines into a transport-neutral `internal/gitproto` package so HTTP and SSH share one engine.

**Architecture:** Two parallel changes. (1) Engine refactor: lift HTTP-coupled `internal/gateway/{upload,receive}_pack.go` and the V0 advertisement helpers from `inforefs.go` into `internal/gitproto/{uploadpack,receivepack}` as pure `(io.Reader, io.Writer)` drivers; HTTP handlers become thin adapters. (2) SSH gateway: a new `internal/sshd` package implementing the SSH server, exec-command parser, host-key load/generate, and session handler that calls the same `gitproto` engines. Auth grows by adding an optional `*Scope` return from `Store.VerifyCredential` that short-circuits `LookupRepoPerm` for deploy keys; `auth.Decide` is unchanged. A new `ssh_keys` table holds user keys (`user_id` set) and deploy keys (`scope_tenant`/`scope_repo`/`scope_perm` set), enforced as user-XOR-deploy by SQL CHECK.

**Tech Stack:** Go 1.22+, `golang.org/x/crypto/ssh` (SSH server), existing `modernc.org/sqlite` driver, existing `internal/auth`, `internal/auth/sqlitestore`, `internal/gateway`, `internal/mirror`, `internal/repo`, `internal/storage` from M0–M5.

**Spec:** `docs/superpowers/specs/2026-05-08-m6-ssh-gateway-design.md` (commit 74fea34).

---

## File Structure

**New packages:**

```
internal/gitproto/uploadpack/
  doc.go, engine.go, advertise.go, service.go, engine_test.go

internal/gitproto/receivepack/
  doc.go, engine.go, advertise.go, service.go, engine_test.go

internal/sshd/
  doc.go, server.go, hostkey.go, session.go, command.go, fingerprint.go
  server_test.go, hostkey_test.go, session_test.go, command_test.go,
  fingerprint_test.go, e2e_test.go
```

**New files in existing packages:**

```
internal/auth/sqlitestore/migrations/0002_ssh_keys.sql
internal/auth/sqlitestore/sshkeys_test.go
internal/auth/conformance/sshkeys.go        (extension to M4 conformance suite)

cmd/bucketvcs/ssh.go        ssh subcommand group + bucketvcs ssh fingerprint
cmd/bucketvcs/userkey.go    user key add/list/revoke
cmd/bucketvcs/deploykey.go  repo deploy-key add/list/revoke
```

**Modified existing files:**

```
internal/auth/types.go              + Scope, SSHKey types
internal/auth/store.go              VerifyCredential signature: + *Scope return,
                                    + AddSSHKey/List*/Revoke*/TouchSSHKeyUsage
internal/auth/permissions_test.go   + scope-based decision rows
internal/auth/conformance/conformance.go   + tests #14-#22 wiring

internal/auth/sqlitestore/store.go      + ssh_keys CRUD, +VerifyCredential SSHKeyFingerprint branch
internal/auth/sqlitestore/store_test.go + ssh_keys behavior tests

internal/gateway/upload_pack.go     SHRINK: thin adapter over uploadpack.Service
internal/gateway/receive_pack.go    SHRINK: thin adapter over receivepack.Service
internal/gateway/inforefs.go        SHRINK: calls uploadpack.Advertise and
                                    receivepack.Advertise; v2 detection stays
internal/gateway/auth.go            handle optional *Scope from VerifyCredential
internal/gateway/server.go          unchanged signature; constructs Store as before

cmd/bucketvcs/serve.go              + --ssh-addr, --ssh-host-key, --ssh-grace;
                                    + sshd.Server lifecycle in same process
cmd/bucketvcs/main.go               + dispatch user-key, deploy-key, ssh subcommands
cmd/bucketvcs/user.go               + "key" subgroup wiring
cmd/bucketvcs/repo.go               + "deploy-key" subgroup wiring

go.mod                              + golang.org/x/crypto (already present in M4
                                    via argon2; transitively provides ssh — verify)
```

---

## Worktree

Per the M1+ review-protocol memory, M6 work happens in a dedicated worktree branched off `main` at the `m5-complete` tag. Before Task 1:

```bash
git worktree add -b m6-ssh-gateway .claude/worktrees/m6-ssh-gateway m5-complete
cd .claude/worktrees/m6-ssh-gateway
```

All file paths below are repo-relative.

---

## Phase 1 — Engine refactor (preserves M3 behavior byte-for-byte; no SSH yet)

This phase MUST land green and pass every existing M3 differential test before any SSH code. The gate at the end of Phase 1 is "all M3 fixtures still byte-equal" — if not, do not advance.

### Task 1: `internal/gitproto/uploadpack` skeleton + `EngineRequest`

**Files:**
- Create: `internal/gitproto/uploadpack/doc.go`
- Create: `internal/gitproto/uploadpack/engine.go`
- Create: `internal/gitproto/uploadpack/engine_test.go`

- [ ] **Step 1: Write `doc.go`**

```go
// Package uploadpack drives the Git upload-pack protocol over arbitrary
// (io.Reader, io.Writer) pairs. HTTP gateway handlers and the SSH session
// handler are both adapters around this engine.
//
// The engine has three entry points:
//
//   Advertise — write the initial ref/capability advertisement
//   Service   — read wants/haves from Stdin, write pack/responses to Stdout
//   Serve     — Advertise then Service (used by SSH; HTTP splits across
//               info/refs and POST upload-pack handlers)
//
// The engine has no HTTP, no SSH, and no SQL imports.
package uploadpack
```

- [ ] **Step 2: Write `engine.go` with `EngineRequest` + stub funcs**

```go
package uploadpack

import (
	"context"
	"errors"
	"io"

	"github.com/eransandler/bucketvcs/internal/auth"
	"github.com/eransandler/bucketvcs/internal/mirror"
	"github.com/eransandler/bucketvcs/internal/storage"
)

// EngineRequest is the inputs to every entry point. Stdin is read for
// negotiation input; Stdout is the protocol response stream; Stderr is
// the side-band-2 / sshd stderr channel (HTTP discards).
type EngineRequest struct {
	Ctx    context.Context
	Tenant string
	Repo   string
	Actor  *auth.Actor

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// ProtocolVersion is 0, 1, or 2. For HTTP, derived from the
	// Git-Protocol header. For SSH, derived from the GIT_PROTOCOL env
	// passed by the client before exec.
	ProtocolVersion int

	Store  storage.ObjectStore
	Mirror *mirror.Manager
}

// ErrNotImplemented is returned by stubs until Phase 1 ports the M3 logic.
var ErrNotImplemented = errors.New("uploadpack: not implemented")

// Advertise writes the upload-pack ref/capability advertisement to req.Stdout.
func Advertise(req *EngineRequest) error { return ErrNotImplemented }

// Service runs the negotiation/pack-streaming loop reading req.Stdin
// and writing to req.Stdout.
func Service(req *EngineRequest) error { return ErrNotImplemented }

// Serve runs Advertise followed by Service on the same request.
func Serve(req *EngineRequest) error {
	if err := Advertise(req); err != nil {
		return err
	}
	return Service(req)
}
```

- [ ] **Step 3: Write a compile-check test**

```go
package uploadpack

import "testing"

func TestStubsCompile(t *testing.T) {
	if err := Advertise(&EngineRequest{}); err == nil {
		t.Fatal("expected ErrNotImplemented")
	}
	if err := Service(&EngineRequest{}); err == nil {
		t.Fatal("expected ErrNotImplemented")
	}
}
```

- [ ] **Step 4: Verify build + test**

```bash
go build ./... && go test ./internal/gitproto/uploadpack/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitproto/uploadpack/
git commit -m "M6 task 1: gitproto/uploadpack package skeleton"
```

### Task 2: `internal/gitproto/receivepack` skeleton + `EngineRequest`

**Files:**
- Create: `internal/gitproto/receivepack/doc.go`
- Create: `internal/gitproto/receivepack/engine.go`
- Create: `internal/gitproto/receivepack/engine_test.go`

- [ ] **Step 1: Write `doc.go`** — same shape as uploadpack/doc.go but for receive-pack.

- [ ] **Step 2: Write `engine.go`** — identical structure to Task 1 step 2, package name `receivepack`. The struct is also called `EngineRequest`; using the package name disambiguates `uploadpack.EngineRequest` from `receivepack.EngineRequest`.

```go
package receivepack

import (
	"context"
	"errors"
	"io"

	"github.com/eransandler/bucketvcs/internal/auth"
	"github.com/eransandler/bucketvcs/internal/mirror"
	"github.com/eransandler/bucketvcs/internal/storage"
)

type EngineRequest struct {
	Ctx    context.Context
	Tenant string
	Repo   string
	Actor  *auth.Actor

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	ProtocolVersion int

	Store  storage.ObjectStore
	Mirror *mirror.Manager
}

var ErrNotImplemented = errors.New("receivepack: not implemented")

func Advertise(req *EngineRequest) error { return ErrNotImplemented }
func Service(req *EngineRequest) error   { return ErrNotImplemented }

func Serve(req *EngineRequest) error {
	if err := Advertise(req); err != nil {
		return err
	}
	return Service(req)
}
```

- [ ] **Step 3: Compile-check test** — same shape as Task 1 step 3.

- [ ] **Step 4: Build + test, then commit**

```bash
go build ./... && go test ./internal/gitproto/...
git add internal/gitproto/receivepack/
git commit -m "M6 task 2: gitproto/receivepack package skeleton"
```

### Task 3: Port upload-pack advertisement (V0 + V2) into `uploadpack.Advertise`

The existing M3 V0 advertisement lives in `internal/gateway/inforefs.go` (`writeV0UploadPackAdvertisement`, `uploadPackV0Caps`) and the V2 capability advertisement code lives in `internal/gateway/upload_pack.go` (top of `handleUploadPack` and `handleFetch`). Move the body-producing logic into `uploadpack/advertise.go`.

**Files:**
- Create: `internal/gitproto/uploadpack/advertise.go`
- Modify: `internal/gateway/inforefs.go:69-186`
- Test: `internal/gitproto/uploadpack/engine_test.go`

- [ ] **Step 1: Read both source files**

Read `internal/gateway/inforefs.go` and the first ~120 lines of `internal/gateway/upload_pack.go` to internalize the exact byte sequences M3 produces. Pay attention to:
- v0 capability list construction (`uploadPackV0Caps`)
- ref ordering
- pkt-line framing including flush packets
- `version=2` capability response shape

- [ ] **Step 2: Write a byte-pinning test that initially fails**

The test runs `Advertise` against a synthetic manifest (constructed in-memory via the test helpers in `internal/repo/manifest`) and compares stdout bytes to a golden vector.

```go
// engine_test.go (new test, append to file)
func TestAdvertise_V0_GoldenBytes(t *testing.T) {
	st, mgr, manifestBody := newFixtureRepo(t, "fixture-basic")
	defer st.Close()

	var out bytes.Buffer
	req := &EngineRequest{
		Ctx:             context.Background(),
		Tenant:          "acme",
		Repo:            "web",
		Stdout:          &out,
		ProtocolVersion: 0,
		Store:           st,
		Mirror:          mgr,
	}
	if err := Advertise(req); err != nil {
		t.Fatalf("Advertise: %v", err)
	}

	got := out.Bytes()
	want := mustReadGolden(t, "testdata/advertise_v0_basic.bin")
	if !bytes.Equal(got, want) {
		t.Fatalf("v0 advertise bytes diverged from M3 golden\n got: %q\nwant: %q", got, want)
	}
	_ = manifestBody
}

func TestAdvertise_V2_GoldenBytes(t *testing.T) {
	// Same but ProtocolVersion: 2 and want = testdata/advertise_v2_basic.bin
}
```

The `newFixtureRepo` helper and `mustReadGolden` helper are written as part of this step (in `engine_test.go`). The fixture data and golden vectors come from M3 — copy the existing M3 fixture inputs and capture current M3 outputs as the goldens with a one-shot helper:

```bash
# One-time golden capture from current M3 behavior, before any refactor.
# Run a small program that wires up the exact synthetic manifest and dumps
# what writeV0UploadPackAdvertisement produces today. Save bytes to:
mkdir -p internal/gitproto/uploadpack/testdata
# (capture script kept in /tmp; do not commit.)
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
go test ./internal/gitproto/uploadpack/ -run TestAdvertise_V0 -v
```
Expected: FAIL — `Advertise` returns `ErrNotImplemented`.

- [ ] **Step 4: Write `advertise.go`**

```go
package uploadpack

import (
	"fmt"
	"io"
	// imports for pktline, manifest, capabilities
)

func Advertise(req *EngineRequest) error {
	switch req.ProtocolVersion {
	case 2:
		return advertiseV2(req)
	default:
		return advertiseV0(req)
	}
}

func advertiseV0(req *EngineRequest) error {
	// Port body of writeV0UploadPackAdvertisement from
	// internal/gateway/inforefs.go.
	// Key changes from the M3 version:
	//   - takes io.Writer (req.Stdout) not http.ResponseWriter
	//   - reads manifest via req.Store / req.Mirror; do not assume an HTTP
	//     "service=git-upload-pack" preamble line — that is HTTP-specific
	//     framing and stays in the gateway adapter.
	// ...
}

func advertiseV2(req *EngineRequest) error {
	// Port v2 capability advertisement (the part of handleUploadPack that
	// runs when wantsV2(headers) is true and the body has not started).
	// ...
}
```

The pktline writers and capability strings are unchanged from M3 — copy them verbatim. The only edit is the writer type and removing HTTP-only framing.

- [ ] **Step 5: Run tests, verify byte equality**

```bash
go test ./internal/gitproto/uploadpack/ -run TestAdvertise -v
```
Expected: PASS for both V0 and V2.

- [ ] **Step 6: Update gateway/inforefs.go to delegate**

Rewrite `handleInfoRefs` to:
1. Detect V2 via existing `wantsV2(header)`.
2. Set HTTP response headers (Content-Type, Cache-Control, "service=git-upload-pack" preamble pkt-line for V0).
3. Call `uploadpack.Advertise(req)` with the right `ProtocolVersion`.

Delete `writeV0UploadPackAdvertisement`, `uploadPackV0Caps`, `wantsV2` from inforefs.go. (Keep `wantsV2` if it's used elsewhere; otherwise move to a small helper inside gateway.)

- [ ] **Step 7: Run M3 fixtures and gateway tests**

```bash
go test ./internal/gateway/... ./internal/gitproto/...
go test ./internal/diffharness/... -run UploadPack
```
Expected: PASS — including all M3 differential tests for upload-pack info/refs.

- [ ] **Step 8: Commit**

```bash
git add internal/gitproto/uploadpack/ internal/gateway/inforefs.go
git commit -m "M6 task 3: port upload-pack V0+V2 advertisement into gitproto"
```

### Task 4: Port upload-pack negotiation/fetch into `uploadpack.Service`

**Files:**
- Create: `internal/gitproto/uploadpack/service.go`
- Modify: `internal/gateway/upload_pack.go` (shrink to adapter)
- Test: `internal/gitproto/uploadpack/engine_test.go`

- [ ] **Step 1: Add a Service-level golden test**

```go
func TestService_V2_FetchBasicClone_GoldenBytes(t *testing.T) {
	st, mgr, _ := newFixtureRepo(t, "fixture-basic")
	defer st.Close()

	in := bytes.NewReader(mustReadGolden(t, "testdata/service_v2_basic_request.bin"))
	var out bytes.Buffer
	req := &EngineRequest{
		Ctx: context.Background(), Tenant: "acme", Repo: "web",
		Stdin: in, Stdout: &out,
		ProtocolVersion: 2,
		Store: st, Mirror: mgr,
	}
	if err := Service(req); err != nil {
		t.Fatalf("Service: %v", err)
	}
	got := out.Bytes()
	want := mustReadGolden(t, "testdata/service_v2_basic_response.bin")
	if !bytes.Equal(got, want) {
		t.Fatalf("V2 fetch response diverged from M3 golden")
	}
}
```

The request and response golden vectors are captured from current M3 behavior, same one-shot harness as Task 3 step 2.

- [ ] **Step 2: Run test, expect FAIL**

```bash
go test ./internal/gitproto/uploadpack/ -run TestService -v
```

- [ ] **Step 3: Write `service.go` by lifting M3 `handleFetch`**

The body of `handleFetch` from `internal/gateway/upload_pack.go:167` ports almost line-for-line. Replace `r.Body` with `req.Stdin`, replace the `http.ResponseWriter` writes with `req.Stdout`. Remove HTTP-only error responses; return Go errors from `Service` and let the adapter map them to HTTP. The pkt-line drain helper `drainPktLine` moves into `service.go` (it has no HTTP coupling).

```go
package uploadpack

func Service(req *EngineRequest) error {
	switch req.ProtocolVersion {
	case 2:
		return serviceV2(req)
	default:
		return serviceV0(req)
	}
}

func serviceV2(req *EngineRequest) error {
	// Port handleFetch from internal/gateway/upload_pack.go (lines 167–405).
	// Reads from req.Stdin, writes to req.Stdout.
	// ...
}

func serviceV0(req *EngineRequest) error {
	// If M3 supports V0 POST upload-pack, port that branch. If M3 only
	// implements V0 advertisement and rejects V0 negotiation, mirror that
	// rejection here as a returned error. Verify by inspecting current
	// upload_pack.go behavior before writing.
	// ...
}
```

- [ ] **Step 4: Verify Service test passes**

```bash
go test ./internal/gitproto/uploadpack/ -v
```

- [ ] **Step 5: Rewrite `internal/gateway/upload_pack.go` as adapter**

```go
package gateway

import (
	"net/http"

	"github.com/eransandler/bucketvcs/internal/gitproto/uploadpack"
)

func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	pv := 0
	if r.Header.Get("Git-Protocol") == "version=2" {
		pv = 2
	}
	req := &uploadpack.EngineRequest{
		Ctx:             r.Context(),
		Tenant:          tenant,
		Repo:            repoID,
		Actor:           actorFromCtx(r.Context()),
		Stdin:           r.Body,
		Stdout:          w,
		Stderr:          io.Discard,
		ProtocolVersion: pv,
		Store:           s.store,
		Mirror:          s.mirror,
	}
	if err := uploadpack.Service(req); err != nil {
		s.logger.Error("upload-pack service", "err", err, "tenant", tenant, "repo", repoID)
		// Body has already started streaming in normal flows; only emit
		// 500 if no bytes have been written. Mirror M3 handler behavior.
	}
}
```

`dedupOIDs` and other helpers in upload_pack.go that the engine uses move into `service.go`. The gateway file shrinks to ~40 lines.

- [ ] **Step 6: Run all gateway + diff harness tests**

```bash
go test ./internal/gateway/... ./internal/gitproto/... ./internal/diffharness/...
```
Expected: every M3 test passes unchanged.

- [ ] **Step 7: Commit**

```bash
git add internal/gitproto/uploadpack/ internal/gateway/upload_pack.go
git commit -m "M6 task 4: port upload-pack service into gitproto, gateway becomes adapter"
```

### Task 5: Port receive-pack advertisement into `receivepack.Advertise`

Symmetric to Task 3 but for receive-pack. The current code lives in `internal/gateway/inforefs.go:127-186` (`writeV0ReceivePackAdvertisement`, `receivePackV0Caps`).

**Files:**
- Create: `internal/gitproto/receivepack/advertise.go`
- Modify: `internal/gateway/inforefs.go`
- Test: `internal/gitproto/receivepack/engine_test.go`

- [ ] **Step 1: Add receive-pack advertisement golden test (V0 only — receive-pack does not implement V2)**

Golden file: `internal/gitproto/receivepack/testdata/advertise_v0_basic.bin`. Captured from current M3 behavior with the same harness used in Task 3.

- [ ] **Step 2: Run test, expect FAIL**

```bash
go test ./internal/gitproto/receivepack/ -run TestAdvertise -v
```

- [ ] **Step 3: Write `advertise.go`**

Port the body of `writeV0ReceivePackAdvertisement` and `receivePackV0Caps` into `receivepack/advertise.go`, taking `req.Stdout` instead of `http.ResponseWriter`.

- [ ] **Step 4: Update `gateway/inforefs.go` to delegate** for the receive-pack branch.

- [ ] **Step 5: Run all tests**

```bash
go test ./internal/gateway/... ./internal/gitproto/...
```

- [ ] **Step 6: Commit**

```bash
git add internal/gitproto/receivepack/ internal/gateway/inforefs.go
git commit -m "M6 task 5: port receive-pack advertisement into gitproto"
```

### Task 6: Port receive-pack service (push pipeline) into `receivepack.Service`

**Files:**
- Create: `internal/gitproto/receivepack/service.go`
- Modify: `internal/gateway/receive_pack.go` (shrink to adapter)
- Test: `internal/gitproto/receivepack/engine_test.go`

This is the largest port — `handleReceivePack` (lines 92–180) plus `completeReceivePack` (lines 182–498) plus `parseReceivePackRequest` (lines 499–671) plus helpers `validHexOID40`, `applyRefUpdateInBare`, `markMirrorStale`, `writeReceiveReport`. None of these have HTTP coupling beyond `r.Body` / `w` reads/writes, the request id from `r.Context()`, and a small set of HTTP error responses that become returned errors.

- [ ] **Step 1: Add receive-pack service golden test**

Synthesize a minimal push request (one fast-forward ref update plus a packfile with a single new blob) as a byte string, run `Service`, capture the report-status response, compare to golden.

- [ ] **Step 2: Run test, expect FAIL**

- [ ] **Step 3: Write `service.go`**

```go
package receivepack

func Service(req *EngineRequest) error {
	parsed, err := parseRequest(req.Ctx, req.Stdin, /* incoming pkt-line frame from advertise step is empty for split HTTP path; for SSH the same buffer */)
	if err != nil {
		return err
	}
	return complete(req, parsed)
}

// parseRequest, complete, applyRefUpdateInBare, validHexOID40,
// writeReceiveReport, markMirrorStale all move here verbatim from
// internal/gateway/receive_pack.go with the only edit being:
//   - r.Body  -> req.Stdin
//   - w       -> req.Stdout
//   - Server fields (s.store, s.mirror, s.logger) -> req fields
```

The pre-receive hook hook-point note from M3 receive_pack.go (if any) carries over unchanged.

- [ ] **Step 4: Verify test passes**

- [ ] **Step 5: Rewrite `internal/gateway/receive_pack.go` as adapter**

```go
package gateway

import (
	"net/http"

	"github.com/eransandler/bucketvcs/internal/gitproto/receivepack"
)

func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	req := &receivepack.EngineRequest{
		Ctx:    r.Context(),
		Tenant: tenant, Repo: repoID,
		Actor:  actorFromCtx(r.Context()),
		Stdin:  r.Body, Stdout: w, Stderr: io.Discard,
		Store:  s.store, Mirror: s.mirror,
	}
	if err := receivepack.Service(req); err != nil {
		s.logger.Error("receive-pack service", "err", err, "tenant", tenant, "repo", repoID)
	}
}
```

- [ ] **Step 6: Run the full test suite (M3 fixtures, M4 e2e, all of diffharness)**

```bash
go test ./...
```

Every existing test must still pass. If any byte-level test diverges, debug the engine port — do not adjust the test.

- [ ] **Step 7: Commit**

```bash
git add internal/gitproto/receivepack/ internal/gateway/receive_pack.go
git commit -m "M6 task 6: port receive-pack service into gitproto, gateway becomes adapter"
```

### Task 7: Phase 1 gate — full diff harness + roborev pass

- [ ] **Step 1: Run the full differential harness with cloud creds where applicable**

```bash
go test ./internal/diffharness/...
go test -tags=stress ./internal/gateway/...
```

- [ ] **Step 2: Confirm M3's "61 pass + 3 documented skips" is preserved**

The numbers are recorded in `m3_progress.md` memory. Any new failure or new skip is a refactor regression.

- [ ] **Step 3: Run roborev on the engine refactor**

Per the spec §9.8 promotion gate: the engine refactor gets its own roborev pass before any SSH code lands.

```bash
# Use roborev-review-branch via Skill tool from within the Claude session.
```

- [ ] **Step 4: Address roborev findings inline; commit fixes; re-run.**

- [ ] **Step 5: Phase 1 gate commit (no code change; explicit checkpoint)**

```bash
git commit --allow-empty -m "M6 phase 1 gate: gitproto refactor passes M3 fixtures + roborev"
```

---

## Phase 2 — `auth.Scope` additive change

After Phase 2, the auth package supports an optional scoped credential, but no SSH credentials exist yet. M4 HTTPS auth keeps working unchanged — the new `*Scope` return is always nil for `BasicPassword` credentials.

### Task 8: Define `auth.Scope` and `auth.SSHKey` types

**Files:**
- Modify: `internal/auth/types.go`

- [ ] **Step 1: Append types to `types.go`**

```go
// Scope narrows a credential to a single repo at a specific permission
// level. Returned by Store.VerifyCredential for deploy-key SSH credentials;
// nil for user keys and BasicPassword tokens.
type Scope struct {
	Tenant string
	Repo   string
	Perm   Perm // PermRead or PermWrite. PermAdmin is not allowed.
}

// SSHKey is the persisted shape of an entry in the ssh_keys table.
// Exactly one of (UserID) or (ScopeTenant + ScopeRepo + ScopePerm) is set.
type SSHKey struct {
	ID          string // bvsk_<24 base32>
	Fingerprint string // "SHA256:..." OpenSSH form
	PublicKey   []byte // raw wire-format public key bytes
	KeyType     string // "ssh-ed25519" | "ssh-rsa" | "ecdsa-..."
	Label       string

	UserID string

	ScopeTenant string
	ScopeRepo   string
	ScopePerm   Perm

	CreatedAt  int64
	LastUsedAt int64
	RevokedAt  int64
}
```

- [ ] **Step 2: Verify build**

```bash
go build ./...
```

- [ ] **Step 3: Commit**

```bash
git add internal/auth/types.go
git commit -m "M6 task 8: add auth.Scope and auth.SSHKey types"
```

### Task 9: Change `Store.VerifyCredential` signature, plumb `*Scope` through

**Files:**
- Modify: `internal/auth/store.go`
- Modify: `internal/auth/sqlitestore/store.go`
- Modify: `internal/gateway/auth.go`
- Modify: `internal/auth/conformance/conformance.go`
- Modify: every `VerifyCredential` caller and test

- [ ] **Step 1: Update interface in `internal/auth/store.go`**

Change the line:

```go
VerifyCredential(ctx context.Context, c Credential) (actor *Actor, tokenID string, err error)
```

to:

```go
// VerifyCredential validates a credential and returns:
//   - actor: the principal (synthetic for deploy keys)
//   - credentialID: tokens.id for BasicPassword, ssh_keys.id for SSHKeyFingerprint
//   - scope: nil for user creds; populated for deploy keys
VerifyCredential(ctx context.Context, c Credential) (actor *Actor, credentialID string, scope *Scope, err error)
```

- [ ] **Step 2: Update sqlitestore implementation**

In `internal/auth/sqlitestore/store.go`'s `VerifyCredential`, the existing BasicPassword branch keeps its behavior and returns `nil` for the new `*Scope`. The SSHKeyFingerprint branch is added in Task 14; for now, the type-switch falls through to `return nil, "", nil, ErrInvalidCredential`.

- [ ] **Step 3: Update conformance suite signature**

In `internal/auth/conformance/conformance.go`, every helper that calls `VerifyCredential` adds the new return slot. Existing assertions (token-id non-empty, error mapping) are unchanged. Add a one-line assertion: scope is nil for all M4 cases.

- [ ] **Step 4: Update gateway middleware**

In `internal/gateway/auth.go`, the call site:

```go
actor, tokenID, err := s.authStore.VerifyCredential(ctx, auth.BasicPassword{...})
```

becomes:

```go
actor, tokenID, scope, err := s.authStore.VerifyCredential(ctx, auth.BasicPassword{...})
```

If `scope != nil` after a `BasicPassword` credential, that's a programming error — log and treat as auth failure. (Defensive; should never trip.)

For the permission lookup, when `scope != nil`:

```go
if scope != nil {
    if scope.Tenant != routed.Tenant || scope.Repo != routed.Repo {
        write403(w, "scope mismatch"); return
    }
    perm = scope.Perm
} else {
    perm, err = s.authStore.LookupRepoPerm(ctx, actor, routed.Tenant, routed.Repo)
    // existing M4 error handling
}
```

The rest of the middleware (call to `auth.Decide(actor, perm, action, flags)`) is unchanged.

- [ ] **Step 5: Update all tests that mock `Store`**

Search for `VerifyCredential` in test files (`grep -rn VerifyCredential internal/`); update fake stores and table-driven assertions to the new arity.

- [ ] **Step 6: Run all tests**

```bash
go test ./...
```
Every existing M4 test must pass.

- [ ] **Step 7: Commit**

```bash
git add internal/auth/ internal/gateway/auth.go
git commit -m "M6 task 9: VerifyCredential returns optional *Scope; M4 paths unchanged"
```

### Task 10: `permissions_test.go` table cases for scoped credentials

**Files:**
- Modify: `internal/auth/permissions_test.go`

- [ ] **Step 1: Add a test asserting that a synthetic deploy-key actor + scope-derived perm produces the same `Decide` outcome as a real user with the equivalent grant**

```go
func TestDecide_DeployKeyScopeSymmetry(t *testing.T) {
	flags := RepoFlags{}
	cases := []struct {
		perm   Perm
		action Action
		want   bool
	}{
		{PermRead, ActionRead, true},
		{PermRead, ActionWrite, false},
		{PermWrite, ActionRead, true},
		{PermWrite, ActionWrite, true},
	}
	deployActor := &Actor{UserID: "deploy:bvsk_xyz", Name: "deploy-key:ci"}
	userActor := &Actor{UserID: "u_abc", Name: "alice"}
	for _, tc := range cases {
		gotDeploy := Decide(deployActor, tc.perm, tc.action, flags)
		gotUser := Decide(userActor, tc.perm, tc.action, flags)
		if gotDeploy != gotUser || gotDeploy != tc.want {
			t.Fatalf("perm=%v action=%v deploy=%v user=%v want=%v",
				tc.perm, tc.action, gotDeploy, gotUser, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run test, verify pass**

```bash
go test ./internal/auth/ -run Decide -v
```

- [ ] **Step 3: Commit**

```bash
git add internal/auth/permissions_test.go
git commit -m "M6 task 10: Decide symmetry test for deploy-key actors"
```

### Task 11: Gateway middleware integration test for scope mismatch

**Files:**
- Modify: `internal/gateway/auth_test.go`

- [ ] **Step 1: Add a test using a fake `Store` that returns a `*Scope` for `acme/web` but the request URL is `acme/other`**

```go
func TestAuthMiddleware_ScopeMismatch(t *testing.T) {
	store := &fakeStore{
		verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
			return &auth.Actor{UserID: "deploy:k1"}, "k1",
				&auth.Scope{Tenant: "acme", Repo: "web", Perm: auth.PermWrite}, nil
		},
		flags: func(t, r string) (auth.RepoFlags, error) {
			return auth.RepoFlags{}, nil
		},
	}
	// Build a request to /acme/other.git/info/refs?service=git-receive-pack
	// with valid Basic auth. Assert 403 and a "scope mismatch" body line.
}
```

The `fakeStore` shape lives next to existing M4 fakes. If a fake doesn't already support a returned `*Scope`, extend it.

- [ ] **Step 2: Run test, verify pass.**

- [ ] **Step 3: Commit**

```bash
git add internal/gateway/auth_test.go
git commit -m "M6 task 11: gateway scope-mismatch denies 403"
```

---

## Phase 3 — `ssh_keys` schema + sqlitestore CRUD

### Task 12: Migration `0002_ssh_keys.sql`

**Files:**
- Create: `internal/auth/sqlitestore/migrations/0002_ssh_keys.sql`
- Modify: `internal/auth/sqlitestore/schema.go` (if migration list is hand-maintained; verify by reading)

- [ ] **Step 1: Write the migration**

```sql
CREATE TABLE ssh_keys (
    id              TEXT PRIMARY KEY,
    fingerprint     TEXT NOT NULL UNIQUE,
    public_key      BLOB NOT NULL,
    key_type        TEXT NOT NULL,
    label           TEXT,
    created_at      INTEGER NOT NULL,
    last_used_at    INTEGER,
    revoked_at      INTEGER,

    user_id         TEXT REFERENCES users(id) ON DELETE CASCADE,
    scope_tenant    TEXT,
    scope_repo      TEXT,
    scope_perm      TEXT CHECK (scope_perm IN ('read','write')),

    CHECK (
        (user_id IS NOT NULL AND scope_tenant IS NULL
                              AND scope_repo IS NULL
                              AND scope_perm IS NULL)
        OR
        (user_id IS NULL      AND scope_tenant IS NOT NULL
                              AND scope_repo IS NOT NULL
                              AND scope_perm IS NOT NULL)
    ),
    FOREIGN KEY (scope_tenant, scope_repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);

CREATE UNIQUE INDEX ssh_keys_fingerprint_idx ON ssh_keys(fingerprint);
CREATE INDEX        ssh_keys_user_idx        ON ssh_keys(user_id);
CREATE INDEX        ssh_keys_scope_idx       ON ssh_keys(scope_tenant, scope_repo);
```

- [ ] **Step 2: Verify the schema runner picks it up**

If `schema.go` uses `embed.FS` walking `migrations/*.sql` in lexical order, this should be automatic. Open `internal/auth/sqlitestore/schema.go` and confirm. If migrations are listed by hand, append `"0002_ssh_keys.sql"`.

- [ ] **Step 3: Add a schema test**

```go
// internal/auth/sqlitestore/schema_test.go (extend existing if present)
func TestMigrate_AppliesV2(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, filepath.Join(dir, "test.db"))
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='ssh_keys'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ssh_keys table missing")
	}
}
```

- [ ] **Step 4: Run schema tests**

```bash
go test ./internal/auth/sqlitestore/ -run Migrate -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/migrations/0002_ssh_keys.sql internal/auth/sqlitestore/schema_test.go
git commit -m "M6 task 12: ssh_keys schema migration"
```

### Task 13: `Store.AddSSHKey` (user keys + deploy keys)

**Files:**
- Modify: `internal/auth/store.go` (extend interface)
- Modify: `internal/auth/sqlitestore/store.go` (implement)
- Create: `internal/auth/sqlitestore/sshkeys_test.go`

- [ ] **Step 1: Extend the interface**

```go
// Append to auth.Store interface in internal/auth/store.go:

// AddSSHKey persists an ssh_keys row. The caller computes ID and
// Fingerprint. Returns ErrDuplicateFingerprint if the fingerprint is
// already known (any user, any scope).
AddSSHKey(ctx context.Context, k SSHKey) error

// ListSSHKeysForUser returns user keys for a userID, including revoked.
ListSSHKeysForUser(ctx context.Context, userID string) ([]SSHKey, error)

// ListSSHKeysForRepo returns deploy keys bound to (tenant, repo),
// including revoked.
ListSSHKeysForRepo(ctx context.Context, tenant, repo string) ([]SSHKey, error)

// RevokeSSHKey sets revoked_at to now for the matching key.
// keyIDOrPrefix accepts a full ID or any unique ID prefix.
RevokeSSHKey(ctx context.Context, keyIDOrPrefix string) error

// TouchSSHKeyUsage updates last_used_at; best-effort, missing key is no-op.
TouchSSHKeyUsage(ctx context.Context, keyID string) error
```

Add `ErrDuplicateFingerprint` to `internal/auth/errors.go`.

- [ ] **Step 2: Write `AddSSHKey` failing tests**

```go
// internal/auth/sqlitestore/sshkeys_test.go
func TestAddSSHKey_UserKey(t *testing.T) { /* insert, lookup by fp, expect equal */ }
func TestAddSSHKey_DeployKey(t *testing.T) { /* same for deploy */ }
func TestAddSSHKey_RejectsBothScopes(t *testing.T) { /* CHECK violation -> typed error */ }
func TestAddSSHKey_RejectsNeitherScope(t *testing.T) { /* CHECK violation -> typed error */ }
func TestAddSSHKey_DuplicateFingerprint(t *testing.T) { /* second insert -> ErrDuplicateFingerprint */ }
```

- [ ] **Step 3: Run tests, expect FAIL**

- [ ] **Step 4: Implement `AddSSHKey` in `internal/auth/sqlitestore/store.go`**

INSERT statement, parameter binding, UNIQUE constraint violation mapped to `ErrDuplicateFingerprint`, CHECK violation mapped to a clear Go error.

- [ ] **Step 5: Run tests, verify pass.**

- [ ] **Step 6: Commit**

```bash
git add internal/auth/store.go internal/auth/errors.go internal/auth/sqlitestore/store.go internal/auth/sqlitestore/sshkeys_test.go
git commit -m "M6 task 13: Store.AddSSHKey + duplicate-fingerprint detection"
```

### Task 14: `Store.VerifyCredential` SSHKeyFingerprint branch

**Files:**
- Modify: `internal/auth/sqlitestore/store.go`
- Modify: `internal/auth/sqlitestore/sshkeys_test.go`

- [ ] **Step 1: Write tests for SSHKeyFingerprint verify**

```go
func TestVerifyCredential_UserKey_Success(t *testing.T) {
    // insert user "alice", insert user key with fp F
    // call VerifyCredential(SSHKeyFingerprint{F})
    // expect actor.UserID == alice, scope == nil, credentialID == ssh_keys.id
}
func TestVerifyCredential_DeployKey_Success(t *testing.T) {
    // register repo acme/web, insert deploy key fp F perm=write
    // expect actor.UserID == "deploy:<keyID>", scope == {acme, web, write}
}
func TestVerifyCredential_RevokedSSHKey(t *testing.T)        { /* expect ErrTokenRevoked */ }
func TestVerifyCredential_DisabledUserSSHKey(t *testing.T)   { /* expect ErrUserDisabled */ }
func TestVerifyCredential_UnknownFingerprint(t *testing.T)   { /* expect ErrInvalidCredential */ }
```

- [ ] **Step 2: Run tests, expect FAIL**

- [ ] **Step 3: Implement the SSHKeyFingerprint branch**

In the `VerifyCredential` switch:

```go
case auth.SSHKeyFingerprint:
    return s.verifySSHKey(ctx, c.Fingerprint)
```

```go
func (s *Store) verifySSHKey(ctx context.Context, fp string) (*auth.Actor, string, *auth.Scope, error) {
    row := s.db.QueryRowContext(ctx, `
        SELECT k.id, k.user_id, k.scope_tenant, k.scope_repo, k.scope_perm,
               k.revoked_at, k.label,
               u.id, u.name, u.is_admin, u.disabled_at
        FROM ssh_keys k
        LEFT JOIN users u ON u.id = k.user_id
        WHERE k.fingerprint = ?
    `, fp)
    // scan; map errors:
    //   sql.ErrNoRows           -> ErrInvalidCredential
    //   revoked_at IS NOT NULL  -> ErrTokenRevoked
    //   disabled_at IS NOT NULL -> ErrUserDisabled
    // Return:
    //   user key:   actor=&Actor{UserID:u.id, Name:u.name, IsAdmin:u.is_admin}, scope=nil
    //   deploy key: actor=&Actor{UserID:"deploy:"+k.id, Name:"deploy-key:"+label},
    //               scope=&Scope{Tenant:k.scope_tenant, Repo:k.scope_repo, Perm:permFromText(k.scope_perm)}
}
```

- [ ] **Step 4: Run tests, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/store.go internal/auth/sqlitestore/sshkeys_test.go
git commit -m "M6 task 14: VerifyCredential branch for SSHKeyFingerprint"
```

### Task 15: List, Revoke, Touch — sqlitestore implementations

**Files:**
- Modify: `internal/auth/sqlitestore/store.go`
- Modify: `internal/auth/sqlitestore/sshkeys_test.go`

- [ ] **Step 1: Write tests for ListSSHKeysForUser, ListSSHKeysForRepo, RevokeSSHKey (incl. unique-prefix lookup), TouchSSHKeyUsage idempotence on missing id, cascade on user delete, cascade on repo delete.**

- [ ] **Step 2: Run tests, expect FAIL.**

- [ ] **Step 3: Implement methods.** RevokeSSHKey accepts a prefix: `WHERE id LIKE ? || '%'`; verify exactly one match before updating, else error.

- [ ] **Step 4: Run tests, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/store.go internal/auth/sqlitestore/sshkeys_test.go
git commit -m "M6 task 15: List/Revoke/Touch SSH key sqlitestore implementations"
```

### Task 16: Conformance suite tests #14–#22

**Files:**
- Create: `internal/auth/conformance/sshkeys.go`
- Modify: `internal/auth/conformance/conformance.go` (register the new tests)

- [ ] **Step 1: Implement the nine conformance tests listed in spec §9.6**

Each test takes the same `Store` setup pattern as the M4 tests #1–#13. The tests are portable so any future Postgres-backed Store will run them.

```go
// internal/auth/conformance/sshkeys.go
package conformance

import "testing"

func RunSSHKeyTests(t *testing.T, newStore func(t *testing.T) auth.Store) {
    t.Run("AddSSHKey rejects duplicate fingerprint", func(t *testing.T) { /* ... */ })
    t.Run("AddSSHKey rejects key with no user_id and no scope", func(t *testing.T) { /* ... */ })
    t.Run("AddSSHKey rejects key with both user_id and scope", func(t *testing.T) { /* ... */ })
    t.Run("VerifyCredential SSH user key returns actor, nil scope", func(t *testing.T) { /* ... */ })
    t.Run("VerifyCredential SSH deploy key returns synthetic actor + scope", func(t *testing.T) { /* ... */ })
    t.Run("VerifyCredential rejects revoked SSH key", func(t *testing.T) { /* ... */ })
    t.Run("VerifyCredential rejects SSH key for disabled user", func(t *testing.T) { /* ... */ })
    t.Run("RevokeSSHKey idempotent and accepts unique id prefix", func(t *testing.T) { /* ... */ })
    t.Run("Cascade: user delete removes user keys; repo delete removes deploy keys", func(t *testing.T) { /* ... */ })
}
```

- [ ] **Step 2: Wire into the existing `RunAll` (or equivalent) entry point**

```go
func RunAll(t *testing.T, newStore func(t *testing.T) auth.Store) {
    RunCoreTests(t, newStore)
    RunSSHKeyTests(t, newStore)
}
```

- [ ] **Step 3: Run conformance suite via the sqlitestore conformance test**

```bash
go test ./internal/auth/sqlitestore/ -run Conformance -v
```

- [ ] **Step 4: Commit**

```bash
git add internal/auth/conformance/
git commit -m "M6 task 16: conformance suite tests #14-#22 for ssh_keys"
```

---

## Phase 4 — `internal/sshd` primitives

### Task 17: `sshd` package skeleton + `fingerprint.go`

**Files:**
- Create: `internal/sshd/doc.go`
- Create: `internal/sshd/fingerprint.go`
- Create: `internal/sshd/fingerprint_test.go`

- [ ] **Step 1: Write `doc.go`**

```go
// Package sshd implements the bucketvcs SSH gateway. It wraps
// golang.org/x/crypto/ssh, parses Git exec commands, authenticates via
// public key against an auth.Store, and dispatches to the gitproto
// engines.
//
// The package has no HTTP, no SQL, and no manifest imports. Authentication
// goes through the auth.Store seam; protocol work goes through gitproto.
package sshd
```

- [ ] **Step 2: Write `fingerprint.go`**

```go
package sshd

import (
	"crypto/sha256"
	"encoding/base64"

	"golang.org/x/crypto/ssh"
)

// SHA256Fingerprint computes the OpenSSH-style SHA256 fingerprint:
//   "SHA256:" + base64(sha256(wire_pubkey))   (no padding)
func SHA256Fingerprint(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}
```

- [ ] **Step 3: Write tests with pinned vectors**

Use three fixture public keys (ed25519, rsa-2048, ecdsa-p256) generated with `ssh-keygen` and stored under `internal/sshd/testdata/`. Commit the public-key files. Use `ssh-keygen -lf` to capture the expected fingerprints once and pin them as string constants in the test.

```go
func TestSHA256Fingerprint_Ed25519(t *testing.T) {
	pub := mustReadAuthorizedKey(t, "testdata/ed25519.pub")
	got := SHA256Fingerprint(pub)
	want := "SHA256:..." // captured from ssh-keygen -lf testdata/ed25519.pub
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
```

- [ ] **Step 4: Run tests, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add internal/sshd/ internal/sshd/testdata/
git commit -m "M6 task 17: sshd package skeleton + fingerprint helper"
```

### Task 18: `sshd/hostkey.go` — load-or-generate ed25519 host key

**Files:**
- Create: `internal/sshd/hostkey.go`
- Create: `internal/sshd/hostkey_test.go`

- [ ] **Step 1: Write tests**

```go
func TestLoadOrGenerateHostKey_Generates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host_key")
	signer, err := LoadOrGenerateHostKey(path, slogtest.New(t))
	if err != nil { t.Fatal(err) }
	if signer == nil { t.Fatal("nil signer") }

	// File exists with mode 0600
	st, err := os.Stat(path)
	if err != nil { t.Fatal(err) }
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestLoadOrGenerateHostKey_LoadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host_key")
	first, _ := LoadOrGenerateHostKey(path, slogtest.New(t))
	second, _ := LoadOrGenerateHostKey(path, slogtest.New(t))
	if !bytes.Equal(first.PublicKey().Marshal(), second.PublicKey().Marshal()) {
		t.Fatal("host key changed across loads")
	}
}

func TestLoadOrGenerateHostKey_LooseMode(t *testing.T) {
	// Write a file with mode 0644, expect log warning, no error.
}
```

- [ ] **Step 2: Run tests, expect FAIL.**

- [ ] **Step 3: Implement**

```go
package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"log/slog"
	"os"

	"golang.org/x/crypto/ssh"
)

func LoadOrGenerateHostKey(path string, logger *slog.Logger) (ssh.Signer, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0o600 {
			logger.Warn("ssh host key file mode is permissive", "path", path, "mode", st.Mode().Perm())
		}
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			return nil, err
		}
		logger.Info("loaded ssh host key", "path", path,
			"fingerprint", SHA256Fingerprint(signer.PublicKey()))
		return signer, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Generate fresh ed25519 key
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	pemBytes, err := marshalEd25519PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}
	logger.Info("generated ssh host key", "path", path,
		"fingerprint", SHA256Fingerprint(signer.PublicKey()))
	return signer, nil
}

// marshalEd25519PrivateKey produces the OpenSSH "ssh-ed25519" PEM bytes.
// Use ssh.MarshalPrivateKey if available in the target x/crypto/ssh version;
// otherwise the OpenSSH key format helper from x/crypto/ssh.
func marshalEd25519PrivateKey(priv ed25519.PrivateKey) ([]byte, error) {
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}
```

If the x/crypto/ssh version in the project does not expose `ssh.MarshalPrivateKey`, the fallback is to construct the OpenSSH key format manually — verify by checking `go list -m golang.org/x/crypto` and reading the godoc.

- [ ] **Step 4: Run tests, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add internal/sshd/hostkey.go internal/sshd/hostkey_test.go
git commit -m "M6 task 18: ed25519 host key load-or-generate"
```

### Task 19: `sshd/command.go` — exec-command parser

**Files:**
- Create: `internal/sshd/command.go`
- Create: `internal/sshd/command_test.go`

- [ ] **Step 1: Write tests** covering every accepted form and every rejection from spec §6.4.

```go
func TestParseExecCommand(t *testing.T) {
	cases := []struct {
		in     string
		op     ExecOp
		tenant string
		repo   string
		err    string
	}{
		{`git-upload-pack 'acme/web.git'`, OpUpload, "acme", "web", ""},
		{`git-upload-pack "acme/web.git"`, OpUpload, "acme", "web", ""},
		{`git-upload-pack acme/web.git`, OpUpload, "acme", "web", ""},
		{`git-upload-pack /acme/web.git`, OpUpload, "acme", "web", ""},
		{`git-receive-pack 'acme/web.git'`, OpReceive, "acme", "web", ""},
		// rejections
		{`git-upload-archive 'acme/web.git'`, 0, "", "", "command not allowed"},
		{`bash`, 0, "", "", "command not allowed"},
		{`git-upload-pack 'acme/web'`, 0, "", "", "missing .git suffix"},
		{`git-upload-pack '../web.git'`, 0, "", "", "invalid path"},
		{`git-upload-pack 'acme/web.git'extra`, 0, "", "", "trailing garbage"},
		{`git-upload-pack 'acme"/web.git'`, 0, "", "", "mixed quotes"},
		{`git-upload-pack acme%2fweb.git`, 0, "", "", "invalid path"},
		{`git-upload-pack a/b/c.git`, 0, "", "", "invalid path"},
		{"git-upload-pack 'acme/\x00web.git'", 0, "", "", "invalid path"},
		{``, 0, "", "", "empty command"},
	}
	for _, tc := range cases { /* run; assert */ }
}
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement**

```go
package sshd

import (
	"errors"
	"fmt"
	"strings"
)

type ExecOp int

const (
	OpUpload ExecOp = iota + 1
	OpReceive
)

func (o ExecOp) RequiredAction() auth.Action {
	if o == OpReceive {
		return auth.ActionWrite
	}
	return auth.ActionRead
}

type ExecCommand struct {
	Op     ExecOp
	Tenant string
	Repo   string
}

func ParseExecCommand(s string) (*ExecCommand, error) {
	if s == "" {
		return nil, errors.New("empty command")
	}
	verb, rest, ok := strings.Cut(s, " ")
	if !ok {
		return nil, errors.New("command requires an argument")
	}
	var op ExecOp
	switch verb {
	case "git-upload-pack":  op = OpUpload
	case "git-receive-pack": op = OpReceive
	default:                 return nil, errors.New("command not allowed")
	}
	arg, err := stripQuotes(rest)
	if err != nil {
		return nil, err
	}
	tenant, repo, err := normalizeTenantRepo(arg)
	if err != nil {
		return nil, err
	}
	return &ExecCommand{Op: op, Tenant: tenant, Repo: repo}, nil
}

func stripQuotes(s string) (string, error) {
	// Strip a single matched pair of ' or "; reject mixed/unbalanced.
	// Reject any trailing chars after the closing quote.
	// ...
}

// normalizeTenantRepo MUST share its rules with internal/gateway/routes.go's
// path normalizer. The simplest path: extract a shared helper into a small
// package internal/repo/repopath that both gateway/routes.go and sshd/command.go
// import. The helper takes "tenant/repo.git" (or "/tenant/repo.git") and
// returns (tenant, repo) or an error.
```

If extracting `internal/repo/repopath` is straightforward (as a single tested function), do it as part of this task and have `gateway/routes.go` switch over too — the spec demands bit-identical SSH and HTTP normalization rules. If it's tangled enough to be its own task, use `routes.normalizeTenantRepo` directly via a small exported wrapper.

- [ ] **Step 4: Run tests, verify pass. Run gateway/routes_test.go to confirm no regression.**

- [ ] **Step 5: Commit**

```bash
git add internal/sshd/command.go internal/sshd/command_test.go internal/repo/repopath/  # if extracted
git add internal/gateway/routes.go                                                       # if updated
git commit -m "M6 task 19: SSH exec-command parser shares path normalization with HTTP routes"
```

---

## Phase 5 — `internal/sshd` server + session

### Task 20: `sshd/server.go` skeleton + `PublicKeyCallback`

**Files:**
- Create: `internal/sshd/server.go`
- Create: `internal/sshd/server_test.go`

- [ ] **Step 1: Write `Server` type**

```go
package sshd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/eransandler/bucketvcs/internal/auth"
	"github.com/eransandler/bucketvcs/internal/mirror"
	"github.com/eransandler/bucketvcs/internal/storage"
)

type Options struct {
	Addr        string
	HostKeyPath string
	Grace       time.Duration

	Store  auth.Store
	BVStore storage.ObjectStore // bucket store
	Mirror *mirror.Manager
	Logger *slog.Logger
}

type Server struct {
	opts     Options
	config   *ssh.ServerConfig
	listener net.Listener
	mu       sync.Mutex
	closed   bool
	sessions sync.WaitGroup
}

func NewServer(opts Options) (*Server, error) {
	signer, err := LoadOrGenerateHostKey(opts.HostKeyPath, opts.Logger)
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts}
	s.config = &ssh.ServerConfig{
		MaxAuthTries:      6,
		PublicKeyCallback: s.publicKeyCallback,
		AuthLogCallback:   s.logAuthAttempt,
	}
	s.config.AddHostKey(signer)
	return s, nil
}

func (s *Server) publicKeyCallback(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	if meta.User() != "git" {
		return nil, errors.New("only the 'git' user is supported")
	}
	fp := SHA256Fingerprint(key)
	ctx := context.Background() // x/crypto/ssh has no per-callback ctx; we don't need request cancellation here
	actor, keyID, scope, err := s.opts.Store.VerifyCredential(ctx, auth.SSHKeyFingerprint{Fingerprint: fp})
	if err != nil {
		return nil, err
	}
	return &ssh.Permissions{Extensions: map[string]string{
		"actor_id":   actor.UserID,
		"actor_name": actor.Name,
		"is_admin":   boolStr(actor.IsAdmin),
		"scope":      encodeScope(scope),
		"key_id":     keyID,
	}}, nil
}

func (s *Server) logAuthAttempt(meta ssh.ConnMetadata, method string, err error) {
	if err != nil {
		s.opts.Logger.Info("ssh auth attempt", "remote", meta.RemoteAddr().String(),
			"user", meta.User(), "method", method, "result", "fail", "err", err.Error())
	} else {
		s.opts.Logger.Info("ssh auth attempt", "remote", meta.RemoteAddr().String(),
			"user", meta.User(), "method", method, "result", "ok")
	}
}
```

`encodeScope`/`decodeScope` round-trip a `*Scope` to a string (e.g., empty for nil, `"<tenant>/<repo>:<perm>"` otherwise) — write the helpers and round-trip tests.

- [ ] **Step 2: Write a unit test wiring a fake Store and asserting the callback returns sane Permissions**

```go
func TestPublicKeyCallback_UserKey(t *testing.T) { /* ... */ }
func TestPublicKeyCallback_DeployKey_EncodesScope(t *testing.T) { /* ... */ }
func TestPublicKeyCallback_RejectsNonGitUser(t *testing.T) { /* ... */ }
func TestEncodeDecodeScope_RoundTrip(t *testing.T) { /* ... */ }
```

- [ ] **Step 3: Run, verify pass.**

- [ ] **Step 4: Commit**

```bash
git add internal/sshd/server.go internal/sshd/server_test.go
git commit -m "M6 task 20: sshd Server type + PublicKeyCallback"
```

### Task 21: `sshd/session.go` — per-channel session handler

**Files:**
- Create: `internal/sshd/session.go`
- Create: `internal/sshd/session_test.go`

- [ ] **Step 1: Implement `Server.Listen` / `Server.Serve` / `handleConn` / `handleSession`** following the pseudocode in spec §6.

Key shape:

```go
func (s *Server) Listen() error {
    l, err := net.Listen("tcp", s.opts.Addr)
    if err != nil { return err }
    s.listener = l
    s.opts.Logger.Info("ssh listening", "addr", l.Addr().String())
    return nil
}

func (s *Server) Serve(ctx context.Context) error {
    go func() { <-ctx.Done(); s.listener.Close() }()
    for {
        conn, err := s.listener.Accept()
        if err != nil {
            s.mu.Lock()
            closed := s.closed
            s.mu.Unlock()
            if closed { return nil }
            return err
        }
        s.sessions.Add(1)
        go func() { defer s.sessions.Done(); s.handleConn(ctx, conn) }()
    }
}

func (s *Server) Close() error {
    s.mu.Lock()
    s.closed = true
    s.mu.Unlock()
    s.listener.Close()
    done := make(chan struct{})
    go func() { s.sessions.Wait(); close(done) }()
    select {
    case <-done:
    case <-time.After(s.opts.Grace):
        s.opts.Logger.Warn("ssh grace exceeded; closing in-flight sessions")
        // sessions hold ssh.Channels; closing the underlying tcp listener
        // already pre-disconnects new conns. In-flight ssh.Channels owned
        // by sshConns will close when sshConn.Close() is called from
        // handleConn's defer chain — gravity does the rest within a few ms.
    }
    return nil
}
```

`handleConn` and `handleSession` follow the pseudocode in spec §6.2 / §6.3 line-for-line. Where the spec says "call uploadpack.Serve / receivepack.Serve", build an `EngineRequest` with `Stdin: ch`, `Stdout: ch`, `Stderr: ch.Stderr()`, populate `ProtocolVersion` from the `GIT_PROTOCOL` env value (`version=2` → 2, anything else → 0).

- [ ] **Step 2: Write integration tests** using `golang.org/x/crypto/ssh`'s test client against an in-process `Server` (use `net.Pipe`-backed pairs or `127.0.0.1:0`).

```go
func TestSession_UploadPack_Success(t *testing.T)        { /* full happy path */ }
func TestSession_RejectsShellRequest(t *testing.T)       { /* exit non-zero */ }
func TestSession_RejectsSubsystemRequest(t *testing.T)   { /* reply false */ }
func TestSession_RejectsPtyReq(t *testing.T)
func TestSession_NonGitUserRejected(t *testing.T)
func TestSession_RevokedKeyRejected(t *testing.T)
func TestSession_UnknownExecCommandRejected(t *testing.T)
func TestSession_GitProtocolV2EnvPropagates(t *testing.T)
func TestSession_GracefulShutdown(t *testing.T)
```

`TestSession_UploadPack_Success` uses the same fixture repo as the gitproto tests; the assertion is that the pack bytes returned over SSH equal the bytes returned over a direct `uploadpack.Serve` call against the same Stdin payload.

- [ ] **Step 3: Run tests, debug, iterate.**

- [ ] **Step 4: Commit**

```bash
git add internal/sshd/session.go internal/sshd/session_test.go
git commit -m "M6 task 21: SSH session handler + lifecycle"
```

---

## Phase 6 — Wire SSH into `bucketvcs serve`

### Task 22: `cmd/bucketvcs/serve.go` — flags + lifecycle

**Files:**
- Modify: `cmd/bucketvcs/serve.go`

- [ ] **Step 1: Add the new flags**

```go
sshAddr := fs.String("ssh-addr", "", "SSH listen address, e.g. 127.0.0.1:2222 (empty disables SSH)")
sshHostKey := fs.String("ssh-host-key", "", "path to SSH host key (default: $XDG_STATE_HOME/bucketvcs/ssh_host_ed25519_key)")
sshGrace := fs.Duration("ssh-grace", 10*time.Second, "graceful shutdown deadline for in-flight SSH sessions")
```

- [ ] **Step 2: Resolve the host-key path with the same XDG resolver used for `--auth-db`**

If `--ssh-host-key` is empty, fall back to `<state-dir>/bucketvcs/ssh_host_ed25519_key`. Use the existing resolver from M4 (look in `cmd/bucketvcs/authdb.go` or similar — find the function and reuse it).

- [ ] **Step 3: Validate flag combinations**

```go
if *addr == "" && *sshAddr == "" {
    return errors.New("at least one of --addr or --ssh-addr must be set")
}
```

- [ ] **Step 4: Start the SSH server alongside the HTTP server**

```go
var sshServer *sshd.Server
if *sshAddr != "" {
    sshOpts := sshd.Options{
        Addr:        *sshAddr,
        HostKeyPath: hostKeyPath,
        Grace:       *sshGrace,
        Store:       authStore,
        BVStore:     bucketStore,
        Mirror:      mirror,
        Logger:      logger,
    }
    sshServer, err = sshd.NewServer(sshOpts)
    if err != nil { return err }
    if err := sshServer.Listen(); err != nil { return err }
    go func() {
        if err := sshServer.Serve(ctx); err != nil {
            logger.Error("ssh serve failed", "err", err)
            cancel()
        }
    }()
}
// Existing HTTP server lifecycle remains unchanged.
// On shutdown, call sshServer.Close() in addition to httpServer.Shutdown.
```

- [ ] **Step 5: Update the manual integration check**

Run a smoke clone over both transports (still without CLI commands for keys yet — bootstrap by direct SQL insert if needed for this task; CLI follows in Task 24/25):

```bash
go run ./cmd/bucketvcs serve --addr :8080 --ssh-addr :2222 --bucket-root /tmp/bucket --auth-db /tmp/bucket.db &
# expect two log lines: "http listening on :8080", "ssh listening on :2222"
```

- [ ] **Step 6: Commit**

```bash
git add cmd/bucketvcs/serve.go
git commit -m "M6 task 22: bucketvcs serve runs SSH listener alongside HTTP"
```

---

## Phase 7 — CLI

### Task 23: `bucketvcs ssh fingerprint` subcommand

**Files:**
- Create: `cmd/bucketvcs/ssh.go`
- Modify: `cmd/bucketvcs/main.go` (dispatch)

- [ ] **Step 1: Implement `bucketvcs ssh fingerprint`**

```go
func runSSHFingerprint(args []string) error {
    fs := flag.NewFlagSet("ssh fingerprint", flag.ExitOnError)
    hostKey := fs.String("ssh-host-key", "", "path to host key file")
    if err := fs.Parse(args); err != nil { return err }
    path := *hostKey
    if path == "" { path = defaultHostKeyPath() }
    raw, err := os.ReadFile(path)
    if err != nil { return err }
    signer, err := ssh.ParsePrivateKey(raw)
    if err != nil { return err }
    fp := sshd.SHA256Fingerprint(signer.PublicKey())
    fmt.Printf("%s bucketvcs host key (%s)\n", fp, signer.PublicKey().Type())
    return nil
}
```

- [ ] **Step 2: Unit test**

Generate a host key into a tempdir, run the fingerprint command, assert the output matches `ssh-keygen -lf` (or against the value computed by `SHA256Fingerprint`).

- [ ] **Step 3: Wire dispatch in `cmd/bucketvcs/main.go`**

- [ ] **Step 4: Run tests, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/ssh.go cmd/bucketvcs/main.go
git commit -m "M6 task 23: bucketvcs ssh fingerprint subcommand"
```

### Task 24: `bucketvcs user key {add,list,revoke}`

**Files:**
- Create: `cmd/bucketvcs/userkey.go`
- Modify: `cmd/bucketvcs/user.go` (route the "key" subgroup)

- [ ] **Step 1: Implement `add` (file + `--stdin`), `list` (text + `--json`), `revoke`** using `auth.Store` directly via the same DB-open helper as the M4 user/token commands.

Public key parsing:

```go
parsed, _, _, _, err := ssh.ParseAuthorizedKey(rawBytes)
if err != nil { return fmt.Errorf("not an OpenSSH public key: %w", err) }
fp := sshd.SHA256Fingerprint(parsed)
keyType := parsed.Type()
publicKeyBytes := parsed.Marshal()
```

ID generation: `bvsk_` + 24 base32 chars (Crockford alphabet, no padding) — reuse the existing token-id generator from M4 with a different prefix.

- [ ] **Step 2: Unit tests** for parse-and-insert, duplicate fingerprint detection, revoke by full id, revoke by unique prefix.

- [ ] **Step 3: Run tests, commit.**

```bash
git add cmd/bucketvcs/userkey.go cmd/bucketvcs/user.go
git commit -m "M6 task 24: bucketvcs user key add/list/revoke"
```

### Task 25: `bucketvcs repo deploy-key {add,list,revoke}`

**Files:**
- Create: `cmd/bucketvcs/deploykey.go`
- Modify: `cmd/bucketvcs/repo.go` (route the "deploy-key" subgroup)

- [ ] **Step 1: Implement** symmetric to user key, but inserts with `scope_tenant`/`scope_repo`/`scope_perm`. Refuses if repo is not registered (look up via `Store.GetRepoFlags`).

- [ ] **Step 2: Tests** including cross-tenant repo lookup, repo-not-registered error.

- [ ] **Step 3: Commit.**

```bash
git add cmd/bucketvcs/deploykey.go cmd/bucketvcs/repo.go
git commit -m "M6 task 25: bucketvcs repo deploy-key add/list/revoke"
```

---

## Phase 8 — End-to-end + differential

### Task 26: `internal/sshd/e2e_test.go` against real `git`

**Files:**
- Create: `internal/sshd/e2e_test.go`

- [ ] **Step 1: Write the e2e harness** using a real `git` binary. Reuse the helpers from `internal/gateway/e2e_auth_test.go` for fixture setup; the SSH-side delta is a generated client keypair and a `GIT_SSH_COMMAND` script:

```go
sshScript := writeTempScript(t, fmt.Sprintf(`#!/usr/bin/env bash
exec ssh -i %q -o UserKnownHostsFile=%q -o StrictHostKeyChecking=yes "$@"
`, clientKey, knownHosts))
env := append(os.Environ(), "GIT_SSH_COMMAND="+sshScript)
```

- [ ] **Step 2: Implement each scenario from spec §9.4**

Each scenario is a sub-test:

```go
t.Run("clone with valid user key", func(t *testing.T) { /* expect success */ })
t.Run("clone with revoked key", func(t *testing.T) { /* expect failure */ })
t.Run("clone with disabled-user key", func(t *testing.T) { /* expect failure */ })
t.Run("push with read-only deploy key denied", func(t *testing.T) { /* ... */ })
t.Run("push with write deploy key", func(t *testing.T) { /* ... */ })
t.Run("deploy key cross-repo denied", func(t *testing.T) { /* ... */ })
t.Run("public-read repo no key fails", func(t *testing.T) { /* documented asymmetry */ })
t.Run("force-with-lease over SSH", func(t *testing.T) { /* same outcome as HTTPS */ })
t.Run("annotated tag push", func(t *testing.T) { /* ... */ })
t.Run("ls-remote bytes equal HTTPS modulo framing", func(t *testing.T) { /* ... */ })
```

The test skip predicate is `git --version` — skip if the system has no `git` binary.

- [ ] **Step 2: Run** `go test ./internal/sshd/... -run TestE2E -v`. Iterate until green.

- [ ] **Step 3: Commit**

```bash
git add internal/sshd/e2e_test.go
git commit -m "M6 task 26: SSH e2e against real git binary"
```

### Task 27: Differential harness — SSH-vs-HTTP clone-equivalence oracle

**Files:**
- Modify: `internal/diffharness/...` (add new oracle)

- [ ] **Step 1: Identify where M3 oracles are registered** — read `internal/diffharness/` to find the registry pattern.

- [ ] **Step 2: Add `clone-equivalence-ssh-vs-http` oracle**

For every existing M3 fixture, the oracle:
1. Starts a single in-process `bucketvcs` gateway with both HTTP and SSH listeners.
2. Inserts a user with a known key + token granting read on the fixture repo.
3. Clones over HTTPS into `tmp/http-clone`.
4. Clones over SSH into `tmp/ssh-clone`.
5. Asserts `git rev-list --all --objects` produces identical sorted output for both clones.

- [ ] **Step 3: Run the diff harness; expect identical closure across all 16 fixtures.**

```bash
go test ./internal/diffharness/... -run CloneEquivalence
```

If a fixture diverges, do not paper over it with a known-divergence entry — root-cause first. Updates to the known-divergence list (`docs/known-divergences.md` if present, else the field in the design spec) MUST be PR-reviewed.

- [ ] **Step 4: Commit**

```bash
git add internal/diffharness/
git commit -m "M6 task 27: differential oracle for SSH-vs-HTTP clone equivalence"
```

### Task 28: Stress smoke (`+build stress`)

**Files:**
- Create: `internal/sshd/stress_test.go`
- Create or extend: `internal/auth/sqlitestore/stress_test.go`

- [ ] **Step 1: 200 parallel SSH clones**

```go
//go:build stress

func TestStress_200ParallelSSHClones(t *testing.T) {
    // Set up server with 200 distinct user keys, all granted read on repo R.
    // runtime.GC(); start := runtime.NumGoroutine()
    // sync.WaitGroup; 200 clones in parallel.
    // assert all succeed
    // assert all last_used_at rows updated
    // runtime.GC(); end := runtime.NumGoroutine()
    // require end-start < 5
}
```

- [ ] **Step 2: 10,000 sequential public-key callbacks against 1,000-key DB**

```go
func TestStress_10kPublicKeyCallbacks(t *testing.T) {
    // Insert 1,000 keys. Pick one fingerprint at random per iteration.
    // 10,000 iterations of Server.publicKeyCallback against a fake meta.
    // Wall clock < 5s on dev box.
}
```

- [ ] **Step 3: Run** `go test -tags=stress ./internal/sshd/... ./internal/auth/sqlitestore/...`. Iterate until pass.

- [ ] **Step 4: Commit**

```bash
git add internal/sshd/stress_test.go internal/auth/sqlitestore/stress_test.go
git commit -m "M6 task 28: stress smoke for SSH session and key lookup"
```

---

## Phase 9 — Acceptance

### Task 29: Acceptance checklist run

- [ ] **Step 1: Tick every item in spec §12 against the codebase.** Each item maps to a test or a build-time check:

```
1. §9.1–§9.6 tests pass        -> go test ./...
2. §9.7 stress smoke           -> go test -tags=stress ./...
3. SSH-vs-HTTP differential    -> go test ./internal/diffharness/... -run CloneEquivalence
4. M3 differentials unchanged  -> go test ./internal/diffharness/...  (compare to m3_progress.md numbers)
5. M4 e2e unchanged            -> go test ./internal/gateway/... -run E2EAuth
6. ssh:// clone with valid key -> e2e test (Task 26) passes
7. ssh push read-only denied   -> e2e test (Task 26) passes
8. deploy key cross-repo fails -> e2e test (Task 26) passes
9. bucketvcs ssh fingerprint   -> Task 23 test passes
10. no --addr no --ssh-addr    -> Task 22 step 3 test
11. staticcheck/vet/gofmt      -> see Step 2
12. roborev-refine             -> see Step 3
```

- [ ] **Step 2: Run static checks**

```bash
gofmt -l .                              # expect empty
go vet ./...                            # expect clean
staticcheck ./...                       # expect clean (install if needed)
```

- [ ] **Step 3: roborev-refine on the merge candidate**

Per memory `m1_review_protocol.md`: max-reasoning roborev-refine iterations until passing or diminishing returns. Address findings inline.

- [ ] **Step 4: Update `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md` if any decision diverged from the original M6 row**

The decomposition's M6 row says "SSH gateway + SSH public-key authentication, sharing the M4 authorization engine." If we land scope-credential plumbing that's worth flagging in the row, edit. Otherwise skip.

- [ ] **Step 5: Final commit**

```bash
git commit --allow-empty -m "M6 acceptance: all §12 criteria green"
```

### Task 30: Merge candidate + memory update

- [ ] **Step 1: Merge worktree into main per the M5 pattern**

```bash
cd /home/eran/work/bucketvcs
git merge --no-ff m6-ssh-gateway -m "M6 first SSH backend: merge worktree-m6-ssh-gateway"
```

- [ ] **Step 2: Tag**

```bash
git tag -a m6-complete -m "M6: SSH gateway + user keys + deploy keys"
```

- [ ] **Step 3: Write `m6_progress.md` memory**

Following the pattern of `m5_progress.md`: commit hash, tag, package list, what's NOT in M6, open residuals.

- [ ] **Step 4: Update `~/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md`** to add the M6 line.

- [ ] **Step 5: Worktree cleanup**

```bash
git worktree remove .claude/worktrees/m6-ssh-gateway
git branch -d m6-ssh-gateway
```

---

## Self-Review

**Spec coverage (each section of the design spec → task):**

- §1 Purpose, §2 What changes — captured in Goal/Architecture above.
- §3 Non-goals — implicit; no tasks attempt them.
- §4 Architecture — Tasks 1–7 (engine refactor), 17–21 (sshd package), 22 (wiring).
- §5 Data model — Task 8 (types), 12 (migration), 13–15 (sqlitestore).
- §6 SSH session lifecycle — Tasks 18 (host key), 19 (command), 20 (server), 21 (session).
- §7 CLI surface — Tasks 22 (serve flags), 23 (ssh fingerprint), 24 (user key), 25 (deploy key).
- §8 Engine refactor — Phase 1 (Tasks 1–7).
- §9.1 Unit tests — distributed across implementation tasks.
- §9.2 Engine refactor tests — Task 3 step 2, Task 4 step 1, Task 5 step 1, Task 6 step 1.
- §9.3 Integration tests — Task 21.
- §9.4 E2E against `git` — Task 26.
- §9.5 Differential harness oracle — Task 27.
- §9.6 Conformance suite extension — Task 16.
- §9.7 Stress smoke — Task 28.
- §9.8 Review protocol — Task 7 (Phase 1 gate roborev) + Task 29 step 3 (final roborev-refine).
- §10 Security — distributed (host key 0o600 in Task 18, no plaintext private keys ever).
- §11 Open questions — already decided in spec; no tasks needed.
- §12 Acceptance criteria — Task 29.
- §13 Out of scope — implicit.

No spec-level requirements left unmapped.

**Placeholder scan:** No "TODO" / "TBD" / "implement later" outside of explicit Task-N step bodies that show what the engineer should write.

**Type consistency:**
- `EngineRequest` shape consistent across uploadpack and receivepack (Tasks 1, 2, ports in 3–6).
- `auth.Scope{Tenant, Repo, Perm}` consistent across spec §5.2, Task 8, Task 9, Task 11, Task 14, Task 20.
- `auth.Store.VerifyCredential` returns `(*Actor, string, *Scope, error)` consistently from Task 9 onward.
- `bvsk_` prefix consistent in Tasks 13, 14, 15, 24.
- `ExecOp` constants (`OpUpload`, `OpReceive`) used in Task 19 only; SSH session handler in Task 21 imports them from `internal/sshd/command.go`.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-08-m6-ssh-gateway.md`.**

Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
