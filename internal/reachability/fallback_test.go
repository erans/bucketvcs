package reachability_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
)

func TestClassifyFallback_NoIndex(t *testing.T) {
	if reachability.ClassifyFallback(reachability.ErrNoIndex) != "no_index" {
		t.Fatalf("classify ErrNoIndex")
	}
}

func TestClassifyFallback_DeltaDecode(t *testing.T) {
	wrapped := fmt.Errorf("load delta: %w", deltaindex.ErrMalformed)
	if reachability.ClassifyFallback(wrapped) != "delta_decode" {
		t.Fatalf("classify ErrMalformed")
	}
}

func TestClassifyFallback_Unknown(t *testing.T) {
	if reachability.ClassifyFallback(errors.New("anything else")) != "unknown" {
		t.Fatalf("classify unknown")
	}
}
