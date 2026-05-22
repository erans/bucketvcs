package receivepack

import (
	"context"
	"io"
	"log/slog"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/policy"
	"github.com/bucketvcs/bucketvcs/internal/storage"
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
