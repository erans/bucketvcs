package auth

import (
	"context"
	"log/slog"
)

// EmitTokenRotated logs the auth.token.rotated audit event after a successful
// CLI rotation. The new secret is NEVER logged.
func EmitTokenRotated(ctx context.Context, logger *slog.Logger,
	tokenID, userID, actor string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.token.rotated",
		slog.String("token_id", tokenID),
		slog.String("user_id", userID),
		slog.String("actor", actor),
	)
}

// EmitScopeDenied logs the auth.scope.denied audit event when CheckScope
// rejects an operation. token_id_prefix is the first 8 chars of the token
// id (never the full id, never the secret) — empty string when the actor
// is anonymous or the token id is not available at the call site. (Actor
// does not currently carry the token id; operators correlate via user_id
// and timestamp until a follow-up extends Actor with TokenID.)
func EmitScopeDenied(ctx context.Context, logger *slog.Logger,
	userID, tokenIDPrefix, tenant, repo, operation string,
	required, granted TokenScope) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "auth.scope.denied",
		slog.String("user_id", userID),
		slog.String("token_id_prefix", tokenIDPrefix),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("operation", operation),
		slog.String("required_scope", FormatScopes(required)),
		slog.String("granted_scopes", FormatScopes(granted)),
	)
}
