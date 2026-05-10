package gc

import "log/slog"

// LogMarkCompleted emits an audit-tagged structured log line for the
// completion of a mark phase. The audit=true field is the M8 contract
// for §31 audit emission; M15 will route audit-tagged events to the
// durable audit store without changing call sites.
func LogMarkCompleted(logger *slog.Logger, repoID, markID string, manifestVersion uint64, txCount, packCount, idxCount int) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("gc.mark.completed",
		"audit", true,
		"subsystem", "gc",
		"repo_id", repoID,
		"mark_id", markID,
		"manifest_version", manifestVersion,
		"candidate_tx_records", txCount,
		"candidate_canonical_packs", packCount,
		"candidate_indexes", idxCount,
	)
}

// LogSweepCompleted emits an audit-tagged structured log line for the
// completion of a sweep phase.
func LogSweepCompleted(logger *slog.Logger, repoID, sweepID, markID string, deletedTx, deletedPacks, deletedIdx, skippedRevived, skippedRetention, skippedVersion, skippedNotFound, skippedDisarmed, errorsCount int) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("gc.sweep.completed",
		"audit", true,
		"subsystem", "gc",
		"repo_id", repoID,
		"sweep_id", sweepID,
		"mark_id", markID,
		"deleted_tx_records", deletedTx,
		"deleted_canonical_packs", deletedPacks,
		"deleted_indexes", deletedIdx,
		"skipped_revived", skippedRevived,
		"skipped_retention_not_met", skippedRetention,
		"skipped_version_mismatch", skippedVersion,
		"skipped_not_found", skippedNotFound,
		"skipped_tx_sweep_disarmed", skippedDisarmed,
		"errors_count", errorsCount,
	)
}

// LogDisarmed emits a non-audit log line noting that tx orphan sweep
// was skipped for a repo because no commit markers were observed.
func LogDisarmed(logger *slog.Logger, repoID string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("gc.disarmed",
		"subsystem", "gc",
		"repo_id", repoID,
		"reason", "no commit markers observed in tx/ listing",
	)
}
