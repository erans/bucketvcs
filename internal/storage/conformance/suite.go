// Package conformance is the storage adapter conformance test suite. It
// is a regular Go package, not a _test package, so it can be imported
// from any adapter's _test.go file and (later) from a
// `bucketvcs conformance-test` CLI subcommand.
//
// The contract being tested is documented at internal/storage as
// ObjectStore. Test names map to the §29 correctness and stress lists in
// the original spec; the comment on each test cites its §29 number.
package conformance

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Factory returns a fresh storage.ObjectStore for one test invocation,
// plus a cleanup function the suite calls when the test finishes.
//
// Each call must return an empty, isolated store: the suite does not
// share state across tests.
type Factory func(t testing.TB) (storage.ObjectStore, func())

// Run executes the full conformance suite (correctness + stress) against
// any adapter. Adapter packages call this from their _test.go files. The
// stress sub-suite is skipped when go test -short is in effect.
func Run(t *testing.T, f Factory) {
	t.Helper()
	t.Run("correctness", func(t *testing.T) {
		runCorrectness(t, f)
	})
	t.Run("stress", func(t *testing.T) {
		if testing.Short() {
			t.Skip("stress tests skipped in -short mode")
		}
		runStress(t, f)
	})
}
