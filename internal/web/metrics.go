package web

import (
	"context"
	"log/slog"
)

// EmitLoginMetric records a login outcome. result ∈ "success"|"invalid"|"ratelimited";
// provider ∈ "password"|"oidc".
func EmitLoginMetric(ctx context.Context, logger *slog.Logger, result, provider string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "web_login_total"),
		slog.String("result", result),
		slog.String("provider", provider),
		slog.Int("value", 1),
	)
}

// EmitBrowseMetric records a served browse view. view ∈
// {repo,tree,blob,raw,commits,commit}.
func EmitBrowseMetric(ctx context.Context, logger *slog.Logger, view string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "web_browse_total"),
		slog.String("view", view),
		slog.Int("value", 1),
	)
}

// EmitRequestMetric records a served request by route + status.
func EmitRequestMetric(ctx context.Context, logger *slog.Logger, route string, status int) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "web_requests_total"),
		slog.String("route", route),
		slog.Int("status", status),
		slog.Int("value", 1),
	)
}

// EmitAdminActionMetric records a settings/admin form outcome.
// result ∈ "ok"|"invalid"|"denied"|"error".
func EmitAdminActionMetric(ctx context.Context, logger *slog.Logger, domain, action, result string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "web_admin_actions_total"),
		slog.String("domain", domain),
		slog.String("action", action),
		slog.String("result", result),
		slog.Int("value", 1),
	)
}
