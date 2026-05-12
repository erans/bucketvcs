package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// emitFinalReport logs the completed maintenance event and emits related metrics.
func emitFinalReport(ctx context.Context, logger *slog.Logger, report Report) {
	emitCompleted(ctx, logger, report)
	emitMetric(ctx, logger, "maintenance_runs_total", 1, "outcome", report.Outcome)
	emitMetric(ctx, logger, "maintenance_run_duration_seconds", report.DurationMS/1000, "outcome", report.Outcome)
	emitMetric(ctx, logger, "maintenance_threshold_recent_pack_count", int64(report.TriggerEval.RecentPackCount))
	emitMetric(ctx, logger, "maintenance_threshold_total_pack_count", int64(report.TriggerEval.TotalPackCount))
	emitMetric(ctx, logger, "maintenance_threshold_manifest_pack_bytes", report.TriggerEval.ManifestPackBytes)
	if report.NewPackBytes > 0 {
		emitMetric(ctx, logger, "maintenance_pack_bytes_out", report.NewPackBytes)
		emitMetric(ctx, logger, "maintenance_objects_packed_total", int64(report.NewPackObjects))
	}
	if report.CASAttempts > 0 {
		emitMetric(ctx, logger, "maintenance_cas_attempts", int64(report.CASAttempts))
	}
	if report.BundleResult != nil {
		emitBundleResultMetrics(ctx, logger, report.RepoID, report.BundleResult)
	}
}

// runPipeline executes the full §4 maintenance pipeline against a single
// repo identified by (r, k). opts must already be normalized and validated
// before this is called (Run does that).
func runPipeline(ctx context.Context, s storage.ObjectStore, r *repo.Repo, k *keys.Repo, opts RunOptions) (Report, error) {
	startedAt := opts.Now()
	repoID := r.TenantID() + "/" + r.RepoID()
	report := Report{
		RepoID:           repoID,
		DryRun:           opts.DryRun,
		RepackedPackKeys: []string{}, // Always non-null for JSON consumers (operator-guide §6.1).
	}

	elapsed := func() int64 {
		return opts.Now().Sub(startedAt).Milliseconds()
	}

	// Emit maintenance.started immediately so every completed event is
	// paired with a started event, even when Phase 0 fails before
	// thresholds are evaluated. TriggerEval is zero here; that's
	// accurate (no eval ran yet) and consistent with the contract.
	emitStarted(ctx, opts.Logger, report, opts.DryRun)

	// ----------------------------------------------------------------
	// Phase 0 — Load & gate
	// ----------------------------------------------------------------
	view, err := r.ReadRoot(ctx)
	if err != nil {
		report.Outcome = "failed_other"
		report.DurationMS = elapsed()
		emitFinalReport(ctx, opts.Logger, report)
		return report, fmt.Errorf("maintenance: ReadRoot: %w", err)
	}

	var m0 manifest.Body
	if err := json.Unmarshal(view.Body, &m0); err != nil {
		report.Outcome = "failed_other"
		report.DurationMS = elapsed()
		emitFinalReport(ctx, opts.Logger, report)
		return report, fmt.Errorf("maintenance: unmarshal body: %w", err)
	}

	report.ManifestVersionAt = view.Header.ManifestVersion

	// Compute before-counts from M0.
	pb0, _ := json.Marshal(m0.Packs)
	report.BeforePackCount = len(m0.Packs)
	report.BeforeManifestPB = int64(len(pb0))

	// Threshold evaluation runs before any noop early-return so the
	// audit event is well-formed regardless of whether we proceed.
	// On an empty pack list this is O(1) (no Head calls).
	trigReport, err := Evaluate(ctx, s, m0, opts.Thresholds, opts.RecentWindow, opts.Now())
	if err != nil {
		report.Outcome = "failed_other"
		report.DurationMS = elapsed()
		emitFinalReport(ctx, opts.Logger, report)
		return report, fmt.Errorf("maintenance: threshold evaluation: %w", err)
	}

	// IO-bound reachability commit-count check runs only when cheap
	// thresholds did not already fire (bytes + pushes) and pack
	// thresholds did not fire. This avoids unnecessary GETs when a
	// cheaper trigger has already decided the outcome.
	//
	// Under BundleOnly we skip this entirely — the compact-reachability
	// flag only feeds the Phase 2-4 compact-only path, which BundleOnly
	// also skips. A storage failure inside EvaluateReachabilityCommits
	// would otherwise return "failed_other" with a non-nil error,
	// violating the *_bundle_only outcome contract.
	if !opts.Force && !trigReport.Triggered && !trigReport.CompactReachability && !opts.BundleOnly {
		hit, reason, herr := EvaluateReachabilityCommits(ctx, s, m0, opts.Thresholds)
		if herr != nil {
			report.Outcome = "failed_other"
			report.DurationMS = elapsed()
			emitFinalReport(ctx, opts.Logger, report)
			return report, fmt.Errorf("maintenance: reachability commit eval: %w", herr)
		}
		if hit {
			trigReport.CompactReachability = true
			trigReport.CompactReachabilityReason = reason
		}
	}
	report.TriggerEval = trigReport

	// ----------------------------------------------------------------
	// Phase 0b — Lazy backfill of legacy PackChecksum (M11 Phase 5.2)
	// ----------------------------------------------------------------
	// Pre-M11 manifests have PackEntry rows with empty PackChecksum
	// (the importer / receive-pack didn't record the pack trailer
	// SHA-1). §16.4 packfile-uri advertisement needs that hex, so the
	// first maintenance run to touch a legacy repo backfills it by
	// reading the trailing 20 bytes of each affected pack from object
	// storage and CAS-merging the populated rows back into the
	// manifest.
	//
	// Runs BEFORE the noop early-return below so even noop maintenance
	// runs (no trigger, not Force) still migrate legacy repos. Skipped
	// under DryRun: no mutations.
	if !opts.DryRun {
		updated, didBackfill, berr := BackfillPackChecksumsIfNeeded(ctx, s, r, m0, opts.CASRetry, opts.Actor, opts.Logger)
		if berr != nil {
			// Non-fatal: per-pack trailer-read failures are absorbed inside
			// the callback's WARN-and-skip loop, so an error here means CAS
			// retries were exhausted, a corrupted prev manifest, or context
			// cancellation. Continuing lets the rest of the pipeline make
			// whatever progress it still can; the next maintenance run will
			// retry the migration on the same legacy state.
			opts.Logger.WarnContext(ctx, "PackChecksum backfill failed (non-fatal)",
				slog.String("repo_id", repoID),
				slog.String("err", berr.Error()))
		} else if didBackfill {
			// The manifest version advanced; refresh the in-memory
			// view + report so downstream phases see the post-backfill
			// state. BeforePackCount / BeforeManifestPB intentionally
			// reflect the run-start snapshot — that's what "before
			// this maintenance run started" means.
			m0 = updated
			refView, rerr := r.ReadRoot(ctx)
			if rerr != nil {
				report.Outcome = "failed_other"
				report.DurationMS = elapsed()
				emitFinalReport(ctx, opts.Logger, report)
				return report, fmt.Errorf("maintenance: re-read after backfill: %w", rerr)
			}
			view = refView
			report.ManifestVersionAt = view.Header.ManifestVersion
			opts.Logger.WarnContext(ctx, "backfilled missing pack checksums (legacy pre-M11 manifest)",
				slog.String("repo_id", repoID),
				slog.Uint64("manifest_version_at", view.Header.ManifestVersion))
		}
	}

	// No-refs / no-packs guard: nothing to repack. Under BundleOnly we
	// also have nothing to bundle (no ref tip / no mirror to build), so
	// short-circuit here too — but tag the outcome distinctly so the
	// caller can tell BundleOnly's noop apart from a plain noop.
	if len(m0.Refs) == 0 || len(m0.Packs) == 0 {
		if opts.BundleOnly {
			report.Outcome = "noop_bundle_only"
		} else {
			report.Outcome = "noop"
		}
		report.AfterPackCount = report.BeforePackCount
		report.AfterManifestPB = report.BeforeManifestPB
		report.DurationMS = elapsed()
		emitFinalReport(ctx, opts.Logger, report)
		return report, nil
	}

	// Snapshot P0, T0, D0 from M0.
	t0 := make(map[string]string, len(m0.Refs))
	for ref, oid := range m0.Refs {
		t0[ref] = oid
	}
	d0 := m0.DefaultBranch
	p0 := make([]manifest.PackEntry, len(m0.Packs))
	copy(p0, m0.Packs)

	// Snapshot the delta hashes and count from M0 for the compaction path.
	// Capturing the hash SET (not just a count) makes the CAS trim race-safe:
	// if a second concurrent compaction commits between our snapshot and our
	// CAS write, the prev manifest's Deltas may no longer match our snapshot
	// order. By trimming on hash membership we only drop the exact deltas we
	// consumed, preserving any delta appended by a concurrent push.
	consumedDeltaCount := 0
	consumedHashes := make(map[string]struct{})
	if m0.Indexes.Reachability != nil {
		for _, ref := range m0.Indexes.Reachability.Deltas {
			consumedHashes[ref.Hash] = struct{}{}
		}
		consumedDeltaCount = len(m0.Indexes.Reachability.Deltas)
	}

	// Phase 1+ gate. BundleOnly forces us past this gate so the bundle
	// phase can run even when no repack/compact trigger fired; the
	// outer if !opts.BundleOnly wrapper around the repack/compact
	// branches below ensures we don't actually repack or compact.
	needsPhase1 := opts.Force || trigReport.Triggered || trigReport.CompactReachability || opts.BundleOnly
	if !needsPhase1 {
		report.Outcome = "noop"
		report.AfterPackCount = report.BeforePackCount
		report.AfterManifestPB = report.BeforeManifestPB
		report.DurationMS = elapsed()
		emitFinalReport(ctx, opts.Logger, report)
		return report, nil
	}

	// ----------------------------------------------------------------
	// Phase 1 — Materialize
	// ----------------------------------------------------------------
	tmp, err := os.MkdirTemp("", "bucketvcs-maint-")
	if err != nil {
		report.Outcome = "failed_other"
		report.DurationMS = elapsed()
		emitFinalReport(ctx, opts.Logger, report)
		return report, fmt.Errorf("maintenance: mkdirtemp: %w", err)
	}
	defer os.RemoveAll(tmp)

	p0Refs := make([]PackRef, len(p0))
	for i, p := range p0 {
		p0Refs[i] = PackRef{PackKey: p.PackKey, IdxKey: p.IdxKey}
	}

	if err := Materialize(ctx, s, MaterializeInput{
		BareDir:       tmp,
		Packs:         p0Refs,
		Refs:          t0,
		DefaultBranch: d0,
	}); err != nil {
		if errors.Is(err, ErrCorruptInput) {
			report.Outcome = "failed_walk"
		} else {
			report.Outcome = "failed_other"
		}
		report.DurationMS = elapsed()
		emitFinalReport(ctx, opts.Logger, report)
		return report, fmt.Errorf("maintenance: materialize: %w", err)
	}

	// DryRun: skip phases 2-6; report before counts and return success.
	// BundleOnly exemption: under BundleOnly we want to run the bundle
	// phase even in dry-run mode (it skips CAS-merge internally), so
	// fall through to the bundle phase below.
	if opts.DryRun && !opts.BundleOnly {
		report.Outcome = "success"
		report.AfterPackCount = report.BeforePackCount
		report.AfterManifestPB = report.BeforeManifestPB
		report.DurationMS = elapsed()
		emitFinalReport(ctx, opts.Logger, report)
		return report, nil
	}

	// Phases 2-4 (Repack + Indexes + Upload/CAS-merge) only run when
	// repack or compact-only triggers fired. Under BundleOnly we skip
	// them entirely — Phase 2's Repack and Phase 3's buildIndexes both
	// produce outputs that the Phase-4 upload would consume, and
	// nothing downstream of the bundle phase needs them. Running them
	// under BundleOnly wastes CPU and I/O on every invocation.
	if !opts.BundleOnly {
		// ----------------------------------------------------------------
		// Phase 2 — Repack
		// ----------------------------------------------------------------
		repackOut, err := Repack(ctx, tmp)
		if err != nil {
			report.Outcome = "failed_pack_write"
			report.DurationMS = elapsed()
			emitFinalReport(ctx, opts.Logger, report)
			return report, fmt.Errorf("maintenance: repack: %w", err)
		}

		// ----------------------------------------------------------------
		// Phase 3 — Indexes
		// ----------------------------------------------------------------
		idx, err := buildIndexesFromLocalPack(ctx, repackOut.PackPath, repackOut.IdxPath, repackOut.PackID, t0)
		if err != nil {
			report.Outcome = "failed_other"
			report.DurationMS = elapsed()
			emitFinalReport(ctx, opts.Logger, report)
			return report, fmt.Errorf("maintenance: build indexes: %w", err)
		}

		// ----------------------------------------------------------------
		// Phase 4 — Upload + CAS-merge (repack path vs compact-only path)
		// ----------------------------------------------------------------
		//
		// When trigReport.Triggered || opts.Force the full repack path runs:
		// upload pack+idx+indexes, then CAS-merge with a new consolidated pack.
		//
		// When only CompactReachability fired (no pack threshold, no Force),
		// the compact-only path runs: upload indexes only, then CAS-merge
		// that keeps Packs unchanged and trims consumed deltas.
		if trigReport.Triggered || opts.Force {
			// ---- Repack path (M9 behaviour, unchanged) ----
			uploaded, err := uploadArtifacts(ctx, s, k, uploadInput{
				PackID:           repackOut.PackID,
				PackPath:         repackOut.PackPath,
				IdxPath:          repackOut.IdxPath,
				ObjectMapHash:    idx.ObjectMapHash,
				ObjectMapBytes:   idx.ObjectMapBytes,
				CommitGraphHash:  idx.CommitGraphHash,
				CommitGraphBytes: idx.CommitGraphBytes,
			})
			if err != nil {
				report.Outcome = "failed_other"
				report.DurationMS = elapsed()
				emitFinalReport(ctx, opts.Logger, report)
				return report, fmt.Errorf("maintenance: upload: %w", err)
			}

			// Sorted P0 keys for deterministic output and CAS merge.
			p0Keys := make([]string, len(p0))
			for i, p := range p0 {
				p0Keys[i] = p.PackKey
			}
			sort.Strings(p0Keys)

			report.NewPackKey = uploaded.PackKey
			report.NewObjectMapKey = uploaded.ObjectMapKey
			report.NewCommitGraphKey = uploaded.CommitGraphKey
			report.NewPackBytes = repackOut.SizeBytes
			report.NewPackObjects = idx.ObjectCount
			report.RepackedPackKeys = p0Keys

			mergeIn := mergeInput{
				P0Keys: p0Keys,
				NewPack: manifest.PackEntry{
					PackID:       repackOut.PackID,
					PackKey:      uploaded.PackKey,
					IdxKey:       uploaded.IdxKey,
					SizeBytes:    repackOut.SizeBytes,
					ObjectCount:  idx.ObjectCount,
					PackChecksum: repackOut.PackChecksum,
				},
				NewObjectMap:       manifest.IndexRef{Key: uploaded.ObjectMapKey, Hash: idx.ObjectMapHash},
				NewCommitGraph:     manifest.IndexRef{Key: uploaded.CommitGraphKey, Hash: idx.CommitGraphHash},
				ConsumedHashes:     consumedHashes,
				ConsumedDeltaCount: consumedDeltaCount,
				// BaseManifest is set inside the CAS callback from view.Header.ManifestVersion
				// (the run-start snapshot), not from prev.Header.ManifestVersion
				// (the CAS pre-image), so it correctly records the version the
				// indexes were built from even when concurrent pushes advance the
				// manifest during the maintenance window.
			}

			extraBytes, err := json.Marshal(struct {
				M0Version          uint64    `json:"m0_version"`
				RefTipSnapshot     int       `json:"ref_tip_snapshot"`
				RepackedPackKeys   []string  `json:"repacked_pack_keys"`
				NewPackKey         string    `json:"new_pack_key"`
				NewPackSizeBytes   int64     `json:"new_pack_size_bytes"`
				NewPackObjectCount int       `json:"new_pack_object_count"`
				NewObjectMap       indexInfo `json:"new_object_map"`
				NewCommitGraph     indexInfo `json:"new_commit_graph"`
				RunStartedAt       time.Time `json:"run_started_at"`
			}{
				M0Version:          view.Header.ManifestVersion,
				RefTipSnapshot:     len(t0),
				RepackedPackKeys:   p0Keys,
				NewPackKey:         uploaded.PackKey,
				NewPackSizeBytes:   repackOut.SizeBytes,
				NewPackObjectCount: idx.ObjectCount,
				NewObjectMap:       indexInfo{Key: uploaded.ObjectMapKey, Hash: idx.ObjectMapHash},
				NewCommitGraph:     indexInfo{Key: uploaded.CommitGraphKey, Hash: idx.CommitGraphHash},
				RunStartedAt:       startedAt,
			})
			if err != nil {
				report.Outcome = "failed_other"
				report.DurationMS = elapsed()
				emitFinalReport(ctx, opts.Logger, report)
				return report, fmt.Errorf("maintenance: marshal extra: %w", err)
			}

			txBody := tx.Body{
				Type:  "maintenance",
				Actor: opts.Actor,
				Extra: extraBytes,
			}

			hookFired := false
			attempts := 0
			_, commitErr := r.Commit(ctx, txBody, func(prev *repo.RootView) ([]byte, error) {
				attempts++
				if !hookFired && opts.BetweenRepackAndCAS != nil {
					hookFired = true
					opts.BetweenRepackAndCAS()
				}
				var prevBody manifest.Body
				if perr := json.Unmarshal(prev.Body, &prevBody); perr != nil {
					return nil, perr
				}
				// Set BaseManifest to the version the indexes were built from
				// (the run-start snapshot), not the CAS pre-image version.
				// Using the snapshot version prevents under-reporting staleness
				// when concurrent pushes raise the version during the maintenance
				// window. The CAS pre-image version is still what we commit on
				// top of (that's implicit in the CAS mechanism itself).
				mergeIn.BaseManifest = fmt.Sprintf("v%08d", view.Header.ManifestVersion)
				merged := buildMergedBody(prevBody, mergeIn)
				return manifest.MarshalBody(merged)
			}, repo.WithCommitPolicy(repo.CommitPolicy{MaxRetries: opts.CASRetry}))
			report.CASAttempts = attempts

			if commitErr != nil {
				var gaveUp *repoerrs.CommitGaveUpError
				if errors.As(commitErr, &gaveUp) {
					report.Outcome = "failed_cas"
					report.DurationMS = elapsed()
					emitFinalReport(ctx, opts.Logger, report)
					return report, fmt.Errorf("%w: %w", ErrCASExhausted, commitErr)
				}
				report.Outcome = "failed_other"
				report.DurationMS = elapsed()
				emitFinalReport(ctx, opts.Logger, report)
				return report, fmt.Errorf("maintenance: commit: %w", commitErr)
			}
		} else {
			// ---- Compact-only path (M10) ----
			// Upload only the freshly-built .bvcg (commit-graph); packs and .bvom
			// are preserved. .bvom must NOT be swapped in here because the locally-
			// built pack (repackOut) is never uploaded to storage — swapping .bvom
			// would produce a dangling pack-id reference until the next full repack.
			uploadedIdx, err := uploadIndexesOnlyArtifacts(ctx, s, k, uploadIndexesInput{
				// ObjectMap is intentionally omitted — preserved from prev manifest.
				CommitGraphHash:  idx.CommitGraphHash,
				CommitGraphBytes: idx.CommitGraphBytes,
			})
			if err != nil {
				report.Outcome = "failed_other"
				report.DurationMS = elapsed()
				emitFinalReport(ctx, opts.Logger, report)
				return report, fmt.Errorf("maintenance: upload indexes: %w", err)
			}

			// NewObjectMapKey is intentionally left empty: compact-only does not
			// produce a new .bvom; the existing .bvom is preserved from prev.
			report.NewCommitGraphKey = uploadedIdx.CommitGraphKey
			report.ReachabilityCompaction = ReachabilityCompactionReport{
				Triggered:     true,
				TriggerReason: trigReport.CompactReachabilityReason,
				DeltasDropped: consumedDeltaCount,
				BaseSwapped:   false, // compact-only: .bvom preserved from prev; only .bvcg is refreshed
			}

			compactIn := compactOnlyInput{
				NewCommitGraph:     manifest.IndexRef{Key: uploadedIdx.CommitGraphKey, Hash: idx.CommitGraphHash},
				ConsumedHashes:     consumedHashes,
				ConsumedDeltaCount: consumedDeltaCount,
			}

			extraBytes, err := json.Marshal(struct {
				M0Version      uint64    `json:"m0_version"`
				TriggerReason  string    `json:"trigger_reason"`
				DeltasDropped  int       `json:"deltas_dropped"`
				NewCommitGraph indexInfo `json:"new_commit_graph"`
				RunStartedAt   time.Time `json:"run_started_at"`
			}{
				M0Version:      view.Header.ManifestVersion,
				TriggerReason:  trigReport.CompactReachabilityReason,
				DeltasDropped:  consumedDeltaCount,
				NewCommitGraph: indexInfo{Key: uploadedIdx.CommitGraphKey, Hash: idx.CommitGraphHash},
				RunStartedAt:   startedAt,
			})
			if err != nil {
				report.Outcome = "failed_other"
				report.DurationMS = elapsed()
				emitFinalReport(ctx, opts.Logger, report)
				return report, fmt.Errorf("maintenance: marshal compact extra: %w", err)
			}

			txBody := tx.Body{
				Type:  "maintenance_compact",
				Actor: opts.Actor,
				Extra: extraBytes,
			}

			attempts := 0
			hookFired := false
			_, commitErr := r.Commit(ctx, txBody, func(prev *repo.RootView) ([]byte, error) {
				attempts++
				if !hookFired && opts.BetweenRepackAndCAS != nil {
					hookFired = true
					opts.BetweenRepackAndCAS()
				}
				var prevBody manifest.Body
				if perr := json.Unmarshal(prev.Body, &prevBody); perr != nil {
					return nil, perr
				}
				// Detect concurrent pack additions. Compact-only does not produce a
				// new pack, so the .bvcg it built was derived from the snapshot pack
				// set (p0). If a concurrent push appended a new pack between our
				// snapshot read and this CAS attempt, committing the .bvcg would
				// leave the new pack's commits uncovered by the commit graph — an
				// incomplete .bvcg relative to the live Packs list.
				//
				// Abort by returning an error from the callback; the outer
				// r.Commit logic treats a non-nil callback error as a permanent
				// failure (not a CAS retry). The operator's cron scheduler will
				// retry on the next run with a fresh snapshot that includes the
				// new pack.
				// Pack mutations are append-only between snapshot and CAS in the
				// current pipeline: receive-pack only adds packs to manifest.Packs;
				// concurrent maintenance is excluded by the CAS itself. Length
				// divergence is necessary and sufficient to detect concurrent adds.
				// If a future change introduces pack replacement (e.g., M11 bundle
				// promotion), expand this to compare PackKey sets.
				if len(prevBody.Packs) != len(p0) {
					slog.WarnContext(ctx, "maintenance.compact_only.pack_divergence",
						"snapshot_packs", len(p0),
						"prev_packs", len(prevBody.Packs),
						"msg", "compact-only aborted: concurrent push added a new pack during maintenance window; next run will retry from a fresh snapshot")
					return nil, fmt.Errorf("compact-only: pack set changed during run (snapshot=%d, prev=%d); aborting to avoid incomplete commit graph", len(p0), len(prevBody.Packs))
				}
				// BaseManifest records the snapshot version the indexes were built
				// from (the run-start view), not the CAS pre-image version. This
				// correctly captures staleness when concurrent pushes raised the
				// version during the maintenance window.
				compactIn.BaseManifest = fmt.Sprintf("v%08d", view.Header.ManifestVersion)
				merged := buildCompactOnlyBody(prevBody, compactIn)
				return manifest.MarshalBody(merged)
			}, repo.WithCommitPolicy(repo.CommitPolicy{MaxRetries: opts.CASRetry}))
			report.CASAttempts = attempts

			if commitErr != nil {
				var gaveUp *repoerrs.CommitGaveUpError
				if errors.As(commitErr, &gaveUp) {
					report.Outcome = "failed_cas"
					report.DurationMS = elapsed()
					emitFinalReport(ctx, opts.Logger, report)
					return report, fmt.Errorf("%w: %w", ErrCASExhausted, commitErr)
				}
				report.Outcome = "failed_other"
				report.DurationMS = elapsed()
				emitFinalReport(ctx, opts.Logger, report)
				return report, fmt.Errorf("maintenance: compact commit: %w", commitErr)
			}
		}
	} // end if !opts.BundleOnly

	// ----------------------------------------------------------------
	// Phase BR — Bundle refresh (M11). Best-effort: failures do not
	// fail the maintenance run; they're logged and recorded on
	// report.BundleResult.ErrorMessage. Reuses the Phase-1 materialized
	// mirror at <tmp>/bare.git; the deferred RemoveAll for tmp fires
	// when this function returns (after Phase 5), and
	// GenerateBundleArtifact closes its bundle file before returning,
	// so no I/O races the cleanup.
	// ----------------------------------------------------------------
	if !opts.NoBundle {
		bareMirror := filepath.Join(tmp, "bare.git")
		bundleRes, bundleErr := runBundlePhase(ctx, s, r, k, opts, m0, view.Header.ManifestVersion, bareMirror)
		if bundleRes != nil {
			report.BundleResult = bundleRes
		}
		if bundleErr != nil {
			opts.Logger.WarnContext(ctx, "maintenance: bundle-refresh failed (non-fatal)",
				slog.String("repo_id", repoID),
				slog.String("err", bundleErr.Error()),
			)
		}
	}

	// Under BundleOnly the pipeline does not repack or compact, so
	// there is no maintenance commit and nothing for Phase 5 to read
	// back. Decide the outcome here from BundleResult and return.
	// Three terminal outcomes:
	//   - "success_bundle_only": bundle generated AND CAS-merged.
	//   - "failed_bundle_only":  bundle phase ran but recorded an
	//     ErrorMessage (artifact upload, CAS-merge, walk error, ...).
	//     Surfaces silent failures so automation can distinguish a
	//     real error from a genuine no-op.
	//   - "noop_bundle_only":    no trigger fired, DryRun, or skipped
	//     (no default branch / reachability load); no error recorded.
	//
	// Disambiguating noop subtypes:
	//   - DryRun:        report.DryRun == true; BundleResult.Generated == false.
	//   - No trigger:    BundleResult.TriggerReason == "no_trigger".
	//   - Skipped:       BundleResult.TriggerReason has a "skipped_*" prefix.
	// Callers monitoring Outcome alone get the coarse signal; the
	// BundleResult subfields disambiguate the cause.
	if opts.BundleOnly {
		switch {
		case report.BundleResult != nil && report.BundleResult.Generated:
			report.Outcome = "success_bundle_only"
		case report.BundleResult != nil && report.BundleResult.ErrorMessage != "":
			report.Outcome = "failed_bundle_only"
		default:
			report.Outcome = "noop_bundle_only"
		}
		report.AfterPackCount = report.BeforePackCount
		report.AfterManifestPB = report.BeforeManifestPB
		report.DurationMS = elapsed()
		emitFinalReport(ctx, opts.Logger, report)
		return report, nil
	}

	// ----------------------------------------------------------------
	// Phase 5 — Refresh report from post-commit manifest
	// ----------------------------------------------------------------
	postView, postErr := r.ReadRoot(ctx)
	if postErr == nil {
		report.ManifestVersionTo = postView.Header.ManifestVersion
		var postBody manifest.Body
		if uErr := json.Unmarshal(postView.Body, &postBody); uErr == nil {
			report.AfterPackCount = len(postBody.Packs)
			pb, _ := json.Marshal(postBody.Packs)
			report.AfterManifestPB = int64(len(pb))
		} else {
			opts.Logger.Warn("maintenance: post-commit body unmarshal failed; AfterPackCount is a lower-bound projection — the manifest itself is consistent",
				"err", uErr.Error(), "repo_id", report.RepoID)
			// For the repack path, a successful commit always produces exactly
			// one consolidated pack. For compact-only, packs are unchanged.
			if report.NewPackKey != "" {
				report.AfterPackCount = 1 // repack: lower-bound is the new consolidated pack
			} else {
				report.AfterPackCount = report.BeforePackCount // compact-only: packs unchanged
			}
		}
	} else {
		opts.Logger.Warn("maintenance: post-commit ReadRoot failed; AfterPackCount is a lower-bound projection — the manifest itself is consistent",
			"err", postErr.Error(), "repo_id", report.RepoID)
		// Same lower-bound logic as the unmarshal-failure path above.
		if report.NewPackKey != "" {
			report.AfterPackCount = 1 // repack: lower-bound is the new consolidated pack
		} else {
			report.AfterPackCount = report.BeforePackCount // compact-only: packs unchanged
		}
	}

	report.Outcome = "success"
	report.DurationMS = elapsed()
	emitFinalReport(ctx, opts.Logger, report)
	return report, nil
}

// indexInfo is a tiny helper for the tx Extra JSON object.
type indexInfo struct {
	Key  string `json:"key"`
	Hash string `json:"hash"`
}
