package uploadpack

import (
	"context"
	"io"
	"time"

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

	// SSH, when true, suppresses the "# service=...\n" HTTP preamble from
	// the v2 capability advertisement. The preamble is Smart-HTTP-specific;
	// git clients connecting over SSH expect the advertisement to begin
	// directly with "version 2\n" (or with the v0 ref listing).
	SSH bool

	// AgentVersion is the gateway's advertised agent version string, used in
	// capability advertisements (e.g. "agent=bucketvcs/0.0.0").
	AgentVersion string

	Store  storage.ObjectStore
	Mirror *mirror.Manager

	// BundleURIEnabled drives v2 capability advertisement AND the
	// command=bundle-uri dispatch arm. When false, the cap is omitted and
	// the dispatch arm returns an empty response (clients fall through to
	// standard fetch).
	BundleURIEnabled bool

	// BundleURIBuildURL mints the URL the bundle-uri response advertises.
	// Required when BundleURIEnabled is true; nil disables the feature
	// (HandleBundleURI returns an empty response and the client falls
	// through to standard fetch).
	BundleURIBuildURL func(ctx context.Context, hash, storageKey, expectedHash string) (string, error)

	// BundleWarmCommits and BundleWarmAge bound the freshness window. A
	// bundle whose tip is more than BundleWarmCommits commits behind the
	// current ref tip, OR whose generation timestamp is older than
	// BundleWarmAge, is treated as stale and not advertised. Zero values
	// disable the feature.
	BundleWarmCommits int
	BundleWarmAge     time.Duration

	// PackURIEnabled drives the packfile-uris capability advertisement
	// AND the in-fetch URI advertisement gate. Mirrors BundleURIEnabled
	// but for packs (Git protocol-v2 packfile-uris). When false, the
	// "fetch=packfile-uris" sub-feature is omitted and the in-fetch
	// gate short-circuits to "no advertisement" (clients receive only
	// the inline packfile section).
	PackURIEnabled bool

	// PackURIBuildURL mints the URL the packfile-uris response advertises.
	// Required when PackURIEnabled is true; nil disables the feature
	// (the in-fetch gate returns an empty stanza, the response falls
	// through to the inline packfile section).
	PackURIBuildURL func(ctx context.Context, hash, storageKey, expectedHash string) (string, error)
}

// Service runs the negotiation/pack-streaming loop reading req.Stdin
// and writing to req.Stdout.
func Service(req *EngineRequest) error { return serviceImpl(req) }

// Serve runs Advertise followed by Service on the same request.
func Serve(req *EngineRequest) error {
	if err := Advertise(req); err != nil {
		return err
	}
	return Service(req)
}
