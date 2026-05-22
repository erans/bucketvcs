// Package policy implements per-repo protected-ref rules backed by
// the M4 authdb sqlite. See docs/superpowers/specs/2026-05-21-m14-hooks-policy-design.md
// for the design.
//
// Tier 1 of the §23 hooks-and-policy roadmap: ref-level rules only
// (block deletion, block force-push). Tier 2 (file size, path
// restrictions, author/email rules, commit-message regex) and
// Tier 3 (external hooks / webhooks) are explicitly deferred.
//
// The package is fully opt-in: callers that don't wire a Service
// into receivepack.EngineRequest.Policy see exactly pre-M14
// behaviour (no enforcement, no metric emissions).
package policy
