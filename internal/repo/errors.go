// Package repo is the M1 transaction kernel: the only place in the
// codebase that atomically advances a repo from one durable state to the
// next. Sits between internal/storage (M0) and the future Git object
// engine (M2).
package repo

import (
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

// Re-exports of the sentinel errors defined in internal/repo/repoerrs.
// New code in subpackages (keys, manifest, tx) should import repoerrs
// directly to avoid the import cycle that would arise from depending on
// this package. Callers outside internal/repo may continue to use
// repo.ErrXxx — these are the same error values.
var (
	ErrRepoExists        = repoerrs.ErrRepoExists
	ErrRepoNotFound      = repoerrs.ErrRepoNotFound
	ErrUnsupportedSchema = repoerrs.ErrUnsupportedSchema
	ErrCallbackFailed    = repoerrs.ErrCallbackFailed
	ErrInvalidTenantID   = repoerrs.ErrInvalidTenantID
	ErrInvalidRepoID     = repoerrs.ErrInvalidRepoID
)

// CommitGaveUpError is re-exported from repoerrs.
type CommitGaveUpError = repoerrs.CommitGaveUpError
