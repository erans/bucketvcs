// Package repo is the M1 transaction kernel: the only place in the
// codebase that atomically advances a repo from one durable state to the
// next. Sits between internal/storage (M0) and the future Git object
// engine (M2).
package repo

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrRepoExists: Create called against an existing repo
	// (manifest/root.json already present).
	ErrRepoExists = errors.New("repo: root manifest already exists")

	// ErrRepoNotFound: Open or ReadRoot called against a repo whose
	// manifest/root.json does not exist.
	ErrRepoNotFound = errors.New("repo: root manifest not found")

	// ErrUnsupportedSchema: the on-disk manifest's schema_version
	// exceeds the maximum this build supports, OR its min_reader_version
	// exceeds this build's reader version. Per spec §43.7 the gate is
	// asymmetric and fail-closed: refuse rather than ignore unknown
	// fields.
	ErrUnsupportedSchema = errors.New("repo: schema or min_reader_version exceeds supported")

	// ErrCallbackFailed: the buildBody callback supplied to Commit
	// returned an error. Wrap with errors.Unwrap to retrieve the
	// caller's original error.
	ErrCallbackFailed = errors.New("repo: buildBody callback returned error")

	// ErrInvalidTenantID: tenant_id failed Validate (charset, length,
	// or path-traversal check).
	ErrInvalidTenantID = errors.New("repo: tenant_id invalid")

	// ErrInvalidRepoID: repo_id failed Validate.
	ErrInvalidRepoID = errors.New("repo: repo_id invalid")
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
