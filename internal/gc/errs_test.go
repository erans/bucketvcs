package gc_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gc"
)

func TestErrInvalidPhaseCombo(t *testing.T) {
	var e error = gc.ErrInvalidPhaseCombo
	if !errors.Is(e, gc.ErrInvalidPhaseCombo) {
		t.Fatal("ErrInvalidPhaseCombo must be a sentinel")
	}
}

func TestErrNoMarkForSweep(t *testing.T) {
	var e error = gc.ErrNoMarkForSweep
	if !errors.Is(e, gc.ErrNoMarkForSweep) {
		t.Fatal("ErrNoMarkForSweep must be a sentinel")
	}
}
