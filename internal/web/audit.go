package web

import (
	"context"
	"log/slog"
)

func EmitSessionCreated(ctx context.Context, logger *slog.Logger, userID, name, provider string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.session.created",
		slog.String("user_id", userID),
		slog.String("user", name),
		slog.String("provider", provider),
	)
}

func EmitSessionDestroyed(ctx context.Context, logger *slog.Logger, userID, name string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.session.destroyed",
		slog.String("user_id", userID),
		slog.String("user", name),
	)
}
