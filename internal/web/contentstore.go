package web

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

// ContentStore is the read surface the browse pages need. It is satisfied
// directly by *gitbrowse.Service (which returns browsemodel types), wired at the
// composition root. internal/web depends only on this interface and the leaf
// browsemodel package — never on the storage/mirror layer.
type ContentStore interface {
	ListRefs(ctx context.Context, tenant, repo string) (browsemodel.Refs, error)
	Resolve(ctx context.Context, tenant, repo, rest string) (browsemodel.Resolved, error)
	ReadTree(ctx context.Context, tenant, repo, oid, path string) ([]browsemodel.TreeEntry, error)
	ReadBlob(ctx context.Context, tenant, repo, oid, path string) (browsemodel.Blob, error)
	Log(ctx context.Context, tenant, repo, oid string, offset, limit int) ([]browsemodel.CommitMeta, bool, error)
	Commit(ctx context.Context, tenant, repo, oid string) (browsemodel.CommitDetail, error)
}
