package gcs

import (
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestValidatePartList(t *testing.T) {
	have := map[int][]byte{1: {}, 2: {}, 3: {}}

	if err := validatePartList(nil, have); err == nil {
		t.Errorf("nil parts: want error, got nil")
	}
	if err := validatePartList([]bvstorage.MultipartPart{{PartNumber: 1}, {PartNumber: 2}, {PartNumber: 3}}, have); err != nil {
		t.Errorf("happy path: %v", err)
	}
	if err := validatePartList([]bvstorage.MultipartPart{{PartNumber: 5}}, have); err == nil {
		t.Errorf("unknown part: want error")
	}
	if err := validatePartList([]bvstorage.MultipartPart{{PartNumber: 1}, {PartNumber: 1}}, have); err == nil {
		t.Errorf("dup part: want error")
	}
	_ = errors.New
}

func TestNewUploadID(t *testing.T) {
	a, err := newUploadID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newUploadID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("expected unique IDs, got %s twice", a)
	}
	if len(a) != 32 {
		t.Errorf("len(id) = %d, want 32 hex chars", len(a))
	}
}
