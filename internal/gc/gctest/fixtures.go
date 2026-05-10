// Package gctest holds shared test fixtures for the gc package and its
// CLI consumers. Test-only.
package gctest

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// PutEmpty writes a zero-byte object via PutIfAbsent and fatals on error.
// Useful when a test needs an object to exist without caring about its
// contents (orphan packs, marker stubs, etc.).
func PutEmpty(t *testing.T, s storage.ObjectStore, key string) {
	t.Helper()
	if _, err := s.PutIfAbsent(context.Background(), key, strings.NewReader(""), nil); err != nil {
		t.Fatalf("PutEmpty %s: %v", key, err)
	}
}

// PutBytes writes the given content via PutIfAbsent and fatals on error.
func PutBytes(t *testing.T, s storage.ObjectStore, key string, content []byte) {
	t.Helper()
	if _, err := s.PutIfAbsent(context.Background(), key, bytes.NewReader(content), nil); err != nil {
		t.Fatalf("PutBytes %s: %v", key, err)
	}
}
