package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

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

	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")

	if service == "git-upload-pack" && wantsV2(r.Header.Get("Git-Protocol")) {
		_ = v2proto.WriteV2Advertisement(w, service, s.opts.Version)
		return
	}

	caps := uploadPackV0Caps(s.opts.Version)
	if service == "git-receive-pack" {
		caps = receivePackV0Caps(s.opts.Version)
	}
	writeV0Advertisement(w, service, &body, caps)
}

func writeV0Advertisement(w http.ResponseWriter, service string, body *manifest.Body, caps string) {
	pw := pktline.NewWriter(w)
	_ = pw.WriteString("# service=" + service + "\n")
	_ = pw.WriteFlush()

	names := make([]string, 0, len(body.Refs))
	for n := range body.Refs {
		names = append(names, n)
	}
	sort.Strings(names)

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
