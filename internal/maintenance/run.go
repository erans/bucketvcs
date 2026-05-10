package maintenance

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Run is the public entry point for one maintenance run against a single
// repo. It normalises opts, validates them, then delegates to runPipeline.
func Run(ctx context.Context, s storage.ObjectStore, r *repo.Repo, k *keys.Repo, opts RunOptions) (Report, error) {
	opts.Normalize()
	if err := opts.Validate(); err != nil {
		return Report{
			RepoID:  r.TenantID() + "/" + r.RepoID(),
			Outcome: "failed_other",
		}, err
	}
	return runPipeline(ctx, s, r, k, opts)
}
