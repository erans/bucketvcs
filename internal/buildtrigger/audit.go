package buildtrigger

import (
	"context"
	"log/slog"
)

// EmitFiredAudit logs the build.trigger.fired audit event when a delivery
// attempt begins. refCount is the number of refs carried by the originating
// push that matched this trigger (currently one per delivery row).
func EmitFiredAudit(ctx context.Context, logger *slog.Logger,
	deliveryID, triggerID, kind string, refCount int) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "build.trigger.fired",
		slog.Bool("audit", true),
		slog.String("event", "build.trigger.fired"),
		slog.String("delivery_id", deliveryID),
		slog.String("trigger_id", triggerID),
		slog.String("kind", kind),
		slog.Int("ref_count", refCount),
	)
}

// EmitDelivered logs the build.trigger.delivered audit event after a successful
// attempt.
func EmitDelivered(ctx context.Context, logger *slog.Logger,
	deliveryID, triggerID string, attempts int, durationMs int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "build.trigger.delivered",
		slog.Bool("audit", true),
		slog.String("event", "build.trigger.delivered"),
		slog.String("delivery_id", deliveryID),
		slog.String("trigger_id", triggerID),
		slog.Int("attempts", attempts),
		slog.Int64("duration_ms", durationMs),
	)
}

// EmitFailed logs the build.trigger.failed audit event for an attempt that
// will be retried.
func EmitFailed(ctx context.Context, logger *slog.Logger,
	deliveryID, triggerID string, attempts, statusCode int, errMsg string, nextAttemptAtUnix int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "build.trigger.failed",
		slog.Bool("audit", true),
		slog.String("event", "build.trigger.failed"),
		slog.String("delivery_id", deliveryID),
		slog.String("trigger_id", triggerID),
		slog.Int("attempts", attempts),
		slog.Int("status_code", statusCode),
		slog.String("error", errMsg),
		slog.Int64("next_attempt_at", nextAttemptAtUnix),
	)
}

// EmitDeadLetter logs the build.trigger.deadletter audit event when an attempt
// exhausts the retry budget.
func EmitDeadLetter(ctx context.Context, logger *slog.Logger,
	deliveryID, triggerID string, totalAttempts, finalStatusCode int) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelError, "build.trigger.deadletter",
		slog.Bool("audit", true),
		slog.String("event", "build.trigger.deadletter"),
		slog.String("delivery_id", deliveryID),
		slog.String("trigger_id", triggerID),
		slog.Int("total_attempts", totalAttempts),
		slog.Int("final_status_code", finalStatusCode),
	)
}

// EmitTokenMintedAudit logs the build.token.minted audit event. The token
// value is NEVER logged — only the binding (tenant, repo, label, ttl).
func EmitTokenMintedAudit(ctx context.Context, logger *slog.Logger,
	tenant, repo, tokenLabel string, ttlSeconds int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "build.token.minted",
		slog.Bool("audit", true),
		slog.String("event", "build.token.minted"),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("token_label", tokenLabel),
		slog.Int64("ttl_seconds", ttlSeconds),
	)
}

// EmitEnqueueFailed logs the build.trigger.enqueue_failed audit event when the
// originating push succeeded but the queue INSERT failed. Fail-open path —
// callers do not propagate the error.
func EmitEnqueueFailed(ctx context.Context, logger *slog.Logger,
	tenant, repo, errMsg string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelError, "build.trigger.enqueue_failed",
		slog.Bool("audit", true),
		slog.String("event", "build.trigger.enqueue_failed"),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("error", errMsg),
	)
}

// EmitTriggerLifecycle logs a build-trigger lifecycle audit event. event is the
// fully-qualified event name (build.trigger.added/removed/enabled/disabled),
// emitted by the CLI on operator management actions.
func EmitTriggerLifecycle(ctx context.Context, logger *slog.Logger,
	event, triggerID, tenant, repo string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, event,
		slog.Bool("audit", true),
		slog.String("event", event),
		slog.String("trigger_id", triggerID),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
	)
}
