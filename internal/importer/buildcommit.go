// BuildAndCommit applies an inbound push to an existing bucketvcs repo.
// It is the shared finalize pipeline for both Import (initial population)
// and gateway receive-pack (subsequent pushes). See spec §3.6 + M3 push
// design for context.
package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/oidconst"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// BodyPatcher is an optional callback invoked by BuildAndCommit after all
// pack/idx/.bvom/.bvcg artifacts are uploaded and a draft Body has been
// assembled, but BEFORE the body is marshaled and CAS-committed.
//
// The patcher receives the current manifest body (pre-push), the draft new
// body, and the set of object OIDs new in this push (output of
// RevListNotAll). It may perform additional uploads (e.g. a .bvrd delta)
// and return a modified body. Returning an error aborts the push before
// the manifest is committed.
//
// The patcher is called exactly ONCE per BuildAndCommit invocation (not
// on CAS retries, since BuildAndCommit does not retry — it returns an
// error on CAS version mismatch). Callers that need CAS-retry semantics
// must re-invoke BuildAndCommit from scratch.
type BodyPatcher func(ctx context.Context, prevBody manifest.Body, draft manifest.Body, newOIDs []string) (manifest.Body, error)

// BuildAndCommit applies a push to a bucketvcs repo. It assumes:
//
//   - The repo already exists (created by an earlier Import).
//   - bareDir is a clean, manifest-current local mirror containing the
//     existing committed objects PLUS the inbound validated pack and any
//     new ref tips already applied (typically populated by mirror.Sync
//     and mirror.IngestPack before this call).
//   - refUpdates maps refname -> new OID. The null OID (40 zeros) means
//     delete. Refs not present in the map are preserved as-is. Refs in
//     the map override existing values with the same name.
//   - The caller holds whatever serialization lock the storage demands
//     (typically the mirror's Lock()).
//
// On success: BuildAndCommit repacks bareDir into one canonical pack,
// uploads pack+idx+.bvom+.bvcg to the bucket, constructs a new manifest
// body merging refUpdates into the existing Refs, and commits via M1's
// transaction kernel.
//
// Returns the committed manifest body. Old packs become orphans in the
// bucket; M8 GC sweeps them.
//
// Pack non-determinism: git's pack-objects does not produce a deterministic
// pack ID for identical inputs across runs (delta selection can vary).
// Two concurrent BuildAndCommit calls with the same merged refs may upload
// DIFFERENT pack files before CAS resolves. The losing call's pack becomes
// an orphan in the bucket. M8 GC sweeps these.
//
// Cost: O(repo size) per push. For M3 OSS scope this is acceptable;
// incremental .bvom merge is a future M9 optimization.
//
// patcher may be nil (no-op). See BodyPatcher for semantics.
func BuildAndCommit(
	ctx context.Context,
	store storage.ObjectStore,
	tenantID, repoID string,
	bareDir string,
	refUpdates map[string]string,
	actor string,
	patcher BodyPatcher,
) (*manifest.Body, error) {
	if store == nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: nil store")
	}
	if bareDir == "" {
		return nil, fmt.Errorf("importer: BuildAndCommit: empty bareDir")
	}

	r, err := repo.Open(ctx, store, tenantID, repoID)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: open repo: %w", err)
	}

	view, err := r.ReadRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: read root: %w", err)
	}
	currentBody, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: unmarshal current body: %w", err)
	}
	startVersion := view.Header.ManifestVersion

	k, err := keys.NewRepo(tenantID, repoID)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: keys: %w", err)
	}

	// M12: route the ref update through refstore.Stage. Stage produces
	// either an inline map (v1 layout) or a list of ShardWrites +
	// RefShards (v2 layout) depending on the current body shape; the
	// Mode field discriminates. The Stage's NewShardObjects are written
	// to the bucket inside the r.Commit buildBody callback below (spec's
	// "Phase A" pre-CAS shard publish).
	//
	// refstore.Stage does NOT enforce ref-name syntax (per its contract),
	// so we validate here — gateway and importer entry points are the
	// boundary where untrusted ref names enter; refusing obviously-bad
	// names is a defense in depth in case a caller forgets to validate.
	for ref := range refUpdates {
		if ref == "" {
			return nil, fmt.Errorf("importer: BuildAndCommit: empty refname in updates")
		}
		if !validFullRefName(ref) {
			return nil, fmt.Errorf("importer: BuildAndCommit: invalid refname in updates: %q", ref)
		}
	}
	rs, err := refstore.New(ctx, store, k, &currentBody)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: refstore: %w", err)
	}
	stage, err := rs.Stage(ctx, refUpdates)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: stage: %w", err)
	}

	// Refuse to commit a body where DefaultBranch points at a deleted ref.
	// This matches git's "deny-delete-current" behavior. M14 may add an
	// override flag.
	//
	// Only reject when the default branch actually EXISTED before this push
	// and is absent after the stage. An empty/unborn repo legitimately has
	// DefaultBranch set (e.g. "refs/heads/main" from Import) without the
	// ref present, and a first push that creates only a non-default branch
	// must not be misread as "deleting" the unborn default.
	//
	// Lookup routing: rs.Lookup answers the pre-stage state authoritatively
	// (one shard read for sharded mode, O(1) for inline). stage.Lookup
	// answers the post-stage state in-memory; in sharded mode it can
	// return ErrLookupNotInStage if the default branch's shard was not
	// touched by this stage, in which case the post-stage value equals
	// the pre-stage value (hadBefore).
	if currentBody.DefaultBranch != "" {
		_, hadBefore, err := rs.Lookup(ctx, currentBody.DefaultBranch)
		if err != nil {
			return nil, fmt.Errorf("importer: lookup default before: %w", err)
		}
		_, hasAfter, slErr := stage.Lookup(currentBody.DefaultBranch)
		if slErr != nil && !errors.Is(slErr, refstore.ErrLookupNotInStage) {
			return nil, fmt.Errorf("importer: lookup default after (stage): %w", slErr)
		}
		if errors.Is(slErr, refstore.ErrLookupNotInStage) {
			// Default branch's shard is unchanged — its post-stage value
			// equals the pre-stage value.
			hasAfter = hadBefore
		}
		if hadBefore && !hasAfter {
			return nil, fmt.Errorf("BuildAndCommit: refuses to delete current default branch %q", currentBody.DefaultBranch)
		}
	}

	// Sanity defense: every non-deleted ref OID in the effective post-
	// stage view must resolve in bareDir. The gateway should have
	// validated this pre-call; we re-check defensively because a
	// missing object now means the .bvom we are about to upload will
	// not cover the ref's tip.
	//
	// We compute the effective post-stage flat ref map here (rather
	// than later) so the same map can be reused for buildIndexesFromPack
	// below — and so the loop count matches the pre-M12 mergeRefs-based
	// iteration count, preserving the pack-objects timing characteristics
	// the tests depend on.
	//
	// For sharded mode, hoist the rs.List call here so that the patcher
	// block (which also needs the pre-push ref set to compute excludes)
	// can reuse the same listing — at most one rs.List per
	// BuildAndCommit attempt regardless of whether a patcher is attached.
	var prePushListedRefs map[string]string
	if stage.Mode == refstore.ModeSharded {
		prePushListedRefs, err = rs.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("importer: BuildAndCommit: list pre-push refs: %w", err)
		}
	}
	effectiveRefs, err := buildEffectiveRefs(ctx, rs, stage, refUpdates, prePushListedRefs)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: effective refs: %w", err)
	}
	for ref, oid := range effectiveRefs {
		if _, kerr := gitcli.RevParseObjectKind(ctx, bareDir, oid); kerr != nil {
			return nil, fmt.Errorf("importer: BuildAndCommit: ref %s OID %s not in bareDir: %w", ref, oid, kerr)
		}
	}

	// Effective post-stage ref count and pack-objects inputs: differ by
	// mode. Inline mode hands us the full map; sharded mode hands us
	// the new shard list (untouched + changed). For repack purposes we
	// need at least one tip to pack against; gather them in mode-agnostic
	// form below.
	emptyTarget := false
	switch stage.Mode {
	case refstore.ModeInline:
		emptyTarget = len(stage.NewInlineRefs) == 0
	case refstore.ModeSharded:
		emptyTarget = len(stage.NewRefShards) == 0
	}

	// Empty target (all refs deleted) — commit a body with no packs/indexes.
	if emptyTarget {
		return commitEmptyBody(ctx, r, currentBody, startVersion, actor, stage, store)
	}

	// Repack bareDir to a single canonical pack covering all reachable
	// objects. Place output in a temp dir OUTSIDE bareDir so we don't
	// pollute the mirror with the canonical pack — the mirror's pack
	// lives at <bare>/objects/pack/, which is the inbound pack that
	// IngestPack copied; the canonical we upload is a separate artifact.
	tmpRepackDir, err := os.MkdirTemp("", "bucketvcs-repack-")
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: tmpdir: %w", err)
	}
	defer os.RemoveAll(tmpRepackDir)

	if err := removeKeepFiles(bareDir); err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: remove .keep: %w", err)
	}

	prefix := filepath.Join(tmpRepackDir, "pack")
	packID, err := gitcli.PackObjectsAll(ctx, bareDir, prefix)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: pack-objects: %w", err)
	}
	canonicalPack := prefix + "-" + packID + ".pack"
	canonicalIdx := prefix + "-" + packID + ".idx"

	// effectiveRefs was computed during the sanity defense above.
	// buildIndexesFromPack needs the (refname → oid) map of all commit
	// tips reachable in the canonical pack so its .bvcg can list them.
	idx, err := buildIndexesFromPack(ctx, canonicalPack, canonicalIdx, packID, effectiveRefs)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: build indexes: %w", err)
	}

	// Upload pack/idx/.bvom/.bvcg via PutIfAbsent.
	//
	// Pack/idx use the strict uploadFile (errors on ErrAlreadyExists). Note
	// that the pack_id returned by pack-objects is git's trailing SHA-1
	// over the assembled pack BYTES (header + sorted objects + their
	// compressed deltas), NOT a hash of the abstract object set — repeated
	// repacks of the same reachable set normally yield different pack_ids
	// because delta search is non-deterministic across threads / memory
	// conditions. So in the common case the canonical key for a fresh
	// pack_id is empty and PutIfAbsent succeeds.
	//
	// If ErrAlreadyExists DOES fire here, it indicates a collision against
	// pre-existing bytes (orphan from a crashed prior run, replay, or an
	// extremely lucky deterministic repack). We surface it as an error
	// because our locally-built .bvom encodes pack offsets specific to OUR
	// bytes; committing a manifest whose .bvom expects our offsets but
	// whose pack key resolves to different stored bytes would corrupt
	// object lookup. CAS-loser detection in the mutator catches the
	// concurrent-racer variant before any damage.
	//
	// .bvom/.bvcg are content-addressed by SHA-256 of their bytes, so
	// ErrAlreadyExists trivially means "same bytes already there".
	if err := uploadFile(ctx, store, canonicalPack, k.CanonicalPackKey(packID)); err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: upload pack: %w", err)
	}
	if err := uploadFile(ctx, store, canonicalIdx, k.PackIdxKey(packID, "canonical")); err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: upload idx: %w", err)
	}
	bvomKey := k.ObjectMapKey(idx.ObjectMapHash)
	if err := uploadBytes(ctx, store, idx.ObjectMapBytes, bvomKey); err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: upload .bvom: %w", err)
	}
	bvcgKey := k.CommitGraphKey(idx.CommitGraphHash)
	if err := uploadBytes(ctx, store, idx.CommitGraphBytes, bvcgKey); err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: upload .bvcg: %w", err)
	}

	body := manifest.Body{
		DefaultBranch: currentBody.DefaultBranch,
		Packs: []manifest.PackEntry{{
			PackID:      packID,
			PackKey:     k.CanonicalPackKey(packID),
			IdxKey:      k.PackIdxKey(packID, "canonical"),
			SizeBytes:   idx.PackSizeBytes,
			ObjectCount: idx.ObjectCount,
		}},
		Indexes: manifest.Indexes{
			ObjectMap:   &manifest.IndexRef{Key: bvomKey, Hash: idx.ObjectMapHash},
			CommitGraph: &manifest.IndexRef{Key: bvcgKey, Hash: idx.CommitGraphHash},
		},
		Bundles: currentBody.Bundles,
	}

	// M12: select inline vs sharded layout based on the staged Mode.
	// manifest.UnmarshalBody enforces that Refs and RefShards are
	// mutually exclusive, so the unselected side must be left nil/zero.
	switch stage.Mode {
	case refstore.ModeInline:
		body.Refs = stage.NewInlineRefs
		body.RefShards = nil
		body.RefSharding = ""
	case refstore.ModeSharded:
		body.Refs = nil
		body.RefShards = stage.NewRefShards
		body.RefSharding = "hash_v1"
	}

	// Optional body-patcher hook (e.g. M10 .bvrd delta production).
	// Runs after all artifacts are uploaded, before the manifest CAS.
	// The patcher receives the set of new object OIDs introduced by this
	// push (commits reachable from new tips but not old tips).
	if patcher != nil {
		// Compute "new commits" as: git rev-list <new_tips> --not <all_pre_push_tips>
		// where new_tips are the incoming ref OIDs and all_pre_push_tips are
		// ALL pre-push ref OIDs (not just those being updated). This ensures
		// that a push creating a new branch pointing to an already-indexed
		// commit produces an empty delta — the commit is already reachable
		// from an existing ref.
		//
		// Pre-push tips come from the RefStore (mode-agnostic). For inline
		// repos this is cheap; for sharded repos we reuse the prePushListedRefs
		// already fetched above (at most one rs.List per BuildAndCommit attempt).
		prePushRefs := prePushListedRefs
		if prePushRefs == nil {
			// Inline mode: prePushListedRefs was not populated above; list now.
			prePushRefs, err = rs.List(ctx)
			if err != nil {
				return nil, fmt.Errorf("importer: BuildAndCommit: list pre-push refs: %w", err)
			}
		}
		var newTipArgs []string
		// Seed excludes from every existing ref (these are all "haves" relative
		// to this push). This correctly handles the case where a new ref points
		// to a commit already reachable from another pre-existing ref.
		excludeArgs := make([]string, 0, len(prePushRefs))
		for _, oldOID := range prePushRefs {
			if oldOID == "" || oldOID == oidconst.NullOIDHex {
				continue
			}
			excludeArgs = append(excludeArgs, "^"+oldOID)
		}
		for _, oid := range refUpdates {
			if oid == "" || oid == oidconst.NullOIDHex {
				continue
			}
			newTipArgs = append(newTipArgs, oid)
		}
		var newOIDs []string
		if len(newTipArgs) > 0 {
			newOIDs, err = revListCommitsOnly(ctx, bareDir, newTipArgs, excludeArgs)
			if err != nil {
				return nil, fmt.Errorf("importer: BuildAndCommit: revlist new commits: %w", err)
			}
		}
		body, err = patcher(ctx, currentBody, body, newOIDs)
		if err != nil {
			return nil, fmt.Errorf("importer: BuildAndCommit: body patcher: %w", err)
		}
	}

	bodyBytes, err := manifest.MarshalBody(body)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: marshal body: %w", err)
	}

	commitTxBody := tx.Body{Type: "push", Actor: actor}
	if _, err := r.Commit(ctx, commitTxBody, func(prev *repo.RootView) ([]byte, error) {
		// CAS-loser detection: if the manifest version has advanced since
		// our ReadRoot, a concurrent push won the race and our pre-
		// computed body is stale (its refs were merged against an old
		// snapshot). Fail rather than overwrite.
		if prev.Header.ManifestVersion != startVersion {
			return nil, fmt.Errorf("importer: BuildAndCommit: stale manifest (started at v%d, now v%d): concurrent push lost the CAS race",
				startVersion, prev.Header.ManifestVersion)
		}
		// M12 Phase A: write every NewShardObject to the bucket BEFORE
		// returning the new body bytes. PutIfAbsent is content-
		// addressed (key embeds the SHA-256 of contents) so concurrent
		// identical writes collapse to a single object; ErrAlreadyExists
		// is swallowed — the bytes are the same.
		//
		// Retry semantics: stage is captured from the outer scope and is
		// NOT recomputed if r.Commit invokes this callback more than
		// once. That is safe because the CAS-loser guard above returns
		// immediately on any version mismatch — productive retries from
		// within the callback are impossible, so the same stage's
		// content-addressed PutIfAbsent calls are at worst idempotent
		// no-ops on a retry. If a concurrent push wins, the caller
		// re-enters BuildAndCommit from the top, which recomputes the
		// stage against fresh state.
		//
		// A failed CAS leaves the shards in the bucket as orphans;
		// Phase 7 GC will reclaim them because no manifest references
		// them.
		for _, w := range stage.NewShardObjects {
			if _, perr := store.PutIfAbsent(ctx, w.Key, bytes.NewReader(w.Contents), nil); perr != nil && !errors.Is(perr, storage.ErrAlreadyExists) {
				return nil, fmt.Errorf("importer: PutIfAbsent shard %s: %w", w.Key, perr)
			}
		}
		return bodyBytes, nil
	}); err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: commit: %w", err)
	}

	return &body, nil
}

// commitEmptyBody handles the all-refs-deleted case: nothing to repack,
// no pack to upload, body has empty packs/indexes/refs.
//
// stage is passed so the empty case can still PutIfAbsent any
// NewShardObjects (a deletion-only stage could in principle produce a
// non-empty shard write if some shard still has refs after the merge
// — but for the empty-target branch every shard collapses to "{}" and
// is dropped, so stage.NewShardObjects is expected empty here. We
// still loop defensively so future Stage variants don't silently lose
// writes).
//
// An empty-target body always degrades to inline-empty form (Refs={},
// no RefShards, no RefSharding tag). We don't preserve the v2 layout
// for empty bodies because there is nothing to shard and the validator
// rejects RefSharding='hash_v1' with no RefShards. We degrade to
// inline layout by clearing the tag.
func commitEmptyBody(ctx context.Context, r *repo.Repo, prev manifest.Body, startVersion uint64, actor string, stage refstore.Stage, store storage.ObjectStore) (*manifest.Body, error) {
	body := manifest.Body{
		DefaultBranch: prev.DefaultBranch,
		Refs:          map[string]string{},
		Packs:         nil,
		Indexes:       manifest.Indexes{},
		Bundles:       prev.Bundles,
	}
	bodyBytes, err := manifest.MarshalBody(body)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: marshal empty body: %w", err)
	}
	commitTxBody := tx.Body{Type: "push", Actor: actor}
	if _, err := r.Commit(ctx, commitTxBody, func(prv *repo.RootView) ([]byte, error) {
		if prv.Header.ManifestVersion != startVersion {
			return nil, fmt.Errorf("importer: BuildAndCommit: stale manifest (started at v%d, now v%d): concurrent push lost the CAS race",
				startVersion, prv.Header.ManifestVersion)
		}
		// Defensive: write any NewShardObjects produced by Stage. For
		// an all-empty target every shard collapsed and was dropped, so
		// this loop is normally a no-op.
		for _, w := range stage.NewShardObjects {
			if _, perr := store.PutIfAbsent(ctx, w.Key, bytes.NewReader(w.Contents), nil); perr != nil && !errors.Is(perr, storage.ErrAlreadyExists) {
				return nil, fmt.Errorf("importer: PutIfAbsent shard %s: %w", w.Key, perr)
			}
		}
		return bodyBytes, nil
	}); err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: commit: %w", err)
	}
	return &body, nil
}

// buildEffectiveRefs produces a flat post-stage (refname → oid) map.
//
// For inline-mode stages this is just stage.NewInlineRefs (Stage
// already merged updates against the pre-stage map).
//
// For sharded-mode stages we materialize the flat view by applying
// refUpdates on top of the pre-stage listing. The pre-stage listing is
// supplied via preListed (already fetched by BuildAndCommit before this
// call to avoid a redundant rs.List round-trip). If preListed is nil
// and the mode is sharded, buildEffectiveRefs falls back to calling
// rs.List itself — but in practice BuildAndCommit always supplies it
// for sharded mode.
//
// This is required because the .bvcg builder downstream
// (buildIndexesFromPack → buildTipsFromRefs) needs every tip's OID
// to enumerate commit-graph tips; the sharded Stage only carries
// shards' write-side state, not the unchanged-shard contents in
// memory.
//
// refUpdates uses the same delete convention as refstore.Stage: empty
// OID or 40-zero oidconst.NullOIDHex means delete; any other 40-hex value is
// an upsert.
//
// Precondition: refUpdates has already been refname-validated by the
// BuildAndCommit prelude. buildEffectiveRefs does no further validation.
func buildEffectiveRefs(ctx context.Context, rs refstore.RefStore, stage refstore.Stage, refUpdates map[string]string, preListed map[string]string) (map[string]string, error) {
	if stage.Mode == refstore.ModeInline {
		out := make(map[string]string, len(stage.NewInlineRefs))
		for k, v := range stage.NewInlineRefs {
			out[k] = v
		}
		return out, nil
	}
	// Sharded mode: materialize from pre-stage list + apply updates.
	pre := preListed
	if pre == nil {
		var err error
		pre, err = rs.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("importer: list pre-stage refs: %w", err)
		}
	}
	out := make(map[string]string, len(pre)+len(refUpdates))
	for k, v := range pre {
		out[k] = v
	}
	for ref, oid := range refUpdates {
		if ref == "" {
			return nil, fmt.Errorf("empty refname in updates")
		}
		if oid == "" || oid == oidconst.NullOIDHex {
			delete(out, ref)
			continue
		}
		out[ref] = oid
	}
	return out, nil
}

// revListCommitsOnly returns commit OIDs reachable from tips but not from
// excludes (in "^<oid>" form). Unlike the --objects form it omits trees and
// blobs, so it's appropriate when the caller only needs commits
// (e.g. building a .bvrd reachability delta).
func revListCommitsOnly(ctx context.Context, bareDir string, tips, excludes []string) ([]string, error) {
	return gitcli.RevListCommitsOnly(ctx, bareDir, tips, excludes)
}

// removeKeepFiles deletes any *.keep files left in <bareDir>/objects/pack
// by IndexPackStrict. With keeps in place, git pack-objects --revs --all
// in modern git will still see all reachable objects (keeps gate `git
// repack`, not `pack-objects`), but removing them defensively makes the
// repack a clean single-pack consolidation regardless of git version
// quirks. The keeps existed only to prevent racing GC; once we are
// repacking under the caller's lock that protection is no longer needed.
func removeKeepFiles(bareDir string) error {
	packDir := filepath.Join(bareDir, "objects", "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".keep") {
			continue
		}
		if err := os.Remove(filepath.Join(packDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
