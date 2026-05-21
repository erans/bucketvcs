package gc

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// LoadRefsFromBodyForTest exposes loadRefsFromBody to gc_test. The
// real callers stay inside the package; this thin wrapper exists so
// the v2 sharded-refs invariant can be tested without re-routing the
// helper through an exported public surface.
func LoadRefsFromBodyForTest(ctx context.Context, store storage.ObjectStore, tenantID, repoID string, body *manifest.Body) (map[string]string, error) {
	return loadRefsFromBody(ctx, store, tenantID, repoID, body)
}
