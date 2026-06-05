package gateway

import (
	"context"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ByobResolver is satisfied by *byob.StoreResolver. Defined as an interface
// to avoid a direct dependency on internal/byob from the gateway package.
type ByobResolver interface {
	Resolve(ctx context.Context, tenant string) (storage.ObjectStore, error)
}

// resolveStore returns the ObjectStore for the tenant. In BYOB mode
// (s.resolver != nil), resolves the per-tenant store. Otherwise returns s.store.
func (s *Server) resolveStore(ctx context.Context, tenant string) (storage.ObjectStore, error) {
	if s.resolver != nil {
		return s.resolver.Resolve(ctx, tenant)
	}
	return s.store, nil
}

// byobOK writes a 500 and returns false when resolveStore fails.
func (s *Server) byobOK(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	s.logger.Error("byob: resolve store", "err", err)
	http.Error(w, "storage error", http.StatusInternalServerError)
	return false
}
