package gateway

import (
	"context"
	"log/slog"
)

// emitMetric logs a structured metric with a name, value, and optional
// label pairs. Mirrors internal/maintenance/log.go::emitMetric exactly so
// log consumers can apply the same parsing rules across the two packages.
// Pairs whose key isn't a string are skipped (debug-logged) rather than
// emitted with an empty key.
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

// emitBundleURIAdvertised logs a bundle.uri.advertised audit event after the
// gateway's bundle-uri command response contains at least one bundle entry.
// One event per request that returned URLs (an empty advertise emits nothing).
// Operators correlate first_tip_oid with bundle.generated (maintenance-side
// emitted at runBundlePhase) to attribute serves to the generating maintenance
// run.
func emitBundleURIAdvertised(ctx context.Context, logger *slog.Logger, repoID, freshness, via string, bundleCount int, firstTipOID string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "bundle.uri.advertised",
		slog.Bool("audit", true),
		slog.String("event", "bundle.uri.advertised"),
		slog.String("repo_id", repoID),
		slog.String("freshness", freshness),
		slog.String("via", via),
		slog.Int("bundle_count", bundleCount),
		slog.String("first_tip_oid", firstTipOID),
	)
}

// emitProxiedURLServed logs a proxied.url.served audit event after a
// successful 200/206 reply from the proxied URL endpoint. Emitted from
// proxiedHandler post-io.Copy; the bytes_served value is the actual bytes
// written (via countingWriter), not the Content-Length header.
//
// tenant and repo are the M19 multi-tenant attribution attrs — they
// identify which (tenant, repo) pair this served object belonged to so
// operators running a shared gateway can attribute serve volume per
// repository. Both are validated by the URL parser before reaching this
// call site, so values here are safe to log verbatim.
func emitProxiedURLServed(ctx context.Context, logger *slog.Logger, kind, hash, tenant, repo string, bytesServed int64, statusCode int, rangeRequest bool) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "proxied.url.served",
		slog.Bool("audit", true),
		slog.String("event", "proxied.url.served"),
		slog.String("kind", kind),
		slog.String("hash", hash),
		slog.String("tenant", tenant),
		slog.String("repo", repo),
		slog.Int64("bytes_served", bytesServed),
		slog.Int("status_code", statusCode),
		slog.Bool("range_request", rangeRequest),
	)
}
