package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/gitproto/receivepack"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/shiplog"
)

// handleReceivePack implements POST /<tenant>/<repo>.git/git-receive-pack.
// It is now a thin HTTP adapter: body-cap + header setup, then delegates
// to receivepack.Service for all protocol logic.
func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	if s.opts.Replica != nil {
		s.replicaRefuseWrite(w)
		return
	}
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

	store, err := s.resolveStore(r.Context(), tenant)
	if !s.byobOK(w, err) {
		return
	}

	// Usage metering: wrap the request body so we can report the uploaded
	// packfile bytes (the push payload arrives on the body, not the
	// response). start is taken here, the point we commit to serving a push.
	cr := &countingReader{r: body}
	start := time.Now()

	req := &receivepack.EngineRequest{
		Ctx:           r.Context(),
		Tenant:        tenant,
		Repo:          repoID,
		Actor:         ActorFromContext(r.Context()),
		Stdin:         cr,
		Stdout:        w,
		Stderr:        io.Discard,
		Store:         store,
		Mirror:        s.mgr,
		AgentVersion:  s.opts.Version,
		Policy:        s.opts.Policy,
		Webhooks:      s.opts.Webhooks,
		BuildTriggers: s.opts.BuildTriggers,
		Hooks:         s.opts.Hooks,
		Logger:        s.logger,
	}
	err = receivepack.Service(req)
	// Emit the push usage event after the engine completes. The flush-only
	// probe carries no pack and is not a push attempt, so it is excluded.
	if !errors.Is(err, receivepack.ErrFlushOnlyProbe) {
		s.emitPushUsage(r.Context(), tenant, repoID, cr.n, start, err)
	}
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
		//
		// Emit the underlying error to the logger BEFORE collapsing it to
		// the generic 500 — otherwise the cause vanishes (this swallow
		// hid the importer EXDEV bug for several milestones until M20.1).
		s.logger.Error("receivepack: internal error",
			"err", err, "tenant", tenant, "repo", repoID)
		http.Error(w, "internal storage error", http.StatusInternalServerError)
	}
}

// emitPushUsage records a push usage event after the receive-pack engine
// completes. Bytes is the uploaded packfile size (counted on the request
// body). Nil-safe: when log shipping is off, s.opts.Usage is nil and this
// is a no-op.
func (s *Server) emitPushUsage(ctx context.Context, tenant, repoID string, bytes int64, start time.Time, serveErr error) {
	if s.opts.Usage == nil {
		return
	}
	status := "ok"
	if serveErr != nil {
		status = "error"
	}
	s.opts.Usage.Usage(shiplog.UsageEvent{
		Kind:       shiplog.KindPush,
		Tenant:     tenant,
		Repo:       repoID,
		Actor:      usageActor(ActorFromContext(ctx)),
		Transport:  "https",
		Bytes:      bytes,
		DurationMS: time.Since(start).Milliseconds(),
		Status:     status,
	})
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
