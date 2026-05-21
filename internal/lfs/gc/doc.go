// Package gc implements mark-and-sweep garbage collection for Git
// LFS objects. Parallel to internal/gc (which handles Git pack-level
// objects); the two are independent and operate on different
// storage prefixes:
//
//	internal/gc       → packs/, indexes/, bundles/, ref-shards/ live sets
//	internal/lfs/gc   → lfs/objects/<oid> live set (this package)
//
// Discovery walks every reachable Git blob (via git rev-list +
// cat-file in a materialized mirror), peeks for the LFS pointer
// signature, and extracts the pointed-to LFS object OID. Sweep
// applies time-based retention (default 7 days). See
// docs/superpowers/specs/2026-05-20-m13.4-lfs-gc-design.md.
package gc
