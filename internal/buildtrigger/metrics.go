package buildtrigger

import (
	"context"
	"log/slog"
)

// EmitFired logs one build_trigger_fired_total{kind,result} sample.
// result is one of: delivered, failed_retry, dead_letter.
func EmitFired(ctx context.Context, logger *slog.Logger, kind, result string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "build_trigger_fired_total"),
		slog.String("kind", kind),
		slog.String("result", result),
		slog.Int("value", 1),
	)
}

// EmitAttemptDuration logs one build_trigger_delivery_duration_ms{result} sample.
func EmitAttemptDuration(ctx context.Context, logger *slog.Logger, result string, durationMs int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "build_trigger_delivery_duration_ms"),
		slog.String("result", result),
		slog.Int64("value", durationMs),
	)
}

// EmitDeadLetterMetric logs one build_trigger_deadletter_total{reason} sample.
// reason is one of: permanent, exhausted.
func EmitDeadLetterMetric(ctx context.Context, logger *slog.Logger, reason string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "build_trigger_deadletter_total"),
		slog.String("reason", reason),
		slog.Int("value", 1),
	)
}

// EmitTokenMinted logs one build_token_minted_total sample.
func EmitTokenMinted(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "build_token_minted_total"),
		slog.Int("value", 1),
	)
}
