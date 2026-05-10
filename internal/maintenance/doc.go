// Package maintenance implements bucketvcs's M9 background-maintenance
// pipeline: a single full repack of canonical packs, fresh commit-graph
// (.bvcg) and object-map (.bvom) indexes against the new pack, and a
// CAS-merge that preserves concurrent push packs (§43.6 / §17).
//
// The pipeline is invoked from one-shot operator processes
// (cmd/bucketvcs/maintenance.go) — the package has no scheduler,
// daemon, or background goroutines of its own. Pack production uses
// gitcli.PackObjectsAll against a per-run temp bare repo materialized
// from current canonical packs; index building is pure Go.
//
// Composition with M8: M9 produces, M8 reclaims. Old canonical packs
// and stale indexes drop out of manifest.Packs / manifest.Indexes on
// CAS-merge; M8 GC sweeps them after retention.
package maintenance
