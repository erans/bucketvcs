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

// emitLFSObjectServed records a "lfs.object.served" audit event after
// a /_lfs/ PUT or GET completes. Matches the M11 proxied.url.served
// audit shape.
//
// hash is the token's hash field (<tenant>/<repo>/<oid>); bytes is
// the response body byte count (PUT: input bytes; GET: output bytes);
// status is the HTTP status returned.
func emitLFSObjectServed(ctx context.Context, logger *slog.Logger, op, hash string, bytes int64, status int) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "lfs.object.served",
		slog.String("event", "lfs.object.served"),
		slog.String("op", op),
		slog.String("hash", hash),
		slog.Int64("bytes", bytes),
		slog.Int("status", status),
	)
}
