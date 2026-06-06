package policy

import (
	"context"
	"log/slog"
)

// EmitRefRejected records a "policy.ref.rejected" audit event when
// step 8b blocks a ref update. Fires only on rejection — accepted
// pushes already emit the existing receive-pack audit.
//
// Exported for cross-package use (receivepack calls it).
func EmitRefRejected(ctx context.Context, logger *slog.Logger, tenant, repo string, perr *PolicyError, actor string) {
	if logger == nil {
		logger = slog.Default()
	}
	if perr == nil {
		return
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "policy.ref.rejected",
		slog.Bool("audit", true),
		slog.String("event", "policy.ref.rejected"),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("refname", perr.Refname),
		slog.String("matched_pattern", perr.MatchedPattern),
		slog.String("reason", perr.Reason),
		slog.String("actor", actor),
		slog.String("old_oid", perr.OldOID),
		slog.String("new_oid", perr.NewOID),
	)
}

// EmitRefInternalError logs a policy.ref.internal_error audit event when
// CheckUpdate fails for reasons that aren't a policy decision (sqlite read
// failure, merge-base subprocess failure). The event mirrors the schema of
// policy.ref.rejected but with reason="internal-error" semantics, giving
// operators a single audit trail for all step 8b rejections.
func EmitRefInternalError(ctx context.Context, logger *slog.Logger, tenant, repo, refname, actor string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelError, "policy.ref.internal_error",
		slog.Bool("audit", true),
		slog.String("event", "policy.ref.internal_error"),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.String("refname", refname),
		slog.String("actor", actor),
		slog.String("error", err.Error()),
	)
}
