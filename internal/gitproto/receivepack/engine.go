package receivepack

import (
	"context"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
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
}

// Serve runs Advertise followed by Service on the same request.
func Serve(req *EngineRequest) error {
	if err := Advertise(req); err != nil {
		return err
	}
	return Service(req)
}
