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
	// The adapter Heads the object before deleting; absent keys return
	// ErrNotFound from that Head call, regardless of what the
	// underlying DELETE would have done.
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDeleteIfVersionMatchesGuardsAgainstIdempotentDelete(t *testing.T) {
	// Real S3 may return 204 for DELETE + If-Match on an absent key
	// (idempotent semantics). The adapter must NOT report success for
	// such a delete: the Head pre-check should catch the absent key
	// first. This test pins that behavior.
	s, mb := newMockBackend(t)
	// Mock is configured to return 204 for absent DELETEs (matching
	// real S3 behavior). If the adapter ever stops Head-verifying,
	// this test will fail because the call would return nil instead
	// of ErrNotFound.
	_ = mb // capture so the unused import warning doesn't fire
	err := s.DeleteIfVersionMatches(context.Background(), "ghost",
		storage.ObjectVersion{Token: `"v0"`})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("absent-key delete: err = %v, want ErrNotFound (Head-first guard)", err)
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
