// BuildAndCommit applies an inbound push to an existing bucketvcs repo.
// It is the shared finalize pipeline for both Import (initial population)
// and gateway receive-pack (subsequent pushes). See spec §3.6 + M3 push
// design for context.
package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// nullOIDHex is git's sentinel "no object" hash (40 zeros, SHA-1) used by
// the receive-pack wire protocol to mean "delete this ref".
const nullOIDHex = "0000000000000000000000000000000000000000"

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
func BuildAndCommit(
	ctx context.Context,
	store storage.ObjectStore,
	tenantID, repoID string,
	bareDir string,
	refUpdates map[string]string,
	actor string,
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
	var currentBody manifest.Body
	if err := json.Unmarshal(view.Body, &currentBody); err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: unmarshal current body: %w", err)
	}
	startVersion := view.Header.ManifestVersion

	// Compute target Refs: merge refUpdates into currentBody.Refs.
	newRefs, err := mergeRefs(currentBody.Refs, refUpdates)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: merge refs: %w", err)
	}

	// Refuse to commit a body where DefaultBranch points at a deleted ref.
	// This matches git's "deny-delete-current" behavior. M14 may add an
	// override flag.
	//
	// Only reject when the default branch actually EXISTED before this push
	// and is absent after the merge. An empty/unborn repo legitimately has
	// DefaultBranch set (e.g. "refs/heads/main" from Import) without the
	// ref present, and a first push that creates only a non-default branch
	// must not be misread as "deleting" the unborn default.
	if currentBody.DefaultBranch != "" {
		_, hadBefore := currentBody.Refs[currentBody.DefaultBranch]
		_, hasAfter := newRefs[currentBody.DefaultBranch]
		if hadBefore && !hasAfter {
			return nil, fmt.Errorf("BuildAndCommit: refuses to delete current default branch %q", currentBody.DefaultBranch)
		}
	}

	// Sanity defense: every non-deleted ref OID in newRefs must resolve
	// in bareDir. The gateway should have validated this pre-call; we
	// re-check defensively because a missing object now means the .bvom
	// we are about to upload will not cover the ref's tip.
	for ref, oid := range newRefs {
		if _, kerr := gitcli.RevParseObjectKind(ctx, bareDir, oid); kerr != nil {
			return nil, fmt.Errorf("importer: BuildAndCommit: ref %s OID %s not in bareDir: %w", ref, oid, kerr)
		}
	}

	k, err := keys.NewRepo(tenantID, repoID)
	if err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: keys: %w", err)
	}

	// Empty target (all refs deleted) — commit a body with no packs/indexes.
	if len(newRefs) == 0 {
		return commitEmptyBody(ctx, r, currentBody, startVersion, actor)
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

	idx, err := buildIndexesFromPack(ctx, canonicalPack, canonicalIdx, packID, newRefs)
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
		Refs:          newRefs,
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
		return bodyBytes, nil
	}); err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: commit: %w", err)
	}

	return &body, nil
}

// commitEmptyBody handles the all-refs-deleted case: nothing to repack,
// no pack to upload, body has empty packs/indexes/refs.
func commitEmptyBody(ctx context.Context, r *repo.Repo, prev manifest.Body, startVersion uint64, actor string) (*manifest.Body, error) {
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
		return bodyBytes, nil
	}); err != nil {
		return nil, fmt.Errorf("importer: BuildAndCommit: commit: %w", err)
	}
	return &body, nil
}

// mergeRefs returns prev+updates with deletes (null OID) applied. prev is
// not mutated. updates with empty OID are also treated as deletes (some
// callers normalize away the null hex string). Each update refname is
// syntactically validated — gateway and importer entry points are the
// boundary where untrusted ref names enter, so we reject obviously-bad
// names here as a defense in depth in case a caller forgets to validate.
func mergeRefs(prev, updates map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(prev)+len(updates))
	for k, v := range prev {
		out[k] = v
	}
	for ref, oid := range updates {
		if ref == "" {
			return nil, fmt.Errorf("empty refname in updates")
		}
		if !validFullRefName(ref) {
			return nil, fmt.Errorf("invalid refname in updates: %q", ref)
		}
		if oid == "" || oid == nullOIDHex {
			delete(out, ref)
			continue
		}
		out[ref] = oid
	}
	return out, nil
}

// removeKeepFiles deletes any *.keep files left in <bareDir>/objects/pack
// by IndexPackStrict. With keeps in place, `git pack-objects --revs --all`
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
