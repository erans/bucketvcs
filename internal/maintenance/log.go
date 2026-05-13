package maintenance

import (
	"context"
	"log/slog"
	"strings"
)

// emitStarted logs a maintenance.started audit event after Phase 0 evaluation.
// The audit=true field is the M9 contract for §32 metric emission.
func emitStarted(ctx context.Context, logger *slog.Logger, r Report, dryRun bool) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "maintenance.started",
		slog.Bool("audit", true),
		slog.String("event", "maintenance.started"),
		slog.String("repo_id", r.RepoID),
		slog.Uint64("manifest_version_at_start", r.ManifestVersionAt),
		slog.Bool("dry_run", dryRun),
		slog.Any("threshold_eval", r.TriggerEval),
	)
}

// emitCompleted logs a maintenance.completed audit event with pack counts and outcome.
func emitCompleted(ctx context.Context, logger *slog.Logger, r Report) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "maintenance.completed",
		slog.Bool("audit", true),
		slog.String("event", "maintenance.completed"),
		slog.String("repo_id", r.RepoID),
		slog.String("outcome", r.Outcome),
		slog.Int("before_pack_count", r.BeforePackCount),
		slog.Int("after_pack_count", r.AfterPackCount),
		slog.Int64("before_manifest_pack_bytes", r.BeforeManifestPB),
		slog.Int64("after_manifest_pack_bytes", r.AfterManifestPB),
		slog.String("new_pack_key", r.NewPackKey),
		slog.Int("new_pack_objects", r.NewPackObjects),
		slog.String("new_object_map_key", r.NewObjectMapKey),
		slog.String("new_commit_graph_key", r.NewCommitGraphKey),
		slog.Any("repacked_pack_keys", r.RepackedPackKeys),
		slog.Int("cas_attempts", r.CASAttempts),
		slog.Int64("duration_ms", r.DurationMS),
		slog.Bool("dry_run", r.DryRun),
	)
}

// emitBundleResultMetrics emits up to three structured metrics from the
// bundle-refresh phase whenever a BundleResult is available:
//
//   - bundle_generated_total (value 1): always emitted. The "outcome" label
//     is one of four values:
//   - "success": Generated=true (the bundle was uploaded and CAS-merged).
//   - "noop": Generated=false AND either no error OR a "skipped_*"
//     TriggerReason (phase ran but generation wasn't attempted or wasn't
//     needed). The recoverable cause is preserved in "trigger_reason".
//   - "failure": Generated=false AND non-"skipped_*" TriggerReason with a
//     non-empty ErrorMessage (generation/upload/CAS-merge failed).
//     Labels also include "repo_id" and "trigger_reason" for per-repo
//     and per-cause breakdown in operator dashboards.
//
//   - bundle_generation_duration_seconds: always emitted. Value is
//     DurationMS/1000 (integer division; sub-second runs report 0),
//     matching the maintenance_run_duration_seconds convention. Labelled
//     with "repo_id" for per-repo latency histograms.
//
//   - bundle_bytes: emitted ONLY when Generated is true AND ByteSize > 0.
//     Captures the compressed on-disk size of the generated bundle artifact.
//     Omitted on failure and noop to avoid misleading zero-byte data points
//     that would skew size-distribution queries.
//
// This function is the single authoritative source for bundle metrics in the
// maintenance pipeline. Gateway-side metrics (e.g. per-download counters) are
// deferred to a future phase and intentionally absent here.
func emitBundleResultMetrics(ctx context.Context, logger *slog.Logger, repoID string, br *BundleResult) {
	if br == nil {
		return
	}

	outcome := "noop"
	switch {
	case br.Generated:
		outcome = "success"
	case strings.HasPrefix(br.TriggerReason, "skipped_"):
		outcome = "noop"
	case br.ErrorMessage != "":
		outcome = "failure"
	}

	emitMetric(ctx, logger, "bundle_generated_total", 1,
		"outcome", outcome,
		"repo_id", repoID,
		"trigger_reason", br.TriggerReason,
	)

	emitMetric(ctx, logger, "bundle_generation_duration_seconds", br.DurationMS/1000,
		"repo_id", repoID,
	)

	if br.Generated && br.ByteSize > 0 {
		emitMetric(ctx, logger, "bundle_bytes", br.ByteSize,
			"repo_id", repoID,
		)
	}
}

// emitMetric logs a structured metric with a name, value, and optional
// label pairs. Metrics use metric_name and value fields; labels are
// passed as alternating key-value pairs in kvs. Pairs whose key isn't
// a string are skipped (with a debug log) rather than emitted with an
// empty key, which would produce malformed metric lines.
func emitMetric(ctx context.Context, logger *slog.Logger, name string, value int64, kvs ...any) {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []slog.Attr{
		slog.String("metric_name", name),
		slog.Int64("value", value),
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		k, ok := kvs[i].(string)
		if !ok {
			logger.LogAttrs(ctx, slog.LevelDebug, "emitMetric: skipping non-string label key",
				slog.String("metric_name", name),
				slog.Any("bad_key", kvs[i]))
			continue
		}
		attrs = append(attrs, slog.Any(k, kvs[i+1]))
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric", attrs...)
}

// emitBundleGenerated logs a bundle.generated audit event after a successful
// CAS-merge in the bundle-refresh phase. One event is emitted per generated
// bundle; operators can join on bundle_id to correlate with bundle.retired and
// downstream gateway advertisement events.
func emitBundleGenerated(ctx context.Context, logger *slog.Logger, repoID string, art BundleArtifact, durationMS int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "bundle.generated",
		slog.Bool("audit", true),
		slog.String("event", "bundle.generated"),
		slog.String("repo_id", repoID),
		slog.String("bundle_id", art.Entry.ID),
		slog.String("bundle_hash", art.Entry.BundleHash),
		slog.String("tip_oid", art.Entry.TipOID),
		slog.Uint64("covers_manifest_version", art.Entry.CoversManifestVersion),
		slog.Int64("byte_size", art.Entry.ByteSize),
		slog.Int64("duration_ms", durationMS),
	)
}

// emitBundleRetired logs a bundle.retired audit event when a CAS-merge
// replaces a prior full_default bundle entry. Emitted once per retired
// predecessor, before the paired bundle.generated event so log consumers
// can detect the replacement pair atomically. The replaced_by field carries
// the new bundle_id for easy log correlation.
func emitBundleRetired(ctx context.Context, logger *slog.Logger, repoID, bundleID, reason, replacedBy string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "bundle.retired",
		slog.Bool("audit", true),
		slog.String("event", "bundle.retired"),
		slog.String("repo_id", repoID),
		slog.String("bundle_id", bundleID),
		slog.String("reason", reason),
		slog.String("replaced_by", replacedBy),
	)
}
