package byob

import (
	"context"
	"errors"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Resolver is the structural store-resolution surface: *StoreResolver
// satisfies it, and gateway/lfs consume it behind their own interface aliases
// to keep the concrete type decoupled.
type Resolver interface {
	Resolve(ctx context.Context, tenant string) (storage.ObjectStore, error)
}

// StoreForTenant is the single BYOB store-selection policy shared by every
// consumer (gateway proxied bundle/pack routes, the gateway's LFS batch
// NewStore closure, the proxied /_lfs/ object handler, and any future caller)
// so the rules cannot drift between call sites:
//
//   - resolver == nil            -> operator store (pre-BYOB single-store
//     mode; no resolution was ever configured)
//   - Resolve error matching
//     auth.ErrNoSuchBinding      -> operator store (the binding is genuinely
//     absent; the tenant intentionally has no BYOB store). Note the concrete
//     *StoreResolver already returns the operator store for this case; the
//     sentinel arm here keeps the policy correct for resolver
//     implementations that surface ErrNoSuchBinding as an error instead.
//   - any other Resolve error    -> fail closed: (nil, err). Authdb failures,
//     decrypt failures, and open failures must never substitute the operator
//     store — silently landing/missing tenant bytes in the wrong bucket caused
//     U-5/U-6/U-7.
//   - Resolve success            -> the resolved tenant store
func StoreForTenant(ctx context.Context, resolver Resolver, operator storage.ObjectStore, tenant string) (storage.ObjectStore, error) {
	if resolver == nil {
		return operator, nil
	}
	s, err := resolver.Resolve(ctx, tenant)
	if err != nil {
		if errors.Is(err, auth.ErrNoSuchBinding) {
			return operator, nil
		}
		return nil, err
	}
	return s, nil
}
