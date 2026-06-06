package ratelimit

import (
	"context"
	"log/slog"
)

// EmitRateLimitMetric logs one auth_ratelimit_total{outcome} sample.
// Outcomes: limited_ip, failure_counted, success_reset. The successful-pass
// case is intentionally not emitted — at request-rate granularity it doubles
// gateway log volume for a counter that is rarely inspected per-event.
func EmitRateLimitMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "auth_ratelimit_total"),
		slog.String("outcome", outcome),
		slog.Int("value", 1),
	)
}
