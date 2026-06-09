package auth

import (
	"context"
	"log/slog"
)

// RepoAliasResolver is an optional capability: a Store that also resolves
// repo rename aliases. Entrypoints type-assert their Store to this interface;
// a Store that doesn't implement it simply has no alias resolution (old names
// 404 as before). Implemented by *sqlitestore.Store.
type RepoAliasResolver interface {
	// ResolveAlias returns the current live target for a renamed-away name.
	// ok is false when there is no alias. Callers MUST still verify the
	// target is a live repo and enforce auth on the canonical repo.
	ResolveAlias(ctx context.Context, tenant, name string) (target string, ok bool, err error)
}

// EmitRepoAliasResolvedMetric logs one repo_alias_resolved_total{transport}
// sample. transport is one of: ui, https, ssh, lfs.
func EmitRepoAliasResolvedMetric(ctx context.Context, logger *slog.Logger, transport string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "repo_alias_resolved_total"),
		slog.String("transport", transport),
		slog.Int("value", 1),
	)
}
