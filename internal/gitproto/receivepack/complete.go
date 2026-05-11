package receivepack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// completeReceivePack runs the validate + commit + IngestPack pipeline.
// The caller MUST hold m.Lock() and is responsible for the lifecycle of
// req.PackPath (the staged inbound pack file in incoming/).
//
// This is a verbatim port of gateway.completeReceivePack. The only surface
// differences are:
//   - ctx, w, tenant, repoID, store pulled from eng (EngineRequest)
//   - actor extracted from eng.Actor instead of ActorFromContext(ctx)
//   - markMirrorStale is the engine-local function (same os.Remove logic)
func completeReceivePack(eng *EngineRequest, w io.Writer, m *mirror.Mirror, rp *receivePackRequest) {
	ctx := eng.Ctx
	tenant := eng.Tenant
	repoID := eng.Repo

	// Read the current manifest body under our write lock so old-OID
	// validation sees a snapshot consistent with the BuildAndCommit CAS
	// that follows. Capture the version so we can detect a cross-process
	// commit landing between this read and BuildAndCommit's own ReadRoot
	// (BuildAndCommit's CAS only catches commits AFTER its read; the
	// window before its read is the race the post-commit version-skip
	// check below guards against).
	r2, err := repo.Open(ctx, eng.Store, tenant, repoID)
	if err != nil {
		writeReceiveReport(w, "internal-error: "+err.Error(), nil, rp.Caps)
		return
	}
	view, err := r2.ReadRoot(ctx)
	if err != nil {
		writeReceiveReport(w, "internal-error: "+err.Error(), nil, rp.Caps)
		return
	}
	preCommitVersion := view.Header.ManifestVersion
	var currentBody manifest.Body
	if err := json.Unmarshal(view.Body, &currentBody); err != nil {
		writeReceiveReport(w, "internal-error: "+err.Error(), nil, rp.Caps)
		return
	}

	// Step 5: validate old-OID for each command.
	statuses := make([]string, len(rp.Updates)) // "" = ok-so-far, else "ng <ref> <reason>"
	allOK := true
	for i, u := range rp.Updates {
		if u.OldOID == nullOID {
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
	if rp.IsAtomic && !allOK {
		for i, u := range rp.Updates {
			if statuses[i] == "" {
				statuses[i] = "ng " + u.Refname + " atomic-batch-failed"
			}
		}
		writeReceiveReport(w, "ok", statuses, rp.Caps)
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
		writeReceiveReport(w, "ok", statuses, rp.Caps)
		return
	}

	// Step 7: index any inbound pack into the bare. IndexPackStrict places
	// pack-<hash>.{pack,idx,keep} under <bare>/objects/pack/. The .keep
	// guards the new objects from a racing GC; BuildAndCommit's
	// removeKeepFiles clears it on the happy path. On error paths below
	// we leave the .keep + pack in place — a subsequent successful push
	// or stale-detection rebuild reconciles. (Cleaner cleanup is deferred
	// to M9.)
	if rp.PackPath != "" {
		if _, err := gitcli.IndexPackStrict(ctx, m.BareDir(), rp.PackPath); err != nil {
			writeReceiveReport(w, "invalid-pack: "+err.Error(), nil, rp.Caps)
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
	for i, u := range rp.Updates {
		if statuses[i] == "" && u.NewOID != nullOID {
			newOIDs = append(newOIDs, u.NewOID)
		}
	}
	if len(newOIDs) > 0 {
		out, err := gitcli.RevListNotAll(ctx, m.BareDir(), newOIDs)
		if err != nil {
			writeReceiveReport(w, "missing-connectivity: "+err.Error(), nil, rp.Caps)
			return
		}
		if rp.PackPath == "" && len(out) > 0 {
			writeReceiveReport(w, "missing-connectivity: pack required for new objects", nil, rp.Caps)
			return
		}
	}

	// Step 9: build the refUpdates map for accepted commands.
	refUpdates := map[string]string{}
	for i, u := range rp.Updates {
		if statuses[i] != "" {
			continue
		}
		refUpdates[u.Refname] = u.NewOID
	}
	if len(refUpdates) == 0 {
		// Defensive — covered by the allFailed branch above, but the
		// invariant matters: never call BuildAndCommit with an empty map
		// and an existing repo (it would CAS a no-op manifest version).
		writeReceiveReport(w, "ok", statuses, rp.Caps)
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
	for i, u := range rp.Updates {
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
			for j, uu := range rp.Updates {
				if statuses[j] == "" {
					statuses[j] = "ng " + uu.Refname + " internal-mirror-error"
				}
			}
			writeReceiveReport(w, "ok", statuses, rp.Caps)
			return
		}
	}

	// Step 10: BuildAndCommit. This repacks the bare into a canonical pack,
	// uploads pack/idx/.bvom/.bvcg, and CAS-commits a new manifest body.
	// Stale-manifest losers (concurrent pushes that won the CAS race) are
	// surfaced via err message "stale manifest" / "lost CAS".
	//
	// Actor: pull from eng.Actor so the tx record carries per-user
	// attribution. After M4 Task 18, receive-pack always runs behind the
	// auth middleware with ActionWrite, so a non-nil actor is the expected
	// path; the "anonymous" fallback is defensive only (RunAuth's Decide
	// rejects nil-actor writes with 401 before we get here).
	actorName := "anonymous"
	if a := eng.Actor; a != nil {
		switch {
		case a.Name != "":
			actorName = a.Name
		case a.UserID != "":
			actorName = a.UserID
		}
	}

	// Build the M10 .bvrd delta patcher. Captures the pre-push body and
	// the accepted ref updates so the patcher can build the delta from
	// the new commits introduced by this push.
	acceptedUpdates := make([]updateCommand, 0, len(rp.Updates))
	for i, u := range rp.Updates {
		if statuses[i] == "" {
			acceptedUpdates = append(acceptedUpdates, u)
		}
	}
	deltaPatcher := makeDeltaPatcher(eng, m.BareDir(), acceptedUpdates, preCommitVersion)

	if _, err := importer.BuildAndCommit(ctx, eng.Store, tenant, repoID, m.BareDir(), refUpdates, actorName, deltaPatcher); err != nil {
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
		for i, u := range rp.Updates {
			if statuses[i] == "" {
				statuses[i] = "ng " + u.Refname + " " + reason
			}
		}
		writeReceiveReport(w, "ok", statuses, rp.Caps)
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
		for i, u := range rp.Updates {
			if statuses[i] == "" {
				statuses[i] = "ok " + u.Refname
			}
		}
		writeReceiveReport(w, "ok", statuses, rp.Caps)
		return
	}
	if viewAfter.Header.ManifestVersion > preCommitVersion+1 {
		// Race detected: at least one concurrent commit landed before
		// BuildAndCommit's read, so our updates may have stomped a
		// concurrent change. Surface as stale-manifest. Mark stale so
		// the next request rebuilds from the authoritative root.
		markMirrorStale(m)
		for i, u := range rp.Updates {
			if statuses[i] == "" {
				statuses[i] = "ng " + u.Refname + " stale-manifest"
			}
		}
		writeReceiveReport(w, "ok", statuses, rp.Caps)
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
	for i, u := range rp.Updates {
		if statuses[i] == "" {
			statuses[i] = "ok " + u.Refname
		}
	}
	writeReceiveReport(w, "ok", statuses, rp.Caps)
}

// makeDeltaPatcher returns an importer.BodyPatcher that builds and uploads a
// .bvrd reachability delta for the push, then appends it to the manifest body.
//
// If the pre-push manifest has no base index (ErrNoIndex — legacy repo), the
// patcher is a no-op: the draft body is returned unchanged, skipping delta
// production. Errors from delta build/upload abort the push (BuildAndCommit
// propagates them as a commit failure).
func makeDeltaPatcher(eng *EngineRequest, bareDir string, acceptedUpdates []updateCommand, prePushVersion uint64) importer.BodyPatcher {
	return func(ctx context.Context, freshPrevBody manifest.Body, draft manifest.Body, newOIDs []string) (manifest.Body, error) {
		// CRITICAL: seed the prior reachability chain into the draft body
		// BEFORE any early-return path that might commit draft unchanged.
		// BuildAndCommit constructs draft from scratch on every push with only
		// ObjectMap and CommitGraph; without this seed, any early-return branch
		// (fully-rejected push, delete-only push) would silently wipe the prior
		// chain and leave Reachability == nil in the committed manifest body.
		// The seed fires unconditionally so all commit paths carry the chain.
		if draft.Indexes.Reachability == nil && freshPrevBody.Indexes.Reachability != nil {
			rcopy := *freshPrevBody.Indexes.Reachability
			rcopy.Deltas = append([]manifest.IndexRef(nil), freshPrevBody.Indexes.Reachability.Deltas...)
			draft.Indexes.Reachability = &rcopy
		}

		// Nothing to record when the push accepted no updates and introduced
		// no new objects (e.g. a fully-rejected batch).
		// Uploading an empty .bvrd wastes a delta slot and can prematurely
		// trigger compaction.
		if len(acceptedUpdates) == 0 && len(newOIDs) == 0 {
			return draft, nil // chain preserved by seed above
		}

		k, err := keys.NewRepo(eng.Tenant, eng.Repo)
		if err != nil {
			return manifest.Body{}, err
		}

		// Load gen lookup from the fresh pre-commit body supplied by
		// BuildAndCommit (not the outer-captured snapshot, which may be
		// stale if a concurrent commit landed before BuildAndCommit's own
		// ReadRoot). ErrNoIndex = legacy repo, skip delta production.
		gl, err := reachability.LoadGenLookup(ctx, eng.Store, k, freshPrevBody)
		if err != nil {
			if errors.Is(err, reachability.ErrNoIndex) {
				return draft, nil // legacy repo — no delta machinery yet
			}
			return manifest.Body{}, err
		}

		// Extract canonical pack OIDs from the draft body for the delta's
		// Packs field (records which packs cover the new commits).
		packIDs := make([]pack.OID, 0, len(draft.Packs))
		for _, pe := range draft.Packs {
			oid, e := pack.ParseOID(pe.PackID)
			if e == nil {
				packIDs = append(packIDs, oid)
			}
		}

		d, err := buildDelta(ctx, bareDir, newOIDs, gl, acceptedUpdates, packIDs)
		if err != nil {
			return manifest.Body{}, err
		}

		deltaRef, err := uploadDelta(ctx, eng.Store, k, d)
		if err != nil {
			return manifest.Body{}, err
		}

		// Append the new delta to the Reachability chain.
		// The chain was already seeded from freshPrevBody at the top of this
		// function. If still nil here it means no prior chain exists (truly
		// fresh / legacy repo that passed through ErrNoIndex earlier — but
		// only if LoadGenLookup returned nil without ErrNoIndex, which can
		// happen for repos initialised without a base index). Start a fresh chain
		// with BaseManifest set to the pre-push manifest version so operator
		// arithmetic (manifest_version - base) yields a meaningful delta-depth.
		if draft.Indexes.Reachability == nil {
			draft.Indexes.Reachability = &manifest.ReachabilityRef{
				BaseManifest: fmt.Sprintf("v%08d", prePushVersion),
			}
		}
		// Make a copy of the ReachabilityRef to avoid mutating the original.
		rc := *draft.Indexes.Reachability
		rc.Deltas = append(append([]manifest.IndexRef{}, rc.Deltas...), deltaRef)
		draft.Indexes.Reachability = &rc
		return draft, nil
	}
}
