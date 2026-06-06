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
		slog.Bool("audit", true),
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
		slog.Bool("audit", true),
		slog.String("event", "lfs.object.served"),
		slog.String("op", op),
		slog.String("hash", hash),
		slog.Int64("bytes", bytes),
		slog.Int("status", status),
	)
}

// emitLFSVerify records a "lfs.verify" audit event after a verify
// request completes. Matches the M11 audit flat-attrs shape.
//
// repo is "<tenant>/<repo>". user is the actor name or "" for
// anonymous (impossible today since verify is ActionWrite-only, but
// the param keeps the function shape consistent with emitLFSBatch).
// oid is the verified object's hex SHA-256. size is the claimed size
// (the body's reported size — may differ from stored size on the
// size_mismatch path).
func emitLFSVerify(ctx context.Context, logger *slog.Logger, repo, user, oid string, size int64, result string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "lfs.verify",
		slog.Bool("audit", true),
		slog.String("event", "lfs.verify"),
		slog.String("repo", repo),
		slog.String("user", user),
		slog.String("oid", oid),
		slog.Int64("size", size),
		slog.String("result", result),
	)
}

// emitLFSLockCreate records a "lfs.lock.create" audit event after a
// lock is successfully created (201). repo is "<tenant>/<repo>" form.
// user is the actor's Name. lockID, path, and refName carry the newly
// created lock's fields (refName may be "" if the client omitted Ref).
// ownerUserID is the M4 user ID of the lock creator — provided as an
// explicit field (in addition to the human-readable user name) so
// audit consumers can pivot on user IDs without name-joins.
func emitLFSLockCreate(ctx context.Context, logger *slog.Logger, repo, user, ownerUserID, lockID, path, refName string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "lfs.lock.create",
		slog.Bool("audit", true),
		slog.String("event", "lfs.lock.create"),
		slog.String("repo", repo),
		slog.String("user", user),
		slog.String("owner_user_id", ownerUserID),
		slog.String("lock_id", lockID),
		slog.String("path", path),
		slog.String("ref_name", refName),
	)
}

// emitLFSLockDelete records a "lfs.lock.delete" audit event after a
// lock is successfully deleted (200). repo is "<tenant>/<repo>" form.
// user is the caller's Name. force records whether force=true was
// requested. forceTargetUserID is the lock owner's UserID when the
// caller is NOT the owner (force-delete path); it is empty ("") when
// the caller IS the owner.
func emitLFSLockDelete(ctx context.Context, logger *slog.Logger, repo, user, lockID string, force bool, forceTargetUserID string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "lfs.lock.delete",
		slog.Bool("audit", true),
		slog.String("event", "lfs.lock.delete"),
		slog.String("repo", repo),
		slog.String("user", user),
		slog.String("lock_id", lockID),
		slog.Bool("force", force),
		slog.String("force_target_user_id", forceTargetUserID),
	)
}

// emitLFSLockVerify records a "lfs.lock.verify" audit event after a
// POST /info/lfs/locks/verify completes (200). repo is "<tenant>/<repo>"
// form. user is the actor's Name. oursCount and theirsCount are the
// lengths of the partitioned ours/theirs slices returned to the caller.
func emitLFSLockVerify(ctx context.Context, logger *slog.Logger, repo, user string, oursCount, theirsCount int) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "lfs.lock.verify",
		slog.Bool("audit", true),
		slog.String("event", "lfs.lock.verify"),
		slog.String("repo", repo),
		slog.String("user", user),
		slog.Int("ours_count", oursCount),
		slog.Int("theirs_count", theirsCount),
	)
}

// EmitLFSSSHAuthenticate records a "lfs.ssh_authenticate" audit event
// after a git-lfs-authenticate exec request completes. Matches the M11
// audit flat-attrs shape used by the other lfs.* events.
//
// repo is "<tenant>/<repo>". user is the actor name or "" for anonymous
// / deploy-key paths (which always fail with result=anon). op is
// "upload" or "download". ttlSeconds is 0 on the disabled/forbidden/anon
// paths (no token was minted); the configured TTL otherwise. result is
// one of: "ok", "forbidden", "disabled", "anon", "error",
// "client_disconnected".
//
// Exported because internal/sshd's session dispatcher emits this audit
// event when handling git-lfs-authenticate. No unexported variant
// exists; in-package callers may use this exported function directly.
func EmitLFSSSHAuthenticate(ctx context.Context, logger *slog.Logger, repo, user, op string, ttlSeconds int64, result string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "lfs.ssh_authenticate",
		slog.Bool("audit", true),
		slog.String("event", "lfs.ssh_authenticate"),
		slog.String("repo", repo),
		slog.String("user", user),
		slog.String("op", op),
		slog.Int64("ttl_seconds", ttlSeconds),
		slog.String("result", result),
	)
}

// EmitLFSGCMark records an "lfs.gc.mark" audit event after RunMark
// finishes. repo is "<tenant>/<repo>". markID is the freshly-issued
// mark identifier. candidatesCount is the number of orphan LFS
// objects recorded. manifestVersion is the manifest version observed
// at the start of the mark phase. dryRun reflects whether the caller
// will persist the mark to storage; it's needed in the audit stream
// so log consumers don't conclude a mark exists on disk when none
// was written.
//
// Exported so the sibling internal/lfs/gc package can call it.
func EmitLFSGCMark(ctx context.Context, logger *slog.Logger, repo, markID string, candidatesCount int, manifestVersion uint64, dryRun bool) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "lfs.gc.mark",
		slog.Bool("audit", true),
		slog.String("event", "lfs.gc.mark"),
		slog.String("repo", repo),
		slog.String("mark_id", markID),
		slog.Int("candidates_count", candidatesCount),
		slog.Uint64("manifest_version", manifestVersion),
		slog.Bool("dry_run", dryRun),
	)
}

// EmitLFSGCSweep records an "lfs.gc.sweep" audit event after RunSweep
// finishes. repo is "<tenant>/<repo>". markID is the mark the sweep
// operated against. sweepID is the freshly-issued sweep identifier.
// deletedCount / deletedBytes are the (or would-be, in dry-run)
// reclaimed counts. skippedRetention / skippedConcurrent partition the
// skipped candidates; errorCount is per-object delete failures.
// dryRun reflects whether the sweep persisted any deletions.
//
// Exported so the sibling internal/lfs/gc package can call it.
func EmitLFSGCSweep(ctx context.Context, logger *slog.Logger, repo, markID, sweepID string,
	deletedCount, skippedRetention, skippedConcurrent, errorCount int, deletedBytes int64, dryRun bool) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "lfs.gc.sweep",
		slog.Bool("audit", true),
		slog.String("event", "lfs.gc.sweep"),
		slog.String("repo", repo),
		slog.String("mark_id", markID),
		slog.String("sweep_id", sweepID),
		slog.Int("deleted_count", deletedCount),
		slog.Int64("deleted_bytes", deletedBytes),
		slog.Int("skipped_retention", skippedRetention),
		slog.Int("skipped_concurrent", skippedConcurrent),
		slog.Int("error_count", errorCount),
		slog.Bool("dry_run", dryRun),
	)
}

// EmitLFSQuotaExceeded records a "lfs.quota.exceeded" audit event
// when a Batch upload request is rejected because the tenant's
// quota would be breached. oids is the comma-joined OID list of
// the rejected batch.
//
// Exported for cross-package use.
func EmitLFSQuotaExceeded(ctx context.Context, logger *slog.Logger, tenant string, currentBytes, limitBytes, requestedBytes int64, oids string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "lfs.quota.exceeded",
		slog.Bool("audit", true),
		slog.String("event", "lfs.quota.exceeded"),
		slog.String("tenant", tenant),
		slog.Int64("current_bytes", currentBytes),
		slog.Int64("limit_bytes", limitBytes),
		slog.Int64("requested_bytes", requestedBytes),
		slog.String("oids", oids),
	)
}

// EmitLFSQuotaReconcile records a "lfs.quota.reconcile" audit event
// after a Reconcile call completes. driftBytes is signed: positive
// when actual > counter (under-reporting), negative the other way.
//
// Exported for cross-package use.
func EmitLFSQuotaReconcile(ctx context.Context, logger *slog.Logger, tenant string, beforeBytes, afterBytes, driftBytes int64, dryRun bool) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "lfs.quota.reconcile",
		slog.Bool("audit", true),
		slog.String("event", "lfs.quota.reconcile"),
		slog.String("tenant", tenant),
		slog.Int64("before_bytes", beforeBytes),
		slog.Int64("after_bytes", afterBytes),
		slog.Int64("drift_bytes", driftBytes),
		slog.Bool("dry_run", dryRun),
	)
}
