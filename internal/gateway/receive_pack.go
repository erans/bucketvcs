package gateway

import (
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/gitproto/receivepack"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
)

// handleReceivePack implements POST /<tenant>/<repo>.git/git-receive-pack.
// It is now a thin HTTP adapter: body-cap + header setup, then delegates
// to receivepack.Service for all protocol logic.
func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	// M17 token scopes: receive-pack always requires an authenticated actor
	// (RunAuth has already enforced this for write actions), but defensively
	// nil-guard anyway. Legacy tokens (Scopes==0) bypass via CheckScope.
	if actor := ActorFromContext(r.Context()); actor != nil {
		if err := auth.CheckScope(actor, auth.ScopeRepoWrite); err != nil {
			// M17 Task 6: audit the denial. token_id_prefix is empty
			// because Actor does not carry the token id today; operators
			// correlate via user_id + timestamp until a follow-up wires
			// the token id through.
			required := auth.ScopeRepoWrite
			auth.EmitScopeDenied(r.Context(), s.logger,
				actor.UserID, "", tenant, repoID, "receive-pack",
				required, actor.Scopes)
			http.Error(w, "insufficient scope: token lacks "+required.String(), http.StatusForbidden)
			return
		}
	}
	defer r.Body.Close()
	body := http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)

	req := &receivepack.EngineRequest{
		Ctx:          r.Context(),
		Tenant:       tenant,
		Repo:         repoID,
		Actor:        ActorFromContext(r.Context()),
		Stdin:        body,
		Stdout:       w,
		Stderr:       io.Discard,
		Store:        s.store,
		Mirror:       s.mgr,
		AgentVersion: s.opts.Version,
		Policy:       s.opts.Policy,
		Webhooks:     s.opts.Webhooks,
		Logger:       s.logger,
	}
	err := receivepack.Service(req)
	if err == nil {
		return
	}

	switch {
	case errors.Is(err, receivepack.ErrFlushOnlyProbe):
		// Large-request probe: respond 200 OK with an empty body so
		// the client proceeds to send the real chunked POST. The
		// content-type advertises receive-pack-result for clients
		// that may inspect it before issuing the follow-up request.
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, receivepack.ErrRepoNotFound):
		http.Error(w, "repository not found", http.StatusNotFound)
	case errors.Is(err, receivepack.ErrInvalidName):
		http.Error(w, "invalid tenant or repository name", http.StatusBadRequest)
	case errors.Is(err, receivepack.ErrBadRequest):
		// The error message carries the parse detail (e.g. "trailing bytes
		// after delete-only command list"). Emit verbatim so tests can
		// substring-match the detail.
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		// Important: bytes may already be on the wire if the engine
		// started writing. http.Error then produces a malformed response.
		// M3 has the same limitation; preserve.
		http.Error(w, "internal storage error", http.StatusInternalServerError)
	}
}

// markMirrorStale removes the mirror's manifest-version sentinel so the
// next SyncToCurrent treats the mirror as not-current and rebuilds from
// the authoritative bucket state.
//
// This function is exercised directly from receive_pack_test.go (same
// package). It matches the engine-internal markMirrorStale semantics.
func markMirrorStale(m *mirror.Mirror) {
	_ = os.Remove(m.VersionFile())
}
