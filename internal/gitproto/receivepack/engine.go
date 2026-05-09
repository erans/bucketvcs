package receivepack

import (
	"context"
	"errors"
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

	Store  storage.ObjectStore
	Mirror *mirror.Manager
}

// ErrNotImplemented is returned by stubs until later tasks port the M3 logic.
var ErrNotImplemented = errors.New("receivepack: not implemented")

// Advertise writes the receive-pack ref/capability advertisement to req.Stdout.
func Advertise(req *EngineRequest) error { return ErrNotImplemented }

// Service runs the command-list + pack ingest + report-status loop.
func Service(req *EngineRequest) error { return ErrNotImplemented }

// Serve runs Advertise followed by Service on the same request.
func Serve(req *EngineRequest) error {
	if err := Advertise(req); err != nil {
		return err
	}
	return Service(req)
}
