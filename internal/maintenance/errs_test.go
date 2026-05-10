package maintenance_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
)

func TestSentinelErrors_Distinct(t *testing.T) {
	all := []error{
		maintenance.ErrInvalidFlags,
		maintenance.ErrCASExhausted,
		maintenance.ErrCorruptInput,
		maintenance.ErrNoRefs,
	}
	seen := map[error]struct{}{}
	for _, e := range all {
		if e == nil {
			t.Fatalf("nil sentinel in list")
		}
		if _, dup := seen[e]; dup {
			t.Fatalf("duplicate sentinel: %v", e)
		}
		seen[e] = struct{}{}
	}
	if !errors.Is(maintenance.ErrCASExhausted, maintenance.ErrCASExhausted) {
		t.Fatalf("errors.Is identity broken")
	}
}
