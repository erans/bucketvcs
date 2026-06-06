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
		slog.Bool("audit", true),
		slog.String("event", "auth.token.rotated"),
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
		slog.Bool("audit", true),
		slog.String("event", "auth.scope.denied"),
		slog.String("user_id", userID),
		slog.String("token_id_prefix", tokenIDPrefix),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("operation", operation),
		slog.String("required_scope", FormatScopes(required)),
		slog.String("granted_scopes", FormatScopes(granted)),
	)
}

// EmitRateLimitHit logs the auth.ratelimit.hit audit event when the rate
// limiter rejects an auth attempt. bucket is "ip" or "user". user is
// empty for SSH pre-resolution and anonymous HTTPS. transport is
// "https" or "ssh".
func EmitRateLimitHit(ctx context.Context, logger *slog.Logger,
	ip, user, bucket string, retryAfterSec int, transport string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "auth.ratelimit.hit",
		slog.Bool("audit", true),
		slog.String("event", "auth.ratelimit.hit"),
		slog.String("ip", ip),
		slog.String("user", user),
		slog.String("bucket", bucket),
		slog.Int("retry_after_sec", retryAfterSec),
		slog.String("transport", transport),
	)
}

// EmitOIDCExchanged records a successful token exchange (M22).
func EmitOIDCExchanged(ctx context.Context, logger *slog.Logger,
	issuerAlias, sub, tenant, repo string, scopes TokenScope, ttlSec int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.oidc.exchanged",
		slog.Bool("audit", true),
		slog.String("event", "auth.oidc.exchanged"),
		slog.String("issuer", issuerAlias),
		slog.String("sub", sub),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("scopes", FormatScopes(scopes)),
		slog.Int64("ttl_sec", ttlSec),
	)
}

// EmitOIDCRejected records a rejected exchange (M22). reason is an enum:
// unknown_issuer | invalid_token | no_rule | issuer_unavailable.
func EmitOIDCRejected(ctx context.Context, logger *slog.Logger,
	issuerAlias, ip, reason string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "auth.oidc.rejected",
		slog.Bool("audit", true),
		slog.String("event", "auth.oidc.rejected"),
		slog.String("issuer", issuerAlias),
		slog.String("ip", ip),
		slog.String("reason", reason),
	)
}

// EmitPasswordSet records that a user's local password was set/changed.
func EmitPasswordSet(ctx context.Context, logger *slog.Logger, user string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "auth.password.set",
		slog.Bool("audit", true),
		slog.String("event", "auth.password.set"),
		slog.String("user", user),
	)
}
