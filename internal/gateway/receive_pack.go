package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

// gatewayNullOID is the 40-zero OID sentinel for create/delete commands.
// We define it locally to avoid an import cycle with internal/mirror.
const gatewayNullOID = "0000000000000000000000000000000000000000"

// errReceivePackProbe signals that the request body contained only a flush
// packet (no update commands and no pack data). git-remote-curl's
// "stateless RPC" code (remote-curl.c::probe_rpc) issues such a request as
// an authentication/connectivity probe BEFORE sending a large push body in
// chunked encoding. The server must respond 200 OK with an empty body so
// the client proceeds with the real POST. Treating it as a parse error
// (HTTP 400) breaks every push above http.postBuffer (default 1 MiB).
var errReceivePackProbe = errors.New("receive-pack probe (flush-only body)")

// maxUpdateCommands caps the number of update commands a single
// receive-pack request can carry. Each command spawns a
// `git check-ref-format` subprocess for refname validation, so an
// uncapped count is a CPU/process DoS even at delete-only sizes well
// below the request body limit. 4096 is well above any realistic push
// (the largest known repos advertise <1k branches/tags) and below the
// point where the slice itself is a memory pressure source.
const maxUpdateCommands = 4096

type updateCommand struct {
	OldOID  string
	NewOID  string
	Refname string
}

type receivePackRequest struct {
	Caps     map[string]bool
	Updates  []updateCommand
	PackPath string // empty for delete-only push
	IsAtomic bool
}

// handleReceivePack implements POST /<tenant>/<repo>.git/git-receive-pack
// for the v0 receive-pack protocol. It parses update commands + capability
// set, stages any inbound pack to the mirror's incoming/ dir, then runs
// the full validate + commit + IngestPack pipeline:
//
//  1. Sync mirror to current bucket state (via mgr.Open) and acquire the
//     per-repo write lock for the rest of the request.
//  2. Validate every command's old-OID against the manifest body. In atomic
//     mode any failure poisons the entire batch.
//  3. If a pack accompanied the request: index-pack --strict --fix-thin
//     into the bare, then a `rev-list <new-oids> --not --all` connectivity
//     check.
//  4. Apply ref updates to the local bare so importer.BuildAndCommit's
//     repack sees the new tips, then BuildAndCommit (repack + upload +
//     CAS-commit). Stale-manifest losers fail the CAS and are reported
//     as "stale-manifest".
//  5. Re-read the manifest header to verify the post-commit version is
//     exactly preCommitVersion+1 (race detection: a larger jump means a
//     concurrent push landed before BuildAndCommit's read and our
//     updates may have stomped it). On mismatch, surface as
//     stale-manifest.
//  6. Mark the mirror stale (remove the sentinel) so SyncToCurrent
//     rebuilds from the new authoritative root on the next request.
//     This is more conservative than bumping the sentinel ourselves —
//     the readback that would tell us the new version is itself racy.
//  7. Emit a report-status pkt-line stream (side-band-wrapped if the
//     client advertised side-band-64k).
//
// Failure modes are signaled via the report rather than HTTP status: a 200
// with "ng <ref> <reason>" is the wire protocol's failure channel, since
// report-status framing requires a 2xx response.
func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	defer r.Body.Close()
	body := http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)

	// Resolve the repo first so a missing repo returns a clean 404 instead
	// of a mirror-init 500. mirror.Manager.Open also calls repo.Open, but
	// we want to differentiate "repo not found" from "mirror init failed".
	if _, err := repo.Open(r.Context(), s.store, tenant, repoID); err != nil {
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
	// mgr.Open runs SyncToCurrent under its own RLock/Lock, leaving the
	// mirror current at this point. We deliberately do NOT call
	// SyncToCurrent again under our own write lock — RWMutex is not
	// reentrant and SyncToCurrent's fast path takes RLock, which would
	// deadlock. Any TOCTOU between Open's sync and our Lock() below is
	// resolved by re-reading the manifest body under the write lock and by
	// BuildAndCommit's CAS, which is the authoritative gate for ref drift.
	//
	// KNOWN GAP (deferred to M9): if a CONCURRENT cross-process push
	// advances the bucket between Open's sync and our Lock(), the local
	// bare won't have the new objects/refs. Our re-read sees the new
	// manifest body, so old-OID validation correctly rejects stale
	// updates with "stale info". But a push whose old-OID happens to
	// match the new tip (rare) and whose new-OID's parents are objects
	// the concurrent push added would fail at our connectivity check
	// rather than succeed against the up-to-date bare. Closing this
	// requires an exported held-variant of mirror.SyncToCurrent (or
	// restructuring this handler around mirror's internal sync path);
	// both are deferred to M9 alongside the broader incremental-mirror
	// work.
	m, err := s.mgr.Open(r.Context(), tenant, repoID)
	if err != nil {
		http.Error(w, "mirror: "+err.Error(), http.StatusInternalServerError)
		return
	}

	req, err := parseReceivePackRequest(r.Context(), body, m.IncomingDir())
	if err != nil {
		if req != nil && req.PackPath != "" {
			_ = os.Remove(req.PackPath)
		}
		if errors.Is(err, errReceivePackProbe) {
			// Large-request probe: respond 200 OK with an empty body so
			// the client proceeds to send the real chunked POST. The
			// content-type advertises receive-pack-result for clients
			// that may inspect it before issuing the follow-up request.
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "receive-pack: "+err.Error(), http.StatusBadRequest)
		return
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
		if req.PackPath != "" {
			_ = os.Remove(req.PackPath)
		}
	}()

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	s.completeReceivePack(r.Context(), w, m, req, tenant, repoID)
}

// completeReceivePack runs steps 4-13 of the push pipeline. The caller
// MUST hold m.Lock() and is responsible for the lifecycle of req.PackPath
// (the staged inbound pack file in incoming/).
func (s *Server) completeReceivePack(
	ctx context.Context,
	w http.ResponseWriter,
	m *mirror.Mirror,
	req *receivePackRequest,
	tenant, repoID string,
) {
	// Read the current manifest body under our write lock so old-OID
	// validation sees a snapshot consistent with the BuildAndCommit CAS
	// that follows. Capture the version so we can detect a cross-process
	// commit landing between this read and BuildAndCommit's own ReadRoot
	// (BuildAndCommit's CAS only catches commits AFTER its read; the
	// window before its read is the race the post-commit version-skip
	// check below guards against).
	r2, err := repo.Open(ctx, s.store, tenant, repoID)
	if err != nil {
		writeReceiveReport(w, "internal-error: "+err.Error(), nil, req.Caps)
		return
	}
	view, err := r2.ReadRoot(ctx)
	if err != nil {
		writeReceiveReport(w, "internal-error: "+err.Error(), nil, req.Caps)
		return
	}
	preCommitVersion := view.Header.ManifestVersion
	var currentBody manifest.Body
	if err := json.Unmarshal(view.Body, &currentBody); err != nil {
		writeReceiveReport(w, "internal-error: "+err.Error(), nil, req.Caps)
		return
	}

	// Step 5: validate old-OID for each command.
	statuses := make([]string, len(req.Updates)) // "" = ok-so-far, else "ng <ref> <reason>"
	allOK := true
	for i, u := range req.Updates {
		if u.OldOID == gatewayNullOID {
			// Create: ref must NOT exist.
			if _, exists := currentBody.Refs[u.Refname]; exists {
				statuses[i] = "ng " + u.Refname + " ref already exists"
				allOK = false
			}
		} else {
			cur, ok := currentBody.Refs[u.Refname]
			if !ok || cur != u.OldOID {
				statuses[i] = "ng " + u.Refname + " stale info"
				allOK = false
			}
		}
	}

	// Step 6: atomic batch handling — any failure poisons the whole batch.
	if req.IsAtomic && !allOK {
		for i, u := range req.Updates {
			if statuses[i] == "" {
				statuses[i] = "ng " + u.Refname + " atomic-batch-failed"
			}
		}
		writeReceiveReport(w, "ok", statuses, req.Caps)
		return
	}

	// If non-atomic and EVERY command failed pre-pack-validation, there's
	// nothing to ingest. Emit the report and return.
	allFailed := true
	for _, s := range statuses {
		if s == "" {
			allFailed = false
			break
		}
	}
	if allFailed {
		writeReceiveReport(w, "ok", statuses, req.Caps)
		return
	}

	// Step 7: index any inbound pack into the bare. IndexPackStrict places
	// pack-<hash>.{pack,idx,keep} under <bare>/objects/pack/. The .keep
	// guards the new objects from a racing GC; BuildAndCommit's
	// removeKeepFiles clears it on the happy path. On error paths below
	// we leave the .keep + pack in place — a subsequent successful push
	// or stale-detection rebuild reconciles. (Cleaner cleanup is deferred
	// to M9.)
	if req.PackPath != "" {
		if _, err := gitcli.IndexPackStrict(ctx, m.BareDir(), req.PackPath); err != nil {
			writeReceiveReport(w, "invalid-pack: "+err.Error(), nil, req.Caps)
			return
		}
	}

	// Step 8: connectivity. After IndexPackStrict has placed the inbound
	// pack into the bare with --strict --fix-thin (which itself validates
	// the pack is self-contained and closed under reachability), we run
	// `git rev-list --objects <new-oids> --not --all` as a defensive
	// double-check that walking from the new tips hits no missing objects.
	//
	// Two semantics depending on whether a pack came in:
	//   - WITH pack: a NON-empty rev-list output is normal — it lists
	//     exactly the objects newly introduced by this push (reachable
	//     from new-oids but not from any prior ref; for a first push to
	//     an empty repo this is every object in the pack). What we care
	//     about is whether rev-list SUCCEEDED — a non-zero exit means
	//     the walk hit a missing object.
	//   - WITHOUT pack: the new-OID must already be reachable from an
	//     existing ref. NON-empty output means the new-OID points at
	//     "dangling-but-present" objects (e.g. garbage from a previously
	//     failed push or a stale-detection rebuild). Without this check,
	//     a malicious or buggy client could create a ref pointing at an
	//     object that is in the bare but was never advertised, smuggling
	//     state outside the manifest's coverage. Reject as
	//     missing-connectivity (the closest pack-protocol-level error
	//     for "you didn't supply enough to back this ref").
	var newOIDs []string
	for i, u := range req.Updates {
		if statuses[i] == "" && u.NewOID != gatewayNullOID {
			newOIDs = append(newOIDs, u.NewOID)
		}
	}
	if len(newOIDs) > 0 {
		out, err := gitcli.RevListNotAll(ctx, m.BareDir(), newOIDs)
		if err != nil {
			writeReceiveReport(w, "missing-connectivity: "+err.Error(), nil, req.Caps)
			return
		}
		if req.PackPath == "" && len(out) > 0 {
			writeReceiveReport(w, "missing-connectivity: pack required for new objects", nil, req.Caps)
			return
		}
	}

	// Step 9: build the refUpdates map for accepted commands.
	refUpdates := map[string]string{}
	for i, u := range req.Updates {
		if statuses[i] != "" {
			continue
		}
		refUpdates[u.Refname] = u.NewOID
	}
	if len(refUpdates) == 0 {
		// Defensive — covered by the allFailed branch above, but the
		// invariant matters: never call BuildAndCommit with an empty map
		// and an existing repo (it would CAS a no-op manifest version).
		writeReceiveReport(w, "ok", statuses, req.Caps)
		return
	}

	// Step 9b: apply ref updates to the LOCAL bare BEFORE BuildAndCommit.
	// BuildAndCommit's contract (importer/buildcommit.go header) requires
	// "the inbound validated pack and any new ref tips already applied"
	// — its repack uses `git pack-objects` driven by `rev-list --all`,
	// which would emit nothing if the new tip's ref doesn't yet exist in
	// the bare, leaving the canonical pack empty and buildTipsFromRefs
	// failing with "oid not in idx".
	//
	// We use gitcli directly rather than mirror.IngestPack because IngestPack
	// also writes a sentinel — and at this point we don't yet know the
	// post-commit manifest version. The sentinel is written at the end of
	// the pipeline via a separate IngestPack(refs=nil) call.
	//
	// On failure here the local bare is in an undefined state (some refs
	// may have been applied). We report internal-mirror-error; a follow-up
	// SyncToCurrent will rebuild from the bucket on the next request.
	for i, u := range req.Updates {
		if statuses[i] != "" {
			continue
		}
		mu := mirror.RefUpdate{Refname: u.Refname, OldOID: u.OldOID, NewOID: u.NewOID}
		if err := applyRefUpdateInBare(ctx, m.BareDir(), mu); err != nil {
			// Some refs in this batch may already have been applied to
			// the local bare. The bucket has NOT been updated, but our
			// sentinel still matches the (unchanged) bucket — meaning a
			// subsequent SyncToCurrent would falsely consider the mirror
			// current and start advertising the partially-applied refs.
			// Remove the sentinel so the next request forces a rebuild.
			markMirrorStale(m)
			for j, uu := range req.Updates {
				if statuses[j] == "" {
					statuses[j] = "ng " + uu.Refname + " internal-mirror-error"
				}
			}
			writeReceiveReport(w, "ok", statuses, req.Caps)
			return
		}
	}

	// Step 10: BuildAndCommit. This repacks the bare into a canonical pack,
	// uploads pack/idx/.bvom/.bvcg, and CAS-commits a new manifest body.
	// Stale-manifest losers (concurrent pushes that won the CAS race) are
	// surfaced via err message "stale manifest" / "lost CAS".
	//
	// Actor: pull from RunAuth's request context so the tx record carries
	// per-user attribution. After M4 Task 18, receive-pack always runs
	// behind the auth middleware with ActionWrite, so a non-nil actor is
	// the expected path; the "anonymous" fallback is defensive only
	// (RunAuth's Decide rejects nil-actor writes with 401 before we get
	// here).
	actorName := "anonymous"
	if a := ActorFromContext(ctx); a != nil {
		switch {
		case a.Name != "":
			actorName = a.Name
		case a.UserID != "":
			actorName = a.UserID
		}
	}
	if _, err := importer.BuildAndCommit(ctx, s.store, tenant, repoID, m.BareDir(), refUpdates, actorName); err != nil {
		// Refs are already applied to the local bare (step 9b above), but
		// the bucket commit failed. Sentinel still matches the OLD bucket
		// version, so SyncToCurrent would falsely consider the mirror
		// current. Mark stale so the next request rebuilds from the
		// authoritative bucket state.
		markMirrorStale(m)
		reason := "internal-storage-error"
		emsg := err.Error()
		if strings.Contains(emsg, "stale manifest") || strings.Contains(emsg, "lost the CAS race") {
			reason = "stale-manifest"
		}
		for i, u := range req.Updates {
			if statuses[i] == "" {
				statuses[i] = "ng " + u.Refname + " " + reason
			}
		}
		writeReceiveReport(w, "ok", statuses, req.Caps)
		return
	}

	// Step 11: re-read manifest to verify our commit landed cleanly and to
	// pick up the post-commit ManifestVersion + LatestTx.
	//
	// Race detection (review finding HIGH 1, partial mitigation): we
	// captured preCommitVersion BEFORE old-OID validation; BuildAndCommit
	// internally re-reads root and uses THAT version as its CAS base.
	// If a concurrent cross-process push landed BETWEEN our read and
	// BuildAndCommit's read, BuildAndCommit's mergeRefs would have
	// overlaid our updates onto the newer body — silently overwriting
	// any concurrent ref change for the same refname. BuildAndCommit
	// commits at startVersion+1, so the post-commit version is
	// preCommitVersion+1 in the no-race case; anything larger means the
	// race window fired.
	//
	// We surface this as "ng stale-manifest" so the client sees a clean
	// failure even though the bucket-side commit succeeded. The mirror
	// is marked stale below so SyncToCurrent rebuilds from the new
	// authoritative root (which now has our updates plus whatever the
	// concurrent push made it).
	//
	// KNOWN LIMITATION (deferred to M9): this is detect-after-commit, not
	// prevent-before-commit. The roborev review re-flagged this in job
	// 8274 noting that "the bad commit is already durable." Eliminating
	// the race requires moving old-OID validation into BuildAndCommit's
	// commit callback (which gets `prev *RootView` of the actual
	// CAS-pre-image body) — an importer-package change that the M3 task
	// constraint reserves for M9. The detection narrows the user-visible
	// outcome from "silent stomp" to "honest stale-manifest" (the
	// stomped concurrent ref is then visible to the next reader and the
	// client retries against a fresh tip), but it does not erase the
	// momentary stomp.
	viewAfter, err := r2.ReadRoot(ctx)
	if err != nil {
		// Bucket-side commit succeeded; the local bare has the new refs
		// but we can't bump the sentinel without the new header. Mark
		// stale so SyncToCurrent rebuilds on the next request and picks
		// up the new authoritative root. Report success — the durable
		// commit happened.
		markMirrorStale(m)
		for i, u := range req.Updates {
			if statuses[i] == "" {
				statuses[i] = "ok " + u.Refname
			}
		}
		writeReceiveReport(w, "ok", statuses, req.Caps)
		return
	}
	if viewAfter.Header.ManifestVersion > preCommitVersion+1 {
		// Race detected: at least one concurrent commit landed before
		// BuildAndCommit's read, so our updates may have stomped a
		// concurrent change. Surface as stale-manifest. Mark stale so
		// the next request rebuilds from the authoritative root.
		markMirrorStale(m)
		for i, u := range req.Updates {
			if statuses[i] == "" {
				statuses[i] = "ng " + u.Refname + " stale-manifest"
			}
		}
		writeReceiveReport(w, "ok", statuses, req.Caps)
		return
	}

	// Step 12-13: success path. We could bump the mirror sentinel via
	// IngestPack(refs=nil) for efficiency (avoid a SyncToCurrent rebuild
	// next request), BUT the readback above is itself racy: even after
	// our version-skip check passes, a concurrent commit can land between
	// the readback and the sentinel write, leaving us writing a sentinel
	// for a manifest the local bare hasn't synced. Per review finding
	// HIGH 2, we defer to markMirrorStale + SyncToCurrent on the next
	// request rather than write a possibly-stale sentinel. The cost is
	// one extra rebuild per push; the benefit is correctness without
	// having to plumb the post-commit header back from BuildAndCommit
	// (which would require an importer-package change).
	markMirrorStale(m)

	// Step 15: success. Fill in "ok <ref>" for every accepted command;
	// rejected ones already carry their "ng <ref> <reason>".
	for i, u := range req.Updates {
		if statuses[i] == "" {
			statuses[i] = "ok " + u.Refname
		}
	}
	writeReceiveReport(w, "ok", statuses, req.Caps)
}

// parseReceivePackRequest drains the v0 receive-pack request body. It reads
// pkt-line tokens until flush, accumulating <old> <new> <refname> commands;
// the FIRST command line carries a NUL-suffixed capability list. After the
// flush, if any command is a non-delete (NewOID != gatewayNullOID), the
// remaining body bytes are streamed verbatim into <incoming>/rcv-<rand>.pack.
// On error the returned *receivePackRequest may carry a non-empty PackPath
// the caller must clean up.
func parseReceivePackRequest(ctx context.Context, body io.Reader, incoming string) (*receivePackRequest, error) {
	pr := pktline.NewReader(body)
	req := &receivePackRequest{Caps: map[string]bool{}}
	first := true
	for {
		tok, err := pr.Read()
		if err == io.EOF {
			// A body that ends without any pkt-lines at all is the
			// large-request probe described in errReceivePackProbe — but
			// real git-remote-curl probes carry a single flush packet
			// (handled in the Flush branch below). A bare EOF without any
			// bytes read is an invalid client request.
			return nil, errors.New("unexpected EOF before flush")
		}
		if err != nil {
			return nil, fmt.Errorf("read commands: %w", err)
		}
		if tok.Type == pktline.Flush {
			// Flush as the very first (and so far only) token with no
			// commands accumulated is the large-request probe; signal the
			// caller to short-circuit with a 200 instead of HTTP 400.
			if first && len(req.Updates) == 0 {
				return req, errReceivePackProbe
			}
			break
		}
		if tok.Type != pktline.Data {
			return nil, fmt.Errorf("unexpected token %v", tok.Type)
		}
		// Copy payload because pktline reuses its internal buffer.
		line := string(append([]byte{}, tok.Payload...))
		// pack-protocol(5) describes each receive-pack command as
		// "<old> <new> <name>" — the trailing LF is permitted but not
		// required, and real `git push` clients (observed: git 2.54)
		// omit it on the wire. Strip at most one LF if present; reject
		// any further trailing newlines so a malformed payload (e.g.
		// `...main\n\n`) doesn't sneak past the OID/refname checks
		// below.
		if strings.HasSuffix(line, "\n") {
			line = line[:len(line)-1]
			if strings.HasSuffix(line, "\n") {
				return nil, fmt.Errorf("extra LF in command")
			}
		}
		if first {
			first = false
			if i := strings.IndexByte(line, '\x00'); i >= 0 {
				caps := strings.Fields(line[i+1:])
				for _, c := range caps {
					req.Caps[c] = true
				}
				line = line[:i]
			}
			if req.Caps["atomic"] {
				req.IsAtomic = true
			}
		}
		// "<old> <new> <refname>"
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("malformed update command %q", line)
		}
		old, neu, ref := parts[0], parts[1], parts[2]
		if !validHexOID40(old) || !validHexOID40(neu) {
			return nil, fmt.Errorf("invalid OID in command %q", line)
		}
		if err := gitcli.CheckRefFormat(ctx, ref); err != nil {
			return nil, fmt.Errorf("invalid refname %q: %w", ref, err)
		}
		if strings.HasPrefix(ref, "refs/replace/") {
			return nil, fmt.Errorf("refs/replace/* writes are not allowed")
		}
		if neu == gatewayNullOID && old == gatewayNullOID {
			return nil, fmt.Errorf("noop command (both OIDs are zero)")
		}
		req.Updates = append(req.Updates, updateCommand{OldOID: old, NewOID: neu, Refname: ref})
		if len(req.Updates) > maxUpdateCommands {
			return nil, fmt.Errorf("too many update commands (>%d)", maxUpdateCommands)
		}
	}
	if len(req.Updates) == 0 {
		return nil, errors.New("no update commands")
	}

	// Reject duplicate refnames in a single request. pack-protocol(5)
	// doesn't explicitly forbid duplicates, but our pipeline collapses
	// refUpdates into a map keyed by refname (only the LAST entry wins
	// at BuildAndCommit time) while the per-command status loop still
	// reports BOTH commands as ok. A crafted request with two creates
	// for the same ref could therefore see "ok refs/heads/x" twice with
	// only the second value committed, masking that the client's first
	// command was silently dropped. Rejecting at parse time keeps the
	// invariant "every accepted command corresponds to one applied
	// update" — which the per-ref ok/ng reporting depends on.
	seen := make(map[string]struct{}, len(req.Updates))
	for _, u := range req.Updates {
		if _, dup := seen[u.Refname]; dup {
			return nil, fmt.Errorf("duplicate refname in request: %q", u.Refname)
		}
		seen[u.Refname] = struct{}{}
	}

	allDelete := true
	for _, u := range req.Updates {
		if u.NewOID != gatewayNullOID {
			allDelete = false
			break
		}
	}
	if allDelete {
		// pack-protocol(5) forbids a packfile after a delete-only command
		// list. Trailing bytes indicate a malformed or attacker-crafted
		// request; reject so we don't silently accept arbitrary garbage.
		// We probe with a 1-byte read against the MaxBytesReader, which
		// returns EOF on a clean end and a non-EOF error if the body
		// exceeded the limit.
		var probe [1]byte
		n, err := body.Read(probe[:])
		if n > 0 {
			return req, errors.New("trailing bytes after delete-only command list")
		}
		if err != nil && err != io.EOF {
			return req, fmt.Errorf("body trailer: %w", err)
		}
		return req, nil
	}

	// Non-delete push: read remaining body into a temp pack file under
	// <mirror>/incoming/. IncomingDir is mkdir'd at mirror.Manager.Open
	// time; the MkdirAll here is defensive in case the directory was
	// removed out-of-band.
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		return req, fmt.Errorf("incoming mkdir: %w", err)
	}
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return req, fmt.Errorf("incoming name: %w", err)
	}
	packPath := filepath.Join(incoming, "rcv-"+hex.EncodeToString(idBytes)+".pack")
	f, err := os.OpenFile(packPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return req, fmt.Errorf("create incoming: %w", err)
	}
	written, err := io.Copy(f, body)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(packPath)
		return req, fmt.Errorf("write incoming: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(packPath)
		return req, fmt.Errorf("close incoming: %w", err)
	}
	// Zero-byte body is legal for a non-delete push: the client may be
	// updating a ref to an OID the server already has (e.g. fast-forward
	// to a tip already known via another ref, branch rename via push of
	// the existing tip under a new name). pack-protocol(5) says the
	// packfile is OPTIONAL after the command list, and real `git push`
	// elides it entirely in this case rather than sending a zero-object
	// pack header. Indexing a 0-byte file would fail with "invalid-pack",
	// so leave PackPath empty and let the connectivity check verify the
	// new OID is already present in the bare.
	if written == 0 {
		_ = os.Remove(packPath)
		return req, nil
	}
	req.PackPath = packPath
	return req, nil
}

// validHexOID40 returns true iff s is exactly 40 lowercase hex chars. Git
// OIDs in wire protocols are always lowercase; mixed case would also be
// rejected by downstream tooling, so we don't normalize.
func validHexOID40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(('0' <= c && c <= '9') || ('a' <= c && c <= 'f')) {
			return false
		}
	}
	return true
}

// applyRefUpdateInBare dispatches a single ref change (create/update/
// delete) directly via gitcli against the bare repo. Mirrors the logic
// of mirror.applyRefUpdate (which is unexported) so we can apply refs
// to the bare BEFORE calling importer.BuildAndCommit, whose repack
// requires the new tips to be reachable from a ref.
func applyRefUpdateInBare(ctx context.Context, bareDir string, u mirror.RefUpdate) error {
	switch {
	case u.NewOID == gatewayNullOID:
		return gitcli.UpdateRefDelete(ctx, bareDir, u.Refname, u.OldOID)
	case u.OldOID == "" || u.OldOID == gatewayNullOID:
		return gitcli.UpdateRef(ctx, bareDir, u.Refname, u.NewOID)
	default:
		return gitcli.UpdateRefCAS(ctx, bareDir, u.Refname, u.NewOID, u.OldOID)
	}
}

// markMirrorStale removes the mirror's manifest-version sentinel so the
// next SyncToCurrent treats the mirror as not-current and rebuilds from
// the authoritative bucket state. Used on every failure path AFTER we
// have mutated the local bare (applied refs) but BEFORE the bucket has
// been updated to match — without this, the unchanged sentinel would
// match the unchanged bucket version and SyncToCurrent would falsely
// consider the mirror current while the bare carried partially-applied
// or never-committed refs.
//
// Best-effort: a failure here is logged via the error return being
// dropped. The next read attempt will fail to parse the (possibly
// still-present) sentinel and rebuild anyway, since the file content
// after a partial os.Remove failure is undefined and readSentinel
// treats any error as "not current".
func markMirrorStale(m *mirror.Mirror) {
	_ = os.Remove(m.VersionFile())
}

// writeReceiveReport emits a report-status pkt-line stream per
// pack-protocol(5):
//
//	"unpack <header>\n"   (header == "ok" on success, else an error
//	                       string; clients display this verbatim)
//	per-ref status line   (each entry is either "ok <ref>\n" or
//	                       "ng <ref> <reason>\n"; pre-built by the
//	                       caller in the statuses slice)
//	flush
//
// When the client negotiated side-band-64k the entire pkt-line stream is
// multiplexed on band 1, terminated by a band-level flush, then a final
// outer pkt-line flush. We can't side-band each pkt-line individually
// because the client's report-status parser expects a contiguous pkt-line
// stream on band 1.
//
// Best-effort: if a Write fails we cannot recover (the response is
// partially written and surfacing an error would corrupt framing
// further). Errors are silently dropped; the client will see an EOF and
// surface the partial report.
func writeReceiveReport(w io.Writer, header string, statuses []string, caps map[string]bool) {
	pw := pktline.NewWriter(w)

	if caps["side-band-64k"] {
		var inner bytes.Buffer
		ipw := pktline.NewWriter(&inner)
		if err := ipw.WriteString("unpack " + header + "\n"); err != nil {
			return
		}
		for _, s := range statuses {
			if s == "" {
				continue
			}
			if err := ipw.WriteString(s + "\n"); err != nil {
				return
			}
		}
		if err := ipw.WriteFlush(); err != nil {
			return
		}
		sb := pktline.NewSidebandWriter(pw)
		if _, err := sb.WriteData(inner.Bytes()); err != nil {
			return
		}
		_ = pw.WriteFlush()
		return
	}

	if err := pw.WriteString("unpack " + header + "\n"); err != nil {
		return
	}
	for _, s := range statuses {
		if s == "" {
			continue
		}
		if err := pw.WriteString(s + "\n"); err != nil {
			return
		}
	}
	_ = pw.WriteFlush()
}
