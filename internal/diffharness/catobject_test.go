package diffharness

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
)

func TestCatObject_AllFixtures(t *testing.T) {
	skipIfNoGit(t)
	for name, build := range fixtures.Registry {
		t.Run(name, func(t *testing.T) {
			CatObjectOracle(t, name, build)
		})
	}
}
