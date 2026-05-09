package gcs

import (
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestParseGenRejectsWrongProvider(t *testing.T) {
	_, err := parseGen(bvstorage.ObjectVersion{Provider: "s3compat", Token: "123"})
	if err == nil {
		t.Fatal("expected error for non-gcs provider")
	}
}

func TestParseGenAcceptsEmptyProvider(t *testing.T) {
	gen, err := parseGen(bvstorage.ObjectVersion{Token: "42"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if gen != 42 {
		t.Fatalf("gen = %d, want 42", gen)
	}
}

func TestParseGenRejectsNonNumeric(t *testing.T) {
	_, err := parseGen(bvstorage.ObjectVersion{Token: "not-a-number"})
	if err == nil {
		t.Fatal("expected error for non-numeric token")
	}
}
