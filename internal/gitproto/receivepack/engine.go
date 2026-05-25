package receivepack

import (
	"context"
	"io"
	"log/slog"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/hooks"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/policy"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// EngineRequest is the inputs to every entry point.
type EngineRequest struct {
	Ctx    context.Context
	Tenant string
	Repo   string
	Actor  *auth.Actor

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	ProtocolVersion int

	// AgentVersion is the gateway's advertised agent version string, used in
	// capability advertisements (e.g. "agent=bucketvcs/0.0.0").
	AgentVersion string

	Store  storage.ObjectStore
	Mirror *mirror.Manager

	// Policy is OPTIONAL. When non-nil, completeReceivePack runs
	// step 8b (M14 protected-ref enforcement) between connectivity
	// check and refUpdates map build. nil means no enforcement
	// (pre-M14 behavior).
	Policy *policy.Service

	// Hooks is OPTIONAL. When non-nil, completeReceivePack runs pre-receive
	// hooks at Step 8c (after M14/M16 policy succeeds) and enqueues
	// post-receive hooks after Step 14 (markMirrorStale). nil means no hook
	// execution (pre-M20 behavior).
	Hooks *hooks.Service

	// Webhooks is OPTIONAL. When non-nil, completeReceivePack enqueues
	// EventPush after a successful BuildAndCommit (and EventPolicyRefRejected
	// after each step 8b policy rejection). nil means no enqueues.
	// Enqueue failures are logged and never affect the receive outcome
	// (fail-open).
	Webhooks *webhooks.Service

	// Logger is OPTIONAL. Used by step 8b's metric + audit emission.
	// nil falls back to slog.Default() at emission time.
	Logger *slog.Logger
}

// Serve runs Advertise followed by Service on the same request.
func Serve(req *EngineRequest) error {
	if err := Advertise(req); err != nil {
		return err
	}
	return Service(req)
}

// loggerOrDefault returns e.Logger or slog.Default() if nil. Used by
// step 8b's policy emissions.
func (e *EngineRequest) loggerOrDefault() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}
