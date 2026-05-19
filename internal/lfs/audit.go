package lfs

import (
	"context"
	"log/slog"
)

// emitLFSBatch records a "lfs.batch" audit event matching the
// flat-attribute slog shape used by M11 bundle/pack URI audit events.
//
// repo is "<tenant>/<repo>" form. user is the actor username or ""
// for anonymous. op is "upload" or "download". nObjects is the count
// in the response. result mirrors emitBatchRequestMetric's result
// label.
func emitLFSBatch(ctx context.Context, logger *slog.Logger, repo, user, op string, nObjects int, result string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "lfs.batch",
		slog.String("event", "lfs.batch"),
		slog.String("repo", repo),
		slog.String("user", user),
		slog.String("op", op),
		slog.Int("n_objects", nObjects),
		slog.String("result", result),
	)
}
