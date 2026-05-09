package receivepack

import (
	"errors"
	"fmt"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

// Service runs the receive-pack protocol: reads commands + packfile from
// req.Stdin, writes report-status to req.Stdout. ProtocolVersion is
// ignored (receive-pack is V0 only).
//
// Return values:
//   - nil                 → report written, no further HTTP action needed
//   - ErrFlushOnlyProbe   → client sent a flush-only probe; adapter must
//     respond 200 with an empty body
//   - ErrRepoNotFound     → adapter maps to 404
//   - ErrInvalidName      → adapter maps to 400
//   - ErrBadRequest       → adapter maps to 400 (parse error)
//   - any other error     → adapter maps to 500; bytes may already be on
//     the wire if the engine started writing
func Service(req *EngineRequest) error {
	ctx := req.Ctx

	// Resolve the repo first so a missing repo returns a clean error instead
	// of a mirror-init 500. mirror.Manager.Open also calls repo.Open, but
	// we want to differentiate "repo not found" from "mirror init failed".
	if _, err := repo.Open(ctx, req.Store, req.Tenant, req.Repo); err != nil {
		if errors.Is(err, repoerrs.ErrRepoNotFound) {
			return ErrRepoNotFound
		}
		if errors.Is(err, repoerrs.ErrInvalidTenantID) || errors.Is(err, repoerrs.ErrInvalidRepoID) {
			return ErrInvalidName
		}
		return err
	}

	// mgr.Open runs SyncToCurrent under its own RLock/Lock, leaving the
	// mirror current at this point. We deliberately do NOT call
	// SyncToCurrent again under our own write lock — RWMutex is not
	// reentrant and SyncToCurrent's fast path takes RLock, which would
	// deadlock. Any TOCTOU between Open's sync and our Lock() below is
	// resolved by re-reading the manifest body under the write lock and by
	// BuildAndCommit's CAS, which is the authoritative gate for ref drift.
	m, err := req.Mirror.Open(ctx, req.Tenant, req.Repo)
	if err != nil {
		return err
	}

	rp, err := parseReceivePackRequest(ctx, req.Stdin, m.IncomingDir())
	if err != nil {
		if rp != nil && rp.PackPath != "" {
			_ = os.Remove(rp.PackPath)
		}
		if errors.Is(err, ErrFlushOnlyProbe) {
			return ErrFlushOnlyProbe
		}
		return fmt.Errorf("%w: %s", ErrBadRequest, err.Error())
	}

	// Acquire the per-repo write lock. Held for the entire validate +
	// commit + IngestPack pipeline so two concurrent pushes serialize
	// against the local bare. (CAS at the bucket layer is the broader
	// guarantee for cross-process concurrency.)
	m.Lock()
	defer m.Unlock()

	// The staged pack in incoming/ is consumed by IndexPackStrict (which
	// reads it via stdin and writes pack-<hash>.{pack,idx,keep} into the
	// bare's objects/pack/). On every exit path we remove the staging
	// file. The bare's pack files have separate ownership and are NOT
	// removed here — BuildAndCommit's removeKeepFiles handles the .keep
	// on the success path; on failure paths the .keep stays and the next
	// successful push (or M9 stale-detection rebuild) reconciles.
	defer func() {
		if rp.PackPath != "" {
			_ = os.Remove(rp.PackPath)
		}
	}()

	completeReceivePack(req, req.Stdout, m, rp)
	return nil
}
