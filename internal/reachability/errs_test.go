package reachability_test

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/reachability"
)

func TestErrors_Defined(t *testing.T) {
	if reachability.ErrNoIndex == nil {
		t.Fatalf("ErrNoIndex must be non-nil")
	}
}
