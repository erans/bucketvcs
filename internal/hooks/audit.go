package hooks

import (
	"context"
	"log/slog"
)

// EmitHookRejected fires policy.hook.rejected at WARN.
func EmitHookRejected(ctx context.Context, logger *slog.Logger,
	tenant, repo, trigger, scriptName string, exitCode int,
	pushID, actor string, stderr []byte) {
	if logger == nil {
		logger = slog.Default()
	}
	stderrStr := string(stderr)
	if len(stderrStr) > 1024 {
		stderrStr = stderrStr[:1024] + "...[truncated for audit]"
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "policy.hook.rejected",
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("trigger", trigger),
		slog.String("script_name", scriptName),
		slog.Int("exit_code", exitCode),
		slog.String("push_id", pushID),
		slog.String("actor", actor),
		slog.String("stderr_truncated", stderrStr),
	)
}

// EmitHookInternalError fires policy.hook.internal_error at ERROR.
func EmitHookInternalError(ctx context.Context, logger *slog.Logger,
	tenant, repo, trigger, scriptName, pushID string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelError, "policy.hook.internal_error",
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("trigger", trigger),
		slog.String("script_name", scriptName),
		slog.String("push_id", pushID),
		slog.String("error", err.Error()),
	)
}

// EmitHookLifecycle records hook lifecycle events (added/removed/enabled/disabled).
// Fired from CLI handlers; INFO level.
func EmitHookLifecycle(ctx context.Context, logger *slog.Logger,
	event, tenant, repo, trigger, scriptName, actor string, sortOrder int) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, event,
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("trigger", trigger),
		slog.String("script_name", scriptName),
		slog.String("actor", actor),
		slog.Int("sort_order", sortOrder),
	)
}
