package webhooks

import (
	"context"
	"log/slog"
)

// EmitDeliveryMetric logs one webhooks_delivery_total{outcome} sample.
// Outcomes: delivered, failed_retry, dead_letter, enqueue_failed.
func EmitDeliveryMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "webhooks_delivery_total"),
		slog.String("outcome", outcome),
		slog.Int("value", 1),
	)
}

// EmitQueueDepthGauge logs one webhooks_queue_depth{status} gauge sample.
func EmitQueueDepthGauge(ctx context.Context, logger *slog.Logger, status string, depth int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "webhooks_queue_depth"),
		slog.String("status", status),
		slog.Int64("value", depth),
	)
}

// EmitAttemptDuration logs one webhooks_attempt_duration_ms{outcome} sample.
func EmitAttemptDuration(ctx context.Context, logger *slog.Logger, outcome string, durationMs int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "webhooks_attempt_duration_ms"),
		slog.String("outcome", outcome),
		slog.Int64("value", durationMs),
	)
}

// EmitEndpointsActiveGauge logs one webhooks_endpoints_active gauge sample.
func EmitEndpointsActiveGauge(ctx context.Context, logger *slog.Logger, count int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "webhooks_endpoints_active"),
		slog.Int64("value", count),
	)
}
