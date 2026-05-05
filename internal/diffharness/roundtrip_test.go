package diffharness

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
}

func TestRoundTrip_AllFixtures(t *testing.T) {
	skipIfNoGit(t)
	for name, build := range fixtures.Registry {
		t.Run(name, func(t *testing.T) {
			ImportThenExportAndCompare(t, name, build)
		})
	}
}
