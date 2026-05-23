package webhooks

import (
	"context"
	"log/slog"
)

// EmitDelivered logs the webhooks.delivered audit event after a 2xx response.
func EmitDelivered(ctx context.Context, logger *slog.Logger,
	deliveryID string, endpointID int64, eventType string, attempts int, durationMs int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "webhooks.delivered",
		slog.String("delivery_id", deliveryID),
		slog.Int64("endpoint_id", endpointID),
		slog.String("event_type", eventType),
		slog.Int("attempts", attempts),
		slog.Int64("duration_ms", durationMs),
	)
}

// EmitFailed logs the webhooks.failed audit event for a non-2xx attempt that
// will be retried.
func EmitFailed(ctx context.Context, logger *slog.Logger,
	deliveryID string, endpointID int64, eventType string, attempts int, statusCode int,
	errMsg string, nextAttemptAtUnix int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "webhooks.failed",
		slog.String("delivery_id", deliveryID),
		slog.Int64("endpoint_id", endpointID),
		slog.String("event_type", eventType),
		slog.Int("attempts", attempts),
		slog.Int("status_code", statusCode),
		slog.String("error", errMsg),
		slog.Int64("next_attempt_at", nextAttemptAtUnix),
	)
}

// EmitDeadLetter logs the webhooks.dead_letter audit event when an attempt
// exhausts the retry budget.
func EmitDeadLetter(ctx context.Context, logger *slog.Logger,
	deliveryID string, endpointID int64, eventType string, totalAttempts int, finalStatusCode int) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelError, "webhooks.dead_letter",
		slog.String("delivery_id", deliveryID),
		slog.Int64("endpoint_id", endpointID),
		slog.String("event_type", eventType),
		slog.Int("total_attempts", totalAttempts),
		slog.Int("final_status_code", finalStatusCode),
	)
}

// EmitEnqueueFailed logs the webhooks.enqueue_failed audit event when the
// originating operation succeeded but the queue INSERT failed. Fail-open
// path — callers do not propagate the error.
func EmitEnqueueFailed(ctx context.Context, logger *slog.Logger,
	tenant, repo, eventType, errMsg string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelError, "webhooks.enqueue_failed",
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("event_type", eventType),
		slog.String("error", errMsg),
	)
}

// EmitEndpointCreated logs the webhooks.endpoint_created audit event.
func EmitEndpointCreated(ctx context.Context, logger *slog.Logger,
	endpointID int64, tenant, repo, url, events string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "webhooks.endpoint_created",
		slog.Int64("endpoint_id", endpointID),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("url", url),
		slog.String("events", events),
	)
}

// EmitEndpointRemoved logs the webhooks.endpoint_removed audit event.
func EmitEndpointRemoved(ctx context.Context, logger *slog.Logger,
	endpointID int64, tenant, repo string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "webhooks.endpoint_removed",
		slog.Int64("endpoint_id", endpointID),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
	)
}
