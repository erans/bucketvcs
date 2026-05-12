// Package uploadpack log.go mirrors the metric and audit helpers from
// internal/gateway/log.go and internal/maintenance/log.go. The uploadpack
// engine cannot import internal/gateway (circular dependency risk and
// layering violation), so the helpers are duplicated here. Any change to
// the helper contract in those packages should be mirrored here.
package uploadpack

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
)

// emitMetric logs a structured metric with a name, value, and optional
// label pairs. Mirrors internal/gateway/log.go::emitMetric and
// internal/maintenance/log.go::emitMetric so log consumers can apply the
// same parsing rules across all three packages. Pairs whose key isn't a
// string are skipped (debug-logged) rather than emitted with an empty key.
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
// upload-pack engine's bundle-uri command response contains at least one
// bundle entry. One event per request that returned URLs (an empty advertise
// emits nothing). Operators correlate first_tip_oid with bundle.generated
// (maintenance-side emitted at runBundlePhase) to attribute serves to the
// generating maintenance run.
//
// Mirrors internal/gateway/log.go::emitBundleURIAdvertised.
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

// classifyVia returns "proxied" if the URL's PATH (not query, not fragment)
// contains "/_bundle/" or "/_pack/" — the gateway-proxied route shape per
// M11 Phase 6 routing. Otherwise returns "direct".
//
// Path-only matching avoids misclassifying signed cloud-backend URLs whose
// query parameters happen to contain the substring (e.g. an S3 URL with
// response-content-disposition=attachment;filename=foo/_bundle/x).
func classifyVia(rawURL string) string {
	if rawURL == "" {
		return "direct"
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "direct"
	}
	if strings.Contains(u.Path, "/_bundle/") || strings.Contains(u.Path, "/_pack/") {
		return "proxied"
	}
	return "direct"
}
