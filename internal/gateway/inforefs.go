package gateway

import (
	"bytes"
	"errors"
	"net/http"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitproto/receivepack"
	"github.com/bucketvcs/bucketvcs/internal/gitproto/uploadpack"
	"github.com/bucketvcs/bucketvcs/internal/pktline"
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
			Ctx:              r.Context(),
			Tenant:           tenant,
			Repo:             repoID,
			Stdout:           &body,
			ProtocolVersion:  proto,
			Store:            s.store,
			AgentVersion:     s.opts.Version,
			BundleURIEnabled: s.opts.BundleURIEnabled,
			PackURIEnabled:   s.opts.PackURIEnabled,
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

	// git-receive-pack: delegate to the transport-neutral engine.
	// The Smart-HTTP "# service=git-receive-pack\n" preamble is HTTP-specific
	// framing that we emit here; the engine does not emit it.
	var body bytes.Buffer
	rreq := &receivepack.EngineRequest{
		Ctx:          r.Context(),
		Tenant:       tenant,
		Repo:         repoID,
		Stdout:       &body,
		Store:        s.store,
		AgentVersion: s.opts.Version,
	}
	if err := receivepack.Advertise(rreq); err != nil {
		if errors.Is(err, receivepack.ErrRepoNotFound) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, receivepack.ErrInvalidName) {
			http.Error(w, "invalid tenant or repository name", http.StatusBadRequest)
			return
		}
		http.Error(w, "internal storage error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	pw := pktline.NewWriter(w)
	_ = pw.WriteString("# service=git-receive-pack\n")
	_ = pw.WriteFlush()
	_, _ = w.Write(body.Bytes())
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
