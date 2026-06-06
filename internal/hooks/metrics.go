package hooks

import (
	"context"
	"log/slog"
)

// EmitPreReceiveMetric counts pre-receive outcomes: accepted|rejected|error.
func EmitPreReceiveMetric(ctx context.Context, logger *slog.Logger, tenant, repo, outcome string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "hooks_pre_receive_total"),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("outcome", outcome),
		slog.Int("value", 1),
	)
}

func EmitPreReceiveDuration(ctx context.Context, logger *slog.Logger, tenant, repo string, durNanos int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "hooks_pre_receive_duration_seconds"),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.Float64("value", float64(durNanos)/1e9),
	)
}

// EmitPostReceiveMetric counts post-receive outcomes: ok|nonzero|error|dropped.
func EmitPostReceiveMetric(ctx context.Context, logger *slog.Logger, tenant, repo, outcome string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "hooks_post_receive_total"),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("outcome", outcome),
		slog.Int("value", 1),
	)
}

func EmitPostReceiveDuration(ctx context.Context, logger *slog.Logger, tenant, repo string, durNanos int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "hooks_post_receive_duration_seconds"),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.Float64("value", float64(durNanos)/1e9),
	)
}
