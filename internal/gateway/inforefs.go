package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitproto/uploadpack"
	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/v2proto"
)

func (s *Server) handleInfoRefs(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	service := r.URL.Query().Get("service")
	switch service {
	case "git-upload-pack", "git-receive-pack":
	default:
		http.Error(w, "unknown service", http.StatusBadRequest)
		return
	}

	if service == "git-upload-pack" {
		proto := 0
		if wantsV2(r.Header.Get("Git-Protocol")) {
			proto = 2
		}

		// Buffer the engine output so we can return HTTP errors on failure
		// without having committed response headers. For V0 we prepend the
		// Smart-HTTP service preamble (HTTP-specific framing that the
		// transport-neutral engine does not emit; SSH clients expect the ref
		// advertisement to begin immediately without it).
		var body bytes.Buffer
		req := &uploadpack.EngineRequest{
			Ctx:             r.Context(),
			Tenant:          tenant,
			Repo:            repoID,
			Stdout:          &body,
			ProtocolVersion: proto,
			Store:           s.store,
			AgentVersion:    s.opts.Version,
		}
		if err := uploadpack.Advertise(req); err != nil {
			if errors.Is(err, uploadpack.ErrRepoNotFound) {
				http.Error(w, "repository not found", http.StatusNotFound)
				return
			}
			if errors.Is(err, uploadpack.ErrInvalidName) {
				http.Error(w, "invalid tenant or repository name", http.StatusBadRequest)
				return
			}
			http.Error(w, "internal storage error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		if proto == 0 {
			// Write the Smart-HTTP service preamble before the ref advertisement.
			pw := pktline.NewWriter(w)
			_ = pw.WriteString("# service=git-upload-pack\n")
			_ = pw.WriteFlush()
		}
		_, _ = w.Write(body.Bytes())
		return
	}

	// git-receive-pack: open repo, read manifest, write v0 advertisement.
	r2, err := repo.Open(r.Context(), s.store, tenant, repoID)
	if err != nil {
		if errors.Is(err, repoerrs.ErrRepoNotFound) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, repoerrs.ErrInvalidTenantID) || errors.Is(err, repoerrs.ErrInvalidRepoID) {
			http.Error(w, "invalid tenant or repository name", http.StatusBadRequest)
			return
		}
		http.Error(w, "internal storage error", http.StatusInternalServerError)
		return
	}
	view, err := r2.ReadRoot(r.Context())
	if err != nil {
		http.Error(w, "internal storage error", http.StatusInternalServerError)
		return
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		http.Error(w, "manifest decode error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	writeV0ReceivePackAdvertisement(w, &body, s.opts.Version)
}

// writeV0ReceivePackAdvertisement writes the v0 receive-pack advertisement.
// receive-pack does not advertise HEAD (push targets are real refs).
func writeV0ReceivePackAdvertisement(w http.ResponseWriter, body *manifest.Body, version string) {
	pw := pktline.NewWriter(w)
	_ = pw.WriteString("# service=git-receive-pack\n")
	_ = pw.WriteFlush()

	names := make([]string, 0, len(body.Refs))
	for n := range body.Refs {
		names = append(names, n)
	}
	sort.Strings(names)

	caps := receivePackV0Caps(version)

	if len(names) == 0 {
		_ = pw.WriteString("0000000000000000000000000000000000000000 capabilities^{}\x00" + caps + "\n")
		_ = pw.WriteFlush()
		return
	}

	first := true
	for _, n := range names {
		oid := body.Refs[n]
		if first {
			_ = pw.WriteString(oid + " " + n + "\x00" + caps + "\n")
			first = false
			continue
		}
		_ = pw.WriteString(oid + " " + n + "\n")
	}
	_ = pw.WriteFlush()
}

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

// wantsV2 reports whether the Git-Protocol header advertises protocol v2.
// Per gitprotocol-v2(5), the header is a colon-separated list of key=value
// tokens (e.g. "version=2:other=foo"); we accept any presence of "version=2".
func wantsV2(header string) bool {
	if header == "" {
		return false
	}
	for _, tok := range strings.Split(header, ":") {
		if strings.TrimSpace(tok) == "version=2" {
			return true
		}
	}
	return false
}
