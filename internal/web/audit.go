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
		slog.Bool("audit", true),
		slog.String("event", "auth.session.created"),
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
		slog.Bool("audit", true),
		slog.String("event", "auth.session.destroyed"),
		slog.String("user_id", userID),
		slog.String("user", name),
	)
}

// EmitSessionRevoked records a user-initiated revocation of one of their own
// web sessions. Tagged audit=true so it ships to the activity stream and shows
// in the audit viewer, completing the session lifecycle alongside
// session.created/destroyed.
func EmitSessionRevoked(ctx context.Context, logger *slog.Logger, actor string, count int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.session.revoked",
		slog.Bool("audit", true),
		slog.String("event", "auth.session.revoked"),
		slog.String("actor", actor),
		slog.Int64("count", count),
	)
}

// EmitSessionRevokedAll records a user signing out all of their other web
// sessions. Tagged audit=true for activity-stream visibility.
func EmitSessionRevokedAll(ctx context.Context, logger *slog.Logger, actor string, count int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.session.revoked_all",
		slog.Bool("audit", true),
		slog.String("event", "auth.session.revoked_all"),
		slog.String("actor", actor),
		slog.Int64("count", count),
	)
}

// EmitAdminSessionRevoked records an admin revoking another user's web session
// by id hash. Tagged audit=true for activity-stream visibility.
func EmitAdminSessionRevoked(ctx context.Context, logger *slog.Logger, actor, idHash string, count int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.session.admin_revoked",
		slog.Bool("audit", true),
		slog.String("event", "auth.session.admin_revoked"),
		slog.String("actor", actor),
		slog.String("id_hash", idHash),
		slog.Int64("count", count),
	)
}

func EmitOIDCLogin(ctx context.Context, logger *slog.Logger, userID, name, issuer, subject string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.oidc.login",
		slog.Bool("audit", true),
		slog.String("event", "auth.oidc.login"),
		slog.String("user_id", userID), slog.String("user", name),
		slog.String("issuer", issuer), slog.String("subject", subject))
}

func EmitOIDCIdentityLinked(ctx context.Context, logger *slog.Logger, userID, name, issuer, subject, email string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.oidc.identity_linked",
		slog.Bool("audit", true),
		slog.String("event", "auth.oidc.identity_linked"),
		slog.String("user_id", userID), slog.String("user", name),
		slog.String("issuer", issuer), slog.String("subject", subject), slog.String("email", email))
}

func EmitOIDCRejected(ctx context.Context, logger *slog.Logger, issuer, reason, email string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "auth.oidc.rejected",
		slog.Bool("audit", true),
		slog.String("event", "auth.oidc.rejected"),
		slog.String("issuer", issuer), slog.String("reason", reason), slog.String("email", email))
}
