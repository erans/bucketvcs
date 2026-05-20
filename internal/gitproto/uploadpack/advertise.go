package uploadpack

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

// ErrRepoNotFound is returned by Advertise when the requested repository
// does not exist in the object store.
var ErrRepoNotFound = errors.New("uploadpack: repository not found")

// ErrInvalidName is returned by Advertise when the tenant or repository name
// is syntactically invalid. HTTP callers should map this to 400 Bad Request.
var ErrInvalidName = errors.New("uploadpack: invalid tenant or repository name")

// Advertise writes the upload-pack ref/capability advertisement to req.Stdout.
// For V0 this is the bare ref listing (no Smart-HTTP "# service=" preamble —
// that's an HTTP-specific framing the gateway adapter is responsible for).
// For V2 this delegates to v2proto.WriteV2Advertisement.
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

	if req.ProtocolVersion == 2 {
		caps := v2proto.CapsOptions{
			BundleURI: req.BundleURIEnabled,
			PackURI:   req.PackURIEnabled,
		}
		if req.SSH {
			// SSH protocol: skip the "# service=...\n" HTTP preamble.
			// git over SSH expects the capability advertisement to begin
			// directly with "version 2\n" (no Smart-HTTP service header).
			return v2proto.WriteV2AdvertisementSSH(req.Stdout, req.AgentVersion, caps)
		}
		return v2proto.WriteV2Advertisement(req.Stdout, "git-upload-pack", req.AgentVersion, caps)
	}

	view, err := r.ReadRoot(req.Ctx)
	if err != nil {
		return err
	}

	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		return fmt.Errorf("uploadpack: unmarshal body: %w", err)
	}
	k, err := keys.NewRepo(r.TenantID(), r.RepoID())
	if err != nil {
		return fmt.Errorf("uploadpack: keys: %w", err)
	}
	rs, err := refstore.New(req.Ctx, req.Store, k, &body)
	if err != nil {
		return fmt.Errorf("uploadpack: refstore: %w", err)
	}
	refs, err := rs.List(req.Ctx)
	if err != nil {
		return fmt.Errorf("uploadpack: list refs: %w", err)
	}

	return writeV0Advertisement(req.Stdout, &body, refs, req.AgentVersion)
}

// writeV0Advertisement writes the v0 "smart" upload-pack advertisement.
// When body.DefaultBranch resolves to a known ref, HEAD is advertised first
// with capabilities and a symref=HEAD:<target> attribute so v0 clients can
// determine the remote default branch.
func writeV0Advertisement(w io.Writer, body *manifest.Body, refs map[string]string, version string) error {
	pw := pktline.NewWriter(w)

	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)

	headOID, hasHead := "", false
	if body.DefaultBranch != "" {
		if oid, ok := refs[body.DefaultBranch]; ok {
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
			_ = pw.WriteString(refs[n] + " " + n + "\n")
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
		oid := refs[n]
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
