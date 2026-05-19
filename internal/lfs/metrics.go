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

// TODO(P5): emitPresignSeconds histogram is in the M13 spec §7 metric
// list. Adding it requires plumbing a Logger + backend label through
// Store.PresignPut/PresignGet, which is more wiring than fits in P2's
// scope (the metric would also be redundant with the cloud backends'
// own latency dashboards). Revisit when the operator guide ships in
// P5; if operators don't ask for it, drop from the spec.
