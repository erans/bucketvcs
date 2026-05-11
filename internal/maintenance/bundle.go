package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// BundleArtifact is the output of GenerateBundleArtifact: the uploaded
// bundle's manifest entry plus filesystem paths the caller is expected
// to clean up after CAS-merge concludes (success or failure).
type BundleArtifact struct {
	Entry manifest.BundleEntry

	// LocalBundle is the filesystem path to the just-built bundle file.
	// Lives inside LocalDir.
	LocalBundle string

	// LocalDir is the temporary directory that holds LocalBundle.
	// Removing LocalDir is the caller's cleanup contract — removing
	// only LocalBundle would orphan the parent directory.
	LocalDir string
}

// GenerateBundleArtifact materializes a bundle for the given ref against
// a pre-materialized bare repo at mirrorDir, uploads the bundle + sidecar
// to content-addressed keys under the repo's bundles/ prefix, and
// returns the constructed BundleEntry.
//
// The caller is responsible for:
//   - materializing mirrorDir before calling (reuse repack's mirror)
//   - removing art.LocalDir once CAS-merge concludes
//   - CAS-merging art.Entry into the manifest (RunBundleCASMerge)
func GenerateBundleArtifact(
	ctx context.Context,
	mirrorDir string,
	ref string,
	store storage.ObjectStore,
	rkeys *keys.Repo,
	manifestVersion uint64,
	now time.Time,
) (BundleArtifact, error) {
	// 1. Resolve tip OID via git rev-parse against the mirror.
	tipOID, err := gitcli.RevParse(ctx, mirrorDir, ref)
	if err != nil {
		return BundleArtifact{}, fmt.Errorf("bundle: rev-parse %s: %w", ref, err)
	}
	tipOID = strings.TrimSpace(tipOID)
	if len(tipOID) != 40 {
		return BundleArtifact{}, fmt.Errorf("bundle: rev-parse returned %q (not 40-hex)", tipOID)
	}
	if _, err := hex.DecodeString(tipOID); err != nil {
		return BundleArtifact{}, fmt.Errorf("bundle: rev-parse returned %q (not hex): %w", tipOID, err)
	}

	// 2. Build the bundle into a temp dir. The dir is removed on any
	//    error path via the deferred cleanup; the success path hands
	//    the dir to the caller for later removal.
	tmpDir, err := os.MkdirTemp("", "bvcs-bundle-")
	if err != nil {
		return BundleArtifact{}, fmt.Errorf("bundle: tmpdir: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	bundlePath := filepath.Join(tmpDir, "out.bundle")
	if err := gitcli.BundleCreate(ctx, mirrorDir, bundlePath, ref); err != nil {
		return BundleArtifact{}, fmt.Errorf("bundle: BundleCreate: %w", err)
	}

	// 3. Stream once for SHA-256 + size.
	sum, size, err := hashAndSize(bundlePath)
	if err != nil {
		return BundleArtifact{}, fmt.Errorf("bundle: hash: %w", err)
	}
	hashHex := hex.EncodeToString(sum)
	bundleHash := "sha256-" + hashHex
	// bundleID is a per-generation identifier (NOT content-addressed) —
	// it embeds manifestVersion so two regenerations of the same content
	// at different manifest versions get distinct IDs in the manifest's
	// BundleEntry list. The BundleKey/SidecarKey are content-addressed
	// (deduplicated across regenerations); ID is the "which CAS-merge
	// produced this entry" tag.
	bundleID := fmt.Sprintf("bundle_%s_%s_%d_%s",
		rkeys.TenantID(), rkeys.RepoID(), manifestVersion, hex.EncodeToString(sum[:4]))

	bundleKey := rkeys.BundleKey(bundleHash)
	sidecarKey := rkeys.BundleManifestKey(bundleHash)

	// 4. Upload the bundle file. ErrAlreadyExists is fine — bundle is
	//    content-addressed, so a byte-identical prior upload satisfies
	//    integrity.
	f, err := os.Open(bundlePath)
	if err != nil {
		return BundleArtifact{}, fmt.Errorf("bundle: open for upload: %w", err)
	}
	_, putErr := store.PutIfAbsent(ctx, bundleKey, f, nil)
	_ = f.Close()
	if putErr != nil && !errors.Is(putErr, storage.ErrAlreadyExists) {
		return BundleArtifact{}, fmt.Errorf("bundle: upload %s: %w", bundleKey, putErr)
	}

	// 5. Build + upload sidecar.
	//
	// Sidecar / manifest divergence under regeneration:
	// Bundle and sidecar keys are content-addressed by the bundle file's
	// SHA-256 ("sha256-<hex>"). If the same bundle content is regenerated
	// at a higher manifestVersion, the bundle blob (byte-identical) and
	// the sidecar's mutable fields (CoversManifestVersion, GeneratedAt,
	// ID) would point at different versions. We use PutIfAbsent for both
	// keys, so the sidecar from the first generation "wins" — its
	// CoversManifestVersion / GeneratedAt / ID reflect that earlier
	// moment. The manifest's BundleEntry (which CAS-merge writes from
	// the freshly-constructed `entry` value below) is the source of
	// truth for the current generation; the sidecar is the durable
	// integrity-and-recovery snapshot of the bundle's content-addressed
	// identity, NOT a live mirror of the manifest entry. Callers must
	// read the manifest for current metadata.
	entry := manifest.BundleEntry{
		ID:                    bundleID,
		Kind:                  "full_default",
		BundleKey:             bundleKey,
		SidecarKey:            sidecarKey,
		BundleHash:            bundleHash,
		Ref:                   ref,
		TipOID:                tipOID,
		CoversManifestVersion: manifestVersion,
		ByteSize:              size,
		GeneratedAt:           now.UTC().Format(time.RFC3339),
	}
	sidecarBytes, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return BundleArtifact{}, fmt.Errorf("bundle: marshal sidecar: %w", err)
	}
	if _, err := store.PutIfAbsent(ctx, sidecarKey, bytes.NewReader(sidecarBytes), nil); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
		return BundleArtifact{}, fmt.Errorf("bundle: upload sidecar %s: %w", sidecarKey, err)
	}

	success = true
	return BundleArtifact{Entry: entry, LocalBundle: bundlePath, LocalDir: tmpDir}, nil
}

// hashAndSize streams the file at path through sha256 and returns the
// digest plus the byte count. The file is opened and closed within the
// call; the caller need not pre-open it.
func hashAndSize(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, 0, err
	}
	return h.Sum(nil), n, nil
}

// runBundlePhase orchestrates bundle-refresh: resolve default branch,
// evaluate triggers, generate bundle artifact, CAS-merge BundleEntry
// into the manifest. Reuses mirrorDir (the Phase-1 materialized mirror)
// for bundle generation; an empty mirrorDir is a programmer error in
// the Task 3.7 wiring (Task 3.8's --bundle-only path will materialize
// its own mirror).
//
// Returns *BundleResult and a non-nil error if the phase failed in a
// way the pipeline should log; the pipeline is expected to log the
// error but NOT treat it as a maintenance-run failure (bundle is a
// best-effort acceleration; M9/M10 outcomes still drive the run's
// pass/fail). When the phase short-circuits (no_trigger / skipped),
// the returned error is nil and BundleResult.TriggerReason / Generated
// fields tell the story.
func runBundlePhase(
	ctx context.Context,
	s storage.ObjectStore,
	r *repo.Repo,
	rkeys *keys.Repo,
	opts RunOptions,
	m0 manifest.Body,
	manifestVersion uint64,
	mirrorDir string,
) (*BundleResult, error) {
	started := opts.Now()
	elapsed := func() int64 { return opts.Now().Sub(started).Milliseconds() }

	// 1. Resolve default branch. Failure is benign — the repo simply has
	//    no obvious head to bundle; surface via TriggerReason and let the
	//    pipeline continue without logging.
	ref, err := ResolveDefaultBranch(m0, opts.BundleDefaultBranch)
	if err != nil {
		return &BundleResult{
			TriggerReason: "skipped_no_default_branch",
			DurationMS:    elapsed(),
		}, nil
	}

	// 2. Load reachability set only when the commit-distance trigger is
	//    enabled and we're not forcing a refresh (force skips trigger
	//    evaluation entirely). Reachability load failure is unusual
	//    enough to bubble up so the operator sees it in logs.
	var rset *reachability.Set
	if !opts.Force && opts.Thresholds.BundleCommits > 0 {
		rset, err = reachability.Load(ctx, s, rkeys, m0)
		if err != nil {
			return &BundleResult{
				TriggerReason: "skipped_reachability_load_error",
				ErrorMessage:  err.Error(),
				DurationMS:    elapsed(),
			}, err
		}
	}

	// 3. Evaluate triggers.
	trig, err := EvaluateBundleTriggers(ctx, m0, opts.Thresholds, ref, opts.Now(), rset)
	if err != nil {
		return &BundleResult{
			TriggerReason: "skipped_trigger_eval_error",
			ErrorMessage:  err.Error(),
			DurationMS:    elapsed(),
		}, err
	}

	// 4. No trigger + no Force: short-circuit cleanly.
	if !opts.Force && !trig.Triggered {
		return &BundleResult{
			TriggerReason: "no_trigger",
			DurationMS:    elapsed(),
		}, nil
	}

	// 5. Determine triggerReason. opts.Force unconditionally wins —
	//    "force" is the audit-visible signal that a human / scheduler
	//    asked for an unconditional refresh, and conflating it with an
	//    organic trigger would muddy that meaning. The organic trigger
	//    (trig.Reason) is dropped intentionally on the force path; if a
	//    successor milestone wants both signals, add a separate
	//    OrganicReason field rather than packing them into TriggerReason.
	triggerReason := trig.Reason
	if opts.Force {
		triggerReason = "force"
	}

	// 6. Defensive check: Task 3.7 always supplies the Phase-1 mirror.
	//    Task 3.8 (--bundle-only) will materialize its own mirror and
	//    pass it in; an empty mirrorDir here is a wiring bug.
	if mirrorDir == "" {
		return &BundleResult{
			TriggerReason: triggerReason,
			ErrorMessage:  "bundle phase called without mirror",
			DurationMS:    elapsed(),
		}, errors.New("bundle phase called without mirror")
	}

	// 6. Generate the bundle artifact against the supplied mirror.
	art, err := GenerateBundleArtifact(ctx, mirrorDir, ref, s, rkeys, manifestVersion, opts.Now())
	if err != nil {
		return &BundleResult{
			TriggerReason: triggerReason,
			ErrorMessage:  err.Error(),
			DurationMS:    elapsed(),
		}, err
	}
	defer os.RemoveAll(art.LocalDir)

	// 7. CAS-merge the new BundleEntry. DryRun skips the commit; the
	//    artifact upload above is the only side-effect.
	if !opts.DryRun {
		if err := RunBundleCASMerge(ctx, r, art.Entry, opts.Actor, opts.CASRetry); err != nil {
			return &BundleResult{
				TriggerReason: triggerReason,
				ErrorMessage:  err.Error(),
				DurationMS:    elapsed(),
			}, err
		}
	}

	return &BundleResult{
		Generated:             !opts.DryRun,
		BundleID:              art.Entry.ID,
		BundleHash:            art.Entry.BundleHash,
		CoversManifestVersion: art.Entry.CoversManifestVersion,
		ByteSize:              art.Entry.ByteSize,
		TriggerReason:         triggerReason,
		DurationMS:            elapsed(),
	}, nil
}
