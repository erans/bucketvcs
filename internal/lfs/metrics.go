package lfs

import (
	"context"
	"log/slog"
)

// emitMetric logs a structured metric in the same shape used by
// internal/gateway/log.go and internal/maintenance/log.go: an info-level
// "metric" record with attributes metric_name (string), value (int64),
// plus key/value pairs from kvs.
func emitMetric(ctx context.Context, logger *slog.Logger, name string, value int64, kvs ...any) {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []slog.Attr{
		slog.String("metric_name", name),
		slog.Int64("value", value),
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		k, ok := kvs[i].(string)
		if !ok {
			logger.LogAttrs(ctx, slog.LevelDebug, "emitMetric: skipping non-string label key",
				slog.String("metric_name", name),
				slog.Any("bad_key", kvs[i]))
			continue
		}
		attrs = append(attrs, slog.Any(k, kvs[i+1]))
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric", attrs...)
}

// emitBatchRequestMetric increments lfs_batch_requests_total{op,result}.
// op is "upload" or "download". result is one of:
//
//   - "ok": batch returned 200 with at least one object processed.
//   - "unauthorized": 401.
//   - "forbidden": 403.
//   - "notfound": 404 (repo not found).
//   - "error": any other 4xx or 5xx, including 422 on malformed body.
func emitBatchRequestMetric(ctx context.Context, logger *slog.Logger, op, result string) {
	emitMetric(ctx, logger, "lfs_batch_requests_total", 1,
		"op", op,
		"result", result,
	)
}

// emitBatchObjectMetric increments lfs_batch_objects_total{op,result}.
// Called once per object in a batch response. result is one of:
//
//   - "new": upload that produced an upload action (object missing).
//   - "exists": object present at matching size — upload returned empty
//     actions OR download returned a download action.
//   - "missing": download for an object not present.
//   - "error": any per-object error (size mismatch, presign failure,
//     etc.).
func emitBatchObjectMetric(ctx context.Context, logger *slog.Logger, op, result string) {
	emitMetric(ctx, logger, "lfs_batch_objects_total", 1,
		"op", op,
		"result", result,
	)
}

// emitObjectServedMetric increments lfs_object_served_total{op,result}.
// Emitted once per /_lfs/ PUT or GET request. op is "upload" or
// "download". result is one of: "ok", "exists", "missing", "too_large",
// "hash_mismatch", "error".
//
// hash_mismatch fires on the PUT path when SHA256(body) does not match
// the OID component of the URL — operators should alert on it.
func emitObjectServedMetric(ctx context.Context, logger *slog.Logger, op, result string) {
	emitMetric(ctx, logger, "lfs_object_served_total", 1,
		"op", op,
		"result", result,
	)
}

// emitVerifyRequestMetric increments lfs_verify_requests_total{result}.
// Emitted once per verify request. result is one of: "ok", "missing",
// "size_mismatch", "error".
func emitVerifyRequestMetric(ctx context.Context, logger *slog.Logger, result string) {
	emitMetric(ctx, logger, "lfs_verify_requests_total", 1,
		"result", result,
	)
}

// emitLockCreateMetric increments lfs_locks_created_total{outcome}.
// Called once per POST /info/lfs/locks request. outcome is one of:
//
//   - "created": 201 — lock was created successfully.
//   - "conflict": 409 — path already locked by another owner.
//   - "error": any other failure (401, 400, 503, 500).
func emitLockCreateMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	emitMetric(ctx, logger, "lfs_locks_created_total", 1, "outcome", outcome)
}

// emitLockListMetric increments lfs_locks_listed_total{outcome}.
// Called once per GET /info/lfs/locks request. outcome is one of:
//
//   - "success": 200 — list returned normally.
//   - "error": any failure (401, 400, 503, 500).
func emitLockListMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	emitMetric(ctx, logger, "lfs_locks_listed_total", 1, "outcome", outcome)
}

// emitLockVerifyMetric increments lfs_locks_verified_total{outcome}.
// Called once per POST /info/lfs/locks/verify request. outcome is one of:
//
//   - "success": 200 — partitioned ours/theirs returned normally.
//   - "error": any failure (401, 400, 503, 500).
func emitLockVerifyMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	emitMetric(ctx, logger, "lfs_locks_verified_total", 1, "outcome", outcome)
}

// emitLockDeleteMetric increments lfs_locks_deleted_total{force,outcome}.
// Called once per POST /info/lfs/locks/<id>/unlock request. force is
// whether the caller set force=true. outcome is one of:
//
//   - "owner": 200 — caller is the lock owner.
//   - "forced": 200 — non-owner caller used force=true.
//   - "denied": 403 — non-owner caller did not pass force=true.
//   - "not_found": 404 — lock ID does not exist.
//   - "error": any other failure (401, 400, 503, 500).
func emitLockDeleteMetric(ctx context.Context, logger *slog.Logger, force bool, outcome string) {
	forceStr := "false"
	if force {
		forceStr = "true"
	}
	emitMetric(ctx, logger, "lfs_locks_deleted_total", 1, "force", forceStr, "outcome", outcome)
}

// EmitSSHAuthenticateMetric increments lfs_ssh_authenticate_total{op,result}.
// op is "upload" or "download". result is one of: "ok", "forbidden",
// "disabled", "anon", "error", "client_disconnected".
//
// Exported because internal/sshd's session dispatcher emits this metric
// when handling git-lfs-authenticate. No unexported variant exists;
// in-package callers may use this exported function directly.
func EmitSSHAuthenticateMetric(ctx context.Context, logger *slog.Logger, op, result string) {
	emitMetric(ctx, logger, "lfs_ssh_authenticate_total", 1,
		"op", op,
		"result", result,
	)
}

// EmitGCObjectsMarkedMetric increments lfs_gc_objects_marked_total{outcome}.
// Emitted by RunMark after one mark pass. outcome is:
//
//   - "candidate": object found unreferenced and recorded in the mark.
//
// Exported so the internal/lfs/gc package (a sibling) can call it
// without violating Go's lowercase-export rules across packages.
func EmitGCObjectsMarkedMetric(ctx context.Context, logger *slog.Logger, outcome string, count int64) {
	emitMetric(ctx, logger, "lfs_gc_objects_marked_total", count, "outcome", outcome)
}

// EmitGCObjectsSweptMetric increments lfs_gc_objects_swept_total{outcome}.
// Emitted by RunSweep after one sweep pass, once per outcome bucket.
// outcome is one of:
//
//   - "deleted": object removed from storage (or counted as such in dry-run).
//   - "skipped_retention": candidate still inside the retention window.
//   - "skipped_concurrent": Head/Delete race; will be retried next sweep.
//   - "error": per-object delete failure (logged + counted in the report).
//
// Exported for the same cross-package-call reason as
// EmitGCObjectsMarkedMetric.
func EmitGCObjectsSweptMetric(ctx context.Context, logger *slog.Logger, outcome string, count int64) {
	emitMetric(ctx, logger, "lfs_gc_objects_swept_total", count, "outcome", outcome)
}

// EmitGCBytesSweptMetric increments lfs_gc_bytes_swept_total by bytes.
// Emitted by RunSweep after one sweep pass with the total bytes the
// sweep reclaimed (or would have reclaimed, in dry-run).
//
// Exported for the same cross-package-call reason as the marked /
// swept counters above.
func EmitGCBytesSweptMetric(ctx context.Context, logger *slog.Logger, bytes int64) {
	emitMetric(ctx, logger, "lfs_gc_bytes_swept_total", bytes)
}

// EmitQuotaCheckMetric increments lfs_quota_check_total{outcome}.
// Emitted once per Batch pre-check call. outcome is "ok" or "exceeded".
//
// Exported for cross-package use (internal/lfs/quota package).
func EmitQuotaCheckMetric(ctx context.Context, logger *slog.Logger, outcome string) {
	emitMetric(ctx, logger, "lfs_quota_check_total", 1, "outcome", outcome)
}

// EmitQuotaBytesUsedMetric emits the current used_bytes for a tenant
// as a gauge. value is the post-update absolute value, not a delta.
// Refreshed on Add, Subtract, Set, Clear, Reconcile.
//
// Exported for cross-package use.
func EmitQuotaBytesUsedMetric(ctx context.Context, logger *slog.Logger, tenant string, value int64) {
	emitMetric(ctx, logger, "lfs_quota_bytes_used", value, "tenant", tenant)
}

// TODO(P5): emitPresignSeconds histogram is in the M13 spec §7 metric
// list. Adding it requires plumbing a Logger + backend label through
// Store.PresignPut/PresignGet, which is more wiring than fits in P2's
// scope (the metric would also be redundant with the cloud backends'
// own latency dashboards). Revisit when the operator guide ships in
// P5; if operators don't ask for it, drop from the spec.
