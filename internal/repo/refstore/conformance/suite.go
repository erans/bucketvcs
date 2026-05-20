package conformance

import (
	"testing"
)

// RunSuite executes every property test against the InlineRefStore
// and ShardedRefStore pair the package exposes via internal helpers.
// Future consumers can add Factory-based parameters here; M12 only
// needs the built-in dual-impl matrix.
func RunSuite(t *testing.T) {
	t.Helper()
	t.Run("Equivalence", testEquivalence)
	t.Run("RoundTrip", testRoundTrip)
	t.Run("Determinism", testDeterminism)
}
