package uploadpack

import (
	"context"
	"errors"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/storage"
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

	// AgentVersion is the gateway's advertised agent version string, used in
	// capability advertisements (e.g. "agent=bucketvcs/0.0.0").
	AgentVersion string

	Store  storage.ObjectStore
	Mirror *mirror.Manager
}

// ErrNotImplemented is returned by stubs until later tasks port the M3 logic.
var ErrNotImplemented = errors.New("uploadpack: not implemented")

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

