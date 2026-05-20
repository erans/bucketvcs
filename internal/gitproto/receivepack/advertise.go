package receivepack

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/v2proto"
)

// ErrRepoNotFound is returned when the requested repository does not exist.
var ErrRepoNotFound = errors.New("receivepack: repository not found")

// ErrInvalidName is returned when the requested tenant or repository name is malformed.
var ErrInvalidName = errors.New("receivepack: invalid tenant or repository name")

// Advertise writes the receive-pack ref/capability advertisement to req.Stdout.
// Receive-pack does not implement protocol v2; ProtocolVersion is currently
// ignored (always V0). req.AgentVersion is used for the agent capability.
//
// The "# service=git-receive-pack\n" preamble pkt-line is NOT emitted here —
// the gateway adapter is responsible for that.
func Advertise(req *EngineRequest) error {
	r, err := repo.Open(req.Ctx, req.Store, req.Tenant, req.Repo)
	if err != nil {
		if errors.Is(err, repoerrs.ErrRepoNotFound) {
			return ErrRepoNotFound
		}
		if errors.Is(err, repoerrs.ErrInvalidTenantID) || errors.Is(err, repoerrs.ErrInvalidRepoID) {
			return ErrInvalidName
		}
		return err
	}
	view, err := r.ReadRoot(req.Ctx)
	if err != nil {
		return err
	}
	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		return fmt.Errorf("receivepack: unmarshal body: %w", err)
	}
	k, err := keys.NewRepo(r.TenantID(), r.RepoID())
	if err != nil {
		return fmt.Errorf("receivepack: keys: %w", err)
	}
	rs, err := refstore.New(req.Ctx, req.Store, k, &body)
	if err != nil {
		return fmt.Errorf("receivepack: refstore: %w", err)
	}
	refs, err := rs.List(req.Ctx)
	if err != nil {
		return fmt.Errorf("receivepack: list refs: %w", err)
	}
	return writeV0Advertisement(req.Stdout, refs, req.AgentVersion)
}

// writeV0Advertisement is a verbatim port of M3's writeV0ReceivePackAdvertisement
// minus the "# service=git-receive-pack" preamble (which moved to the HTTP adapter).
func writeV0Advertisement(w io.Writer, refs map[string]string, version string) error {
	pw := pktline.NewWriter(w)
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)

	caps := receivePackV0Caps(version)

	if len(names) == 0 {
		_ = pw.WriteString("0000000000000000000000000000000000000000 capabilities^{}\x00" + caps + "\n")
		_ = pw.WriteFlush()
		return nil
	}

	first := true
	for _, n := range names {
		oid := refs[n]
		if first {
			_ = pw.WriteString(oid + " " + n + "\x00" + caps + "\n")
			first = false
			continue
		}
		_ = pw.WriteString(oid + " " + n + "\n")
	}
	_ = pw.WriteFlush()
	return nil
}

// receivePackV0Caps is verbatim from M3.
func receivePackV0Caps(version string) string {
	return strings.Join([]string{
		"report-status",
		"delete-refs",
		"ofs-delta",
		"atomic",
		"side-band-64k",
		"agent=" + v2proto.AgentName + "/" + version,
		"object-format=sha1",
	}, " ")
}
