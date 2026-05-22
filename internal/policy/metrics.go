package policy

import (
	"context"
	"log/slog"
)

// emitMetric logs a structured metric in the same shape used by
// internal/lfs/metrics.go: an info-level "metric" record with attrs
// metric_name (string), value (int64), plus key/value pairs from kvs.
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
			continue
		}
		attrs = append(attrs, slog.Any(k, kvs[i+1]))
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric", attrs...)
}

// EmitRefCheckMetric increments policy_refs_check_total{outcome}.
// Emitted once per Step 8b ref-update check.
// outcome ∈ {"ok", "blocked_deletion", "blocked_force_push", "internal_error"}.
//
// Exported for cross-package use (receivepack calls it).
func EmitRefCheckMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	emitMetric(ctx, logger, "policy_refs_check_total", 1, "outcome", outcome)
}
