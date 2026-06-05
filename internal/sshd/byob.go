package sshd

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// byobResolver is satisfied by *byob.StoreResolver. Defined as an interface
// to avoid a direct dependency on internal/byob from the sshd package.
type byobResolver interface {
	Resolve(ctx context.Context, tenant string) (storage.ObjectStore, error)
}

// resolveStore returns the ObjectStore for the tenant. In BYOB mode
// (s.opts.Resolver != nil), resolves the per-tenant store. Otherwise returns
// s.opts.BVStore.
func (s *Server) resolveStore(ctx context.Context, tenant string) (storage.ObjectStore, error) {
	if s.opts.Resolver != nil {
		return s.opts.Resolver.Resolve(ctx, tenant)
	}
	return s.opts.BVStore, nil
}
