package conformance_test

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance/conformance"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestRunPropertyMaintenanceSafety_LocalFS(t *testing.T) {
	conformance.RunPropertyMaintenanceSafety(t, func(t testing.TB) (storage.ObjectStore, func()) {
		dir := t.TempDir()
		s, err := localfs.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		return s, func() {} // localfs needs no cleanup beyond t.TempDir
	})
}
