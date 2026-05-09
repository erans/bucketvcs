package uploadpack

import (
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/v2proto"
)

// ErrRepoNotFound is returned by Advertise when the requested repository
// does not exist in the object store.
var ErrRepoNotFound = errors.New("uploadpack: repository not found")

// Advertise writes the upload-pack ref/capability advertisement to req.Stdout.
// For protocol v2 it delegates to v2proto.WriteV2Advertisement; for v0/v1 it
// writes the "smart HTTP" v0 advertisement including the service preamble.
func Advertise(req *EngineRequest) error {
	r, err := repo.Open(req.Ctx, req.Store, req.Tenant, req.Repo)
	if err != nil {
		if errors.Is(err, repoerrs.ErrRepoNotFound) {
			return ErrRepoNotFound
		}
		return err
	}

	view, err := r.ReadRoot(req.Ctx)
	if err != nil {
		return err
	}

	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		return err
	}

	if req.ProtocolVersion == 2 {
		return v2proto.WriteV2Advertisement(req.Stdout, "git-upload-pack", req.AgentVersion)
	}

	return writeV0Advertisement(req.Stdout, &body, req.AgentVersion)
}

// writeV0Advertisement writes the v0 "smart" upload-pack advertisement.
// When body.DefaultBranch resolves to a known ref, HEAD is advertised first
// with capabilities and a symref=HEAD:<target> attribute so v0 clients can
// determine the remote default branch.
func writeV0Advertisement(w io.Writer, body *manifest.Body, version string) error {
	pw := pktline.NewWriter(w)
	_ = pw.WriteString("# service=git-upload-pack\n")
	_ = pw.WriteFlush()

	names := make([]string, 0, len(body.Refs))
	for n := range body.Refs {
		names = append(names, n)
	}
	sort.Strings(names)

	headOID, hasHead := "", false
	if body.DefaultBranch != "" {
		if oid, ok := body.Refs[body.DefaultBranch]; ok {
			headOID, hasHead = oid, true
		}
	}

	baseCaps := uploadPackV0Caps(version)
	// symref=HEAD:<default> is informational; advertise it whenever the
	// repo has a configured default branch, even if that branch is unborn
	// (target ref absent). v0 clients use it to learn the remote default
	// branch on clone/fetch.
	capsWithSymref := baseCaps
	if body.DefaultBranch != "" {
		capsWithSymref = baseCaps + " symref=HEAD:" + body.DefaultBranch
	}

	if hasHead {
		_ = pw.WriteString(headOID + " HEAD\x00" + capsWithSymref + "\n")
		for _, n := range names {
			_ = pw.WriteString(body.Refs[n] + " " + n + "\n")
		}
		_ = pw.WriteFlush()
		return nil
	}

	if len(names) == 0 {
		_ = pw.WriteString("0000000000000000000000000000000000000000 capabilities^{}\x00" + capsWithSymref + "\n")
		_ = pw.WriteFlush()
		return nil
	}

	first := true
	for _, n := range names {
		oid := body.Refs[n]
		if first {
			_ = pw.WriteString(oid + " " + n + "\x00" + capsWithSymref + "\n")
			first = false
			continue
		}
		_ = pw.WriteString(oid + " " + n + "\n")
	}
	_ = pw.WriteFlush()
	return nil
}

func uploadPackV0Caps(version string) string {
	return strings.Join([]string{
		"multi_ack_detailed",
		"no-done",
		"side-band-64k",
		"thin-pack",
		"ofs-delta",
		"agent=" + v2proto.AgentName + "/" + version,
		"object-format=sha1",
	}, " ")
}
