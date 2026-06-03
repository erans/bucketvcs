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

func EmitOIDCLogin(ctx context.Context, logger *slog.Logger, userID, name, issuer, subject string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.oidc.login",
		slog.String("user_id", userID), slog.String("user", name),
		slog.String("issuer", issuer), slog.String("subject", subject))
}

func EmitOIDCIdentityLinked(ctx context.Context, logger *slog.Logger, userID, name, issuer, subject, email string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.oidc.identity_linked",
		slog.String("user_id", userID), slog.String("user", name),
		slog.String("issuer", issuer), slog.String("subject", subject), slog.String("email", email))
}

func EmitOIDCRejected(ctx context.Context, logger *slog.Logger, issuer, reason, email string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "auth.oidc.rejected",
		slog.String("issuer", issuer), slog.String("reason", reason), slog.String("email", email))
}
