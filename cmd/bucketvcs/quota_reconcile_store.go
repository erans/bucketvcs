package main

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/byob"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// reconcileQuotaForTenant resolves the same tenant store used by LFS transfer
// paths before walking object usage. Resolution failures fail closed; only an
// absent binding falls back to the operator store via StoreForTenant.
func reconcileQuotaForTenant(ctx context.Context, svc *quota.Service, resolver byob.Resolver, operator storage.ObjectStore, tenant string, dryRun bool) (quota.Report, error) {
	store, err := byob.StoreForTenant(ctx, resolver, operator, tenant)
	if err != nil {
		return quota.Report{}, err
	}
	return svc.Reconcile(ctx, store, tenant, dryRun)
}
