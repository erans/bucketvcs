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
		slog.String("metric_name", "webhooks_delivery_total"),
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
		slog.String("metric_name", "webhooks_queue_depth"),
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
		slog.String("metric_name", "webhooks_attempt_duration_ms"),
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
		slog.String("metric_name", "webhooks_endpoints_active"),
		slog.Int64("value", count),
	)
}

// EmitWebhookPrunedMetric counts pruned-rows per outcome. Outcomes:
// delivered, dead_letter. Emitted once per outcome per Prune call (callers
// emit both even when the count is zero).
func EmitWebhookPrunedMetric(ctx context.Context, logger *slog.Logger, outcome string, count int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "webhook_deliveries_pruned_total"),
		slog.String("outcome", outcome),
		slog.Int64("value", count),
	)
}

// EmitRepoRenamedMetric counts repo-rename outcomes. Emitted once per
// `bucketvcs repo rename` invocation. Outcomes:
//   - ok                  : rename succeeded
//   - collision_auth      : destination row already present in auth.db
//   - collision_storage   : storage already has keys under the new prefix
//   - not_found           : source repo not registered (or vanished mid-rename)
//   - cross_tenant        : rejected at CLI surface (slash in <new-name>)
func EmitRepoRenamedMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "repo_renamed_total"),
		slog.String("outcome", outcome),
		slog.Int("value", 1),
	)
}

// EmitEgressDeniedMetric logs one webhook_egress_denied_total sample. The
// metric deliberately carries NO host/url/tenant labels — endpoint URLs are
// attacker-influenced and would explode cardinality under probing (same
// reasoning as M19's proxied_url_token_invalid_total).
func EmitEgressDeniedMetric(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "webhook_egress_denied_total"),
		slog.Int("value", 1),
	)
}
