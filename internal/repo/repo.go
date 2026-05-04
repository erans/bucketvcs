package repo

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Repo is a handle to one (tenant, repo) pair backed by an
// ObjectStore. Construct via Open or Create. Repo holds no per-call
// mutable state and is safe to share between goroutines.
type Repo struct {
	store storage.ObjectStore
	keys  *keys.Repo
}

// Open returns a handle for an existing repo. Errors:
//   - ErrInvalidTenantID / ErrInvalidRepoID if the IDs fail validation.
//   - ErrRepoNotFound if the root manifest is missing.
//   - ErrUnsupportedSchema if the manifest's header fails the §43.7 gate.
//   - wrapped storage error otherwise.
//
// Open does not create anything. Use Create (Task 11) to initialize a
// new repo.
func Open(ctx context.Context, store storage.ObjectStore, tenantID, repoID string) (*Repo, error) {
	k, err := keys.NewRepo(tenantID, repoID)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := manifest.ReadRoot(ctx, store, k.RootManifestKey()); err != nil {
		return nil, err
	}
	return &Repo{store: store, keys: k}, nil
}

// TenantID returns the tenant identifier this Repo was opened with.
func (r *Repo) TenantID() string { return r.keys.TenantID() }

// RepoID returns the repo identifier this Repo was opened with.
func (r *Repo) RepoID() string { return r.keys.RepoID() }
