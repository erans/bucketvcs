// Package conformance contains property-style cross-implementation
// tests for the RefStore interface. Mirrors the pattern in
// internal/storage/conformance: the test bodies live here and are
// invoked from a single _test.go entry point in the consuming
// package, so a future second consumer (e.g., a remote cache that
// implements RefStore) can re-run the same suite by supplying its
// own Factory.
//
// Three properties asserted:
//
//   - Equivalence: for any ref set R, InlineRefStore(R).List() ==
//     ShardedRefStore(shard(R)).List().
//   - Round-trip: applying a Stage and reconstructing the store
//     equals expected(oldRefs, updates).
//   - Determinism: marshalling the same ref set twice yields
//     byte-identical shard objects.
//
// Random ref-set generation is seeded explicitly so failures
// reproduce. Sizes scaled to keep the suite under a second.
package conformance
