package maintenance

import (
	"context"
	"log/slog"
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
