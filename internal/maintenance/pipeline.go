package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	report.TriggerEval = trigReport

	// No-refs / no-packs guard: nothing to repack.
	if len(m0.Refs) == 0 || len(m0.Packs) == 0 {
		report.Outcome = "noop"
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

	if !opts.Force && !trigReport.Triggered {
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
	if opts.DryRun {
		report.Outcome = "success"
		report.AfterPackCount = report.BeforePackCount
		report.AfterManifestPB = report.BeforeManifestPB
		report.DurationMS = elapsed()
		emitFinalReport(ctx, opts.Logger, report)
		return report, nil
	}

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
	// Phase 4 — Upload
	// ----------------------------------------------------------------
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

	// ----------------------------------------------------------------
	// Phase 5 / 6 — Tx record + CAS-merge
	// ----------------------------------------------------------------
	mergeIn := mergeInput{
		P0Keys: p0Keys,
		NewPack: manifest.PackEntry{
			PackID:      repackOut.PackID,
			PackKey:     uploaded.PackKey,
			IdxKey:      uploaded.IdxKey,
			SizeBytes:   repackOut.SizeBytes,
			ObjectCount: idx.ObjectCount,
		},
		NewObjectMap:   manifest.IndexRef{Key: uploaded.ObjectMapKey, Hash: idx.ObjectMapHash},
		NewCommitGraph: manifest.IndexRef{Key: uploaded.CommitGraphKey, Hash: idx.CommitGraphHash},
	}

	// Build Extra for the tx record. Keys must not collide with:
	// header: schema_version, tx_id, repo_id, base_manifest_version,
	//         base_manifest_object_version, started_at
	// body: type, actor, ref_updates, new_packs, validation
	extraBytes, err := json.Marshal(struct {
		M0Version            uint64    `json:"m0_version"`
		RefTipSnapshot       int       `json:"ref_tip_snapshot"`
		RepackedPackKeys     []string  `json:"repacked_pack_keys"`
		NewPackKey           string    `json:"new_pack_key"`
		NewPackSizeBytes     int64     `json:"new_pack_size_bytes"`
		NewPackObjectCount   int       `json:"new_pack_object_count"`
		NewObjectMap         indexInfo `json:"new_object_map"`
		NewCommitGraph       indexInfo `json:"new_commit_graph"`
		RunStartedAt         time.Time `json:"run_started_at"`
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

	// Track whether the BetweenRepackAndCAS hook has been fired yet.
	// We fire it during the first buildBody callback invocation so that
	// it can trigger a CAS version mismatch and test the retry mechanism.
	hookFired := false

	attempts := 0
	_, commitErr := r.Commit(ctx, txBody, func(prev *repo.RootView) ([]byte, error) {
		attempts++
		// Fire the test hook on the first invocation, just before we
		// construct the merged body. This allows concurrent version bumps
		// to be detected by the CAS operation and trigger a retry.
		if !hookFired && opts.BetweenRepackAndCAS != nil {
			hookFired = true
			opts.BetweenRepackAndCAS()
		}
		var prevBody manifest.Body
		if perr := json.Unmarshal(prev.Body, &prevBody); perr != nil {
			return nil, perr
		}
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
		// Non-CAS failure (storage error, callback error, etc.) — do
		// NOT misroute the operator with "cas retries exhausted".
		report.Outcome = "failed_other"
		report.DurationMS = elapsed()
		emitFinalReport(ctx, opts.Logger, report)
		return report, fmt.Errorf("maintenance: commit: %w", commitErr)
	}

	// ----------------------------------------------------------------
	// Phase 7 — Refresh report from post-commit manifest
	// ----------------------------------------------------------------
	//
	// On refresh failure we don't want to silently leave AfterPackCount=0
	// (which would falsely suggest the run lost every pack). Instead we
	// fall back to a best-effort projection: post-state = [new pack] +
	// late packs that came in during CAS retries — which is exactly the
	// body we just committed. Then log the refresh failure as a warning
	// so operators see it without demoting the outcome.
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
			report.AfterPackCount = 1 // lower-bound: at minimum we have our new pack.
		}
	} else {
		opts.Logger.Warn("maintenance: post-commit ReadRoot failed; AfterPackCount is a lower-bound projection — the manifest itself is consistent",
			"err", postErr.Error(), "repo_id", report.RepoID)
		// CAS succeeded — manifest is fine, just unreadable now (transient).
		// Use a best-effort projection: at minimum we have our new pack.
		report.AfterPackCount = 1
		// AfterManifestPB left at zero rather than guess; operators read
		// the warning to understand why.
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
