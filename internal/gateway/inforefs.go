package gateway

import (
	"bytes"
	"errors"
	"net/http"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
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

	// M26 replica: pushes route to the write region; refuse the
	// receive-pack advertisement up front with the pointer message.
	if s.opts.Replica != nil && service == "git-receive-pack" {
		s.replicaRefuseWrite(w)
		return
	}

	// M17 token scopes: the ref advertisement leaks branch/tag names and tip
	// OIDs, so it is gated by the same scope as the corresponding POST
	// handler. Without this check, a token authenticated but lacking
	// repo:read (e.g. an lfs:read-only token whose user has read perm on the
	// repo) could call GET /info/refs?service=git-upload-pack and pull the
	// full ref list before the POST scope check would have fired. Anonymous
	// public-read flows have a nil actor and skip the scope check; legacy
	// tokens (Scopes==0) also bypass via auth.CheckScope.
	if actor := ActorFromContext(r.Context()); actor != nil {
		required := auth.ScopeRepoRead
		op := "info/refs.upload-pack"
		if service == "git-receive-pack" {
			required = auth.ScopeRepoWrite
			op = "info/refs.receive-pack"
		}
		if err := auth.CheckScope(actor, required); err != nil {
			// M17 Task 6: audit the denial. token_id_prefix is empty
			// because Actor does not carry the token id today; operators
			// correlate via user_id + timestamp until a follow-up wires
			// the token id through.
			auth.EmitScopeDenied(r.Context(), s.logger,
				actor.UserID, "", tenant, repoID, op,
				required, actor.Scopes)
			http.Error(w, "insufficient scope: token lacks "+required.String(), http.StatusForbidden)
			return
		}
	}

	if service == "git-upload-pack" {
		if !s.replicaGateCheck(w, r, tenant, repoID) {
			return
		}
		proto := 0
		if wantsV2(r.Header.Get("Git-Protocol")) {
			proto = 2
		}

		store, err := s.resolveStore(r.Context(), tenant)
		if !s.byobOK(w, err) {
			return
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
			Store:            store,
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
			s.logger.Error("inforefs upload-pack: internal error",
				"err", err, "tenant", tenant, "repo", repoID)
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
	rpStore, err := s.resolveStore(r.Context(), tenant)
	if !s.byobOK(w, err) {
		return
	}
	rreq := &receivepack.EngineRequest{
		Ctx:          r.Context(),
		Tenant:       tenant,
		Repo:         repoID,
		Stdout:       &body,
		Store:        rpStore,
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
		s.logger.Error("inforefs receive-pack: internal error",
			"err", err, "tenant", tenant, "repo", repoID)
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
