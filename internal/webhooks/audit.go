package webhooks

import (
	"context"
	"log/slog"
	"time"
)

// EmitDelivered logs the webhooks.delivered audit event after a 2xx response.
func EmitDelivered(ctx context.Context, logger *slog.Logger,
	deliveryID string, endpointID int64, eventType string, attempts int, durationMs int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "webhooks.delivered",
		slog.Bool("audit", true),
		slog.String("event", "webhooks.delivered"),
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
		slog.Bool("audit", true),
		slog.String("event", "webhooks.failed"),
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
		slog.Bool("audit", true),
		slog.String("event", "webhooks.dead_letter"),
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
		slog.Bool("audit", true),
		slog.String("event", "webhooks.enqueue_failed"),
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
		slog.Bool("audit", true),
		slog.String("event", "webhooks.endpoint_created"),
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
		slog.Bool("audit", true),
		slog.String("event", "webhooks.endpoint_removed"),
		slog.Int64("endpoint_id", endpointID),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
	)
}

// EmitEndpointSecretRotated logs the webhooks.endpoint_secret_rotated audit
// event when an operator rotates an endpoint's signing secret. The new
// secret value is NEVER logged.
func EmitEndpointSecretRotated(ctx context.Context, logger *slog.Logger,
	endpointID int64, tenant, repo, actor string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "webhooks.endpoint_secret_rotated",
		slog.Bool("audit", true),
		slog.String("event", "webhooks.endpoint_secret_rotated"),
		slog.Int64("endpoint_id", endpointID),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("actor", actor),
	)
}

// EmitWebhookPruned logs the webhooks.pruned audit event after a Prune
// sweep. dryRun=true indicates no rows were actually deleted.
func EmitWebhookPruned(ctx context.Context, logger *slog.Logger,
	deliveredRows, deadLetterRows int64,
	deliveredCutoff, deadLetterCutoff time.Time,
	dryRun bool, actor string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "webhooks.pruned",
		slog.Bool("audit", true),
		slog.String("event", "webhooks.pruned"),
		slog.Int64("delivered_rows", deliveredRows),
		slog.Int64("dead_letter_rows", deadLetterRows),
		slog.Int64("delivered_cutoff_unix", deliveredCutoff.Unix()),
		slog.Int64("dead_letter_cutoff_unix", deadLetterCutoff.Unix()),
		slog.Bool("dry_run", dryRun),
		slog.String("actor", actor),
	)
}

// EmitEgressDenied logs the webhooks.egress_denied audit event when the
// delivery worker refuses to dial an endpoint under the M25 egress policy.
// deniedBy is "host" (deny-host pattern matched, pattern populated) or "ip"
// (resolved address in a blocked range, ip populated).
func EmitEgressDenied(ctx context.Context, logger *slog.Logger,
	deliveryID string, endpointID int64, host, ip, deniedBy, pattern string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "webhooks.egress_denied",
		slog.Bool("audit", true),
		slog.String("event", "webhooks.egress_denied"),
		slog.String("delivery_id", deliveryID),
		slog.Int64("endpoint_id", endpointID),
		slog.String("host", host),
		slog.String("ip", ip),
		slog.String("denied_by", deniedBy),
		slog.String("pattern", pattern),
	)
}
