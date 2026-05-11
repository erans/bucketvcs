package conformance_test

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/reachability/conformance"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestReachabilitySafety_Localfs(t *testing.T) {
	conformance.RunPropertyReachabilitySafety(t, func(t testing.TB) (storage.ObjectStore, func()) {
		s, err := localfs.Open(t.TempDir())
		if err != nil {
			t.Fatalf("localfs.Open: %v", err)
		}
		return s, func() { _ = s.Close() }
	})
}
