package repo_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestSentinelErrorsAreDistinct(t *testing.T) {
	all := []error{
		repo.ErrRepoExists,
		repo.ErrRepoNotFound,
		repo.ErrUnsupportedSchema,
		repo.ErrCallbackFailed,
		repo.ErrInvalidTenantID,
		repo.ErrInvalidRepoID,
	}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("sentinel %d == %d but should be distinct: %v", i, j, a)
			}
		}
	}
}

func TestCommitGaveUpErrorUnwrap(t *testing.T) {
	got := &repo.CommitGaveUpError{
		Attempts:    8,
		OrphanTxIDs: []string{"tx_a", "tx_b"},
		LastErr:     storage.ErrVersionMismatch,
	}
	if !errors.Is(got, storage.ErrVersionMismatch) {
		t.Fatalf("CommitGaveUpError must Unwrap to LastErr; got %v", got)
	}
	if msg := got.Error(); msg == "" {
		t.Fatalf("CommitGaveUpError.Error() must produce a message")
	}
}
