package s3compat

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestDeleteIfVersionMatchesSuccess(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("v"), `"v0"`)
	if err := s.DeleteIfVersionMatches(context.Background(), "k", storage.ObjectVersion{Token: `"v0"`}); err != nil {
		t.Fatalf("DeleteIfVersionMatches: %v", err)
	}
	if _, ok := mb.objects["k"]; ok {
		t.Fatalf("object should be deleted")
	}
}

func TestDeleteIfVersionMatchesMismatch(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("v"), `"v0"`)
	err := s.DeleteIfVersionMatches(context.Background(), "k", storage.ObjectVersion{Token: `"WRONG"`})
	if !errors.Is(err, storage.ErrVersionMismatch) {
		t.Fatalf("err = %v, want ErrVersionMismatch", err)
	}
}

func TestDeleteIfVersionMatchesAbsent(t *testing.T) {
	s, _ := newMockBackend(t)
	err := s.DeleteIfVersionMatches(context.Background(), "missing",
		storage.ObjectVersion{Token: `"v0"`})
	// Real S3/R2 may return 404 OR 412 for DELETE + If-Match against an
	// absent key (AWS docs underspecify this). Accept either; both
	// tell the caller "the delete didn't happen, the object isn't
	// where you thought it was". Live conformance (T13) is the
	// authoritative test against real backends.
	if !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrVersionMismatch) {
		t.Fatalf("err = %v, want ErrNotFound or ErrVersionMismatch", err)
	}
}

func TestDeleteIfVersionMatchesAcceptsEmptyShape(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("v"), `"v0"`)
	// Caller-built OV with empty Provider/Kind is accepted charitably
	// (matches T8's PutIfVersionMatchesAcceptsEmptyShape).
	if err := s.DeleteIfVersionMatches(context.Background(), "k",
		storage.ObjectVersion{Token: `"v0"`}); err != nil {
		t.Fatalf("DeleteIfVersionMatches with empty Provider/Kind: %v", err)
	}
}

func TestDeleteIfVersionMatchesRejectsInvalidKey(t *testing.T) {
	s, _ := newMockBackend(t)
	bad := []string{"", "/foo", "foo/", "foo\x00bar"}
	for _, k := range bad {
		t.Run(k, func(t *testing.T) {
			err := s.DeleteIfVersionMatches(context.Background(), k, storage.ObjectVersion{Token: `"v0"`})
			if !errors.Is(err, storage.ErrInvalidArgument) {
				t.Fatalf("Delete(%q) err = %v, want ErrInvalidArgument", k, err)
			}
		})
	}
}

func TestDeleteIfVersionMatchesRejectsWrongProvider(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("v"), `"v0"`)
	expected := storage.ObjectVersion{Provider: "localfs", Token: `"v0"`, Kind: storage.VersionEtag}
	err := s.DeleteIfVersionMatches(context.Background(), "k", expected)
	if !errors.Is(err, storage.ErrVersionMismatch) {
		t.Fatalf("err = %v, want ErrVersionMismatch (wrong Provider)", err)
	}
}
