package browsemodel

import (
	"errors"
	"fmt"
	"testing"
)

func TestSentinelsAreDistinct(t *testing.T) {
	if errors.Is(ErrNotFound, ErrWarming) || errors.Is(ErrWarming, ErrNotFound) {
		t.Fatal("ErrNotFound and ErrWarming must be distinct sentinels")
	}
	wrapped := fmt.Errorf("read tree: %w", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Fatal("wrapped ErrNotFound must satisfy errors.Is")
	}
}

func TestRefsZeroValueIsSafe(t *testing.T) {
	var r Refs
	if len(r.Branches) != 0 || len(r.Tags) != 0 || r.Default != "" {
		t.Fatal("zero Refs should be empty")
	}
}
