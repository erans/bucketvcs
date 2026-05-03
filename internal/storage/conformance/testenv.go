package conformance

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// newStore wraps the Factory call so test code reads more cleanly.
func newStore(t testing.TB, f Factory) storage.ObjectStore {
	t.Helper()
	s, cleanup := f(t)
	t.Cleanup(cleanup)
	return s
}

// ctx returns a fresh background context for use in tests. Tests that
// need cancellation create their own; this helper is for the common case.
func ctx() context.Context { return context.Background() }
