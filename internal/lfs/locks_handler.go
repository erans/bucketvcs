// Locks handlers for the LFS Locking API (M13.3).
//
// Wire format follows the Git LFS Locking API spec:
// https://github.com/git-lfs/git-lfs/blob/main/docs/api/locking.md
//
// All handlers expect upstream middleware to have already (a) verified
// the request's credentials and (b) enforced the route's RequiredAction
// (set in internal/gateway/routes.go for OpLFSLocks*). The handler picks
// up the authed actor via deps.ActorFromContext for owner-attribution
// of newly created locks and the force-unlock policy check.
package lfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/lfs/locks"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// requireLFSContentType returns true if the request has a valid LFS
// Content-Type (or none — the batch handler's existing tolerance).
// Returns false and writes a 415 response if the type is set but mismatched.
func requireLFSContentType(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return true
	}
	mt, _, perr := mime.ParseMediaType(ct)
	if perr != nil || mt != ContentType {
		WriteError(w, http.StatusUnsupportedMediaType, "expected Content-Type: "+ContentType)
		return false
	}
	return true
}

// handleLocksCreate is POST /{tenant}/{repo}.git/info/lfs/locks.
// The route is gated on ActionWrite at the gateway layer.
func handleLocksCreate(w http.ResponseWriter, r *http.Request, deps *Deps, tenant, repo string, actor *auth.Actor, logger *slog.Logger) {
	ctx := r.Context()
	if deps.LocksStore == nil {
		emitLockCreateMetric(ctx, logger, "error")
		WriteError(w, http.StatusServiceUnavailable, "locks API not configured")
		return
	}
	if actor == nil {
		emitLockCreateMetric(ctx, logger, "error")
		w.Header().Set("WWW-Authenticate", `Basic realm="bucketvcs"`)
		WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !requireLFSContentType(w, r) {
		emitLockCreateMetric(ctx, logger, "error")
		return
	}
	var req LockRequest
	if err := decodeLockJSON(r, &req); err != nil {
		emitLockCreateMetric(ctx, logger, "error")
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Path == "" {
		emitLockCreateMetric(ctx, logger, "error")
		WriteError(w, http.StatusBadRequest, "path is required")
		return
	}
	in := locks.CreateInput{
		Tenant:      tenant,
		Repo:        repo,
		Path:        req.Path,
		OwnerUserID: actor.UserID,
		Now:         time.Now(),
	}
	if req.Ref != nil {
		in.RefName = req.Ref.Name
	}
	id, err := deps.LocksStore.Create(ctx, in)
	if err != nil {
		if errors.Is(err, locks.ErrAlreadyLocked) {
			// Per LFS spec: 409 conflict body MUST carry the existing
			// lock. Look up the conflicting record so the client can
			// surface its owner.
			existing, _, lerr := deps.LocksStore.List(ctx, tenant, repo, locks.ListOptions{Path: req.Path, Limit: 1})
			if lerr != nil || len(existing) == 0 {
				// The lookup itself failed, or some concurrent caller
				// deleted the conflict between our INSERT and the
				// re-read. Surface the conflict with a message-only
				// body — the client retries either way.
				emitLockCreateMetric(ctx, logger, "conflict")
				WriteError(w, http.StatusConflict, "lock already exists for path")
				return
			}
			emitLockCreateMetric(ctx, logger, "conflict")
			writeLockJSON(w, http.StatusConflict, LockConflictResponse{
				Lock:    toLockWire(existing[0]),
				Message: "lock already exists for path",
			})
			return
		}
		logger.Error("locks: create", "err", err.Error(), "tenant", tenant, "repo", repo)
		emitLockCreateMetric(ctx, logger, "error")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Build the wire response from the in-memory state we just
	// committed, avoiding a readback Get. The readback would also
	// fail if a concurrent caller deletes the just-created lock
	// between INSERT and readback — a small race window that this
	// avoids entirely.
	emitLockCreateMetric(ctx, logger, "created")
	emitLFSLockCreate(ctx, logger, tenant+"/"+repo, actor.Name, actor.UserID, id, in.Path, in.RefName)
	// M15 webhook: lfs.lock.created. Fail-open.
	if deps.Webhooks != nil {
		payload := webhooks.LFSLockPayload{
			LockID: id,
			Path:   in.Path,
			Ref:    in.RefName,
		}
		if werr := deps.Webhooks.Enqueue(ctx, webhooks.EventLFSLockCreated,
			tenant, repo, actor.Name, payload); werr != nil {
			webhooks.EmitEnqueueFailed(ctx, logger,
				tenant, repo, "lfs.lock.created", werr.Error())
		}
	}
	wire := LockWire{
		ID:       id,
		Path:     in.Path,
		LockedAt: in.Now.UTC(),
		Owner:    LockOwner{Name: actor.Name},
	}
	writeLockJSON(w, http.StatusCreated, LockResponse{Lock: wire})
}

// handleLocksList is GET /{tenant}/{repo}.git/info/lfs/locks.
// Gated on ActionRead at the gateway layer.
func handleLocksList(w http.ResponseWriter, r *http.Request, deps *Deps, tenant, repo string, actor *auth.Actor, logger *slog.Logger) {
	ctx := r.Context()
	if deps.LocksStore == nil {
		emitLockListMetric(ctx, logger, "error")
		WriteError(w, http.StatusServiceUnavailable, "locks API not configured")
		return
	}
	if actor == nil {
		emitLockListMetric(ctx, logger, "error")
		w.Header().Set("WWW-Authenticate", `Basic realm="bucketvcs"`)
		WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q := r.URL.Query()
	opts := locks.ListOptions{
		Path:    q.Get("path"),
		ID:      q.Get("id"),
		RefName: q.Get("refspec"),
		Cursor:  q.Get("cursor"),
	}
	if limStr := q.Get("limit"); limStr != "" {
		n, perr := strconv.Atoi(limStr)
		if perr != nil || n < 0 || n > locks.MaxLimit {
			emitLockListMetric(ctx, logger, "error")
			WriteError(w, http.StatusBadRequest, "bad limit")
			return
		}
		opts.Limit = n
	}
	page, next, err := deps.LocksStore.List(ctx, tenant, repo, opts)
	if err != nil {
		if errors.Is(err, locks.ErrBadCursor) {
			emitLockListMetric(ctx, logger, "error")
			WriteError(w, http.StatusBadRequest, "bad cursor")
			return
		}
		logger.Error("locks: list", "err", err.Error(), "tenant", tenant, "repo", repo)
		emitLockListMetric(ctx, logger, "error")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	resp := ListLocksResponse{Locks: make([]LockWire, 0, len(page)), NextCursor: next}
	for _, l := range page {
		resp.Locks = append(resp.Locks, toLockWire(l))
	}
	emitLockListMetric(ctx, logger, "success")
	writeLockJSON(w, http.StatusOK, resp)
}

// handleLocksVerify is POST /{tenant}/{repo}.git/info/lfs/locks/verify.
// Partitions visible locks into ours (= owned by caller) and theirs.
// Gated on ActionRead at the gateway layer.
func handleLocksVerify(w http.ResponseWriter, r *http.Request, deps *Deps, tenant, repo string, actor *auth.Actor, logger *slog.Logger) {
	ctx := r.Context()
	if deps.LocksStore == nil {
		emitLockVerifyMetric(ctx, logger, "error")
		WriteError(w, http.StatusServiceUnavailable, "locks API not configured")
		return
	}
	if actor == nil {
		emitLockVerifyMetric(ctx, logger, "error")
		w.Header().Set("WWW-Authenticate", `Basic realm="bucketvcs"`)
		WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !requireLFSContentType(w, r) {
		emitLockVerifyMetric(ctx, logger, "error")
		return
	}
	var req LocksVerifyRequest
	if r.ContentLength != 0 {
		if err := decodeLockJSON(r, &req); err != nil {
			if !errors.Is(err, io.EOF) {
				emitLockVerifyMetric(ctx, logger, "error")
				WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			// Empty chunked body (ContentLength == -1, no payload) —
			// accept as if Content-Length: 0.
		}
	}
	if req.Limit < 0 || req.Limit > locks.MaxLimit {
		emitLockVerifyMetric(ctx, logger, "error")
		WriteError(w, http.StatusBadRequest, "bad limit")
		return
	}
	opts := locks.ListOptions{Cursor: req.Cursor, Limit: req.Limit}
	if req.Ref != nil {
		opts.RefName = req.Ref.Name
	}
	result, err := deps.LocksStore.Verify(ctx, tenant, repo, actor.UserID, opts)
	if err != nil {
		if errors.Is(err, locks.ErrBadCursor) {
			emitLockVerifyMetric(ctx, logger, "error")
			WriteError(w, http.StatusBadRequest, "bad cursor")
			return
		}
		logger.Error("locks: verify", "err", err.Error(), "tenant", tenant, "repo", repo)
		emitLockVerifyMetric(ctx, logger, "error")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	resp := LocksVerifyResponse{
		Ours:       toLockWires(result.Ours),
		Theirs:     toLockWires(result.Theirs),
		NextCursor: result.NextCursor,
	}
	emitLockVerifyMetric(ctx, logger, "success")
	emitLFSLockVerify(ctx, logger, tenant+"/"+repo, actor.Name, len(result.Ours), len(result.Theirs))
	writeLockJSON(w, http.StatusOK, resp)
}

// handleLocksUnlock is POST /{tenant}/{repo}.git/info/lfs/locks/<id>/unlock.
// Owner can unlock freely; non-owners must pass {"force": true}, which
// is allowed for any ActionWrite caller (per LFS spec).
// lockID is extracted and validated by parseLFSPath before dispatch.
func handleLocksUnlock(w http.ResponseWriter, r *http.Request, deps *Deps, tenant, repo, lockID string, actor *auth.Actor, logger *slog.Logger) {
	ctx := r.Context()
	if deps.LocksStore == nil {
		emitLockDeleteMetric(ctx, logger, false, "error")
		WriteError(w, http.StatusServiceUnavailable, "locks API not configured")
		return
	}
	if actor == nil {
		emitLockDeleteMetric(ctx, logger, false, "error")
		w.Header().Set("WWW-Authenticate", `Basic realm="bucketvcs"`)
		WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if lockID == "" {
		emitLockDeleteMetric(ctx, logger, false, "error")
		WriteError(w, http.StatusBadRequest, "lock id missing from URL")
		return
	}
	if !requireLFSContentType(w, r) {
		emitLockDeleteMetric(ctx, logger, false, "error")
		return
	}
	var req UnlockRequest
	if r.ContentLength != 0 {
		if err := decodeLockJSON(r, &req); err != nil {
			if !errors.Is(err, io.EOF) {
				emitLockDeleteMetric(ctx, logger, false, "error")
				WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			// Empty chunked body (ContentLength == -1, no payload) —
			// accept as if Content-Length: 0.
		}
	}
	lock, err := deps.LocksStore.Get(ctx, tenant, repo, lockID)
	if err != nil {
		if errors.Is(err, locks.ErrNotFound) {
			emitLockDeleteMetric(ctx, logger, req.Force, "not_found")
			WriteError(w, http.StatusNotFound, "lock not found")
			return
		}
		logger.Error("locks: get before unlock", "err", err.Error(), "tenant", tenant, "repo", repo, "lock_id", lockID)
		emitLockDeleteMetric(ctx, logger, req.Force, "error")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if lock.Owner.UserID != actor.UserID && !req.Force {
		emitLockDeleteMetric(ctx, logger, false, "denied")
		WriteError(w, http.StatusForbidden, "lock owned by another user; pass force=true to override")
		return
	}
	if err := deps.LocksStore.Delete(ctx, tenant, repo, lockID); err != nil {
		logger.Error("locks: delete", "err", err.Error(), "tenant", tenant, "repo", repo, "lock_id", lockID)
		emitLockDeleteMetric(ctx, logger, req.Force, "error")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Emit metric + audit, branching on whether caller is the owner.
	if lock.Owner.UserID == actor.UserID {
		emitLockDeleteMetric(ctx, logger, req.Force, "owner")
		emitLFSLockDelete(ctx, logger, tenant+"/"+repo, actor.Name, lockID, req.Force, "")
	} else {
		// Non-owner force-unlock.
		emitLockDeleteMetric(ctx, logger, true, "forced")
		emitLFSLockDelete(ctx, logger, tenant+"/"+repo, actor.Name, lockID, true, lock.Owner.UserID)
	}
	// M15 webhook: lfs.lock.released. Fail-open.
	if deps.Webhooks != nil {
		payload := webhooks.LFSLockPayload{
			LockID: lock.ID,
			Path:   lock.Path,
			Ref:    lock.RefName,
		}
		if werr := deps.Webhooks.Enqueue(ctx, webhooks.EventLFSLockReleased,
			tenant, repo, actor.Name, payload); werr != nil {
			webhooks.EmitEnqueueFailed(ctx, logger,
				tenant, repo, "lfs.lock.released", werr.Error())
		}
	}
	writeLockJSON(w, http.StatusOK, UnlockResponse{Lock: toLockWire(lock)})
}

// --- helpers ---

// decodeLockJSON reads up to 1 MiB of the request body and decodes it
// into dst. Unknown fields are rejected to catch client typos early.
func decodeLockJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

// writeLockJSON writes a JSON body with the LFS content-type at the
// given status. Distinct from WriteError (which writes the error
// envelope shape).
func writeLockJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// toLockWire projects a server-internal locks.Lock onto the wire
// shape. LockedAt is normalised to UTC so the timestamp serialises
// with a "Z" suffix regardless of server timezone.
func toLockWire(l locks.Lock) LockWire {
	return LockWire{
		ID:       l.ID,
		Path:     l.Path,
		LockedAt: l.LockedAt.UTC(),
		Owner:    LockOwner{Name: l.Owner.Name},
	}
}

func toLockWires(ls []locks.Lock) []LockWire {
	out := make([]LockWire, 0, len(ls))
	for _, l := range ls {
		out = append(out, toLockWire(l))
	}
	return out
}
