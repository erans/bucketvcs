package gateway

import (
	"errors"
	"io"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/gitproto/uploadpack"
)

// uploadPackBodyLimit caps the upload-pack POST request body. A real fetch
// command body is dominated by want/have OID lines (~50 bytes each) plus a
// handful of capability lines. With our maxWants=4096 + maxHaves=8192 caps
// the absolute worst-case legitimate body is well under 1 MiB; 4 MiB gives
// plenty of headroom for client-side capability lines and future growth
// without letting a client exhaust gateway memory through unbounded
// pkt-line padding (capability lines, duplicated args, etc.) before we
// even reach the per-OID caps in handleFetch.
const uploadPackBodyLimit = 4 << 20 // 4 MiB

// handleUploadPack serves POST /<tenant>/<repo>.git/git-upload-pack for
// protocol v2 clients. The handler requires Git-Protocol: version=2 (v0
// upload-pack POST is not supported in M3 — v0 fallback is read-only via
// info/refs only). It delegates to uploadpack.Service for protocol work.
func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	if !s.replicaGateCheck(w, r, tenant, repoID) {
		return
	}
	if !wantsV2(r.Header.Get("Git-Protocol")) {
		http.Error(w, "protocol v2 required (Git-Protocol: version=2)", http.StatusBadRequest)
		return
	}
	// M17 token scopes: when a credential authenticated the request, the token
	// must carry repo:read (legacy tokens with Scopes==0 bypass via CheckScope).
	// Anonymous public-read flows have a nil actor and skip the scope check.
	if actor := ActorFromContext(r.Context()); actor != nil {
		if err := auth.CheckScope(actor, auth.ScopeRepoRead); err != nil {
			// M17 Task 6: audit the denial. token_id_prefix is empty
			// because Actor does not carry the token id today; operators
			// correlate via user_id + timestamp until a follow-up wires
			// the token id through.
			required := auth.ScopeRepoRead
			auth.EmitScopeDenied(r.Context(), s.logger,
				actor.UserID, "", tenant, repoID, "upload-pack",
				required, actor.Scopes)
			http.Error(w, "insufficient scope: token lacks "+required.String(), http.StatusForbidden)
			return
		}
	}
	defer r.Body.Close()
	// Use the SMALLER of (a) the operator-configured global cap and
	// (b) the upload-pack-specific cap. The global cap is sized for
	// receive-pack push uploads (which carry whole packfiles); upload-pack
	// requests are command frames, not bulk data, so a tighter cap closes
	// the pre-cap allocation window the reviewer flagged.
	limit := int64(uploadPackBodyLimit)
	if s.opts.MaxBodyBytes > 0 && s.opts.MaxBodyBytes < limit {
		limit = s.opts.MaxBodyBytes
	}
	body := http.MaxBytesReader(w, r.Body, limit)

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	req := &uploadpack.EngineRequest{
		Ctx:               r.Context(),
		Tenant:            tenant,
		Repo:              repoID,
		Stdin:             body,
		Stdout:            w,
		Stderr:            io.Discard,
		ProtocolVersion:   2,
		Store:             s.store,
		Mirror:            s.mgr,
		AgentVersion:      s.opts.Version,
		BundleURIEnabled:  s.opts.BundleURIEnabled,
		BundleWarmCommits: s.opts.BundleWarmCommits,
		BundleWarmAge:     s.opts.BundleWarmAge,
	}
	if s.bundleURIBuildURL != nil {
		// Closure built once in NewServer (avoid per-request allocation).
		req.BundleURIBuildURL = s.bundleURIBuildURL
	}
	req.PackURIEnabled = s.opts.PackURIEnabled
	if s.packURIBuildURL != nil {
		req.PackURIBuildURL = s.packURIBuildURL
	}
	req.Logger = s.logger
	if err := uploadpack.Service(req); err != nil {
		// Map engine errors to HTTP statuses. Note: bytes may already
		// have been written before some failures; this matches M3.
		switch {
		case errors.Is(err, uploadpack.ErrRepoNotFound):
			http.Error(w, "repository not found", http.StatusNotFound)
		case errors.Is(err, uploadpack.ErrInvalidName):
			http.Error(w, "invalid tenant or repository name", http.StatusBadRequest)
		case errors.Is(err, uploadpack.ErrV2Required):
			http.Error(w, "protocol v2 required (Git-Protocol: version=2)", http.StatusBadRequest)
		case errors.Is(err, uploadpack.ErrBadRequest):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			// Emit the underlying error before collapsing it to the
			// generic 500 — otherwise the cause vanishes (mirrors the
			// receive-pack swallow that hid the EXDEV bug until M20.1).
			s.logger.Error("uploadpack: internal error",
				"err", err, "tenant", tenant, "repo", repoID)
			http.Error(w, "internal storage error", http.StatusInternalServerError)
		}
	}
}
