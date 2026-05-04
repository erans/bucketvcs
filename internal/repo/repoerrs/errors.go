// Package repoerrs holds the canonical sentinel errors for the
// internal/repo subsystem. It exists as a leaf so subpackages
// (keys, manifest, tx) and the parent repo package can all
// reference the same error values without creating an import cycle.
//
// Callers that import internal/repo directly may continue to use
// repo.ErrRepoExists etc. — those names are re-exports of the values
// defined here.
package repoerrs

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrRepoExists        = errors.New("repo: root manifest already exists")
	ErrRepoNotFound      = errors.New("repo: root manifest not found")
	ErrUnsupportedSchema = errors.New("repo: schema or min_reader_version exceeds supported")
	ErrCallbackFailed    = errors.New("repo: buildBody callback returned error")
	ErrInvalidTenantID   = errors.New("repo: tenant_id invalid")
	ErrInvalidRepoID     = errors.New("repo: repo_id invalid")
)

// CommitGaveUpError is returned by Repo.Commit when the retry budget is
// exhausted by repeated CAS conflicts. OrphanTxIDs lists the tx records
// written across all attempts; they remain on disk and become M8 GC
// candidates per §43.6.
type CommitGaveUpError struct {
	Attempts    int
	OrphanTxIDs []string
	LastErr     error
}

func (e *CommitGaveUpError) Error() string {
	return fmt.Sprintf(
		"repo: commit gave up after %d attempts (orphans: %s): %v",
		e.Attempts, strings.Join(e.OrphanTxIDs, ","), e.LastErr,
	)
}

func (e *CommitGaveUpError) Unwrap() error { return e.LastErr }
