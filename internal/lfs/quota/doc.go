// Package quota implements per-tenant LFS byte quotas backed by the
// M4 authdb sqlite. See docs/superpowers/specs/2026-05-21-m13.5-quotas-design.md
// for the design and race-semantics rationale.
//
// The package is fully opt-in: callers that don't wire a Service into
// internal/lfs.Deps / internal/lfs.ProxiedDeps see exactly pre-M13.5
// behaviour (no enforcement, no counter changes, no metric emissions
// for the quota family).
package quota
