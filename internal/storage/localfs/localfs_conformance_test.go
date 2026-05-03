package localfs_test

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/conformance"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t testing.TB) (storage.ObjectStore, func()) {
		dir := t.TempDir()
		s, err := localfs.Open(dir)
		if err != nil {
			t.Fatalf("localfs.Open(%q): %v", dir, err)
		}
		return s, func() { _ = s.Close() }
	})
}
