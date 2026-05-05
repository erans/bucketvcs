# Differential harness

The M2 differential harness compares bucketvcs against upstream git on a
synthetic in-test fixture corpus. See spec §5 of the M2 design and §40.3
of the source spec.

## Adding a fixture

1. Add a builder function in `internal/diffharness/fixtures/synthetic.go`.
2. Register it in `internal/diffharness/fixtures/fixtures.go` `Registry` map.
3. Run `go test ./internal/diffharness/...`. Both round-trip and
   cat-object oracles auto-pick up the new fixture.

## Adding an oracle

The current oracles are round-trip (`roundtrip.go`) and cat-object
(`catobject.go`). To add a new oracle (e.g., for fetch negotiation in
M3), create a parallel test file under `internal/diffharness/` that
consumes `fixtures.Registry` and asserts equivalence between bucketvcs
and upstream git on some operation.

## Promotion rule

Per spec §40.3, a pure-Go serving path must reach 100% pass on this
harness + 4-week shadow before becoming default serving. M2 doesn't
promote anything (no serving path); the harness exists to be extended
at M3+.
